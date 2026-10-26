package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A data directory that cannot be made, locked or read is refused, and leaves
// nothing open.
func TestOpenRefusesADirectoryItCannotUse(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockIsADir := t.TempDir()
	if err := os.Mkdir(filepath.Join(lockIsADir, lockFile), 0o750); err != nil {
		t.Fatal(err)
	}
	notAnIndex := t.TempDir()
	if err := os.WriteFile(filepath.Join(notAnIndex, indexFile), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	badVersion := t.TempDir()
	withIndex(t, badVersion, func(db *sql.DB) {
		if _, err := db.ExecContext(t.Context(), `CREATE TABLE schema_version (version VARCHAR); INSERT INTO schema_version VALUES ('one')`); err != nil {
			t.Fatal(err)
		}
	})
	for name, dir := range map[string]string{
		"a file":                        notADir,
		"a lock file that is no file":   lockIsADir,
		"an index that is no database":  notAnIndex,
		"a schema version that is text": badVersion,
	} {
		s, err := Open(t.Context(), dir)
		if err == nil {
			_ = s.Close()
			t.Errorf("%s: Open succeeded", name)
		}
	}
	// None of them kept the lock.
	s, err := Open(t.Context(), badVersion)
	if errors.Is(err, ErrBusy) {
		t.Error("a refused Open kept the lock")
	}
	if err == nil {
		_ = s.Close()
	}
}

// A write the index refuses is reported and leaves no half of itself: a lap whose
// row cannot be written keeps its file but has no row, and the next delivery
// completes it.
func TestAWriteTheIndexRefusesIsReported(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := t.Context()
	sess := race("live", sid)
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.index.ExecContext(ctx, `ALTER TABLE laps RENAME TO laps_away`); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[0], sess.frames); err == nil {
		t.Error("WriteLap with no laps table succeeded")
	}
	if err := s.EndSession(ctx, "live", sess.ended); err == nil {
		t.Error("EndSession with no laps table succeeded")
	}
	if _, err := os.Stat(lapFile(dir, "live", 1)); err != nil {
		t.Errorf("the file before the row: %v", err)
	}
	if _, err := s.Laps(ctx, "live"); err == nil {
		t.Error("Laps with no laps table succeeded")
	}
	if err := s.ExportLaps(ctx, &failingWriter{}, "live"); err == nil {
		t.Error("ExportLaps with no laps table succeeded")
	}
	if _, err := s.index.ExecContext(ctx, `ALTER TABLE laps_away RENAME TO laps`); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[0], sess.frames); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportLaps(ctx, &failingWriter{}, "live"); err == nil {
		t.Error("ExportLaps to a writer that fails succeeded")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the reader went away") }

// A lap file that cannot be written leaves no row and no temporary file.
func TestALapFileThatCannotBeWrittenLeavesNoRow(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := t.Context()
	sess := race("live", sid)
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	// The lap's path is a directory, so the rename over it fails.
	if err := os.MkdirAll(filepath.Join(lapFile(dir, "live", 1), "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[0], sess.frames); err == nil {
		t.Error("WriteLap over a directory succeeded")
	}
	// The temporary file's path is a directory, so the copy fails.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(lapFile(dir, "live", 2)), ".lap-002.parquet.tmp", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[1], sess.frames); err == nil {
		t.Error("WriteLap with its temporary path taken succeeded")
	}
	// The session's directory is a file, so it cannot be made.
	if err := os.WriteFile(filepath.Join(dir, "sessions", "parity"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	parity := race("parity", sid)
	if err := s.StartSession(ctx, parity.started); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "parity", parity.laps[0], parity.frames); err == nil {
		t.Error("WriteLap with no session directory succeeded")
	}
	// The scratch table is gone, so the frames cannot be appended.
	if _, err := s.scratch.ExecContext(ctx, `DROP TABLE frames`); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[2], sess.frames); err == nil {
		t.Error("WriteLap with no scratch table succeeded")
	}
	laps, err := s.Laps(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(laps) != 0 {
		t.Errorf("rows for laps whose files failed: %+v", laps)
	}
}

// A cancelled context stops every call before it writes.
func TestACancelledContextWritesNothing(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.StartSession(ctx, sess.started); err == nil {
		t.Error("StartSession succeeded")
	}
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(ctx, "live", sess.laps[0], sess.frames); err == nil {
		t.Error("WriteLap succeeded")
	}
	if err := s.EndSession(ctx, "live", sess.ended); err == nil {
		t.Error("EndSession succeeded")
	}
	if _, err := s.Laps(ctx, "live"); err == nil {
		t.Error("Laps succeeded")
	}
}
