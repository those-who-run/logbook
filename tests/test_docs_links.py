"""The link check (scripts/docs_links.py), ported from v1's tests/test_docs.py to
unittest so that it needs nothing beyond python3. Each case writes a small site to a
temporary directory; the first runs the check on this repository."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

_spec = importlib.util.spec_from_file_location("docs_links", ROOT / "scripts" / "docs_links.py")
assert _spec is not None and _spec.loader is not None
docs_links = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(docs_links)


class ThisRepository(unittest.TestCase):
    def test_the_repositorys_links_resolve(self):
        err = io.StringIO()
        with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(docs_links.main([]), 0, err.getvalue())

    def test_the_check_reads_every_markdown_file(self):
        names = {p.relative_to(ROOT).as_posix() for p in docs_links.markdown_files()}
        self.assertLessEqual(
            {"README.md", "AGENTS.md", "CLAUDE.md"},
            names,
        )

    def test_claude_code_worktrees_and_the_changelog_are_left_out(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("README.md", "CHANGELOG.md", ".claude/worktrees/x/README.md",
                         "build/venv/LICENSE.md", ".github/a.md"):
                (root / name).parent.mkdir(parents=True, exist_ok=True)
                (root / name).write_text("# x\n")
            names = {p.relative_to(root).as_posix() for p in docs_links.markdown_files(root)}
            self.assertEqual(names, {"README.md", ".github/a.md"})


class Site(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = Path(self._tmp.name)
        (self.root / "docs").mkdir()
        (self.root / "docs" / "operations.md").write_text(
            "# Operations\n\n## Releasing\n\nText.\n\n## Releasing\n\n## `make check` and `pit_lane`\n"
        )
        (self.root / "docs" / "img.png").write_bytes(b"")

    def broken(self, text: str) -> list[str]:
        page = self.root / "docs" / "page.md"
        page.write_text(text)
        docs_links.anchors.cache_clear()
        return docs_links.check_file(page, self.root)

    def test_links_that_resolve_pass(self):
        self.assertEqual(
            self.broken(
                "[a](operations.md#releasing) [b](operations.md#releasing-1)"
                " [c](operations.md#make-check-and-pit_lane) [d](#here) ![e](img.png)\n"
                '<img src="img.png"> [f](https://example.com/x#y)\n'
                "[g]: operations.md#releasing\n\n## Here\n"
            ),
            [],
        )

    def test_a_reference_definition_is_checked(self):
        self.assertEqual(
            self.broken("[g]: operations.md#shipping\n"),
            ["docs/page.md:1: operations.md#shipping: no heading #shipping in docs/operations.md"],
        )

    def test_a_percent_encoded_path_is_decoded(self):
        (self.root / "docs" / "a b.md").write_text("# A b\n")
        self.assertEqual(
            self.broken("[x](a%20b.md#a-b) ![y](a%20c.png)\n"),
            ["docs/page.md:1: a%20c.png: no such file a%20c.png"],
        )

    def test_a_root_relative_link_is_from_the_repository_root(self):
        self.assertEqual(
            self.broken("[x](/docs/operations.md#releasing) [y](/docs/gone.md)\n"),
            ["docs/page.md:1: /docs/gone.md: no such file /docs/gone.md"],
        )

    def test_a_renamed_heading_fails_naming_the_file_and_the_link(self):
        self.assertEqual(
            self.broken("See [Releasing](operations.md#shipping).\n"),
            ["docs/page.md:1: operations.md#shipping: no heading #shipping in docs/operations.md"],
        )

    def test_a_missing_file_fails(self):
        self.assertEqual(
            self.broken("\n![x](missing.png) [y](../nowhere.md)\n"),
            [
                "docs/page.md:2: missing.png: no such file missing.png",
                "docs/page.md:2: ../nowhere.md: no such file ../nowhere.md",
            ],
        )

    def test_an_anchor_in_the_same_file_is_checked(self):
        self.assertEqual(
            self.broken("[x](#nope)\n\n## Yes\n"),
            ["docs/page.md:1: #nope: no heading #nope in docs/page.md"],
        )

    def test_links_in_code_are_not_links(self):
        self.assertEqual(self.broken("```\n[x](nope.md)\n```\n\n`[y](nope.md)`\n"), [])

    def test_external_links_are_not_fetched(self):
        self.assertEqual(self.broken("[x](https://example.invalid/gone.md#nope)\n"), [])

    def test_the_check_fails_with_a_message(self):
        page = self.root / "docs" / "page.md"
        page.write_text("[x](gone.md)\n")
        docs_links.anchors.cache_clear()
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(docs_links.main([str(page)], root=self.root), 1)
        self.assertIn("1 broken link(s)", err.getvalue())


if __name__ == "__main__":
    unittest.main()
