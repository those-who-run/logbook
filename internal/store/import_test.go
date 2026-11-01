package store

import (
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// v1-import takes v1's own ids, and only it does: a v1 id is refused under any
// other source, and a ULID under v1-import (paddock ADR 0009, point 4).
func TestV1ImportKeys(t *testing.T) {
	for _, c := range []struct {
		src, id string
		ok      bool
	}{
		{SourceV1Import, "20261017-143142", true},
		{SourceV1Import, "20261017-143142-2", true},
		{SourceV1Import, "01JB8Z3K5Q9T2M7XW4R6N1PDHF", false},
		{SourceV1Import, "../20261017-143142", false},
		{SourceLive, "20261017-143142", false},
		{"v1", "20261017-143142", false},
	} {
		if err := CheckKey(c.src, c.id); (err == nil) != c.ok {
			t.Errorf("CheckKey(%q, %q): %v, want ok %v", c.src, c.id, err, c.ok)
		}
	}
}

// Schema 7: an imported session says how it was made, and readers see it as
// from_v1_rows; a session v2 saw itself has none. Imported finds a session only
// once it is ended and marked.
func TestImportedSessionsCarryFromV1Rows(t *testing.T) {
	s := open(t, t.TempDir())
	ctx := t.Context()
	const id = "20261017-143142"
	if err := s.StartSession(ctx, &eventsv1.SessionStarted{SessionId: id, Source: SourceV1Import,
		StartedAt: timestamppb.New(time.Date(2026, 10, 17, 14, 31, 42, 0, time.UTC))}); err != nil {
		t.Fatal(err)
	}
	if done, _, err := s.Imported(ctx, id); err != nil || done {
		t.Fatalf("Imported before the end: %v %v", done, err)
	}
	if err := s.EndSession(ctx, SourceV1Import, &eventsv1.SessionEnded{SessionId: id, Summary: &eventsv1.SessionSummary{}},
		time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if done, _, err := s.Imported(ctx, id); err != nil || done {
		t.Fatalf("Imported before it is marked: %v %v", done, err)
	}
	if err := s.MarkImported(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if done, fromRows, err := s.Imported(ctx, id); err != nil || !done || !fromRows {
		t.Fatalf("Imported: %v %v %v, want imported from v1's rows", done, fromRows, err)
	}
	resp, err := s.GetSession(ctx, &storev1.GetSessionRequest{Sources: []string{SourceV1Import}, SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSession().FromV1Rows == nil || !resp.GetSession().GetFromV1Rows() {
		t.Errorf("from_v1_rows %v, want true", resp.GetSession().FromV1Rows)
	}
	if err := s.MarkImported(ctx, "20261017-150000", false); err == nil {
		t.Error("MarkImported of a session not in the store: no error")
	}
	live := race(SourceLive, "01JB8Z3K5Q9T2M7XW4R6N1PDHF")
	live.write(t, s)
	resp, err = s.GetSession(ctx, &storev1.GetSessionRequest{Sources: []string{SourceLive}, SessionId: "01JB8Z3K5Q9T2M7XW4R6N1PDHF"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSession().FromV1Rows != nil {
		t.Errorf("a live session's from_v1_rows is %v, want absent", resp.GetSession().GetFromV1Rows())
	}
}
