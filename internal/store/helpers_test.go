package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const sid = "01JB8Z3K5Q9T2M7XW4R6N1PDHE"

// A session as ecu would publish it, built here: a race of three laps, each
// lapFrames frames long, with frames at the track before the start and the
// finished frame after the flag. Lap 3 is the one the session ended part way
// through, so it is partial and untimed.
const lapFrames = 600

type session struct {
	started *eventsv1.SessionStarted
	laps    []*eventsv1.LapCompleted
	frames  []*telemetryv1.Frame
	ended   *eventsv1.SessionEnded
}

func race(src, id string) session {
	var s session
	seq := uint32(100)
	frame := func(lap int32, phase telemetryv1.Phase, i int, fuel float64) *telemetryv1.Frame {
		angle := 2 * math.Pi * float64(i) / lapFrames
		f := &telemetryv1.Frame{
			Seq: seq, T: float64(seq) / 60, PacketId: seq + 5000, Lap: lap, Phase: phase,
			PosX: 100 * math.Cos(angle), PosY: 2, PosZ: 100 * math.Sin(angle),
			SpeedKph: 150 + 50*math.Sin(angle) + float64(lap), EngineRpm: 7000,
			Gear: 4, SuggestedGear: 15, Throttle: 255, FuelLevel: fuel,
			TimeOfDayMs: 50_000_000 + int32(seq), //nolint:gosec // a few thousand frames CarCode: 3431, Flags: 1,
		}
		if phase == telemetryv1.Phase_PHASE_IN_RACE || phase == telemetryv1.Phase_PHASE_FINISHED {
			f.Position = proto.Int32(2)
			f.TrackId = proto.Int32(7)
		}
		seq++
		return f
	}
	for i := range 30 {
		s.frames = append(s.frames, frame(0, telemetryv1.Phase_PHASE_AT_TRACK, i, 50))
	}
	s.started = &eventsv1.SessionStarted{
		SessionId: id, Source: src, Game: eventsv1.Game_GAME_GT7,
		SessionType: eventsv1.SessionType_SESSION_TYPE_RACE, CarCode: 3431,
		TotalLaps: proto.Int32(3), FirstSeq: seq,
		StartedAt: timestamppb.New(time.Date(2026, 10, 15, 19, 30, 5, 250e6, time.UTC)),
	}
	fuel := 50.0
	for lap := int32(1); lap <= 3; lap++ {
		first := seq
		start := fuel
		n := lapFrames
		if lap == 3 {
			n = lapFrames / 2
		}
		for i := range n {
			s.frames = append(s.frames, frame(lap, telemetryv1.Phase_PHASE_IN_RACE, i, fuel))
			fuel -= 0.005
		}
		ev := &eventsv1.LapCompleted{
			SessionId: id, Lap: lap, LapTimeMs: 98_000 + lap*100, Complete: true,
			FuelAtStart: start, FuelAtEnd: fuel, FuelUsed: start - fuel,
			Position: proto.Int32(3 - lap + 1), PitRole: eventsv1.PitRole_PIT_ROLE_NONE,
			FirstSeq: first, LastSeq: seq - 1,
		}
		if lap == 3 {
			// The flag falls part way round: the finished frame closes the race.
			s.frames = append(s.frames, frame(4, telemetryv1.Phase_PHASE_FINISHED, n, fuel))
			ev.LapTimeMs, ev.Complete, ev.LastSeq, ev.Position = 0, false, seq-1, nil
		}
		s.laps = append(s.laps, ev)
	}
	for i := range 10 {
		s.frames = append(s.frames, frame(4, telemetryv1.Phase_PHASE_FINISHED, i, fuel))
	}
	s.ended = &eventsv1.SessionEnded{
		SessionId: id, Reason: eventsv1.EndReason_END_REASON_FINISHED,
		Summary: &eventsv1.SessionSummary{
			LapsCompleted: 2, BestLap: proto.Int32(1), BestLapTimeMs: proto.Int32(98_100),
		},
	}
	return s
}

// write stores every event of s, each lap with the whole session's frames, as a
// subscriber that has not cut them would pass them.
func (sess session) write(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	for _, lap := range sess.laps {
		if err := s.WriteLap(ctx, sess.started.GetSource(), lap, sess.frames); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndSession(ctx, sess.started.GetSource(), sess.ended); err != nil {
		t.Fatal(err)
	}
}

// open opens a store on dir, closed when the test ends, on a clock that moves a
// second a call.
func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// query runs q in an in-memory DuckDB of its own, as a reader of the files would,
// and returns its rows as strings.
func query(t *testing.T, q string, args ...any) [][]string {
	t.Helper()
	conn, err := duckdb.NewConnector("", nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer func() {
		if err := errors.Join(db.Close(), conn.Close()); err != nil {
			t.Error(err)
		}
	}()
	return rowsOf(t, t.Context(), db, q, args...)
}

func rowsOf(t *testing.T, ctx context.Context, db *sql.DB, q string, args ...any) [][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = "NULL"
			if v.Valid {
				row[i] = v.String
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// files returns every regular file under dir with its contents, by path relative
// to dir.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // a test's own temporary directory
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
