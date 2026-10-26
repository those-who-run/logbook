// Package dependencies_test holds the dependency table's check: every direct
// requirement in go.mod has a row in docs/dependencies.md, and every row names one.
// It is a package of its own, holding only this test, so it never shares a
// directory with a repository's own package; `go test ./...` (make check and go.yml)
// runs it from here, and it uses only the standard library.
package dependencies_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"testing"
)

// A table row's first cell, a module path in backticks.
var row = regexp.MustCompile("(?m)^\\|\\s*`([^`]+)`\\s*\\|")

// directRequires returns the modules a go.mod file requires without `// indirect`,
// as `go mod edit -json` reads them.
func directRequires(t *testing.T, gomod string) []string {
	t.Helper()
	// go test puts its own toolchain's go first on PATH, so this is the go running
	// the tests.
	out, err := exec.CommandContext(t.Context(), "go", "mod", "edit", "-json", gomod).Output() //nolint:gosec // gomod is go.mod or a fixture, never input
	if err != nil {
		t.Fatalf("go mod edit -json %s: %v", gomod, err)
	}
	var mod struct {
		Require []struct {
			Path     string
			Indirect bool
		}
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("go mod edit -json %s: %v", gomod, err)
	}
	var direct []string
	for _, r := range mod.Require {
		if !r.Indirect {
			direct = append(direct, r.Path)
		}
	}
	return direct
}

// tableRows returns the module path in each row of a dependency table.
func tableRows(t *testing.T, table string) []string {
	t.Helper()
	text, err := os.ReadFile(table) //nolint:gosec // the table or a fixture, never input
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, m := range row.FindAllStringSubmatch(string(text), -1) {
		rows = append(rows, m[1])
	}
	return rows
}

// problems says what is wrong between a go.mod file and its dependency table.
func problems(t *testing.T, gomod, table string) []string {
	t.Helper()
	direct := directRequires(t, gomod)
	rows := tableRows(t, table)
	var found []string
	for _, path := range direct {
		if !slices.Contains(rows, path) {
			found = append(found, path+" is required directly in "+gomod+" but has no row in "+table)
		}
	}
	for _, path := range rows {
		if !slices.Contains(direct, path) {
			found = append(found, path+" has a row in "+table+" but is not required directly in "+gomod)
		}
	}
	return found
}

func TestEveryDirectDependencyHasARow(t *testing.T) {
	for _, p := range problems(t, "../../go.mod", "../../docs/dependencies.md") {
		t.Error(p)
	}
}

func TestTheCheckFindsAMissingRowAndAStaleOne(t *testing.T) {
	const gomod = "testdata/requires.mod"
	cases := []struct {
		table string
		want  []string
	}{
		{"testdata/complete.md", nil},
		{"testdata/missing.md", []string{
			"example.com/unlisted is required directly in " + gomod +
				" but has no row in testdata/missing.md",
		}},
		{"testdata/stale.md", []string{
			"example.com/gone has a row in testdata/stale.md" +
				" but is not required directly in " + gomod,
		}},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			if got := problems(t, gomod, c.table); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
