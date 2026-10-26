package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
)

// StagedFrame is a frame on its way to a lap file: staged in frames_pending, under
// its source and session, until the lap it belongs to is written (paddock#38).
type StagedFrame struct {
	Src       string
	SessionID string
	// Run is the ecu run that published the frame, from its ce-id
	// `<run>:frame:<seq>`.
	Run   string
	Frame *telemetryv1.Frame
}

// OpenSession is a session that has started and not yet ended, as a subscriber
// needs it to place frames after a restart.
type OpenSession struct {
	Src       string
	ID        string
	FirstSeq  uint32
	StartedAt time.Time
	// Run is the run of the session's first staged frame, "" until one is staged.
	Run string
	// MaxStaged is the highest seq staged for the session, -1 when none is.
	MaxStaged int64
}

// SessionState says whether a session is in the index, and whether it has ended.
type SessionState int

// The states of a session.
const (
	SessionUnknown SessionState = iota
	SessionOpen
	SessionEnded
)

// stageRows is how many frames one INSERT carries: enough that a batch of
// hundreds is a few statements, few enough to keep each statement small.
const stageRows = 100

// StageFrames writes frames to frames_pending in one transaction: a frame staged
// again (a redelivery) overwrites its own row. Each session takes the run of its
// first staged frame.
func (s *Store) StageFrames(ctx context.Context, frames []StagedFrame) (err error) {
	if len(frames) == 0 {
		return nil
	}
	for _, f := range frames {
		if err := checkKey(f.Src, f.SessionID); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.index.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: stage frames: %w", err)
	}
	defer func() { err = rollback(tx, err) }()
	runs := map[[2]string]string{}
	for start := 0; start < len(frames); start += stageRows {
		chunk := frames[start:min(start+stageRows, len(frames))]
		args := make([]any, 0, 5*len(chunk))
		for _, f := range chunk {
			b, merr := proto.Marshal(f.Frame)
			if merr != nil {
				return fmt.Errorf("store: stage frame %d: %w", f.Frame.GetSeq(), merr)
			}
			args = append(args, f.Src, f.SessionID, int64(f.Frame.GetSeq()), f.Run, b)
			if _, ok := runs[[2]string{f.Src, f.SessionID}]; !ok {
				runs[[2]string{f.Src, f.SessionID}] = f.Run
			}
		}
		// Only placeholders are repeated into the statement; the values are its
		// arguments.
		q := `INSERT INTO frames_pending (src, session_id, seq, run, frame) VALUES ` + //nolint:gosec // see above
			strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?), ", len(chunk)), ", ") +
			` ON CONFLICT (src, session_id, seq) DO UPDATE SET run = excluded.run, frame = excluded.frame`
		if _, err = tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("store: stage frames: %w", err)
		}
	}
	for key, run := range runs {
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET run = ? WHERE src = ? AND id = ? AND run IS NULL`,
			run, key[0], key[1]); err != nil {
			return fmt.Errorf("store: stage frames: %w", err)
		}
	}
	return tx.Commit()
}

// WriteStagedLap writes lap from the frames staged for its session, and in the
// row's transaction deletes the staged frames up to the lap's last, which no later
// lap needs. It returns how many frames in the lap's seq range were staged, which
// is fewer than the range when some never arrived, and how many the file holds.
func (s *Store) WriteStagedLap(ctx context.Context, src string, lap *eventsv1.LapCompleted) (staged, written int, err error) {
	src = sourceOf(src)
	if err := checkKey(src, lap.GetSessionId()); err != nil {
		return 0, 0, err
	}
	if lap.GetLap() < 1 {
		return 0, 0, fmt.Errorf("store: lap %d of session %s: laps count from 1", lap.GetLap(), lap.GetSessionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sessionExists(ctx, src, lap.GetSessionId()); err != nil {
		return 0, 0, err
	}
	// The frame before the lap's first as well: it says whether a finished frame at
	// the lap's start closes a race (racingFrames).
	from := int64(lap.GetFirstSeq()) - 1
	frames, err := s.stagedFrames(ctx, src, lap.GetSessionId(), from, int64(lap.GetLastSeq()))
	if err != nil {
		return 0, 0, err
	}
	for _, f := range frames {
		if f.GetSeq() >= lap.GetFirstSeq() {
			staged++
		}
	}
	written, err = s.writeLap(ctx, src, lap, frames, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM frames_pending WHERE src = ? AND session_id = ? AND seq <= ?`,
			src, lap.GetSessionId(), int64(lap.GetLastSeq()))
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return staged, written, nil
}

func (s *Store) stagedFrames(ctx context.Context, src, sessionID string, from, to int64) (frames []*telemetryv1.Frame, err error) {
	rows, err := s.index.QueryContext(ctx, `
SELECT frame FROM frames_pending WHERE src = ? AND session_id = ? AND seq BETWEEN ? AND ? ORDER BY seq`,
		src, sessionID, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: staged frames: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("store: staged frames: %w", cerr)
		}
	}()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("store: staged frames: %w", err)
		}
		f := &telemetryv1.Frame{}
		if err := proto.Unmarshal(b, f); err != nil {
			return nil, fmt.Errorf("store: staged frame: %w", err)
		}
		frames = append(frames, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: staged frames: %w", err)
	}
	return frames, nil
}

// DropStaged deletes whatever is still staged for a session, once it has ended: the
// frames after its last lap, which no lap will take.
func (s *Store) DropStaged(ctx context.Context, src, sessionID string) error {
	src = sourceOf(src)
	if err := checkKey(src, sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.index.ExecContext(ctx, `DELETE FROM frames_pending WHERE src = ? AND session_id = ?`, src, sessionID); err != nil {
		return fmt.Errorf("store: drop staged frames of %s/%s: %w", src, sessionID, err)
	}
	return nil
}

// OpenSessions returns the sessions that have not ended, ordered by start, with
// their runs and the highest seq staged for each.
func (s *Store) OpenSessions(ctx context.Context) (out []OpenSession, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.index.QueryContext(ctx, `
SELECT s.src, s.id, s.first_seq, s.started_at, coalesce(s.run, ''),
    coalesce((SELECT max(seq) FROM frames_pending f WHERE f.src = s.src AND f.session_id = s.id), -1)
FROM sessions s WHERE s.ended_at IS NULL ORDER BY s.started_at, s.src, s.id`)
	if err != nil {
		return nil, fmt.Errorf("store: open sessions: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("store: open sessions: %w", cerr)
		}
	}()
	for rows.Next() {
		var o OpenSession
		var first int64
		if err := rows.Scan(&o.Src, &o.ID, &first, &o.StartedAt, &o.Run, &o.MaxStaged); err != nil {
			return nil, fmt.Errorf("store: open sessions: %w", err)
		}
		o.FirstSeq = uint32(first) //nolint:gosec // written from a uint32
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: open sessions: %w", err)
	}
	return out, nil
}

// Session says whether src's session id is in the index, and whether it has ended.
func (s *Store) Session(ctx context.Context, src, sessionID string) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ended sql.NullTime
	err := s.index.QueryRowContext(ctx, `SELECT ended_at FROM sessions WHERE src = ? AND id = ?`,
		sourceOf(src), sessionID).Scan(&ended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionUnknown, nil
	case err != nil:
		return SessionUnknown, fmt.Errorf("store: session %s/%s: %w", src, sessionID, err)
	case ended.Valid:
		return SessionEnded, nil
	}
	return SessionOpen, nil
}

// HasLap says whether src's session has a row for lap.
func (s *Store) HasLap(ctx context.Context, src, sessionID string, lap int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.index.QueryRowContext(ctx, `SELECT count(*) FROM laps WHERE src = ? AND session_id = ? AND lap = ?`,
		sourceOf(src), sessionID, lap).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: lap %d of %s/%s: %w", lap, src, sessionID, err)
	}
	return n > 0, nil
}

// Sessions is how many sessions the index holds, of every source.
func (s *Store) Sessions(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.index.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: sessions: %w", err)
	}
	return n, nil
}
