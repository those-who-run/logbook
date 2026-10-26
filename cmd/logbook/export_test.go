package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/logbook/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stored writes two sessions of two laps to a store in dir and closes it: the
// later one first, so the order printed is the store's, not the writes'.
func stored(t *testing.T, dir string) {
	t.Helper()
	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for i, id := range []string{"01JB8Z3K5Q9T2M7XW4R6N1PDHF", "01JB8Z3K5Q9T2M7XW4R6N1PDHE"} {
		start := time.Date(2026, 10, 15, 20-i, 0, 0, 0, time.UTC)
		if err := s.StartSession(t.Context(), &eventsv1.SessionStarted{
			SessionId: id, Source: "live", StartedAt: timestamppb.New(start),
		}); err != nil {
			t.Fatal(err)
		}
		for lap := range int32(2) {
			frames := []*telemetryv1.Frame{{Seq: uint32(lap), Lap: lap + 1, Phase: telemetryv1.Phase_PHASE_IN_RACE}}
			ev := &eventsv1.LapCompleted{SessionId: id, Lap: lap + 1, LapTimeMs: 90_000 + lap, Complete: true,
				FirstSeq: uint32(lap), LastSeq: uint32(lap)}
			if err := s.WriteLap(t.Context(), "live", ev, frames); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// AC6: `logbook export --laps` prints the store's laps in order, one JSON object
// a line.
func TestExportPrintsTheStoredLapsInOrder(t *testing.T) {
	dir := t.TempDir()
	stored(t, dir)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"export", "--data", dir, "--laps"}, env(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var got []string
	for line := range strings.Lines(stdout.String()) {
		var lap struct {
			SessionID string `json:"session_id"`
			Lap       int    `json:"lap"`
		}
		if err := json.Unmarshal([]byte(line), &lap); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got = append(got, fmt.Sprintf("%s/%d", lap.SessionID[24:], lap.Lap))
	}
	want := []string{"HE/1", "HE/2", "HF/1", "HF/2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("printed %q, want %q", got, want)
	}

	// Another source has no laps here; TWR_DATA_DIR stands in for --data.
	stdout.Reset()
	code := run(t.Context(), []string{"export", "--laps", "--src", "parity"}, env(map[string]string{"TWR_DATA_DIR": dir}), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 {
		t.Errorf("exit %d, printed %q for parity; want 0 and nothing", code, stdout.String())
	}
}

// R7: export takes the lock, so it refuses while the store is open elsewhere.
func TestExportRefusesWhileTheStoreIsOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var stderr bytes.Buffer
	code := run(t.Context(), []string{"export", "--data", dir, "--laps"}, env(nil), &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "in use") {
		t.Errorf("exit %d, stderr %q; want 1 and the store said to be in use", code, stderr.String())
	}
}

func TestExportUsageErrors(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"export", "--laps"}, 2},      // no data directory
		{[]string{"export", "--data", dir}, 2}, // nothing asked for
		{[]string{"export", "--data", dir, "--laps", "x"}, 2},
		{[]string{"export", "--nope"}, 2},
		{[]string{"export", "-h"}, 0},
		{[]string{"export", "--data", dir, "--laps", "--src", "../x"}, 1},
	} {
		var stderr bytes.Buffer
		if code := run(t.Context(), c.args, env(nil), &bytes.Buffer{}, &stderr); code != c.code {
			t.Errorf("%q: exit %d, want %d (stderr %q)", c.args, code, c.code, stderr.String())
		}
	}
}
