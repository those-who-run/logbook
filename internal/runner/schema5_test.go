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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
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
	// An end with no summary (ecu always sends one) is dropped, not applied: the
	// session stays open. So are a card with no time, and one that cannot be read.
	publish(t, js, []published{{subject: "twr.v1.live.evt." + sidA + ".session.ended", id: "live:" + sidA + ":session.ended:9",
		at: postedAt, msg: &eventsv1.SessionEnded{SessionId: sidA}}}, false)
	publish(t, js, []published{{subject: "twr.v1.live.evt." + sidA + ".card.posted", id: "live:" + sidA + ":card.posted:2",
		at: postedAt, msg: &eventsv1.CardPosted{SessionId: sidA}}}, false)
	h, _, err := ce.Encode(&eventsv1.CardPosted{}, ce.Event{ID: "broken", Source: "/twr/podium", Time: postedAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.PublishMsg(t.Context(), &nats.Msg{Subject: "twr.v1.live.evt." + sidA + ".card.posted",
		Header: nats.Header(h), Data: []byte{0xff, 0xff}}); err != nil {
		t.Fatal(err)
	}
	in.logs.await(t, 3, "event dropped", nil)
	if n := len(in.logs.find("event dropped", map[string]any{"err": "no summary"})); n != 1 {
		t.Errorf("%d ends dropped for no summary, want 1:\n%s", n, in.logs.String())
	}
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
	// And events it cannot read: a bad session id of each type, and a broken one.
	at := time.Date(2026, 10, 15, 19, 30, 0, 0, time.UTC)
	events = append(events,
		published{subject: "twr.v1.live.evt.x.session.ended", id: "x:session.ended:1", at: at,
			msg: &eventsv1.SessionEnded{SessionId: "not-a-ulid"}},
		published{subject: "twr.v1.live.evt.x.lap.completed", id: "x:lap.completed:1", at: at,
			msg: &eventsv1.LapCompleted{SessionId: "not-a-ulid", Lap: 1}},
		published{subject: "twr.v1.live.evt.x.card.posted", id: "x:card.posted:1", at: at,
			msg: &eventsv1.CardPosted{SessionId: "not-a-ulid"}})
	publish(t, js, events, false)
	for what, m := range map[string]proto.Message{"session.ended": &eventsv1.SessionEnded{},
		"lap.completed": &eventsv1.LapCompleted{}, "card.posted": &eventsv1.CardPosted{}} {
		h, _, err := ce.Encode(m, ce.Event{ID: "broken:" + what, Source: "/twr/ecu/gt7", Time: at})
		if err != nil {
			t.Fatal(err)
		}
		msg := &nats.Msg{Subject: "twr.v1.live.evt." + sidB + "." + what, Header: nats.Header(h), Data: []byte{0xff, 0xff}}
		if _, err := js.PublishMsg(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}
	as015(t, dir)
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
		// Every event it reads: the race's start, 3 laps, end and card, the other
		// session's 3 laps and its end, and the 6 it cannot read.
		want.Read = 16
		if n := len(logs.find("backfill: event skipped", nil)); n != 6 {
			t.Errorf("run %d: %d events skipped, want 6:\n%s", run+1, n, logs.String())
		}
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

// On an empty EVENTS the backfill reads nothing and changes nothing; without the
// stream it fails.
func TestTheBackfillOfAnEmptyStream(t *testing.T) {
	srv, _ := testServer(t)
	s, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	logger := slog.New(slog.NewJSONHandler(newLogs(), nil))
	if got, err := Backfill(t.Context(), srv.ClientURL(), s, logger); err != nil || got != (BackfillCounts{}) {
		t.Errorf("%+v, %v, want nothing", got, err)
	}
	bare := startNATS(t, 0)
	if _, err := Backfill(t.Context(), bare.ClientURL(), s, logger); err == nil {
		t.Error("a server with no EVENTS stream did not fail the backfill")
	}
}

// as015 empties the columns schema 5 added in dir's index, as logbook 0.1.5 left
// them.
func as015(t *testing.T, dir string) {
	t.Helper()
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
}

// slowConsumer is a consumer whose first empty fetches come back with nothing,
// as a fetch that times out on a loaded host does.
type slowConsumer struct {
	jetstream.Consumer
	empty int
}

func (c *slowConsumer) Fetch(n int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if c.empty > 0 {
		c.empty--
		return emptyBatch{}, nil
	}
	return c.Consumer.Fetch(n, opts...)
}

type emptyBatch struct{}

func (emptyBatch) Messages() <-chan jetstream.Msg {
	ch := make(chan jetstream.Msg)
	close(ch)
	return ch
}

func (emptyBatch) Error() error { return nil }

// An empty fetch with events still pending does not end the backfill: it fetches
// again until it has read them all.
func TestTheBackfillGoesOnPastAnEmptyFetch(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	publish(t, js, schema5Race("live", sidA).msgs, false)
	in.logs.await(t, 1, "card posted", map[string]any{"session": sidA})
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	as015(t, dir)
	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := newLogs()
	got, err := backfill(t.Context(), srv.ClientURL(), s, slog.New(slog.NewJSONHandler(logs, nil)),
		func(c jetstream.Consumer) jetstream.Consumer { return &slowConsumer{Consumer: c, empty: 3} })
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if want := (BackfillCounts{Read: 6, Sessions: 1, Laps: 3, Cards: 1}); got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
	if n := len(logs.find("backfill: a fetch came back empty with events pending; fetching again", nil)); n != 3 {
		t.Errorf("%d empty fetches logged, want 3:\n%s", n, logs.String())
	}
	done := logs.find("backfill done", nil)
	if len(done) != 1 || done[0]["last"] != done[0]["last_read"] {
		t.Errorf("done %v, want the last sequence read to be the stream's last", done)
	}
	if got := indexRows(t, dir, schema5Sessions); !slices.EqualFunc(got, wantSchema5Sessions, slices.Equal) {
		t.Errorf("sessions %q, want %q", got, wantSchema5Sessions)
	}
}

// An open session's row keeps what its events say so far, as the subscriber reads
// them (paddock#337): the start's joined lap and tank, the kind, the track, ecu's
// best lap, the time paused, the gap of a continue and each pit stop. The
// backfill applies the same events again to the same end.
func TestAnOpenSessionKeepsItsFacts(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	msgs := newRace("live", runA, sidA).messages()
	msgs.started.JoinedOnLap, msgs.started.FuelCapacity = proto.Int32(1), 100
	// The race without its end: the session stays open.
	race := msgs.msgs[:len(msgs.msgs)-1]
	at := time.Date(2026, 10, 15, 19, 35, 0, 0, time.UTC)
	fact := func(what string, m proto.Message) published {
		return published{subject: "twr.v1.live.evt." + sidA + "." + what, id: sidA + ":" + what + ":1", at: at, msg: m}
	}
	facts := []published{
		fact("session.kind", &eventsv1.SessionKindChanged{SessionId: sidA, Kind: &eventsv1.SessionKind{
			SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(3)}}),
		fact("session.track", &eventsv1.SessionTrackChanged{SessionId: sidA, Track: &eventsv1.TrackVerdict{
			TrackId: proto.Int32(350), Name: "Suzuka Circuit", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}}),
		fact("session.paused", &eventsv1.SessionPaused{SessionId: sidA, PausedMs: 0}),
		fact("session.resumed", &eventsv1.SessionResumed{SessionId: sidA, PauseMs: 2500, PausedMs: 2500}),
		fact("lap.best", &eventsv1.LapBestChanged{SessionId: sidA, Lap: 1, LapTimeMs: 98_100, EndedLap: 1}),
		fact("pit.exited", &eventsv1.PitExited{SessionId: sidA, Stop: &eventsv1.PitStop{Stop: 1, InLap: 1}}),
		// Of a session never started: dropped.
		{subject: "twr.v1.live.evt." + sidB + ".lap.best", id: sidB + ":lap.best:1", at: at,
			msg: &eventsv1.LapBestChanged{SessionId: sidB, Lap: 1}},
	}
	publish(t, js, slices.Concat(race, facts), false)
	in.logs.await(t, 6, "session fact kept", map[string]any{"session": sidA})
	in.logs.await(t, 1, "event of a session never started; dropped", map[string]any{"session": sidB})
	in.logs.await(t, len(msgs.laps)-1, "lap written", nil)
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	const q = `SELECT joined_on_lap, fuel_capacity, session_kind, total_laps, track_id, track_name, track_sure,
    best_lap, best_lap_ms, paused_ms, pit_stops::VARCHAR, track_label, kind IS NOT NULL, track IS NOT NULL,
    track_seq < best_seq FROM sessions`
	want := [][]string{{"1", "100", "lap_race", "3", "350", "Suzuka Circuit", "true", "1", "98100", "2500",
		`[{"stop":1,"inLap":1}]`, "Suzuka Circuit", "true", "true", "true"}}
	if got := indexRows(t, dir, q); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("%q,\nwant %q", got, want)
	}

	s, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Backfill(t.Context(), srv.ClientURL(), s, slog.New(slog.NewJSONHandler(newLogs(), nil)))
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	// The start, its laps, the 6 facts and the one of a session never started.
	// The kind, track and best lap come at the sequences that already set them,
	// so they change nothing; the pause, resume and stop are applied again to the
	// same end.
	if wantN := (BackfillCounts{Read: 1 + len(msgs.laps) + 7, Facts: 3}); got != wantN {
		t.Errorf("backfill %+v, want %+v", got, wantN)
	}
	if got := indexRows(t, dir, q); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("after the backfill %q,\nwant %q", got, want)
	}
}
