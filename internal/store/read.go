package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The reads behind logbook's services (paddock#122 and #123): each takes and
// answers contracts' twr.store.v1 messages. They map what the store holds and
// work nothing out: a session's summary is the one its end carried, a lap is its
// lap.completed with the frames logbook counted, and the reader (the pit wall)
// derives its own view from them. Filters, counts and pages run in DuckDB, so a
// reply carries only what was asked for.

// ErrBadRequest is a request the reads refuse: no sources, a page too large, a
// day or time zone that cannot be read.
var ErrBadRequest = errors.New("bad request")

// ErrNotFound is a session, lap or lap file the sources asked for do not have.
var ErrNotFound = errors.New("not found")

// The page sizes (twr.store.v1): a limit of 0 is DefaultLimit, and one over
// MaxLimit is refused, so no reply nears NATS's 1 MiB payload limit.
const (
	DefaultLimit = 200
	MaxLimit     = 500
)

// The trace's points: 0 is DefaultPoints, and more than MaxPoints is refused.
const (
	DefaultPoints = 1500
	MaxPoints     = 10000
)

// readers is how many reads may run at once, each on a connection of its own.
const readers = 4

// v1's lap_trace constants (v1's pitwall/data.py): a gap between two frames
// counts as at most maxStepSecs on the time axis, and a step in position longer
// than jumpMetres, and than twice what the car's speed covers between the two
// frames, is a jump (the pit cut-scene).
const (
	maxStepSecs = 0.1
	jumpMetres  = 20.0
)

// touch records a write: the data version changes. A writer calls it holding mu.
func (s *Store) touch() { s.writes.Add(1) }

// DataVersion is the store's data version: one stamp for the whole store, which
// changes when a write is made, and on a restart. A read takes it before it
// reads, so an answer never carries a version newer than its data: a write
// between the two only makes the next poll read again. The sessions and laps
// answers carry it; a trace carries its lap's own (GetTrace).
func (s *Store) DataVersion() string {
	return s.epoch + "." + strconv.FormatUint(s.writes.Load(), 10)
}

// ListSessions answers svc.logbook.sessions.list: the sessions of the sources,
// newest first, filtered and paged, with each filter's values and counts.
func (s *Store) ListSessions(ctx context.Context, req *storev1.ListSessionsRequest) (*storev1.ListSessionsResponse, error) {
	if err := checkSources(req.GetSources()); err != nil {
		return nil, err
	}
	limit, err := pageLimit(req.GetOffset(), req.GetLimit())
	if err != nil {
		return nil, err
	}
	f, err := filtersOf(req)
	if err != nil {
		return nil, err
	}
	version := s.DataVersion()
	if req.KnownDataVersion != nil && req.GetKnownDataVersion() == version {
		return &storev1.ListSessionsResponse{DataVersion: version, Unchanged: true}, nil
	}
	out := &storev1.ListSessionsResponse{DataVersion: version, Options: &storev1.FilterCounts{}}
	err = s.read(ctx, func(tx *sql.Tx) error {
		with, args := sessionsOf(req.GetSources())
		all, allArgs := f.where("")
		q := with + `SELECT count(*), count(*) FILTER (WHERE ` + all + `) FROM s`
		if err := tx.QueryRowContext(ctx, q, append(slices.Clone(args), allArgs...)...).Scan(&out.Total, &out.Matched); err != nil {
			return err
		}
		if err := f.counts(ctx, tx, with, args, out.Options); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, with+`SELECT `+rowColumns+` FROM s WHERE `+all+`
ORDER BY started_at DESC, src, id LIMIT ? OFFSET ?`,
			append(append(slices.Clone(args), allArgs...), limit, req.GetOffset())...)
		if err != nil {
			return err
		}
		if out.Sessions, err = scanRows(rows); err != nil {
			return err
		}
		out.Sessions, err = fit(out, out.GetSessions())
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSession answers svc.logbook.sessions.get: the session from the first of the
// sources, in their order, that has it.
func (s *Store) GetSession(ctx context.Context, req *storev1.GetSessionRequest) (*storev1.GetSessionResponse, error) {
	if err := checkSources(req.GetSources()); err != nil {
		return nil, err
	}
	version := s.DataVersion()
	if req.KnownDataVersion != nil && req.GetKnownDataVersion() == version {
		return &storev1.GetSessionResponse{DataVersion: version, Unchanged: true}, nil
	}
	out := &storev1.GetSessionResponse{DataVersion: version}
	err := s.read(ctx, func(tx *sql.Tx) (err error) {
		out.Session, err = findSession(ctx, tx, req.GetSources(), req.GetSessionId())
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListLaps answers svc.logbook.laps.list: the session and its laps, complete and
// partial, by number.
func (s *Store) ListLaps(ctx context.Context, req *storev1.ListLapsRequest) (*storev1.ListLapsResponse, error) {
	if err := checkSources(req.GetSources()); err != nil {
		return nil, err
	}
	version := s.DataVersion()
	if req.KnownDataVersion != nil && req.GetKnownDataVersion() == version {
		return &storev1.ListLapsResponse{DataVersion: version, Unchanged: true}, nil
	}
	out := &storev1.ListLapsResponse{DataVersion: version}
	err := s.read(ctx, func(tx *sql.Tx) (err error) {
		if out.Session, err = findSession(ctx, tx, req.GetSources(), req.GetSessionId()); err != nil {
			return err
		}
		out.Laps, err = lapsOf(ctx, tx, out.Session.GetSource(), req.GetSessionId())
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListSameTrack answers svc.logbook.sessions.same_track: the other sessions at the
// session's track, newest first, matched by track id where the detector was sure
// of both (track_sure, v1's sure_track_id); none when it was not sure of the
// session's own.
func (s *Store) ListSameTrack(ctx context.Context, req *storev1.ListSameTrackRequest) (*storev1.ListSameTrackResponse, error) {
	if err := checkSources(req.GetSources()); err != nil {
		return nil, err
	}
	limit, err := pageLimit(req.GetOffset(), req.GetLimit())
	if err != nil {
		return nil, err
	}
	version := s.DataVersion()
	if req.KnownDataVersion != nil && req.GetKnownDataVersion() == version {
		return &storev1.ListSameTrackResponse{DataVersion: version, Unchanged: true}, nil
	}
	out := &storev1.ListSameTrackResponse{DataVersion: version}
	err = s.read(ctx, func(tx *sql.Tx) error {
		row, err := findSession(ctx, tx, req.GetSources(), req.GetSessionId())
		if err != nil {
			return err
		}
		with, args := sessionsOf(req.GetSources())
		var track sql.NullInt32
		err = tx.QueryRowContext(ctx, with+`SELECT track_id FROM s WHERE src = ? AND id = ? AND track_sure`,
			append(slices.Clone(args), row.GetSource(), row.GetSessionId())...).Scan(&track)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !track.Valid {
			return nil
		}
		if err != nil {
			return err
		}
		same := ` FROM s WHERE track_sure AND track_id = ? AND id <> ?`
		args = append(args, track.Int32, row.GetSessionId())
		if err := tx.QueryRowContext(ctx, with+`SELECT count(*)`+same, args...).Scan(&out.Matched); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, with+`SELECT `+rowColumns+same+` ORDER BY started_at DESC, src, id LIMIT ? OFFSET ?`,
			append(args, limit, req.GetOffset())...)
		if err != nil {
			return err
		}
		if out.Sessions, err = scanRows(rows); err != nil {
			return err
		}
		out.Sessions, err = fit(out, out.GetSessions())
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetTrace answers svc.logbook.laps.trace: one lap's trace, thinned by v1's
// lap_trace query, run as v1 runs it against the lap's file (paddock#123).
func (s *Store) GetTrace(ctx context.Context, req *storev1.GetTraceRequest) (*storev1.GetTraceResponse, error) {
	if err := checkSources(req.GetSources()); err != nil {
		return nil, err
	}
	points := req.GetPoints()
	if points < 0 || points > MaxPoints {
		return nil, fmt.Errorf("%w: points %d is not 0 to %d", ErrBadRequest, points, MaxPoints)
	}
	if points == 0 {
		points = DefaultPoints
	}
	out := &storev1.GetTraceResponse{}
	err := s.read(ctx, func(tx *sql.Tx) error {
		row, err := findSession(ctx, tx, req.GetSources(), req.GetSessionId())
		if err != nil {
			return err
		}
		var file string
		var recordedAt time.Time
		var frames, firstSeq, lastSeq int64
		err = tx.QueryRowContext(ctx, `SELECT file, recorded_at, frames, first_seq, last_seq
FROM laps WHERE src = ? AND session_id = ? AND lap = ?`,
			row.GetSource(), req.GetSessionId(), req.GetLap()).Scan(&file, &recordedAt, &frames, &firstSeq, &lastSeq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: lap %d of session %s", ErrNotFound, req.GetLap(), req.GetSessionId())
		}
		if err != nil {
			return err
		}
		// The lap's own version (#123 R7), from its row: a lap written again with
		// other frames changes its count or its seqs, and its row is written after
		// its file, so the version never runs ahead of the frames.
		out.DataVersion = "lap." + strconv.FormatInt(recordedAt.UnixMicro(), 36) + "." +
			strconv.FormatInt(frames, 10) + "." + strconv.FormatInt(firstSeq, 10) + "." + strconv.FormatInt(lastSeq, 10)
		if req.KnownDataVersion != nil && req.GetKnownDataVersion() == out.DataVersion {
			out.Unchanged = true
			return nil
		}
		out.Trace, err = s.lapTrace(ctx, tx, file, int(points))
		if err != nil {
			return err
		}
		out.Trace.SessionId, out.Trace.Lap = req.GetSessionId(), req.GetLap()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// replyBudget is the most a list's reply may take as protobuf JSON: under NATS's
// 1 MiB payload limit, with room for the headers.
var replyBudget = 900 << 10

// fit is the sessions of reply, rows, cut to those that keep its JSON within
// replyBudget, in order: the contract answers "at most limit", and the caller
// pages on from offset plus what it got. A page always keeps its first row.
func fit(reply proto.Message, rows []*storev1.SessionRow) ([]*storev1.SessionRow, error) {
	b, err := protojson.Marshal(reply)
	if err != nil || len(b) <= replyBudget {
		return rows, err
	}
	// The reply without its rows, then each row and a comma.
	bare := proto.Clone(reply)
	bare.ProtoReflect().Clear(bare.ProtoReflect().Descriptor().Fields().ByName("sessions"))
	if b, err = protojson.Marshal(bare); err != nil {
		return nil, err
	}
	size := len(b) + len(`,"sessions":[]`)
	for i, r := range rows {
		rb, err := protojson.Marshal(r)
		if err != nil {
			return nil, err
		}
		if size += len(rb) + 1; size > replyBudget && i > 0 {
			return rows[:i], nil
		}
	}
	return rows, nil
}

// read runs f in a transaction on the reader, so each read sees one snapshot of
// the index. It refuses once the store is closed.
func (s *Store) read(ctx context.Context, f func(*sql.Tx) error) (err error) {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	if s.reader == nil {
		return errors.New("store: closed")
	}
	tx, err := s.reader.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: read: %w", err)
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	if err := f(tx); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrBadRequest) {
			return err
		}
		return fmt.Errorf("store: read: %w", err)
	}
	return nil
}

// checkSources refuses a request with no sources: twr.store.v1 makes it a bad
// request, not "every source".
func checkSources(sources []string) error {
	if len(sources) == 0 {
		return fmt.Errorf("%w: no sources", ErrBadRequest)
	}
	return nil
}

// pageLimit is a request's page size: 0 is DefaultLimit, and a negative offset or
// limit, or a limit over MaxLimit, is a bad request.
func pageLimit(offset, limit int32) (int32, error) {
	switch {
	case offset < 0:
		return 0, fmt.Errorf("%w: offset %d is negative", ErrBadRequest, offset)
	case limit < 0 || limit > MaxLimit:
		return 0, fmt.Errorf("%w: limit %d is not 0 to %d", ErrBadRequest, limit, MaxLimit)
	case limit == 0:
		return DefaultLimit, nil
	}
	return limit, nil
}

// sessionsOf is the sessions of sources as the CTE s, with the keys the filters
// read: track_key is the track's name when the detector was sure of it and the
// empty name otherwise, kind_key the type as stored (ecu's final verdict once the
// session ends, its first one before), and last_lap the highest lap stored.
func sessionsOf(sources []string) (string, []any) {
	args := make([]any, len(sources))
	for i, src := range sources {
		args[i] = src
	}
	return `WITH s AS (
    SELECT s.*,
        CASE WHEN s.track_sure THEN coalesce(s.track_name, '') ELSE '' END AS track_key,
        coalesce(s.session_kind, s.type) AS kind_key,
        (SELECT max(l.lap) FROM laps l WHERE l.src = s.src AND l.session_id = s.id) AS last_lap
    FROM sessions s WHERE s.src IN (` + placeholders(len(sources)) + `)
)
`, args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// rowColumns are the columns scanRows reads, in its order.
const rowColumns = `id, src, ended_at IS NOT NULL, end_reason, summary::VARCHAR, posted_at, last_lap,
    started_at, ended_at, kind_key, car_code, car_name, total_laps, laps_completed, started_fuel_at_start,
    joined_on_lap, fuel_capacity, best_lap, best_lap_ms, paused_ms, gaps::VARCHAR, pit_stops::VARCHAR,
    track_id, track_name, track_sure`

// findSession is the session id from the first of sources, in their order, that
// has it.
func findSession(ctx context.Context, tx *sql.Tx, sources []string, id string) (*storev1.SessionRow, error) {
	with, args := sessionsOf(sources)
	args = append(args, id)
	order := make([]string, len(sources))
	for i, src := range sources {
		order[i] = "WHEN ? THEN " + strconv.Itoa(i)
		args = append(args, src)
	}
	rows, err := tx.QueryContext(ctx, with+`SELECT `+rowColumns+` FROM s WHERE id = ?
ORDER BY CASE src `+strings.Join(order, " ")+` END LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	found, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: session %s in %s", ErrNotFound, id, strings.Join(sources, ", "))
	}
	return found[0], nil
}

// scanRows reads rowColumns' rows as SessionRows, and closes rows.
func scanRows(rows *sql.Rows) (out []*storev1.SessionRow, err error) {
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var (
			r                  storev1.SessionRow
			endReason, summary sql.NullString
			postedAt           sql.NullTime
			lastLap            sql.NullInt32
			o                  openRow
		)
		if err := rows.Scan(&r.SessionId, &r.Source, &r.Closed, &endReason, &summary, &postedAt, &lastLap,
			&o.startedAt, &o.endedAt, &o.kind, &o.carCode, &o.carName, &o.totalLaps, &o.lapsDone, &o.fuelAtStart,
			&o.joinedOnLap, &o.fuelCapacity, &o.bestLap, &o.bestLapMs, &o.pausedMs, &o.gaps, &o.pitStops,
			&o.trackID, &o.trackName, &o.trackSure); err != nil {
			return nil, err
		}
		if endReason.Valid {
			r.EndReason = eventsv1.EndReason(enumValue(eventsv1.EndReason_value, "END_REASON_", endReason.String))
		}
		if summary.Valid {
			// The summary its end carried, whole, but for its laps: ListLaps has them.
			r.Summary = &eventsv1.SessionSummary{}
			if err := protojson.Unmarshal([]byte(summary.String), r.Summary); err != nil {
				return nil, fmt.Errorf("session %s/%s: its summary: %w", r.Source, r.SessionId, err)
			}
			r.Summary.Laps = nil
		} else if r.Summary, err = o.summary(); err != nil {
			return nil, fmt.Errorf("session %s/%s: %w", r.Source, r.SessionId, err)
		}
		if postedAt.Valid {
			r.CardPostedAt = timestamppb.New(postedAt.Time.UTC())
		}
		if lastLap.Valid {
			r.LastLap = proto.Int32(lastLap.Int32)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// openRow is the columns an open session's summary is made from: what its events
// have said so far (schema 6, paddock#337).
type openRow struct {
	startedAt                                 time.Time
	endedAt                                   sql.NullTime
	kind, carName, gaps, pitStops, trackName  sql.NullString
	carCode, totalLaps, lapsDone, joinedOnLap sql.NullInt32
	bestLap, bestLapMs, pausedMs, trackID     sql.NullInt32
	fuelAtStart, fuelCapacity                 sql.NullFloat64
	trackSure                                 sql.NullBool
}

// summary is the summary of a session with no end, or one ended before schema 5
// that the backfill has not reached: what its stored events say so far, and
// nothing else (twr.store.v1's SessionRow):
//
//   - started_at, car_code, car_name, fuel_at_start, fuel_capacity and
//     joined_on_lap from session.started;
//   - session_type and total_laps from the last session.kind, else
//     session.started's;
//   - track from the last session.track, or a continue's carried track before
//     one. logbook keeps whether it was sure, not its confidence: a sure track
//     reads TRACK_CONFIDENCE_PROBABLE (at least probable), and one not sure
//     TRACK_CONFIDENCE_AMBIGUOUS;
//   - laps_completed, the complete laps stored;
//   - best_lap and best_lap_time_ms from the last lap.best, absent before one;
//   - paused_ms from session.paused and session.resumed;
//   - gaps from each session.continued, and pit_stops from each pit.exited.
//
// ended_at is the end's time for a session ended before schema 5; every other
// field is absent.
func (o openRow) summary() (*eventsv1.SessionSummary, error) {
	sum := &eventsv1.SessionSummary{
		StartedAt:     timestamppb.New(o.startedAt.UTC()),
		SessionType:   eventsv1.SessionType(enumValue(eventsv1.SessionType_value, "SESSION_TYPE_", o.kind.String)),
		CarCode:       o.carCode.Int32,
		CarName:       o.carName.String,
		LapsCompleted: o.lapsDone.Int32,
		FuelAtStart:   o.fuelAtStart.Float64,
		FuelCapacity:  o.fuelCapacity.Float64,
		PausedMs:      o.pausedMs.Int32,
		TotalLaps:     optionalInt(o.totalLaps),
		JoinedOnLap:   optionalInt(o.joinedOnLap),
	}
	if o.endedAt.Valid {
		sum.EndedAt = timestamppb.New(o.endedAt.Time.UTC())
	}
	// best_lap_ms alone is the laps' quickest, which logbook works out; only ecu's
	// own best (best_lap) is copied.
	if o.bestLap.Valid {
		sum.BestLap, sum.BestLapTimeMs = optionalInt(o.bestLap), optionalInt(o.bestLapMs)
	}
	if o.trackID.Valid || o.trackName.Valid || o.trackSure.Valid {
		sum.Track = &eventsv1.TrackVerdict{TrackId: optionalInt(o.trackID), Name: o.trackName.String}
		switch {
		case !o.trackSure.Valid:
		case o.trackSure.Bool:
			sum.Track.Confidence = eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE
		default:
			sum.Track.Confidence = eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS
		}
	}
	var err error
	if sum.Gaps, err = protoList(o.gaps, func() *eventsv1.SessionGap { return &eventsv1.SessionGap{} }); err != nil {
		return nil, fmt.Errorf("its gaps: %w", err)
	}
	if sum.PitStops, err = protoList(o.pitStops, func() *eventsv1.PitStop { return &eventsv1.PitStop{} }); err != nil {
		return nil, fmt.Errorf("its pit stops: %w", err)
	}
	return sum, nil
}

func optionalInt(v sql.NullInt32) *int32 {
	if !v.Valid {
		return nil
	}
	return proto.Int32(v.Int32)
}

// protoList reads a JSON column holding an array of messages in protobuf JSON;
// NULL is none.
func protoList[M proto.Message](col sql.NullString, newM func() M) ([]M, error) {
	if !col.Valid {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(col.String), &raw); err != nil {
		return nil, err
	}
	out := make([]M, len(raw))
	for i, b := range raw {
		out[i] = newM()
		if err := protojson.Unmarshal(b, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// enumValue is the number of an enum value stored by name (enumName): its name
// without prefix, in lower case, or its bare number for a value newer than the
// contracts logbook was built with.
func enumValue(values map[string]int32, prefix, name string) int32 {
	if v, ok := values[prefix+strings.ToUpper(name)]; ok {
		return v
	}
	n, _ := strconv.ParseInt(name, 10, 32)
	return int32(n)
}

// filters are ListSessionsRequest's filters, each a condition on the CTE s and
// its arguments, keyed by facet: the facet's own is left out of its counts.
type filters struct {
	conds map[string]string
	args  map[string][]any
}

// The facets the counts are for, and the date range, which has none.
const (
	facetTrack = "track"
	facetCar   = "car"
	facetKind  = "kind"
	facetDays  = "days"
)

func filtersOf(req *storev1.ListSessionsRequest) (filters, error) {
	f := filters{conds: map[string]string{}, args: map[string][]any{}}
	if req.Track != nil {
		f.conds[facetTrack], f.args[facetTrack] = "track_key = ?", []any{req.GetTrack()}
	}
	if req.Car != nil {
		f.conds[facetCar], f.args[facetCar] = "car_name = ?", []any{req.GetCar()}
	}
	if t := req.GetSessionType(); t != eventsv1.SessionType_SESSION_TYPE_UNSPECIFIED {
		f.conds[facetKind], f.args[facetKind] = "kind_key = ?", []any{enumName(t.String(), "SESSION_TYPE_")}
	}
	if req.GetFromDay() == "" && req.GetToDay() == "" {
		return f, nil
	}
	loc, err := time.LoadLocation(req.GetTimeZone())
	if err != nil {
		return f, fmt.Errorf("%w: time zone %q: %w", ErrBadRequest, req.GetTimeZone(), err)
	}
	var conds []string
	var args []any
	// A day in the viewer's zone is the instants from its midnight to the next
	// one, which started_at, in UTC, is compared with.
	if d := req.GetFromDay(); d != "" {
		from, err := time.ParseInLocation(time.DateOnly, d, loc)
		if err != nil {
			return f, fmt.Errorf("%w: from_day %q is not a day such as 2026-10-17", ErrBadRequest, d)
		}
		conds, args = append(conds, "started_at >= ?"), append(args, from.UTC())
	}
	if d := req.GetToDay(); d != "" {
		to, err := time.ParseInLocation(time.DateOnly, d, loc)
		if err != nil {
			return f, fmt.Errorf("%w: to_day %q is not a day such as 2026-10-17", ErrBadRequest, d)
		}
		conds, args = append(conds, "started_at < ?"), append(args, to.AddDate(0, 0, 1).UTC())
	}
	f.conds[facetDays], f.args[facetDays] = strings.Join(conds, " AND "), args
	return f, nil
}

// where is every filter but the facet left out ("" for none), ANDed, with its
// arguments in order.
func (f filters) where(leftOut string) (string, []any) {
	conds := []string{"true"}
	var args []any
	for _, k := range []string{facetTrack, facetCar, facetKind, facetDays} {
		if c, ok := f.conds[k]; ok && k != leftOut {
			conds, args = append(conds, c), append(args, f.args[k]...)
		}
	}
	return strings.Join(conds, " AND "), args
}

// counts fills each facet's values, from every session of the sources, with the
// sessions each would leave under the other filters (v1's option_counts, #318).
func (f filters) counts(ctx context.Context, tx *sql.Tx, with string, args []any, out *storev1.FilterCounts) error {
	facet := func(key, col, notNull string, add func(string, int32)) error {
		cond, condArgs := f.where(key)
		rows, err := tx.QueryContext(ctx, with+`SELECT `+col+`, count(*) FILTER (WHERE `+cond+`) FROM s`+notNull+`
GROUP BY `+col+` ORDER BY `+col, append(slices.Clone(args), condArgs...)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var v string
			var n int32
			if err := rows.Scan(&v, &n); err != nil {
				return err
			}
			add(v, n)
		}
		return rows.Err()
	}
	err := facet(facetTrack, "track_key", "", func(v string, n int32) {
		out.Track = append(out.Track, &storev1.ValueCount{Value: v, Count: n})
	})
	if err == nil {
		err = facet(facetCar, "car_name", " WHERE car_name IS NOT NULL", func(v string, n int32) {
			out.Car = append(out.Car, &storev1.ValueCount{Value: v, Count: n})
		})
	}
	if err == nil {
		err = facet(facetKind, "kind_key", "", func(v string, n int32) {
			t := eventsv1.SessionType(enumValue(eventsv1.SessionType_value, "SESSION_TYPE_", v))
			out.Kind = append(out.Kind, &storev1.KindCount{SessionType: t, Count: n})
		})
	}
	// Kinds in the enum's order, as v1 listed them in its labels' order.
	slices.SortFunc(out.Kind, func(a, b *storev1.KindCount) int { return int(a.GetSessionType() - b.GetSessionType()) })
	return err
}

// lapsOf is a session's laps by number, each its lap.completed as stored with the
// frames logbook counted.
func lapsOf(ctx context.Context, tx *sql.Tx, src, sessionID string) (out []*storev1.LapRow, err error) {
	rows, err := tx.QueryContext(ctx, `
SELECT lap, lap_time_ms, complete, fuel_start, fuel_end, fuel_used, position, pit_role, first_seq, last_seq,
    counts_for_best, frames, top_speed_kph
FROM laps WHERE src = ? AND session_id = ? ORDER BY lap`, src, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var (
			l                 = &eventsv1.LapCompleted{SessionId: sessionID}
			r                 = &storev1.LapRow{Lap: l}
			lapTime, position sql.NullInt32
			pitRole           sql.NullString
			firstSeq, lastSeq int64
			countsForBest     sql.NullBool
			topSpeed          sql.NullFloat64
		)
		if err := rows.Scan(&l.Lap, &lapTime, &l.Complete, &l.FuelAtStart, &l.FuelAtEnd, &l.FuelUsed, &position,
			&pitRole, &firstSeq, &lastSeq, &countsForBest, &r.Frames, &topSpeed); err != nil {
			return nil, err
		}
		l.LapTimeMs, l.CountsForBest = lapTime.Int32, countsForBest.Bool
		l.FirstSeq, l.LastSeq = uint32(firstSeq), uint32(lastSeq) //nolint:gosec // stored from the event's uint32
		if position.Valid {
			l.Position = proto.Int32(position.Int32)
		}
		if pitRole.Valid {
			l.PitRole = eventsv1.PitRole(enumValue(eventsv1.PitRole_value, "PIT_ROLE_", pitRole.String))
		}
		if topSpeed.Valid {
			r.TopSpeedKph = proto.Float64(topSpeed.Float64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
