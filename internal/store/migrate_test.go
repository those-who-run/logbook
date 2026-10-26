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
