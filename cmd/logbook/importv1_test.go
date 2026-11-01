package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/those-who-run/logbook/internal/store"
)

// v1Fixture is importv1's v1 data directory, read only by these tests.
var v1Fixture = filepath.Join("..", "..", "internal", "importv1", "testdata", "v1")

// AC4, the lock: import-v1 takes the store's lock, so it refuses while `logbook
// run` (here, any process) holds the store, and writes nothing.
func TestImportV1RefusesWhileTheStoreIsOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"import-v1", "--from", v1Fixture, "--data", dir}, env(nil), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "in use") || stdout.Len() > 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1, nothing printed and the store said to be in use",
			code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); !os.IsNotExist(err) {
		t.Errorf("the import wrote under the store: %v", err)
	}
}

// The command prints the summary and imports; --dry-run prints the same and
// opens no store.
func TestImportV1PrintsItsSummary(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"import-v1", "--from", v1Fixture, "--dry-run"},
		{"import-v1", "--from", v1Fixture, "--data", dir},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, env(nil), &stdout, &stderr); code != 0 {
			t.Fatalf("%q: exit %d, stderr %q", args, code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "20261015-180014 re-derived: 6 laps (v1 6); lap times are v1's; checked: complete laps, fuel, kind, track, finish\n") {
			t.Errorf("%q: stdout %q", args, stdout.String())
		}
	}
	var stdout bytes.Buffer
	if code := run(t.Context(), []string{"import-v1", "--from", v1Fixture, "--data", dir}, env(nil), &stdout, &bytes.Buffer{}); code != 0 ||
		!strings.Contains(stdout.String(), "20261015-180014 already imported: re-derived\n") {
		t.Errorf("a second import: exit %d, stdout %q", code, stdout.String())
	}
}

func TestImportV1UsageErrors(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"import-v1", "--data", dir}, 2},       // no v1 directory
		{[]string{"import-v1", "--from", v1Fixture}, 2}, // no store, no dry run
		{[]string{"import-v1", "--from", v1Fixture, "--data", dir, "x"}, 2},
		{[]string{"import-v1", "--nope"}, 2},
		{[]string{"import-v1", "-h"}, 0},
		{[]string{"import-v1", "--from", dir, "--dry-run"}, 1}, // not v1's
	} {
		var stderr bytes.Buffer
		if code := run(t.Context(), c.args, env(nil), &bytes.Buffer{}, &stderr); code != c.code {
			t.Errorf("%q: exit %d, want %d (stderr %q)", c.args, code, c.code, stderr.String())
		}
	}
}
