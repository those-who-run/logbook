package importv1

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"

	"github.com/those-who-run/logbook/internal/store"
)

// fixtureID is the one session of testdata/v1, v1.0.0's store of the Suzuka pit
// stop recording (testdata/v1/README.md).
const fixtureID = "20261015-180014"

// v1Dir copies testdata/v1 to a temporary directory, so a test can change it.
func v1Dir(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "v1")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own fixture
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o600) //nolint:gosec // a copy of the test's own fixture
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func summaryOf(t *testing.T, dir, id string) *v1SessionSummary {
	t.Helper()
	sum, err := readSummary(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// lapsOf is the stored laps of a v1-import session.
func lapsOf(t *testing.T, st *store.Store, id string) []*storev1.LapRow {
	t.Helper()
	resp, err := st.ListLaps(t.Context(), &storev1.ListLapsRequest{Sources: []string{store.SourceV1Import}, SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetLaps()
}

func sessionOf(t *testing.T, st *store.Store, id string) *storev1.SessionRow {
	t.Helper()
	resp, err := st.GetSession(t.Context(), &storev1.GetSessionRequest{Sources: []string{store.SourceV1Import}, SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetSession()
}

// AC1: the Suzuka pit stop, as v1 stored it, comes in as one session under v1's
// id, re-derived by ecu, and its laps equal v1's rows within parity's tolerances:
// lap times exactly, fuel to 0.001 L, and v1's pit roles.
func TestImportGivesOneSessionEqualToV1sRows(t *testing.T) {
	from, data := v1Dir(t), t.TempDir()
	st := openStore(t, data)
	sum, err := Run(t.Context(), from, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Sessions) != 1 {
		t.Fatalf("%d sessions in the summary, want 1: %+v", len(sum.Sessions), sum.Sessions)
	}
	got := sum.Sessions[0]
	if got.ID != fixtureID || got.Outcome != Derived || len(got.Differences) > 0 {
		t.Fatalf("summary %+v, want %s re-derived with no differences", got, fixtureID)
	}
	row := sessionOf(t, st, fixtureID)
	if row.GetSource() != store.SourceV1Import || !row.GetClosed() || row.FromV1Rows == nil || row.GetFromV1Rows() {
		t.Errorf("session row %v: want v1-import, closed, from_v1_rows false", row)
	}
	if row.GetEndReason() != eventsv1.EndReason_END_REASON_FINISHED {
		t.Errorf("end reason %v, want finished", row.GetEndReason())
	}
	if row.GetSummary().GetTrack().GetName() != "Suzuka Circuit East Course" || len(row.GetSummary().GetPitStops()) != 1 {
		t.Errorf("summary track %q and %d pit stops, want Suzuka East and 1",
			row.GetSummary().GetTrack().GetName(), len(row.GetSummary().GetPitStops()))
	}
	v1 := summaryOf(t, from, fixtureID)
	laps := lapsOf(t, st, fixtureID)
	if len(laps) != len(v1.Laps) {
		t.Fatalf("%d laps stored, v1 has %d", len(laps), len(v1.Laps))
	}
	roles := map[string]eventsv1.PitRole{"in": eventsv1.PitRole_PIT_ROLE_IN_LAP, "out": eventsv1.PitRole_PIT_ROLE_OUT_LAP}
	for i, want := range v1.Laps {
		lap := laps[i].GetLap()
		if lap.GetLap() != want.Lap || lap.GetLapTimeMs() != *want.LapTimeMs || lap.GetComplete() != want.Complete {
			t.Errorf("lap %d: %d ms complete %v, v1 lap %d %d ms complete %v", lap.GetLap(), lap.GetLapTimeMs(),
				lap.GetComplete(), want.Lap, *want.LapTimeMs, want.Complete)
		}
		if math.Abs(lap.GetFuelUsed()-*want.FuelUsed) > fuelTolerance+1e-9 {
			t.Errorf("lap %d: fuel used %.3f L, v1 %.3f L", lap.GetLap(), lap.GetFuelUsed(), *want.FuelUsed)
		}
		role := eventsv1.PitRole_PIT_ROLE_NONE
		if want.Pit != nil {
			role = roles[*want.Pit]
		}
		if lap.GetPitRole() != role {
			t.Errorf("lap %d: pit role %v, v1's %v", lap.GetLap(), lap.GetPitRole(), role)
		}
		if laps[i].GetFrames() == 0 {
			t.Errorf("lap %d: no frames stored", lap.GetLap())
		}
	}
}

// fileSums are the sha256 of every file under dir but the index and the lock.
func fileSums(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(filepath.Join(dir, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // the test's own store
		out[path] = sha256.Sum256(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// AC2: a second run finds the session by source and id and changes nothing:
// the same rows, the same lap files, untouched.
func TestASecondImportChangesNothing(t *testing.T) {
	from, data := v1Dir(t), t.TempDir()
	st := openStore(t, data)
	if _, err := Run(t.Context(), from, st); err != nil {
		t.Fatal(err)
	}
	row, laps, sums := sessionOf(t, st, fixtureID), lapsOf(t, st, fixtureID), fileSums(t, data)
	stat := map[string]time.Time{}
	for path := range sums {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat[path] = info.ModTime()
	}
	sum, err := Run(t.Context(), from, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.Sessions[0]; got.Outcome != Already {
		t.Fatalf("second run: %+v, want already imported", got)
	}
	if again := sessionOf(t, st, fixtureID); fmt.Sprint(again) != fmt.Sprint(row) {
		t.Errorf("the session changed:\n%v\n%v", row, again)
	}
	if again := lapsOf(t, st, fixtureID); fmt.Sprint(again) != fmt.Sprint(laps) {
		t.Errorf("the laps changed:\n%v\n%v", laps, again)
	}
	for path, want := range sums {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fileSums(t, data)[path] != want || !info.ModTime().Equal(stat[path]) {
			t.Errorf("%s was written again", path)
		}
	}
}

// copySession copies the fixture's session to a new v1 id, so a directory holds
// two.
func copySession(t *testing.T, from, id string) {
	t.Helper()
	src, dst := filepath.Join(from, v1Sessions, fixtureID), filepath.Join(from, v1Sessions, id)
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // the test's own copy
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == v1Summary {
			data = bytes.Replace(data, []byte(`"`+fixtureID+`"`), []byte(`"`+id+`"`), 1)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil { //nolint:gosec // the test's own copy
			t.Fatal(err)
		}
	}
}

// AC3: a session with a lap file missing is left out, with the file named, and
// the rest import.
func TestASessionMissingALapFileIsLeftOut(t *testing.T) {
	from, data := v1Dir(t), t.TempDir()
	const broken = "20261015-190000"
	copySession(t, from, broken)
	if err := os.Remove(filepath.Join(from, v1Sessions, broken, "lap-03.parquet")); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, data)
	sum, err := Run(t.Context(), from, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Sessions) != 2 || sum.Sessions[0].Outcome != Derived {
		t.Fatalf("summary %+v: want the fixture re-derived and the copy left out", sum.Sessions)
	}
	if got := sum.Sessions[1]; got.ID != broken || got.Outcome != LeftOut || got.Reason != "lap-03.parquet is missing" {
		t.Errorf("%+v, want %s left out: lap-03.parquet is missing", got, broken)
	}
	if _, err := st.GetSession(t.Context(), &storev1.GetSessionRequest{Sources: []string{store.SourceV1Import},
		SessionId: broken}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the left-out session is in the store: %v", err)
	}
	var out bytes.Buffer
	if err := sum.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), broken+" left out: lap-03.parquet is missing\n") ||
		!strings.Contains(out.String(), "v1 sessions: 2; re-derived: 1 (differing from v1: 0); from v1's rows: 0; left out: 1;") {
		t.Errorf("summary:\n%s", out.String())
	}
}

// AC4, the index: a v1 index with a -wal newer than itself is still being
// written, and the import refuses it before reading anything.
func TestTheImportRefusesALiveV1Index(t *testing.T) {
	from := v1Dir(t)
	wal := filepath.Join(from, v1Index+"-wal")
	if err := os.WriteFile(wal, []byte("pages"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(wal, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), from, nil); !errors.Is(err, ErrLive) {
		t.Fatalf("Run: %v, want ErrLive", err)
	}
	// A -wal older than the index, left by a checkpointed close, is no sign of
	// a writer.
	earlier := time.Now().Add(-time.Hour)
	if err := os.Chtimes(wal, earlier, earlier); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), from, nil); err != nil {
		t.Fatalf("Run with an old -wal: %v", err)
	}
}

func TestADirectoryWithNoIndexIsNotV1s(t *testing.T) {
	if _, err := Run(t.Context(), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "not v1's data directory") {
		t.Fatalf("Run: %v", err)
	}
}

// A session whose lap files lack a column ecu needs cannot be judged again, and
// is taken from v1's rows, flagged, with v1's pit and out-lap flags (ADR 0009,
// point 5; the maintainer's decision, 2026-10-31).
func TestASessionWhoseFramesLackFieldsComesFromV1sRows(t *testing.T) {
	from, data := v1Dir(t), t.TempDir()
	lap := filepath.Join(from, v1Sessions, fixtureID, "lap-02.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tmp := filepath.Join(t.TempDir(), "lap.parquet")
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf(`COPY (SELECT * EXCLUDE (pos_x) FROM read_parquet('%s')) TO '%s' (FORMAT PARQUET)`,
		lap, tmp)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, lap); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, data)
	sum, err := Run(t.Context(), from, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.Sessions[0]; got.Outcome != FromRows || got.Reason != "its lap files lack pos_x" || got.Laps != 6 {
		t.Fatalf("%+v, want from v1's rows: its lap files lack pos_x, 6 laps", got)
	}
	row := sessionOf(t, st, fixtureID)
	if !row.GetFromV1Rows() || row.GetEndReason() != eventsv1.EndReason_END_REASON_FINISHED {
		t.Errorf("row %v: want from_v1_rows, finished", row)
	}
	s := row.GetSummary()
	if s.GetSessionType() != eventsv1.SessionType_SESSION_TYPE_LAP_RACE || s.GetBestLap() != 5 || s.GetBestLapTimeMs() != 59541 ||
		s.GetTrack().GetName() != "Suzuka Circuit East Course" || len(s.GetPitStops()) != 1 || s.GetFinishingPosition() != 1 {
		t.Errorf("summary %v: want v1's lap race, best lap 5 in 59541 ms, Suzuka East, 1 stop, P1", s)
	}
	laps := lapsOf(t, st, fixtureID)
	want := []eventsv1.PitRole{eventsv1.PitRole_PIT_ROLE_NONE, eventsv1.PitRole_PIT_ROLE_NONE, eventsv1.PitRole_PIT_ROLE_IN_LAP,
		eventsv1.PitRole_PIT_ROLE_OUT_LAP, eventsv1.PitRole_PIT_ROLE_NONE, eventsv1.PitRole_PIT_ROLE_NONE}
	for i, l := range laps {
		if l.GetLap().GetPitRole() != want[i] || l.GetFrames() == 0 {
			t.Errorf("lap %d: pit role %v, %d frames; want %v and its frames", i+1, l.GetLap().GetPitRole(), l.GetFrames(), want[i])
		}
	}
}

// R6: --dry-run's summary is the import's, and nothing is written.
func TestADryRunWritesNothing(t *testing.T) {
	from := v1Dir(t)
	sum, err := Run(t.Context(), from, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Sessions) != 1 || sum.Sessions[0].Outcome != Derived {
		t.Fatalf("summary %+v", sum.Sessions)
	}
}

// A summary.json that cannot be read fails the session, and the import goes on.
func TestAnUnreadableSummaryFailsTheSession(t *testing.T) {
	from := v1Dir(t)
	copySession(t, from, "20261015-190000")
	if err := os.WriteFile(filepath.Join(from, v1Sessions, "20261015-190000", v1Summary), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(from, v1Sessions, "20261015-200000"), 0o750); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(t.Context(), from, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Sessions) != 3 || sum.Count(Derived) != 1 || sum.Count(Failed) != 1 || sum.Count(LeftOut) != 1 {
		t.Fatalf("summary %+v", sum.Sessions)
	}
	if got := sum.Sessions[2]; got.Reason != "no summary.json: v1 never ended it" {
		t.Errorf("%+v", got)
	}
}

// The frames made whole carry GT7's lap times as it sent them: the last lap's and
// the best so far, -1 before the first.
func TestGameTimes(t *testing.T) {
	var sum v1SessionSummary
	if err := json.Unmarshal([]byte(`{"laps":[{"lap":1,"lap_time_ms":61000,"complete":true},
		{"lap":2,"lap_time_ms":60000,"complete":true},{"lap":3,"lap_time_ms":62000,"complete":true},
		{"lap":4,"lap_time_ms":null,"complete":false}]}`), &sum); err != nil {
		t.Fatal(err)
	}
	f := factsOf(&sum)
	for lap, want := range map[int32][2]int32{1: {-1, -1}, 2: {61000, 61000}, 3: {60000, 60000}, 4: {62000, 60000}, 5: {62000, 60000}} {
		if last, best := f.gameTimes(lap); last != want[0] || best != want[1] {
			t.Errorf("lap %d: last %d best %d, want %v", lap, last, best, want)
		}
	}
}
