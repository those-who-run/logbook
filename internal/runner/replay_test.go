package runner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
)

// The replay of a v1 recording as ecu v0.1.4 published it, captured from NATS by
// testdata/replays/capture.go, which says where it came from and how to make it
// again; internal/store's TestTheReplayFixtureIsTheCapture pins it.
const suzukaPitStop = "../../testdata/replays/real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz"

// captured is one message of a replay fixture, as ecu published it.
type captured struct {
	Subject string              `json:"subject"`
	Headers map[string][]string `json:"headers"`
	Payload json.RawMessage     `json:"payload"`
}

// recording is a replay fixture: its messages, in the order ecu published them,
// and what they name.
type recording struct {
	msgs     []captured
	src, sid string
	run      string
}

func readRecording(t *testing.T, path string) recording {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a fixed test file
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var r recording
	sc := bufio.NewScanner(zr)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m captured
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		r.msgs = append(r.msgs, m)
		h := ce.Header(m.Headers)
		switch {
		case h.Get(ce.TypeHeader) == typeStarted:
			r.src, _ = eventSource(m.Subject)
			r.sid = strings.Split(m.Subject, ".")[4]
		case r.run == "" && strings.HasSuffix(m.Subject, ".frame.gt7"):
			r.run = runOf(h.Get(ce.IDHeader))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if r.src == "" || r.run == "" {
		t.Fatalf("%s holds no session.started or no frame", path)
	}
	return r
}

// as is the same replay under another source, session and run, with car code
// car in place of the recording's: a second `ecu replay` of the recording, told
// apart by its frames.
func (r recording) as(t *testing.T, src, sid, run string, car int) recording {
	t.Helper()
	out := recording{src: src, sid: sid, run: run}
	repl := strings.NewReplacer("twr.v1."+r.src+".", "twr.v1."+src+".", `"`+r.src+`"`, `"`+src+`"`,
		r.src+":", src+":", r.sid, sid, r.run, run, `"carCode":683`, `"carCode":`+strconv.Itoa(car))
	for _, m := range r.msgs {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var c captured
		if err := json.Unmarshal([]byte(repl.Replace(string(b))), &c); err != nil {
			t.Fatal(err)
		}
		out.msgs = append(out.msgs, c)
	}
	return out
}

// publishRaw sends msgs to JetStream as they were captured, headers and payload
// unchanged, and waits for the server to store them all.
func publishRaw(t *testing.T, js jetstream.JetStream, msgs []captured) {
	t.Helper()
	for _, m := range msgs {
		if _, err := js.PublishMsgAsync(&nats.Msg{Subject: m.Subject, Header: nats.Header(m.Headers), Data: m.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(30 * time.Second):
		t.Fatal("publishing did not complete within 30 s")
	}
}

// v1's laps of real-nuc-suzuka-pit-stop-2026-10-15, as v1 1.0.0 stored them
// (ecu's internal/race/testdata/golden at v0.1.4, which paddock's parity
// compares): lap, lap time, fuel used, position and frames. Lap 3 holds one frame
// more than v1's 166: the frozen-pause frame v1 left out (internal/store's
// v1SuzukaPitStop says more).
var v1Laps = [][]string{
	{"1", "57109", "31.124", "1", "118"},
	{"2", "60538", "27.691", "1", "121"},
	{"3", "82544", "15.009", "1", "167"},
	{"4", "69892", "-58.577", "1", "146"},
	{"5", "59541", "18.662", "1", "119"},
	{"6", "60310", "19.382", "1", "122"},
}

// replayRecording runs logbook against a server while r is published, until its
// session has ended and every message is acked, and returns the snapshot.
func replayRecording(t *testing.T, r recording) (snapshot, *instance) {
	t.Helper()
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	publishRaw(t, js, r.msgs)
	in.logs.await(t, 1, "session ended", map[string]any{"session": r.sid})
	waitStreamRead(t, js, srv)
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	return takeSnapshot(t, dir), in
}

// AC1: `ecu replay --speed max` of the Suzuka pit-stop recording leaves one
// session and its laps in the store, equal to the replay's events, which are v1's
// six laps; every message is acked once its write commits, and nothing is left
// staged.
func TestTheSuzukaReplayLeavesItsSessionAndLaps(t *testing.T) {
	r := readRecording(t, suzukaPitStop)
	snap, in := replayRecording(t, r)

	if len(snap.sessions) != 1 {
		t.Fatalf("sessions %q, want one", snap.sessions)
	}
	s := snap.sessions[0]
	// src, id, type, car code, total laps, laps completed, fuel at start, fuel
	// used, final position, why it ended, run.
	got := []string{s[0], s[1], s[4], s[5], s[6], s[8], s[10], s[11], s[12], s[13], s[14]}
	want := []string{r.src, r.sid, "lap_race", "683", "6", "6", "96.72370910644531", "53.291", "1", "finished", r.run}
	if !slices.Equal(got, want) {
		t.Errorf("session %q,\nwant %q", got, want)
	}
	// started_at is the first frame's ce-time and ended_at the end's: both the
	// replay's clock, 2026-01-01, about seven minutes apart.
	startedAt, _ := time.Parse(time.RFC3339Nano, s[2])
	endedAt, _ := time.Parse(time.RFC3339Nano, s[3])
	if endedAt.Before(startedAt) || endedAt.Sub(startedAt) > 10*time.Minute ||
		startedAt.Format(time.DateOnly) != "2026-01-01" {
		t.Errorf("the session runs from %v to %v", startedAt, endedAt)
	}

	if len(snap.laps) != len(v1Laps) {
		t.Fatalf("%d laps, want v1's %d: %q", len(snap.laps), len(v1Laps), snap.laps)
	}
	for i, lap := range snap.laps {
		// lap, lap time, fuel used, position, frames.
		if got := []string{lap[2], lap[3], lap[7], lap[8], lap[11]}; !slices.Equal(got, v1Laps[i]) {
			t.Errorf("lap %d: %q, want v1's %q", i+1, got, v1Laps[i])
		}
		file := fmt.Sprintf("sessions/%s/%s/lap-%03d.parquet", r.src, r.sid, i+1)
		if lap[16] != file || snap.files[file] == "" {
			t.Errorf("lap %d: file %q, want %s in place", i+1, lap[16], file)
		}
	}
	if len(snap.files) != len(v1Laps) {
		t.Errorf("%d lap files, want %d", len(snap.files), len(v1Laps))
	}
	if snap.staged != 0 {
		t.Errorf("%d frames still staged after the session ended", snap.staged)
	}
	if n := len(in.logs.find("lap written", nil)); n != len(v1Laps) {
		t.Errorf("%d laps written, want %d", n, len(v1Laps))
	}
	for _, msg := range []string{"lap written without all its frames", "event dropped", "frame dropped"} {
		if w := in.logs.find(msg, nil); len(w) > 0 {
			t.Errorf("%s: %v", msg, w)
		}
	}
}

// AC2: killing logbook (SIGKILL, a process of its own) mid-replay, after lap 2's
// lap.completed arrives and before it is written, then starting it again, gives
// the same store as a replay with no kill.
func TestAKillMidReplayGivesTheSameStore(t *testing.T) {
	r := readRecording(t, suzukaPitStop)
	clean, _ := replayRecording(t, r)

	srv, js := testServer(t)
	dir := t.TempDir()
	first := newLogs()
	child := startChild(t, srv.ClientURL(), dir, 2, first)
	first.await(t, 1, "consumers bound", nil)
	publishRaw(t, js, r.msgs)
	err := child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the first run ended with %v, want SIGKILL; log:\n%s", err, first.String())
	}
	if n := len(first.find("lap written", nil)); n != 1 {
		t.Fatalf("%d laps written before the kill, want 1", n)
	}

	second := start(t, testConfig(srv.ClientURL(), dir))
	second.logs.await(t, 1, "session ended", map[string]any{"session": r.sid})
	waitStreamRead(t, js, srv)
	if err := second.stop(t); err != nil {
		t.Fatal(err)
	}
	if d := clean.diff(takeSnapshot(t, dir)); d != "" {
		t.Errorf("the store after a kill differs from a clean replay's:\n%s", d)
	}
	if got := second.logs.find("lap written", map[string]any{"lap": 1}); len(got) != 0 {
		t.Errorf("lap 1 was written again after the restart")
	}
}

// AC3: two replays of the recording at once, under different sources, give two
// sessions, each as a replay of its own gives, its lap files holding only its own
// frames, though both number their frames alike.
func TestTwoReplaysAtOnceNeverMix(t *testing.T) {
	a := readRecording(t, suzukaPitStop)
	b := a.as(t, "replay-other", sidB, runB, 1234)
	alone, _ := replayRecording(t, b)

	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	// Interleaved, as two replays publishing at once would be.
	var both []captured
	for i := range max(len(a.msgs), len(b.msgs)) {
		if i < len(a.msgs) {
			both = append(both, a.msgs[i])
		}
		if i < len(b.msgs) {
			both = append(both, b.msgs[i])
		}
	}
	publishRaw(t, js, both)
	in.logs.await(t, 1, "session ended", map[string]any{"session": a.sid})
	in.logs.await(t, 1, "session ended", map[string]any{"session": b.sid})
	waitStreamRead(t, js, srv)
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	if len(snap.sessions) != 2 || len(snap.laps) != 2*len(v1Laps) {
		t.Fatalf("sessions %q with %d laps, want two of %d", snap.sessions, len(snap.laps), len(v1Laps))
	}
	if d := alone.diff(snap.only(b.sid)); d != "" {
		t.Errorf("replay-other differs from a replay of it alone:\n%s", d)
	}
	for _, lap := range snap.laps {
		car := "683"
		if lap[0] == b.src {
			car = "1234"
		}
		got := query(t, fmt.Sprintf(`SELECT DISTINCT car_code FROM '%s'`, filepath.Join(dir, lap[16])))
		if len(got) != 1 || got[0][0] != car {
			t.Errorf("%s lap %s holds frames of cars %q, want only %s", lap[0], lap[2], got, car)
		}
	}
}
