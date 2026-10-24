# The repository's checks. `make check` is the Definition of done (AGENTS.md): it
# runs what CI runs, the commit hooks on every file (CI's `checks / checks`) and
# `make test` (CI's `test`, which calls this same target so the two can't drift).
# Needs python3 and pre-commit; the guard copy check needs the network.

PY ?= python3

.PHONY: help
help:
	@echo "check  - everything a PR must pass: the commit hooks on every file, then make test (CI runs both)"
	@echo "test   - the repository's own checks: links, the guard copy, and the tests (CI's test job)"
	@echo "hooks  - install the commit hooks; a new clone or worktree runs it once"
	@echo "links  - every internal link and anchor in the Markdown resolves (offline)"
	@echo "guard  - .claude/ holds those-who-run/.github's guard and settings at the pinned commit"

.PHONY: check
check:
	pre-commit run --all-files
	$(MAKE) test

.PHONY: test
test: links guard
	$(PY) -m unittest discover -s tests

.PHONY: hooks
hooks:
	pre-commit install

.PHONY: links
links:
	$(PY) scripts/docs_links.py

.PHONY: guard
guard:
	$(PY) scripts/guard_copy.py
