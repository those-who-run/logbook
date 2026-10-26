// Package store is logbook's store: each lap's frames in a Parquet file, and a row
// per session and per lap in the index, index.duckdb, under one data directory that
// logbook alone reads and writes (paddock ADR 0003 and ADR 0007).
//
// The layout under the data directory:
//
//	logbook.lock                          held exclusively by the one process using it
//	index.duckdb                          the sessions and laps tables
//	sessions/<src>/<sid>/lap-NNN.parquet  a lap's racing frames, ZSTD
//
// Every write is safe to repeat: a lap's file is written to a temporary file and
// renamed, its row is written after the file is in place, and rows are upserted, so
// the same event or lap delivered twice leaves one file and one row.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// The files under the data directory.
const (
	lockFile  = "logbook.lock"
	indexFile = "index.duckdb"
	lapsDir   = "sessions"
)

// SourceLive is the source of a live session, and the one readers show unless
// asked for another.
const SourceLive = "live"

// ErrBusy is returned by Open while another process holds the data directory.
var ErrBusy = errors.New("store: the data directory is in use by another process")

// ErrUnknownSession is returned for a lap or a session end whose session was never
// started in the store.
var ErrUnknownSession = errors.New("store: no such session")

// A source is `live`, `parity` or `replay-<run>`, as SessionStarted's source says,
// and a session id is a ULID. Both become directory names, so anything else is
// refused rather than allowed to reach outside the data directory.
var (
	validSource    = regexp.MustCompile(`^(live|parity|replay-[A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)
	validSessionID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
)

// Store is an open data directory. Its methods may be called from more than one
// goroutine; writes are made one at a time.
type Store struct {
	dir  string
	lock *os.File
	// index is index.duckdb; scratch is an in-memory database that a lap's frames
	// pass through on their way to Parquet, so they never touch the index.
	indexConn   *duckdb.Connector
	index       *sql.DB
	scratchConn *duckdb.Connector
	scratch     *sql.DB
	now         func() time.Time

	mu sync.Mutex
}

// Open takes the data directory dir, creating it if it does not exist: it locks
// logbook.lock (ErrBusy if another process holds it), opens index.duckdb and brings
// its schema up to date. The caller closes the store.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	lock, err := takeLock(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, now: time.Now}
	if err := s.open(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func (s *Store) open(ctx context.Context) error {
	var err error
	if s.indexConn, err = duckdb.NewConnector(filepath.Join(s.dir, indexFile), nil); err != nil {
		return fmt.Errorf("store: open %s: %w", indexFile, err)
	}
	s.index = sql.OpenDB(s.indexConn)
	if s.scratchConn, err = duckdb.NewConnector("", nil); err != nil {
		return fmt.Errorf("store: open the scratch database: %w", err)
	}
	s.scratch = sql.OpenDB(s.scratchConn)
	// One connection each: the scratch table lives in that connection's database,
	// and the index has one writer.
	s.index.SetMaxOpenConns(1)
	s.scratch.SetMaxOpenConns(1)
	if err := migrate(ctx, s.index); err != nil {
		return err
	}
	if _, err := s.scratch.ExecContext(ctx, createFramesTable); err != nil {
		return fmt.Errorf("store: the scratch table: %w", err)
	}
	return nil
}

// takeLock takes an exclusive, non-blocking flock on path. The lock goes when the
// file is closed or the process dies, so a crash leaves nothing stale.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the path is the data directory's lock file
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	return f, nil
}

// Close closes the databases and releases the lock. It is safe to call once on a
// store Open returned; it returns the first error, after closing everything.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The databases before their connectors, and the lock last, so nothing is
	// written to the directory once another process can take it.
	var errs []error
	if s.scratch != nil {
		errs = append(errs, s.scratch.Close())
	}
	if s.scratchConn != nil {
		errs = append(errs, s.scratchConn.Close())
	}
	if s.index != nil {
		errs = append(errs, s.index.Close())
	}
	if s.indexConn != nil {
		errs = append(errs, s.indexConn.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	s.scratch, s.scratchConn, s.index, s.indexConn, s.lock = nil, nil, nil, nil, nil
	return errors.Join(errs...)
}

// checkKey refuses a source or session id that cannot name a directory.
func checkKey(src, sessionID string) error {
	if !validSource.MatchString(src) {
		return fmt.Errorf("store: source %q is not live, parity or replay-<run>", src)
	}
	if !validSessionID.MatchString(sessionID) {
		return fmt.Errorf("store: session id %q is not a ULID", sessionID)
	}
	return nil
}
