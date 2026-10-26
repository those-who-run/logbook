"""Total statement coverage from a Go cover profile, against the floor.

    python3 scripts/go_coverage.py <cover.out> <floor>

Run from the module's directory, after `go test -coverprofile`. It leaves out
generated files, those with Go's `// Code generated … DO NOT EDIT.` line before
their package clause (https://go.dev/s/generatedcode), as go.yml's go_check.py
does in those-who-run/.github (from its v0.2.4: those-who-run/.github#27 and #29), so
`make check` and CI's `go / check` measure the same statements. It prints the
total and fails below the floor. Standard library only.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import tempfile

GENERATED = re.compile(r"^// Code generated .* DO NOT EDIT\.$")
TOTAL = re.compile(r"^total:\s+\(statements\)\s+([\d.]+)%\s*$", re.MULTILINE)


def is_generated(path: str) -> bool:
    """Whether the Go file has the generated-code line before its package clause.

    As go/ast's IsGenerated: only comments come before the package clause, line or
    block, and any other text there (the clause itself, even after a block comment
    on its line) ends the search."""
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            in_block = False
            for line in f:
                rest = line.rstrip("\r\n")
                while True:
                    if in_block:
                        end = rest.find("*/")
                        if end < 0:
                            break
                        rest, in_block = rest[end + 2 :], False
                    rest = rest.lstrip()
                    if not rest:
                        break
                    if rest.startswith("//"):
                        if GENERATED.match(rest):
                            return True
                        break
                    if not rest.startswith("/*"):
                        return False
                    rest, in_block = rest[2:], True
    except OSError:
        return False
    return False


def drop_generated(lines: list[str], dirs: dict[str, str]) -> tuple[list[str], list[str]]:
    """The profile without generated files' blocks, and those files. The profile
    names each file by its package's import path; dirs maps that to a directory."""
    kept: list[str] = []
    dropped: list[str] = []
    seen: dict[str, bool] = {}
    for line in lines:
        name = "" if line.startswith("mode:") else line.split(":", 1)[0]
        if name and name not in seen:
            pkg, _, base = name.rpartition("/")
            path = os.path.join(dirs[pkg], base) if pkg in dirs else ""
            seen[name] = bool(path) and is_generated(path)
            if seen[name]:
                dropped.append(os.path.relpath(path))
        if name and seen[name]:
            continue
        kept.append(line)
    return kept, sorted(dropped)


def package_dirs() -> dict[str, str] | None:
    """Each package's directory, by import path; None when `go list` fails."""
    out = subprocess.run(
        ["go", "list", "-f", "{{.ImportPath}}\t{{.Dir}}", "./..."],
        capture_output=True,
        text=True,
    )
    if out.returncode != 0:
        print(out.stdout + out.stderr, end="", file=sys.stderr)
        return None
    return dict(line.split("\t", 1) for line in out.stdout.splitlines() if "\t" in line)


def statements(lines: list[str]) -> int:
    """How many statements a cover profile's blocks hold."""
    return sum(int(line.split()[-2]) for line in lines if line.strip() and not line.startswith("mode:"))


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    profile, floor = argv[0], float(argv[1])
    dirs = package_dirs()
    if dirs is None:
        print("go list failed, so generated files can't be left out of coverage", file=sys.stderr)
        return 1
    with open(profile, encoding="utf-8") as f:
        kept, dropped = drop_generated(f.readlines(), dirs)
    if dropped:
        print(f"Coverage leaves out {len(dropped)} generated file(s): " + ", ".join(dropped))
    if not statements(kept):
        print("Coverage: no statements are left once generated files are left out, so the floor doesn't apply.")
        return 0
    # Beside the profile, with the rest of the build's output.
    with tempfile.NamedTemporaryFile(
        "w", suffix=".out", dir=os.path.dirname(os.path.abspath(profile)), delete=False, encoding="utf-8"
    ) as f:
        f.writelines(kept)
        filtered = f.name
    try:
        out = subprocess.run(
            ["go", "tool", "cover", f"-func={filtered}"], capture_output=True, text=True
        )
    finally:
        os.unlink(filtered)
    match = TOTAL.search(out.stdout)
    if out.returncode != 0 or not match:
        print(out.stdout + out.stderr, end="", file=sys.stderr)
        print("no total coverage in the profile", file=sys.stderr)
        return 1
    total = float(match.group(1))
    print(f"Coverage: {total:.1f}% of statements (the floor is {floor:g}%).")
    if total < floor:
        print("coverage is below the floor", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
