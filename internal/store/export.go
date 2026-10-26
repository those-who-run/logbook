package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Lap is a row of the laps table, with its columns' names as JSON keys. A NULL
// is a nil pointer, written as null.
type Lap struct {
	Src         string    `json:"src"`
	SessionID   string    `json:"session_id"`
	Lap         int32     `json:"lap"`
	LapTimeMs   *int32    `json:"lap_time_ms"`
	Complete    bool      `json:"complete"`
	FuelStart   float64   `json:"fuel_start"`
	FuelEnd     float64   `json:"fuel_end"`
	FuelUsed    float64   `json:"fuel_used"`
	Position    *int32    `json:"position"`
	FirstSeq    int64     `json:"first_seq"`
	LastSeq     int64     `json:"last_seq"`
	Frames      int32     `json:"frames"`
	TopSpeedKph *float64  `json:"top_speed_kph"`
	MinSpeedKph *float64  `json:"min_speed_kph"`
	PathM       float64   `json:"path_m"`
	MaxJumpM    float64   `json:"max_jump_m"`
	File        string    `json:"file"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// Laps returns the laps of src's sessions (`live` when src is empty), ordered by
// their session's start, then by lap.
func (s *Store) Laps(ctx context.Context, src string) (laps []Lap, err error) {
	src = sourceOf(src)
	if !validSource.MatchString(src) {
		return nil, fmt.Errorf("store: source %q is not live, parity or replay-<run>", src)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.index.QueryContext(ctx, `
SELECT l.src, l.session_id, l.lap, l.lap_time_ms, l.complete, l.fuel_start, l.fuel_end,
    l.fuel_used, l.position, l.first_seq, l.last_seq, l.frames, l.top_speed_kph,
    l.min_speed_kph, l.path_m, l.max_jump_m, l.file, l.recorded_at
FROM laps l JOIN sessions s ON s.src = l.src AND s.id = l.session_id
WHERE l.src = ?
ORDER BY s.started_at, s.id, l.lap`, src)
	if err != nil {
		return nil, fmt.Errorf("store: laps: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("store: laps: %w", cerr)
		}
	}()
	for rows.Next() {
		var l Lap
		if err := rows.Scan(&l.Src, &l.SessionID, &l.Lap, &l.LapTimeMs, &l.Complete, &l.FuelStart,
			&l.FuelEnd, &l.FuelUsed, &l.Position, &l.FirstSeq, &l.LastSeq, &l.Frames,
			&l.TopSpeedKph, &l.MinSpeedKph, &l.PathM, &l.MaxJumpM, &l.File, &l.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: laps: %w", err)
		}
		laps = append(laps, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: laps: %w", err)
	}
	return laps, nil
}

// ExportLaps writes src's laps to w as JSON lines, one object per lap, in Laps'
// order: what the parity harness compares.
func (s *Store) ExportLaps(ctx context.Context, w io.Writer, src string) error {
	laps, err := s.Laps(ctx, src)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	for _, l := range laps {
		if err := enc.Encode(l); err != nil {
			return fmt.Errorf("store: export: %w", err)
		}
	}
	return nil
}
