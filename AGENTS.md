# Agents and contributors

`logbook` is part of Those Who Run v2; [`README.md`](README.md) says what it is.

The rules for every v2 repository live once, in paddock's
[`AGENTS.md`](https://github.com/those-who-run/paddock/blob/main/AGENTS.md), and apply here in full, for
coding agents and for people. This file adds only what is specific to this
repository, and never repeats or weakens a rule from there.

## Issues

Issues are off here: every issue lives in
[those-who-run/paddock](https://github.com/those-who-run/paddock/issues). Read one with
`gh issue view N -R those-who-run/paddock --comments`. A PR here says
`Closes those-who-run/paddock#N.` only when every acceptance criterion is met,
and `Part of those-who-run/paddock#N` otherwise, as paddock's `AGENTS.md` says.

## Definition of done

`make check` passes. It runs what CI runs: the commit hooks on every file
(CI's `checks / checks`) and `make test` (CI's `test`): every internal Markdown
link and anchor resolves, the guard and settings in `.claude/` are
`those-who-run/.github`'s `agent/` at the commit CI pins, the external commit
hooks are pinned by commit SHA, and the tests of those checks. It needs
`python3`, `pre-commit` and the network. Docs change in the same PR as what
they describe.

It holds no language code yet. The code, and this file's rules for its
language (tool versions, formatting, generated files, where dependencies
are declared and locked), arrive from the `chassis` template
(those-who-run/paddock#21, #22) or with later issues.

## Make targets

- `make check`: everything a PR must pass, as CI runs it.
- `make test`: the repository's own checks, CI's `test` job, while iterating.
- `make hooks`: installs the commit hooks. A new clone or worktree runs it
  once, so the hooks run on every commit and commit message.

## The guard

`.claude/settings.json` and `.claude/hooks/guard.py` are copies: a change to
them is made in `those-who-run/.github`, then copied here unchanged with the
bump of the `checks.yml` pin in `.github/workflows/ci.yml`.
`.claude/guard.json` holds this repository's own values.
