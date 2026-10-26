# The repository's checks. `make check` is the Definition of done (AGENTS.md): it
# runs what CI runs, the commit hooks on every file (CI's `checks / checks`), the Go
# checks in go.yml's order (CI's `go / check`) and `make test` (CI's `test`, which
# calls this same target so the two can't drift).
# Needs Go (the version in go.mod's toolchain line), python3 and pre-commit; the
# guard copy check and the first run of the Go tools need the network.

PY ?= python3
# golangci-lint and govulncheck, at the versions tools/go.mod pins and tools/go.sum
# checksums, as go.yml runs them: by package path, outside any go.work.
GO_TOOL := GOWORK=off go tool -modfile=tools/go.mod
GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck
# The lowest total statement coverage the tests may reach, in per cent: the
# coverage-floor ci.yml passes to go.yml (tests/test_go_floor.py keeps them equal).
COVERAGE_FLOOR := 90
# The module's Go files, skipping what the go command skips: hidden directories,
# testdata and vendor.
GO_FILES = $(shell find . -name '*.go' -not -path '*/.*' -not -path '*/testdata/*' -not -path '*/vendor/*')
BUILD := build

.PHONY: help
help:
	@echo "check     - everything a PR must pass: the commit hooks on every file, the Go checks, then make test (CI runs all three)"
	@echo "test      - the Go tests (race detector, no skips, coverage floor) and the repository's own checks (CI's test job)"
	@echo "fmt       - format the Go code with gofmt"
	@echo "lint      - golangci-lint, with .golangci.yml"
	@echo "hooks     - install the commit hooks; a new clone or worktree runs it once"
	@echo "fmt-check, tidy, vet, go-test, build, vuln - the Go checks one at a time"
	@echo "links     - every internal link and anchor in the Markdown resolves (offline)"
	@echo "guard     - .claude/ holds those-who-run/.github's guard and settings at the pinned commit"

# go.yml's order: gofmt, tidy, vet, golangci-lint, the tests, build, govulncheck.
.PHONY: check
check:
	pre-commit run --all-files
	$(MAKE) fmt-check tidy vet lint go-test build vuln
	$(MAKE) test-repo

.PHONY: test
test: go-test test-repo

# The repository's own checks apart from the Go tests: links, the guard copy, and
# the tests in tests/.
.PHONY: test-repo
test-repo: links guard
	$(PY) -m unittest discover -s tests

.PHONY: hooks
hooks:
	pre-commit install

.PHONY: fmt
fmt:
	gofmt -w $(GO_FILES)

# A gofmt that is missing or can't read a file fails, rather than listing nothing.
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l $(GO_FILES)) || exit 1; \
	if [ -n "$$unformatted" ]; then echo "not formatted (make fmt):" $$unformatted >&2; exit 1; fi

# Neither the module nor the tools module may need `go mod tidy`.
.PHONY: tidy
tidy:
	go mod tidy -diff
	cd tools && go mod tidy -diff

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	$(GO_TOOL) $(GOLANGCI_LINT) run ./...

# The race detector (which needs cgo, whatever the build uses) in shuffled order. A
# skipped test fails, as in go.yml, and so does total coverage below the floor;
# scripts/go_coverage.py totals it, leaving out generated files as go.yml does.
.PHONY: go-test
go-test:
	@mkdir -p $(BUILD)
	CGO_ENABLED=1 go test -race -shuffle=on -covermode=atomic -coverprofile=$(BUILD)/cover.out -v ./... > $(BUILD)/test.log 2>&1; \
	  status=$$?; grep -v -E '^(=== |\s+--- PASS|--- PASS)' $(BUILD)/test.log; exit $$status
	@if grep -E '^\s*--- SKIP' $(BUILD)/test.log; then echo "a skipped test fails: fix it or remove it" >&2; exit 1; fi
	@$(PY) scripts/go_coverage.py $(BUILD)/cover.out $(COVERAGE_FLOOR)

.PHONY: build
build:
	go build ./...

.PHONY: vuln
vuln:
	$(GO_TOOL) $(GOVULNCHECK) ./...

.PHONY: links
links:
	$(PY) scripts/docs_links.py

.PHONY: guard
guard:
	$(PY) scripts/guard_copy.py
