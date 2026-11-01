package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// factSeq is the EVENTS sequence apply gives its next event.
var factSeq uint64 = 100

// openFacts reads an open session's fact columns (paddock#337).
const openFacts = `SELECT joined_on_lap, fuel_capacity, session_kind, total_laps, track_id, track_name, track_sure,
    best_lap, best_lap_ms, paused_ms FROM sessions WHERE id = ?`

// apply applies evs in order, as EVENTS would deliver them: each at the next
// sequence.
func apply(t *testing.T, s *Store, evs ...proto.Message) {
	t.Helper()
	for _, ev := range evs {
		factSeq++
		if _, err := s.ApplyFact(t.Context(), "live", ev, factSeq); err != nil {
			t.Fatalf("%T: %v", ev, err)
		}
	}
}

// jsonList is a JSON column as a list of messages, nil when it is NULL.
func jsonList[M proto.Message](t *testing.T, s *Store, col, id string, mk func() M) []M {
	t.Helper()
	got := rowsOf(t, t.Context(), s.index, `SELECT `+col+`::VARCHAR FROM sessions WHERE id = ?`, id)
	if got[0][0] == "NULL" {
		return nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(got[0][0]), &raws); err != nil {
		t.Fatal(err)
	}
	var out []M
	for _, r := range raws {
		m := mk()
		if err := protojson.Unmarshal(r, m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// An open session's row keeps what its events say so far, each from its own
// event: the start's joined lap and tank, the last kind and track, ecu's best
// lap, the time paused, each gap and each pit stop. The end's summary then has
// the last word, and an event after the end changes nothing.
func TestAnOpenSessionKeepsWhatItsEventsSay(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	sess.started.JoinedOnLap = proto.Int32(2)
	sess.started.FuelCapacity = 100
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 19, 32, 0, 0, time.UTC)
	gap := &eventsv1.SessionGap{StartedAt: timestamppb.New(at), EndedAt: timestamppb.New(at.Add(time.Minute)), Lap: 2,
		Reason: eventsv1.GapReason_GAP_REASON_ENGINEER_RESTART}
	stop1 := &eventsv1.PitStop{Stop: 1, InLap: 1, FuelAdded: 20}
	apply(t, s,
		&eventsv1.SessionKindChanged{SessionId: sid, Kind: &eventsv1.SessionKind{
			SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(5)}},
		&eventsv1.SessionTrackChanged{SessionId: sid, Track: &eventsv1.TrackVerdict{
			TrackId: proto.Int32(7), Name: "Suzuka", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE}},
		// A continue's carried track does not replace the session's own.
		&eventsv1.SessionContinued{SessionId: sid, RunId: "r", Gap: gap, Track: &eventsv1.TrackVerdict{Name: "other"}},
		&eventsv1.SessionContinued{SessionId: sid, RunId: "r", Gap: gap}, // again: one gap
		&eventsv1.SessionPaused{SessionId: sid, PausedMs: 1000},
		&eventsv1.SessionResumed{SessionId: sid, PausedMs: 4000},
		&eventsv1.SessionPaused{SessionId: sid, PausedMs: 1000}, // a redelivery takes nothing back
		&eventsv1.LapBestChanged{SessionId: sid, Lap: 1, LapTimeMs: 97_000},
		&eventsv1.PitExited{SessionId: sid, Stop: &eventsv1.PitStop{Stop: 1, InLap: 1}},
		&eventsv1.PitExited{SessionId: sid, Stop: stop1}, // the same stop: in its place
		&eventsv1.PitExited{SessionId: sid, Stop: &eventsv1.PitStop{Stop: 2, InLap: 3}},
		&eventsv1.PitExited{SessionId: sid}, // no stop: nothing
	)
	// A lap written after ecu named its best keeps ecu's best.
	if err := s.WriteLap(t.Context(), "live", sess.laps[0], sess.frames); err != nil {
		t.Fatal(err)
	}
	got := rowsOf(t, t.Context(), s.index, openFacts, sid)
	want := [][]string{{"2", "100", "lap_race", "5", "7", "Suzuka", "true", "1", "97000", "4000"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("open: %q,\nwant %q", got, want)
	}
	gaps := jsonList(t, s, "gaps", sid, func() *eventsv1.SessionGap { return &eventsv1.SessionGap{} })
	if len(gaps) != 1 || !proto.Equal(gaps[0], gap) {
		t.Errorf("gaps %v, want [%v]", gaps, gap)
	}
	stops := jsonList(t, s, "pit_stops", sid, func() *eventsv1.PitStop { return &eventsv1.PitStop{} })
	if len(stops) != 2 || !proto.Equal(stops[0], stop1) || stops[1].GetStop() != 2 {
		t.Errorf("pit stops %v", stops)
	}

	// The end's summary has the last word.
	sum := sess.ended.Summary
	sum.JoinedOnLap, sum.FuelCapacity, sum.PausedMs = proto.Int32(2), 100, 5000
	sum.PitStops = []*eventsv1.PitStop{stop1}
	if err := s.EndSession(t.Context(), "live", sess.ended, sess.endedAt); err != nil {
		t.Fatal(err)
	}
	ended := [][]string{{"2", "100", "NULL", "5", "NULL", "NULL", "NULL", "1", "98100", "5000"}}
	if got := rowsOf(t, t.Context(), s.index, openFacts, sid); !slices.EqualFunc(got, ended, slices.Equal) {
		t.Errorf("ended: %q,\nwant %q", got, ended)
	}
	if gaps := jsonList(t, s, "gaps", sid, func() *eventsv1.SessionGap { return &eventsv1.SessionGap{} }); gaps != nil {
		t.Errorf("gaps %v after an end that lists none", gaps)
	}
	// After the end an event changes nothing.
	did, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: sid, Lap: 2, LapTimeMs: 1}, 1_000_000)
	if did || err != nil {
		t.Errorf("a fact after the end: %v, %v", did, err)
	}
	if got := rowsOf(t, t.Context(), s.index, openFacts, sid); !slices.EqualFunc(got, ended, slices.Equal) {
		t.Errorf("after the end: %q", got)
	}
}

// A continue's carried track is the session's while it has none of its own.
func TestACarriedTrackIsTakenWhileThereIsNone(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.StartSession(t.Context(), race("live", sid).started); err != nil {
		t.Fatal(err)
	}
	apply(t, s, &eventsv1.SessionContinued{SessionId: sid, RunId: "r", Track: &eventsv1.TrackVerdict{
		TrackId: proto.Int32(3), Name: "Fuji", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}})
	got := rowsOf(t, t.Context(), s.index, `SELECT track_id, track_name, track_sure, gaps FROM sessions`)
	if want := [][]string{{"3", "Fuji", "true", "NULL"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%q, want %q", got, want)
	}
}

// ApplyFact refuses an event of a session the index does not hold, a bad id, and
// a message that is not one of its events.
func TestApplyFactRefusesWhatItCannotUse(t *testing.T) {
	s := open(t, t.TempDir())
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: sid}, 1); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("an unknown session: %v, want ErrUnknownSession", err)
	}
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: "x"}, 1); err == nil {
		t.Error("a bad session id: no error")
	}
	if err := s.StartSession(t.Context(), race("live", sid).started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapCompleted{SessionId: sid}, 1); err == nil {
		t.Error("a lap.completed: no error")
	}
	if _, err := s.ApplyFact(t.Context(), "live", &timestamppb.Timestamp{}, 1); err == nil {
		t.Error("a message with no session: no error")
	}
}

// The backfill's start write fills joined_on_lap and fuel_capacity where they
// are NULL, and a second run changes nothing; an end's summary fills the open
// columns of a session ended before schema 5.
func TestTheBackfillFillsTheOpenColumns(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	sess.started.JoinedOnLap, sess.started.FuelCapacity = proto.Int32(4), 80
	sum := sess.ended.Summary
	sum.JoinedOnLap, sum.FuelCapacity, sum.PausedMs = proto.Int32(4), 80, 2500
	sum.Gaps = []*eventsv1.SessionGap{{Lap: 2}}
	sum.PitStops = []*eventsv1.PitStop{{Stop: 1, InLap: 2}}
	sess.write(t, s)
	before5(t, s)
	for run, want := range []bool{true, false} {
		did, err := s.BackfillStarted(t.Context(), "live", sess.started)
		if err != nil || did != want {
			t.Errorf("run %d: BackfillStarted %v, %v, want %v", run+1, did, err, want)
		}
		if did, err := s.BackfillSummary(t.Context(), "live", sess.ended); err != nil || did != want {
			t.Errorf("run %d: BackfillSummary %v, %v, want %v", run+1, did, err, want)
		}
	}
	if did, err := s.BackfillStarted(t.Context(), "live", race("live", sid).started); did || err != nil {
		t.Errorf("a start with neither: %v, %v", did, err)
	}
	if _, err := s.BackfillStarted(t.Context(), "live", &eventsv1.SessionStarted{SessionId: "x"}); err == nil {
		t.Error("a bad session id: no error")
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT `+schema5Open+` FROM sessions`)
	want := [][]string{{"4", "80", "1", "2500", `[{"lap":2}]`, `[{"inLap":2,"stop":1}]`}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%s %q,\nwant %q", schema5Open, got, want)
	}
}

// Schema 6 leaves migration 5 as it was: an index at 5 (logbook 0.1.7) opens at 6
// with its rows unchanged and the open-session columns NULL.
func TestVersion6AddsTheOpenSessionColumns(t *testing.T) {
	dir := t.TempDir()
	withIndex(t, dir, func(db *sql.DB) {
		ctx := t.Context()
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for v := range 5 {
			if err := step(ctx, db, v); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
INSERT INTO sessions (src, id, started_at, type, car_code, joined_part_way, first_seq, track_name)
VALUES ('live', 's', '2026-10-31 20:00:00', 'lap_race', 3431, false, 1, 'Suzuka')`); err != nil {
			t.Fatal(err)
		}
	})
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	withIndex(t, dir, func(db *sql.DB) {
		got := rowsOf(t, t.Context(), db, `SELECT (SELECT version FROM schema_version), track_name, `+schema5Open+` FROM sessions`)
		// At the code's version: 7 (paddock#264) leaves 6's columns as they were.
		if want := [][]string{{fmt.Sprint(SchemaVersion), "Suzuka", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("%q, want %q", got, want)
		}
	})
}

// A session that ended under logbook 0.1.7 has its summary but not schema 6's
// columns: they are filled from its stored summary, with no event, once. A
// session still open, or one already filled, is left alone.
func TestSchema6IsFilledFromTheStoredSummary(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	sum := sess.ended.Summary
	sum.FuelCapacity, sum.PausedMs = 90, 1500
	sum.PitStops = []*eventsv1.PitStop{{Stop: 1, InLap: 2}}
	sum.Kind = &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Field: true}
	sum.Track = &eventsv1.TrackVerdict{Name: "Fuji", Family: "Fuji", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE}
	sess.write(t, s)
	if err := s.StartSession(t.Context(), race("live", sid[:25]+"Z").started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.index.ExecContext(t.Context(), `UPDATE sessions SET `+
		`joined_on_lap = NULL, fuel_capacity = NULL, best_lap = NULL, paused_ms = NULL, gaps = NULL, pit_stops = NULL,
    kind = NULL, track = NULL, track_label = NULL`); err != nil {
		t.Fatal(err)
	}
	for run, want := range []int{1, 0} {
		if n, err := s.FillFromSummaries(t.Context()); err != nil || n != want {
			t.Errorf("run %d: %d, %v, want %d", run+1, n, err, want)
		}
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT `+schema5Open+`, kind IS NOT NULL, track IS NOT NULL, track_label
FROM sessions ORDER BY id`)
	want := [][]string{
		{"NULL", "90", "1", "1500", "NULL", `[{"inLap":2,"stop":1}]`, "true", "true", "Fuji"},
		slices.Concat(slices.Repeat([]string{"NULL"}, 6), []string{"false", "false", "NULL"}),
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%q,\nwant %q", got, want)
	}
}

// A session.started delivered again after a session.kind leaves the kind's
// total_laps.
func TestAStartAgainKeepsTheKindsLapTotal(t *testing.T) {
	s := open(t, t.TempDir())
	started := race("live", sid).started
	if err := s.StartSession(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	apply(t, s, &eventsv1.SessionKindChanged{SessionId: sid, Kind: &eventsv1.SessionKind{
		SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(10)}})
	if err := s.StartSession(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, t.Context(), s.index, `SELECT total_laps FROM sessions`); got[0][0] != "10" {
		t.Errorf("total_laps %s after the start again, want the kind's 10", got[0][0])
	}
}

// The kind and the track are kept whole as JSON, with the label the track filter
// keys on; the end's summary replaces them.
func TestTheKindAndTrackAreKeptWhole(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	kind := &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(5),
		Field: true, FuelUse: eventsv1.FuelUse_FUEL_USE_OFF, CarsOnTrack: true, Start: eventsv1.StartType_START_TYPE_GRID,
		Evidence: "a lap total and a field"}
	track := &eventsv1.TrackVerdict{TrackId: proto.Int32(7), Name: "Suzuka Circuit", Family: "Suzuka",
		Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS, Evidence: "two layouts", Carried: true}
	apply(t, s, &eventsv1.SessionKindChanged{SessionId: sid, Kind: kind},
		&eventsv1.SessionTrackChanged{SessionId: sid, Track: track})
	check := func(when string, kind *eventsv1.SessionKind, track *eventsv1.TrackVerdict, label string) {
		t.Helper()
		got := rowsOf(t, t.Context(), s.index, `SELECT kind::VARCHAR, track::VARCHAR, track_label FROM sessions`)
		var k eventsv1.SessionKind
		var v eventsv1.TrackVerdict
		if err := errors.Join(protojson.Unmarshal([]byte(got[0][0]), &k), protojson.Unmarshal([]byte(got[0][1]), &v)); err != nil {
			t.Fatalf("%s: %q: %v", when, got, err)
		}
		if !proto.Equal(&k, kind) || !proto.Equal(&v, track) || got[0][2] != label {
			t.Errorf("%s: kind %v, track %v, label %q; want %v, %v, %q", when, &k, &v, got[0][2], kind, track, label)
		}
	}
	// Not sure of the layout: the label is the circuit's family.
	check("open", kind, track, "Suzuka")
	sure := &eventsv1.TrackVerdict{TrackId: proto.Int32(8), Name: "Suzuka East", Family: "Suzuka",
		Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}
	endKind := &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS}
	sess.ended.Summary.Track, sess.ended.Summary.Kind = sure, endKind
	sess.write(t, s)
	check("ended", endKind, sure, "Suzuka East")
}

// The track, the kind and the best lap are the last event's: an older event
// delivered again after a newer one changes nothing.
func TestAnOlderEventDeliveredAgainChangesNothing(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.StartSession(t.Context(), race("live", sid).started); err != nil {
		t.Fatal(err)
	}
	at := func(seq uint64, ev proto.Message) {
		t.Helper()
		if _, err := s.ApplyFact(t.Context(), "live", ev, seq); err != nil {
			t.Fatal(err)
		}
	}
	track := func(name string) *eventsv1.SessionTrackChanged {
		return &eventsv1.SessionTrackChanged{SessionId: sid, Track: &eventsv1.TrackVerdict{Name: name,
			Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE}}
	}
	kind := func(laps int32) *eventsv1.SessionKindChanged {
		return &eventsv1.SessionKindChanged{SessionId: sid, Kind: &eventsv1.SessionKind{
			SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(laps)}}
	}
	best := func(lap int32) *eventsv1.LapBestChanged {
		return &eventsv1.LapBestChanged{SessionId: sid, Lap: lap, LapTimeMs: 90_000 + lap}
	}
	// In order, then the older ones again.
	at(10, track("old"))
	at(11, kind(3))
	at(12, best(1))
	at(20, track("new"))
	at(21, kind(5))
	at(22, best(2))
	at(10, track("old"))
	at(11, kind(3))
	at(12, best(1))
	// A continue's carried track does not replace one there is, whatever its sequence.
	at(30, &eventsv1.SessionContinued{SessionId: sid, RunId: "r", Track: &eventsv1.TrackVerdict{Name: "carried"}})
	got := rowsOf(t, t.Context(), s.index, `SELECT track_name, track_label, total_laps, best_lap, best_lap_ms,
    track_seq, kind_seq, best_seq FROM sessions`)
	if want := [][]string{{"new", "new", "5", "2", "90002", "20", "21", "22"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%q, want %q", got, want)
	}
}

// A message protobuf JSON cannot hold (a string that is not UTF-8, which only a
// message built in Go can carry) is refused, and changes nothing.
func TestAFactThatIsNotJSONIsRefused(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	bad := "\xff"
	for name, ev := range map[string]proto.Message{
		"a kind":          &eventsv1.SessionKindChanged{SessionId: sid, Kind: &eventsv1.SessionKind{Evidence: bad}},
		"a track":         &eventsv1.SessionTrackChanged{SessionId: sid, Track: &eventsv1.TrackVerdict{Name: bad}},
		"a carried track": &eventsv1.SessionContinued{SessionId: sid, Track: &eventsv1.TrackVerdict{Name: bad}},
		"a pit stop":      &eventsv1.PitExited{SessionId: sid, Stop: &eventsv1.PitStop{Evidence: bad}},
	} {
		if _, err := s.ApplyFact(t.Context(), "live", ev, 1); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for name, sum := range map[string]*eventsv1.SessionSummary{
		"a car name": {CarName: bad},
		"a pit stop": {PitStops: []*eventsv1.PitStop{{Evidence: bad}}},
		"a kind":     {Kind: &eventsv1.SessionKind{Evidence: bad}},
		"a track":    {Track: &eventsv1.TrackVerdict{Evidence: bad}},
	} {
		if _, err := summaryFields(sum); err == nil {
			t.Errorf("a summary with %s: no error", name)
		}
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT `+schema5All+` FROM sessions`)
	if want := [][]string{slices.Repeat([]string{"NULL"}, 19)}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%q, want NULL", got)
	}
}

// A stored summary that is not a SessionSummary stops the fill with an error.
func TestABadStoredSummaryIsAnError(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.StartSession(t.Context(), race("live", sid).started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.index.ExecContext(t.Context(), `UPDATE sessions SET summary = '{"lapsCompleted": "many"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FillFromSummaries(t.Context()); err == nil {
		t.Error("no error")
	}
}
