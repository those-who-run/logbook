"""The workflows behind `main`'s required checks run in the merge queue: ci.yml and,
with an image, image.yml (`image / build`) trigger on merge_group, and their
concurrency cancels only a superseded PR run. A queue whose required checks never
report never merges. The workflows that release or score (release-pr.yml,
release.yml, scorecard.yml) stay out of the queue. Standard library only, so the
trigger block is read as text. Copied from chassis (those-who-run/chassis#23)."""

from __future__ import annotations

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"

REQUIRED = ("ci.yml", "image.yml")
NOT_IN_THE_QUEUE = ("release-pr.yml", "release.yml", "scorecard.yml")

MERGE_GROUP = "  merge_group:\n    types: [checks_requested]\n"
CANCEL = "  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n"


def on_block(text: str) -> str:
    """The workflow's top-level `on:` mapping, up to the next top-level key."""
    found = re.search(r"^on:\n((?:[ #].*\n|\n)*)", text, re.M)
    return found.group(1) if found else ""


def present(names: tuple[str, ...]) -> list[Path]:
    # A repository without an image has no image.yml or release.yml.
    return [WORKFLOWS / n for n in names if (WORKFLOWS / n).is_file()]


class MergeQueue(unittest.TestCase):
    def test_ci_is_there(self):
        self.assertTrue((WORKFLOWS / "ci.yml").is_file())

    def test_the_required_checks_run_in_the_queue(self):
        for path in present(REQUIRED):
            with self.subTest(workflow=path.name):
                on = on_block(path.read_text(encoding="utf-8"))
                self.assertIn(MERGE_GROUP, on)
                self.assertIn("  pull_request:\n", on)

    def test_only_a_superseded_pr_run_is_cancelled(self):
        for path in present(REQUIRED):
            with self.subTest(workflow=path.name):
                self.assertIn(CANCEL, path.read_text(encoding="utf-8"))

    def test_releases_and_scorecard_stay_out_of_the_queue(self):
        for path in present(NOT_IN_THE_QUEUE):
            with self.subTest(workflow=path.name):
                self.assertNotIn("merge_group", on_block(path.read_text(encoding="utf-8")))

    def test_a_missing_trigger_is_found(self):
        self.assertNotIn(MERGE_GROUP, on_block("on:\n  pull_request:\n  push:\n    branches: [main]\njobs:\n"))
        self.assertIn(MERGE_GROUP, on_block("on:\n  pull_request:\n" + MERGE_GROUP + "\njobs:\n"))


if __name__ == "__main__":
    unittest.main()
