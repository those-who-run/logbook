package store

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
)

func lapFile(dir, src string, lap int) string {
	return filepath.Join(dir, "sessions", src, sid, fmt.Sprintf("lap-%03d.parquet", lap))
}

// AC1: one Parquet file per lap, and rows whose lap times and fuel are the events'.
func TestWritesAFilePerLapAndRowsEqualToTheEvents(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	sess := race("live", sid)
	sess.write(t, s)

	laps, err := s.Laps(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(laps) != len(sess.laps) {
		t.Fatalf("%d laps stored, want %d", len(laps), len(sess.laps))
	}
	for i, got := range laps {
		ev := sess.laps[i]
		if _, err := os.Stat(lapFile(dir, "live", int(ev.GetLap()))); err != nil {
			t.Errorf("lap %d: %v", ev.GetLap(), err)
		}
		if got.Lap != ev.GetLap() || got.FuelStart != ev.GetFuelAtStart() ||
			got.FuelEnd != ev.GetFuelAtEnd() || got.FuelUsed != ev.GetFuelUsed() ||
			got.Complete != ev.GetComplete() {
			t.Errorf("lap %d row %+v, want the event's %v", ev.GetLap(), got, ev)
		}
		wantTime := ev.LapTimeMs
		if ev.GetLapTimeMs() == 0 {
			wantTime = 0 // a partial lap has no time: NULL
		}
		if (got.LapTimeMs == nil) != (wantTime == 0) || (got.LapTimeMs != nil && *got.LapTimeMs != wantTime) {
			t.Errorf("lap %d time %v, want %d (0 for none)", ev.GetLap(), got.LapTimeMs, wantTime)
		}
		if !proto.Equal(&eventsv1.LapCompleted{Position: ev.Position}, &eventsv1.LapCompleted{Position: got.Position}) {
			t.Errorf("lap %d position %v, want %v", ev.GetLap(), got.Position, ev.Position)
		}
		if want := fmt.Sprintf("sessions/live/%s/lap-%03d.parquet", sid, ev.GetLap()); got.File != want {
			t.Errorf("lap %d file %q, want %q", ev.GetLap(), got.File, want)
		}
	}
	if got := laps[0].Frames; got != lapFrames {
		t.Errorf("lap 1 has %d frames, want %d", got, lapFrames)
	}

	session := rowsOf(t, t.Context(), s.index, `SELECT src, id, started_at, ended_at IS NOT NULL, type,
car_code, total_laps, joined_part_way, laps_completed, best_lap_ms, fuel_at_start, fuel_used,
final_position, end_reason FROM sessions`)
	want := [][]string{{
		"live", sid, "2026-10-15T19:30:05.25Z", "true", "race", "3431", "3", "false", "2", "98100", "50",
		fmt.Sprint(roundTo3(sess.laps[0].GetFuelUsed() + sess.laps[1].GetFuelUsed() + sess.laps[2].GetFuelUsed())),
		"2", "finished",
	}}
	if !slices.EqualFunc(session, want, slices.Equal) {
		t.Errorf("sessions\n got %q\nwant %q", session, want)
	}
}

func roundTo3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

// AC2: every lap written twice leaves the same files, byte for byte, and the same
// rows, though the frames come again in another order and with repeats.
func TestWritingEveryLapTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	sess := race("live", sid)
	sess.write(t, s)
	before, beforeRows := files(t, filepath.Join(dir, "sessions")), indexRows(t, s)

	again := sess
	again.frames = append(slices.Clone(sess.frames), sess.frames[:200]...)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(again.frames), func(i, j int) { //nolint:gosec // a fixed shuffle, not a secret
		again.frames[i], again.frames[j] = again.frames[j], again.frames[i]
	})
	again.write(t, s)

	after, afterRows := files(t, filepath.Join(dir, "sessions")), indexRows(t, s)
	if len(after) != len(sess.laps) {
		t.Errorf("%d files after the repeat, want %d: %v", len(after), len(sess.laps), keys(after))
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("%s changed when the lap was written again", name)
		}
	}
	if !slices.EqualFunc(beforeRows, afterRows, slices.Equal) {
		t.Errorf("rows changed\nbefore %q\n after %q", beforeRows, afterRows)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func indexRows(t *testing.T, s *Store) [][]string {
	t.Helper()
	return append(
		rowsOf(t, t.Context(), s.index, `SELECT * FROM sessions ORDER BY src, id`),
		rowsOf(t, t.Context(), s.index, `SELECT * FROM laps ORDER BY src, session_id, lap`)...)
}

// ADR 0003, point 5: a crash between the file and the row leaves the file and no
// row (or a temporary file beside it), which the next delivery completes.
func TestTheNextDeliveryCompletesALapCutShort(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	sess := race("live", sid)
	sess.write(t, s)
	// The row comes back as it was, but for when it was recorded.
	lapRows := `SELECT * EXCLUDE (recorded_at) FROM laps ORDER BY lap`
	want := rowsOf(t, t.Context(), s.index, lapRows)
	if _, err := s.index.ExecContext(t.Context(), `DELETE FROM laps WHERE lap = 2`); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(lapFile(dir, "live", 2)), ".lap-002.parquet.tmp")
	if err := os.WriteFile(tmp, []byte("half a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(t.Context(), "live", sess.laps[1], sess.frames); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(t.Context(), "live", sess.ended); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, t.Context(), s.index, lapRows); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("rows\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary file is still there: %v", err)
	}
}

// AC3: a second Open on the same directory fails with ErrBusy while the first is
// open, and succeeds once it is closed.
func TestASecondOpenIsBusy(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), dir)
	if !errors.Is(err, ErrBusy) {
		t.Errorf("second Open: %v, want ErrBusy", err)
		if second != nil {
			_ = second.Close()
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

// AC5: a lap file's metadata holds the frame schema version, and its columns are
// v1's frame columns plus seq.
func TestALapFileHasTheFrameSchemaVersionAndV1Columns(t *testing.T) {
	dir := t.TempDir()
	race("live", sid).write(t, open(t, dir))
	path := lapFile(dir, "live", 1)

	meta := query(t, `SELECT decode(value) FROM parquet_kv_metadata(?) WHERE decode(key) = 'frame_schema_version'`, path)
	if want := [][]string{{fmt.Sprint(FrameSchemaVersion)}}; !slices.EqualFunc(meta, want, slices.Equal) {
		t.Errorf("frame_schema_version %q, want %q", meta, want)
	}
	if !strings.Contains(fmt.Sprint(query(t, `SELECT compression FROM parquet_metadata(?)`, path)), "ZSTD") {
		t.Error("the file is not ZSTD-compressed")
	}

	var cols []string
	for _, row := range query(t, `SELECT column_name, column_type FROM (DESCRIBE SELECT * FROM read_parquet(?))`, path) {
		cols = append(cols, row[0]+" "+row[1])
	}
	// v1's FRAME_COLUMNS, from its store.py, and seq.
	v1 := []string{
		"t DOUBLE", "packet_id BIGINT", "lap INTEGER", "phase VARCHAR",
		"pos_x DOUBLE", "pos_y DOUBLE", "pos_z DOUBLE", "vel_x DOUBLE", "vel_y DOUBLE", "vel_z DOUBLE",
		"speed_kph DOUBLE", "engine_rpm DOUBLE", "gear INTEGER", "suggested_gear INTEGER",
		"throttle INTEGER", "brake INTEGER", "fuel_level DOUBLE",
		"tire_fl_temp DOUBLE", "tire_fr_temp DOUBLE", "tire_rl_temp DOUBLE", "tire_rr_temp DOUBLE",
		"tire_fl_radius DOUBLE", "tire_fr_radius DOUBLE", "tire_rl_radius DOUBLE", "tire_rr_radius DOUBLE",
		"sus_fl DOUBLE", "sus_fr DOUBLE", "sus_rl DOUBLE", "sus_rr DOUBLE",
		"wheel_fl_rps DOUBLE", "wheel_fr_rps DOUBLE", "wheel_rl_rps DOUBLE", "wheel_rr_rps DOUBLE",
		"water_temp DOUBLE", "oil_temp DOUBLE", "oil_pressure DOUBLE", "boost_pressure DOUBLE",
		"body_height DOUBLE", "position INTEGER", "time_of_day_ms BIGINT", "car_code INTEGER",
		"track_id INTEGER", "rot_x DOUBLE", "rot_y DOUBLE", "rot_z DOUBLE", "orientation DOUBLE",
		"ang_vel_x DOUBLE", "ang_vel_y DOUBLE", "ang_vel_z DOUBLE",
	}
	if want := append([]string{"seq BIGINT"}, v1...); !slices.Equal(cols, want) {
		t.Errorf("columns\n got %q\nwant %q", cols, want)
	}

	// The values are the frames', and a missing optional is NULL.
	got := query(t, `SELECT seq, lap, phase, packet_id, position, track_id FROM read_parquet(?) ORDER BY seq LIMIT 1`, path)
	if want := [][]string{{"130", "1", "in_race", "5130", "2", "7"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("first frame %q, want %q", got, want)
	}
}

// R5: a lap's file holds the frames in its seq range whose phase is in race, and
// the finished frame that closes the race; nothing before the start, nothing paused
// and nothing after the flag.
func TestOnlyRacingFramesGoInALapFile(t *testing.T) {
	in := func(seq uint32, phase telemetryv1.Phase) *telemetryv1.Frame {
		return &telemetryv1.Frame{Seq: seq, Phase: phase}
	}
	const (
		race     = telemetryv1.Phase_PHASE_IN_RACE
		paused   = telemetryv1.Phase_PHASE_PAUSED
		finished = telemetryv1.Phase_PHASE_FINISHED
		atTrack  = telemetryv1.Phase_PHASE_AT_TRACK
	)
	frames := []*telemetryv1.Frame{
		in(14, finished), in(10, race), in(9, atTrack), in(11, paused), in(12, race),
		in(13, race), in(12, race), in(15, finished), in(16, race), in(8, race),
	}
	var got []uint32
	for _, f := range racingFrames(frames, 9, 15) {
		got = append(got, f.GetSeq())
	}
	if want := []uint32{10, 12, 13, 14}; !slices.Equal(got, want) {
		t.Errorf("racing frames %v, want %v", got, want)
	}
	// A finished frame after a pause is not the one that closes the race.
	got = nil
	for _, f := range racingFrames([]*telemetryv1.Frame{in(1, race), in(2, paused), in(3, finished)}, 0, 10) {
		got = append(got, f.GetSeq())
	}
	if want := []uint32{1}; !slices.Equal(got, want) {
		t.Errorf("racing frames %v, want %v", got, want)
	}
}

// R9: a lap row records the top and minimum speed, the path length and the largest
// single-frame jump, from the lap's frames.
func TestALapRowRecordsFactsFromItsFrames(t *testing.T) {
	frames := []*telemetryv1.Frame{
		{Seq: 1, Phase: telemetryv1.Phase_PHASE_IN_RACE, SpeedKph: 120, PosX: 0, PosZ: 0},
		{Seq: 2, Phase: telemetryv1.Phase_PHASE_IN_RACE, SpeedKph: 180, PosX: 3, PosZ: 4, PosY: 50},
		{Seq: 3, Phase: telemetryv1.Phase_PHASE_IN_RACE, SpeedKph: 90, PosX: 3, PosZ: 16},
	}
	dir := t.TempDir()
	s := open(t, dir)
	sess := race("live", sid)
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	lap := &eventsv1.LapCompleted{SessionId: sid, Lap: 1, FirstSeq: 1, LastSeq: 3}
	if err := s.WriteLap(t.Context(), "live", lap, frames); err != nil {
		t.Fatal(err)
	}
	laps, err := s.Laps(t.Context(), "live")
	if err != nil || len(laps) != 1 {
		t.Fatalf("laps %v, %v", laps, err)
	}
	l := laps[0]
	if *l.TopSpeedKph != 180 || *l.MinSpeedKph != 90 || l.PathM != 17 || l.MaxJumpM != 12 || l.Frames != 3 {
		t.Errorf("top %v, min %v, path %v m, jump %v m, frames %d; want 180, 90, 17, 12, 3",
			*l.TopSpeedKph, *l.MinSpeedKph, l.PathM, l.MaxJumpM, l.Frames)
	}

	// A lap with no racing frames still gets its file and row, with no speeds.
	empty := &eventsv1.LapCompleted{SessionId: sid, Lap: 2, FirstSeq: 50, LastSeq: 60}
	if err := s.WriteLap(t.Context(), "live", empty, frames); err != nil {
		t.Fatal(err)
	}
	if laps, err = s.Laps(t.Context(), "live"); err != nil {
		t.Fatal(err)
	}
	if l := laps[1]; l.Frames != 0 || l.TopSpeedKph != nil || l.MinSpeedKph != nil || l.PathM != 0 {
		t.Errorf("an empty lap's row %+v, want no frames and no speeds", l)
	}
	if got := query(t, `SELECT count(*) FROM read_parquet(?)`, lapFile(dir, "live", 2)); got[0][0] != "0" {
		t.Errorf("the empty lap's file has %s rows", got[0][0])
	}
}

// Replays and parity runs share the index with live sessions, under their own
// source in every row and every path, and readers show live unless asked.
func TestSourcesShareTheIndexAndReadersShowLive(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	race("live", sid).write(t, s)
	race("replay-suzuka", sid).write(t, s)
	noSource := race("", "01JB8Z3K5Q9T2M7XW4R6N1PDHF")
	noSource.write(t, s)

	for src, want := range map[string]int{"": 6, "live": 6, "replay-suzuka": 3, "parity": 0} {
		laps, err := s.Laps(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		if len(laps) != want {
			t.Errorf("Laps(%q): %d laps, want %d", src, len(laps), want)
		}
		for _, l := range laps {
			if want := sourceOf(src); l.Src != want || !strings.HasPrefix(l.File, "sessions/"+want+"/") {
				t.Errorf("Laps(%q) gave %s's lap at %s", src, l.Src, l.File)
			}
		}
	}
	if _, err := os.Stat(lapFile(dir, "replay-suzuka", 1)); err != nil {
		t.Error(err)
	}
}

// A source or session id that cannot name a directory, a lap numbered 0, a lap or
// end for a session never started, and a start with no time are refused, and
// write nothing.
func TestRefusesWhatItCannotStore(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	ctx := t.Context()
	good := race("live", sid)
	for name, err := range map[string]error{
		"a source outside the data directory": s.StartSession(ctx, &eventsv1.SessionStarted{SessionId: sid, Source: "../x", StartedAt: good.started.GetStartedAt()}),
		"a session id that is no ULID":        s.StartSession(ctx, &eventsv1.SessionStarted{SessionId: "../../etc", StartedAt: good.started.GetStartedAt()}),
		"a start with no time":                s.StartSession(ctx, &eventsv1.SessionStarted{SessionId: sid}),
		"a lap of a bad source":               s.WriteLap(ctx, "replay-", good.laps[0], good.frames),
		"lap 0":                               s.WriteLap(ctx, "live", &eventsv1.LapCompleted{SessionId: sid}, good.frames),
		"the end of a bad session id":         s.EndSession(ctx, "live", &eventsv1.SessionEnded{SessionId: "x"}),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := s.WriteLap(ctx, "live", good.laps[0], good.frames); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("a lap of an unknown session: %v, want ErrUnknownSession", err)
	}
	if err := s.EndSession(ctx, "live", good.ended); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("the end of an unknown session: %v, want ErrUnknownSession", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a lap was written: %v", err)
	}
	if got := rowsOf(t, ctx, s.index, `SELECT count(*) FROM sessions`); got[0][0] != "0" {
		t.Errorf("%s sessions stored", got[0][0])
	}
}

// The end of a session keeps its first end time, and with no finishing position
// takes the last lap's.
func TestEndingASessionAgainKeepsItsEnd(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	sess.laps[2].Position = proto.Int32(5)
	sess.ended.Reason = eventsv1.EndReason_END_REASON_QUIT
	sess.write(t, s)
	first := rowsOf(t, t.Context(), s.index, `SELECT ended_at, final_position, end_reason FROM sessions`)
	if err := s.EndSession(t.Context(), "live", sess.ended); err != nil {
		t.Fatal(err)
	}
	again := rowsOf(t, t.Context(), s.index, `SELECT ended_at, final_position, end_reason FROM sessions`)
	if !slices.EqualFunc(first, again, slices.Equal) {
		t.Errorf("ending again changed the row: %q to %q", first, again)
	}
	if first[0][1] != "5" || first[0][2] != "quit" {
		t.Errorf("final position %s, reason %s; want 5 (the last lap's), quit", first[0][1], first[0][2])
	}
}

// R8: every database connection and file is closed, on the paths that succeed and
// on those that fail: the process holds as many file descriptors after as before.
func TestNothingIsLeftOpen(t *testing.T) {
	dir := t.TempDir()
	// DuckDB starts its own threads and caches on first use; warm them up first.
	warm, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	race("live", sid).write(t, warm)
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	before := openFiles(t)

	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := race("live", sid)
	sess.write(t, s)
	_ = s.WriteLap(t.Context(), "live", &eventsv1.LapCompleted{SessionId: "01JB8Z3K5Q9T2M7XW4R6N1PDHX", Lap: 1}, nil)
	if _, err := Open(t.Context(), dir); !errors.Is(err, ErrBusy) {
		t.Errorf("Open while open: %v", err)
	}
	if _, err := s.Laps(t.Context(), "live"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after := openFiles(t); after != before {
		t.Errorf("%d files open after Close, %d before", after, before)
	}
	// Closing twice is harmless.
	if err := s.Close(); err != nil {
		t.Error(err)
	}
}

// openFiles is the number of file descriptors the process holds.
func openFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
