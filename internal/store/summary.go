package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// summarySet is the SET list of the session columns schema 5 keeps from a
// session's summary, for summaryFields' values in its order. car_name,
// joined_on_lap and fuel_capacity keep the start's where the summary has none.
const summarySet = `summary = ?, car_name = COALESCE(?, car_name), track_id = ?, track_name = ?, track_sure = ?,
    joined_on_lap = COALESCE(?, joined_on_lap), fuel_capacity = COALESCE(?, fuel_capacity), best_lap = ?,
    paused_ms = ?, gaps = ?, pit_stops = ?`

// summaryFields are the values of summarySet from a session's summary: the
// summary kept whole, as protobuf JSON, then its own fields, NULL where it has
// none, the gaps and pit stops as JSON arrays. An end with no summary leaves them
// all NULL.
func summaryFields(sum *eventsv1.SessionSummary) ([]any, error) {
	out := make([]any, 11)
	if sum == nil {
		return out, nil
	}
	js, err := protojson.Marshal(sum)
	if err != nil {
		return out, fmt.Errorf("summary as JSON: %w", err)
	}
	out[0], out[1] = string(js), textOrNull(sum.GetCarName())
	if t := sum.GetTrack(); t != nil {
		out[2], out[3], out[4] = optional(t.TrackId), textOrNull(t.GetName()), trackSure(t.GetConfidence())
	}
	out[5], out[6], out[7] = optional(sum.JoinedOnLap), positiveOrNull(sum.GetFuelCapacity()), optional(sum.BestLap)
	out[8] = sum.GetPausedMs()
	if out[9], err = jsonArray(sum.GetGaps()); err != nil {
		return out, err
	}
	if out[10], err = jsonArray(sum.GetPitStops()); err != nil {
		return out, err
	}
	return out, nil
}

// trackSure is whether a track verdict is sure: probable or confirmed, as
// contracts' LiveTrack.sure and v1's sure_track_id read a confidence. An unset
// confidence is NULL.
func trackSure(c eventsv1.TrackConfidence) driver.Value {
	if c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_UNSPECIFIED {
		return nil
	}
	return c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE || c == eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED
}

// pitRole is a lap's pit_role as stored: the PitRole's name without its prefix, in
// lower case (`none`, `in_lap`, `out_lap`, `in_and_out`), or NULL for an event
// that carries none.
func pitRole(lap *eventsv1.LapCompleted) driver.Value {
	if lap.GetPitRole() == eventsv1.PitRole_PIT_ROLE_UNSPECIFIED {
		return nil
	}
	return enumName(lap.GetPitRole().String(), "PIT_ROLE_")
}

// countsForBest is a lap's counts_for_best, NULL for an event that carries no pit
// role: contracts added both together, so an event without one is from before
// them, and its false means nothing.
func countsForBest(lap *eventsv1.LapCompleted) driver.Value {
	if lap.GetPitRole() == eventsv1.PitRole_PIT_ROLE_UNSPECIFIED {
		return nil
	}
	return lap.GetCountsForBest()
}

func textOrNull(v string) driver.Value {
	if v == "" {
		return nil
	}
	return v
}

// MarkPosted records when ev's session's results card was posted (card.posted,
// paddock#67). The first post is kept: a card.posted delivered again, or read
// again by the backfill, changes nothing. A session the index does not hold is
// ErrUnknownSession.
func (s *Store) MarkPosted(ctx context.Context, src string, ev *eventsv1.CardPosted) error {
	src = sourceOf(src)
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return err
	}
	if ev.GetPostedAt() == nil {
		return fmt.Errorf("store: card of session %s has no posted_at", ev.GetSessionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sessionExists(ctx, src, ev.GetSessionId()); err != nil {
		return err
	}
	_, err := s.index.ExecContext(ctx, `
UPDATE sessions SET posted_at = COALESCE(posted_at, ?) WHERE src = ? AND id = ?`,
		ev.GetPostedAt().AsTime().UTC(), src, ev.GetSessionId())
	if err != nil {
		return fmt.Errorf("store: card of %s/%s: %w", src, ev.GetSessionId(), err)
	}
	return nil
}

// BackfillSummary keeps ev's summary on a session stored before schema 5: its
// summary and the columns taken from it (summarySet), only where summary is still NULL, so the
// first end's summary stays and a second run changes nothing. It changes no other
// column. It says whether the row changed; a session the index does not hold is
// skipped.
func (s *Store) BackfillSummary(ctx context.Context, src string, ev *eventsv1.SessionEnded) (bool, error) {
	src = sourceOf(src)
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return false, err
	}
	kept, err := summaryFields(ev.GetSummary())
	if err != nil || kept[0] == nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return changed(s.index.ExecContext(ctx, `UPDATE sessions SET `+summarySet+`
WHERE src = ? AND id = ? AND summary IS NULL`, //nolint:gosec // a constant
		append(kept, src, ev.GetSessionId())...))
}

// BackfillLap sets pit_role and counts_for_best on a lap row stored before schema
// 5, from its lap.completed, where pit_role is still NULL. It changes no other
// column, and no file. It says whether the row changed; a lap the index does not
// hold (its frames were gone, or its session never started) is skipped.
func (s *Store) BackfillLap(ctx context.Context, src string, lap *eventsv1.LapCompleted) (bool, error) {
	src = sourceOf(src)
	if err := checkKey(src, lap.GetSessionId()); err != nil {
		return false, err
	}
	role := pitRole(lap)
	if role == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return changed(s.index.ExecContext(ctx, `
UPDATE laps SET pit_role = ?, counts_for_best = ?
WHERE src = ? AND session_id = ? AND lap = ? AND pit_role IS NULL`,
		role, countsForBest(lap), src, lap.GetSessionId(), lap.GetLap()))
}

// BackfillPosted is MarkPosted for the backfill: it says whether the row changed,
// and skips a session the index does not hold.
func (s *Store) BackfillPosted(ctx context.Context, src string, ev *eventsv1.CardPosted) (bool, error) {
	src = sourceOf(src)
	if err := checkKey(src, ev.GetSessionId()); err != nil {
		return false, err
	}
	if ev.GetPostedAt() == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return changed(s.index.ExecContext(ctx, `
UPDATE sessions SET posted_at = ? WHERE src = ? AND id = ? AND posted_at IS NULL`,
		ev.GetPostedAt().AsTime().UTC(), src, ev.GetSessionId()))
}

// changed is whether a backfill's update changed a row.
func changed(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("store: backfill: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
