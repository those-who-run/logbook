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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
)

// Replays of two v1 recordings as ecu v0.1.4 published them, captured from NATS by
// testdata/replays/capture.go, which says where they came from and how to make
// them again; internal/store's TestTheReplayFixturesAreTheCaptures pins them.
const (
	suzukaPitStop = "../../testdata/replays/real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz"
	suzukaNoStop  = "../../testdata/replays/real-nuc-suzuka-no-stop-2026-10-15.jsonl.gz"
)

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

// v1's laps of real-nuc-suzuka-no-stop-2026-10-15, from the same golden, with no
// frame of difference.
var v1NoStopLaps = [][]string{
	{"1", "60358", "13.592", "10", "151"},
	{"2", "64611", "16.263", "7", "139"},
	{"3", "63139", "17.33", "4", "135"},
	{"4", "61186", "17.332", "2", "125"},
	{"5", "63366", "17.24", "2", "127"},
	{"6", "63981", "15.661", "2", "129"},
}

// checkLaps compares a session's lap rows with v1's (lap, lap time, fuel used,
// position, frames), and checks each lap's file holds exactly the frames its row
// counts, all in the row's seq range.
func checkLaps(t *testing.T, dir string, laps, v1 [][]string) {
	t.Helper()
	if len(laps) != len(v1) {
		t.Errorf("%d laps, want v1's %d: %q", len(laps), len(v1), laps)
		return
	}
	for i, lap := range laps {
		if got := []string{lap[2], lap[3], lap[7], lap[8], lap[11]}; !slices.Equal(got, v1[i]) {
			t.Errorf("%s lap %d: %q, want v1's %q", lap[0], i+1, got, v1[i])
		}
		n := query(t, fmt.Sprintf(`SELECT count(*), count(DISTINCT seq) FROM '%s' WHERE seq BETWEEN %s AND %s`,
			filepath.Join(dir, lap[16]), lap[9], lap[10]))
		all := query(t, fmt.Sprintf(`SELECT count(*) FROM '%s'`, filepath.Join(dir, lap[16])))
		if want := []string{lap[11], lap[11]}; !slices.Equal(n[0], want) || all[0][0] != lap[11] {
			t.Errorf("%s lap %d: the file holds %q frames (%q in its range, distinct), the row says %s",
				lap[0], i+1, all[0], n[0], lap[11])
		}
	}
}

// replayRecording runs logbook against a server while r is published, until its
// session has ended and every message is acked, and returns the snapshot.
func replayRecording(t *testing.T, r recording) (snapshot, *instance, string) {
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
	return takeSnapshot(t, dir), in, dir
}

// AC1: `ecu replay --speed max` of the Suzuka pit-stop recording leaves one
// session and its laps in the store, equal to the replay's events, which are v1's
// six laps; every message is acked once its write commits, and nothing is left
// staged.
func TestTheSuzukaReplayLeavesItsSessionAndLaps(t *testing.T) {
	r := readRecording(t, suzukaPitStop)
	snap, in, dir := replayRecording(t, r)

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

	checkLaps(t, dir, snap.laps, v1Laps)
	for i, lap := range snap.laps {
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
	clean, _, _ := replayRecording(t, r)

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

// AC3: two replays of different recordings at once, under different sources,
// give two sessions, each as a replay of it alone gives and with v1's laps,
// though both number their frames alike. Each runs ahead of the other in turn:
// in lockstep, two replays of one track keep their seqs close, and a session
// that took the other's progress for its own would not show (R5). With
// consume.go's frame loop raising high for every open session, not only the
// frame's own, each order fails.
func TestTwoReplaysAtOnceNeverMix(t *testing.T) {
	pit, noStop := readRecording(t, suzukaPitStop), readRecording(t, suzukaNoStop)
	alonePit, _, _ := replayRecording(t, pit)
	aloneNoStop, _, _ := replayRecording(t, noStop)

	for _, c := range []struct {
		name          string
		ahead, behind recording
	}{
		{"the pit stop ahead", pit, noStop},
		{"the no stop ahead", noStop, pit},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, js := testServer(t)
			dir := t.TempDir()
			in := start(t, testConfig(srv.ClientURL(), dir))
			publishRaw(t, js, interleave(c.ahead.msgs, c.behind.msgs, 150))
			in.logs.await(t, 1, "session ended", map[string]any{"session": pit.sid})
			in.logs.await(t, 1, "session ended", map[string]any{"session": noStop.sid})
			waitStreamRead(t, js, srv)
			if err := in.stop(t); err != nil {
				t.Fatal(err)
			}
			snap := takeSnapshot(t, dir)
			if len(snap.sessions) != 2 {
				t.Fatalf("sessions %q, want two", snap.sessions)
			}
			for _, s := range []struct {
				r     recording
				alone snapshot
				v1    [][]string
			}{{pit, alonePit, v1Laps}, {noStop, aloneNoStop, v1NoStopLaps}} {
				got := snap.only(s.r.sid)
				checkLaps(t, dir, got.laps, s.v1)
				if d := s.alone.diff(got); d != "" {
					t.Errorf("%s differs from a replay of it alone:\n%s", s.r.src, d)
				}
			}
		})
	}
}

// interleave is two replays publishing at once, the second starting lag messages
// after the first.
func interleave(first, second []captured, lag int) []captured {
	out := slices.Clone(first[:min(lag, len(first))])
	first = first[len(out):]
	for i := range max(len(first), len(second)) {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(second) {
			out = append(out, second[i])
		}
	}
	return out
}
