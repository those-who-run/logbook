"""Checks on .pre-commit-config.yaml that no linter makes.

Lifted unchanged from those-who-run/paddock, which took it, but for this docstring,
from those-who-run/.github's tests/test_precommit_config.py. Every external hook runs code on the maintainer's
machine at each commit, so it is pinned to a commit SHA with the tag in a `frozen:`
comment, as in v1
(tests/test_claude_hooks.py::test_external_hooks_are_pinned_to_a_commit). zizmor and
actionlint don't read this file, and Renovate keeps a pin but doesn't stop a hand
edit to `rev: v6.0.0`.

Standard library only. `make test` and CI's `test` job run it.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CONFIG = ROOT / ".pre-commit-config.yaml"

# `repo: local` and `repo: meta` have no rev; every other repo must have one.
REPO = re.compile(r"^\s*-\s*repo:\s*(\S+)\s*$")
REV = re.compile(r"^\s*rev:\s*(.*)$")
PINNED = re.compile(r"[0-9a-f]{40}\s+# frozen: v\d+(\.\d+)+")


def bad_pins(text: str) -> list[str]:
    """What is wrong with the external hooks' revs in a pre-commit config, if anything."""
    problems = []
    repo = None
    seen_rev = True
    for number, line in enumerate(text.splitlines(), 1):
        if m := REPO.match(line):
            if not seen_rev:
                problems.append(f"{repo}: no rev")
            repo = m.group(1)
            seen_rev = repo in ("local", "meta")
        elif m := REV.match(line):
            seen_rev = True
            if not PINNED.fullmatch(m.group(1).strip()):
                problems.append(
                    f"line {number} ({repo}): rev must be a 40-character commit SHA "
                    f"followed by `# frozen: vX.Y.Z`, not {m.group(1).strip()!r}"
                )
    if not seen_rev:
        problems.append(f"{repo}: no rev")
    return problems


class ExternalHooksArePinned(unittest.TestCase):
    def test_this_config(self):
        text = CONFIG.read_text()
        self.assertTrue(any(REV.match(x) for x in text.splitlines()), "no rev found")
        self.assertEqual(bad_pins(text), [])

    def test_a_tag_is_refused(self):
        self.assertTrue(bad_pins("repos:\n  - repo: https://x/y\n    rev: v6.0.0\n"))

    def test_a_sha_without_frozen_is_refused(self):
        self.assertTrue(bad_pins(f"repos:\n  - repo: https://x/y\n    rev: {'a' * 40}\n"))

    def test_a_short_sha_is_refused(self):
        self.assertTrue(bad_pins("repos:\n  - repo: https://x/y\n    rev: 3e8a870  # frozen: v6.0.0\n"))

    def test_a_missing_rev_is_refused(self):
        self.assertTrue(bad_pins("repos:\n  - repo: https://x/y\n    hooks: []\n"))

    def test_a_pinned_rev_and_local_pass(self):
        text = (
            "repos:\n"
            f"  - repo: https://x/y\n    rev: {'a' * 40}  # frozen: v0.11.0.1\n"
            "  - repo: local\n    hooks: []\n"
        )
        self.assertEqual(bad_pins(text), [])


if __name__ == "__main__":
    unittest.main()
