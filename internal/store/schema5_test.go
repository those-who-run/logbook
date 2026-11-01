package store

import (
	"database/sql"
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

// schema5Sessions and schema5Laps are the columns migration 5 adds (the schema 5
// issue).
const (
	schema5Sessions = `summary, car_name, track_id, track_name, track_sure, posted_at`
	schema5Laps     = `pit_role, counts_for_best`
	// schema5Open are the columns an open session's events fill (paddock#337).
	schema5Open = `joined_on_lap, fuel_capacity, best_lap, paused_ms, gaps, pit_stops`
	// schema6More are migration 6's other columns: the kind and track whole, and
	// the sequences.
	schema6More = `kind, track, track_label, kind_seq, track_seq, best_seq`
	// schema7Sessions is migration 7's column: how an imported session was made
	// (paddock#264).
	schema7Sessions = `from_v1_rows`
	// schema5All is every sessions column migrations 5 to 7 add.
	schema5All = schema5Sessions + `, ` + schema5Open + `, ` + schema6More + `, ` + schema7Sessions
)

// withSummary gives sess the summary ecu sends from contracts v0.2: a car, a
// confirmed track, a pit stop and the laps, and the car's name at the start.
func withSummary(sess session) session {
	sess.started.CarName = "Porsche 911 GT3 RS (992) '22"
	sum := sess.ended.Summary
	sum.CarName = "Porsche 911 GT3 RS (992) '22"
	sum.CarCode = 3431
	sum.Track = &eventsv1.TrackVerdict{
		TrackId: proto.Int32(350), Name: "Suzuka Circuit", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED,
		Evidence: "line crossed 3 times",
	}
	sum.Kind = &eventsv1.SessionKind{SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Field: true,
		FuelUse: eventsv1.FuelUse_FUEL_USE_ON}
	sum.DrivingMs, sum.PausedMs = 296_000, 4_000
	sum.GameBestLapMs = proto.Int32(98_100)
	sum.PitStops = []*eventsv1.PitStop{{}}
	sum.Laps = sess.laps
	sess.laps[0].PitRole, sess.laps[0].CountsForBest = eventsv1.PitRole_PIT_ROLE_NONE, false
	sess.laps[1].PitRole, sess.laps[1].CountsForBest = eventsv1.PitRole_PIT_ROLE_IN_LAP, false
	sess.laps[2].PitRole = eventsv1.PitRole_PIT_ROLE_OUT_LAP
	return sess
}

// storedSummary is the session's summary column, parsed back.
func storedSummary(t *testing.T, s *Store, id string) *eventsv1.SessionSummary {
	t.Helper()
	got := rowsOf(t, t.Context(), s.index, `SELECT summary FROM sessions WHERE id = ?`, id)
	if len(got) != 1 || got[0][0] == "NULL" {
		t.Fatalf("summary %q, want one", got)
	}
	var sum eventsv1.SessionSummary
	if err := protojson.Unmarshal([]byte(got[0][0]), &sum); err != nil {
		t.Fatalf("summary %q: %v", got[0][0], err)
	}
	return &sum
}

// AC1: a session.ended keeps its summary whole, as protobuf JSON that parses back
// to the same SessionSummary, with the car's name and the track verdict in
// columns of their own. Ending it again keeps the first.
func TestStoresTheSummaryAndItsTrack(t *testing.T) {
	s := open(t, t.TempDir())
	sess := withSummary(race("live", sid))
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	// Open, it has the start's car name and no summary yet.
	got := rowsOf(t, t.Context(), s.index, `SELECT `+schema5Sessions+` FROM sessions`)
	want := [][]string{{"NULL", "Porsche 911 GT3 RS (992) '22", "NULL", "NULL", "NULL", "NULL"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("open: %s %q, want %q", schema5Sessions, got, want)
	}
	sess.write(t, s)
	if sum := storedSummary(t, s, sid); !proto.Equal(sum, sess.ended.GetSummary()) {
		t.Errorf("summary %v, want %v", sum, sess.ended.GetSummary())
	}
	got = rowsOf(t, t.Context(), s.index, `SELECT car_name, track_id, track_name, track_sure, posted_at FROM sessions`)
	want = [][]string{{"Porsche 911 GT3 RS (992) '22", "350", "Suzuka Circuit", "true", "NULL"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("car_name, track_id, track_name, track_sure, posted_at %q, want %q", got, want)
	}

	again := proto.Clone(sess.ended).(*eventsv1.SessionEnded)
	again.Summary.CarName = "another"
	again.Summary.Track = nil
	if err := s.EndSession(t.Context(), "live", again, sess.endedAt); err != nil {
		t.Fatal(err)
	}
	if after := rowsOf(t, t.Context(), s.index, `SELECT car_name, track_id, track_name, track_sure, posted_at FROM sessions`); !slices.EqualFunc(after, got, slices.Equal) {
		t.Errorf("ending again changed the columns: %q to %q", got, after)
	}
	if sum := storedSummary(t, s, sid); !proto.Equal(sum, sess.ended.GetSummary()) {
		t.Error("ending again changed the summary")
	}
}

// AC1: track_sure is a probable or confirmed verdict, as contracts' LiveTrack.sure
// reads it; an ambiguous one is not sure, and a summary with no track leaves the
// track columns NULL.
func TestTrackSureIsAProbableOrConfirmedVerdict(t *testing.T) {
	s := open(t, t.TempDir())
	cases := []struct {
		track *eventsv1.TrackVerdict
		want  []string
	}{
		{&eventsv1.TrackVerdict{TrackId: proto.Int32(1), Name: "a", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE},
			[]string{"1", "a", "true"}},
		{&eventsv1.TrackVerdict{TrackId: proto.Int32(2), Name: "b", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS},
			[]string{"2", "b", "false"}},
		{&eventsv1.TrackVerdict{Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE},
			[]string{"NULL", "NULL", "false"}},
		{&eventsv1.TrackVerdict{TrackId: proto.Int32(3), Name: "c"}, []string{"3", "c", "NULL"}},
		{nil, []string{"NULL", "NULL", "NULL"}},
	}
	var want [][]string
	for i, c := range cases {
		id := fmt.Sprintf("%s%d", sid[:25], i)
		sess := race("live", id)
		sess.ended.Summary.Track = c.track
		sess.write(t, s)
		want = append(want, append([]string{id}, c.want...))
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT id, track_id, track_name, track_sure FROM sessions ORDER BY id`)
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("id, track_id, track_name, track_sure %q, want %q", got, want)
	}
}

// AC2: a lap keeps its pit role, by name as the other enums, and whether it counts
// for the best; an event with no pit role, from before contracts had one, leaves
// both NULL.
func TestStoresTheLapsPitRole(t *testing.T) {
	s := open(t, t.TempDir())
	sess := withSummary(race("live", sid))
	sess.laps[0].CountsForBest = true
	sess.laps[2].PitRole = eventsv1.PitRole_PIT_ROLE_UNSPECIFIED
	sess.write(t, s)
	got := rowsOf(t, t.Context(), s.index, `SELECT lap, `+schema5Laps+` FROM laps ORDER BY lap`)
	want := [][]string{{"1", "none", "true"}, {"2", "in_lap", "false"}, {"3", "NULL", "NULL"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("lap, %s %q, want %q", schema5Laps, got, want)
	}
}

// AC3: card.posted sets posted_at once; another post keeps the first, and a card
// of a session the index does not hold is ErrUnknownSession.
func TestMarkPostedKeepsTheFirstPost(t *testing.T) {
	s := open(t, t.TempDir())
	race("live", sid).write(t, s)
	at := time.Date(2026, 10, 15, 19, 35, 2, 500e6, time.UTC)
	for _, when := range []time.Time{at, at.Add(time.Hour)} {
		if err := s.MarkPosted(t.Context(), "live", &eventsv1.CardPosted{SessionId: sid, PostedAt: timestamppb.New(when)}); err != nil {
			t.Fatal(err)
		}
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT posted_at FROM sessions`)
	if want := [][]string{{"2026-10-15T19:35:02.5Z"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("posted_at %q, want %q", got, want)
	}
	err := s.MarkPosted(t.Context(), "live", &eventsv1.CardPosted{SessionId: sid[:25] + "X", PostedAt: timestamppb.New(at)})
	if !errors.Is(err, ErrUnknownSession) {
		t.Errorf("MarkPosted of an unknown session: %v, want ErrUnknownSession", err)
	}
}

// before5 makes s's rows look as logbook 0.1.5 left them: the columns schema 5
// added NULL.
func before5(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.index.ExecContext(t.Context(), `
UPDATE sessions SET summary = NULL, car_name = NULL, track_id = NULL, track_name = NULL, track_sure = NULL, posted_at = NULL,
    joined_on_lap = NULL, fuel_capacity = NULL, best_lap = NULL, paused_ms = NULL, gaps = NULL, pit_stops = NULL,
    kind = NULL, track = NULL, track_label = NULL, kind_seq = NULL, track_seq = NULL, best_seq = NULL;
UPDATE laps SET pit_role = NULL, counts_for_best = NULL`); err != nil {
		t.Fatal(err)
	}
}

// AC5, the store's part: the backfill writes fill schema 5's columns on rows that
// exist, change no other column, skip what the index does not hold, and change
// nothing when run again.
func TestTheBackfillFillsOnlyTheNewColumnsOnce(t *testing.T) {
	s := open(t, t.TempDir())
	sess := withSummary(race("live", sid))
	sess.write(t, s)
	before5(t, s)
	const (
		others    = `SELECT * EXCLUDE (` + schema5All + `) FROM sessions ORDER BY src, id`
		otherLaps = `SELECT * EXCLUDE (` + schema5Laps + `) FROM laps ORDER BY src, session_id, lap`
	)
	wantOthers := rowsOf(t, t.Context(), s.index, others)
	wantLaps := rowsOf(t, t.Context(), s.index, otherLaps)
	card := &eventsv1.CardPosted{SessionId: sid, PostedAt: timestamppb.New(time.Date(2026, 10, 15, 19, 35, 2, 0, time.UTC))}
	unknown := sid[:25] + "X"

	for run, want := range []bool{true, false} {
		ctx := t.Context()
		did, err := s.BackfillSummary(ctx, "live", sess.ended)
		if err != nil || did != want {
			t.Errorf("run %d: BackfillSummary %v, %v, want %v", run+1, did, err, want)
		}
		for _, lap := range sess.laps {
			did, err := s.BackfillLap(ctx, "live", lap)
			if err != nil || did != want {
				t.Errorf("run %d: BackfillLap %d %v, %v, want %v", run+1, lap.GetLap(), did, err, want)
			}
		}
		did, err = s.BackfillPosted(ctx, "live", card)
		if err != nil || did != want {
			t.Errorf("run %d: BackfillPosted %v, %v, want %v", run+1, did, err, want)
		}
		// What the index does not hold is skipped.
		other := proto.Clone(sess.ended).(*eventsv1.SessionEnded)
		other.SessionId = unknown
		lap := proto.Clone(sess.laps[0]).(*eventsv1.LapCompleted)
		lap.Lap = 9
		if did, err := s.BackfillSummary(ctx, "live", other); did || err != nil {
			t.Errorf("run %d: BackfillSummary of an unknown session %v, %v", run+1, did, err)
		}
		if did, err := s.BackfillLap(ctx, "live", lap); did || err != nil {
			t.Errorf("run %d: BackfillLap of an unknown lap %v, %v", run+1, did, err)
		}
		if did, err := s.BackfillPosted(ctx, "live", &eventsv1.CardPosted{SessionId: unknown, PostedAt: card.GetPostedAt()}); did || err != nil {
			t.Errorf("run %d: BackfillPosted of an unknown session %v, %v", run+1, did, err)
		}

		if sum := storedSummary(t, s, sid); !proto.Equal(sum, sess.ended.GetSummary()) {
			t.Errorf("run %d: summary %v", run+1, sum)
		}
		got := rowsOf(t, ctx, s.index, `SELECT car_name, track_id, track_name, track_sure, posted_at FROM sessions`)
		want := [][]string{{"Porsche 911 GT3 RS (992) '22", "350", "Suzuka Circuit", "true", "2026-10-15T19:35:02Z"}}
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("run %d: %q, want %q", run+1, got, want)
		}
		got = rowsOf(t, ctx, s.index, `SELECT lap, `+schema5Laps+` FROM laps ORDER BY lap`)
		want = [][]string{{"1", "none", "false"}, {"2", "in_lap", "false"}, {"3", "out_lap", "false"}}
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("run %d: lap, %s %q, want %q", run+1, schema5Laps, got, want)
		}
		if got := rowsOf(t, ctx, s.index, others); !slices.EqualFunc(got, wantOthers, slices.Equal) {
			t.Errorf("run %d: the backfill changed other session columns: %q, want %q", run+1, got, wantOthers)
		}
		if got := rowsOf(t, ctx, s.index, otherLaps); !slices.EqualFunc(got, wantLaps, slices.Equal) {
			t.Errorf("run %d: the backfill changed other lap columns", run+1)
		}
	}
}

// AC4: an index at schema version 4 opens at the code's (5, then 6) with its rows unchanged and the new
// columns NULL, and a second open changes nothing.
func TestVersion5AddsNullColumns(t *testing.T) {
	dir := t.TempDir()
	withIndex(t, dir, func(db *sql.DB) {
		ctx := t.Context()
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for v := range 4 {
			if err := step(ctx, db, v); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
INSERT INTO sessions (src, id, started_at, type, car_code, joined_part_way, first_seq, end_reason)
VALUES ('live', 's', '2026-10-29 20:00:00', 'lap_race', 3431, false, 1, 'finished');
INSERT INTO laps (src, session_id, lap, lap_time_ms, complete, fuel_start, fuel_end, fuel_used, first_seq,
    last_seq, frames, path_m, max_jump_m, file, recorded_at)
VALUES ('live', 's', 1, 98000, true, 50, 49, 1, 1, 600, 600, 5000, 2, 'f', '2026-10-29 20:02:00')`); err != nil {
			t.Fatal(err)
		}
	})
	var before [2][][]string
	withIndex(t, dir, func(db *sql.DB) {
		before[0] = rowsOf(t, t.Context(), db, `SELECT * FROM sessions`)
		before[1] = rowsOf(t, t.Context(), db, `SELECT * FROM laps`)
	})
	for start := range 2 {
		s, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		withIndex(t, dir, func(db *sql.DB) {
			ctx := t.Context()
			if got := rowsOf(t, ctx, db, `SELECT version FROM schema_version`); got[0][0] != fmt.Sprint(SchemaVersion) {
				t.Errorf("start %d: schema version %s, want %d", start+1, got[0][0], SchemaVersion)
			}
			if got := rowsOf(t, ctx, db, `SELECT * EXCLUDE (`+schema5All+`) FROM sessions`); !slices.EqualFunc(got, before[0], slices.Equal) {
				t.Errorf("start %d: sessions %q, want %q", start+1, got, before[0])
			}
			if got := rowsOf(t, ctx, db, `SELECT * EXCLUDE (`+schema5Laps+`) FROM laps`); !slices.EqualFunc(got, before[1], slices.Equal) {
				t.Errorf("start %d: laps %q, want %q", start+1, got, before[1])
			}
			got := rowsOf(t, ctx, db, `SELECT `+schema5All+`, l.pit_role, l.counts_for_best FROM sessions s JOIN laps l ON l.session_id = s.id`)
			if want := [][]string{slices.Repeat([]string{"NULL"}, 21)}; !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("start %d: new columns %q, want NULL", start+1, got)
			}
		})
	}
}

// What a schema 5 write cannot use is refused or skipped, and changes nothing: a
// bad key, a card with no time, a lap with no pit role, an end with no summary.
func TestSchema5WritesRefuseWhatTheyCannotUse(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race("live", sid)
	sess.write(t, s)
	before5(t, s)
	ctx := t.Context()
	bad := "not-a-ulid"
	at := timestamppb.New(time.Date(2026, 10, 15, 19, 35, 2, 0, time.UTC))
	if err := s.MarkPosted(ctx, "live", &eventsv1.CardPosted{SessionId: bad, PostedAt: at}); err == nil {
		t.Error("MarkPosted took a bad session id")
	}
	if err := s.MarkPosted(ctx, "live", &eventsv1.CardPosted{SessionId: sid}); err == nil {
		t.Error("MarkPosted took a card with no posted_at")
	}
	if _, err := s.BackfillSummary(ctx, "live", &eventsv1.SessionEnded{SessionId: bad}); err == nil {
		t.Error("BackfillSummary took a bad session id")
	}
	if _, err := s.BackfillLap(ctx, "live", &eventsv1.LapCompleted{SessionId: bad, Lap: 1}); err == nil {
		t.Error("BackfillLap took a bad session id")
	}
	if _, err := s.BackfillPosted(ctx, "live", &eventsv1.CardPosted{SessionId: bad, PostedAt: at}); err == nil {
		t.Error("BackfillPosted took a bad session id")
	}
	noRole := proto.Clone(sess.laps[0]).(*eventsv1.LapCompleted)
	noRole.PitRole = eventsv1.PitRole_PIT_ROLE_UNSPECIFIED
	for name, f := range map[string]func() (bool, error){
		"an end with no summary": func() (bool, error) { return s.BackfillSummary(ctx, "live", &eventsv1.SessionEnded{SessionId: sid}) },
		"a lap with no pit role": func() (bool, error) { return s.BackfillLap(ctx, "live", noRole) },
		"a card with no time":    func() (bool, error) { return s.BackfillPosted(ctx, "live", &eventsv1.CardPosted{SessionId: sid}) },
	} {
		if did, err := f(); did || err != nil {
			t.Errorf("%s: %v, %v, want skipped", name, did, err)
		}
	}
	got := rowsOf(t, ctx, s.index, `SELECT `+schema5Sessions+` FROM sessions`)
	if want := [][]string{slices.Repeat([]string{"NULL"}, 6)}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%s %q, want NULL", schema5Sessions, got)
	}
}
