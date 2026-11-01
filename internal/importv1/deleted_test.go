package importv1

import (
	"database/sql"
	"path/filepath"
	"testing"

	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/proto"

	"github.com/those-who-run/logbook/internal/store"
)

// paddock ADR 0014, decision 3: an imported session deleted from the pit wall
// stays deleted when the import runs again, both when the run finds it imported
// and when it writes it again (an import stopped before marking it, here made
// by clearing from_v1_rows): StartSession, WriteLap, EndSession and MarkImported
// keep deleted_at.
func TestARerunKeepsADeletedSessionDeleted(t *testing.T) {
	from, data := v1Dir(t), t.TempDir()
	st, err := store.Open(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), from, st); err != nil {
		t.Fatal(err)
	}
	del, err := st.DeleteSession(t.Context(), &storev1.DeleteSessionRequest{
		Sources: []string{store.SourceV1Import}, SessionId: fixtureID,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted := del.GetSession().GetDeletedAt()
	if deleted == nil {
		t.Fatalf("delete answered %v", del)
	}
	sum, err := Run(t.Context(), from, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.Sessions[0]; got.Outcome != Already {
		t.Fatalf("second run: %+v, want already imported", got)
	}
	if got := sessionOf(t, st, fixtureID).GetDeletedAt(); !proto.Equal(got, deleted) {
		t.Errorf("deleted_at %v after a rerun, want %v", got, deleted)
	}

	// The session as an import stopped before marking it leaves it.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("duckdb", filepath.Join(data, "index.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE sessions SET from_v1_rows = NULL WHERE id = ?`, fixtureID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st = openStore(t, data)
	if sum, err = Run(t.Context(), from, st); err != nil {
		t.Fatal(err)
	}
	if got := sum.Sessions[0]; got.Outcome != Derived {
		t.Fatalf("third run: %+v, want the session written again", got)
	}
	row := sessionOf(t, st, fixtureID)
	if !proto.Equal(row.GetDeletedAt(), deleted) || row.FromV1Rows == nil {
		t.Errorf("after the session was written again: deleted_at %v, want %v; from_v1_rows %v",
			row.GetDeletedAt(), deleted, row.FromV1Rows)
	}
	list, err := st.ListSessions(t.Context(), &storev1.ListSessionsRequest{Sources: []string{store.SourceV1Import}})
	if err != nil || len(list.GetSessions()) != 0 {
		t.Errorf("listed %v, %v; want none", list.GetSessions(), err)
	}
}
