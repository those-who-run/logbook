package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// StartSession opens ev's session in the index, under ev's source (`live` when it
// names none). Starting the same session again changes nothing; total_laps stays
// a session.kind's once one has set it (kind_seq).
func (s *Store) StartSession(ctx context.Context, ev *eventsv1.SessionStarted) error {
	src := sourceOf(ev.GetSource())
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return err
	}
	if ev.GetStartedAt() == nil {
		return fmt.Errorf("store: session %s has no started_at", ev.GetSessionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.index.ExecContext(ctx, `
INSERT INTO sessions (src, id, started_at, type, car_code, total_laps, joined_part_way, first_seq,
    started_fuel_at_start, car_name, joined_on_lap, fuel_capacity)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (src, id) DO UPDATE SET
    started_at = excluded.started_at,
    type = excluded.type,
    car_code = excluded.car_code,
    total_laps = CASE WHEN sessions.kind_seq IS NULL THEN excluded.total_laps ELSE sessions.total_laps END,
    joined_part_way = excluded.joined_part_way,
    first_seq = excluded.first_seq,
    started_fuel_at_start = excluded.started_fuel_at_start,
    car_name = COALESCE(sessions.car_name, excluded.car_name),
    joined_on_lap = COALESCE(sessions.joined_on_lap, excluded.joined_on_lap),
    fuel_capacity = COALESCE(sessions.fuel_capacity, excluded.fuel_capacity)`,
		src, ev.GetSessionId(), ev.GetStartedAt().AsTime().UTC(),
		enumName(ev.GetSessionType().String(), "SESSION_TYPE_"), ev.GetCarCode(),
		optional(ev.TotalLaps), ev.GetJoinedPartWay(), int64(ev.GetFirstSeq()),
		fuelAtStart(ev), textOrNull(ev.GetCarName()), optional(ev.JoinedOnLap), positiveOrNull(ev.GetFuelCapacity()))
	if err != nil {
		return fmt.Errorf("store: start session %s/%s: %w", src, ev.GetSessionId(), err)
	}
	return nil
}

// WriteLap writes lap's file from frames, then its row. frames may hold more than
// the lap, in any order and with repeats: the file takes the racing frames in the
// lap's seq range (racingFrames). Writing the same lap again leaves one file and
// one row, with the same content.
func (s *Store) WriteLap(ctx context.Context, src string, lap *eventsv1.LapCompleted, frames []*telemetryv1.Frame) error {
	src = sourceOf(src)
	if err := checkKey(src, lap.GetSessionId()); err != nil {
		return err
	}
	if lap.GetLap() < 1 {
		return fmt.Errorf("store: lap %d of session %s: laps count from 1", lap.GetLap(), lap.GetSessionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sessionExists(ctx, src, lap.GetSessionId()); err != nil {
		return err
	}
	_, err := s.writeLap(ctx, src, lap, frames, nil)
	return err
}

// writeLap writes lap's file from frames, then its row, and runs also, when it is
// not nil, in the row's transaction. It returns the frames the file holds. The
// caller holds s.mu and has checked the session.
func (s *Store) writeLap(ctx context.Context, src string, lap *eventsv1.LapCompleted, frames []*telemetryv1.Frame,
	also func(*sql.Tx) error,
) (int, error) {
	racing := racingFrames(frames, lap.GetFirstSeq(), lap.GetLastSeq())
	rel := filepath.Join(lapsDir, src, lap.GetSessionId(), fmt.Sprintf("lap-%03d.parquet", lap.GetLap()))
	// The file before its row (v1 #148): a reader that finds the row finds the file.
	if err := s.writeLapFile(ctx, filepath.Join(s.dir, rel), racing); err != nil {
		return 0, fmt.Errorf("store: lap %d of %s/%s: %w", lap.GetLap(), src, lap.GetSessionId(), err)
	}
	if err := s.writeLapRow(ctx, src, lap, racing, filepath.ToSlash(rel), also); err != nil {
		return 0, fmt.Errorf("store: lap %d of %s/%s: %w", lap.GetLap(), src, lap.GetSessionId(), err)
	}
	return len(racing), nil
}

// fuelAtStart is ev's fuel on the session's first frame, or NULL for an event from
// before contracts v0.1.4, which has none. proto3 sends no zero, so a 0 is told
// apart by the session type: the types ecu sends from v0.1.4 on (LAP_RACE and
// after, RANKED from v0.2.7 included) are newer than v0.1.3, so with one of them a
// 0 is a real empty tank, and with any other it is an event that carries no
// start fuel.
func fuelAtStart(ev *eventsv1.SessionStarted) driver.Value {
	if ev.GetFuelAtStart() == 0 && !fromV014(ev.GetSessionType()) {
		return nil
	}
	return ev.GetFuelAtStart()
}

// fromV014 says whether t is one of the session types contracts v0.1.4 added, or
// a later one (SESSION_TYPE_RANKED, v0.2.7): every value from LAP_RACE on.
func fromV014(t eventsv1.SessionType) bool {
	return t >= eventsv1.SessionType_SESSION_TYPE_LAP_RACE
}

// endedFields are the columns of ev's own summary fields that contracts v0.1.4
// added: last_seq, ended_fuel_used, ended_final_position and session_kind, all NULL
// for an event from before v0.1.4. Such an event has no last_seq, and a later one
// always has: seq counts from 1.
func endedFields(ev *eventsv1.SessionEnded) [4]driver.Value {
	if ev.GetLastSeq() == 0 {
		return [4]driver.Value{}
	}
	summary := ev.GetSummary()
	return [4]driver.Value{
		int64(ev.GetLastSeq()), summary.GetFuelUsed(), optional(summary.FinalPosition),
		enumName(summary.GetSessionType().String(), "SESSION_TYPE_"),
	}
}

// EndSession records how ev's session ended, at endedAt: the time the event was
// published (its ce-time), which the caller passes because SessionEnded carries no
// time of its own, and which is right for a replay or a catch-up where the store's
// clock is not. The summary columns (laps_completed, best_lap_ms, final_position,
// end_reason, ended_at, and endedFields') come from the first end and only from it:
// ending the session again, or writing one of its laps after the end, leaves them
// alone.
func (s *Store) EndSession(ctx context.Context, src string, ev *eventsv1.SessionEnded, endedAt time.Time) (err error) {
	src = sourceOf(src)
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return err
	}
	if endedAt.IsZero() {
		return fmt.Errorf("store: session %s has no end time", ev.GetSessionId())
	}
	if ev.GetSummary() == nil {
		return fmt.Errorf("store: the end of session %s has no summary", ev.GetSessionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sessionExists(ctx, src, ev.GetSessionId()); err != nil {
		return err
	}
	summary := ev.GetSummary()
	ended := endedFields(ev)
	kept, err := summaryFields(summary)
	if err != nil {
		return fmt.Errorf("store: end session %s/%s: %w", src, ev.GetSessionId(), err)
	}
	tx, err := s.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { err = rollback(tx, err) }()
	if err = refreshSession(ctx, tx, src, ev.GetSessionId()); err != nil {
		return err
	}
	// The summary is the event's word on the laps. With no finishing position,
	// the final position is the last one a lap recorded by the end, as v1's was;
	// a lap written later does not change it.
	_, err = tx.ExecContext(ctx, `
UPDATE sessions SET
    ended_at = ?,
    end_reason = ?,
    laps_completed = ?,
    best_lap_ms = ?,
    final_position = COALESCE(?, (SELECT position FROM laps
        WHERE src = ? AND session_id = ? AND position IS NOT NULL ORDER BY lap DESC LIMIT 1)),
    last_seq = ?,
    ended_fuel_used = ?,
    ended_final_position = ?,
    session_kind = ?,
    `+summarySet+`
WHERE src = ? AND id = ? AND ended_at IS NULL`, //nolint:gosec // a constant
		slices.Concat([]any{endedAt.UTC(), enumName(ev.GetReason().String(), "END_REASON_"),
			summary.GetLapsCompleted(), optional(summary.BestLapTimeMs),
			optional(summary.FinishingPosition), src, ev.GetSessionId(),
			ended[0], ended[1], ended[2], ended[3]},
			kept, []any{src, ev.GetSessionId()})...)
	if err != nil {
		return fmt.Errorf("store: end session %s/%s: %w", src, ev.GetSessionId(), err)
	}
	return tx.Commit()
}

// sourceOf is a source as stored: an empty one is live.
func sourceOf(src string) string {
	if src == "" {
		return SourceLive
	}
	return src
}

func (s *Store) sessionExists(ctx context.Context, src, sessionID string) error {
	var n int
	err := s.index.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE src = ? AND id = ?`, src, sessionID).Scan(&n)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s/%s", ErrUnknownSession, src, sessionID)
	}
	return nil
}

// writeLapFile writes frames to path as Parquet: through the scratch table to a
// temporary file in the same directory, synced, then renamed over path, so a crash
// leaves the old file or the new one, never half of one.
func (s *Store) writeLapFile(ctx context.Context, path string, frames []*telemetryv1.Frame) (err error) {
	dir := filepath.Dir(path)
	if err := mkdirSynced(dir); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	conn, err := s.scratch.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	// Empty before and after, so a write that failed half way leaves nothing behind
	// for the next.
	if _, err := conn.ExecContext(ctx, `DELETE FROM frames`); err != nil {
		return err
	}
	defer func() {
		if _, derr := conn.ExecContext(context.WithoutCancel(ctx), `DELETE FROM frames`); err == nil {
			err = derr
		}
	}()
	if err := conn.Raw(func(dc any) error {
		c, ok := dc.(driver.Conn)
		if !ok {
			return fmt.Errorf("the scratch connection is a %T, not a driver.Conn", dc)
		}
		return appendFrames(c, frames)
	}); err != nil {
		return fmt.Errorf("append frames: %w", err)
	}
	// COPY takes no parameters, so the path is a quoted literal: the store's own
	// path, with its quotes doubled, and a source and session id checkKey allowed.
	copySQL := fmt.Sprintf( //nolint:gosec // see above: no input reaches it unchecked
		`COPY (SELECT * FROM frames ORDER BY seq) TO '%s'
(FORMAT PARQUET, COMPRESSION ZSTD, KV_METADATA {frame_schema_version: '%d'})`,
		strings.ReplaceAll(tmp, "'", "''"), FrameSchemaVersion)
	if _, err := conn.ExecContext(ctx, copySQL); err != nil {
		return fmt.Errorf("copy to parquet: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncFile(dir)
}

// mkdirSynced makes dir and any parents it lacks, and syncs the parent of each
// directory it made. A new directory's entry lives in its parent, so without that
// a power loss could keep the lap's row, committed through DuckDB's WAL, and lose
// the session directory that holds its file.
func mkdirSynced(dir string) error {
	var made []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || !errors.Is(err, os.ErrNotExist) || filepath.Dir(d) == d {
			break
		}
		made = append(made, d)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// From the top down, so each parent's entry is on disk before its child's.
	for i := len(made) - 1; i >= 0; i-- {
		if err := syncFile(filepath.Dir(made[i])); err != nil {
			return err
		}
	}
	return nil
}

// syncFile flushes a file, or a directory's entries, to the disk.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // a path the store made under its own directory
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// writeLapRow upserts lap's row and brings its session's totals up to date, in one
// transaction. recorded_at is kept from the first write, so a repeat leaves the row
// as it was.
func (s *Store) writeLapRow(ctx context.Context, src string, lap *eventsv1.LapCompleted, frames []*telemetryv1.Frame, file string,
	also func(*sql.Tx) error,
) (err error) {
	facts := factsOf(frames)
	var lapTime driver.Value // a lap with no time (a partial one) is NULL, as in v1
	if lap.GetLapTimeMs() > 0 {
		lapTime = lap.GetLapTimeMs()
	}
	tx, err := s.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { err = rollback(tx, err) }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO laps (src, session_id, lap, lap_time_ms, complete, fuel_start, fuel_end, fuel_used,
    position, first_seq, last_seq, frames, top_speed_kph, min_speed_kph, path_m, max_jump_m,
    file, recorded_at, pit_role, counts_for_best)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (src, session_id, lap) DO UPDATE SET
    lap_time_ms = excluded.lap_time_ms,
    complete = excluded.complete,
    fuel_start = excluded.fuel_start,
    fuel_end = excluded.fuel_end,
    fuel_used = excluded.fuel_used,
    position = excluded.position,
    first_seq = excluded.first_seq,
    last_seq = excluded.last_seq,
    frames = excluded.frames,
    top_speed_kph = excluded.top_speed_kph,
    min_speed_kph = excluded.min_speed_kph,
    path_m = excluded.path_m,
    max_jump_m = excluded.max_jump_m,
    file = excluded.file,
    pit_role = excluded.pit_role,
    counts_for_best = excluded.counts_for_best`,
		src, lap.GetSessionId(), lap.GetLap(), lapTime, lap.GetComplete(),
		lap.GetFuelAtStart(), lap.GetFuelAtEnd(), lap.GetFuelUsed(),
		optional(lap.Position), int64(lap.GetFirstSeq()), int64(lap.GetLastSeq()), len(frames),
		floatOrNull(facts.topSpeedKph), floatOrNull(facts.minSpeedKph), facts.pathM, facts.maxJumpM,
		file, s.now().UTC(), pitRole(lap), countsForBest(lap))
	if err != nil {
		return err
	}
	if err = refreshSession(ctx, tx, src, lap.GetSessionId()); err != nil {
		return err
	}
	if also != nil {
		if err = also(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// refreshSession works out a session's totals from its laps, as v1 did. The fuel
// at the first lap's start and the fuel all its laps used come from the laps
// always. The complete laps and the best complete lap do only until the session
// ends: from then on they are the summary's (EndSession), which a late or
// redelivered lap must not overwrite. Once ecu has named a best lap (lap.best,
// best_lap), best_lap_ms is its word, not the laps' minimum.
func refreshSession(ctx context.Context, tx *sql.Tx, src, sessionID string) error {
	_, err := tx.ExecContext(ctx, `
UPDATE sessions SET
    laps_completed = CASE WHEN ended_at IS NULL
        THEN (SELECT count(*) FROM laps WHERE src = $1 AND session_id = $2 AND complete)
        ELSE laps_completed END,
    best_lap_ms = CASE WHEN ended_at IS NULL AND best_lap IS NULL
        THEN (SELECT min(lap_time_ms) FROM laps WHERE src = $1 AND session_id = $2 AND complete)
        ELSE best_lap_ms END,
    fuel_at_start = (SELECT fuel_start FROM laps WHERE src = $1 AND session_id = $2 ORDER BY lap LIMIT 1),
    fuel_used = (SELECT round(sum(fuel_used), 3) FROM laps WHERE src = $1 AND session_id = $2)
WHERE src = $1 AND id = $2`, src, sessionID)
	return err
}

func floatOrNull(v *float64) driver.Value {
	if v == nil {
		return nil
	}
	return *v
}
