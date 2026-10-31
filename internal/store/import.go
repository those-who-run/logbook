package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MarkImported records how `logbook import-v1` made an imported session
// (schema 7, paddock#264): fromRows true when it was taken from v1's own rows,
// false when it was re-derived from v1's frames. It is the import's last write
// for a session, so Imported finds only a session written whole.
func (s *Store) MarkImported(ctx context.Context, sessionID string, fromRows bool) error {
	if err := checkKey(SourceV1Import, sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.touch()
	if err := s.sessionExists(ctx, SourceV1Import, sessionID); err != nil {
		return err
	}
	_, err := s.index.ExecContext(ctx, `UPDATE sessions SET from_v1_rows = ? WHERE src = ? AND id = ?`,
		fromRows, SourceV1Import, sessionID)
	if err != nil {
		return fmt.Errorf("store: mark %s/%s imported: %w", SourceV1Import, sessionID, err)
	}
	return nil
}

// Imported says whether an import has written v1's session id whole, and if so
// whether from v1's rows. A session an import stopped part way through is not
// imported, so the next import writes it again.
func (s *Store) Imported(ctx context.Context, sessionID string) (imported, fromRows bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var flag sql.NullBool
	err = s.index.QueryRowContext(ctx, `SELECT from_v1_rows FROM sessions WHERE src = ? AND id = ? AND ended_at IS NOT NULL`,
		SourceV1Import, sessionID).Scan(&flag)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("store: session %s/%s: %w", SourceV1Import, sessionID, err)
	}
	return flag.Valid, flag.Bool, nil
}
