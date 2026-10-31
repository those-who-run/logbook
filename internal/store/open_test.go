package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// openFacts reads an open session's fact columns (paddock#337).
const openFacts = `SELECT joined_on_lap, fuel_capacity, session_kind, total_laps, track_id, track_name, track_sure,
    best_lap, best_lap_ms, paused_ms FROM sessions WHERE id = ?`

func apply(t *testing.T, s *Store, evs ...proto.Message) {
	t.Helper()
	for _, ev := range evs {
		if _, err := s.ApplyFact(t.Context(), "live", ev); err != nil {
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
	did, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: sid, Lap: 2, LapTimeMs: 1})
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
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: sid}); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("an unknown session: %v, want ErrUnknownSession", err)
	}
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapBestChanged{SessionId: "x"}); err == nil {
		t.Error("a bad session id: no error")
	}
	if err := s.StartSession(t.Context(), race("live", sid).started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyFact(t.Context(), "live", &eventsv1.LapCompleted{SessionId: sid}); err == nil {
		t.Error("a lap.completed: no error")
	}
	if _, err := s.ApplyFact(t.Context(), "live", &timestamppb.Timestamp{}); err == nil {
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
		if want := [][]string{{"6", "Suzuka", "NULL", "NULL", "NULL", "NULL", "NULL", "NULL"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("%q, want %q", got, want)
		}
	})
}
