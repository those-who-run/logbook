---
name: twr-test-audit
description: The gate every new or changed test passes before it is committed, and the audit for tests already in a diff. Use it whenever a change adds, edits or removes a test.
---

# Auditing a test

A test earns its place by failing when the behaviour breaks. Run every new or
changed test through this gate.

## Four questions before adding a test

1. **What observable behaviour does it protect?** Name it in a sentence a user of
   the component would recognise. "The function returns what the function returns"
   is not one.
2. **What credible regression makes it fail?** Name a change someone might really
   make.
3. **Why does existing coverage miss that?** If an existing test already fails for
   that regression, extend it instead.
4. **Does it need a seam that exists only for the test?** If yes, look for a way
   through the real caller first.

A test with no answer to one of these is not added.

## A regression test fails on the old code

For a bug fix, show the test failing before the fix:

1. Commit the test on its own, or keep it apart from the fix.
2. Run it against the code without the fix and keep the failing output.
3. Apply the fix and run it again.

Quote both results in the pull request. A regression test that was never seen to
fail proves nothing.

## Tests that are not valid

- A test that confirms behaviour its own mock, stub or fixture invented.
- A test built on a packet, an event or a state the game or the bus cannot
  produce.
- A test that asserts on an implementation detail a correct refactor would change.
- A test that passes when the code under test is deleted.

Rewrite or remove these, and say so.

## Prefer the real path

- Telemetry behaviour is pinned by a recording of a real drive, replayed.
- Extend an existing end-to-end or replay test through its real caller before
  writing a new harness.
- Fixtures live under `testdata/`, or in the place the repository's `AGENTS.md`
  names.

## Never to get to green

- Never delete, skip or loosen a test, and never lower a coverage floor. Fix the
  cause.
- If a test really is wrong, change it in the open: the pull request has a line
  starting `Tests weakened:` that names the test and says why.
- A skipped test fails the check in these repositories. Do not add a skip.

## Documentation changes

No tests. Use the link check and read the rendered page.
