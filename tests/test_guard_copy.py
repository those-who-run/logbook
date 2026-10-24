"""The guard copy check (scripts/guard_copy.py), offline: the canonical files come from
a stand-in for the fetch. `make guard` runs the real check against GitHub."""

from __future__ import annotations

import importlib.util
import io
import json
import shutil
import tempfile
import unittest
import urllib.error
from contextlib import redirect_stderr
from unittest import mock
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

_spec = importlib.util.spec_from_file_location("guard_copy", ROOT / "scripts" / "guard_copy.py")
assert _spec is not None and _spec.loader is not None
guard_copy = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(guard_copy)

SHA = "a" * 40
CI = f"""jobs:
  checks:
    uses: those-who-run/.github/.github/workflows/checks.yml@{SHA} # v0.2.0
"""


class Pin(unittest.TestCase):
    def test_this_repositorys_ci_pins_one_commit(self):
        sha = guard_copy.pinned_commit((ROOT / guard_copy.CI).read_text())
        self.assertRegex(sha, r"^[0-9a-f]{40}$")

    def test_a_tag_is_not_a_pin(self):
        with self.assertRaises(ValueError):
            guard_copy.pinned_commit(CI.replace(SHA, "v0.2.0"))

    def test_two_different_pins_are_refused(self):
        with self.assertRaises(ValueError):
            guard_copy.pinned_commit(CI + CI.replace(SHA, "b" * 40).split("\n", 1)[1])


class Copies(unittest.TestCase):
    """A repository with this repository's .claude/ and a ci.yml pinned at SHA."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = Path(self._tmp.name)
        shutil.copytree(ROOT / ".claude", self.root / ".claude",
                        ignore=shutil.ignore_patterns("worktrees", "settings.local.json"))
        (self.root / guard_copy.CI).parent.mkdir(parents=True)
        (self.root / guard_copy.CI).write_text(CI)
        self.asked: list[tuple[str, str]] = []

    def canonical(self, commit: str, path: str) -> bytes:
        """What .github holds at `commit`: here, this repository's own copies."""
        self.asked.append((commit, path))
        return (ROOT / guard_copy.COPIES[path]).read_bytes()

    def test_the_same_files_pass_and_are_fetched_at_the_pin(self):
        self.assertEqual(guard_copy.check(self.root, self.canonical), [])
        self.assertEqual(sorted(self.asked), [(SHA, "agent/guard.py"), (SHA, "agent/settings.json")])

    def test_a_changed_guard_fails(self):
        guard = self.root / ".claude/hooks/guard.py"
        guard.write_bytes(guard.read_bytes() + b"\n# changed\n")
        problems = guard_copy.check(self.root, self.canonical)
        self.assertEqual(len(problems), 1)
        self.assertIn(".claude/hooks/guard.py is not those-who-run/.github's agent/guard.py", problems[0])

    def test_a_missing_settings_file_fails(self):
        (self.root / ".claude/settings.json").unlink()
        problems = guard_copy.check(self.root, self.canonical)
        self.assertEqual(len(problems), 1)
        self.assertIn(".claude/settings.json", problems[0])

    def test_a_config_the_guard_cannot_read_fails(self):
        (self.root / ".claude/guard.json").write_text('{"branch": "main", "brnach": "main"}\n')
        problems = guard_copy.check(self.root, self.canonical)
        self.assertEqual(len(problems), 1)
        self.assertIn("the guard can't read it", problems[0])

    def test_no_pin_fails_without_fetching(self):
        (self.root / guard_copy.CI).write_text("jobs: {}\n")
        self.assertEqual(len(guard_copy.check(self.root, self.canonical)), 1)
        self.assertEqual(self.asked, [])

    def test_a_failed_fetch_is_one_line_not_a_traceback(self):
        def refused(*_args, **_kwargs):
            raise urllib.error.HTTPError("https://example.invalid", 404, "Not Found", {}, io.BytesIO())

        with mock.patch.object(guard_copy.urllib.request, "urlopen", refused):
            with self.assertRaises(guard_copy.FetchError) as raised:
                guard_copy.fetch(SHA, "agent/guard.py")
            message = str(raised.exception)
            self.assertIn(f"raw.githubusercontent.com/those-who-run/.github/{SHA}/agent/guard.py", message)
            self.assertNotIn("\n", message)

            with mock.patch.object(guard_copy, "fetch", side_effect=guard_copy.FetchError(message)):
                stderr = io.StringIO()
                with redirect_stderr(stderr):
                    self.assertEqual(guard_copy.main(self.root), 1)
            self.assertEqual(stderr.getvalue(), message + "\n")

    def test_no_network_is_one_line_too(self):
        def offline(*_args, **_kwargs):
            raise urllib.error.URLError(OSError("Name or service not known"))

        with mock.patch.object(guard_copy.urllib.request, "urlopen", offline):
            with self.assertRaises(guard_copy.FetchError) as raised:
                guard_copy.fetch(SHA, "agent/settings.json")
        self.assertIn("Name or service not known", str(raised.exception))


class ThisRepositorysConfig(unittest.TestCase):
    """This repository's own values in .claude/guard.json."""

    def test_the_guard_config_values(self):
        config = json.loads((ROOT / guard_copy.CONFIG).read_text(encoding="utf-8"))
        self.assertEqual(
            config,
            {
                "branch": "main",
                "release_tags": ["v*"],
                "release_workflow": "release.yml",
                "owner": "those-who-run",
            },
        )


if __name__ == "__main__":
    unittest.main()
