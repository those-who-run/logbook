package store

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
)

// The replay fixtures are what ecu's `ecu replay --speed max` published for two
// of v1's recordings (paddock's recordings/SOURCES.yaml has their sha256s),
// captured from NATS by testdata/replays/capture.go, which says how to make them
// again. A new capture changes its row here.
var replayFixtures = []struct {
	file, sha256, ecu, recording string
}{
	{
		"real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz",
		"ec15a8e3346b5008b1058ec659ecc8803c674954a5b9011cee1150d2704f3f2d",
		"v0.1.4", "9684c2ee9e1da825b0870b22bfec81ecff833a3ae20e523c0efc61e8863e5bdb",
	},
	{
		"real-nuc-suzuka-no-stop-2026-10-15.jsonl.gz",
		"295341b0978e4d8f347038bce1763334a2424ae6d45f3450c39ace75bac1c5bc",
		"v0.1.4", "0477116644a19ab97aac4c19dbecaa4129ee691c57378562cf36c7a094c4aeaa",
	},
}

const replayFixture = "../../testdata/replays/real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz"

// v1Lap is a lap as v1 1.0.0 stored it for the recording: ecu's golden
// (internal/race/testdata/golden/real-nuc-suzuka-pit-stop-2026-10-15.jsonl at
// ecu v0.1.4), the numbers paddock's parity compares.
type v1Lap struct {
	lap       int32
	lapTimeMs int32
	fuelStart float64
	fuelEnd   float64
	fuelUsed  float64
	position  int32
	frames    int32
}

// But for one frame: lap 3 holds 167 here and 166 in v1. v1 also left out seq
// 322, the first in-race frame after the pause in lap 3, which it read as a
// frozen pause (the car still for a second or more); v2 keeps every in-race frame
// of the lap (R5), and ecu's race tests leave the frozen pause out of scope
// (TestSessionsAndLapsEqualV1s and frozen, ecu v0.1.4).
var v1SuzukaPitStop = []v1Lap{
	{1, 57109, 96.72370910644531, 65.59944915771484, 31.124, 1, 118},
	{2, 60538, 65.59944915771484, 37.908443450927734, 27.691, 1, 121},
	{3, 82544, 37.908443450927734, 22.89894676208496, 15.009, 1, 166 + 1},
	{4, 69892, 22.89894676208496, 81.47602844238281, -58.577, 1, 146},
	{5, 59541, 81.47602844238281, 62.81436538696289, 18.662, 1, 119},
	{6, 60310, 62.81436538696289, 43.43214797973633, 19.382, 1, 122},
}

// recorded is a session read from the replay fixture: its events, its frames and
// the end's ce-time.
type recorded struct {
	src     string
	started *eventsv1.SessionStarted
	laps    []*eventsv1.LapCompleted
	frames  []*telemetryv1.Frame
	ended   *eventsv1.SessionEnded
	endedAt time.Time
}

func readReplay(t *testing.T) recorded {
	t.Helper()
	f, err := os.Open(replayFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var r recorded
	sc := bufio.NewScanner(zr)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l struct {
			Subject string              `json:"subject"`
			Headers map[string][]string `json:"headers"`
			Payload json.RawMessage     `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		h := ce.Header(l.Headers)
		var m proto.Message
		switch h.Get(ce.TypeHeader) {
		case "run.thosewho.frame.gt7.v1":
			f := &telemetryv1.Frame{}
			r.frames, m = append(r.frames, f), f
		case "run.thosewho.session.started.v1":
			r.started = &eventsv1.SessionStarted{}
			m = r.started
		case "run.thosewho.lap.completed.v1":
			lap := &eventsv1.LapCompleted{}
			r.laps, m = append(r.laps, lap), lap
		case "run.thosewho.session.ended.v1":
			r.ended = &eventsv1.SessionEnded{}
			m = r.ended
		default:
			continue // session.track: logbook does not write it yet
		}
		e, err := ce.Decode(h, l.Payload, m)
		if err != nil {
			t.Fatalf("%s: %v", l.Subject, err)
		}
		if m == r.ended {
			r.endedAt = e.Time
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if r.started == nil || r.ended == nil {
		t.Fatal("the fixture holds no whole session")
	}
	r.src = r.started.GetSource()
	return r
}

// Each fixture is the capture its provenance names, byte for byte, and there is
// no other.
func TestTheReplayFixturesAreTheCaptures(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "replays")
	found, err := filepath.Glob(filepath.Join(dir, "*.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != len(replayFixtures) {
		t.Errorf("%d fixtures in %s, want %d: %q", len(found), dir, len(replayFixtures), found)
	}
	for _, f := range replayFixtures {
		b, err := os.ReadFile(filepath.Join(dir, f.file)) //nolint:gosec // a fixed test file
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.sha256 {
			t.Errorf("%s has sha256 %x, want %s: capture it again with testdata/replays/capture.go "+
				"(ecu %s, recording sha256 %s), never by hand", f.file, sum, f.sha256, f.ecu, f.recording)
		}
	}
}

// AC1: writing the sessions and laps of real-nuc-suzuka-pit-stop-2026-10-15, as
// ecu's race state gives them, gives one Parquet file per lap and rows whose lap
// times and fuel are the events', which are v1's for the recording.
func TestARecordingGivesAFilePerLapAndRowsEqualToTheEvents(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	r := readReplay(t)
	if err := s.StartSession(t.Context(), r.started); err != nil {
		t.Fatal(err)
	}
	for _, lap := range r.laps {
		if err := s.WriteLap(t.Context(), r.src, lap, r.frames); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndSession(t.Context(), r.src, r.ended, r.endedAt); err != nil {
		t.Fatal(err)
	}

	laps, err := s.Laps(t.Context(), r.src)
	if err != nil {
		t.Fatal(err)
	}
	if len(laps) != len(v1SuzukaPitStop) || len(r.laps) != len(v1SuzukaPitStop) {
		t.Fatalf("%d laps stored from %d events, want v1's %d", len(laps), len(r.laps), len(v1SuzukaPitStop))
	}
	sid := r.started.GetSessionId()
	var want []string
	for i, got := range laps {
		ev, v1 := r.laps[i], v1SuzukaPitStop[i]
		file := fmt.Sprintf("sessions/%s/%s/lap-%03d.parquet", r.src, sid, v1.lap)
		want = append(want, filepath.FromSlash(file))
		if got.File != file {
			t.Errorf("lap %d file %q, want %q", v1.lap, got.File, file)
		}
		row := v1Lap{got.Lap, deref(got.LapTimeMs), got.FuelStart, got.FuelEnd, got.FuelUsed, deref(got.Position), got.Frames}
		fromEvent := v1Lap{ev.GetLap(), ev.GetLapTimeMs(), ev.GetFuelAtStart(), ev.GetFuelAtEnd(), ev.GetFuelUsed(),
			ev.GetPosition(), got.Frames}
		if row != fromEvent {
			t.Errorf("lap %d row %+v, want the event's %+v", v1.lap, row, fromEvent)
		}
		if row != v1 {
			t.Errorf("lap %d row %+v, want v1's %+v", v1.lap, row, v1)
		}
		if !got.Complete {
			t.Errorf("lap %d is not complete", v1.lap)
		}
		// The file holds what the row counts: the lap's racing frames.
		n := query(t, `SELECT count(*), min(seq), max(seq) FROM read_parquet(?)`, filepath.Join(dir, file))
		if want := []string{fmt.Sprint(got.Frames), fmt.Sprint(got.FirstSeq), fmt.Sprint(got.LastSeq)}; !slices.Equal(n[0], want) {
			t.Errorf("lap %d file holds %q (frames, first, last), the row says %q", v1.lap, n[0], want)
		}
	}
	onDisk := keys(files(t, dir))
	onDisk = slices.DeleteFunc(onDisk, func(f string) bool { return filepath.Ext(f) != ".parquet" })
	if !slices.Equal(onDisk, want) {
		t.Errorf("lap files %q, want one per lap %q", onDisk, want)
	}

	session := rowsOf(t, t.Context(), s.index, `SELECT src, id, type, car_code, total_laps, laps_completed,
fuel_at_start, fuel_used, final_position, end_reason FROM sessions`)
	wantSession := [][]string{{r.src, sid, "lap_race", "683", "6", "6", "96.72370910644531", "53.291", "1", "finished"}}
	if !slices.EqualFunc(session, wantSession, slices.Equal) {
		t.Errorf("sessions\n got %q\nwant %q", session, wantSession)
	}
}

func deref(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}
