# Logbook

The store for Those Who Run v2: it subscribes to the bus and writes
sessions and laps to the data volume it owns.
It is the only writer on that volume, and everything else reads through
its services ([ADR 0003](https://github.com/those-who-run/paddock/blob/main/docs/adr/0003-the-store.md) in paddock).

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here
names its paddock issue: `Closes those-who-run/paddock#N.` when it meets every
acceptance criterion, `Part of those-who-run/paddock#N` otherwise
([`AGENTS.md`](AGENTS.md)).

## The store

`internal/store` writes a session's events and each lap's frames under one
data directory, which only `logbook` reads and writes
([ADR 0007](https://github.com/those-who-run/paddock/blob/main/docs/adr/0007-the-store-engine.md) in paddock):

```text
logbook.lock                            held by the one process using the directory
index.duckdb                            the sessions, session_runs, laps and frames_pending tables
sessions/<src>/<sid>/lap-NNN.parquet    a lap's racing frames, ZSTD
```

- **Sources.** Live sessions, replays and parity runs share the index. Every
  row and every path carries its source, `src`: `live`, `replay-<run>` or
  `parity`. Readers show `live` unless asked for another.
- **One process.** Opening the store takes an exclusive lock on
  `logbook.lock`, and a second process is refused (`ErrBusy`) rather than
  made to wait.
- **The index.** `sessions` (keyed by `src` and `id`) and `laps` (keyed by
  `src`, `session_id` and `lap`). The columns v1's index has keep v1's names;
  a lap row also records its frames' top and minimum speed, path length and
  largest single-frame jump. The schema is one ordered list of migrations, run
  at open; an index newer than the code is refused.
- **A migration is one-way.** A logbook that brings the index to a new schema
  version leaves it unreadable to the release before, which refuses it as
  newer. Before deploying a release that migrates (schema version 4 came with
  paddock#76, and 5 with the schema 5 issue), stop logbook and copy
  `index.duckdb` (with `index.duckdb.wal`, if there is one) out of the data
  directory: [Schema 5](#schema-5-and-its-backfill) has the steps. Rolling back needs that
  copy put back: the sessions and laps written since are lost from the index,
  and the lap files already written stay under `sessions/`, where a later lap
  write of the same lap replaces them.
- **Staged frames.** `frames_pending` (keyed by `src`, `session_id`, `run`
  and `seq`) holds the frames of laps not yet written, each as its protobuf
  encoding with the ecu run that published it. A `seq` counts within its run,
  so the frames of two runs of one session never mix. A lap's write deletes its own
  frames there, all but its last, in the transaction that writes its row: the
  next lap reads that last frame, and an earlier lap written later still finds
  its own. A session's end deletes what is left. A session's `run` is the run
  of its first frame, so a frame of another run never joins it, unless that
  run continued the session.
- **Continued sessions.** `session_runs` (keyed by `src`, `session_id` and
  `run`) holds each later ecu run that continued a session after an ecu
  restart (paddock#76): its first `seq` of the session (`from_seq`, the
  event's `at`), the lap the gap fell in (`from_lap`) and the event's time.
- **A session's summary.** Until a session ends, its complete laps and best
  lap are worked out from its lap rows. From `session.ended` on, the summary
  columns (`laps_completed`, `best_lap_ms`, `final_position`, `end_reason`,
  `ended_at`, and the end event's own columns below) are the event's alone:
  ending it again, or a lap written after the end, leaves them as they were.
- **Enums by name.** `type` and `session_kind` hold a `SessionType` and
  `end_reason` an `EndReason` as v1 spelt them: the name without its prefix,
  in lower case (`lap_race`, `solo_timed`; `restarted`,
  `next_session_started`, `input_ended`). A value from a newer contract than
  logbook's is stored as its bare number (`"9"`), in `type`, `end_reason` and
  `session_kind` alike.
- **The events' own session fields** (contracts v0.1.4, paddock#276), kept
  apart from the columns `logbook` works out from the laps:
  `started_fuel_at_start` (the fuel on the session's first frame),
  `last_seq` (the `seq` of its last frame), `ended_fuel_used` (the fuel the
  session used, by ecu's count), `ended_final_position` (the position on its
  last frame, absent outside a field) and `session_kind` (ecu's final verdict
  on the type, under v1's name for it; `type` is its verdict on the first
  frame). Each is null in a row written before logbook took them, and for an
  event from contracts v0.1.3, which carries none of them: a `session.ended`
  with no `last_seq`, or a `session.started` with no start fuel. A start fuel
  of 0 is stored as 0 when the session's type is one contracts v0.1.4 added
  (`lap_race` to `solo_timed`), and as null otherwise. So a real 0 L from a
  v0.1.4 or later start sent as `unknown`, or with a type added after
  `solo_timed`, is stored as null; the lasting fix is to make the field
  `optional` in contracts.
- **`final_position` is not v1's.** v1's `final_position` was the position on
  the session's last frame, which is what `ended_final_position` holds, but
  for a solo session, where v1 stored GT7's 1 and ecu leaves it out.
  `logbook`'s `final_position` is the finishing position when there is one,
  else the last lap's position. A known difference for the parity harness
  (paddock#40), which compares against v1's rows.
- **`ended_at`** is the end event's own time, which the caller passes:
  `session.ended`'s `ce-time`, so a replay or a catch-up after a restart
  records when the session ended, not when `logbook` read it.
- **`fuel_at_start`** is the first lap's starting fuel. For a session joined
  part way it can differ from v1's, which was the fuel when the session began;
  `started_fuel_at_start` is that fuel, from `session.started`.
- **What the pit wall reads** (schema 5). `sessions.summary` is
  `session.ended`'s `SessionSummary`, whole, as protobuf JSON (DuckDB's `JSON`
  type), from the first end only, as the other summary columns. `car_name` is
  `session.started`'s, then the summary's. `track_id`, `track_name` and
  `track_sure` are the summary's track verdict, sure when its confidence is
  probable or confirmed, as contracts' `LiveTrack.sure` and v1's
  `sure_track_id` read it. `posted_at` is `card.posted`'s: when podium posted
  the session's results card, the first post kept. `laps.pit_role` is the lap's
  `PitRole` by name (`none`, `in_lap`, `out_lap`, `in_and_out`) and
  `counts_for_best` its own flag; both are null for an event with no pit role.
  logbook works none of them out: they are what ecu and podium publish. Each is
  null in a row written before schema 5 until the backfill fills it.
- **Lap files.** `seq` and v1's frame columns, holding the frames in the lap's
  `seq` range that are in race, and the finished frame that closes a race. Each
  file's key-value metadata records `frame_schema_version`.
- **Writes are safe to repeat.** A lap's file is written to a temporary file
  and renamed, before its row; rows are upserted. Writing the same lap twice
  leaves the same file, byte for byte, and the same row.

`logbook export --data DIR --laps [--src SRC]` prints the stored laps of one
source (`live` by default) as JSON lines, one object per lap with the `laps`
columns, ordered by session start and lap: what the parity harness compares.
`TWR_DATA_DIR` stands in for `--data`. It takes the lock, so it refuses while
`logbook` runs on the same directory.

## Schema 5 and its backfill

`logbook backfill --data DIR [--nats URL]` fills schema 5's columns for what
logbook stored before it. It opens the store, which migrates it, reads
`session.ended`, `lap.completed` and `card.posted` back from `EVENTS` with an
ephemeral ordered consumer, from the stream's first message to its last when
the backfill began, and sets each column only where it is still null, on rows
the store already holds. A fetch that comes back empty ends it only once the
server has none of the three pending, and it gives up, exiting 1, after
10 min. It adds no session, lap or lap file and changes no
other column, so it is safe to run again: a second run changes nothing. It
logs `backfill done` with the events read and the sessions, laps and cards it
filled. `TWR_DATA_DIR` and `TWR_NATS_URL` stand in for the flags. It takes the
lock, so logbook must be stopped. `EVENTS` keeps 30 days, so a session whose
events have aged out stays without them.

On a cluster, with the StatefulSet of paddock's base:

1. **Back up the index.**
   - If the stack is deployed by Argo CD with self-heal on, turn self-heal off
     for its application first, or it scales logbook straight back up:
     `kubectl -n argocd patch application <app> --type merge -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}'`.
   - Stop logbook: `kubectl -n <namespace> scale statefulset/logbook --replicas=0`,
     and wait for its pod to go
     (`kubectl -n <namespace> wait --for=delete pod/logbook-0`).
   - Copy `index.duckdb`, and `index.duckdb.wal` if there is one
     (`index.duckdb*`), off the volume. The image is distroless, with no shell
     or `tar`, and logbook is stopped, so copy from the volume itself: from
     the storage's host directory (with a local-path provisioner, the
     PersistentVolume's `spec.hostPath.path` or `spec.local.path`), or from a
     pod of its own, such as busybox, that mounts the claim `data-logbook-0`
     (`kubectl cp` from that pod).
2. **Run the backfill** as a Job on the volume, with the release's image that
   brings schema 5 and the StatefulSet's user and environment, and the
   command `logbook backfill --data /data --nats nats://nats:4222`. It
   migrates the index, then fills it. Its log ends with `backfill done`, which
   names the stream's last sequence (`last`), the last it read (`last_read`)
   and what it filled; a backfill that stops short exits 1 and says where.
3. **Roll that release:** bump the image where the stack pins it (paddock's
   base, then a hard refresh of the Argo CD application), scale logbook back
   to 1, and turn self-heal on again. A logbook of the release before refuses
   the migrated index, so it must not start in between.

Rolling back needs the copy put back, as for any migration.

## Running

`logbook run --data DIR [--nats URL] [--frames-timeout D]` feeds the store from
NATS until it gets SIGINT or SIGTERM (paddock#38). The environment stands in
for the flags: `TWR_DATA_DIR`, `TWR_NATS_URL` (`nats://127.0.0.1:4222` when
unset) and `TWR_FRAMES_TIMEOUT` (`10s`); `TWR_HEALTH_ADDR` (`:8080`) is where
`GET /healthz` listens, and `LOG_LEVEL` (`info`) the lowest level logged. It
retries the server until it answers, and logs one line at start with the
number of sessions in its store and its schema version.

- **Consumers.** It creates two durable pull consumers at start, or updates
  them to the settings in its code: `logbook-frames` on `TELEMETRY`
  (`twr.v1.*.frame.gt7`, ack all) and `logbook-events` on `EVENTS`
  (`twr.v1.*.evt.>`, ack explicit), both delivering everything the stream
  holds, with a 30 s ack wait, 1,000 acks pending and at most 20 deliveries of
  a message. The streams themselves come from paddock's bootstrap
  (`deploy/nats`).
- **Frames.** A session's first frame is the frame of its source whose `seq`
  is the session's `first_seq` and whose `ce-time` is its `started_at`; the
  session takes that frame's ecu run (from its `ce-id`, `<run>:frame:<seq>`).
  ecu drops frames while its connection is down, so if that frame is missing
  the session takes the run of the earliest frame that fits its start: one of
  its first 1,800 frames by `seq` (30 s at 60 Hz), with a `ce-time` no earlier
  than `started_at` and within 5 s of where a 60 Hz run from that start puts
  it. An older run's frames are before `started_at`, and an earlier session of
  a run still going is too far back by `seq`, so neither fits. A newer run that
  starts within a few seconds of a session's start can fit, so a fitting frame
  never takes a session from the run its first frame gave it, and a session
  already on the frame's run takes the frame first. A session with none of its
  first 30 s of frames gets no run, and its laps are skipped as gone (below).
  Each fetched batch is placed: of the open sessions of its source whose first
  `seq` in the frame's run (for a run that continued a session, its `at`) is
  at or before its own, latest first, the frame goes to the first one that is
  on its run, or has no run yet and whose first frame it is or fits.
  The batch is staged in `frames_pending` and acked once that commits. A frame
  outside every open session is acked and not staged.
- **Read-back.** The two streams are read apart, so a session's first frames
  can pass through before its `session.started` does. When a session opens
  without its first frame, logbook reads its source back from `TELEMETRY`
  with an ephemeral ordered consumer, from 1 min before `started_at` up to the
  last frame its own consumer has been given, and stages the run's frames from
  the first frame on, or failing it from the earliest frame that fits. Where
  two runs carry the first frame itself (two replays of one recording under
  one source), it takes the later. A stop is checked between
  fetches, and the ordered consumer is deleted when the read is done.
- **After an ecu restart** (paddock#76). A graceful stop of ecu publishes no
  `session.ended`: the session stays open in logbook, however long the gap,
  until ecu continues it or ends it (`engineer_stopped`). logbook never ends
  a session itself. `session.continued` binds the session to its `run_id` from
  its `at` on: that run's frames from `at` join the session, kept apart from
  the earlier run's though their `seq`s overlap, and are read back from
  `TELEMETRY` from 1 min before the event's `ce-time`, as for a start. ecu
  numbers the laps on across the gap, and logbook takes them as they come:
  from the gap's lap on (`gap.lap`), a lap's `first_seq` and `last_seq` count
  in the later run's `seq`s, and the lap is written from that run's frames. A
  `session.continued` delivered again, or for a run already recorded, changes
  nothing; one for a session that has ended is acked, and one for a session
  never started is dropped with a warning.
- **Events.** `session.started` opens the session. `lap.completed` waits
  until a frame of its run at or beyond the lap's `last_seq` is staged, then the lap is
  written from its staged frames. `session.ended` waits for the complete laps its
  summary lists (`laps`; from an ecu that lists none, laps 1 to
  `laps_completed`), for at most an ack wait and the frames timeout from its
  own arrival (40 s), then ends the session at its `ce-time`. A
  `lap.completed` that comes after its session's end has been applied still
  writes its row and leaves the summary as the end made it. The session's
  staged frames went at the end, so the lap's frames are read back from
  `TELEMETRY`; those the stream no longer holds are missing. Each event is
  acked after its write commits. `card.posted` sets the session's `posted_at`
  at once, without waiting for the session's end. Any other type is acked and
  ignored. One that cannot be read, or a lap, end or card of a session never
  started, is dropped with a warning.
- **Missing frames.** A lap whose frames stop coming is written after the
  frames timeout with those there are: its row's `frames` says how many, and a
  warning, `lap written without all its frames`, names the source, session, lap
  and how many were missing. The timeout runs from the lap's event or the
  session's last staged frame, whichever is later, so a catch-up that is still
  streaming a lap's frames does not cut it short.
- **Sessions whose frames are gone.** `TELEMETRY` keeps 2 h of frames and
  `EVENTS` 30 d of events, so a first start, or one after a long stop, reads
  sessions whose frames have aged out. A session whose first frame never comes
  takes no frames at all, and its laps are not written: each is acked with the
  warning `lap not written: its session's frames are gone`. Its start and end
  are still recorded.
- **Restarts.** Nothing is acked before it is committed, so a restart resumes
  from the staged frames and the events not yet acked. A redelivered event
  changes nothing (a lap already written is acked and skipped; the store's
  writes are upserts), and a redelivered frame overwrites its own staged row.
  A store failure stops logbook with the message unacked; a message the store
  refuses on 5 deliveries is dropped with the error `the store refused a
  message on every delivery; dropped`, so it does not stop logbook at every
  start. On SIGTERM it stops fetching, finishes the write in progress and
  starts no other, hands the events it holds back to the server, closes the
  store and exits 0, also when the signal comes while it is starting.
- **`GET /healthz`** answers 200 while it is connected and both consumers are
  bound, and 503 otherwise, with the same JSON either way: `status`,
  `version`, `connected`, `consumers` (each consumer's name and whether it is
  bound), `laps_written` since start and `last_write` (`null` before the first).

The image runs `logbook run` with its data directory at `/data`, a volume to
mount; pass `TWR_NATS_URL`. `/healthz` answers 503 until the server and the
streams are there.

DuckDB needs cgo: the image is built with `CGO_ENABLED=1` and runs from
distroless's `cc` image, which carries glibc and libstdc++. Building or testing
on a laptop needs a C and C++ toolchain (`gcc`, `g++`).

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.

The tests of `logbook run` (`internal/runner`) run a real `nats-server -js`
as a process of its own: the release paddock pins (`v2.15.1`), downloaded
once from its GitHub release into the user's cache directory, and checked
against the SHA-256 sums in `internal/runner/natsserver_test.go` on every
run. AC2's kill is a real one: a test runs logbook as a process of its own
and kills it with SIGKILL.
`TWR_TEST_NATS_SERVER` names a binary of that release to use instead. They
create `TELEMETRY` and `EVENTS` from `internal/runner/testdata/streams`, exact
copies of paddock's `deploy/nats/streams` at the commit the tests name, which
a test checks by their sums.

The store and the runner are also tested on real recordings: what ecu
v0.1.4's `ecu replay --speed max` published for v1's
`real-nuc-suzuka-pit-stop-2026-10-15` and `real-nuc-suzuka-no-stop-2026-10-15`,
captured once from NATS into [`testdata/replays/`](testdata/replays)
(`suzuka-pit-stop-2026-10-15.jsonl.gz` and `suzuka-no-stop-2026-10-15.jsonl.gz`)
as gzipped JSON lines (subject, headers, payload). The tests publish them as they
were captured, so they need neither ecu nor the recordings, and compare the
laps with v1's. [`capture.go`](testdata/replays/capture.go) there says where
they came from and captures them again; a test pins their sums.

## Conventions

The ones every Go component shares, from the
[`chassis`](https://github.com/those-who-run/chassis) template:

- **Configuration from the environment**, read once at start-up into a typed
  struct; an unset variable takes its default and a value that can't be read
  stops the process. The example reads `ADDR` (`:8080`) and
  `LOG_LEVEL` (`info`).
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs.
- **A health endpoint**, `GET /healthz`, which answers 200 while the process can
  serve. The example service, which `logbook` with no command still runs,
  reads `ADDR` for it; `logbook run` reads `TWR_HEALTH_ADDR` and answers by
  its own rule (above).

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-logbook:X.Y.Z` once the maintainer approves it in
the `release` environment.
