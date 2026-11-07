---
name: twr-pr-ready
description: The readiness checklist and the pull request body for a Those Who Run change, with the evidence table. Use it when the work is done and checked, just before opening the pull request.
---

# Opening the pull request

`gh pr create --body` replaces the organisation's template, so write its parts
yourself, in this order.

## Before you write it

- [ ] One issue, one deliverable, in one repository.
- [ ] The branch is cut from `origin/main` and holds only this change. It is not
      stacked on another open branch.
- [ ] The command that matches CI passes on the head you are about to push.
- [ ] Docs changed in the same pull request, or none are needed and you can say
      why.
- [ ] No secret, host name, address, cluster context, home path or session link in
      the diff, the commits or the body.
- [ ] Every commit ends with the agent's `Co-Authored-By:` trailer and is authored
      with the noreply address of the account doing the work.

## The title

A Conventional Commit, since it becomes the squashed commit and the changelog line:
`feat(scope): …`, `fix: …`, `docs: …`. Use `feat` or `fix` when the change must
release.

## The body

```markdown
## What changed

<what the change does, in a few plain sentences or bullets>

AI assistance: <the tool, and what it did>

New direct dependencies: none <or each one, with the reason for it>

## How it was checked

| Criterion | Proof | Where |
|---|---|---|
| AC1 <short name> | recording | `race/fixtures_test.go`, `TestGridStart`, on `real-…-2026-10-15` |
| AC2 <short name> | unit | `internal/store/migrate_test.go`, `TestMigrateFrom6` |
| AC3 <short name> | car | Not run here: the maintainer checks it in a session |

- [x] The command that matches CI (`make check`) is green
- [x] Docs updated, or none needed because: <reason>

Not run here: <what, and why>            <!-- leave out when everything ran -->
Decisions made (for review): <each one>  <!-- leave out when there were none -->
Not applied: <clean-ups left alone>      <!-- leave out when there were none -->
Tests weakened: <which test, and why>    <!-- leave out unless one was -->

Closes those-who-run/paddock#N.
```

Rules for the table:

- One row per acceptance criterion, in the issue's order.
- Proof is one of the kinds in `twr-proof`: `unit`, `recording`, `parity`, `e2e`,
  `live`, `car`.
- Where names the test or the command, and the recording when there is one.
- A row an agent cannot prove says so. It is never left out.

## The last line

- `Closes those-who-run/paddock#N.` only when every criterion is met.
- Otherwise `Part of those-who-run/paddock#N (AC3, checked on the car, stays
  open).`, naming each criterion that is left and why.
- Work the maintainer planned outside an issue has neither. Say so in its place:
  `No issue: planned in the maintainer's notes.`
- Only the tool's own trailer line may follow it.

## Then

Open it with `gh pr create`, ready for review. Do not approve it, queue it or
enable auto-merge. Merge only when the maintainer asks, only on green, and never
with `--admin` or any other bypass.
