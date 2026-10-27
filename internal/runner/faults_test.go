package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
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
	// Its frames were dropped with the session's at the end; they are read back.
	if len(snap.laps) == 3 && snap.laps[1][11] != strconv.Itoa(r.lapFrames) {
		t.Errorf("the late lap 2 holds %s frames, want %d", snap.laps[1][11], r.lapFrames)
	}
	if snap.staged != 0 {
		t.Errorf("%d frames left staged", snap.staged)
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

// B-1: a session whose first frame never reached TELEMETRY (ecu drops frames
// while its connection is down) takes its run from the earliest frame that fits
// its start, and loses that one frame only.
func TestASessionWithoutItsFirstFrameKeepsItsLaps(t *testing.T) {
	r := newRace("live", runA, sidA)
	clean, msgs, _ := replay(t, r)
	first := msgs.started.GetFirstSeq()
	var sent []published
	for _, p := range msgs.msgs {
		if f, ok := p.msg.(*telemetryv1.Frame); ok && f.GetSeq() == first {
			continue
		}
		sent = append(sent, p)
	}
	for name, order := range map[string]func(t *testing.T, js jetstream.JetStream, srv *natsServer){
		"events with the frames": func(t *testing.T, js jetstream.JetStream, _ *natsServer) {
			publish(t, js, sent, false)
		},
		"frames before events, read back": func(t *testing.T, js jetstream.JetStream, srv *natsServer) {
			var frames, events []published
			for _, p := range sent {
				if _, ok := p.msg.(*telemetryv1.Frame); ok {
					frames = append(frames, p)
				} else {
					events = append(events, p)
				}
			}
			publish(t, js, frames, false)
			waitStreamRead(t, js, srv)
			publish(t, js, events, false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, js := testServer(t)
			dir := t.TempDir()
			in := start(t, testConfig(srv.ClientURL(), dir))
			order(t, js, srv)
			in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
			if err := in.stop(t); err != nil {
				t.Fatal(err)
			}
			snap := takeSnapshot(t, dir)
			if len(snap.laps) != r.laps {
				t.Fatalf("%d laps written, want %d", len(snap.laps), r.laps)
			}
			if got := snap.laps[0][11]; got != strconv.Itoa(r.lapFrames-1) {
				t.Errorf("lap 1 holds %s frames, want all but the first, %d", got, r.lapFrames-1)
			}
			if !slices.Equal(snap.laps[1], clean.laps[1]) || !slices.Equal(snap.laps[2], clean.laps[2]) {
				t.Errorf("laps 2 and 3 %q differ from a clean run's %q", snap.laps[1:], clean.laps[1:])
			}
			if snap.sessions[0][14] != runA {
				t.Errorf("the session's run is %q", snap.sessions[0][14])
			}
		})
	}
}

// N-B: a session's end waits for a missing lap for an ack wait and a frames
// timeout from its own arrival, though frames go on coming after the race.
func TestAnEndWaitsNoLongerWhileFramesFlow(t *testing.T) {
	r := newRace("live", runA, sidA)
	msgs := r.messages()
	var sent []published
	lastSeq := uint32(0)
	for _, p := range msgs.msgs {
		if l, ok := p.msg.(*eventsv1.LapCompleted); ok && l.GetLap() == 2 {
			continue
		}
		if f, ok := p.msg.(*telemetryv1.Frame); ok {
			lastSeq = f.GetSeq()
		}
		sent = append(sent, p)
	}
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	in := start(t, cfg)
	publish(t, js, sent, false)
	// The menus after the race: a frame every 100 ms for up to 30 s.
	began := time.Now()
	deadline := began.Add(30 * time.Second)
	at := r.endedAt
	for seq := lastSeq + 1; time.Now().Before(deadline); seq++ {
		if len(in.logs.find("session ended", map[string]any{"session": sidA})) > 0 {
			if waited := time.Since(began); waited > cfg.ackWait+cfg.FramesTimeout+5*time.Second {
				t.Errorf("the end waited %v while frames flowed, want about %v", waited, cfg.ackWait+cfg.FramesTimeout)
			}
			if err := in.stop(t); err != nil {
				t.Fatal(err)
			}
			return
		}
		at = at.Add(100 * time.Millisecond)
		publish(t, js, []published{{subject: "twr.v1.live.frame.gt7", id: fmt.Sprintf("%s:frame:%d", runA, seq), at: at,
			msg: &telemetryv1.Frame{Seq: seq, Phase: telemetryv1.Phase_PHASE_FINISHED}}}, false)
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the session had not ended after 30 s of frames")
}

// N-E, B3: after a stop no new write starts. Three laps are ready in one round;
// a stop during the first leaves the other two, and the end, for the next start.
func TestAStopStartsNoNewWrite(t *testing.T) {
	r := newRace("live", runA, sidA)
	r.lapFrames = 100 // every lap in one batch of frames, so all are ready at once
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	var running atomic.Pointer[instance]
	cfg.beforeLapWrite = func(_, _ string, lap int32) error {
		if lap != 1 {
			return nil
		}
		for running.Load() == nil {
			time.Sleep(time.Millisecond) // until start has returned
		}
		running.Load().cancel()
		return nil
	}
	// All of it before logbook starts, so the first round finds every lap ready.
	publish(t, js, r.messages().msgs, false)
	in := start(t, cfg)
	running.Store(in)
	if err := in.wait(t); err != nil {
		t.Fatal(err)
	}
	if got := len(in.logs.find("lap written", nil)); got != 1 {
		t.Errorf("%d laps written after the stop came during lap 1, want only lap 1", got)
	}
	if got := in.logs.find("session ended", nil); len(got) != 0 {
		t.Error("the session was ended after the stop")
	}
	cfg.beforeLapWrite = nil
	again := start(t, cfg)
	again.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := again.stop(t); err != nil {
		t.Fatal(err)
	}
	if snap := takeSnapshot(t, dir); len(snap.laps) != r.laps {
		t.Errorf("%d laps, want %d", len(snap.laps), r.laps)
	}
}

// onStart is the store with a callback before each session's start.
type onStart struct {
	storeAPI
	before func()
}

func (o *onStart) StartSession(ctx context.Context, ev *eventsv1.SessionStarted) error {
	o.before()
	return o.storeAPI.StartSession(ctx, ev)
}

// A stop while logbook works through waiting events finishes the event in hand
// and hands the rest back, for the next start to handle.
func TestAStopPartWayThroughTheEventsHandsTheRestBack(t *testing.T) {
	a, b := newRace("replay-one", runA, sidA), newRace("replay-two", runB, sidB)
	srv, js := testServer(t)
	publish(t, js, slices.Concat(a.messages().msgs, b.messages().msgs), false)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	var running atomic.Pointer[instance]
	cfg.wrapStore = func(s storeAPI) storeAPI {
		return &onStart{storeAPI: s, before: func() {
			for running.Load() == nil {
				time.Sleep(time.Millisecond) // until start has returned
			}
			running.Load().cancel()
		}}
	}
	in := start(t, cfg)
	running.Store(in)
	if err := in.wait(t); err != nil {
		t.Fatal(err)
	}
	if got := len(in.logs.find("session started", nil)); got != 1 {
		t.Errorf("%d sessions started after the stop came during the first, want 1", got)
	}
	again := start(t, testConfig(srv.ClientURL(), dir))
	again.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	again.logs.await(t, 1, "session ended", map[string]any{"session": sidB})
	if err := again.stop(t); err != nil {
		t.Fatal(err)
	}
	if snap := takeSnapshot(t, dir); len(snap.laps) != a.laps+b.laps {
		t.Errorf("%d laps, want %d", len(snap.laps), a.laps+b.laps)
	}
}
