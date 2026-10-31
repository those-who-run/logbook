package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/proto"
)

// v1Trace is one of v1's lap traces, as testdata/traces/v1_lap_trace.py saved it
// from v1 1.0.0's lap_trace, run on the lap files this replay gives.
type v1Trace struct {
	T           []float64 `json:"t"`
	SpeedKph    []float64 `json:"speed_kph"`
	ThrottlePct []float64 `json:"throttle_pct"`
	BrakePct    []float64 `json:"brake_pct"`
	Gear        []int32   `json:"gear"`
	Rpm         []float64 `json:"rpm"`
	X           []float64 `json:"x"`
	Z           []float64 `json:"z"`
}

// tracedReplay writes the suzuka pit-stop replay's laps, as the v1 traces were
// made from, and returns the store and the session's id.
func tracedReplay(t *testing.T) (*Store, string, string) {
	t.Helper()
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
	return s, r.src, r.started.GetSessionId()
}

func trace(t *testing.T, s *Store, src, id string, lap, points int32) *storev1.LapTrace {
	t.Helper()
	got, err := s.GetTrace(t.Context(), &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: lap, Points: points})
	if err != nil {
		t.Fatal(err)
	}
	return got.GetTrace()
}

// AC1: every lap's trace equals v1 1.0.0's lap_trace for the same lap file, value
// for value, at 1500 points and thinned (40 and 10 points keep peaks, 3 takes
// every n-th frame).
func TestTracesEqualV1s(t *testing.T) {
	s, src, id := tracedReplay(t)
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "traces", "suzuka-pit-stop-lap-*.json"))
	if err != nil || len(found) != 6 {
		t.Fatalf("v1's traces: %q, %v; want one a lap", found, err)
	}
	for lap := int32(1); lap <= 6; lap++ {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "traces", fmt.Sprintf("suzuka-pit-stop-lap-%03d.json", lap)))
		if err != nil {
			t.Fatal(err)
		}
		var v1 map[string]v1Trace
		if err := json.Unmarshal(b, &v1); err != nil {
			t.Fatal(err)
		}
		for points, want := range v1 {
			n, _ := strconv.Atoi(points)
			got := trace(t, s, src, id, lap, int32(n)) //nolint:gosec // a few thousand at most
			if got.GetSessionId() != id || got.GetLap() != lap {
				t.Errorf("lap %d at %s points names %s lap %d", lap, points, got.GetSessionId(), got.GetLap())
			}
			for name, pair := range map[string][2][]float64{
				"t": {want.T, got.GetT()}, "speed_kph": {want.SpeedKph, got.GetSpeedKph()},
				"throttle_pct": {want.ThrottlePct, got.GetThrottlePct()}, "brake_pct": {want.BrakePct, got.GetBrakePct()},
				"rpm": {want.Rpm, got.GetRpm()}, "x": {want.X, got.GetX()}, "z": {want.Z, got.GetZ()},
			} {
				if !slices.Equal(pair[0], pair[1]) {
					t.Errorf("lap %d at %s points: %s\n got %v\nwant %v", lap, points, name, pair[1], pair[0])
				}
			}
			if !slices.Equal(want.Gear, got.GetGear()) {
				t.Errorf("lap %d at %s points: gear\n got %v\nwant %v", lap, points, got.GetGear(), want.Gear)
			}
		}
	}
}

// AC2: on the pit lap, the frames either side of the cut-scene's jump are kept,
// even thinned to 10 points, and they are over 20 m apart.
func TestTheJumpIsKept(t *testing.T) {
	s, src, id := tracedReplay(t)
	jumps := 0
	for lap := int32(1); lap <= 6; lap++ {
		tr := trace(t, s, src, id, lap, 10)
		for i := 1; i < len(tr.GetX()); i++ {
			if math.Hypot(tr.GetX()[i]-tr.GetX()[i-1], tr.GetZ()[i]-tr.GetZ()[i-1]) > jumpMetres {
				full := trace(t, s, src, id, lap, 0)
				j := slices.Index(full.GetT(), tr.GetT()[i])
				if j < 1 || full.GetT()[j-1] != tr.GetT()[i-1] {
					continue // a gap the thinning made, not a jump
				}
				jumps++
			}
		}
	}
	if jumps == 0 {
		t.Error("no jump of over 20 m between two neighbouring frames kept at 10 points")
	}
}

// AC3: at 10 points, the lap's top speed and its lowest speed are both kept.
func TestThinningKeepsThePeaks(t *testing.T) {
	s, src, id := tracedReplay(t)
	for lap := int32(1); lap <= 6; lap++ {
		full, thin := trace(t, s, src, id, lap, 0), trace(t, s, src, id, lap, 10)
		if len(thin.GetT()) >= len(full.GetT()) {
			t.Fatalf("lap %d: %d points at 10, of %d", lap, len(thin.GetT()), len(full.GetT()))
		}
		if slices.Max(thin.GetSpeedKph()) != slices.Max(full.GetSpeedKph()) ||
			slices.Min(thin.GetSpeedKph()) != slices.Min(full.GetSpeedKph()) {
			t.Errorf("lap %d at 10 points: speeds %v to %v, want the lap's %v to %v", lap,
				slices.Min(thin.GetSpeedKph()), slices.Max(thin.GetSpeedKph()),
				slices.Min(full.GetSpeedKph()), slices.Max(full.GetSpeedKph()))
		}
	}
}

// AC4, R6: a lap that does not exist, or whose file is gone, is not found, never
// an empty trace. AC5: two reads give the same version.
func TestTraceNotFoundAndItsVersion(t *testing.T) {
	s, src, id := tracedReplay(t)
	ctx := t.Context()
	for _, lap := range []int32{0, 7, 99} {
		_, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: lap})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("lap %d: %v, want not found", lap, err)
		}
	}
	a, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetDataVersion() != b.GetDataVersion() || a.GetDataVersion() == "" {
		t.Errorf("two reads: versions %q and %q", a.GetDataVersion(), b.GetDataVersion())
	}
	file := filepath.Join(s.dir, "sessions", src, id, "lap-002.parquet")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: 2}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a lap whose file is gone: %v, want not found", err)
	}
}

// Rounding is Python's: the nearest decimal of the binary value, ties to even.
func TestRoundIsPythons(t *testing.T) {
	for _, c := range []struct{ v, want float64 }{
		{2.675, 2.67}, {0.125, 0.12}, {0.375, 0.38}, {-1.25, -1.2}, {1.0005, 1}, {math.Inf(1), math.Inf(1)},
	} {
		digits := 2
		if c.v == 1.0005 || c.v == -1.25 {
			digits = 1
			if c.v == 1.0005 {
				digits = 3
			}
		}
		if got := round(c.v, digits); got != c.want {
			t.Errorf("round(%v, %d) = %v, want %v", c.v, digits, got, c.want)
		}
	}
}

// R7: a trace's version is its lap's: another lap written leaves it, the lap
// written again with other frames changes it, and a caller that has it is told
// unchanged, with no trace.
func TestATraceVersionIsItsLaps(t *testing.T) {
	s, src, id := tracedReplay(t)
	ctx := t.Context()
	version := func(lap int32) string {
		t.Helper()
		got, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: lap})
		if err != nil {
			t.Fatal(err)
		}
		return got.GetDataVersion()
	}
	two, three := version(2), version(3)
	if two == three || two == "" {
		t.Errorf("laps 2 and 3: versions %q and %q", two, three)
	}
	r := readReplay(t)
	if err := s.WriteLap(ctx, src, r.laps[2], r.frames); err != nil { // lap 3 again, unchanged
		t.Fatal(err)
	}
	if got := version(2); got != two {
		t.Errorf("lap 2 after lap 3 was written again: %q, was %q", got, two)
	}
	shorter := proto.Clone(r.laps[1]).(*eventsv1.LapCompleted) //nolint:forcetypeassert // a clone of a LapCompleted
	shorter.LastSeq--
	if err := s.WriteLap(ctx, src, shorter, r.frames); err != nil {
		t.Fatal(err)
	}
	now := version(2)
	if now == two {
		t.Errorf("lap 2 written again with a frame less kept its version %q", two)
	}
	got, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: []string{src}, SessionId: id, Lap: 2, KnownDataVersion: proto.String(now)})
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetUnchanged() || got.GetTrace() != nil || got.GetDataVersion() != now {
		t.Errorf("with its current version: %v", got)
	}
}
