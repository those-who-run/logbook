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
index.duckdb                            the sessions and laps tables
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

DuckDB needs cgo: the image is built with `CGO_ENABLED=1` and runs from
distroless's `cc` image, which carries glibc and libstdc++. Building or testing
on a laptop needs a C and C++ toolchain (`gcc`, `g++`).

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.

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
  serve.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-logbook:X.Y.Z` once the maintainer approves it in
the `release` environment.
