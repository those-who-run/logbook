package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/duckdb/duckdb-go/v2"
)

// withIndex runs f on dir's index.duckdb, opened directly, as a store of another
// release would have left it.
func withIndex(t *testing.T, dir string, f func(db *sql.DB)) {
	t.Helper()
	conn, err := duckdb.NewConnector(filepath.Join(dir, indexFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer func() {
		if err := errors.Join(db.Close(), conn.Close()); err != nil {
			t.Fatal(err)
		}
	}()
	f(db)
}

// A new index is made at SchemaVersion, and opening it again changes nothing.
func TestANewIndexIsAtTheCodesVersion(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		s, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	withIndex(t, dir, func(db *sql.DB) {
		got := rowsOf(t, t.Context(), db, `SELECT version FROM schema_version`)
		if want := [][]string{{fmt.Sprint(SchemaVersion)}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("schema_version %q, want %q", got, want)
		}
	})
}

// AC4: an index at a higher schema version than the code is refused, with both
// versions in the message, and left as it was.
func TestANewerIndexIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	newer := SchemaVersion + 41
	withIndex(t, dir, func(db *sql.DB) {
		if _, err := db.ExecContext(t.Context(), `UPDATE schema_version SET version = ?`, newer); err != nil {
			t.Fatal(err)
		}
	})

	_, err = Open(t.Context(), dir)
	if err == nil {
		t.Fatal("Open accepted a newer index")
	}
	for _, v := range []int{newer, SchemaVersion} {
		if !strings.Contains(err.Error(), fmt.Sprintf(" %d", v)) {
			t.Errorf("%q does not name version %d", err, v)
		}
	}
	// The refusal released the lock, and left the version alone.
	withIndex(t, dir, func(db *sql.DB) {
		if got := rowsOf(t, t.Context(), db, `SELECT version FROM schema_version`); got[0][0] != fmt.Sprint(newer) {
			t.Errorf("version %s after the refusal, want %d", got[0][0], newer)
		}
	})
}

// A migration that fails changes nothing: the step and its version go together.
func TestAFailedMigrationChangesNothing(t *testing.T) {
	saved := migrations
	t.Cleanup(func() { migrations = saved; SchemaVersion = len(saved) })
	migrations = append(slices.Clone(saved), `CREATE TABLE extra (x INTEGER); SELECT nonsense FROM nowhere;`)
	SchemaVersion = len(migrations)

	dir := t.TempDir()
	if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", SchemaVersion)) {
		t.Fatalf("Open: %v, want the failed step named", err)
	}
	withIndex(t, dir, func(db *sql.DB) {
		got := rowsOf(t, t.Context(), db, `SELECT version, (SELECT count(*) FROM duckdb_tables() WHERE table_name = 'extra') FROM schema_version`)
		if want := [][]string{{fmt.Sprint(len(saved)), "0"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("version and extra tables %q, want %q", got, want)
		}
	})
}

// Schema version 4 keys staged frames by run as well (paddock#76): an index at
// version 3 keeps the frames it had staged, and its end reasons stored as bare
// numbers under contracts v0.1.5 get the names v0.2 gives them.
func TestVersion4KeepsTheStagedFrames(t *testing.T) {
	dir := t.TempDir()
	withIndex(t, dir, func(db *sql.DB) {
		ctx := t.Context()
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for v := range 3 {
			if err := step(ctx, db, v); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO frames_pending VALUES ('live', 'x', 7, 'r', 'f')`); err != nil {
			t.Fatal(err)
		}
		for i, reason := range []string{"9", "10", "11", "finished", "12"} {
			if _, err := db.ExecContext(ctx, `INSERT INTO sessions (src, id, started_at, type, car_code, joined_part_way,
    first_seq, end_reason) VALUES ('live', ?, '2026-10-30 12:00:00', 'race', 1, false, 1, ?)`, fmt.Sprint("s", i), reason); err != nil {
				t.Fatal(err)
			}
		}
	})
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	withIndex(t, dir, func(db *sql.DB) {
		got := rowsOf(t, t.Context(), db, `SELECT src, session_id, seq, run, frame FROM frames_pending`)
		if want := [][]string{{"live", "x", "7", "r", "f"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("staged %q after the migration, want %q", got, want)
		}
		// The end reasons contracts v0.1.5 did not name get their names; the rest
		// stay as they were.
		got = rowsOf(t, t.Context(), db, `SELECT end_reason FROM sessions ORDER BY id`)
		want := [][]string{{"timed_finish"}, {"menu"}, {"pause_limit"}, {"finished"}, {"12"}}
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("end reasons %q after the migration, want %q", got, want)
		}
	})
}
