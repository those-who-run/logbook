package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/proto"
)

// fixtureV013 is a data directory's index.duckdb as logbook v0.1.3 left it, at
// schema version 2: the test race's session and laps, ended, without their lap
// files. It was written by `go test ./internal/store -run TestOpensTheCommittedIndex
// -update` in a checkout of the v0.1.3 tag.
const fixtureV013 = "testdata/index-v0.1.3.duckdb"

// newColumns are the sessions columns migration 3 adds (paddock#289).
const newColumns = `started_fuel_at_start, last_seq, ended_fuel_used, ended_final_position, session_kind`

// withV014Fields gives sess every session field contracts v0.1.4 added.
func withV014Fields(sess session) session {
	sess.started.SessionType = eventsv1.SessionType_SESSION_TYPE_LAP_RACE
	sess.started.FuelAtStart = 50
	sess.ended.LastSeq = sess.frames[len(sess.frames)-1].GetSeq()
	sess.ended.Summary.FuelUsed = 8.995
	sess.ended.Summary.FinalPosition = proto.Int32(2)
	sess.ended.Summary.SessionType = eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD
	return sess
}

// AC2: a SessionStarted and a SessionEnded carrying every field paddock#276 added
// are stored, each in its own column, apart from the columns worked out from the
// laps.
func TestStoresEverySessionFieldContractsV014Added(t *testing.T) {
	s := open(t, t.TempDir())
	sess := withV014Fields(race("live", sid))
	sess.write(t, s)
	got := rowsOf(t, t.Context(), s.index, `SELECT type, `+newColumns+`, final_position FROM sessions`)
	want := [][]string{{"lap_race", "50", fmt.Sprint(sess.ended.GetLastSeq()), "8.995", "2", "timed_field", "2"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("type, %s, final_position %q, want %q", newColumns, got, want)
	}
	// Ending it again keeps the first end's fields, as it keeps the rest of the end.
	again := proto.Clone(sess.ended).(*eventsv1.SessionEnded)
	again.LastSeq++
	again.Summary.FuelUsed = 1
	again.Summary.FinalPosition = proto.Int32(9)
	again.Summary.SessionType = eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED
	if err := s.EndSession(t.Context(), "live", again, sess.endedAt); err != nil {
		t.Fatal(err)
	}
	if again := rowsOf(t, t.Context(), s.index, `SELECT type, `+newColumns+`, final_position FROM sessions`); !slices.EqualFunc(again, got, slices.Equal) {
		t.Errorf("ending again changed the fields: %q to %q", got, again)
	}
}

// AC3 and R3: a session ended by a restart, by the next session and when a
// replay's frames stopped each stores its own end_reason, by name as the older
// reasons are; and the session types contracts v0.1.4 added are stored by name in
// type and session_kind.
func TestTheNewEndReasonsAndSessionTypesAreStoredByName(t *testing.T) {
	s := open(t, t.TempDir())
	cases := []struct {
		reason       eventsv1.EndReason
		typ          eventsv1.SessionType
		reasonStored string
		typeStored   string
	}{
		{eventsv1.EndReason_END_REASON_RESTARTED, eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS, "restarted", "solo_laps"},
		{eventsv1.EndReason_END_REASON_NEXT_SESSION_STARTED, eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, "next_session_started", "timed_field"},
		{eventsv1.EndReason_END_REASON_INPUT_ENDED, eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, "input_ended", "solo_timed"},
	}
	var want [][]string
	for i, c := range cases {
		id := fmt.Sprintf("%s%d", sid[:25], i)
		sess := withV014Fields(race("live", id))
		sess.started.SessionType = c.typ
		sess.ended.Reason = c.reason
		sess.ended.Summary.SessionType = c.typ
		sess.write(t, s)
		want = append(want, []string{id, c.typeStored, c.reasonStored, c.typeStored})
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT id, type, end_reason, session_kind FROM sessions ORDER BY id`)
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("id, type, end_reason, session_kind %q, want %q", got, want)
	}
}

// A start fuel of 0 is a real empty tank when the session's type is one contracts
// v0.1.4 added, and so the event is from v0.1.4 on; with an older type it is an
// event that carries no start fuel, and NULL.
func TestAStartFuelOf0IsKeptOnlyFromAV014Event(t *testing.T) {
	s := open(t, t.TempDir())
	cases := []struct {
		typ  eventsv1.SessionType
		want string
	}{
		{eventsv1.SessionType_SESSION_TYPE_LAP_RACE, "0"},
		{eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, "0"},
		{eventsv1.SessionType_SESSION_TYPE_RACE, "NULL"},
		{eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED, "NULL"},
	}
	var want [][]string
	for i, c := range cases {
		id := fmt.Sprintf("%s%d", sid[:25], i)
		started := race("live", id).started
		started.SessionType = c.typ
		started.FuelAtStart = 0
		if err := s.StartSession(t.Context(), started); err != nil {
			t.Fatal(err)
		}
		want = append(want, []string{id, c.want})
	}
	got := rowsOf(t, t.Context(), s.index, `SELECT id, started_fuel_at_start FROM sessions ORDER BY id`)
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("id, started_fuel_at_start %q, want %q", got, want)
	}
}

// AC5 and R5: a session from contracts v0.1.3, whose messages carry none of the
// new fields, is stored as before, with the new columns NULL.
func TestAV013SessionLeavesTheNewColumnsNull(t *testing.T) {
	s := open(t, t.TempDir())
	race("live", sid).write(t, s)
	got := rowsOf(t, t.Context(), s.index, `SELECT type, end_reason, laps_completed, `+newColumns+` FROM sessions`)
	want := [][]string{{"race", "finished", "2", "NULL", "NULL", "NULL", "NULL", "NULL"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("type, end_reason, laps_completed, %s %q, want %q", newColumns, got, want)
	}
}

// AC4 and R4: a data directory logbook v0.1.3 wrote upgrades in place on start:
// its rows are unchanged and the new columns NULL. A second start changes nothing.
func TestAV013DataDirectoryUpgradesInPlace(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(fixtureV013)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexFile), b, 0o600); err != nil { //nolint:gosec // the test's own directory
		t.Fatal(err)
	}
	const (
		sessions = `SELECT * EXCLUDE (` + newColumns + `, ` + schema5Sessions + `) FROM sessions ORDER BY src, id`
		laps     = `SELECT * EXCLUDE (` + schema5Laps + `) FROM laps ORDER BY src, session_id, lap`
		version  = `SELECT version FROM schema_version`
	)
	var before struct{ sessions, laps [][]string }
	withIndex(t, dir, func(db *sql.DB) {
		if got := rowsOf(t, t.Context(), db, version); got[0][0] != "2" {
			t.Fatalf("the fixture is at schema version %s, want 2 (v0.1.3's)", got[0][0])
		}
		before.sessions = rowsOf(t, t.Context(), db, `SELECT * FROM sessions ORDER BY src, id`)
		before.laps = rowsOf(t, t.Context(), db, `SELECT * FROM laps ORDER BY src, session_id, lap`)
	})
	if len(before.sessions) == 0 || len(before.laps) == 0 {
		t.Fatalf("the fixture holds %d sessions and %d laps", len(before.sessions), len(before.laps))
	}

	var first [][]string
	for start := range 2 {
		s, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		withIndex(t, dir, func(db *sql.DB) {
			if got := rowsOf(t, t.Context(), db, version); got[0][0] != fmt.Sprint(SchemaVersion) {
				t.Errorf("start %d: schema version %s, want %d", start+1, got[0][0], SchemaVersion)
			}
			if got := rowsOf(t, t.Context(), db, sessions); !slices.EqualFunc(got, before.sessions, slices.Equal) {
				t.Errorf("start %d: sessions %q, want them unchanged, %q", start+1, got, before.sessions)
			}
			if got := rowsOf(t, t.Context(), db, laps); !slices.EqualFunc(got, before.laps, slices.Equal) {
				t.Errorf("start %d: laps changed", start+1)
			}
			got := rowsOf(t, t.Context(), db, `SELECT `+newColumns+` FROM sessions`)
			for _, row := range got {
				if !slices.Equal(row, []string{"NULL", "NULL", "NULL", "NULL", "NULL"}) {
					t.Errorf("start %d: %s %q, want NULL", start+1, newColumns, row)
				}
			}
			all := rowsOf(t, t.Context(), db, `SELECT * FROM sessions ORDER BY src, id`)
			if start == 1 && !slices.EqualFunc(all, first, slices.Equal) {
				t.Errorf("the second start changed sessions: %q to %q", first, all)
			}
			first = all
		})
	}
}
