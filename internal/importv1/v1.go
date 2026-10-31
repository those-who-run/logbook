package importv1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// v1's data directory, as v1.0.0's store.py lays it out:
//
//	index.sqlite                     sessions and laps tables (WAL journal)
//	sessions/<id>/lap-NN.parquet     the frames v1 kept for lap NN
//	sessions/<id>/summary.json       the session's summary, written at its end
//
// The import reads summary.json for v1's rows, not index.sqlite: it holds every
// column of the index's rows and more (each lap's pit role, the kind, the track,
// the pit stops), and needs no SQLite reader. index.sqlite is only checked for a
// live -wal (R1).
const (
	v1Index    = "index.sqlite"
	v1Sessions = "sessions"
	v1Summary  = "summary.json"
)

// v1ID is v1's session id: its start, UTC, with -2, -3 and so on for a second
// session in the same second (SessionStore.start_session).
var v1ID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9]{1,4})?$`)

// v1SessionSummary is the part of v1's summary.json the import reads
// (SessionStore.end_session at v1.0.0). A field older summaries lack is nil.
type v1SessionSummary struct {
	SessionID     string      `json:"session_id"`
	Type          string      `json:"type"`
	StartedAt     float64     `json:"started_at"`
	EndedAt       *float64    `json:"ended_at"`
	DrivingMs     *int64      `json:"driving_ms"`
	PausedMs      *int64      `json:"paused_ms"`
	EndedPaused   bool        `json:"ended_paused"`
	Track         *v1Track    `json:"track"`
	Kind          *v1Kind     `json:"session_kind"`
	CarCode       int32       `json:"car_code"`
	CarName       *string     `json:"car_name"`
	TotalLaps     int32       `json:"total_laps"`
	LapsCompleted int32       `json:"laps_completed"`
	BestLapMs     *int32      `json:"best_lap_ms"`
	FastestLapMs  *int32      `json:"fastest_lap_ms"`
	Finished      bool        `json:"finished"`
	JoinedOnLap   *int32      `json:"joined_on_lap"`
	TotalTimeMs   *int32      `json:"total_time_ms"`
	FinalPosition *int32      `json:"final_position"`
	TotalCars     *int32      `json:"total_cars"`
	FuelBurned    *float64    `json:"fuel_burned"`
	FuelByStint   []float64   `json:"fuel_burned_by_stint"`
	FuelStints    []float64   `json:"fuel_stints"`
	GT7BestLapMs  *int32      `json:"gt7_best_lap_ms"`
	FuelCapacity  *float64    `json:"fuel_capacity"`
	FuelAtStart   *float64    `json:"fuel_at_start"`
	FuelAtEnd     *float64    `json:"fuel_at_end"`
	FuelUsed      *float64    `json:"fuel_used"`
	Stints        *int32      `json:"stints"`
	PitStops      []v1PitStop `json:"pit_stops"`
	Shutdown      bool        `json:"engineer_shutdown"`
	Laps          []v1Lap     `json:"laps"`
}

type v1Track struct {
	ID         *int32  `json:"id"`
	Name       *string `json:"name"`
	Confidence string  `json:"confidence"`
	Family     *string `json:"family"`
	Evidence   string  `json:"evidence"`
}

type v1Kind struct {
	Kind        string  `json:"kind"`
	TotalLaps   *int32  `json:"total_laps"`
	Field       bool    `json:"field"`
	FuelUse     *bool   `json:"fuel_use"`
	CarsOnTrack *bool   `json:"cars_on_track"`
	Start       *string `json:"start"`
	Evidence    string  `json:"evidence"`
}

type v1PitStop struct {
	Stop           int32    `json:"stop"`
	InLap          int32    `json:"in_lap"`
	OutLap         *int32   `json:"out_lap"`
	PitLaneSecs    *float64 `json:"pit_lane_secs"`
	StationarySecs float64  `json:"stationary_secs"`
	FuelBefore     float64  `json:"fuel_before"`
	FuelAfter      float64  `json:"fuel_after"`
	FuelAdded      float64  `json:"fuel_added"`
	TyresChanged   bool     `json:"tyres_changed"`
	PitLaneKph     *float64 `json:"pit_lane_kph"`
	PositionBefore *int32   `json:"position_before"`
	PositionAfter  *int32   `json:"position_after"`
	Evidence       string   `json:"evidence"`
}

// v1Lap is one of summary.json's laps, as _flush_lap wrote it. Pit is v1's pit
// role for the lap: "in", "out", "in_out", or nil.
type v1Lap struct {
	Lap       int32    `json:"lap"`
	LapTimeMs *int32   `json:"lap_time_ms"`
	Complete  bool     `json:"complete"`
	FuelStart *float64 `json:"fuel_start"`
	FuelEnd   *float64 `json:"fuel_end"`
	FuelUsed  *float64 `json:"fuel_used"`
	Position  *int32   `json:"position"`
	Pit       *string  `json:"pit"`
	Frames    int      `json:"frames"`
	File      string   `json:"file"`
}

// ErrLive is a v1 index still being written: its -wal is newer than it (R1).
var ErrLive = errors.New("v1's index has a -wal newer than itself: it is still being written; import from a copy taken with v1 stopped")

// checkV1Dir checks that dir is v1's data directory, and refuses one whose index
// has a -wal file, not empty, newer than the index: v1 (or something else) still
// has it open, and the copy may be torn (R1).
func checkV1Dir(dir string) error {
	index, err := os.Stat(filepath.Join(dir, v1Index))
	if err != nil {
		return fmt.Errorf("%s is not v1's data directory: %w", dir, err)
	}
	wal, err := os.Stat(filepath.Join(dir, v1Index+"-wal"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if wal.Size() > 0 && !wal.ModTime().Before(index.ModTime()) {
		return ErrLive
	}
	return nil
}

// v1SessionIDs are the session directories under dir/sessions with a v1 id, in
// order, oldest first.
func v1SessionIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, v1Sessions))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && v1ID.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// readSummary reads a v1 session's summary.json. A session v1 never ended has
// none: os.ErrNotExist.
func readSummary(dir, id string) (*v1SessionSummary, error) {
	data, err := os.ReadFile(filepath.Join(dir, v1Sessions, id, v1Summary)) //nolint:gosec // under the directory the maintainer named
	if err != nil {
		return nil, err
	}
	var sum v1SessionSummary
	if err := json.Unmarshal(data, &sum); err != nil {
		return nil, fmt.Errorf("%s: %w", v1Summary, err)
	}
	if sum.SessionID != id {
		return nil, fmt.Errorf("%s names session %q, not %q", v1Summary, sum.SessionID, id)
	}
	return &sum, nil
}

// v1FrameColumns are the lap files' columns at v1.0.0 (store.py's
// FRAME_COLUMNS). rot_* and ang_vel_* came later than the rest (v1#77), so older
// files lack them, and they read as 0, as v1's from_dict read a missing key.
var v1FrameColumns = []string{
	"t", "packet_id", "lap", "phase", "pos_x", "pos_y", "pos_z", "vel_x", "vel_y", "vel_z",
	"speed_kph", "engine_rpm", "gear", "suggested_gear", "throttle", "brake", "fuel_level",
	"tire_fl_temp", "tire_fr_temp", "tire_rl_temp", "tire_rr_temp",
	"tire_fl_radius", "tire_fr_radius", "tire_rl_radius", "tire_rr_radius",
	"sus_fl", "sus_fr", "sus_rl", "sus_rr",
	"wheel_fl_rps", "wheel_fr_rps", "wheel_rl_rps", "wheel_rr_rps",
	"water_temp", "oil_temp", "oil_pressure", "boost_pressure", "body_height",
	"position", "time_of_day_ms", "car_code", "track_id",
	"rot_x", "rot_y", "rot_z", "orientation", "ang_vel_x", "ang_vel_y", "ang_vel_z",
}

// coreColumns are the columns without which a lap's frames cannot be judged
// again: a file lacking one is a session imported from v1's rows (ADR 0009,
// point 5).
var coreColumns = []string{"t", "packet_id", "lap", "phase", "pos_x", "pos_z", "speed_kph", "fuel_level"}

// v1Frame is one row of a lap file, by column name; a column the file lacks, or
// a NULL, is absent.
type v1Frame map[string]any

// readLapFile reads a lap file's rows, in the file's order, through DuckDB, and
// the core columns it lacks.
func readLapFile(ctx context.Context, db *sql.DB, path string) (frames []v1Frame, missing []string, err error) {
	have := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM (DESCRIBE SELECT * FROM read_parquet(?))`, path)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		have[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	var cols []string
	for _, c := range v1FrameColumns {
		if have[c] {
			cols = append(cols, `"`+c+`"`)
		}
	}
	for _, c := range coreColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(cols) == 0 {
		return nil, missing, nil
	}
	rows, err = db.QueryContext(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM read_parquet(?)`, path) //nolint:gosec // column names from the list above
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	names, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		f := make(v1Frame, len(names))
		for i, n := range names {
			if vals[i] != nil {
				f[n] = vals[i]
			}
		}
		frames = append(frames, f)
	}
	return frames, missing, rows.Err()
}
