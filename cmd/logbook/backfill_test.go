package main

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/those-who-run/logbook/internal/store"
)

// `logbook backfill` without a data directory, or with an argument it does not
// take, is a usage error.
func TestBackfillUsage(t *testing.T) {
	for _, args := range [][]string{{"backfill"}, {"backfill", "--data", t.TempDir(), "extra"}, {"backfill", "--bogus"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, env(nil), &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"backfill", "-h"}, env(nil), &stdout, &stderr); code != 0 {
		t.Errorf("-h: exit %d, want 0", code)
	}
}

// With no server to read EVENTS from, the backfill fails, having migrated the
// store and changed nothing else.
func TestBackfillWithoutAServerFails(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("nats://%s", l.Addr())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"backfill", "--data", t.TempDir(), "--nats", url}, env(nil), &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "backfill failed") {
		t.Errorf("exit %d, stdout %q, want 1 and the failure logged", code, stdout.String())
	}
}

// A store logbook holds is refused: the backfill runs with logbook stopped.
func TestBackfillRefusesAStoreInUse(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"backfill", "--data", dir}, env(nil), &stdout, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}
