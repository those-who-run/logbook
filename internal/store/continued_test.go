package store

import (
	"fmt"
	"slices"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

const laterRun = "01JB8Z0000RUNBBBBBBBBBBBBB"

// paddock#76: a session continued by a later run keeps that run's frames apart
// from the earlier run's, though their seqs overlap. A lap of the later run is
// written from its own run's frames alone, and recording the run again changes
// nothing.
func TestAContinuedSessionKeepsEachRunsFrames(t *testing.T) {
	ctx := t.Context()
	sess := race("live", sid)
	s := open(t, t.TempDir())
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	stage(t, s, sess, 0, len(sess.frames))
	if _, _, err := s.WriteStagedLap(ctx, "live", run, sess.laps[0]); err != nil {
		t.Fatal(err)
	}
	// The later run counts its seqs from 1, so its lap overlaps the earlier
	// run's frames still staged.
	lap := sess.laps[1]
	from := uint32(150)
	c := Continuation{Run: laterRun, FromSeq: from, FromLap: 2, At: time.Date(2026, 10, 15, 19, 33, 0, 0, time.UTC)}
	for range 2 {
		if err := s.ContinueSession(ctx, "live", sid, c); err != nil {
			t.Fatal(err)
		}
	}
	var later []StagedFrame
	for i := range 40 {
		later = append(later, StagedFrame{Src: "live", SessionID: sid, Run: laterRun,
			Frame: &telemetryv1.Frame{Seq: from + uint32(i), Lap: 2, Phase: telemetryv1.Phase_PHASE_IN_RACE, //nolint:gosec // 40
				SpeedKph: 1, CarCode: 3431}})
	}
	if err := s.StageFrames(ctx, later); err != nil {
		t.Fatal(err)
	}
	gap := &eventsv1.LapCompleted{SessionId: sid, Lap: 2, LapTimeMs: 99_000, FirstSeq: from, LastSeq: from + 39,
		FuelAtStart: lap.GetFuelAtStart(), FuelAtEnd: lap.GetFuelAtEnd()}
	n, written, err := s.WriteStagedLap(ctx, "live", laterRun, gap)
	if err != nil || n != 40 || written != 40 {
		t.Errorf("the later run's lap: %d staged, %d written, %v; want 40 of its own", n, written, err)
	}
	if got := rowsOf(t, ctx, s.index, `SELECT top_speed_kph FROM laps WHERE lap = 2`); got[0][0] != "1" {
		t.Errorf("lap 2's top speed %s: an earlier run's frame was mixed in", got[0][0])
	}
	// The session's run stays the run of its first frame.
	open, err := s.OpenSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Run != run || len(open[0].Continued) != 1 {
		t.Fatalf("open sessions %+v", open)
	}
	got := open[0].Continued[0]
	if want := (Continuation{Run: laterRun, FromSeq: from, FromLap: 2, At: c.At, MaxStaged: int64(from) + 39}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("continued %+v, want %+v", got, want)
	}
	info, err := s.Session(ctx, "live", sid)
	if err != nil || info.Run != run || len(info.Continued) != 1 {
		t.Errorf("Session = %+v, %v", info, err)
	}
	for n, want := range map[int32]string{1: run, 2: laterRun, 3: laterRun} {
		if r := RunOf(info.Run, info.Continued, n); r != want {
			t.Errorf("RunOf lap %d = %s, want %s", n, r, want)
		}
	}
	if err := s.ContinueSession(ctx, "live", "01JB8Z9ZZQ9T2M7XW4R6N1PDHF", c); err == nil {
		t.Error("continued a session never started")
	}
	if err := s.ContinueSession(ctx, "live", sid, Continuation{}); err == nil {
		t.Error("continued a session with no run")
	}
	if got := rowsOf(t, ctx, s.index, `SELECT run, count(*) FROM frames_pending GROUP BY run ORDER BY run`); !slices.EqualFunc(got,
		[][]string{{run, fmt.Sprint(len(sess.frames) - int(sess.laps[0].GetLastSeq()-sess.laps[0].GetFirstSeq()))}, {laterRun, "1"}}, slices.Equal) {
		t.Errorf("staged by run %q", got)
	}
}
