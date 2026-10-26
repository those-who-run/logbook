package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The stream definitions in testdata/streams are paddock's deploy/nats/streams at
// this commit (paddock#36, merged as paddock#271), copied unchanged: the tests
// create TELEMETRY and EVENTS from them, as the bootstrap does. To take a newer
// definition, copy the files from that commit and change the commit and the sums
// here together.
const paddockCommit = "c0fe6669ee16aab1f3ee49f3a21e8a312cb790bc"

var streamSHA256 = map[string]string{
	"events.json":    "35f36adb53eebba2f9501faf08a3f3c15d889b17579896bdfc62e35a4becf37e",
	"telemetry.json": "fb19cea7144a8070149e55de0f7890817903968e09a95ebdbbf8607a43faa4b5",
}

// startNATS starts an in-process nats-server with JetStream on port (-1 for any),
// stopped when the test ends. A real server: nothing of NATS is mocked (R9).
func startNATS(t *testing.T, port int) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server not ready after 10 s")
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	return s
}

// connect is a JetStream client for the test's own publishing and checks.
func connect(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// createStreams creates the streams from paddock's definition files, as the
// bootstrap would.
func createStreams(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range streamSHA256 {
			if !yield(k) {
				return
			}
		}
	}) {
		b, err := os.ReadFile(filepath.Join("testdata", "streams", name)) //nolint:gosec // a fixed test file
		if err != nil {
			t.Fatal(err)
		}
		var cfg jetstream.StreamConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := js.CreateStream(t.Context(), cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// testServer is a server with the streams, and a client on it.
func testServer(t *testing.T) (*server.Server, jetstream.JetStream) {
	t.Helper()
	s := startNATS(t, -1)
	js := connect(t, s.ClientURL())
	createStreams(t, js)
	return s, js
}

// The test's own copies of the definitions are paddock's, byte for byte.
func TestTheStreamsArePaddocksDefinitions(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "streams"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(streamSHA256) {
		t.Errorf("testdata/streams holds %d files, want %d", len(entries), len(streamSHA256))
	}
	for name, want := range streamSHA256 {
		b, err := os.ReadFile(filepath.Join("testdata", "streams", name)) //nolint:gosec // a fixed test file
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: sha256 %s, want %s: copy it from paddock at %s", name, got, want, paddockCommit)
		}
	}
}

// race is a session as ecu publishes it: frames before the start, a race of laps
// each lapFrames long, the last cut short by the flag, the finished frames after
// it, and the events between them where ecu publishes them.
type race struct {
	src, run, sid string
	carCode       int32
	firstSeq      uint32 // the seq of the first frame published
	laps          int
	lapFrames     int
	endedAt       time.Time
}

func newRace(src, run, sid string) race {
	return race{src: src, run: run, sid: sid, carCode: 3431, firstSeq: 0, laps: 3, lapFrames: 300,
		endedAt: time.Date(2026, 10, 15, 19, 40, 0, 0, time.UTC)}
}

// published is what a race sends, in order: each message with its subject.
type published struct {
	subject string
	msg     proto.Message
	id      string
	at      time.Time
}

type raceMessages struct {
	msgs    []published
	started *eventsv1.SessionStarted
	laps    []*eventsv1.LapCompleted
	ended   *eventsv1.SessionEnded
}

func (r race) messages() raceMessages {
	var out raceMessages
	seq := r.firstSeq
	start := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	at := func(s uint32) time.Time { return start.Add(time.Duration(s) * time.Second / 60) }
	frame := func(lap int32, phase telemetryv1.Phase, i int, fuel float64) {
		angle := 2 * math.Pi * float64(i) / float64(r.lapFrames)
		f := &telemetryv1.Frame{
			Seq: seq, T: float64(seq) / 60, PacketId: seq + 5000, Lap: lap, Phase: phase,
			PosX: 100 * math.Cos(angle), PosY: 2, PosZ: 100 * math.Sin(angle),
			SpeedKph: 150 + 50*math.Sin(angle) + float64(lap), EngineRpm: 7000,
			Gear: 4, SuggestedGear: 15, Throttle: 255, FuelLevel: fuel,
			CarCode: r.carCode, Flags: 1,
		}
		if phase != telemetryv1.Phase_PHASE_AT_TRACK {
			f.Position = proto.Int32(2)
			f.TrackId = proto.Int32(7)
		}
		out.msgs = append(out.msgs, published{
			subject: "twr.v1." + r.src + ".frame.gt7", msg: f,
			id: fmt.Sprintf("%s:frame:%d", r.run, seq), at: at(seq),
		})
		seq++
	}
	event := func(what string, m proto.Message, part int) {
		out.msgs = append(out.msgs, published{
			subject: "twr.v1." + r.src + ".evt." + r.sid + "." + what, msg: m,
			id: fmt.Sprintf("%s:%s:%d", r.sid, what, part), at: at(seq - 1),
		})
	}
	for i := range 20 {
		frame(0, telemetryv1.Phase_PHASE_AT_TRACK, i, 50)
	}
	out.started = &eventsv1.SessionStarted{
		SessionId: r.sid, Source: r.src, Game: eventsv1.Game_GAME_GT7,
		SessionType: eventsv1.SessionType_SESSION_TYPE_RACE, CarCode: r.carCode,
		TotalLaps: proto.Int32(int32(r.laps)), FirstSeq: seq, //nolint:gosec // a few laps
		StartedAt: timestamppb.New(at(seq)),
	}
	// ecu sees the start on the session's first frame, so it publishes the frame
	// first and the event after it.
	frame(1, telemetryv1.Phase_PHASE_IN_RACE, 0, 50)
	event("session.started", out.started, 1)
	fuel := 50.0
	for lap := int32(1); lap <= int32(r.laps); lap++ { //nolint:gosec // a few laps
		// The lap's first frame went out before the event that closed the lap
		// before it, or with the start.
		first := seq - 1
		startFuel := fuel
		n := r.lapFrames
		if lap == int32(r.laps) { //nolint:gosec // a few laps
			n = r.lapFrames / 2
		}
		for i := range n {
			if i == 0 {
				continue // published already
			}
			fuel -= 0.005
			frame(lap, telemetryv1.Phase_PHASE_IN_RACE, i, fuel)
		}
		ev := &eventsv1.LapCompleted{
			SessionId: r.sid, Lap: lap, LapTimeMs: 98_000 + lap*100, Complete: true,
			FuelAtStart: startFuel, FuelAtEnd: fuel, FuelUsed: startFuel - fuel,
			Position: proto.Int32(2), FirstSeq: first, LastSeq: seq - 1,
		}
		if lap == int32(r.laps) { //nolint:gosec // a few laps
			// The flag falls part way round: the finished frame closes the race.
			frame(lap+1, telemetryv1.Phase_PHASE_FINISHED, n, fuel)
			ev.LapTimeMs, ev.Complete, ev.Position, ev.LastSeq = 0, false, nil, seq-1
		} else {
			// ecu sees a lap end on the next lap's first frame.
			frame(lap+1, telemetryv1.Phase_PHASE_IN_RACE, 0, fuel)
		}
		event("lap.completed", ev, int(lap))
		out.laps = append(out.laps, ev)
	}
	for i := range 10 {
		frame(int32(r.laps)+1, telemetryv1.Phase_PHASE_FINISHED, i, fuel) //nolint:gosec // a few laps
	}
	out.ended = &eventsv1.SessionEnded{
		SessionId: r.sid, Reason: eventsv1.EndReason_END_REASON_FINISHED,
		Summary: &eventsv1.SessionSummary{
			LapsCompleted: int32(r.laps - 1), BestLap: proto.Int32(1), //nolint:gosec // a few laps
			BestLapTimeMs: proto.Int32(98_100),
		},
	}
	event("session.ended", out.ended, 1)
	out.msgs[len(out.msgs)-1].at = r.endedAt
	return out
}

// publish sends msgs to JetStream, each with its CloudEvents headers, and waits for
// the server to store them all. With noMsgID the Nats-Msg-Id is left off, so the
// server keeps a repeat as a message of its own, as it would after the duplicate
// window.
func publish(t *testing.T, js jetstream.JetStream, msgs []published, noMsgID bool) {
	t.Helper()
	for _, p := range msgs {
		h, data, err := ce.Encode(p.msg, ce.Event{ID: p.id, Source: "/twr/ecu/gt7", Time: p.at})
		if err != nil {
			t.Fatal(err)
		}
		if noMsgID {
			delete(h, ce.MsgIDHeader)
		}
		if _, err := js.PublishMsgAsync(&nats.Msg{Subject: p.subject, Header: nats.Header(h), Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(30 * time.Second):
		t.Fatal("publishing did not complete within 30 s")
	}
}

// logs collects the runner's JSON log, one record per Write, and lets a test wait
// for a record.
type logs struct {
	mu      sync.Mutex
	records []map[string]any
	raw     bytes.Buffer
	changed chan struct{}
}

func newLogs() *logs { return &logs{changed: make(chan struct{})} }

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var r map[string]any
	if err := json.Unmarshal(p, &r); err == nil {
		l.records = append(l.records, r)
	}
	l.raw.Write(p)
	close(l.changed)
	l.changed = make(chan struct{})
	return len(p), nil
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.raw.String()
}

// find returns the records with msg and every attr given.
func (l *logs) find(msg string, attrs map[string]any) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, r := range l.records {
		if r["msg"] != msg {
			continue
		}
		ok := true
		for k, v := range attrs {
			if fmt.Sprint(r[k]) != fmt.Sprint(v) {
				ok = false
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// await waits until n records match, or fails the test after timeout.
func (l *logs) await(t *testing.T, n int, msg string, attrs map[string]any) []map[string]any {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		l.mu.Lock()
		changed := l.changed
		l.mu.Unlock()
		if got := l.find(msg, attrs); len(got) >= n {
			return got
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("no %d %q records with %v within 60 s; log:\n%s", n, msg, attrs, l.String())
		}
	}
}

// instance is a running logbook.
type instance struct {
	logs   *logs
	cancel context.CancelFunc
	done   chan error
}

// testConfig is a configuration with the waits short enough for a test.
func testConfig(url, dir string) Config {
	return Config{
		NATSURL: url, DataDir: dir, HealthAddr: "127.0.0.1:0", FramesTimeout: 3 * time.Second,
		ackWait: 2 * time.Second, fetchWait: 50 * time.Millisecond,
		retryWait: 100 * time.Millisecond, reconnectWait: 100 * time.Millisecond,
	}
}

func start(t *testing.T, cfg Config) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	in := &instance{logs: newLogs(), cancel: cancel, done: make(chan error, 1)}
	logger := slog.New(slog.NewJSONHandler(in.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() { in.done <- Run(ctx, cfg, logger, "test") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-in.done:
		case <-time.After(15 * time.Second):
			t.Error("logbook still running 15 s after the test")
		}
	})
	return in
}

// stop cancels the instance, as SIGTERM does, and returns what Run returned.
func (in *instance) stop(t *testing.T) error {
	t.Helper()
	in.cancel()
	return in.wait(t)
}

// wait waits for Run to return, for at most the 10 s R8 allows.
func (in *instance) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-in.done:
		in.done <- err // for the cleanup
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("logbook still running 10 s after it was stopped; log:\n%s", in.logs.String())
	}
	return nil
}

// healthAddr is the address the instance serves GET /healthz on.
func (in *instance) healthAddr(t *testing.T) string {
	t.Helper()
	r := in.logs.await(t, 1, "serving", nil)
	addr, _ := r[0]["addr"].(string)
	return addr
}

func getHealth(t *testing.T, addr string) (int, healthBody) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body healthBody
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatalf("GET /healthz: %v: %q", err, b)
	}
	return resp.StatusCode, body
}

// snapshot is what a data directory holds once logbook has stopped: its sessions
// and laps, without the time each lap was first written, its lap files, byte for
// byte, and the frames still staged.
type snapshot struct {
	sessions [][]string
	laps     [][]string
	files    map[string]string
	staged   int
}

func takeSnapshot(t *testing.T, dir string) snapshot {
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
	var snap snapshot
	snap.sessions = rows(t, db, `SELECT src, id, started_at, ended_at, type, car_code, total_laps, first_seq,
    laps_completed, best_lap_ms, fuel_at_start, fuel_used, final_position, end_reason, run
FROM sessions ORDER BY src, id`)
	snap.laps = rows(t, db, `SELECT src, session_id, lap, lap_time_ms, complete, fuel_start, fuel_end, fuel_used,
    position, first_seq, last_seq, frames, top_speed_kph, min_speed_kph, path_m, max_jump_m, file
FROM laps ORDER BY src, session_id, lap`)
	n := rows(t, db, `SELECT count(*) FROM frames_pending`)
	_, _ = fmt.Sscan(n[0][0], &snap.staged)
	snap.files = map[string]string{}
	err = filepath.WalkDir(filepath.Join(dir, "sessions"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the test's own directory
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		snap.files[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return snap
}

func rows(t *testing.T, db *sql.DB, q string, args ...any) [][]string {
	t.Helper()
	r, err := db.QueryContext(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	cols, err := r.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for r.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = "NULL"
			if v.Valid {
				row[i] = v.String
			}
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// query reads a lap file, or any query, in an in-memory DuckDB of its own.
func query(t *testing.T, q string) [][]string {
	t.Helper()
	conn, err := duckdb.NewConnector("", nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer func() { _ = errors.Join(db.Close(), conn.Close()) }()
	return rows(t, db, q)
}

func (s snapshot) diff(o snapshot) string {
	var d []string
	if !slices.EqualFunc(s.sessions, o.sessions, slices.Equal) {
		d = append(d, fmt.Sprintf("sessions %q != %q", s.sessions, o.sessions))
	}
	if !slices.EqualFunc(s.laps, o.laps, slices.Equal) {
		d = append(d, fmt.Sprintf("laps %q != %q", s.laps, o.laps))
	}
	if len(s.files) != len(o.files) {
		d = append(d, fmt.Sprintf("%d lap files != %d", len(s.files), len(o.files)))
	}
	for name, b := range s.files {
		if o.files[name] != b {
			d = append(d, name+" differs")
		}
	}
	if s.staged != o.staged {
		d = append(d, fmt.Sprintf("%d frames staged != %d", s.staged, o.staged))
	}
	return strings.Join(d, "\n")
}

// freePort is a TCP port nothing listens on, for a server started later.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// heldAddr is an address another listener holds until the test ends.
func heldAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}
