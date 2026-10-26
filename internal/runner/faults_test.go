package runner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"

	"github.com/those-who-run/logbook/internal/store"
)

// only keeps the snapshot's rows and files of one session.
func (s snapshot) only(sessionID string) snapshot {
	out := snapshot{files: map[string]string{}, staged: s.staged}
	for _, r := range s.sessions {
		if r[1] == sessionID {
			out.sessions = append(out.sessions, r)
		}
	}
	for _, r := range s.laps {
		if r[1] == sessionID {
			out.laps = append(out.laps, r)
		}
	}
	for name, b := range s.files {
		if strings.Contains(name, "/"+sessionID+"/") {
			out.files[name] = b
		}
	}
	return out
}

// B1: a session from EVENTS' long history, whose frames have aged out of
// TELEMETRY, never takes the frames of a later ecu run of its source, though
// that run's seqs pass its first seq. Its laps are not written; the current
// session's are as if the old one were not there.
func TestAnOldSessionNeverTakesAnotherRunsFrames(t *testing.T) {
	current := newRace("live", runB, sidB)
	clean, _, _ := replay(t, current)

	old := newRace("live", runA, sidA)
	old.carCode, old.firstSeq, old.laps = 1111, 200, 2
	old.start = current.start.Add(-30 * 24 * time.Hour)
	old.endedAt = old.start.Add(10 * time.Minute)
	var oldEvents []published
	for _, p := range old.messages().msgs {
		if _, ok := p.msg.(*telemetryv1.Frame); !ok {
			oldEvents = append(oldEvents, p)
		}
	}

	srv, js := testServer(t)
	dir := t.TempDir()
	publish(t, js, oldEvents, false)
	in := start(t, testConfig(srv.ClientURL(), dir))
	publish(t, js, current.messages().msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidB})
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if d := clean.diff(snap.only(sidB)); d != "" {
		t.Errorf("the current session differs from a run without the old one:\n%s", d)
	}
	if o := snap.only(sidA); len(o.laps) != 0 || len(o.sessions) != 1 || o.sessions[0][14] != "NULL" {
		t.Errorf("the old session has laps %q and run %q, want none", o.laps, o.sessions)
	}
	if got := in.logs.find("lap not written: its session's frames are gone", map[string]any{"session": sidA}); len(got) != old.laps {
		t.Errorf("%d old laps skipped, want %d", len(got), old.laps)
	}

	// A late copy of one of its laps, after its end, is not written either.
	again := start(t, testConfig(srv.ClientURL(), dir))
	var lap1 []published
	for _, p := range old.messages().msgs {
		if l, ok := p.msg.(*eventsv1.LapCompleted); ok && l.GetLap() == 1 {
			lap1 = append(lap1, p)
		}
	}
	publish(t, js, lap1, true)
	again.logs.await(t, 1, "lap not written: its session's frames are gone", map[string]any{"session": sidA})
	if err := again.stop(t); err != nil {
		t.Fatal(err)
	}
}

// Two replays of one recording under one source carry the same first frame,
// seq and ce-time alike. A session whose first frames came before its start
// reads back the later run's, not the earlier replay's.
func TestAReadBackTakesTheLaterOfTwoReplays(t *testing.T) {
	second := newRace("replay-suzuka", runB, sidB)
	clean, _, _ := replay(t, second)

	first := newRace("replay-suzuka", runA, sidA)
	first.carCode = 1111
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	publish(t, js, first.messages().msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	var frames, events []published
	for _, p := range second.messages().msgs {
		if _, ok := p.msg.(*telemetryv1.Frame); ok {
			frames = append(frames, p)
		} else {
			events = append(events, p)
		}
	}
	publish(t, js, frames, false)
	waitStreamRead(t, js, srv)
	publish(t, js, events, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidB})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if d := clean.diff(takeSnapshot(t, dir).only(sidB)); d != "" {
		t.Errorf("the second replay differs from a run of it alone:\n%s", d)
	}
}

// A stop before logbook has started returns nil, as SIGTERM while it starts
// should (R8).
func TestAStopWhileStartingIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Run(ctx, testConfig("nats://127.0.0.1:1", t.TempDir()), slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Errorf("Run with a stopped context = %v, want nil", err)
	}
}

// B2: a lap.completed that comes after its session's session.ended, as both do
// when they come again after a stop, each at the end of its own ack wait, is
// still written: the end waits for the laps its summary counts.
func TestALapAfterItsSessionsEndIsWritten(t *testing.T) {
	r := newRace("live", runA, sidA)
	clean, msgs, _ := replay(t, r)

	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	var lap2 published
	var rest []published
	for _, p := range msgs.msgs {
		if l, ok := p.msg.(*eventsv1.LapCompleted); ok && l.GetLap() == 2 {
			lap2 = p
			continue
		}
		rest = append(rest, p)
	}
	publish(t, js, rest, false)
	in.logs.await(t, 1, "lap written", map[string]any{"lap": 3})
	publish(t, js, []published{lap2}, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if d := clean.diff(takeSnapshot(t, dir)); d != "" {
		t.Errorf("differs from a run in order:\n%s", d)
	}
}

// A lap.completed that comes after its session's end has been applied still
// writes its row, from what is staged, and leaves the session's summary as the
// end made it.
func TestALapAfterTheEndIsAppliedWritesItsRow(t *testing.T) {
	r := newRace("live", runA, sidA)
	msgs := r.messages()
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	cfg.FramesTimeout = 500 * time.Millisecond
	in := start(t, cfg)
	var lap2 published
	var rest []published
	for _, p := range msgs.msgs {
		if l, ok := p.msg.(*eventsv1.LapCompleted); ok && l.GetLap() == 2 {
			lap2 = p
			continue
		}
		rest = append(rest, p)
	}
	publish(t, js, rest, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	publish(t, js, []published{lap2}, false)
	in.logs.await(t, 1, "lap written", map[string]any{"lap": 2})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if len(snap.laps) != 3 || snap.sessions[0][8] != "2" || snap.sessions[0][9] != "98100" {
		t.Errorf("laps %q, session %q", snap.laps, snap.sessions)
	}
}

// N4: nothing is acked before its commit. A batch of frames whose staging fails,
// and a lap whose write fails, come again to the next start, which then leaves
// the same store as a run with no failure.
func TestAFailedWriteIsNotAcked(t *testing.T) {
	r := newRace("live", runA, sidA)
	clean, _, _ := replay(t, r)
	for name, when := range map[string]func(string, any) bool{
		// Not the first batch: the session has its run by then, so the next
		// start has no read-back to find the frames again.
		"staging a batch of frames in lap 3": func(method string, arg any) bool {
			frames, _ := arg.([]store.StagedFrame)
			return method == "StageFrames" && slices.ContainsFunc(frames, func(f store.StagedFrame) bool {
				return f.Frame.GetLap() == 3
			})
		},
		"writing lap 2": func(method string, arg any) bool {
			lap, _ := arg.(*eventsv1.LapCompleted)
			return method == "WriteStagedLap" && lap.GetLap() == 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, js := testServer(t)
			dir := t.TempDir()
			cfg := testConfig(srv.ClientURL(), dir)
			cfg.wrapStore = failingOnce(when)
			first := start(t, cfg)
			publish(t, js, r.messages().msgs, false)
			if err := first.wait(t); !errors.Is(err, errInjected) {
				t.Fatalf("the first run returned %v, want the injected failure", err)
			}
			second := start(t, testConfig(srv.ClientURL(), dir))
			second.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
			if err := second.stop(t); err != nil {
				t.Fatal(err)
			}
			if d := clean.diff(takeSnapshot(t, dir)); d != "" {
				t.Errorf("differs from a run with no failure:\n%s", d)
			}
		})
	}
}

// N5: a message the store refuses on every delivery is dropped, with an error,
// after giveUpAfter deliveries, rather than stopping logbook at every start.
func TestAMessageTheStoreAlwaysRefusesIsDropped(t *testing.T) {
	r := newRace("live", runA, sidA)
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	cfg.wrapStore = func(s storeAPI) storeAPI {
		return &faultStore{storeAPI: s, fail: func(method string, arg any) bool {
			lap, _ := arg.(*eventsv1.LapCompleted)
			return method == "WriteStagedLap" && lap.GetLap() == 2
		}}
	}
	publish(t, js, r.messages().msgs, false)
	for range giveUpAfter - 1 {
		in := start(t, cfg)
		if err := in.wait(t); !errors.Is(err, errInjected) {
			t.Fatalf("Run returned %v, want the injected failure", err)
		}
	}
	in := start(t, cfg)
	in.logs.await(t, 1, "the store refused a message on every delivery; dropped", nil)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if snap := takeSnapshot(t, dir); len(snap.laps) != r.laps-1 {
		t.Errorf("%d laps, want all but lap 2", len(snap.laps))
	}
}
