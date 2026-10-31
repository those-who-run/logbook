package importv1

import (
	"context"
	"math"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/logbook/internal/store"
)

// A session taken from v1's rows (ADR 0009, point 5) is v1's own judgement, as
// summary.json has it, put into contracts' messages field for field: its laps
// with v1's pit and out-lap flags (the maintainer's decision, 2026-10-31: the pit
// wall gets no refuel fallback), its kind, track, pit stops and result. Its lap
// files are still the frames v1 kept, as ecu reads them; only the session's facts
// are v1's.

var (
	v1Kinds = map[string]eventsv1.SessionType{
		"lap_race": eventsv1.SessionType_SESSION_TYPE_LAP_RACE, "solo_laps": eventsv1.SessionType_SESSION_TYPE_SOLO_LAPS,
		"timed_field": eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, "solo_timed": eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED,
		"unknown": eventsv1.SessionType_SESSION_TYPE_UNKNOWN,
	}
	// v1Types are v1's session types (store.py's session_type), for a session
	// stored before v1 judged kinds.
	v1Types = map[string]eventsv1.SessionType{
		"race": eventsv1.SessionType_SESSION_TYPE_RACE, "time_trial": eventsv1.SessionType_SESSION_TYPE_TIME_TRIAL,
		"practice": eventsv1.SessionType_SESSION_TYPE_PRACTICE,
	}
	v1Starts = map[string]eventsv1.StartType{
		"grid": eventsv1.StartType_START_TYPE_GRID, "rolling": eventsv1.StartType_START_TYPE_ROLLING,
		"flying": eventsv1.StartType_START_TYPE_FLYING, "unknown": eventsv1.StartType_START_TYPE_UNKNOWN,
	}
	v1Confidences = map[string]eventsv1.TrackConfidence{
		"none": eventsv1.TrackConfidence_TRACK_CONFIDENCE_NONE, "ambiguous": eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS,
		"probable": eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE, "confirmed": eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED,
	}
	// v1PitRoles are v1's lap roles (pitstop.py's lap_role); a lap with none is
	// PIT_ROLE_NONE.
	v1PitRoles = map[string]eventsv1.PitRole{
		"in": eventsv1.PitRole_PIT_ROLE_IN_LAP, "out": eventsv1.PitRole_PIT_ROLE_OUT_LAP,
		"in_out": eventsv1.PitRole_PIT_ROLE_IN_AND_OUT,
	}
)

func sessionType(v1 *v1SessionSummary) eventsv1.SessionType {
	if v1.Kind != nil {
		if t, ok := v1Kinds[v1.Kind.Kind]; ok {
			return t
		}
	}
	if t, ok := v1Types[v1.Type]; ok {
		return t
	}
	return eventsv1.SessionType_SESSION_TYPE_UNKNOWN
}

func timestamp(secs float64) *timestamppb.Timestamp {
	return timestamppb.New(time.Unix(0, int64(math.Round(secs*1e9))).UTC())
}

func f64(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func ms(secs float64) int32 { return int32(math.Round(secs * 1000)) }

// rowsLaps are v1's laps as LapCompleted, each with the seq range of its file's
// frames: frames are ecu's frames of the session, in the files' order, and
// lapEnds the index in them after each lap's file.
func rowsLaps(v1 *v1SessionSummary, frames []*telemetryv1.Frame, lapEnds []int) []*eventsv1.LapCompleted {
	out := make([]*eventsv1.LapCompleted, 0, len(v1.Laps))
	from := 0
	for i, l := range v1.Laps {
		lap := &eventsv1.LapCompleted{
			SessionId: v1.SessionID, Lap: l.Lap, Complete: l.Complete,
			FuelAtStart: f64(l.FuelStart), FuelAtEnd: f64(l.FuelEnd), FuelUsed: f64(l.FuelUsed),
			Position: l.Position, PitRole: eventsv1.PitRole_PIT_ROLE_NONE,
		}
		if l.LapTimeMs != nil {
			lap.LapTimeMs = *l.LapTimeMs
		}
		if l.Pit != nil {
			if role, ok := v1PitRoles[*l.Pit]; ok {
				lap.PitRole = role
			}
		}
		// v1 had no word for it: a timed complete lap outside a stop counts.
		lap.CountsForBest = l.Complete && lap.GetLapTimeMs() > 0 && lap.GetPitRole() == eventsv1.PitRole_PIT_ROLE_NONE
		if to := lapEnds[i]; to > from && to <= len(frames) {
			lap.FirstSeq, lap.LastSeq = frames[from].GetSeq(), frames[to-1].GetSeq()
		}
		from = lapEnds[i]
		out = append(out, lap)
	}
	return out
}

// rowsSummary is v1's summary.json as a SessionSummary.
func rowsSummary(v1 *v1SessionSummary, laps []*eventsv1.LapCompleted) *eventsv1.SessionSummary {
	sum := &eventsv1.SessionSummary{
		LapsCompleted: v1.LapsCompleted, FuelUsed: f64(v1.FuelUsed), FinalPosition: v1.FinalPosition,
		SessionType: sessionType(v1), StartedAt: timestamp(v1.StartedAt),
		EndedPaused: v1.EndedPaused, CarCode: v1.CarCode, FastestLapMs: v1.FastestLapMs,
		GameBestLapMs: v1.GT7BestLapMs, Finished: v1.Finished, JoinedOnLap: v1.JoinedOnLap,
		TotalTimeMs: v1.TotalTimeMs, TotalCars: v1.TotalCars, FuelBurned: f64(v1.FuelBurned),
		FuelBurnedByStint: v1.FuelByStint, FuelStints: v1.FuelStints, FuelCapacity: f64(v1.FuelCapacity),
		FuelAtStart: f64(v1.FuelAtStart), FuelAtEnd: f64(v1.FuelAtEnd), Laps: laps,
	}
	if v1.TotalLaps > 0 {
		sum.TotalLaps = proto.Int32(v1.TotalLaps)
	}
	if v1.EndedAt != nil {
		sum.EndedAt = timestamp(*v1.EndedAt)
	}
	if v1.DrivingMs != nil {
		sum.DrivingMs = int32(*v1.DrivingMs) //nolint:gosec // a session's milliseconds fit
	}
	if v1.PausedMs != nil {
		sum.PausedMs = int32(*v1.PausedMs) //nolint:gosec // a session's milliseconds fit
	}
	if v1.CarName != nil {
		sum.CarName = *v1.CarName
	}
	if v1.Stints != nil {
		sum.Stints = *v1.Stints
	}
	if v1.Finished {
		sum.FinishingPosition = v1.FinalPosition
	}
	if v1.BestLapMs != nil {
		sum.BestLapTimeMs = v1.BestLapMs
		for _, l := range laps {
			if l.GetComplete() && l.GetLapTimeMs() == *v1.BestLapMs {
				sum.BestLap = proto.Int32(l.GetLap())
				break
			}
		}
	}
	if t := v1.Track; t != nil {
		sum.Track = &eventsv1.TrackVerdict{TrackId: t.ID, Confidence: v1Confidences[t.Confidence], Evidence: t.Evidence}
		if t.Name != nil {
			sum.Track.Name = *t.Name
		}
		if t.Family != nil {
			sum.Track.Family = *t.Family
		}
	}
	if k := v1.Kind; k != nil {
		sum.Kind = &eventsv1.SessionKind{
			SessionType: sessionType(v1), TotalLaps: k.TotalLaps, Field: k.Field, Evidence: k.Evidence,
			FuelUse: eventsv1.FuelUse_FUEL_USE_UNKNOWN, Start: eventsv1.StartType_START_TYPE_UNKNOWN,
		}
		if k.CarsOnTrack != nil {
			sum.Kind.CarsOnTrack = *k.CarsOnTrack
		}
		if k.FuelUse != nil {
			sum.Kind.FuelUse = map[bool]eventsv1.FuelUse{true: eventsv1.FuelUse_FUEL_USE_ON, false: eventsv1.FuelUse_FUEL_USE_OFF}[*k.FuelUse]
		}
		if k.Start != nil {
			if s, ok := v1Starts[*k.Start]; ok {
				sum.Kind.Start = s
			}
		}
	}
	for _, p := range v1.PitStops {
		stop := &eventsv1.PitStop{
			Stop: p.Stop, InLap: p.InLap, OutLap: p.OutLap, StationaryMs: ms(p.StationarySecs),
			FuelBefore: p.FuelBefore, FuelAfter: p.FuelAfter, FuelAdded: p.FuelAdded, TyresChanged: p.TyresChanged,
			PitLaneKph: p.PitLaneKph, PositionBefore: p.PositionBefore, PositionAfter: p.PositionAfter, Evidence: p.Evidence,
		}
		if p.PitLaneSecs != nil {
			stop.PitLaneMs = proto.Int32(ms(*p.PitLaneSecs))
		}
		sum.PitStops = append(sum.PitStops, stop)
	}
	return sum
}

// rowsEndReason is v1's end as an EndReason: v1 kept whether the session ended
// at the flag, and whether its own shutdown ended it, and no more.
func rowsEndReason(v1 *v1SessionSummary) eventsv1.EndReason {
	switch {
	case v1.Finished:
		return eventsv1.EndReason_END_REASON_FINISHED
	case v1.Shutdown:
		return eventsv1.EndReason_END_REASON_ENGINEER_STOPPED
	}
	return eventsv1.EndReason_END_REASON_UNKNOWN
}

// writeRows stores a session from v1's rows, its lap files from ecu's frames of
// it, then marks it imported, flagged from_v1_rows.
func writeRows(ctx context.Context, st Store, v1 *v1SessionSummary, frames []*telemetryv1.Frame, lapEnds []int) error {
	laps := rowsLaps(v1, frames, lapEnds)
	sum := rowsSummary(v1, laps)
	started := &eventsv1.SessionStarted{
		SessionId: v1.SessionID, Source: store.SourceV1Import, Game: eventsv1.Game_GAME_GT7,
		SessionType: sum.GetSessionType(), CarCode: v1.CarCode, TotalLaps: sum.TotalLaps,
		JoinedPartWay: v1.JoinedOnLap != nil, StartedAt: sum.GetStartedAt(), FuelAtStart: f64(v1.FuelAtStart),
		CarName: sum.GetCarName(), JoinedOnLap: v1.JoinedOnLap, FuelCapacity: f64(v1.FuelCapacity),
	}
	if len(frames) > 0 {
		started.FirstSeq = frames[0].GetSeq()
	}
	if err := st.StartSession(ctx, started); err != nil {
		return err
	}
	for _, l := range laps {
		if err := st.WriteLap(ctx, store.SourceV1Import, l, frames); err != nil {
			return err
		}
	}
	ended := &eventsv1.SessionEnded{SessionId: v1.SessionID, Reason: rowsEndReason(v1), Summary: sum}
	if len(frames) > 0 {
		ended.LastSeq = frames[len(frames)-1].GetSeq()
	}
	endedAt := sum.GetStartedAt().AsTime()
	if sum.GetEndedAt() != nil {
		endedAt = sum.GetEndedAt().AsTime()
	}
	if err := st.EndSession(ctx, store.SourceV1Import, ended, endedAt); err != nil {
		return err
	}
	return st.MarkImported(ctx, v1.SessionID, true)
}
