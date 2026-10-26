package store

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// update rewrites the committed index fixture: `go test ./internal/store -run
// TestOpensTheCommittedIndex -update`. Do it only on purpose, after a DuckDB upgrade
// whose file format the store has been made to migrate.
var update = flag.Bool("update", false, "rewrite testdata/index.duckdb with this DuckDB")

// fixture is an index.duckdb written by DuckDB 1.5.6 (duckdb-go v2.10506.0) at
// schema version 1: the test race's session and laps, without their lap files.
const fixture = "testdata/index.duckdb"

// A DuckDB upgrade that changes the index's file format must not pass unnoticed
// (paddock ADR 0007: such an upgrade is a logbook release that migrates its own
// data). The committed index, written by the DuckDB of its time, still opens and
// reads the same.
func TestOpensTheCommittedIndex(t *testing.T) {
	if *update {
		writeFixture(t)
	}
	dir := t.TempDir()
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexFile), b, 0o600); err != nil { //nolint:gosec // the test's own directory
		t.Fatal(err)
	}
	s := open(t, dir)
	laps, err := s.Laps(t.Context(), "live")
	if err != nil {
		t.Fatal(err)
	}
	var got []int32
	for _, l := range laps {
		got = append(got, l.Lap)
	}
	if want := []int32{1, 2, 3}; !slices.Equal(got, want) || laps[0].SessionID != sid || *laps[0].LapTimeMs != 98_100 {
		t.Errorf("the committed index holds laps %v of %s, want %v of %s", got, laps[0].SessionID, want, sid)
	}
	session := rowsOf(t, t.Context(), s.index, `SELECT id, laps_completed, best_lap_ms, ended_at FROM sessions`)
	if want := [][]string{{sid, "2", "98100", "2026-10-15T19:35:00Z"}}; !slices.EqualFunc(session, want, slices.Equal) {
		t.Errorf("sessions %q, want %q", session, want)
	}
}

// writeFixture writes the test race to a new store and copies its index, tables
// and all, into a database with DuckDB's smallest block size: one file with no WAL
// beside it, small enough to commit (the default 256 KiB blocks make it 1.3 MB).
func writeFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	race("live", sid).write(t, s)
	small := filepath.Join(t.TempDir(), "small.duckdb")
	for _, q := range []string{
		`ATTACH '` + small + `' AS small (BLOCK_SIZE 16384)`,
		`COPY FROM DATABASE index TO small`,
		`DETACH small`,
	} {
		if _, err := s.index.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(small) //nolint:gosec // the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, b, 0o600); err != nil { //nolint:gosec // the fixture's fixed path
		t.Fatal(err)
	}
}
