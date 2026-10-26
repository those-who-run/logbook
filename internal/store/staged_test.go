package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
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
		left := rowsOf(t, ctx, s.index, `SELECT count(*) FROM frames_pending WHERE seq <= ?`, int64(lap.GetLastSeq()))
		if left[0][0] != "0" {
			t.Errorf("lap %d left %s frames staged up to its end", lap.GetLap(), left[0][0])
		}
	}
	if err := s.EndSession(ctx, "live", sess.ended, sess.endedAt); err != nil {
		t.Fatal(err)
	}
	if got := staged(t, s); len(got) != 1 {
		t.Errorf("the frames after the last lap: %q", got)
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
	}{{"live", sid, SessionEnded}, {"replay-x", b.started.GetSessionId(), SessionOpen}, {"parity", sid, SessionUnknown}} {
		if got, err := s.Session(ctx, c.src, c.id); err != nil || got != c.want {
			t.Errorf("Session(%s, %s) = %v, %v; want %v", c.src, c.id, got, err, c.want)
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
