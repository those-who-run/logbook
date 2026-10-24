# Logbook

The store for Those Who Run v2: it subscribes to the bus and writes
sessions and laps to the data volume it owns.
It is the only writer on that volume, and everything else reads through
its services ([ADR 0003](https://github.com/those-who-run/paddock/blob/main/docs/adr/0003-the-store.md) in paddock).

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here closes
its paddock issue with `Closes those-who-run/paddock#N.`

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.
