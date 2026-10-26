package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ExportLaps prints one JSON object per lap, with the laps columns as keys, ordered
// by session start and lap: a later session written first still comes second.
func TestExportPrintsTheLapsInOrder(t *testing.T) {
	s := open(t, t.TempDir())
	later := race("live", "01JB8Z3K5Q9T2M7XW4R6N1PDHF")
	later.started.StartedAt = timestamppb.New(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	later.write(t, s)
	race("live", sid).write(t, s)
	race("parity", sid).write(t, s)

	var out bytes.Buffer
	if err := s.ExportLaps(t.Context(), &out, ""); err != nil {
		t.Fatal(err)
	}
	type key struct {
		Session string
		Lap     int
	}
	var got []key
	lines := bufio.NewScanner(&out)
	for lines.Scan() {
		var row map[string]any
		if err := json.Unmarshal(lines.Bytes(), &row); err != nil {
			t.Fatalf("%q: %v", lines.Text(), err)
		}
		for _, col := range []string{"src", "session_id", "lap", "lap_time_ms", "complete", "fuel_start",
			"fuel_end", "fuel_used", "position", "frames", "file", "recorded_at"} {
			if _, ok := row[col]; !ok {
				t.Errorf("%q has no %s", lines.Text(), col)
			}
		}
		got = append(got, key{row["session_id"].(string), int(row["lap"].(float64))})
	}
	want := []key{{sid, 1}, {sid, 2}, {sid, 3}, {later.started.GetSessionId(), 1}, {later.started.GetSessionId(), 2}, {later.started.GetSessionId(), 3}}
	if len(got) != len(want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d is %v, want %v", i+1, got[i], want[i])
		}
	}
}
