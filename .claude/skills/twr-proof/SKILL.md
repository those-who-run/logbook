---
name: twr-proof
description: Chooses the kind of evidence each acceptance criterion needs, by what the change touches, and how to report it honestly. Use it before writing code and again when filling the pull request's evidence table.
---

# Choosing the proof

Different evidence proves different claims. Decide, for each acceptance criterion,
which kind it needs, and report exactly that kind and nothing stronger.

## The kinds

| Kind | What it is | What it proves |
|---|---|---|
| `unit` | A test beside the code: a `_test.go` file, a pytest test, a Vitest test | The logic of one function or package |
| `recording` | A recorded drive replayed through the code: a fixture under `testdata/`, a file in paddock's `recordings/`, an outing in `outings` | What the system detects and says for real telemetry |
| `parity` | paddock's harness (`make parity`, `make parity-events`, `make parity-pitwall`) replaying one recording through v1 and v2 | That v2 gives the same laps, events and callouts as v1 |
| `e2e` | The composed stack: paddock's `make compose-it` and `make nats-it`, pitwall's `make web-e2e` | That the components work together through their real callers |
| `live` | The maintainer's cluster | That the released images behave when deployed |
| `car` | The maintainer's car, phone or tablet | That it works in a real session |

An agent never claims `live` or `car`. Only the maintainer produces them. A
criterion that needs one stays open: the pull request says `Part of`, names the
criterion and says who checks it.

## Which kind for which change

| The change touches | Needs |
|---|---|
| Telemetry decoding, lap, session, pit or event detection, callout rules | `recording`. Add `parity` where v1 has the same behaviour |
| What the engineer says or when | `recording`, with the fixture in the pull request |
| A contract: `proto/`, `subjects.yaml` | contracts' `make proto` (buf's breaking-change check) and its round-trip tests. Consumers bump in their own pull requests |
| The store or its schema | `unit`, and a migration test that starts from the previous schema |
| An HTTP route, the API client or the pit wall's screens | `unit`, pitwall's `make web-check`, and `e2e` |
| Voice or phrasing | `unit`. `make voice-test` is outside `make check`: say whether it ran |
| The deploy manifests | paddock's `make check-deploy`. `live` is the maintainer's |
| Workflows, the guard, scripts in `those-who-run/.github` | The tests in `tests/` and `agent/tests/` |
| Documentation only | The link check. No tests |

When a change fits two rows, it needs both.

## A recording is the referee

- A claim about telemetry names the recording it came from and the numbers seen.
  Anything not checked against a recording is marked **Unverified:**.
- A v2 result that differs from v1's is a bug in v2 until a recording shows v1 was
  wrong.
- Use real recordings. Never invent a packet, a field value or a session state the
  game cannot produce.

## Reporting

- Say what you ran, as a command, and what it printed that proves the point.
- A green command with skipped cases proves nothing about the skipped cases. List
  them.
- What you could not run goes under "Not run here", with the reason (it needs NATS,
  a container runtime, the voice model, the car).
- If the evidence is weaker than the criterion asks for, say so. Do not round up.
