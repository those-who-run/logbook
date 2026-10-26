"""Check every internal link and anchor in the repository's Markdown.

Lifted from those-who-run/paddock's scripts/docs_links.py (paddock#20), itself from
v1 (those-who-run/those-who-run, scripts/docs_links.py, #338 R4). ``make test``, CI's ``test`` job and the ``docs-links`` commit hook run
it. It reads every Markdown file in the repository (but ``CHANGELOG.md``, which
release-please writes, and what lives under ``.git``, ``.claude``, where Claude
Code keeps its worktrees, or ``build``, which ``make`` writes), and for every link
or image in them, Markdown or HTML (``href``, ``src``, ``srcset``), checks that:

- a relative path names a file or directory that exists (percent-encoded, as in
  ``a%20b.md``, it is decoded first; one starting ``/`` is from the repository's
  root, as GitHub reads it);
- a ``#fragment`` names a heading (or an HTML ``id``/``name``) in the Markdown file
  it points to, by GitHub's rules: lower case, punctuation dropped, spaces to
  hyphens, ``-1``, ``-2`` … after repeats.

External URLs (``http:``, ``https:``, ``mailto:`` …) are not fetched: it runs
offline, in well under a second. Links inside code blocks and code spans are not
links. Each failure names the file, the line and what is missing.

    python3 scripts/docs_links.py [FILE ...]
"""

from __future__ import annotations

import os
import re
import sys
from functools import cache
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parent.parent
SKIP = {"CHANGELOG.md"}
# Not the repository's own Markdown: git's internals, Claude Code's worktrees,
# which are other checkouts, and what `make` writes into build/ (a virtual
# environment's packages among it).
SKIP_DIRS = {".git", ".claude", "build"}

FENCE = re.compile(r"^\s*(```|~~~)")
CODE_SPAN = re.compile(r"(`+)(?:(?!\1).)+?\1")
INLINE = re.compile(r"!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
REFERENCE = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+\"[^\"]*\")?\s*$")
HTML = re.compile(r"\b(?:href|src|srcset)=\"([^\"]+)\"")
HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$")
HTML_ID = re.compile(r"\b(?:id|name)=\"([^\"]+)\"")
EXTERNAL = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")


def markdown_files(root: Path = ROOT) -> list[Path]:
    """Every Markdown file in ``root``, but the ones skipped above."""
    found = []
    for directory, dirnames, filenames in os.walk(root):
        # Pruned in place, so the walk never enters them: each worktree is a full
        # checkout.
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        found += [
            Path(directory, f) for f in filenames if f.endswith(".md") and f not in SKIP
        ]
    return sorted(found)


def prose(text: str) -> list[tuple[int, str]]:
    """``(line number, line)`` for the lines outside code blocks."""
    lines, fenced = [], False
    for n, line in enumerate(text.splitlines(), 1):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if not fenced:
            lines.append((n, line))
    return lines


def slug(heading: str) -> str:
    """GitHub's anchor for a heading's text."""
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", heading)  # a link: its text
    text = re.sub(r"<[^>]+>", "", text).replace("`", "")
    text = re.sub(r"[^\w\- ]", "", text.strip().lower())
    return text.replace(" ", "-")


@cache
def anchors(path: Path) -> frozenset[str]:
    """Every anchor a Markdown file has: its headings' slugs and HTML ids."""
    found: set[str] = set()
    seen: dict[str, int] = {}
    text = path.read_text(encoding="utf-8")
    for _, line in prose(text):
        if m := HEADING.match(line):
            base = slug(m.group(2))
            n = seen.get(base, 0)
            seen[base] = n + 1
            found.add(base if n == 0 else f"{base}-{n}")
        found.update(HTML_ID.findall(CODE_SPAN.sub("", line)))
    return frozenset(found)


def targets(line: str) -> list[str]:
    """The link targets on a line, leaving out code spans."""
    line = CODE_SPAN.sub("", line)
    found = INLINE.findall(line) + HTML.findall(line)
    if m := REFERENCE.match(line):
        found.append(m.group(1))
    out = []
    for t in found:
        # srcset may list several "url size" pairs.
        out += [part.split()[0] for part in t.split(",") if part.strip()] if " " in t else [t]
    return out


def check_file(path: Path, root: Path = ROOT) -> list[str]:
    """The broken links in one file, one message each."""
    problems = []
    name = path.relative_to(root).as_posix()
    for n, line in prose(path.read_text(encoding="utf-8")):
        for target in targets(line):
            if EXTERNAL.match(target) or target.startswith("//"):
                continue
            file_part, _, fragment = target.partition("#")
            local = unquote(file_part)
            if not local:
                dest = path
            elif local.startswith("/"):
                dest = (root / local.lstrip("/")).resolve()
            else:
                dest = (path.parent / local).resolve()
            if not dest.exists():
                problems.append(f"{name}:{n}: {target}: no such file {file_part}")
                continue
            if fragment and dest.suffix == ".md" and fragment.lower() not in anchors(dest):
                where = dest.relative_to(root).as_posix() if dest.is_relative_to(root) else dest
                problems.append(f"{name}:{n}: {target}: no heading #{fragment} in {where}")
    return problems


def main(argv: list[str] | None = None, root: Path = ROOT) -> int:
    args = sys.argv[1:] if argv is None else argv
    files = [Path(a).resolve() for a in args] if args else markdown_files(root)
    problems = [p for f in files for p in check_file(f, root)]
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        print(f"{len(problems)} broken link(s): fix the link or the heading", file=sys.stderr)
        return 1
    print(f"links ok in {len(files)} Markdown files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
