#!/usr/bin/env python3
"""Claude Code's PreToolUse guard on Bash for the organisation's repositories.

The canonical copy is `agent/guard.py` in those-who-run/.github; each repository
copies it unchanged to `.claude/hooks/guard.py` (see `agent/README.md` there). It
started as those-who-run/those-who-run#352. What differs between repositories (the
protected branch, the release tags, the release workflow, the organisation) is read
from `.claude/guard.json`, next to the `hooks` directory that holds this file.

Claude Code runs it before every Bash command an agent sends (`.claude/settings.json`)
with the hook input as JSON on stdin. It never runs in the maintainer's own shell.
Permission rules match the command's text and are easy to sidestep (`git -C . commit`,
`bash -c '...'`), so this parses the command the way a shell would: it splits `;`,
`&&`, `||`, `|` and newlines, follows `$(...)`, backquotes, `bash -c`, `eval`, here-
documents fed to a shell, `env`/`VAR=` prefixes and wrappers such as `sudo` or
`xargs`, and then checks each simple command against the rules below.

  * block (exit 2, the reason on stderr): the agent is told why and to ask the
    maintainer;
  * ask (JSON `permissionDecision: "ask"`): the maintainer is asked before it runs;
  * nothing: Claude Code's own permission rules decide.

It fails closed: if the guard itself errors, the command is blocked, and if
`.claude/guard.json` is missing or can't be read, every Bash command is blocked until
it is fixed with the Edit tool. A command it can't parse that names one of the guarded
programs, and a command whose program is a variable (`$g ...`) or an alias set in the
same command, are sent to `ask`.

It is a guard against mistakes, not a sandbox. It can't see into a script file, a
program that runs git itself (`python3 -c 'subprocess.run(...)'`), a file it opens
by a path it builds, or a command decoded at run time (`eval "$(echo ... | base64
-d)"`). CI and the protected branch's ruleset are the backstop.

Run it on a command to see the verdict:

    echo '{"tool_name": "Bash", "tool_input": {"command": "git commit -n"}}' \\
        | python3 .claude/hooks/guard.py

Plain Python 3.9+, standard library only: it runs before any `.venv` exists, and in
repositories that have no Python toolchain at all.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import sys
import time
from dataclasses import dataclass, field

ALLOW, ASK, BLOCK = 0, 1, 2

# How deep `bash -c`, `eval` and `$(...)` are followed before the guard gives up and asks.
MAX_DEPTH = 3

# A command the guard can't follow is sent to `ask` when it names one of these.
SENSITIVE = re.compile(r"(?<![\w.-])(git|gh|kubectl|oc|helm|docker|podman|pre-commit)(?![\w-])")

ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")

SHELLS = {"bash", "sh", "zsh", "dash", "ksh", "fish"}
# Shell keywords that may start a simple command and run what follows.
KEYWORDS = {"!", "{", "}", "if", "then", "else", "elif", "do", "while", "until", "time"}
# Programs that run the command named in their arguments.
WRAPPERS = {
    "sudo", "doas", "nice", "ionice", "nohup", "timeout", "stdbuf", "command", "builtin",
    "exec", "xargs", "watch", "setsid", "chronic", "unbuffer", "flock", "caffeinate",
    "strace", "ltrace", "unshare", "chroot", "runuser",
}  # fmt: skip


@dataclass(frozen=True)
class Verdict:
    level: int
    rule: str = ""
    reason: str = ""


ALLOWED = Verdict(ALLOW)


def block(rule: str, reason: str) -> Verdict:
    return Verdict(BLOCK, rule, reason)


def ask(rule: str, reason: str) -> Verdict:
    return Verdict(ASK, rule, reason)


def worst(verdicts: list[Verdict]) -> Verdict:
    """The strictest verdict; the first of equals, so the reason is the earliest."""
    out = ALLOWED
    for v in verdicts:
        if v.level > out.level:
            out = v
    return out


# ---------------------------------------------------------------------------
# The repository's configuration: `.claude/guard.json`.
# ---------------------------------------------------------------------------


class ConfigError(Exception):
    pass


@dataclass(frozen=True)
class Config:
    branch: str = "main"
    release_tags: tuple[str, ...] = ("v*",)
    release_workflow: str = "release.yml"
    owner: str = "those-who-run"


CONFIG_PATH = os.path.normpath(
    os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, "guard.json")
)

# The rules read the configuration from here rather than as an argument, so that every
# rule keeps the one signature the tables below rely on. decide() sets it for each run.
CONFIG = Config()


# A branch name as the config may give it: short, as `git push origin <branch>` names
# it. Anything else (a space, a `refs/` prefix) would never equal the ref being pushed,
# and so would quietly protect nothing.
BRANCH = re.compile(r"[A-Za-z0-9._/-]+")
# A release tag pattern: a non-empty prefix and one trailing *. A bare `*` would leave
# `v1.0.0` unprotected while covering only tags that start with a digit.
TAG_PATTERN = re.compile(r"[A-Za-z0-9._/-]+\*")


def unique_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """A JSON object that names each key once. json's default keeps the last of a repeated
    key, so a diff could show one value while a later line overrides it unseen."""
    out: dict[str, object] = {}
    for k, v in pairs:
        if k in out:
            raise ValueError(f"the key {k!r} is given more than once")
        out[k] = v
    return out


def load_config(path: str) -> Config:
    """The configuration in `path`. Keys it leaves out take v1's values; an unknown key
    is an error, because a misspelt one would otherwise fall back to a default unseen."""
    try:
        # utf-8-sig: an editor that writes a byte order mark shouldn't block every command.
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f, object_pairs_hook=unique_keys)
    except (OSError, ValueError) as e:
        raise ConfigError(f"{path} can't be read ({e})") from e
    if not isinstance(data, dict):
        raise ConfigError(f"{path} is not a JSON object")
    unknown = sorted(set(data) - {"branch", "release_tags", "release_workflow", "owner"})
    if unknown:
        raise ConfigError(f"{path} has unknown keys: {', '.join(unknown)}")
    for key in ("branch", "release_workflow", "owner"):
        if key in data and (not isinstance(data[key], str) or not data[key].strip()):
            raise ConfigError(f"{path}: {key} must be a non-empty string")
    branch = data.get("branch", Config.branch).strip()
    if branch.startswith("refs/") or not BRANCH.fullmatch(branch):
        raise ConfigError(
            f"{path}: branch must be a short branch name such as \"main\", "
            "with no refs/ prefix, spaces or other characters outside A-Z a-z 0-9 . _ / -"
        )
    tags = data.get("release_tags", list(Config.release_tags))
    if (
        not isinstance(tags, list)
        or not tags
        or not all(isinstance(t, str) and TAG_PATTERN.fullmatch(t) for t in tags)
    ):
        raise ConfigError(
            f"{path}: release_tags must be a non-empty list of patterns, each a prefix "
            'followed by one *, such as "v*" or "gen/go/v*"'
        )
    return Config(
        branch=branch,
        release_tags=tuple(tags),
        release_workflow=data.get("release_workflow", Config.release_workflow),
        owner=data.get("owner", Config.owner),
    )


# ---------------------------------------------------------------------------
# The rules. Each takes a program's arguments (after its name) and the environment
# the command runs with, and returns a verdict. The tables are the policy: a new rule
# is a line here and a test in agent/tests/test_guard.py.
# ---------------------------------------------------------------------------

HOOKS_SKIP = (
    "the commit hooks must run, and CI runs the same checks anyway; "
    "fix what they report instead"
)


def long_option(arg: str, names: set[str] | tuple[str, ...], min_len: int = 4) -> str | None:
    """The long option in `names` that `arg` spells, allowing git's unique prefixes."""
    name = arg.split("=", 1)[0]
    if not name.startswith("--") or len(name) < min_len:
        return None
    for n in names:
        if n.startswith(name):
            return n
    return None


def short_flags(arg: str, with_value: str, optional_value: str = "") -> set[str]:
    """The flags in a short-option cluster such as `-nm`, up to one that takes a value."""
    if not arg.startswith("-") or arg.startswith("--") or len(arg) < 2:
        return set()
    out = set()
    for c in arg[1:]:
        out.add(c)
        if c in with_value or c in optional_value:
            break
    return out


# git's options before the subcommand that take a separate value.
GIT_GLOBAL_WITH_VALUE = {
    "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix",
    "--config-env", "--attr-source",
}  # fmt: skip
# Subcommands whose --no-verify skips a hook.
GIT_NO_VERIFY = {"commit", "merge", "pull", "am", "rebase", "push", "cherry-pick", "revert"}
# git commit's options that take a separate value (long) or the rest of the cluster.
COMMIT_LONG_WITH_VALUE = {
    "--message", "--file", "--reuse-message", "--reedit-message", "--template",
    "--author", "--date", "--fixup", "--squash", "--cleanup", "--trailer",
    "--pathspec-from-file",
}  # fmt: skip
COMMIT_SHORT_WITH_VALUE = "mFCct"
COMMIT_SHORT_OPTIONAL = "Su"


def hooks_path_override(key: str) -> bool:
    return key.strip().lower() == "core.hookspath"


def git_rule(args: list[str], env: dict[str, str]) -> Verdict:
    i = 0
    while i < len(args):
        a = args[i]
        if a in GIT_GLOBAL_WITH_VALUE and i + 1 < len(args):
            value = args[i + 1]
            if a in ("-c", "--config-env") and hooks_path_override(value.split("=", 1)[0]):
                return block(
                    "git-hooks-path", f"overriding core.hooksPath skips the hooks: {HOOKS_SKIP}"
                )
            i += 2
            continue
        if a.startswith("--config-env=") and hooks_path_override(a.split("=", 2)[1]):
            return block(
                "git-hooks-path", f"overriding core.hooksPath skips the hooks: {HOOKS_SKIP}"
            )
        if a.startswith("-"):
            i += 1
            continue
        break
    if i >= len(args):
        return ALLOWED
    for k, v in env.items():
        if (k.startswith("GIT_CONFIG_KEY_") and hooks_path_override(v)) or (
            k == "GIT_CONFIG_PARAMETERS" and "core.hookspath" in v.lower()
        ):
            return block(
                "git-hooks-path", f"overriding core.hooksPath skips the hooks: {HOOKS_SKIP}"
            )
    sub, rest = args[i], args[i + 1 :]
    verdicts = []
    if sub in GIT_NO_VERIFY:
        verdicts.append(git_no_verify(sub, rest, env))
    rule = GIT_RULES.get(sub)
    if rule is not None:
        verdicts.append(rule(rest, env))
    return worst(verdicts)


def takes_value(arg: str, long_with_value: set[str], short_with_value: str) -> bool:
    """Whether the argument after `arg` is its value: `-m`, `-am`, `--message`."""
    if arg in long_with_value:
        return True
    flags = short_flags(arg, short_with_value)
    return bool(flags) and arg[-1] in short_with_value


def git_no_verify(sub: str, args: list[str], env: dict[str, str]) -> Verdict:
    for i, a in enumerate(args):
        if a == "--":
            break
        commit = sub == "commit"
        if (
            commit
            and i > 0
            and takes_value(args[i - 1], COMMIT_LONG_WITH_VALUE, COMMIT_SHORT_WITH_VALUE)
        ):
            continue  # the previous option's value, such as -m's message
        if long_option(a, ("--no-verify",), min_len=len("--no-veri")):
            return block("git-no-verify", f"git {sub} --no-verify skips the hooks: {HOOKS_SKIP}")
        if commit and "n" in short_flags(a, COMMIT_SHORT_WITH_VALUE, COMMIT_SHORT_OPTIONAL):
            return block("git-no-verify", f"git commit -n skips the hooks: {HOOKS_SKIP}")
    if sub == "commit" and "SKIP" in env:
        return block("git-skip", f"SKIP= turns hooks off for a commit: {HOOKS_SKIP}")
    return ALLOWED


def git_config_rule(args: list[str], env: dict[str, str]) -> Verdict:
    reads = {"--get", "--get-all", "--get-regexp", "--list", "-l", "get", "list"}
    if any(hooks_path_override(a) for a in args) and not reads.intersection(args):
        return block("git-hooks-path", f"setting core.hooksPath moves the hooks away: {HOOKS_SKIP}")
    return ALLOWED


def say(text: str) -> str:
    """A reason with the repository's values filled in. The tables keep `{branch}` and
    the rest as placeholders because the configuration is read after they are built."""
    return text.format(
        branch=CONFIG.branch,
        tags=" or ".join(CONFIG.release_tags),
        workflow=CONFIG.release_workflow,
        owner=CONFIG.owner,
    )


RELEASE = "a {tags} tag starts {workflow}, which publishes a release"


def is_release_tag(ref: str) -> bool:
    """A ref under refs/tags/, or one a release tag pattern matches. The pattern's *
    stands for a version, so what it covers starts with a digit: `v*` matches v0.7.5
    but not `very-old` or a branch such as `fix/v-shaped-trace`."""
    ref = ref.lstrip("+")
    if ref.startswith("refs/tags/"):
        return True
    for pattern in CONFIG.release_tags:
        prefix = pattern[:-1]
        if ref.startswith(prefix) and ref[len(prefix) : len(prefix) + 1].isdigit():
            return True
    return False


# git push's options: those that take a separate value, and those the guard blocks.
PUSH_LONG_WITH_VALUE = {"--repo", "--push-option", "--receive-pack", "--exec"}
PUSH_DANGEROUS_LONG = {
    "--force": "force-pushing rewrites published history",
    "--force-with-lease": "force-pushing rewrites published history",
    "--force-if-includes": "force-pushing rewrites published history",
    "--mirror": "--mirror pushes, and deletes, every ref",
    "--delete": "deleting a remote branch or tag can't be undone from here",
    "--prune": "--prune deletes remote branches",
    "--tags": f"pushing tags: {RELEASE}",
    "--follow-tags": f"pushing tags: {RELEASE}",
    "--all": "pushing every branch pushes {branch}, which takes changes only through a PR",
    "--branches": "pushing every branch pushes {branch}, which takes changes only through a PR",
}
PUSH_DANGEROUS_SHORT = {
    "f": "force-pushing rewrites published history",
    "d": "deleting a remote branch or tag can't be undone from here",
}


def is_protected_branch(ref: str) -> bool:
    # Without regard to case: a config that says "Main" almost certainly means main, and
    # blocking a push to a branch that differs from it only in case costs little.
    ref, branch = ref.lower(), CONFIG.branch.lower()
    return ref in (branch, f"refs/heads/{branch}")


def git_push_rule(args: list[str], env: dict[str, str]) -> Verdict:
    positionals, i = [], 0
    while i < len(args):
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a.startswith("--"):
            hit = long_option(a, tuple(PUSH_DANGEROUS_LONG))
            if hit:
                return block("git-push", say(PUSH_DANGEROUS_LONG[hit]))
            if a in PUSH_LONG_WITH_VALUE:
                i += 1
        elif a.startswith("-") and len(a) > 1:
            flags = short_flags(a, "o")
            for f in sorted(flags & set(PUSH_DANGEROUS_SHORT)):
                return block("git-push", PUSH_DANGEROUS_SHORT[f])
            if a.endswith("o"):
                i += 1  # -o <push option>
        else:
            positionals.append(a)
        i += 1
    for spec in positionals[1:]:  # after the remote
        if spec == "tag":
            return block("git-push", say(f"pushing a tag: {RELEASE}"))
        if spec.startswith("+"):
            return block("git-push", "a + refspec force-pushes, which rewrites published history")
        if spec.startswith(":"):
            return block("git-push", "an empty source deletes the remote ref")
        src, _, dst = spec.partition(":")
        if is_release_tag(src) or is_release_tag(dst):
            return block("git-push", say(f"pushing a tag: {RELEASE}"))
        if is_protected_branch(dst or src):
            return block("git-push", say("{branch} takes changes only through a PR"))
    return ALLOWED


TAG_LONG_WITH_VALUE = {
    "--message", "--file", "--local-user", "--sort", "--format", "--cleanup", "--trailer",
    "--contains", "--no-contains", "--points-at", "--merged", "--no-merged",
}  # fmt: skip
TAG_READS = {"--list", "--delete", "--verify", "--contains", "--points-at", "--merged"}


def git_tag_rule(args: list[str], env: dict[str, str]) -> Verdict:
    """Creating a release tag (`release_tags`). Listing, verifying and deleting local tags
    are fine."""
    positionals, reading, i = [], False, 0
    while i < len(args):
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a.startswith("--"):
            reading = reading or long_option(a, tuple(TAG_READS)) is not None
            if a in TAG_LONG_WITH_VALUE:
                i += 1
        elif a.startswith("-") and len(a) > 1:
            flags = short_flags(a, "mFu")
            reading = reading or bool(flags & set("ldvn"))
            if a[-1] in "mFu":
                i += 1
        else:
            positionals.append(a)
        i += 1
    if positionals and not reading and is_release_tag(positionals[0]):
        return block(
            "git-tag",
            say(f"creating a {{tags}} tag: {RELEASE}; releases go through the release process"),
        )
    return ALLOWED


GIT_RULES = {
    "config": git_config_rule,
    "push": git_push_rule,
    "tag": git_tag_rule,
}


def pre_commit_rule(args: list[str], env: dict[str, str]) -> Verdict:
    if args[:1] == ["uninstall"]:
        return block("pre-commit-uninstall", f"uninstalling the hooks skips them: {HOOKS_SKIP}")
    return ALLOWED


# Where the hooks live and what says where they are: the common .git/hooks (or a path
# built from `$(git rev-parse ...)`), and .git/config, which holds core.hooksPath.
GIT_INTERNALS = re.compile(
    r"\.git/(worktrees/[^/\s'\"]+/)?(hooks|config)(?![\w.-])"
    r"|[)`]/hooks(?![\w.-])"
    r"|--git-path[\s=]+hooks(?![\w.-])"
)
# Programs that change, move or remove the files named in their arguments.
FILE_WRITERS = {
    "rm", "rmdir", "unlink", "mv", "cp", "ln", "install", "chmod", "chown", "chgrp",
    "touch", "truncate", "tee", "dd", "shred", "sed", "perl", "ruby", "node", "python",
    "python3", "find", "rsync", "mkdir",
}  # fmt: skip
# A redirection that writes its target.
WRITE_REDIRECTIONS = {">", ">>", ">|", "&>", "&>>", "<>", ">&"}
VARIABLE = re.compile(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?")


def expand(word: str, env: dict[str, str]) -> str:
    """The word with the variables the command set itself (`d=...; rm "$d"`) filled in."""
    return VARIABLE.sub(lambda m: env.get(m.group(1), m.group(0)), word)


def git_internals(word: str, env: dict[str, str]) -> Verdict:
    if GIT_INTERNALS.search(expand(word, env)) is None:
        return ALLOWED
    return block(
        "git-hooks-dir",
        "changing .git/hooks or .git/config can turn the hooks off for every worktree: "
        + HOOKS_SKIP,
    )


def file_writer_rule(args: list[str], env: dict[str, str]) -> Verdict:
    return worst([git_internals(a, env) for a in args])


# gh: (command, subcommand) -> why it is blocked. "*" is every subcommand but `reads`.
GH_BLOCK = {
    ("release", "create"): RELEASE.replace("a {tags} tag", "a release and its {tags} tag"),
    ("release", "edit"): "releases are made by the release process, and are public",
    ("release", "delete"): "releases are made by the release process, and are public",
    ("release", "upload"): "releases are made by the release process, and are public",
    ("release", "delete-asset"): "releases are made by the release process, and are public",
    ("workflow", "run"): "a workflow run can release or publish ({workflow})",
    ("workflow", "enable"): "turning workflows on or off changes what CI enforces",
    ("workflow", "disable"): "turning workflows on or off changes what CI enforces",
    ("repo", "edit"): "it changes the repository's settings",
    ("repo", "delete"): "it deletes the repository",
    ("repo", "rename"): "it renames the repository",
    ("repo", "archive"): "it archives the repository",
    ("repo", "unarchive"): "it changes the repository's settings",
    ("alias", "set"): "an alias can hide a blocked command",
    ("alias", "import"): "an alias can hide a blocked command",
    ("auth", "token"): "it prints the maintainer's token",
    ("auth", "login"): "it changes the maintainer's GitHub login",
    ("auth", "logout"): "it changes the maintainer's GitHub login",
    ("auth", "refresh"): "it changes the token's scopes",
}
# Commands blocked whatever the subcommand, except the read-only ones listed.
GH_BLOCK_ALL = {
    "secret": ({"list"}, "it changes the repository's secrets"),
    "variable": ({"list", "get"}, "it changes the repository's variables"),
    "ruleset": ({"list", "view", "check"}, "it changes the repository's rulesets"),
}
# Public, permanent and in the maintainer's name: the maintainer sees them first.
GH_ASK = {
    ("issue", "create"), ("issue", "comment"), ("issue", "edit"), ("issue", "close"),
    ("issue", "delete"), ("pr", "create"), ("pr", "comment"), ("pr", "review"),
    ("pr", "close"), ("pr", "merge"), ("pr", "ready"),
}  # fmt: skip
# gh api: a write (any method but GET or HEAD) to one of these blocks.
GH_API_PROTECTED = re.compile(
    r"""^(
        repos/[^/]+/[^/]+/?$                       # the repository itself
      | repos/[^/]+/[^/]+/(
            rulesets | actions/permissions | environments | collaborators | invitations
          | hooks | keys | branches/[^/]+/protection | actions/secrets | actions/variables
          | releases | git/refs | git/tags | dispatches | actions/workflows/[^/]+/
          | actions/runs/[^/]+/rerun | pulls/[^/]+/merge | transfer | topics
          | vulnerability-alerts | automated-security-fixes | deployments
        )
      | orgs/[^/]+ | user(/|$)
    )""",
    re.X,
)
# GraphQL mutations that do what the blocked commands do.
GH_GRAPHQL_PROTECTED = re.compile(
    r"\b(mergePullRequest|updateRepository|deleteRepository|archiveRepository|"
    r"(create|update|delete)(Ref|RepositoryRuleset|BranchProtectionRule|Environment)|"
    r"updateRefs|createCommitOnBranch|(create|update|delete)Release)\b"
)
# What `gh issue|pr create|comment|edit|close|review` do, through the API: ask, as they do.
GH_API_POSTS = re.compile(r"^repos/[^/]+/[^/]+/(issues|pulls)(/|$)")
GH_GRAPHQL_POSTS = re.compile(
    r"\b(addComment|updateIssueComment|addPullRequestReview\w*|submitPullRequestReview|"
    r"(create|update|close|reopen|delete)(Issue|PullRequest)|addDiscussionComment)\b"
)
GH_API_WITH_VALUE = {
    "-X", "--method", "-f", "--raw-field", "-F", "--field", "-H", "--header", "--input",
    "-q", "--jq", "-t", "--template", "--hostname", "-p", "--preview", "--cache",
}  # fmt: skip


def repo_owner(repo: str) -> str:
    parts = repo.strip("/").split("/")
    return (parts[-2] if len(parts) >= 3 else parts[0]).lower()


def gh_rule(args: list[str], env: dict[str, str]) -> Verdict:
    positionals, repos, i = [], [], 0
    if env.get("GH_REPO"):
        repos.append(env["GH_REPO"])
    while i < len(args):
        a = args[i]
        if a in ("-R", "--repo") and i + 1 < len(args):
            repos.append(args[i + 1])
            i += 2
            continue
        if a.startswith("--repo="):
            repos.append(a.split("=", 1)[1])
        elif a.startswith("-R") and len(a) > 2:
            repos.append(a[2:])
        elif not a.startswith("-"):
            positionals.append(a)
        i += 1
    if not positionals:
        return ALLOWED
    cmd = positionals[0]
    sub = positionals[1] if len(positionals) > 1 else ""
    verdicts = []
    if cmd == "pr" and sub == "merge" and any(a.split("=")[0] == "--admin" for a in args):
        verdicts.append(block("gh-admin-merge", "--admin merges past the required checks"))
    if (cmd, sub) in GH_BLOCK:
        verdicts.append(block(f"gh-{cmd}-{sub}", say(GH_BLOCK[(cmd, sub)])))
    if cmd in GH_BLOCK_ALL and sub:
        reads, why = GH_BLOCK_ALL[cmd]
        if sub not in reads:
            verdicts.append(block(f"gh-{cmd}", why))
    if cmd == "run" and sub == "rerun":
        verdicts.append(
            ask("gh-run-rerun", say("re-running a run can re-run a release ({workflow})"))
        )
    if cmd == "api":
        verdicts.append(gh_api_rule(args))
    if (cmd, sub) in GH_ASK:
        verdicts.append(ask(f"gh-{cmd}-{sub}", "it posts in public, in the maintainer's name"))
    owner = CONFIG.owner.lower()
    others = sorted({r for r in repos if repo_owner(r) != owner})
    if others:
        verdicts.append(
            ask("gh-other-repo", f"it acts on {', '.join(others)}, outside {CONFIG.owner}")
        )
    return worst(verdicts)


def gh_api_rule(args: list[str]) -> Verdict:
    endpoint, method, fields, has_input, i = None, None, [], False, 0
    while i < len(args):
        a = args[i]
        name, eq, attached = a.partition("=")
        value = None
        if a in GH_API_WITH_VALUE and i + 1 < len(args):
            name, value = a, args[i + 1]
            i += 1
        elif a.startswith("--") and eq:
            value = attached
        elif a[:2] in ("-X", "-f", "-F") and len(a) > 2:
            name, value = a[:2], a[2:]
        else:
            name = a
        if name in ("-X", "--method"):
            method = (value or "").upper()
        elif name in ("-f", "--raw-field", "-F", "--field"):
            fields.append(value or "")
        elif name == "--input":
            has_input = True
        elif not a.startswith("-") and a != "api" and endpoint is None:
            endpoint = a
        i += 1
    if method is None:
        method = "POST" if fields or has_input else "GET"
    if endpoint is None or method in ("GET", "HEAD"):
        return ALLOWED
    path = endpoint.split("?", 1)[0].lstrip("/")
    if path == "graphql":
        query = " ".join(fields)
        m = GH_GRAPHQL_PROTECTED.search(query)
        if m:
            return block(
                "gh-api", f"the GraphQL mutation {m.group(0)} does what a blocked command does"
            )
        m = GH_GRAPHQL_POSTS.search(query)
        if m:
            return ask("gh-api", f"{m.group(0)} posts in public, in the maintainer's name")
        if has_input:
            return ask("gh-api", "a GraphQL request from a file the guard can't read")
        return ALLOWED
    if GH_API_PROTECTED.match(path):
        return block(
            "gh-api", f"{method} {path} changes the repository's settings, releases or refs"
        )
    if GH_API_POSTS.match(path):
        return ask("gh-api", f"{method} {path} posts in public, in the maintainer's name")
    return ALLOWED


# docker and podman: options before the subcommand that take a separate value.
CONTAINER_GLOBAL_WITH_VALUE = {
    "--config", "-c", "--context", "-H", "--host", "-l", "--log-level", "--tlscacert",
    "--tlscert", "--tlskey", "--connection", "--url", "--identity", "--root", "--runroot",
    "--storage-driver", "--storage-opt", "--cgroup-manager", "--events-backend", "--module",
    "--runtime", "--tmpdir", "--volumepath", "--imagestore", "--conmon", "--ssh",
}  # fmt: skip
HOME = os.path.expanduser("~")
# Host paths a container must not see.
MOUNT_FORBIDDEN = re.compile(
    r"^(/|~|\$HOME|\$\{HOME\}|/home|/root"
    r"|(~|\$HOME|\$\{HOME\}|/home/[^/]+|/root)/(\.kube|\.ssh|\.docker|\.config|\.netrc|\.gnupg)(/.*)?"
    r"|.*/(docker|podman)\.sock)$"
)


def forbidden_mount(spec: str) -> bool:
    if spec.startswith(("type=", "src=", "source=")) or ",src=" in spec or ",source=" in spec:
        parts = dict(p.partition("=")[::2] for p in spec.split(","))
        source = parts.get("source") or parts.get("src") or ""
    else:
        source = spec.split(":", 1)[0]
    if not source:
        return False  # an anonymous volume
    source = source.rstrip("/") or "/"
    if source == HOME or source.startswith(HOME + "/"):
        source = "~" + source[len(HOME) :]
    return MOUNT_FORBIDDEN.match(source) is not None


def container_rule(args: list[str], env: dict[str, str]) -> Verdict:
    i = 0
    while i < len(args) and args[i].startswith("-"):
        i += 2 if args[i] in CONTAINER_GLOBAL_WITH_VALUE else 1
    if i >= len(args):
        return ALLOWED
    sub, rest = args[i], args[i + 1 :]
    first = next((a for a in rest if not a.startswith("-")), "")
    if sub in ("login", "logout", "push") or (
        sub in ("image", "manifest", "artifact", "compose") and first == "push"
    ):
        return block("container-registry", "it signs in to or pushes to a registry")
    if sub == "buildx" and first == "imagetools":
        return block("container-registry", "it pushes to a registry")
    for j, a in enumerate(rest):
        if a == "--":
            break
        if a == "--push" or "type=registry" in a or a.startswith("--push="):
            return block("container-registry", "it pushes to a registry")
        if a == "--privileged" or a.startswith("--privileged="):
            return block("container-privileged", "a privileged container can reach the host")
        spec = None
        if a in ("-v", "--volume", "--mount") and j + 1 < len(rest):
            spec = rest[j + 1]
        elif a.startswith(("--volume=", "--mount=")):
            spec = a.split("=", 1)[1]
        elif a.startswith("-v") and len(a) > 2 and not a.startswith("--"):
            spec = a[2:]
        if spec is not None and forbidden_mount(spec):
            return block(
                "container-mount",
                f"mounting {spec} hands the host's files or secrets to a container",
            )
    return ALLOWED


KUBECTL_GLOBAL_WITH_VALUE = {
    "--context", "--kubeconfig", "-n", "--namespace", "--cluster", "--user", "-s", "--server",
    "--token", "--as", "--as-group", "--as-uid", "--request-timeout", "-v", "--v",
    "--cache-dir", "--certificate-authority", "--client-certificate", "--client-key",
    "--tls-server-name", "--profile", "--profile-output", "--password", "--username",
    "--log-file",
}  # fmt: skip
KUBECTL_WRITES = {
    "apply", "create", "delete", "edit", "patch", "replace", "scale", "set", "label",
    "annotate", "drain", "cordon", "uncordon", "taint", "exec", "attach", "port-forward",
    "cp", "run", "expose", "autoscale", "debug",
}  # fmt: skip
KUBECTL_ROLLOUT_WRITES = {"restart", "undo", "pause", "resume"}
KUBECTL_CONFIG_READS = {"current-context", "get-contexts", "get-clusters", "get-users", "view"}
HELM_GLOBAL_WITH_VALUE = {
    "--kube-context", "--kubeconfig", "-n", "--namespace", "--registry-config",
    "--repository-cache", "--repository-config", "--burst-limit", "--kube-apiserver",
    "--kube-as-group", "--kube-as-user", "--kube-ca-file", "--kube-token", "--qps",
    "--kube-tls-server-name", "--content-cache",
}  # fmt: skip
HELM_WRITES = {"install", "upgrade", "uninstall", "delete", "del", "un", "rollback", "test", "push"}
CLUSTER = "the cluster changes through the deployment repository and Argo CD, not from here"


def verb_and_rest(args: list[str], with_value: set[str]) -> tuple[str, list[str]]:
    """The first argument that isn't an option or an option's value, and what follows it."""
    i = 0
    while i < len(args):
        a = args[i]
        if a in with_value:
            i += 2
        elif a.startswith("-"):
            i += 1
        else:
            return a, args[i + 1 :]
    return "", []


def kubectl_rule(args: list[str], env: dict[str, str]) -> Verdict:
    verb, rest = verb_and_rest(args, KUBECTL_GLOBAL_WITH_VALUE)
    nxt, _ = verb_and_rest(rest, KUBECTL_GLOBAL_WITH_VALUE)
    if verb in KUBECTL_WRITES or (verb == "rollout" and nxt in KUBECTL_ROLLOUT_WRITES):
        return block("kubectl-write", f"kubectl {verb} changes the cluster: {CLUSTER}")
    if verb == "config" and nxt and nxt not in KUBECTL_CONFIG_READS:
        return block("kubectl-config", f"kubectl config {nxt} changes the kubeconfig: {CLUSTER}")
    if verb == "certificate" and nxt in ("approve", "deny"):
        return block("kubectl-write", f"kubectl certificate {nxt} changes the cluster: {CLUSTER}")
    return ALLOWED


def helm_rule(args: list[str], env: dict[str, str]) -> Verdict:
    verb, rest = verb_and_rest(args, HELM_GLOBAL_WITH_VALUE)
    nxt, _ = verb_and_rest(rest, HELM_GLOBAL_WITH_VALUE)
    if verb in HELM_WRITES:
        return block("helm-write", f"helm {verb} changes the cluster or a registry: {CLUSTER}")
    if verb == "registry" and nxt in ("login", "logout"):
        return block("helm-registry", "it signs in to a registry")
    return ALLOWED


# The secrets on this machine: the paths .claude/settings.json denies to the Read and
# Edit tools, which don't cover a Bash command that reads them.
_HOMES = "|".join(
    [r"~", r"\$HOME", r"\$\{HOME\}", r"/home/[^/\s'\"]+", r"/root"]
    + ([re.escape(HOME)] if HOME.startswith("/") and HOME != "/" else [])
)
SECRET_PATH = re.compile(
    r"(?<![\w.-])\.env(\.[\w.-]+)?(?![\w.-])"
    rf"|({_HOMES})/(\.aws|\.config/gcloud|\.gnupg|\.config/helm|\.docker|\.kube|\.ssh"
    r"|\.config/gh)(?![\w.-])"
    rf"|({_HOMES})/(\.claude/\.credentials\.json|\.claude\.json|\.netrc"
    r"|\.config/containers/auth\.json)(?![\w.-])"
    r"|(/run/user/[^/\s'\"]+|\$\{?XDG_RUNTIME_DIR\}?)/containers/auth\.json"
    r"|[\w-][\w.-]*\.(pem|key)(?![\w.-])"
    r"|(?<![\w.-])id_(rsa|ed25519)[\w.-]*"
)
# Programs whose arguments are mostly prose (a commit message, a PR body): only an
# argument that is a bare path is checked. echo, printf, ls and test read nothing;
# kubectl, oc and helm use a kubeconfig (`--kubeconfig ~/.kube/...`) without printing it.
SECRET_PROSE = {"git", "gh"}
SECRET_HARMLESS = {"echo", "printf", "ls", "test", "[", "true", ":", "kubectl", "oc", "helm"}
# Redirections whose target is a file (not a here-document's delimiter or text).
FILE_REDIRECTIONS = WRITE_REDIRECTIONS | {"<", "<&"}


def secret_path(word: str, env: dict[str, str]) -> Verdict:
    m = SECRET_PATH.search(expand(word, env))
    if m is None:
        return ALLOWED
    return block(
        "secret-read",
        f"{m.group(0)} holds secrets, which agents don't read or change "
        "(the deny rules in .claude/settings.json)",
    )


def secret_args(name: str, args: list[str], env: dict[str, str]) -> Verdict:
    if name in SECRET_HARMLESS:
        return ALLOWED
    if name in SECRET_PROSE:
        args = [a for a in args if not any(c.isspace() for c in a)]
    return worst([secret_path(a, env) for a in args])


# The maintainer's own jobs in those-who-run/.github: the make targets in its Makefile's
# "Maintainer only" group and the scripts behind them. They run with the gh login of
# whoever runs them, change repository settings and the organisation's project board,
# and one stores the release App's private key, so agents never run them, not even the
# read-only checks. Reading or linting a script is fine; running it is not.
MAINTAINER_ONLY = (
    "it is the maintainer's own job, run with his gh login: it reads or changes "
    "repository settings, the project board or the release App's key"
)
MAINTAINER_TARGETS = {
    "board", "board-check", "protect", "protect-check", "envs", "envs-check", "new-repo-check",
}  # fmt: skip
MAINTAINER_SCRIPTS = {"protect-repo.sh", "board-sync.sh", "release-envs.sh", "new-repo-check.sh"}
# make's options that take a separate value.
MAKE_WITH_VALUE = {
    "-C", "-f", "-I", "-o", "-W", "-E", "--directory", "--file", "--makefile",
    "--include-dir", "--old-file", "--assume-old", "--new-file", "--assume-new", "--what-if",
    "--eval",
}  # fmt: skip


def make_dry_run(args: list[str]) -> bool:
    """Only `make -n <target>`, exactly. Options that take a value (`-o -n`, `-C -n`)
    make any reading of a longer command a guess, so nothing else counts as a dry
    run; `make --dry-run board` or `make -nk board` is blocked like `make board`."""
    return len(args) == 2 and args[0] == "-n" and not args[1].startswith("-") and "=" not in args[1]


def make_rule(args: list[str], env: dict[str, str]) -> Verdict:
    if make_dry_run(args):
        return ALLOWED
    i = 0
    while i < len(args):
        a = args[i]
        if a in MAKE_WITH_VALUE:
            i += 2
            continue
        if not a.startswith("-") and not ASSIGNMENT.match(a) and a in MAINTAINER_TARGETS:
            return block("maintainer-only", f"make {a}: {MAINTAINER_ONLY}")
        i += 1
    return ALLOWED


def maintainer_script(path: str) -> Verdict:
    """Running one of the maintainer's scripts, by its path or through a shell."""
    name = os.path.basename(path)
    if name in MAINTAINER_SCRIPTS:
        return block("maintainer-only", f"{name}: {MAINTAINER_ONLY}")
    return ALLOWED


def script_rule(name: str):
    def rule(args: list[str], env: dict[str, str]) -> Verdict:
        return maintainer_script(name)

    return rule


# Program name -> rule. Programs are matched by their base name, so `/usr/bin/git`
# is `git`.
RULES = {
    "git": git_rule,
    "pre-commit": pre_commit_rule,
    **dict.fromkeys(FILE_WRITERS, file_writer_rule),
    "gh": gh_rule,
    "docker": container_rule,
    "podman": container_rule,
    "kubectl": kubectl_rule,
    "oc": kubectl_rule,
    "helm": helm_rule,
    "make": make_rule,
    "gmake": make_rule,
    **{name: script_rule(name) for name in MAINTAINER_SCRIPTS},
}


# ---------------------------------------------------------------------------
# Parsing: enough of the shell's grammar to find every simple command.
# ---------------------------------------------------------------------------


class ParseError(Exception):
    pass


@dataclass
class Token:
    kind: str  # "word", "op" or "redir"
    text: str
    subs: list[str] = field(default_factory=list)  # $(...), `...` and <(...) inside it
    heredoc: str | None = None  # the body of a << here-document


@dataclass
class Command:
    words: list[str] = field(default_factory=list)
    subs: list[str] = field(default_factory=list)
    stdin_scripts: list[str] = field(default_factory=list)  # here-documents and <<< strings
    redirects: list[tuple[str, str]] = field(default_factory=list)  # (operator, target)


OPERATORS = ("&&", "||", ";;", "|&", ";", "|", "&", "(", ")")
REDIRECTIONS = ("<<<", "<<-", "<<", "&>>", "&>", ">>", ">&", "<&", "<>", ">|", ">", "<")


# A here-document's start: `<<EOF`, `<<-EOF`, `<< 'EOF'`, `<<"EOF"`, `<<\EOF`.
HEREDOC = re.compile(r"<<(-?)[ \t]*(?:'([^'\n]*)'|\"([^\"\n]*)\"|\\?([^\s;&|<>()'\"]+))")


def skip_heredoc_bodies(text: str, i: int, pending: list[tuple[str, bool]]) -> int:
    """The index after the bodies of the pending here-documents, which start at i."""
    for delim, strip_tabs in pending:
        while i < len(text):
            end = text.find("\n", i)
            line = text[i:] if end < 0 else text[i:end]
            i = len(text) if end < 0 else end + 1
            if (line.lstrip("\t") if strip_tabs else line) == delim:
                break
    pending.clear()
    return i


def read_balanced(text: str, i: int) -> tuple[str, int]:
    """The text from i up to the `)` that closes an already opened `(`, and the index
    after it. Quotes, backslashes and here-document bodies are skipped over, so an
    apostrophe in `"$(cat <<'EOF' ... EOF)"` doesn't open a quote."""
    depth, start = 1, i
    pending: list[tuple[str, bool]] = []
    while i < len(text):
        c = text[i]
        if c == "\n" and pending:
            i = skip_heredoc_bodies(text, i + 1, pending)
            continue
        if c == "<" and not text.startswith("<<<", i):
            m = HEREDOC.match(text, i)
            if m:
                delim = next(g for g in m.group(2, 3, 4) if g is not None)
                pending.append((delim, m.group(1) == "-"))
                i = m.end()
                continue
        if c == "\\":
            i += 2
            continue
        if c == "'":
            end = text.find("'", i + 1)
            if end < 0:
                raise ParseError("unterminated '")
            i = end + 1
            continue
        if c == '"':
            i = skip_double_quoted(text, i + 1)
            continue
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                return text[start:i], i + 1
        i += 1
    raise ParseError("unterminated (")


def skip_double_quoted(text: str, i: int) -> int:
    while i < len(text):
        c = text[i]
        if c == "\\":
            i += 2
            continue
        if c == '"':
            return i + 1
        if text.startswith("$(", i):
            _, i = read_balanced(text, i + 2)
            continue
        i += 1
    raise ParseError('unterminated "')


def read_backquoted(text: str, i: int) -> tuple[str, int]:
    out = []
    while i < len(text):
        c = text[i]
        if c == "\\" and i + 1 < len(text):
            out.append(text[i + 1])
            i += 2
            continue
        if c == "`":
            return "".join(out), i + 1
        out.append(c)
        i += 1
    raise ParseError("unterminated `")


def substitutions(text: str) -> list[str]:
    """The $(...) and `...` in text where quotes mean nothing (an unquoted here-document)."""
    subs, i = [], 0
    while i < len(text):
        if text.startswith("$(", i):
            inner, i = read_balanced(text, i + 2)
            subs.append(inner)
        elif text[i] == "`":
            inner, i = read_backquoted(text, i + 1)
            subs.append(inner)
        elif text[i] == "\\":
            i += 2
        else:
            i += 1
    return subs


def tokenize(text: str) -> list[Token]:
    tokens: list[Token] = []
    word: list[str] = []
    subs: list[str] = []
    in_word = False
    quoted = False  # whether the current word has quotes or backslashes in it
    pending: list[tuple[Token, str, bool, bool]] = []  # here-documents awaiting a body
    expect_delimiter: Token | None = None
    i, n = 0, len(text)

    def end_word() -> None:
        nonlocal word, subs, in_word, quoted, expect_delimiter
        if in_word:
            tok = Token("word", "".join(word), subs)
            if expect_delimiter is not None:
                strip_tabs = expect_delimiter.text == "<<-"
                pending.append((expect_delimiter, tok.text, strip_tabs, quoted))
                expect_delimiter = None
            else:
                tokens.append(tok)
        word, subs, in_word, quoted = [], [], False, False

    def read_heredocs(i: int) -> int:
        for tok, delim, strip_tabs, delim_quoted in pending:
            body = []
            while i < n:
                end = text.find("\n", i)
                line = text[i:] if end < 0 else text[i:end]
                i = n if end < 0 else end + 1
                if (line.lstrip("\t") if strip_tabs else line) == delim:
                    break
                body.append(line)
            tok.heredoc = "\n".join(body)
            if not delim_quoted:
                tok.subs.extend(substitutions(tok.heredoc))
        pending.clear()
        return i

    while i < n:
        c = text[i]
        if c == "\\" and i + 1 < n and text[i + 1] == "\n":
            i += 2
            continue
        if c in " \t\r":
            end_word()
            i += 1
            continue
        if c == "\n":
            end_word()
            tokens.append(Token("op", "\n"))
            i = read_heredocs(i + 1)
            continue
        if c == "#" and not in_word:
            end = text.find("\n", i)
            i = n if end < 0 else end
            continue
        if c in "<>" or (c == "&" and text.startswith("&>", i)):
            if c in "<>" and text.startswith("(", i + 1):
                inner, i = read_balanced(text, i + 2)
                subs.append(inner)
                in_word = True
                continue
            if in_word and "".join(word).isdigit():
                word, in_word = [], False  # a file descriptor, such as 2>
            end_word()
            op = next(r for r in REDIRECTIONS if text.startswith(r, i))
            tok = Token("redir", op)
            tokens.append(tok)
            if op in ("<<", "<<-"):
                expect_delimiter = tok
            i += len(op)
            continue
        if not in_word or c in ";|&()":
            op = next((o for o in OPERATORS if text.startswith(o, i)), None)
            if op is not None:
                end_word()
                tokens.append(Token("op", op))
                i += len(op)
                continue
        in_word = True
        if c in "\\'\"" or text.startswith("$'", i):
            quoted = True
        if c == "\\":
            if i + 1 < n:
                word.append(text[i + 1])
            i += 2
        elif c == "'":
            end = text.find("'", i + 1)
            if end < 0:
                raise ParseError("unterminated '")
            word.append(text[i + 1 : end])
            i = end + 1
        elif c == "$" and text.startswith("$'", i):
            j = i + 2
            while j < n and text[j] != "'":
                j += 2 if text[j] == "\\" else 1
            if j >= n:
                raise ParseError("unterminated $'")
            word.append(text[i + 2 : j])
            i = j + 1
        elif c == '"':
            j = i + 1
            while True:
                if j >= n:
                    raise ParseError('unterminated "')
                d = text[j]
                if d == '"':
                    break
                if d == "\\" and j + 1 < n:
                    if text[j + 1] in '"\\$`':
                        word.append(text[j + 1])
                    elif text[j + 1] != "\n":
                        word.append(text[j : j + 2])
                    j += 2
                elif text.startswith("$(", j):
                    inner, k = read_balanced(text, j + 2)
                    subs.append(inner)
                    word.append(text[j:k])
                    j = k
                elif d == "`":
                    inner, k = read_backquoted(text, j + 1)
                    subs.append(inner)
                    word.append(text[j:k])
                    j = k
                else:
                    word.append(d)
                    j += 1
            i = j + 1
        elif text.startswith("$(", i):
            inner, k = read_balanced(text, i + 2)
            subs.append(inner)
            word.append(text[i:k])
            i = k
        elif c == "`":
            inner, k = read_backquoted(text, i + 1)
            subs.append(inner)
            word.append(text[i:k])
            i = k
        else:
            word.append(c)
            i += 1
    end_word()
    if expect_delimiter is not None:
        raise ParseError("<< without a delimiter")
    read_heredocs(n)
    return tokens


def parse(text: str) -> list[Command]:
    """The simple commands in a shell script, in order."""
    commands, cmd = [], Command()
    tokens = tokenize(text)
    i = 0
    while i < len(tokens):
        tok = tokens[i]
        if tok.kind == "op":
            if cmd.words or cmd.subs or cmd.stdin_scripts or cmd.redirects:
                commands.append(cmd)
            cmd = Command()
        elif tok.kind == "redir":
            cmd.subs.extend(tok.subs)
            if tok.heredoc is not None:
                cmd.stdin_scripts.append(tok.heredoc)
            elif i + 1 < len(tokens) and tokens[i + 1].kind == "word":
                i += 1
                cmd.subs.extend(tokens[i].subs)
                cmd.redirects.append((tok.text, tokens[i].text))
                if tok.text == "<<<":
                    cmd.stdin_scripts.append(tokens[i].text)
        else:
            cmd.words.append(tok.text)
            cmd.subs.extend(tok.subs)
        i += 1
    if cmd.words or cmd.subs or cmd.stdin_scripts or cmd.redirects:
        commands.append(cmd)
    return commands


# ---------------------------------------------------------------------------
# Checking.
# ---------------------------------------------------------------------------


def ask_if_sensitive(text: str, why: str) -> Verdict:
    m = SENSITIVE.search(text)
    if m is None:
        return ALLOWED
    return ask("unparsed", f"the guard can't follow this command ({why}) and it runs {m.group(1)}")


def check(text: str, depth: int = 0) -> Verdict:
    """The verdict on a shell script."""
    if depth > MAX_DEPTH:
        return ask_if_sensitive(text, "nested too deeply")
    try:
        commands = parse(text)
    except ParseError as e:
        return ask_if_sensitive(text, str(e))
    shell_env: dict[str, str] = {}  # what `export` and bare assignments leave set
    verdicts = []
    for cmd in commands:
        verdicts.extend(check(s, depth + 1) for s in cmd.subs)
        verdicts.append(check_command(cmd, shell_env, text, depth))
    return worst(verdicts)


def check_command(cmd: Command, shell_env: dict[str, str], text: str, depth: int) -> Verdict:
    words, env, i = cmd.words, dict(shell_env), 0
    while i < len(words) and ASSIGNMENT.match(words[i]):
        k, v = words[i].split("=", 1)
        env[k] = v
        i += 1
    files = [
        v
        for op, t in cmd.redirects
        if op in FILE_REDIRECTIONS
        for v in (
            git_internals(t, env) if op in WRITE_REDIRECTIONS else ALLOWED,
            secret_path(t, env),
        )
    ]
    if i == len(words):
        shell_env.update(env)
        return worst(files)
    return worst([*files, check_argv(words[i:], env, cmd, shell_env, text, depth)])


def check_argv(
    argv: list[str],
    env: dict[str, str],
    cmd: Command,
    shell_env: dict[str, str],
    text: str,
    depth: int,
) -> Verdict:
    prog, args = argv[0], argv[1:]
    if "$" in prog or "`" in prog:
        return indirect(prog, env.get(variable_name(prog) or ""), args, depth)
    if ALIAS + prog in env:
        return indirect(prog, env[ALIAS + prog], args, depth)
    name = os.path.basename(prog)
    if name == "alias":
        for a in args:
            if "=" in a and not a.startswith("-"):
                k, v = a.split("=", 1)
                shell_env[ALIAS + k] = v
        return ALLOWED
    if name in KEYWORDS:
        return check_argv(args, env, cmd, shell_env, text, depth) if args else ALLOWED
    if name in ("export", "declare", "typeset", "local", "readonly"):
        for a in args:
            if ASSIGNMENT.match(a):
                k, v = a.split("=", 1)
                shell_env[k] = v
        return ALLOWED
    if name == "eval":
        return check(" ".join(args), depth + 1)
    if name in ("source", ".") and args:
        return worst([secret_args(name, args, env), maintainer_script(args[0])])
    if name == "env":
        return check_env(args, env, cmd, shell_env, text, depth)
    if name in SHELLS:
        return check_shell(args, cmd, text, depth)
    if name in WRAPPERS or (name == "uv" and args[:1] == ["run"]):
        return check_wrapper(args, env, cmd, shell_env, text, depth)
    rule = RULES.get(name)
    return worst([secret_args(name, args, env), rule(args, env) if rule else ALLOWED])


# Aliases set earlier in the command live in its environment under this prefix, which
# no variable name can have.
ALIAS = "alias "


def variable_name(word: str) -> str | None:
    m = VARIABLE.fullmatch(word)
    return m.group(1) if m else None


def indirect(prog: str, value: str | None, args: list[str], depth: int) -> Verdict:
    """`$g push`, `alias g=git; g push`: the guard can't be sure what runs, so it asks;
    when the command set the variable or alias itself, what it names is checked too."""
    verdicts = [
        ask("indirect", f"the program is {prog}, and the guard can't be sure what that runs")
    ]
    if value is not None:
        verdicts.append(check(" ".join([value, *map(shlex.quote, args)]), depth + 1))
    return worst(verdicts)


def runs_something(word: str) -> bool:
    name = os.path.basename(word)
    return name in RULES or name in SHELLS or name in WRAPPERS or name in ("env", "eval")


def check_wrapper(
    args: list[str],
    env: dict[str, str],
    cmd: Command,
    shell_env: dict[str, str],
    text: str,
    depth: int,
) -> Verdict:
    """`sudo -u x git ...`, `xargs -n1 git ...`, `uv run git ...`: the first argument
    that names a program the guard knows is where the wrapped command starts. A single
    argument holding a whole command line (`watch 'kubectl ...'`) is checked as one."""
    verdicts = [check(a, depth + 1) for a in args if " " in a and SENSITIVE.search(a)]
    for j, a in enumerate(args):
        if runs_something(a):
            verdicts.append(check_argv(args[j:], env, cmd, shell_env, text, depth))
            break
    else:  # `sudo cat ~/.ssh/...`: a program the guard has no rule for
        verdicts.append(secret_args("", args, env))
    return worst(verdicts)


def check_env(
    args: list[str],
    env: dict[str, str],
    cmd: Command,
    shell_env: dict[str, str],
    text: str,
    depth: int,
) -> Verdict:
    env, i = dict(env), 0
    while i < len(args):
        a = args[i]
        if a in ("-u", "--unset", "-C", "--chdir"):
            i += 2
        elif a in ("-S", "--split-string") and i + 1 < len(args):
            return check(" ".join(args[i + 1 :]), depth + 1)
        elif a.startswith("--split-string="):
            return check(a.split("=", 1)[1] + " " + " ".join(args[i + 1 :]), depth + 1)
        elif a.startswith("-"):
            i += 1
        elif ASSIGNMENT.match(a):
            k, v = a.split("=", 1)
            env[k] = v
            i += 1
        else:
            return check_argv(args[i:], env, cmd, shell_env, text, depth)
    return ALLOWED


def shell_names_maintainer_script(args: list[str]) -> Verdict:
    """A shell given one of the maintainer's scripts anywhere in its arguments is
    blocked, unless the whole command is `bash -n <script>` (or `sh -n`), which only
    reads it. Reading the shell's options instead is a guess: a cluster such as
    `-eo pipefail` takes the next word as a value, so the script isn't where it seems."""
    named = [a for a in args if os.path.basename(a) in MAINTAINER_SCRIPTS]
    if not named or (len(args) == 2 and args[0] == "-n"):
        return ALLOWED
    return maintainer_script(named[0])


def check_shell(args: list[str], cmd: Command, text: str, depth: int) -> Verdict:
    """`bash -c '...'` checks the script; `bash <<EOF` checks the here-document; a
    shell reading stdin from a pipe can't be followed."""
    script = shell_names_maintainer_script(args)
    has_c, i = False, 0
    while i < len(args):
        a = args[i]
        if a == "--":
            i += 1
            break
        if a in ("-o", "+o", "-O", "+O"):
            i += 2
            continue
        if a.startswith("-") and not a.startswith("--") and "c" in a[1:]:
            has_c = True
        if not a.startswith(("-", "+")):
            break
        i += 1
    if has_c:
        # Which word is the command string depends on the options before it (in
        # `bash -co pipefail '...'` it is the second), so every word that isn't an
        # option is checked as one.
        return worst([script, *(check(a, depth + 1) for a in args if not a.startswith(("-", "+")))])
    if i < len(args) or script.level:
        return script  # a script file: what it runs is out of sight, but not its name
    if cmd.stdin_scripts:
        return worst([check(s, depth + 1) for s in cmd.stdin_scripts])
    return ask_if_sensitive(text, "a shell reads commands from a pipe")


# ---------------------------------------------------------------------------
# The hook.
# ---------------------------------------------------------------------------

BLOCKED = (
    "blocked by .claude/hooks/guard.py ({rule}): {reason}. "
    "If it has to happen, stop and ask the maintainer to do it."
)


# What the log leaves out: a token in a header, an option or a variable, and the
# token formats GitHub, AWS, Slack and the LLM providers use.
REDACT = [
    (re.compile(r"(?i)(authorization:\s*(bearer|token|basic)?\s*)[^\s'\"]+"), r"\1[redacted]"),
    (
        re.compile(
            r"(?i)([\w-]*(token|passw(or)?d|secret|api[_-]?key|credential)[\w-]*"
            r"(\s*[=:]\s*|\s+))(\"[^\"]*\"|'[^']*'|[^\s'\"]+)"
        ),
        r"\1[redacted]",
    ),
    # `docker login -p ...`, `helm registry login -p ...`
    (re.compile(r"(\blogin\b[^;&|\n]*?\s-p\s*)(\"[^\"]*\"|'[^']*'|[^\s'\"]+)"), r"\1[redacted]"),
    (
        re.compile(
            r"\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_\w{20,}|AKIA[0-9A-Z]{16}"
            r"|xox[abprs]-[\w-]{10,}|sk-[\w-]{20,}|glpat-[\w-]{20,})"
        ),
        "[redacted]",
    ),
]


def redact(command: str) -> str:
    for pattern, replacement in REDACT:
        command = pattern.sub(replacement, command)
    return command


def log(decision: str, rule: str, command: str) -> None:
    """One line per block or ask in ~/.claude/twr-guard.log (TWR_GUARD_LOG moves it; empty
    turns it off), to look at afterwards, with tokens and passwords redacted. A log that
    can't be written is skipped."""
    path = os.environ.get("TWR_GUARD_LOG", os.path.join("~", ".claude", "twr-guard.log"))
    if not path:
        return
    when = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    try:
        with open(os.path.expanduser(path), "a", encoding="utf-8") as f:
            f.write(f"{when}\t{decision}\t{rule}\t{json.dumps(redact(command)[:200])}\n")
    except OSError:
        pass


def decide(command: str, config_path: str) -> Verdict:
    """The verdict on a command under the repository's configuration. Without a
    readable one the guard can't tell which branch or tags to protect, so every command
    is blocked. Blocking only the commands that name a guarded program would trust the
    raw text, which quoting (`gi""t`) hides from; and nothing needs Bash to fix the
    config, since the Read and Edit tools reach it."""
    global CONFIG
    try:
        CONFIG = load_config(config_path)
    except ConfigError as e:
        return block(
            "config",
            f"{e}, so every Bash command is blocked until .claude/guard.json is fixed; "
            "fix it with the Edit tool (agent/README.md in those-who-run/.github says what "
            "it holds)",
        )
    return check(command)


def main(stdin=None, stdout=None, stderr=None, config_path: str | None = None) -> int:
    stdin = stdin or sys.stdin
    stdout = stdout or sys.stdout
    stderr = stderr or sys.stderr
    command = ""
    try:
        data = json.load(stdin)
        if data.get("tool_name") != "Bash":
            return 0
        command = data["tool_input"]["command"]
        if not isinstance(command, str):
            raise TypeError(f"tool_input.command is {type(command).__name__}")
        verdict = decide(command, config_path or CONFIG_PATH)
    except Exception as e:  # fail closed: a guard that can't decide blocks
        print(
            f"blocked by .claude/hooks/guard.py: the guard failed ({e!r}), so the command "
            "is blocked. Tell the maintainer.",
            file=stderr,
        )
        log("error", type(e).__name__, command if isinstance(command, str) else repr(command))
        return 2
    if verdict.level != ALLOW:
        log("block" if verdict.level == BLOCK else "ask", verdict.rule, command)
    if verdict.level == BLOCK:
        print(BLOCKED.format(rule=verdict.rule, reason=verdict.reason), file=stderr)
        return 2
    if verdict.level == ASK:
        out = {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "ask",
                "permissionDecisionReason": (
                    f".claude/hooks/guard.py ({verdict.rule}): {verdict.reason}"
                ),
            }
        }
        json.dump(out, stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
