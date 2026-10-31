package runner

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/logbook/internal/store"
)

// postedAt is when podium posted the test race's card.
var postedAt = time.Date(2026, 10, 15, 19, 40, 3, 0, time.UTC)

// schema5Race is a race whose events carry what schema 5 keeps: a summary with a
// car and a confirmed track, each lap's pit role, and the card's post after the
// end.
func schema5Race(src, sid string) raceMessages {
	msgs := newRace(src, runA, sid).messages()
	roles := []eventsv1.PitRole{eventsv1.PitRole_PIT_ROLE_NONE, eventsv1.PitRole_PIT_ROLE_IN_LAP, eventsv1.PitRole_PIT_ROLE_OUT_LAP}
	for i, l := range msgs.laps {
		l.PitRole, l.CountsForBest = roles[i], i == 0
	}
	sum := msgs.ended.Summary
	sum.CarName = "Porsche 911 GT3 RS (992) '22"
	sum.Track = &eventsv1.TrackVerdict{TrackId: proto.Int32(350), Name: "Suzuka Circuit",
		Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}
	msgs.msgs = append(msgs.msgs, published{
		subject: "twr.v1." + src + ".evt." + sid + ".card.posted", id: src + ":" + sid + ":card.posted:1", at: postedAt,
		msg: &eventsv1.CardPosted{SessionId: sid, PostedAt: timestamppb.New(postedAt), MessageId: "1"},
	})
	return msgs
}

// indexRows runs q on dir's index, with logbook stopped.
func indexRows(t *testing.T, dir, q string) [][]string {
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
	return rows(t, db, q)
}

const (
	schema5Sessions = `SELECT car_name, track_id, track_name, track_sure, posted_at, summary IS NOT NULL FROM sessions ORDER BY id`
	schema5Laps     = `SELECT lap, pit_role, counts_for_best FROM laps ORDER BY lap`
)

var (
	wantSchema5Sessions = [][]string{{"Porsche 911 GT3 RS (992) '22", "350", "Suzuka Circuit", "true", "2026-10-15T19:40:03Z", "true"}}
	wantSchema5Laps     = [][]string{{"1", "none", "true"}, {"2", "in_lap", "false"}, {"3", "out_lap", "false"}}
)

// AC1 to AC3, as the subscriber stores them: the summary and its track from
// session.ended, each lap's pit role from its lap.completed, and posted_at from
// card.posted. A card of a session never started is dropped.
func TestTheSubscriberKeepsTheSummaryPitRolesAndCard(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	msgs := schema5Race("live", sidA)
	publish(t, js, msgs.msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	in.logs.await(t, 1, "card posted", map[string]any{"session": sidA})
	publish(t, js, []published{{subject: "twr.v1.live.evt." + sidB + ".card.posted", id: "live:" + sidB + ":card.posted:1",
		at: postedAt, msg: &eventsv1.CardPosted{SessionId: sidB, PostedAt: timestamppb.New(postedAt)}}}, false)
	in.logs.await(t, 1, "card of a session never started; dropped", map[string]any{"session": sidB})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if got := indexRows(t, dir, schema5Sessions); !slices.EqualFunc(got, wantSchema5Sessions, slices.Equal) {
		t.Errorf("sessions %q, want %q", got, wantSchema5Sessions)
	}
	if got := indexRows(t, dir, schema5Laps); !slices.EqualFunc(got, wantSchema5Laps, slices.Equal) {
		t.Errorf("laps %q, want %q", got, wantSchema5Laps)
	}
}

// AC5: on a store as logbook 0.1.5 left it, with the events still in EVENTS, the
// backfill fills schema 5's columns; a second run changes nothing; and an event of
// a session the store does not hold adds nothing.
func TestTheBackfillFillsWhatWasStoredBefore(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	msgs := schema5Race("live", sidA)
	publish(t, js, msgs.msgs, false)
	in.logs.await(t, 1, "card posted", map[string]any{"session": sidA})
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	// As 0.1.5 left it: the columns empty. And events of a session it never stored.
	other := newRace("live", runB, sidB).messages()
	var events []published
	for _, p := range other.msgs {
		if _, ok := p.msg.(*eventsv1.LapCompleted); ok || p.msg == other.ended {
			events = append(events, p)
		}
	}
	publish(t, js, events, false)
	conn, err := duckdb.NewConnector(filepath.Join(dir, "index.duckdb"), nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	_, err = db.ExecContext(t.Context(), `
UPDATE sessions SET summary = NULL, car_name = NULL, track_id = NULL, track_name = NULL, track_sure = NULL, posted_at = NULL;
UPDATE laps SET pit_role = NULL, counts_for_best = NULL`)
	if err := errors.Join(err, db.Close(), conn.Close()); err != nil {
		t.Fatal(err)
	}
	before := takeSnapshot(t, dir)

	for run, want := range []BackfillCounts{{Sessions: 1, Laps: 3, Cards: 1}, {}} {
		s, err := store.Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		logs := newLogs()
		got, err := Backfill(t.Context(), srv.ClientURL(), s, slog.New(slog.NewJSONHandler(logs, nil)))
		if cerr := s.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		// Every event of the three: the race's 3 laps, its end and its card, and the
		// other session's 3 laps and its end.
		want.Read = 9
		if got != want {
			t.Errorf("run %d: %+v, want %+v", run+1, got, want)
		}
		if got := indexRows(t, dir, schema5Sessions); !slices.EqualFunc(got, wantSchema5Sessions, slices.Equal) {
			t.Errorf("run %d: sessions %q, want %q", run+1, got, wantSchema5Sessions)
		}
		if got := indexRows(t, dir, schema5Laps); !slices.EqualFunc(got, wantSchema5Laps, slices.Equal) {
			t.Errorf("run %d: laps %q, want %q", run+1, got, wantSchema5Laps)
		}
		if d := before.diff(takeSnapshot(t, dir)); d != "" {
			t.Errorf("run %d: the backfill changed the store beyond schema 5's columns:\n%s", run+1, d)
		}
	}
}
