package runner

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// restartPair is a session ecu continued after a restart (paddock#76), as ecu#56
// publishes it. runA publishes the start, laps 1 and 2 and part of lap 3, and
// stops with no session.ended. runB counts its seqs from 1 again, so its seqs
// overlap runA's: it publishes session.continued on its first frame of the
// session, the rest of lap 3 (the lap the gap fell in, not complete and timed),
// lap 4, cut short by the flag, and the end. Its frames carry packet ids from
// 90,000, runA's below, so a lap file says which run its frames came from.
type restartPair struct {
	old, next []published
	continued published
	laps      []*eventsv1.LapCompleted
}

const (
	pairSrc      = "live"
	nextPacketID = 90_000
)

func newRestartPair() restartPair {
	r := newRace(pairSrc, runA, sidA)
	r.laps = 4
	msgs := r.messages()
	var p restartPair
	// runA up to lap 2's lap.completed, then 100 frames of lap 3.
	cut := slices.IndexFunc(msgs.msgs, func(m published) bool {
		l, ok := m.msg.(*eventsv1.LapCompleted)
		return ok && l.GetLap() == 2
	})
	p.old = slices.Clone(msgs.msgs[:cut+1])
	p.old = append(p.old, msgs.msgs[cut+1:cut+101]...)
	p.laps = msgs.laps[:2]
	last := p.old[len(p.old)-1]
	lastFuel := last.msg.(*telemetryv1.Frame).GetFuelLevel()

	// runB: 8.7 s on, its frames before the session's from seq 1, then the
	// session from at on.
	const at = uint32(600)
	start := last.at.Add(8700 * time.Millisecond)
	when := func(seq uint32) time.Time { return start.Add(time.Duration(seq-1) * time.Second / 60) }
	seq := uint32(1)
	fuel := lastFuel
	frame := func(lap int32, phase telemetryv1.Phase) {
		f := &telemetryv1.Frame{
			Seq: seq, T: float64(seq) / 60, PacketId: seq + nextPacketID, Lap: lap, Phase: phase,
			PosX: float64(seq), PosY: 2, PosZ: 0, SpeedKph: 160, EngineRpm: 7000, Gear: 4,
			Throttle: 255, FuelLevel: fuel, CarCode: r.carCode, Flags: 1,
		}
		if phase != telemetryv1.Phase_PHASE_AT_TRACK {
			f.Position, f.TrackId = proto.Int32(2), proto.Int32(7)
		}
		p.next = append(p.next, published{subject: "twr.v1." + pairSrc + ".frame.gt7", msg: f,
			id: fmt.Sprintf("%s:frame:%d", runB, seq), at: when(seq)})
		seq++
	}
	event := func(what, part string, m proto.Message) published {
		e := published{subject: "twr.v1." + pairSrc + ".evt." + sidA + "." + what, msg: m,
			id: fmt.Sprintf("%s:%s:%s:%s", pairSrc, sidA, what, part), at: when(seq - 1)}
		p.next = append(p.next, e)
		return e
	}
	for seq < at {
		frame(3, telemetryv1.Phase_PHASE_AT_TRACK)
	}
	frame(3, telemetryv1.Phase_PHASE_IN_RACE)
	p.continued = event("session.continued", runB, &eventsv1.SessionContinued{
		SessionId: sidA, RunId: runB, At: at,
		Gap: &eventsv1.SessionGap{
			StartedAt: timestamppb.New(last.at), EndedAt: timestamppb.New(when(at)), Lap: 3,
			Reason: eventsv1.GapReason_GAP_REASON_ENGINEER_RESTART,
		},
	})
	lap := func(n int32, first uint32, complete bool) {
		ev := &eventsv1.LapCompleted{SessionId: sidA, Lap: n, LapTimeMs: 98_000 + n*100, Complete: complete,
			FuelAtStart: fuel, FuelAtEnd: fuel, Position: proto.Int32(2), FirstSeq: first, LastSeq: seq - 1}
		event("lap.completed", strconv.Itoa(int(n)), ev)
		p.laps = append(p.laps, ev)
	}
	for seq < at+200 {
		frame(3, telemetryv1.Phase_PHASE_IN_RACE)
	}
	frame(4, telemetryv1.Phase_PHASE_IN_RACE) // ecu sees the lap end on the next lap's first frame
	lap(3, at, false)
	first := seq - 1
	for seq < first+150 {
		frame(4, telemetryv1.Phase_PHASE_IN_RACE)
	}
	frame(5, telemetryv1.Phase_PHASE_FINISHED)
	lap(4, first, false)
	p.laps[3].LapTimeMs, p.laps[3].Position = 0, nil
	for range 10 {
		frame(5, telemetryv1.Phase_PHASE_FINISHED)
	}
	event("session.ended", "1", &eventsv1.SessionEnded{
		SessionId: sidA, Reason: eventsv1.EndReason_END_REASON_FINISHED, LastSeq: seq - 1,
		Summary: &eventsv1.SessionSummary{LapsCompleted: 2, BestLap: proto.Int32(1), BestLapTimeMs: proto.Int32(98_100)},
	})
	return p
}

// checkPair checks the store after a restart pair: one session, ended, on runA's
// run; laps 1 to 4 as ecu numbered them, laps 1 and 2 from runA's frames and laps
// 3 and 4 from runB's alone, each with every frame of its range; nothing staged;
// and runB recorded once.
func checkPair(t *testing.T, p restartPair, dir string, in ...*instance) {
	t.Helper()
	snap := takeSnapshot(t, dir)
	if len(snap.sessions) != 1 || snap.sessions[0][3] == "NULL" || snap.sessions[0][14] != runA {
		t.Fatalf("sessions %q, want one ended, on run %s", snap.sessions, runA)
	}
	if len(snap.laps) != len(p.laps) {
		t.Fatalf("%d laps, want %d: %q", len(snap.laps), len(p.laps), snap.laps)
	}
	for i, ev := range p.laps {
		got := snap.laps[i]
		want := []string{strconv.Itoa(int(ev.GetLap())), strconv.FormatBool(ev.GetComplete()),
			strconv.Itoa(int(ev.GetFirstSeq())), strconv.Itoa(int(ev.GetLastSeq())),
			strconv.Itoa(int(ev.GetLastSeq() - ev.GetFirstSeq() + 1))}
		if g := []string{got[2], got[4], got[9], got[10], got[11]}; !slices.Equal(g, want) {
			t.Errorf("lap %d: lap, complete, seqs and frames %q, want %q", ev.GetLap(), g, want)
		}
		// Which run the file's frames came from.
		cond := "packet_id >= " + strconv.Itoa(nextPacketID)
		if ev.GetLap() >= 3 {
			cond = "packet_id < " + strconv.Itoa(nextPacketID)
		}
		if n := query(t, fmt.Sprintf(`SELECT count(*) FROM '%s' WHERE %s`, filepath.Join(dir, got[16]), cond)); n[0][0] != "0" {
			t.Errorf("lap %d holds %s frames of the other run", ev.GetLap(), n[0][0])
		}
	}
	if snap.staged != 0 {
		t.Errorf("%d frames still staged after the session ended", snap.staged)
	}
	if got := sessionRuns(t, dir); !slices.EqualFunc(got, [][]string{{sidA, runB, "600", "3"}}, slices.Equal) {
		t.Errorf("session_runs %q, want runB once, from seq 600 and lap 3", got)
	}
	var continued, ended int
	for _, i := range in {
		continued += len(i.logs.find("session continued", map[string]any{"session": sidA}))
		ended += len(i.logs.find("session ended", map[string]any{"session": sidA}))
		if w := i.logs.find("lap written without all its frames", nil); len(w) > 0 {
			t.Errorf("warned of missing frames: %v", w)
		}
	}
	if continued != 1 || ended != 1 {
		t.Errorf("continued %d times and ended %d times, want once each", continued, ended)
	}
}

func sessionRuns(t *testing.T, dir string) [][]string {
	t.Helper()
	conn, err := duckdb.NewConnector(filepath.Join(dir, "index.duckdb"), nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer func() {
		if err := errors.Join(db.Close(), conn.Close()); err != nil {
			t.Error(err)
		}
	}()
	return rows(t, db, `SELECT session_id, run, from_seq, from_lap FROM session_runs ORDER BY ALL`)
}

// paddock#76: a session ecu continues after a restart is one session in the
// store, with all its frames and laps. The session waits open between the runs,
// past the frames timeout, since a graceful stop ends nothing; a redelivered
// session.continued, before the end and after it, changes nothing.
func TestARestartPairIsOneSession(t *testing.T) {
	p := newRestartPair()
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	in := start(t, cfg)
	publish(t, js, p.old, false)
	in.logs.await(t, 1, "lap written", map[string]any{"lap": 2})
	// ecu stopped: nothing ends the session, however long the gap.
	time.Sleep(cfg.FramesTimeout + cfg.ackWait + time.Second)
	if got := in.logs.find("session ended", nil); len(got) > 0 {
		t.Fatalf("the session ended with no session.ended: %v", got)
	}
	publish(t, js, p.next, false)
	in.logs.await(t, 1, "session continued", map[string]any{"session": sidA, "run": runB})
	// Delivered again (without its id, so the server keeps it), while the session
	// is open and once it has ended.
	publish(t, js, []published{p.continued}, true)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	publish(t, js, []published{p.continued}, true)
	// One of a session never started, and one with no run, are dropped.
	stray, noRun := p.continued, p.continued
	stray.subject, stray.id = "twr.v1.live.evt."+sidB+".session.continued", "live:"+sidB+":session.continued:"+runB
	stray.msg = &eventsv1.SessionContinued{SessionId: sidB, RunId: runB, At: 1}
	noRun.msg = &eventsv1.SessionContinued{SessionId: sidA, At: 1}
	publish(t, js, []published{stray, noRun}, true)
	waitStreamRead(t, js, srv)
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	checkPair(t, p, dir, in)
	for _, msg := range []string{"continue of a session never started; dropped", "event dropped"} {
		if len(in.logs.find(msg, nil)) == 0 {
			t.Errorf("no %q in the log", msg)
		}
	}
}

// A logbook stopped once the session has been continued restores it with both
// its runs, and goes on staging the later run's frames for it.
func TestAContinuedSessionIsRestored(t *testing.T) {
	p := newRestartPair()
	srv, js := testServer(t)
	dir := t.TempDir()
	cfg := testConfig(srv.ClientURL(), dir)
	first := start(t, cfg)
	publish(t, js, p.old, false)
	// runB up to lap 3's middle, then a stop.
	mid := slices.IndexFunc(p.next, func(m published) bool {
		f, ok := m.msg.(*telemetryv1.Frame)
		return ok && f.GetSeq() == 700
	})
	publish(t, js, p.next[:mid], false)
	first.logs.await(t, 1, "session continued", map[string]any{"session": sidA})
	waitStreamRead(t, js, srv)
	if err := first.stop(t); err != nil {
		t.Fatal(err)
	}
	second := start(t, cfg)
	second.logs.await(t, 1, "open sessions restored", map[string]any{"sessions": 1})
	publish(t, js, p.next[mid:], false)
	second.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := second.stop(t); err != nil {
		t.Fatal(err)
	}
	checkPair(t, p, dir, first, second)
}

// A session's end waits for the complete laps its summary lists. The lap an ecu
// restart's gap took the line crossing of is in the summary alone, not complete
// and with no lap.completed, so the end does not wait for it; from an ecu that
// lists no laps, it waits for laps 1 to laps_completed.
func TestTheEndWaitsForTheSummarysCompleteLaps(t *testing.T) {
	lap := func(n int32, complete bool) *eventsv1.LapCompleted {
		return &eventsv1.LapCompleted{Lap: n, Complete: complete}
	}
	for name, c := range map[string]struct {
		sum  *eventsv1.SessionSummary
		want []int32
	}{
		"listed": {&eventsv1.SessionSummary{LapsCompleted: 7, Laps: []*eventsv1.LapCompleted{
			lap(1, true), lap(2, true), lap(3, true), lap(4, true), lap(5, true), lap(6, true),
			lap(7, false), lap(8, true), lap(9, false)}}, []int32{1, 2, 3, 4, 5, 6, 8}},
		"none listed": {&eventsv1.SessionSummary{LapsCompleted: 3}, []int32{1, 2, 3}},
		"no summary":  {nil, nil},
	} {
		if got := expectedLaps(c.sum); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
