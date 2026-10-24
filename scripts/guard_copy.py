"""Check that this repository's copy of the agent guard is the canonical one.

The guard and the settings that run it live once, in those-who-run/.github's
``agent/``, and each repository copies them unchanged (``agent/README.md`` there).
This checks the copies here byte for byte against ``agent/`` at the commit this
repository's CI pins for ``checks.yml``, so there is one pinned ``.github`` version to bump:
Renovate's bump of that pin fails this check until the copies are brought up to the
same commit, in the same PR.

- ``.claude/hooks/guard.py`` is ``agent/guard.py``;
- ``.claude/settings.json`` is ``agent/settings.json``;
- ``.claude/guard.json``, this repository's own values, loads with the guard's own reader,
  since a config the guard can't read blocks every Bash command.

It fetches the two files from raw.githubusercontent.com over HTTPS, so it needs the
network. ``make test`` and CI's ``test`` job run it. Lifted from
those-who-run/paddock's scripts/guard_copy.py (paddock#20 R3).

    python3 scripts/guard_copy.py
"""

from __future__ import annotations

import importlib.util
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CI = Path(".github/workflows/ci.yml")
CANONICAL = "those-who-run/.github"
COPIES = {
    "agent/guard.py": Path(".claude/hooks/guard.py"),
    "agent/settings.json": Path(".claude/settings.json"),
}
CONFIG = Path(".claude/guard.json")

PIN = re.compile(
    r"^\s*uses:\s*" + re.escape(CANONICAL) + r"/\.github/workflows/checks\.yml@([0-9a-f]{40})\b",
    re.M,
)


def pinned_commit(ci_text: str) -> str:
    """The commit of those-who-run/.github that ci.yml calls checks.yml at."""
    pins = set(PIN.findall(ci_text))
    if len(pins) != 1:
        raise ValueError(
            f"{CI} should call {CANONICAL}'s checks.yml at one full commit SHA; found {sorted(pins) or 'none'}"
        )
    return pins.pop()


class FetchError(Exception):
    """A canonical file couldn't be fetched; the message says which and why."""


def fetch(commit: str, path: str) -> bytes:
    """A file of those-who-run/.github at a commit."""
    url = f"https://raw.githubusercontent.com/{CANONICAL}/{commit}/{path}"
    try:
        with urllib.request.urlopen(url, timeout=30) as response:  # noqa: S310 (fixed https URL)
            return response.read()
    except (urllib.error.URLError, TimeoutError) as e:
        # A 404 is a pin that names no commit of .github; anything else is most
        # likely the network. One line either way, as the other problems are.
        if isinstance(e, urllib.error.HTTPError):
            e.close()  # it holds the response open
        reason = e.reason if isinstance(e, urllib.error.URLError) else e
        raise FetchError(
            f"could not fetch {url} ({reason}): check that the checks.yml pin in {CI} "
            "is a commit of those-who-run/.github, and the network"
        ) from None


def config_problem(guard: Path, config: Path) -> str | None:
    """Why the guard can't read the config, or None when it can."""
    spec = importlib.util.spec_from_file_location("repo_guard", guard)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules["repo_guard"] = module  # dataclasses look their module up
    spec.loader.exec_module(module)
    try:
        module.load_config(str(config))
    except module.ConfigError as e:
        return f"{config}: the guard can't read it: {e}"
    return None


def check(root: Path = ROOT, get=fetch) -> list[str]:
    """What differs from the canonical files, one message each."""
    try:
        commit = pinned_commit((root / CI).read_text(encoding="utf-8"))
    except ValueError as e:
        return [str(e)]
    problems = []
    for source, copy in COPIES.items():
        try:
            want = get(commit, source)
        except FetchError as e:
            return [str(e)]
        have = (root / copy).read_bytes() if (root / copy).exists() else None
        if have != want:
            problems.append(
                f"{copy} is not {CANONICAL}'s {source} at {commit[:12]}, the commit {CI} pins: "
                "copy it unchanged from there"
            )
    if not problems and (reason := config_problem(root / COPIES["agent/guard.py"], root / CONFIG)):
        problems.append(reason)
    return problems


def main(root: Path = ROOT) -> int:
    problems = check(root)
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        return 1
    print(f"the guard and settings in .claude/ are {CANONICAL}'s agent/ at the commit {CI} pins")
    return 0


if __name__ == "__main__":
    sys.exit(main())
