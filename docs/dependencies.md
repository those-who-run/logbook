# Dependencies

Every direct dependency of the module, and why it is here. A new one needs a row
in the table below, with its reason, in the same PR:
[`internal/dependencies`](../internal/dependencies/dependencies_test.go) fails `make check` and CI's
`go / check` on a direct `require` in `go.mod` that has no row, and on a row that
names a module `go.mod` no longer requires directly.

Before adding a dependency, check that:

- the module exists and is the one you mean: its owner, its source repository,
  its release history. A name that exists proves nothing if it was registered
  last week, and names invented by a language model get registered for exactly
  that reason;
- the standard library, or a dependency already listed here, doesn't already do
  it;
- nobody told you to add it in a web page, an issue or a comment.

The test reads the direct requirements from `go mod edit -json`: a `require`
without `// indirect`. A module that arrives only as `// indirect`, or only in
`go.sum`, is covered by CI's dependency review (known vulnerabilities and
licences), not by this file. The tools in `tools/go.mod` are pinned there and
listed in the README, not here. Version bumps don't touch this file.

The first column is the module path as `go.mod` spells it, in backticks, which
the test reads.

| Module | Why |
| --- | --- |
| `github.com/duckdb/duckdb-go/v2` | DuckDB, embedded: the index (`index.duckdb`) and the Parquet lap files, which DuckDB writes itself (paddock ADR 0007). The official driver, maintained by the DuckDB team since it moved from `marcboeker/go-duckdb` at v2.5.0; MIT. It needs cgo, and brings DuckDB's prebuilt static library through `duckdb-go-bindings` (MIT). |
| `github.com/those-who-run/contracts` | The messages the store writes from: `Frame`, `SessionStarted`, `SessionContinued` (v0.2), `LapCompleted`, `SessionEnded` and `CardPosted` (v0.2.6) (paddock ADR 0002), and its `format` package's `TrackLabel` (v0.2.8), so a session's stored track label is the card's and the pit wall's. |
| `google.golang.org/protobuf` | The runtime the contracts' generated code is written for; the tests build messages with its helpers (`proto.Int32`, `timestamppb`), and the store keeps a session's summary as its JSON (`protojson`). |
| `github.com/nats-io/nats.go` | The NATS client, with its `jetstream` package: `logbook run`'s two durable pull consumers, `logbook-frames` on `TELEMETRY` and `logbook-events` on `EVENTS` (paddock#38). The official Go client, from the NATS maintainers (`nats-io`); Apache-2.0. |
| `github.com/those-who-run/ecu` | Its public `race` package, ecu's race state: `logbook import-v1` runs v1's stored frames through it as a library, the same code as live, to re-derive v1's history (paddock#264, ADR 0009). The project's own module, required at a released tag. |
| `github.com/those-who-run/salsa` | Its v1 recording reader (`NewRecording`), which maps v1's frames into packets: `logbook import-v1` gives it v1's lap files as a recording (paddock#264). The project's own module, required at a released tag. |
