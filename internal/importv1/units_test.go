package importv1

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
)

// ADR 0009, point 4: anything but one session started and ended is not one to one.
func TestOneToOne(t *testing.T) {
	ended := &eventsv1.SessionEnded{}
	for _, c := range []struct {
		d    derived
		want string
	}{
		{derived{starts: 1, ended: ended}, ""},
		{derived{starts: 1, discarded: true}, "ecu discarded it as a GT7 replay or demo"},
		{derived{}, "ecu started no session from its frames"},
		{derived{starts: 2, ended: ended}, "ecu split it into 2 sessions"},
		{derived{starts: 1}, "ecu did not end it"},
	} {
		if got := c.d.oneToOne(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.d, got, c.want)
		}
	}
}

// The differences name each lap ecu and v1 disagree on beyond the tolerances.
func TestDifferences(t *testing.T) {
	d := &derived{
		ended: &eventsv1.SessionEnded{Summary: &eventsv1.SessionSummary{LapsCompleted: 2}},
		laps: []*eventsv1.LapCompleted{
			{Lap: 1, LapTimeMs: 60000, FuelUsed: 2.0},
			{Lap: 2, LapTimeMs: 61000, FuelUsed: 2.0015},
			{Lap: 4, LapTimeMs: 62000},
		},
	}
	sum := &v1SessionSummary{LapsCompleted: 3, Laps: []v1Lap{
		{Lap: 1, LapTimeMs: proto.Int32(60000), FuelUsed: ptr(2.0005)},
		{Lap: 2, LapTimeMs: proto.Int32(61001), FuelUsed: ptr(2.0)},
		{Lap: 3, LapTimeMs: nil},
	}}
	got := strings.Join(differences(d, sum), "; ")
	want := "complete laps 2, v1 3; lap 2: time 61000 ms, v1 61001 ms; lap 2: fuel used 2.002 L, v1 2.000 L; " +
		"lap 3: none, v1 has it; lap 4: v1 has none"
	if got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}

func ptr[T any](v T) *T { return &v }

// The checks independent of v1's lap times: the track and the finish, from the
// frames alone, and the kind, from the frames and v1's filled inputs. A 6-lap race with a field that comes out
// solo timed, at another track or none, unfinished, says so.
func TestDifferencesInWhatEcuJudgedAlone(t *testing.T) {
	d := &derived{ended: &eventsv1.SessionEnded{Summary: &eventsv1.SessionSummary{
		SessionType: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, Track: &eventsv1.TrackVerdict{TrackId: proto.Int32(7)},
	}}}
	v1 := &v1SessionSummary{Kind: &v1Kind{Kind: "lap_race"}, Track: &v1Track{ID: proto.Int32(442)}, Finished: true}
	want := "kind solo_timed, v1 lap_race; track 7, v1 442; finished false, v1 true"
	if got := strings.Join(differences(d, v1), "; "); got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
	d.ended.Summary = &eventsv1.SessionSummary{Kind: &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE}}
	if got := strings.Join(differences(d, v1), "; "); got != "track none, v1 442; finished false, v1 true" {
		t.Errorf("no track: %s", got)
	}
	d.ended.Summary.Track = &eventsv1.TrackVerdict{TrackId: proto.Int32(7)}
	if got := strings.Join(differences(d, &v1SessionSummary{}), "; "); got != "track 7, v1 none" {
		t.Errorf("v1 named none: %s", got)
	}
}

func TestNum(t *testing.T) {
	for _, c := range []struct {
		v    any
		want float64
	}{
		{1.5, 1.5}, {float32(2.5), 2.5}, {int64(3), 3}, {int32(4), 4}, {int16(5), 5}, {int8(6), 6},
		{7, 7}, {uint8(8), 8}, {nil, 0}, {"x", 0},
	} {
		if got := num(c.v); got != c.want {
			t.Errorf("num(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
}

// A session stored before v1 judged kinds takes v1's session type; v1's end is
// the flag, its own shutdown, or unknown.
func TestRowsTypesAndEnds(t *testing.T) {
	for typ, want := range map[string]eventsv1.SessionType{
		"race": eventsv1.SessionType_SESSION_TYPE_RACE, "time_trial": eventsv1.SessionType_SESSION_TYPE_TIME_TRIAL,
		"practice": eventsv1.SessionType_SESSION_TYPE_PRACTICE, "odd": eventsv1.SessionType_SESSION_TYPE_UNKNOWN,
	} {
		if got := sessionType(&v1SessionSummary{Type: typ, Kind: &v1Kind{Kind: "new"}}); got != want {
			t.Errorf("type %q: %v, want %v", typ, got, want)
		}
	}
	for _, c := range []struct {
		sum  v1SessionSummary
		want eventsv1.EndReason
	}{
		{v1SessionSummary{Finished: true}, eventsv1.EndReason_END_REASON_FINISHED},
		{v1SessionSummary{Shutdown: true}, eventsv1.EndReason_END_REASON_ENGINEER_STOPPED},
		{v1SessionSummary{}, eventsv1.EndReason_END_REASON_UNKNOWN},
	} {
		if got := rowsEndReason(&c.sum); got != c.want {
			t.Errorf("%+v: %v, want %v", c.sum, got, c.want)
		}
	}
	if f64(nil) != 0 || !contains([]string{"a"}, "a") || contains(nil, "a") {
		t.Error("helpers")
	}
}

// lapFrames gives a lap's seq range and the frame before it, or every frame when
// the seqs are not one per frame from 1.
func TestLapFrames(t *testing.T) {
	var frames []*telemetryv1.Frame
	for i := range 10 {
		frames = append(frames, &telemetryv1.Frame{Seq: uint32(i + 1)}) //nolint:gosec // small
	}
	if got := lapFrames(frames, &eventsv1.LapCompleted{FirstSeq: 4, LastSeq: 6}); len(got) != 4 || got[0].GetSeq() != 3 {
		t.Errorf("seqs 4 to 6: %v", got)
	}
	if got := lapFrames(frames, &eventsv1.LapCompleted{FirstSeq: 1, LastSeq: 2}); len(got) != 2 {
		t.Errorf("seqs 1 to 2: %v", got)
	}
	if got := lapFrames(frames, &eventsv1.LapCompleted{FirstSeq: 4, LastSeq: 20}); len(got) != 10 {
		t.Errorf("past the end: %d frames, want all", len(got))
	}
}

// failing is a store whose write number n fails.
type failing struct {
	n, calls int
}

var errStore = errors.New("store failed")

func (f *failing) fail() error {
	f.calls++
	if f.calls == f.n {
		return errStore
	}
	return nil
}

func (f *failing) StartSession(context.Context, *eventsv1.SessionStarted) error { return f.fail() }
func (f *failing) WriteLap(context.Context, string, *eventsv1.LapCompleted, []*telemetryv1.Frame) error {
	return f.fail()
}

func (f *failing) EndSession(context.Context, string, *eventsv1.SessionEnded, time.Time) error {
	return f.fail()
}
func (f *failing) MarkImported(context.Context, string, bool) error { return f.fail() }
func (f *failing) Imported(context.Context, string) (bool, bool, error) {
	if f.n == 0 {
		return false, false, errStore
	}
	return false, false, nil
}

// A store that fails stops the import with its error, whichever write fails.
func TestAStoreThatFailsStopsTheImport(t *testing.T) {
	from := v1Dir(t)
	for n := range 10 { // Imported, start, 6 laps, end, mark
		if _, err := Run(t.Context(), from, &failing{n: n}); !errors.Is(err, errStore) {
			t.Errorf("write %d failing: %v", n, err)
		}
	}
	// The same through the rows path.
	sum := summaryOf(t, from, fixtureID)
	for n := 1; n <= 9; n++ {
		if err := writeRows(t.Context(), &failing{n: n}, sum, nil, make([]int, len(sum.Laps))); !errors.Is(err, errStore) {
			t.Errorf("rows write %d failing: %v", n, err)
		}
	}
}

// A lap file that is not Parquet fails the session; the import goes on and says so.
func TestAnUnreadableLapFileFailsTheSession(t *testing.T) {
	from := v1Dir(t)
	if err := os.WriteFile(filepath.Join(from, v1Sessions, fixtureID, "lap-02.parquet"), []byte("not parquet"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(t.Context(), from, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.Sessions[0]; got.Outcome != Failed || !strings.HasPrefix(got.Reason, "lap-02.parquet: ") {
		t.Errorf("%+v, want failed on lap-02.parquet", got)
	}
	var out bytes.Buffer
	if err := (Summary{Sessions: []Session{
		{ID: "a", Outcome: Derived, Laps: 2, V1Laps: 3, Differences: []string{"complete laps 2, v1 3"}},
		{ID: "b", Outcome: FromRows, Laps: 1, V1Laps: 1, Reason: "ecu split it into 2 sessions"},
	}}).Write(&out); err != nil {
		t.Fatal(err)
	}
	want := "a re-derived: 2 laps (v1 3); " + derivedChecks + "\n  differs: complete laps 2, v1 3\n" +
		"b from v1's rows: 1 laps (v1 1): ecu split it into 2 sessions\n" +
		"v1 sessions: 2; re-derived: 1 (differing from v1: 1); from v1's rows: 1; left out: 0; already imported: 0; failed to read: 0\n"
	if out.String() != want {
		t.Errorf("%s\nwant\n%s", out.String(), want)
	}
}

// An import stopped part way returns what it did so far and why it stopped; an
// empty v1 directory imports nothing.
func TestAStoppedOrEmptyImport(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, v1Dir(t), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled import: %v", err)
	}
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, v1Index), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if sum, err := Run(t.Context(), empty, nil); err != nil || len(sum.Sessions) != 0 {
		t.Errorf("an empty directory: %+v %v", sum, err)
	}
}
