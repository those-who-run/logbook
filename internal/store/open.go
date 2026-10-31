package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/those-who-run/contracts/format"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ApplyFact copies what one of a session's events says into its row while the
// session is open (paddock#337), for the open session's summary (paddock#122).
// seq is the event's sequence in EVENTS. It takes:
//
//   - SessionKindChanged: kind (the SessionKind, protobuf JSON), session_kind and
//     total_laps;
//   - SessionTrackChanged: track (the TrackVerdict, protobuf JSON), track_id,
//     track_name, track_sure and track_label;
//   - SessionContinued: its gap, added to gaps, and its carried track while the
//     session has none yet (track NULL);
//   - SessionPaused and SessionResumed: paused_ms, the session's time paused so
//     far, which only grows, so a redelivery never takes it back;
//   - LapBestChanged: best_lap and best_lap_ms;
//   - PitExited: its stop, added to pit_stops, or put in place of the stop of the
//     same number.
//
// The track, the kind and the best lap are the last event's: each group keeps the
// sequence that set it (track_seq, kind_seq, best_seq), and an event of a lower
// one, an older event delivered again after a newer, changes nothing. It copies
// ecu's facts and works nothing out. Once the session has ended its summary has
// the last word, and an event changes nothing. It says whether the row changed; a
// session the index does not hold is ErrUnknownSession, and any other message an
// error.
func (s *Store) ApplyFact(ctx context.Context, src string, ev proto.Message, seq uint64) (bool, error) {
	src = sourceOf(src)
	id, err := factSession(ev)
	if err != nil {
		return false, err
	}
	if err := checkKey(src, id); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sessionExists(ctx, src, id); err != nil {
		return false, err
	}
	// exec updates the open session's row, where cond holds as well: args are
	// set's values, then cond's.
	exec := func(set, cond string, args ...any) (bool, error) {
		return changed(s.index.ExecContext(ctx, `UPDATE sessions SET `+set+ //nolint:gosec // constant SQL
			` WHERE `+cond+` AND src = ? AND id = ? AND ended_at IS NULL`, append(args, src, id)...))
	}
	n := int64(seq) //nolint:gosec // a stream sequence is far below 2^63
	switch ev := ev.(type) {
	case *eventsv1.SessionKindChanged:
		k := ev.GetKind()
		js, err := jsonOf(k)
		if err != nil {
			return false, err
		}
		return exec(`kind = ?, session_kind = ?, total_laps = ?, kind_seq = ?`, `COALESCE(kind_seq, 0) < ?`,
			js, enumName(k.GetSessionType().String(), "SESSION_TYPE_"), optional(k.TotalLaps), n, n)
	case *eventsv1.SessionTrackChanged:
		set, args, err := trackSet(ev.GetTrack())
		if err != nil {
			return false, err
		}
		return exec(set+`, track_seq = ?`, `COALESCE(track_seq, 0) < ?`, append(args, n, n)...)
	case *eventsv1.SessionContinued:
		return s.continuedFacts(ctx, src, id, ev, n, exec)
	case *eventsv1.SessionPaused:
		return exec(`paused_ms = greatest(COALESCE(paused_ms, 0), ?)`, `true`, ev.GetPausedMs())
	case *eventsv1.SessionResumed:
		return exec(`paused_ms = greatest(COALESCE(paused_ms, 0), ?)`, `true`, ev.GetPausedMs())
	case *eventsv1.LapBestChanged:
		return exec(`best_lap = ?, best_lap_ms = ?, best_seq = ?`, `COALESCE(best_seq, 0) < ?`,
			ev.GetLap(), ev.GetLapTimeMs(), n, n)
	case *eventsv1.PitExited:
		if ev.GetStop() == nil {
			return false, nil
		}
		list, err := s.jsonColumn(ctx, src, id, "pit_stops")
		if err != nil {
			return false, err
		}
		list, err = putJSON(list, ev.GetStop(), func(a, b *eventsv1.PitStop) bool { return a.GetStop() == b.GetStop() })
		if err != nil {
			return false, err
		}
		return exec(`pit_stops = ?`, `true`, list)
	}
	return false, fmt.Errorf("store: %T is not a session fact", ev)
}

// trackSet is the SET list and its values for a track verdict: the verdict whole,
// and the columns the filters read.
func trackSet(t *eventsv1.TrackVerdict) (string, []any, error) {
	js, err := jsonOf(t)
	if err != nil {
		return "", nil, err
	}
	return `track = ?, track_id = ?, track_name = ?, track_sure = ?, track_label = ?`,
		[]any{js, optional(t.TrackId), textOrNull(t.GetName()), trackSure(t.GetConfidence()), trackLabel(t)}, nil
}

// trackLabel is a verdict's label as the pit wall and the card name the track
// (contracts' format.TrackLabel), NULL for no verdict.
func trackLabel(t *eventsv1.TrackVerdict) driver.Value {
	if t == nil {
		return nil
	}
	return format.TrackLabel(t)
}

// jsonOf is m as protobuf JSON, NULL for none.
func jsonOf[M interface {
	proto.Message
	comparable
}](m M) (driver.Value, error) {
	var zero M
	if m == zero {
		return nil, nil
	}
	js, err := protojson.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(js), nil
}

// continuedFacts adds a SessionContinued's gap to the session's gaps, a gap of the
// same lap and start replacing its copy, and takes its carried track while the
// session has none.
func (s *Store) continuedFacts(ctx context.Context, src, id string, ev *eventsv1.SessionContinued, n int64,
	exec func(string, string, ...any) (bool, error),
) (bool, error) {
	did := false
	if g := ev.GetGap(); g != nil {
		list, err := s.jsonColumn(ctx, src, id, "gaps")
		if err != nil {
			return false, err
		}
		list, err = putJSON(list, g, func(a, b *eventsv1.SessionGap) bool {
			return a.GetLap() == b.GetLap() && proto.Equal(a.GetStartedAt(), b.GetStartedAt())
		})
		if err != nil {
			return false, err
		}
		if did, err = exec(`gaps = ?`, `true`, list); err != nil {
			return false, err
		}
	}
	if t := ev.GetTrack(); t != nil {
		// Only before a track of its own: one there is stays.
		set, args, err := trackSet(t)
		if err != nil {
			return false, err
		}
		took, err := exec(set+`, track_seq = ?`, `track IS NULL`, append(args, n)...)
		if err != nil {
			return false, err
		}
		did = did || took
	}
	return did, nil
}

// jsonColumn is a session's JSON column as text, "" when it is NULL. The caller
// holds s.mu.
func (s *Store) jsonColumn(ctx context.Context, src, id, col string) (string, error) {
	var v sql.NullString
	//nolint:gosec // col is one of this file's own column names
	err := s.index.QueryRowContext(ctx, `SELECT `+col+`::VARCHAR FROM sessions WHERE src = ? AND id = ?`, src, id).Scan(&v)
	if err != nil {
		return "", fmt.Errorf("store: %s of %s/%s: %w", col, src, id, err)
	}
	return v.String, nil
}

// putJSON puts m in list, a JSON array of messages of m's type: in place of the
// first that same says is m's, or at the end.
func putJSON[M proto.Message](list string, m M, same func(a, b M) bool) (string, error) {
	var items []json.RawMessage
	if list != "" {
		if err := json.Unmarshal([]byte(list), &items); err != nil {
			return "", fmt.Errorf("store: a JSON list: %w", err)
		}
	}
	js, err := protojson.Marshal(m)
	if err != nil {
		return "", err
	}
	put := false
	for i, raw := range items {
		old := m.ProtoReflect().New().Interface().(M) //nolint:forcetypeassert // New of an M is an M
		if err := protojson.Unmarshal(raw, old); err != nil {
			return "", fmt.Errorf("store: a JSON list: %w", err)
		}
		if same(old, m) {
			items[i], put = js, true
			break
		}
	}
	if !put {
		items = append(items, js)
	}
	out, err := json.Marshal(items)
	return string(out), err
}

// jsonArray is msgs as a JSON array of their protobuf JSON, NULL for none.
func jsonArray[M proto.Message](msgs []M) (driver.Value, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		js, err := protojson.Marshal(m)
		if err != nil {
			return nil, err
		}
		parts[i] = string(js)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// positiveOrNull is v, or NULL for 0 or less: a capacity an event leaves unset.
func positiveOrNull(v float64) driver.Value {
	if v <= 0 {
		return nil
	}
	return v
}

// factSession is the session id of an event ApplyFact takes.
func factSession(ev proto.Message) (string, error) {
	if m, ok := ev.(interface{ GetSessionId() string }); ok {
		return m.GetSessionId(), nil
	}
	return "", errors.New("store: an event with no session id")
}

// BackfillStarted sets joined_on_lap and fuel_capacity on a session stored before
// schema 5, from its session.started, each only where it is still NULL. It says
// whether the row changed; a session the index does not hold is skipped.
func (s *Store) BackfillStarted(ctx context.Context, src string, ev *eventsv1.SessionStarted) (bool, error) {
	src = sourceOf(src)
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return false, err
	}
	joined, capacity := optional(ev.JoinedOnLap), positiveOrNull(ev.GetFuelCapacity())
	if joined == nil && capacity == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return changed(s.index.ExecContext(ctx, `
UPDATE sessions SET joined_on_lap = COALESCE(joined_on_lap, ?), fuel_capacity = COALESCE(fuel_capacity, ?)
WHERE src = ? AND id = ? AND ((joined_on_lap IS NULL AND ?::INTEGER IS NOT NULL)
    OR (fuel_capacity IS NULL AND ?::DOUBLE IS NOT NULL))`,
		joined, capacity, src, ev.GetSessionId(), joined, capacity))
}
