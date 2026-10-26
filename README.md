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
index.duckdb                            the sessions, laps and frames_pending tables
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
- **Staged frames.** `frames_pending` (keyed by `src`, `session_id` and
  `seq`) holds the frames of laps not yet written, each as its protobuf
  encoding with the ecu run that published it. A lap's write deletes its
  frames there in the transaction that writes its row; a session's end deletes
  what is left. A session's `run` is the run of its first staged frame, so a
  frame of another run never joins it.
- **A session's summary.** Until a session ends, its complete laps and best
  lap are worked out from its lap rows. From `session.ended` on, the summary
  columns (`laps_completed`, `best_lap_ms`, `final_position`, `end_reason`,
  `ended_at`) are the event's alone: ending it again, or a lap written after
  the end, leaves them as they were.
- **`ended_at`** is the end event's own time, which the caller passes:
  `session.ended`'s `ce-time`, so a replay or a catch-up after a restart
  records when the session ended, not when `logbook` read it.
- **`fuel_at_start`** is the first lap's starting fuel. For a session joined
  part way it can differ from v1's, which was the fuel when the session began,
  until `session.started` carries the start fuel (Prost).
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
  holds, with a 30 s ack wait and 1,000 acks pending. The streams themselves
  come from paddock's bootstrap (`deploy/nats`).
- **Frames.** Each fetched batch is placed: a frame of source `src` goes to the
  open session of that source whose first `seq` is the latest at or before its
  own, among those with no run yet or the frame's run. The batch is staged in `frames_pending` and
  acked once that commits. A frame outside every open session is acked and not
  staged; the latest 600 of them per source are kept in memory, so a session
  whose `session.started` comes after its first frames (the two streams are read
  apart) claims them when it opens.
- **Events.** `session.started` opens the session; `lap.completed` waits until a
  frame at or beyond the lap's `last_seq` is staged, then the lap is written
  from its staged frames; `session.ended` waits for the session's laps, then
  ends it at its `ce-time`. Each is acked after its write commits. Any other
  type is acked and ignored; one that cannot be read, or a lap or end of a
  session that is not open, is dropped with a warning.
- **Missing frames.** A lap whose frames stop coming is written after the
  frames timeout with those there are: its row's `frames` says how many, and a
  warning, `lap written without all its frames`, names the source, session, lap
  and how many were missing. The timeout runs from the lap's event or the
  session's last staged frame, whichever is later, so a catch-up that is still
  streaming a lap's frames does not cut it short.
- **Restarts.** Nothing is acked before it is committed, so a restart resumes
  from the staged frames and the events not yet acked. A redelivered event
  changes nothing (a lap already written is acked and skipped; the store's
  writes are upserts), and a redelivered frame overwrites its own staged row.
  On SIGTERM it stops fetching, finishes the write in progress, hands the
  events it holds back to the server, closes the store and exits 0.
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
once from its GitHub release into the user's cache directory and checked
against the SHA-256 sums in `internal/runner/natsserver_test.go`.
`TWR_TEST_NATS_SERVER` names a binary of that release to use instead. They
create `TELEMETRY` and `EVENTS` from `internal/runner/testdata/streams`, exact
copies of paddock's `deploy/nats/streams` at the commit the tests name, which
a test checks by their sums.

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
