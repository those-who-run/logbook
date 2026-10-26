package runner

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// Session ids are ULIDs, and runs too.
const (
	sidA = "01JB8Z3K5Q9T2M7XW4R6N1PDHE"
	sidB = "01JB8Z9ZZQ9T2M7XW4R6N1PDHF"
	runA = "01JB8Z0000RUNAAAAAAAAAAAAA"
	runB = "01JB8Z0000RUNBBBBBBBBBBBBB"
)

// replay runs logbook against a server holding one race, published while it runs,
// until the session has ended, and returns the data directory's snapshot.
func replay(t *testing.T, r race) (snapshot, raceMessages, *instance) {
	t.Helper()
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	msgs := r.messages()
	publish(t, js, msgs.msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": r.sid})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	return takeSnapshot(t, dir), msgs, in
}

// AC1: a replay leaves one session and its laps in the store, equal to the
// replay's events, and nothing staged.
func TestAReplayLeavesItsSessionAndLaps(t *testing.T) {
	r := newRace("replay-suzuka", runA, sidA)
	snap, msgs, in := replay(t, r)

	if len(snap.sessions) != 1 {
		t.Fatalf("sessions %q, want one", snap.sessions)
	}
	s := snap.sessions[0]
	// The index keeps microseconds.
	started := msgs.started.GetStartedAt().AsTime().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	want := []string{"replay-suzuka", sidA, started, r.endedAt.Format(time.RFC3339Nano), "race", "3431", "3", strconv.Itoa(int(msgs.started.GetFirstSeq())),
		"2", "98100", s[10], s[11], "2", "finished", runA}
	if !slices.Equal(s, want) {
		t.Errorf("session %q,\nwant %q", s, want)
	}
	// started_at is the first frame's time and ended_at the end event's: a
	// replay that stamped one with today would end before it began, or span days.
	startedAt, _ := time.Parse(time.RFC3339Nano, s[2])
	endedAt, _ := time.Parse(time.RFC3339Nano, s[3])
	if endedAt.Before(startedAt) || endedAt.Sub(startedAt) > time.Hour {
		t.Errorf("the session runs from %v to %v", startedAt, endedAt)
	}

	if len(snap.laps) != len(msgs.laps) {
		t.Fatalf("%d laps, want %d: %q", len(snap.laps), len(msgs.laps), snap.laps)
	}
	for i, ev := range msgs.laps {
		got := snap.laps[i]
		lapTime, position := "NULL", "NULL"
		if ev.GetLapTimeMs() > 0 {
			lapTime = strconv.Itoa(int(ev.GetLapTimeMs()))
		}
		if ev.Position != nil {
			position = strconv.Itoa(int(ev.GetPosition()))
		}
		want := []string{"replay-suzuka", sidA, strconv.Itoa(int(ev.GetLap())), lapTime, strconv.FormatBool(ev.GetComplete()),
			position, strconv.Itoa(int(ev.GetFirstSeq())), strconv.Itoa(int(ev.GetLastSeq()))}
		if g := []string{got[0], got[1], got[2], got[3], got[4], got[8], got[9], got[10]}; !slices.Equal(g, want) {
			t.Errorf("lap %d: %q, want %q", ev.GetLap(), g, want)
		}
		// Every in-race frame of the lap, and the finished one that closes the race.
		frames := int(ev.GetLastSeq() - ev.GetFirstSeq() + 1)
		if got[11] != strconv.Itoa(frames) {
			t.Errorf("lap %d holds %s frames, want %d", ev.GetLap(), got[11], frames)
		}
	}
	if snap.staged != 0 {
		t.Errorf("%d frames still staged after the session ended", snap.staged)
	}
	if n := len(in.logs.find("lap written", nil)); n != len(msgs.laps) {
		t.Errorf("%d laps written, want %d", n, len(msgs.laps))
	}
	if w := in.logs.find("lap written without all its frames", nil); len(w) > 0 {
		t.Errorf("warned of missing frames: %v", w)
	}
}

// AC2: stopping logbook after lap 2's lap.completed arrives and before it is
// written, then starting it again, gives the same store as a run with no stop: no
// lap missing, none twice.
func TestAStopBeforeALapIsWrittenLosesNothing(t *testing.T) {
	r := newRace("replay-suzuka", runA, sidA)
	clean, _, _ := replay(t, r)

	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	crash := errors.New("killed before lap 2 was written")
	cfg.beforeLapWrite = func(_, _ string, lap int32) error {
		if lap == 2 {
			return crash
		}
		return nil
	}
	first := start(t, cfg)
	publish(t, js, r.messages().msgs, false)
	if err := first.wait(t); !errors.Is(err, crash) {
		t.Fatalf("the first run returned %v, want the crash", err)
	}
	if n := len(first.logs.find("lap written", nil)); n != 1 {
		t.Fatalf("%d laps written before the crash, want 1", n)
	}

	cfg.beforeLapWrite = nil
	second := start(t, cfg)
	second.logs.await(t, 1, "store open", map[string]any{"sessions": 1})
	second.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := second.stop(t); err != nil {
		t.Fatal(err)
	}
	if d := clean.diff(takeSnapshot(t, dir)); d != "" {
		t.Errorf("the store after a crash differs from a clean run's:\n%s", d)
	}
	if got := second.logs.find("lap written", map[string]any{"lap": 1}); len(got) != 0 {
		t.Errorf("lap 1 was written again after the restart")
	}
}

// A stop after a lap's commit and before its ack delivers the lap.completed again,
// which changes nothing; so does every message delivered twice (R6).
func TestRedeliveriesAndDuplicatesChangeNothing(t *testing.T) {
	r := newRace("replay-suzuka", runA, sidA)
	clean, _, _ := replay(t, r)

	t.Run("a stop between commit and ack", func(t *testing.T) {
		srv, js := testServer(t)
		dir := t.TempDir()
		cfg := testConfig(srv.ClientURL(), dir)
		crash := errors.New("killed after lap 2 was committed")
		cfg.afterLapWrite = func(_, _ string, lap int32) error {
			if lap == 2 {
				return crash
			}
			return nil
		}
		first := start(t, cfg)
		publish(t, js, r.messages().msgs, false)
		if err := first.wait(t); !errors.Is(err, crash) {
			t.Fatalf("the first run returned %v, want the crash", err)
		}
		cfg.afterLapWrite = nil
		second := start(t, cfg)
		second.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
		if err := second.stop(t); err != nil {
			t.Fatal(err)
		}
		if got := second.logs.find("lap written", map[string]any{"lap": 2}); len(got) != 0 {
			t.Errorf("lap 2 was written again after the restart")
		}
		if d := clean.diff(takeSnapshot(t, dir)); d != "" {
			t.Errorf("differs from a clean run:\n%s", d)
		}
	})

	t.Run("every message twice", func(t *testing.T) {
		srv, js := testServer(t)
		dir := t.TempDir()
		in := start(t, testConfig(srv.ClientURL(), dir))
		msgs := r.messages().msgs
		// Without Nats-Msg-Id, as after the duplicate window, so the server keeps
		// both copies and logbook reads each twice.
		publish(t, js, slices.Concat(msgs, msgs), true)
		in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
		// The second copy of the whole race comes after the end.
		waitStreamRead(t, js, srv)
		if err := in.stop(t); err != nil {
			t.Fatal(err)
		}
		if d := clean.diff(takeSnapshot(t, dir)); d != "" {
			t.Errorf("differs from a clean run:\n%s", d)
		}
	})
}

// waitStreamRead waits until both consumers have acked every message their streams
// hold.
func waitStreamRead(t *testing.T, js jetstream.JetStream, _ *server.Server) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, c := range [][2]string{{framesStream, FramesConsumer}, {eventsStream, EventsConsumer}} {
			cons, err := js.Consumer(t.Context(), c[0], c[1])
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				done = false // logbook has not bound it yet
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := cons.Info(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if info.NumPending > 0 || info.NumAckPending > 0 {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the consumers had not read their streams within 60 s")
}

// AC3: two replays under different sources at once give two sessions, each with
// only its own frames, though their runs number frames alike.
func TestTwoSourcesAtOnceNeverMix(t *testing.T) {
	a := newRace("replay-one", runA, sidA)
	b := newRace("replay-two", runB, sidB)
	b.carCode, b.laps = 1234, 4
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	ma, mb := a.messages(), b.messages()
	// Interleaved, as two replays publishing at once would be.
	var both []published
	for i := range max(len(ma.msgs), len(mb.msgs)) {
		if i < len(ma.msgs) {
			both = append(both, ma.msgs[i])
		}
		if i < len(mb.msgs) {
			both = append(both, mb.msgs[i])
		}
	}
	publish(t, js, both, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidB})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if len(snap.sessions) != 2 || snap.sessions[0][0] != "replay-one" || snap.sessions[1][0] != "replay-two" {
		t.Fatalf("sessions %q, want one under each source", snap.sessions)
	}
	if len(snap.laps) != a.laps+b.laps {
		t.Fatalf("%d laps, want %d", len(snap.laps), a.laps+b.laps)
	}
	for _, lap := range snap.laps {
		car := "3431"
		if lap[0] == "replay-two" {
			car = "1234"
		}
		got := query(t, fmt.Sprintf(`SELECT DISTINCT car_code FROM '%s'`, filepath.Join(dir, lap[16])))
		if len(got) != 1 || got[0][0] != car {
			t.Errorf("%s lap %s holds frames of cars %q, want only %s", lap[0], lap[2], got, car)
		}
		want := []string{lap[11]}
		if n := query(t, fmt.Sprintf(`SELECT count(*) FROM '%s'`, filepath.Join(dir, lap[16]))); !slices.Equal(n[0], want) {
			t.Errorf("%s lap %s: the file holds %q frames, the row says %q", lap[0], lap[2], n[0], want)
		}
	}
}

// AC4: a lap.completed whose frames never arrive is written after the timeout with
// the frames there are, and the warning names the source, session, lap and how
// many were missing.
func TestALapWhoseFramesNeverArriveIsWrittenAfterTheTimeout(t *testing.T) {
	r := newRace("live", runA, sidA)
	msgs := r.messages()
	// Everything up to lap 1's lap.completed, less the last 40 frames of lap 1.
	lap1 := msgs.laps[0]
	var sent []published
	for _, p := range msgs.msgs {
		if f, ok := p.msg.(*telemetryv1.Frame); ok && f.GetSeq() > lap1.GetLastSeq()-40 {
			continue
		}
		sent = append(sent, p)
		if l, ok := p.msg.(*eventsv1.LapCompleted); ok && l.GetLap() == 1 {
			break
		}
	}
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	cfg.FramesTimeout = time.Second
	in := start(t, cfg)
	began := time.Now()
	publish(t, js, sent, false)
	warn := in.logs.await(t, 1, "lap written without all its frames", nil)[0]
	if waited := time.Since(began); waited < cfg.FramesTimeout {
		t.Errorf("written after %v, before the %v timeout", waited, cfg.FramesTimeout)
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if warn["level"] != "WARN" || warn["src"] != "live" || warn["session"] != sidA ||
		warn["lap"] != float64(1) || warn["missing"] != float64(40) || warn["timed_out"] != true {
		t.Errorf("warning %v", warn)
	}
	snap := takeSnapshot(t, dir)
	present := int(lap1.GetLastSeq()-lap1.GetFirstSeq()+1) - 40
	if len(snap.laps) != 1 || snap.laps[0][11] != strconv.Itoa(present) {
		t.Errorf("laps %q, want lap 1 with %d frames", snap.laps, present)
	}
}

// Frames that reach logbook before their session.started still join the session
// (the order across the two streams is not guaranteed, R2), and frames outside
// every session are acked and not staged.
func TestFramesBeforeTheirSessionAndOutsideOne(t *testing.T) {
	r := newRace("live", runA, sidA)
	r.laps, r.lapFrames = 2, 200
	clean, _, _ := replay(t, r)

	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	var frames, events []published
	for _, p := range r.messages().msgs {
		if _, ok := p.msg.(*telemetryv1.Frame); ok {
			frames = append(frames, p)
		} else {
			events = append(events, p)
		}
	}
	// Frames of a source that never starts a session.
	other := newRace("parity", runB, sidB).messages().msgs
	for _, p := range other {
		if _, ok := p.msg.(*telemetryv1.Frame); ok {
			frames = append(frames, p)
		}
	}
	publish(t, js, frames, false)
	waitStreamRead(t, js, srv)
	publish(t, js, events, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if d := clean.diff(takeSnapshot(t, dir)); d != "" {
		t.Errorf("differs from a run with the events first:\n%s", d)
	}
}

// Messages logbook cannot use are dropped with a warning, and the rest goes on:
// an event of another type, one that cannot be decoded, a lap or an end of a
// session never started, a source no directory can be named after.
func TestWhatItCannotUseIsDropped(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	at := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	stray := []published{
		{subject: "twr.v1.live.evt." + sidB + ".lap.completed", id: sidB + ":lap.completed:1", at: at,
			msg: &eventsv1.LapCompleted{SessionId: sidB, Lap: 1, LastSeq: 5}},
		{subject: "twr.v1.live.evt." + sidB + ".session.ended", id: sidB + ":session.ended:1", at: at,
			msg: &eventsv1.SessionEnded{SessionId: sidB}},
		{subject: "twr.v1.live.evt." + sidB + ".lap.completed", id: sidB + ":lap.completed:0", at: at,
			msg: &eventsv1.LapCompleted{SessionId: sidB, Lap: 0}},
		{subject: "twr.v1.live.evt.not-a-ulid.session.started", id: "x:session.started:1", at: at,
			msg: &eventsv1.SessionStarted{SessionId: "not-a-ulid"}},
		{subject: "twr.v1.live.evt.not-a-ulid.session.ended", id: "x:session.ended:1", at: at,
			msg: &eventsv1.SessionEnded{SessionId: "not-a-ulid"}},
		// A frame of a source no session is open for.
		{subject: "twr.v1.parity.frame.gt7", id: runB + ":frame:1", at: at, msg: &telemetryv1.Frame{Seq: 1}},
	}
	publish(t, js, stray, false)
	// A frame on the events stream: a ce-type logbook does not write.
	publish(t, js, []published{{subject: "twr.v1.live.evt._.frame.gt7", id: runA + ":x:1", at: at,
		msg: &telemetryv1.Frame{}}}, false)
	// A lap.completed that is not one.
	nc := js.Conn()
	if err := nc.Publish("twr.v1.live.evt."+sidB+".lap.completed", []byte("{")); err != nil {
		t.Fatal(err)
	}
	m, err := js.Publish(t.Context(), "twr.v1.live.evt."+sidB+".session.started", []byte("{"),
		jetstream.WithMsgID("broken"))
	if err != nil || m == nil {
		t.Fatal(err)
	}
	// Then a race, which is written as if nothing had come before it.
	r := newRace("live", runA, sidA)
	publish(t, js, r.messages().msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	waitStreamRead(t, js, srv)
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if len(snap.sessions) != 1 || len(snap.laps) != r.laps {
		t.Errorf("sessions %q and %d laps, want the race's alone", snap.sessions, len(snap.laps))
	}
	for _, msg := range []string{"lap of a session that is not open; not written",
		"end of a session never started; dropped", "event dropped"} {
		if len(in.logs.find(msg, nil)) == 0 {
			t.Errorf("no %q in the log:\n%s", msg, in.logs.String())
		}
	}
}

// A session left open by a stop is restored at the next start, and a session that
// has ended is not opened again by a late copy of its start.
func TestARestartRestoresOpenSessions(t *testing.T) {
	r := newRace("live", runA, sidA)
	msgs := r.messages()
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	// Up to the end of lap 1, then a stop.
	end := slices.IndexFunc(msgs.msgs, func(p published) bool {
		l, ok := p.msg.(*eventsv1.LapCompleted)
		return ok && l.GetLap() == 1
	})
	first := start(t, cfg)
	publish(t, js, msgs.msgs[:end+1], false)
	first.logs.await(t, 1, "lap written", map[string]any{"lap": 1})
	if err := first.stop(t); err != nil {
		t.Fatal(err)
	}
	second := start(t, cfg)
	second.logs.await(t, 1, "open sessions restored", map[string]any{"sessions": 1})
	publish(t, js, msgs.msgs[end+1:], false)
	second.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	// The start again, without its id so the server keeps it: logbook ignores it.
	publish(t, js, msgs.msgs[20:22], true)
	waitStreamRead(t, js, srv)
	if err := second.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if len(snap.sessions) != 1 || snap.sessions[0][3] == "NULL" || len(snap.laps) != r.laps {
		t.Errorf("sessions %q with %d laps, want one ended with %d", snap.sessions, len(snap.laps), r.laps)
	}
}

// R8: a stop finishes the write in progress and closes the store; the next start
// can open it.
func TestAStopFinishesTheWriteInProgress(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	var in *instance
	var stopped atomic.Bool
	cfg.beforeLapWrite = func(_, _ string, lap int32) error {
		if lap == 1 && stopped.CompareAndSwap(false, true) {
			in.cancel() // SIGTERM while lap 1 is being written
		}
		return nil
	}
	in = start(t, cfg)
	r := newRace("live", runA, sidA)
	publish(t, js, r.messages().msgs, false)
	in.logs.await(t, 1, "lap written", map[string]any{"lap": 1})
	if err := in.wait(t); err != nil {
		t.Fatalf("Run returned %v after a stop, want nil", err)
	}
	in.logs.await(t, 1, "shutting down", nil)
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

// What Run refuses before it starts.
func TestRunRefusesABadConfiguration(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no data directory":         {},
		"a negative timeout":        {DataDir: t.TempDir(), FramesTimeout: -time.Second},
		"a data directory in use":   testConfig("nats://127.0.0.1:1", busyDir(t)),
		"a health address in use":   {DataDir: t.TempDir(), HealthAddr: heldAddr(t)},
		"a server URL it can't use": {DataDir: t.TempDir(), HealthAddr: "127.0.0.1:0", NATSURL: "nats://[::1"},
	} {
		t.Run(name, func(t *testing.T) {
			in := start(t, cfg)
			if err := in.wait(t); err == nil {
				t.Error("Run returned nil")
			}
		})
	}
}

func busyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	in := start(t, Config{DataDir: dir, HealthAddr: "127.0.0.1:0", NATSURL: "nats://127.0.0.1:1",
		reconnectWait: 100 * time.Millisecond})
	in.logs.await(t, 1, "store open", nil)
	return dir
}
