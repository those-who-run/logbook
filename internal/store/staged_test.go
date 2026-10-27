package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

const run = "01JB8Z0000RUNAAAAAAAAAAAAA"

// stage stages frames of the race's session, all of one run.
func stage(t *testing.T, s *Store, sess session, from, to int) {
	t.Helper()
	var staged []StagedFrame
	for _, f := range sess.frames[from:to] {
		staged = append(staged, StagedFrame{Src: sess.started.GetSource(), SessionID: sess.started.GetSessionId(), Run: run, Frame: f})
	}
	if err := s.StageFrames(t.Context(), staged); err != nil {
		t.Fatal(err)
	}
}

func staged(t *testing.T, s *Store) [][]string {
	t.Helper()
	return rowsOf(t, t.Context(), s.index, `SELECT src, session_id, count(*), min(seq), max(seq), min(run)
FROM frames_pending GROUP BY ALL ORDER BY ALL`)
}

// Laps written from staged frames equal laps written from the frames themselves,
// and each write takes its frames out of frames_pending.
func TestALapFromStagedFramesEqualsOneFromItsFrames(t *testing.T) {
	sess := race("live", sid)
	direct := open(t, t.TempDir())
	sess.write(t, direct)

	dir := t.TempDir()
	s := open(t, dir)
	ctx := t.Context()
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	// In two batches, the second overlapping the first, as a redelivery would.
	stage(t, s, sess, 0, 700)
	stage(t, s, sess, 650, len(sess.frames))
	if got := staged(t, s); len(got) != 1 || got[0][2] != fmt.Sprint(len(sess.frames)) || got[0][5] != run {
		t.Fatalf("staged %q, want every frame once", got)
	}
	for _, lap := range sess.laps {
		n, written, err := s.WriteStagedLap(ctx, "live", lap)
		if err != nil {
			t.Fatal(err)
		}
		if want := int(lap.GetLastSeq() - lap.GetFirstSeq() + 1); n != want || written != want {
			t.Errorf("lap %d: %d staged and %d written, want %d", lap.GetLap(), n, written, want)
		}
		left := rowsOf(t, ctx, s.index, `SELECT count(*) FROM frames_pending WHERE seq >= ? AND seq < ?`,
			int64(lap.GetFirstSeq()), int64(lap.GetLastSeq()))
		if left[0][0] != "0" {
			t.Errorf("lap %d left %s of its frames staged before its last", lap.GetLap(), left[0][0])
		}
	}
	if err := s.EndSession(ctx, "live", sess.ended, sess.endedAt); err != nil {
		t.Fatal(err)
	}
	// Left for the end: the frames before lap 1, each lap's last and those after.
	if got := staged(t, s); len(got) != 1 || got[0][2] != fmt.Sprint(30+len(sess.laps)+10) {
		t.Errorf("staged at the end: %q", got)
	}
	if got, err := s.LapsWritten(ctx, "live", sid); err != nil || !slices.Equal(got, []int32{1, 2, 3}) {
		t.Errorf("LapsWritten = %v, %v", got, err)
	}
	if err := s.DropStaged(ctx, "live", sid); err != nil {
		t.Fatal(err)
	}
	if got := staged(t, s); len(got) != 0 {
		t.Errorf("staged after the drop: %q", got)
	}
	want, err := direct.Laps(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Laps(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d laps, want %d", len(got), len(want))
	}
	for i := range got {
		// Each store's clock started alike, so even recorded_at matches.
		g, _ := json.Marshal(got[i])
		w, _ := json.Marshal(want[i])
		if string(g) != string(w) {
			t.Errorf("lap %d: %s,\nwant %s", got[i].Lap, g, w)
		}
	}
	if a, b := files(t, dir)["sessions/live/"+sid+"/lap-001.parquet"], files(t, direct.dir)["sessions/live/"+sid+"/lap-001.parquet"]; a != b || a == "" {
		t.Error("lap 1's file differs from one written from the frames")
	}
}

// A lap whose frames never all arrived is written with those there are, and says
// how many those were.
func TestALapWithFramesMissing(t *testing.T) {
	sess := race("live", sid)
	s := open(t, t.TempDir())
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	stage(t, s, sess, 0, 100)
	lap := sess.laps[0]
	n, written, err := s.WriteStagedLap(t.Context(), "live", lap)
	if err != nil {
		t.Fatal(err)
	}
	if n != 70 || written != 70 { // 30 frames before the start, then 70 of the lap
		t.Errorf("%d staged and %d written, want 70", n, written)
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT frames FROM laps`)
	if !slices.EqualFunc(got, [][]string{{"70"}}, slices.Equal) {
		t.Errorf("the row records %q frames, want 70", got)
	}
}

// A session takes the run of its first staged frame and keeps it; the open
// sessions come back with their runs and highest staged seq, and an ended one
// does not.
func TestOpenSessionsAndTheirRuns(t *testing.T) {
	ctx := t.Context()
	s := open(t, t.TempDir())
	if n, err := s.Sessions(ctx); err != nil || n != 0 {
		t.Errorf("Sessions = %d, %v on a new store", n, err)
	}
	a, b := race("live", sid), race("replay-x", "01JB8Z3K5Q9T2M7XW4R6N1PDHF")
	for _, sess := range []session{a, b} {
		if err := s.StartSession(ctx, sess.started); err != nil {
			t.Fatal(err)
		}
	}
	stage(t, s, a, 0, 50)
	other := []StagedFrame{{Src: "live", SessionID: sid, Run: "another", Frame: a.frames[60]}}
	if err := s.StageFrames(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.StageFrames(ctx, nil); err != nil {
		t.Errorf("staging nothing: %v", err)
	}
	open, err := s.OpenSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(open[0].Src, open[0].ID, open[0].Run, open[0].MaxStaged, open[0].FirstSeq, " ",
		open[1].Src, open[1].Run, open[1].MaxStaged)
	want := fmt.Sprint("live", sid, run, a.frames[60].GetSeq(), a.started.GetFirstSeq(), " ", "replay-x", "", -1)
	if len(open) != 2 || got != want {
		t.Errorf("open sessions %v, want %s", open, want)
	}
	if err := s.EndSession(ctx, "live", a.ended, a.endedAt); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenSessions(ctx); err != nil || len(open) != 1 || open[0].Src != "replay-x" {
		t.Errorf("after the end: %v, %v", open, err)
	}
	for _, c := range []struct {
		src, id string
		want    SessionState
		run     string
	}{{"live", sid, SessionEnded, run}, {"replay-x", b.started.GetSessionId(), SessionOpen, ""}, {"parity", sid, SessionUnknown, ""}} {
		got, err := s.Session(ctx, c.src, c.id)
		if err != nil || got.State != c.want || got.Run != c.run {
			t.Errorf("Session(%s, %s) = %+v, %v; want %v, %q", c.src, c.id, got, err, c.want, c.run)
		}
		if c.want != SessionUnknown && !got.StartedAt.Equal(a.started.GetStartedAt().AsTime()) {
			t.Errorf("Session(%s, %s) started at %v", c.src, c.id, got.StartedAt)
		}
	}
	if n, err := s.Sessions(ctx); err != nil || n != 2 {
		t.Errorf("Sessions = %d, %v", n, err)
	}
	if has, err := s.HasLap(ctx, "live", sid, 1); err != nil || has {
		t.Errorf("HasLap before the lap = %v, %v", has, err)
	}
}

// The staging methods refuse what cannot be stored, and report a closed store.
func TestStagingRefusesWhatItCannotStore(t *testing.T) {
	ctx := t.Context()
	sess := race("live", sid)
	s := open(t, t.TempDir())
	if err := s.StageFrames(ctx, []StagedFrame{{Src: "../x", SessionID: sid, Frame: sess.frames[0]}}); err == nil {
		t.Error("staged a frame under ../x")
	}
	if _, _, err := s.WriteStagedLap(ctx, "live", sess.laps[0]); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("a lap of no session: %v", err)
	}
	if _, _, err := s.WriteStagedLap(ctx, "nope", sess.laps[0]); err == nil {
		t.Error("wrote a lap under source nope")
	}
	bad := race("live", sid).laps[0]
	bad.Lap = 0
	if _, _, err := s.WriteStagedLap(ctx, "live", bad); err == nil {
		t.Error("wrote lap 0")
	}
	if err := s.DropStaged(ctx, "live", "x"); err == nil {
		t.Error("dropped the frames of session x")
	}
	if err := CheckKey("", sid); err != nil {
		t.Errorf("CheckKey of an empty source, which is live: %v", err)
	}

	closed, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	// The index closed under the store, as a failed disk would leave it.
	if err := closed.index.Close(); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"StageFrames": closed.StageFrames(ctx, []StagedFrame{{Src: "live", SessionID: sid, Frame: sess.frames[0]}}),
		"DropStaged":  closed.DropStaged(ctx, "live", sid),
		"OpenSessions": func() error {
			_, err := closed.OpenSessions(ctx)
			return err
		}(),
		"Session": func() error {
			_, err := closed.Session(ctx, "live", sid)
			return err
		}(),
		"LapsWritten": func() error {
			_, err := closed.LapsWritten(ctx, "live", sid)
			return err
		}(),
		"HasLap": func() error {
			_, err := closed.HasLap(ctx, "live", sid, 1)
			return err
		}(),
		"Sessions": func() error {
			_, err := closed.Sessions(ctx)
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s on a closed index succeeded", name)
		}
	}
	// The index is closed already, so Close reports it; the rest still closes.
	_ = closed.Close()
}

// N3: a lap that starts on the finished frame closing a race counts it, though
// the lap before was written first: that lap's write leaves its own last frame
// staged for the next lap to read.
func TestTheNextLapStillSeesThePreviousLapsLastFrame(t *testing.T) {
	ctx := t.Context()
	sess := race("live", sid)
	inRace := func(seq uint32) *telemetryv1.Frame {
		return &telemetryv1.Frame{Seq: seq, Lap: 1, Phase: telemetryv1.Phase_PHASE_IN_RACE}
	}
	frames := []*telemetryv1.Frame{inRace(130), inRace(131),
		{Seq: 132, Lap: 2, Phase: telemetryv1.Phase_PHASE_FINISHED}}
	lap1 := &eventsv1.LapCompleted{SessionId: sid, Lap: 1, Complete: true, FirstSeq: 130, LastSeq: 131}
	lap2 := &eventsv1.LapCompleted{SessionId: sid, Lap: 2, FirstSeq: 132, LastSeq: 132}

	direct := open(t, t.TempDir())
	if err := direct.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*eventsv1.LapCompleted{lap1, lap2} {
		if err := direct.WriteLap(ctx, "live", l, frames); err != nil {
			t.Fatal(err)
		}
	}
	s := open(t, t.TempDir())
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	var st []StagedFrame
	for _, f := range frames {
		st = append(st, StagedFrame{Src: "live", SessionID: sid, Run: run, Frame: f})
	}
	if err := s.StageFrames(ctx, st); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*eventsv1.LapCompleted{lap1, lap2} {
		if _, _, err := s.WriteStagedLap(ctx, "live", l); err != nil {
			t.Fatal(err)
		}
	}
	want := rowsOf(t, ctx, direct.index, `SELECT lap, frames FROM laps ORDER BY lap`)
	got := rowsOf(t, ctx, s.index, `SELECT lap, frames FROM laps ORDER BY lap`)
	if !slices.EqualFunc(got, want, slices.Equal) || got[1][1] != "1" {
		t.Errorf("staged laps hold %q frames, from the frames %q", got, want)
	}
}

// A lap written before an earlier one, as when the earlier lap's lap.completed
// comes late, leaves the earlier lap's frames staged for it.
func TestALaterLapLeavesAnEarlierLapsFrames(t *testing.T) {
	ctx := t.Context()
	sess := race("live", sid)
	s := open(t, t.TempDir())
	if err := s.StartSession(ctx, sess.started); err != nil {
		t.Fatal(err)
	}
	stage(t, s, sess, 0, len(sess.frames))
	if _, _, err := s.WriteStagedLap(ctx, "live", sess.laps[2]); err != nil {
		t.Fatal(err)
	}
	n, written, err := s.WriteStagedLap(ctx, "live", sess.laps[1])
	if want := int(sess.laps[1].GetLastSeq() - sess.laps[1].GetFirstSeq() + 1); err != nil || n != want || written != want {
		t.Errorf("lap 2 after lap 3: %d staged, %d written, %v; want %d", n, written, err, want)
	}
}
