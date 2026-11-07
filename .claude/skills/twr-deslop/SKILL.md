---
name: twr-deslop
description: A behaviour-neutral clean-up of the diff before review. Use it after the checks pass and before the pull request is opened. It removes what a careful author would not have left in, and changes nothing a test could notice.
---

# Cleaning the diff

Work only on lines this change added or edited. Leave the rest of the file alone,
however tempting.

## Remove

- Debug output, temporary logging and commented-out code.
- Comments that say what the code does. Keep the ones that say why.
- A helper that duplicates one the repository already has. Use the existing one.
- An abstraction with one caller and no second one in the issue.
- Checks for states that cannot happen, and error handling that only rewraps.
- Formatting changes, renames and reordering outside what the issue asks for.
- A `TODO` with no issue behind it. Either do it, or name the paddock issue.
- Files that should not be in the diff: build output, editor files, local notes.

## Fix the words

- British English, plain sentences. No exclamation marks, no marketing words.
- Numbers carry their units: `3 s`, `20 Hz`, `120 °C`.
- No host names, addresses, cluster contexts, home paths or tokens. Use
  placeholders such as `<host>` and documentation addresses such as `192.0.2.x`.
- No session links.

## The one rule

If a clean-up could change behaviour, do not make it. List it in the pull request
under "Not applied" with the reason, and let the reviewer decide.

## Afterwards

Run the command that matches CI again. A clean-up that breaks a test was not
behaviour-neutral: undo it.
