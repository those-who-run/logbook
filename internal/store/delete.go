package store

import (
	"context"
	"errors"
	"fmt"

	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
)

// The pit wall's two writes (paddock ADR 0014): deleting a session, softly, and
// restoring it. A delete sets the row's deleted_at (schema 8) and keeps every row
// and lap file; a restore sets it back to NULL. No other write touches
// deleted_at, so a session's events delivered again, or `logbook import-v1` run
// again, leave a deleted session deleted.

// ErrConflict is a write the session's state refuses: deleting a session that is
// still open.
var ErrConflict = errors.New("conflict")

// DeleteSession answers svc.logbook.sessions.delete: it deletes the session from
// the first of the sources, in their order, that has it. A session already
// deleted is answered as it is, with the same data version; an open one is
// ErrConflict.
func (s *Store) DeleteSession(ctx context.Context, req *storev1.DeleteSessionRequest) (*storev1.DeleteSessionResponse, error) {
	row, version, err := s.setDeleted(ctx, req.GetSources(), req.GetSessionId(), true)
	if err != nil {
		return nil, err
	}
	return &storev1.DeleteSessionResponse{Session: row, DataVersion: version}, nil
}

// RestoreSession answers svc.logbook.sessions.restore: it restores the session
// from the first of the sources, in their order, that has it. A session not
// deleted is answered as it is, with the same data version.
func (s *Store) RestoreSession(ctx context.Context, req *storev1.RestoreSessionRequest) (*storev1.RestoreSessionResponse, error) {
	row, version, err := s.setDeleted(ctx, req.GetSources(), req.GetSessionId(), false)
	if err != nil {
		return nil, err
	}
	return &storev1.RestoreSessionResponse{Session: row, DataVersion: version}, nil
}

// setDeleted deletes the session, or restores it, and answers its row after, with
// the data version after: a new one only when the row changed.
func (s *Store) setDeleted(ctx context.Context, sources []string, id string, deleted bool) (_ *storev1.SessionRow, _ string, err error) {
	if err := checkSources(sources); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == nil {
		return nil, "", errors.New("store: closed")
	}
	tx, err := s.index.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("store: %w", err)
	}
	defer func() { err = rollback(tx, err) }()
	row, err := findSession(ctx, tx, sources, id)
	if err != nil {
		return nil, "", fmt.Errorf("store: %w", err)
	}
	switch {
	case (row.GetDeletedAt() != nil) == deleted:
		// Already so: nothing changes, nor the data version.
		return row, s.DataVersion(), tx.Commit()
	case deleted && !row.GetClosed():
		return nil, "", fmt.Errorf("%w: session %s/%s is open: only a closed session can be deleted",
			ErrConflict, row.GetSource(), id)
	}
	var at any
	if deleted {
		at = s.now().UTC()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET deleted_at = ? WHERE src = ? AND id = ?`,
		at, row.GetSource(), id); err != nil {
		return nil, "", fmt.Errorf("store: %w", err)
	}
	if row, err = findSession(ctx, tx, []string{row.GetSource()}, id); err != nil {
		return nil, "", fmt.Errorf("store: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("store: %w", err)
	}
	s.touch()
	return row, s.DataVersion(), nil
}
