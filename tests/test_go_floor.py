"""The coverage floor is one number in two places: the Makefile's COVERAGE_FLOOR,
which `make check` and `make test` use, and the coverage-floor ci.yml passes to
go.yml, which CI's `go / check` uses. go.yml's test guard watches the second; this
keeps the first from drifting away from it. Standard library only."""

from __future__ import annotations

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

MAKEFILE = re.compile(r"^COVERAGE_FLOOR\s*:?=\s*(\S+)\s*$", re.M)
CI = re.compile(r"^\s+coverage-floor:\s*(\S+)\s*$", re.M)


def floors(makefile: str, ci: str) -> tuple[list[str], list[str]]:
    return MAKEFILE.findall(makefile), CI.findall(ci)


class CoverageFloor(unittest.TestCase):
    def test_the_makefile_and_ci_agree(self):
        make, ci = floors(
            (ROOT / "Makefile").read_text(encoding="utf-8"),
            (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8"),
        )
        self.assertEqual(len(make), 1, "the Makefile should set COVERAGE_FLOOR once")
        self.assertEqual(len(ci), 1, "ci.yml should pass coverage-floor to go.yml once")
        self.assertEqual(make, ci)

    def test_a_difference_is_found(self):
        make, ci = floors("COVERAGE_FLOOR := 80\n", "    with:\n      coverage-floor: 90\n")
        self.assertNotEqual(make, ci)


if __name__ == "__main__":
    unittest.main()
