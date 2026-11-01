package store

import (
	"database/sql"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func deleteSession(t *testing.T, s *Store, id string) *storev1.DeleteSessionResponse {
	t.Helper()
	got, err := s.DeleteSession(t.Context(), &storev1.DeleteSessionRequest{Sources: live, SessionId: id})
	if err != nil {
		t.Fatalf("delete %s: %v", id, err)
	}
	return got
}

func restoreSession(t *testing.T, s *Store, id string) *storev1.RestoreSessionResponse {
	t.Helper()
	got, err := s.RestoreSession(t.Context(), &storev1.RestoreSessionRequest{Sources: live, SessionId: id})
	if err != nil {
		t.Fatalf("restore %s: %v", id, err)
	}
	return got
}

func listAll(t *testing.T, s *Store) *storev1.ListSessionsResponse {
	t.Helper()
	got, err := s.ListSessions(t.Context(), &storev1.ListSessionsRequest{Sources: live})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func getRow(t *testing.T, s *Store, id string) *storev1.SessionRow {
	t.Helper()
	got, err := s.GetSession(t.Context(), &storev1.GetSessionRequest{Sources: live, SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	return got.GetSession()
}

// ADR 0014, decisions 1 and 2: a delete is soft. It sets deleted_at, keeps every
// row and lap file, and changes the data version; the session leaves
// sessions.list, its total, its filter counts and same_track, while
// sessions.get, laps.list and laps.trace still answer for it with deleted_at
// set.
func TestADeleteIsSoft(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	history(t, s)
	ctx := t.Context()
	before, filesBefore := listAll(t, s), files(t, dir)
	lapsBefore, err := s.ListLaps(ctx, &storev1.ListLapsRequest{Sources: live, SessionId: idSuzuka2})
	if err != nil {
		t.Fatal(err)
	}

	got := deleteSession(t, s, idSuzuka2)
	if got.GetSession().GetSessionId() != idSuzuka2 || got.GetSession().GetDeletedAt() == nil {
		t.Fatalf("delete answered %v", got)
	}
	if got.GetDataVersion() == before.GetDataVersion() || got.GetDataVersion() != s.DataVersion() {
		t.Errorf("data version %q after the delete, was %q, store's %q", got.GetDataVersion(), before.GetDataVersion(), s.DataVersion())
	}
	if after := files(t, dir); !slices.Equal(slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(filesBefore))) {
		t.Errorf("files %q after the delete, were %q", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(filesBefore)))
	}

	list := listAll(t, s)
	if want := []string{idOpen, idUnsure, idSugo, idSuzuka1}; !slices.Equal(ids(list.GetSessions()), want) {
		t.Errorf("listed %q, want %q", ids(list.GetSessions()), want)
	}
	if list.GetTotal() != 4 || list.GetMatched() != 4 {
		t.Errorf("total %d matched %d, want 4 and 4", list.GetTotal(), list.GetMatched())
	}
	// Suzuka2 was the only ranked session, and one of three Porsche ones.
	for _, k := range list.GetOptions().GetKind() {
		if k.GetSessionType() == eventsv1.SessionType_SESSION_TYPE_RANKED {
			t.Errorf("kind counts %v still have the deleted session's kind", list.GetOptions().GetKind())
		}
	}
	for _, c := range list.GetOptions().GetCar() {
		if c.GetValue() == "Porsche 911 RSR" && c.GetCount() != 2 {
			t.Errorf("car counts %v, want 2 Porsche sessions", list.GetOptions().GetCar())
		}
	}
	// A filter that only the deleted session matched matches nothing.
	ranked, err := s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live,
		SessionType: eventsv1.SessionType_SESSION_TYPE_RANKED})
	if err != nil || len(ranked.GetSessions()) != 0 || ranked.GetMatched() != 0 {
		t.Errorf("ranked: %v, %v", ranked, err)
	}

	same, err := s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{Sources: live, SessionId: idSuzuka1})
	if err != nil || len(same.GetSessions()) != 0 || same.GetMatched() != 0 {
		t.Errorf("same track as Suzuka1: %v, %v; want none, Suzuka2 is deleted", same, err)
	}
	// The deleted session's own page still finds the others at its track.
	same, err = s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{Sources: live, SessionId: idSuzuka2})
	if err != nil || !slices.Equal(ids(same.GetSessions()), []string{idSuzuka1}) {
		t.Errorf("same track as the deleted Suzuka2: %v, %v", same, err)
	}

	if row := getRow(t, s, idSuzuka2); diff(got.GetSession(), row) != "" {
		t.Errorf("get: %s", diff(got.GetSession(), row))
	}
	laps, err := s.ListLaps(ctx, &storev1.ListLapsRequest{Sources: live, SessionId: idSuzuka2})
	if err != nil || laps.GetSession().GetDeletedAt() == nil || len(laps.GetLaps()) != len(lapsBefore.GetLaps()) {
		t.Errorf("laps: %v, %v", laps, err)
	}
	tr, err := s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: live, SessionId: idSuzuka2, Lap: 1})
	if err != nil || len(tr.GetTrace().GetT()) == 0 {
		t.Errorf("trace: %v, %v", tr, err)
	}
}

// Decision 3: deleting a deleted session, or restoring one that is not deleted,
// succeeds and changes nothing, the data version included; a restore clears
// deleted_at and lists the session again under a new data version.
func TestDeleteAndRestoreAreIdempotent(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	v0 := s.DataVersion()
	restored := restoreSession(t, s, idSugo)
	if restored.GetDataVersion() != v0 || restored.GetSession().GetDeletedAt() != nil {
		t.Errorf("restoring a session not deleted: %v, data version was %q", restored, v0)
	}
	first := deleteSession(t, s, idSugo)
	again := deleteSession(t, s, idSugo)
	if d := diff(first, again); d != "" {
		t.Errorf("a second delete changed something: %s", d)
	}
	if again.GetDataVersion() != s.DataVersion() {
		t.Errorf("data version %q, store's %q", again.GetDataVersion(), s.DataVersion())
	}

	back := restoreSession(t, s, idSugo)
	if back.GetSession().GetDeletedAt() != nil || back.GetDataVersion() == first.GetDataVersion() {
		t.Errorf("restore answered %v after the delete's %q", back, first.GetDataVersion())
	}
	if !slices.Contains(ids(listAll(t, s).GetSessions()), idSugo) {
		t.Error("a restored session is not listed")
	}
	if againBack := restoreSession(t, s, idSugo); diff(back, againBack) != "" {
		t.Errorf("a second restore changed something: %s", diff(back, againBack))
	}
	// Deleted again, it has a new deleted_at.
	if d := deleteSession(t, s, idSugo); d.GetSession().GetDeletedAt().AsTime().Equal(first.GetSession().GetDeletedAt().AsTime()) {
		t.Errorf("deleted again at %v, as the first time", d.GetSession().GetDeletedAt().AsTime())
	}
}

// Decision 4: an open session cannot be deleted (409, ErrConflict) and is left
// as it was; a session no source has is ErrNotFound, and a request with no
// sources ErrBadRequest. The first of the sources that has the session is the
// one deleted, as sessions.get's.
func TestWhatADeleteRefuses(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	ctx := t.Context()
	v0 := s.DataVersion()
	if _, err := s.DeleteSession(ctx, &storev1.DeleteSessionRequest{Sources: live, SessionId: idOpen}); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting an open session: %v, want ErrConflict", err)
	}
	if row := getRow(t, s, idOpen); row.GetDeletedAt() != nil || s.DataVersion() != v0 {
		t.Errorf("the refused delete changed the session: %v, data version %q, was %q", row, s.DataVersion(), v0)
	}
	for name, err := range map[string]error{
		"delete, unknown": func() error {
			_, err := s.DeleteSession(ctx, &storev1.DeleteSessionRequest{Sources: live, SessionId: "nope"})
			return err
		}(),
		"restore, unknown": func() error {
			_, err := s.RestoreSession(ctx, &storev1.RestoreSessionRequest{Sources: live, SessionId: "nope"})
			return err
		}(),
		"delete, another source": func() error {
			_, err := s.DeleteSession(ctx, &storev1.DeleteSessionRequest{Sources: []string{"parity"}, SessionId: idSugo})
			return err
		}(),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.DeleteSession(ctx, &storev1.DeleteSessionRequest{SessionId: idSugo}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("no sources: %v, want ErrBadRequest", err)
	}
	if _, err := s.RestoreSession(ctx, &storev1.RestoreSessionRequest{SessionId: idSugo}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("no sources: %v, want ErrBadRequest", err)
	}

	// The same id under two sources: the first source asked for is the one.
	at(idSugo, day(1), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, sugo).inSource("parity").write(t, s)
	got, err := s.DeleteSession(ctx, &storev1.DeleteSessionRequest{Sources: []string{"parity", SourceLive}, SessionId: idSugo})
	if err != nil || got.GetSession().GetSource() != "parity" {
		t.Fatalf("delete from parity, live: %v, %v", got, err)
	}
	if getRow(t, s, idSugo).GetDeletedAt() != nil {
		t.Error("the live session was deleted with the parity one")
	}
}

// inSource is sess under src.
func (sess session) inSource(src string) session {
	sess.started = proto.CloneOf(sess.started)
	sess.started.Source = src
	return sess
}

// Decision 3: a delete sticks. Every write that upserts or updates the session
// (each of its events delivered again, the backfill reading them back) keeps
// deleted_at; only a restore clears it.
func TestADeleteSticksThroughEveryWrite(t *testing.T) {
	s := open(t, t.TempDir())
	ctx := t.Context()
	sess := at(idSuzuka1, day(0), "Mazda RX-VISION GT3", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, suzuka)
	sess.write(t, s)
	posted := &eventsv1.CardPosted{SessionId: idSuzuka1, PostedAt: timestamppb.New(day(0).Add(time.Hour))}
	if err := s.MarkPosted(ctx, SourceLive, posted); err != nil {
		t.Fatal(err)
	}
	deleted := deleteSession(t, s, idSuzuka1).GetSession().GetDeletedAt()

	// Every event delivered again, as a redelivery or a replay of EVENTS would.
	sess.write(t, s)
	for _, w := range []func() error{
		func() error { return s.MarkPosted(ctx, SourceLive, posted) },
		func() error { _, err := s.BackfillStarted(ctx, SourceLive, sess.started); return err },
		func() error { _, err := s.BackfillSummary(ctx, SourceLive, sess.ended); return err },
		func() error { _, err := s.BackfillLap(ctx, SourceLive, sess.laps[0]); return err },
		func() error { _, err := s.BackfillPosted(ctx, SourceLive, posted); return err },
		func() error { _, err := s.FillFromSummaries(ctx); return err },
		func() error {
			_, err := s.ApplyFact(ctx, SourceLive, &eventsv1.LapBestChanged{SessionId: idSuzuka1, Lap: 1, LapTimeMs: 98_100}, 99)
			return err
		},
	} {
		if err := w(); err != nil {
			t.Fatal(err)
		}
	}
	row := getRow(t, s, idSuzuka1)
	if !proto.Equal(row.GetDeletedAt(), deleted) {
		t.Errorf("deleted_at %v after the session was written again, want %v", row.GetDeletedAt(), deleted)
	}
	if slices.Contains(ids(listAll(t, s).GetSessions()), idSuzuka1) {
		t.Error("the deleted session is listed again after its events came again")
	}
	if restoreSession(t, s, idSuzuka1).GetSession().GetDeletedAt() != nil {
		t.Error("the restore left deleted_at")
	}
}

// Schema 8 adds deleted_at, NULL for every session already stored, so none is
// deleted by the migration.
func TestVersion8DeletesNothing(t *testing.T) {
	dir := t.TempDir()
	withIndex(t, dir, func(db *sql.DB) {
		ctx := t.Context()
		if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for v := range 7 {
			if err := step(ctx, db, v); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sessions (src, id, started_at, type, car_code, joined_part_way,
    first_seq, ended_at) VALUES ('live', ?, '2026-10-30 12:00:00', 'race', 1, false, 1, '2026-10-30 12:30:00')`, sid); err != nil {
			t.Fatal(err)
		}
	})
	s := open(t, dir)
	if got := listAll(t, s); !slices.Equal(ids(got.GetSessions()), []string{sid}) || got.GetSessions()[0].GetDeletedAt() != nil {
		t.Errorf("after the migration: %v", got)
	}
}
