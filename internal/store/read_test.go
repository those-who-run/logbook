package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// diff is "" when want and got are equal, else both.
func diff(want, got proto.Message) string {
	if proto.Equal(want, got) {
		return ""
	}
	return "want " + prototext.Format(want) + "\n got " + prototext.Format(got)
}

var live = []string{SourceLive}

// at is a session like race's, with its own id, start, car, kind and track: a
// nil track is one the detector placed nowhere, and sure is its confidence.
func at(id string, started time.Time, car string, kind eventsv1.SessionType, track *eventsv1.TrackVerdict) session {
	s := race(SourceLive, id)
	s.started.StartedAt, s.started.CarName = timestamppb.New(started), car
	s.ended.LastSeq = 9999
	s.ended.Summary.SessionType, s.ended.Summary.CarName, s.ended.Summary.Track = kind, car, track
	s.ended.Summary.StartedAt = s.started.GetStartedAt()
	s.ended.Summary.Laps = []*eventsv1.LapCompleted{s.laps[0]}
	return s
}

func trackOf(id int32, name string, c eventsv1.TrackConfidence) *eventsv1.TrackVerdict {
	return &eventsv1.TrackVerdict{TrackId: proto.Int32(id), Name: name, Confidence: c}
}

const (
	idSuzuka1 = "01JB8Z3K5Q9T2M7XW4R6N1PDH1"
	idSuzuka2 = "01JB8Z3K5Q9T2M7XW4R6N1PDH2"
	idSugo    = "01JB8Z3K5Q9T2M7XW4R6N1PDH3"
	idUnsure  = "01JB8Z3K5Q9T2M7XW4R6N1PDH4"
	idOpen    = "01JB8Z3K5Q9T2M7XW4R6N1PDH5"
)

var (
	suzuka = trackOf(10, "Suzuka Circuit", eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED)
	sugo   = trackOf(20, "Sportsland SUGO", eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE)
	guess  = trackOf(10, "Suzuka Circuit", eventsv1.TrackConfidence_TRACK_CONFIDENCE_AMBIGUOUS)
)

// day is 19:30 UTC on the 15th plus n days: 04:30 the next day in Tokyo.
func day(n int) time.Time { return time.Date(2026, 10, 15+n, 19, 30, 0, 0, time.UTC) }

// history writes four ended sessions and one open one, oldest first.
func history(t *testing.T, s *Store) {
	t.Helper()
	at(idSuzuka1, day(0), "Mazda RX-VISION GT3", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, suzuka).write(t, s)
	at(idSugo, day(1), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, sugo).write(t, s)
	at(idUnsure, day(2), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, guess).write(t, s)
	at(idSuzuka2, day(3), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_RANKED, suzuka).write(t, s)
	open := at(idOpen, day(4), "Mazda RX-VISION GT3", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, nil)
	open.started.SessionType = eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD
	open.started.FuelAtStart = 42
	if err := s.StartSession(t.Context(), open.started); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLap(t.Context(), SourceLive, open.laps[0], open.frames); err != nil {
		t.Fatal(err)
	}
}

func ids(rows []*storev1.SessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.GetSessionId()
	}
	return out
}

// AC6 (#122 R8): an empty store answers an empty list with total 0, not an
// error.
func TestAnEmptyStoreAnswersAnEmptyList(t *testing.T) {
	s := open(t, t.TempDir())
	got, err := s.ListSessions(t.Context(), &storev1.ListSessionsRequest{Sources: live})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetSessions()) != 0 || got.GetTotal() != 0 || got.GetMatched() != 0 || got.GetDataVersion() == "" {
		t.Errorf("empty store answered %v", got)
	}
}

// R1: newest first, every filter applied in the query, each filter's values
// counted under the other filters, total and matched.
func TestListSessionsFiltersAndCounts(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	ctx := t.Context()

	all, err := s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idOpen, idSuzuka2, idUnsure, idSugo, idSuzuka1}; !slices.Equal(ids(all.GetSessions()), want) {
		t.Errorf("sessions %q, want newest first %q", ids(all.GetSessions()), want)
	}
	if all.GetTotal() != 5 || all.GetMatched() != 5 {
		t.Errorf("total %d matched %d, want 5 and 5", all.GetTotal(), all.GetMatched())
	}

	got, err := s.ListSessions(ctx, &storev1.ListSessionsRequest{
		Sources: live, Car: proto.String("Porsche 911 RSR"), Track: proto.String("Suzuka Circuit"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.GetSessions()), []string{idSuzuka2}) || got.GetTotal() != 5 || got.GetMatched() != 1 {
		t.Errorf("Porsche at Suzuka: %q, total %d, matched %d", ids(got.GetSessions()), got.GetTotal(), got.GetMatched())
	}
	// The track's values with the car filter only: the open session and the
	// possible Suzuka have no sure track, the empty name.
	wantOptions := &storev1.FilterCounts{
		Track: []*storev1.ValueCount{{Value: "", Count: 1}, {Value: "Sportsland SUGO", Count: 1}, {Value: "Suzuka Circuit", Count: 1}},
		Car:   []*storev1.ValueCount{{Value: "Mazda RX-VISION GT3", Count: 1}, {Value: "Porsche 911 RSR", Count: 1}},
		Kind: []*storev1.KindCount{
			{SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, Count: 0},
			{SessionType: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD, Count: 0},
			{SessionType: eventsv1.SessionType_SESSION_TYPE_SOLO_TIMED, Count: 0},
			{SessionType: eventsv1.SessionType_SESSION_TYPE_RANKED, Count: 1},
		},
	}
	if d := diff(wantOptions, got.GetOptions()); d != "" {
		t.Errorf("options :\n%s", d)
	}

	// The kind filter, and the track the detector was not sure of.
	got, err = s.ListSessions(ctx, &storev1.ListSessionsRequest{
		Sources: live, Track: proto.String(""), SessionType: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.GetSessions()), []string{idOpen}) {
		t.Errorf("open timed field with no sure track: %q", ids(got.GetSessions()))
	}

	// Days in the viewer's zone: each start is 04:30 the next day in Tokyo.
	got, err = s.ListSessions(ctx, &storev1.ListSessionsRequest{
		Sources: live, FromDay: "2026-10-17", ToDay: "2026-10-18", TimeZone: "Asia/Tokyo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idUnsure, idSugo}; !slices.Equal(ids(got.GetSessions()), want) {
		t.Errorf("17th to 18th in Tokyo: %q, want %q", ids(got.GetSessions()), want)
	}
	got, err = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, FromDay: "2026-10-18", ToDay: "2026-10-18"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idSuzuka2}; !slices.Equal(ids(got.GetSessions()), want) {
		t.Errorf("the 18th in UTC: %q, want %q", ids(got.GetSessions()), want)
	}

	// Pages: from offset, at most limit, with matched for the whole.
	got, err = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Offset: 1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idSuzuka2, idUnsure}; !slices.Equal(ids(got.GetSessions()), want) || got.GetMatched() != 5 {
		t.Errorf("page 2 of 2: %q matched %d, want %q matched 5", ids(got.GetSessions()), got.GetMatched(), want)
	}

	// Only the sources asked for.
	got, err = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: []string{"parity", "v1-import"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetSessions()) != 0 || got.GetTotal() != 0 {
		t.Errorf("other sources answered %q", ids(got.GetSessions()))
	}
}

// A request the reads refuse is a bad request; what the sources do not have is
// not found.
func TestBadRequestsAndNotFound(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	ctx := t.Context()
	bad := map[string]error{}
	_, bad["no sources"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{})
	_, bad["limit 501"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Limit: 501})
	_, bad["negative limit"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Limit: -1})
	_, bad["negative offset"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Offset: -1})
	_, bad["time zone"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, FromDay: "2026-10-15", TimeZone: "Mars/Olympus"})
	_, bad["from day"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, FromDay: "15/10/2026"})
	_, bad["to day"] = s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, ToDay: "tomorrow"})
	_, bad["get, no sources"] = s.GetSession(ctx, &storev1.GetSessionRequest{SessionId: idSugo})
	_, bad["laps, no sources"] = s.ListLaps(ctx, &storev1.ListLapsRequest{SessionId: idSugo})
	_, bad["same track, no sources"] = s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{SessionId: idSugo})
	_, bad["same track, limit 501"] = s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{Sources: live, SessionId: idSugo, Limit: 501})
	_, bad["trace, no sources"] = s.GetTrace(ctx, &storev1.GetTraceRequest{SessionId: idSugo, Lap: 1})
	_, bad["trace, 10001 points"] = s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: live, SessionId: idSugo, Lap: 1, Points: 10001})
	for name, err := range bad {
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v, want a bad request", name, err)
		}
	}
	missing := map[string]error{}
	_, missing["get"] = s.GetSession(ctx, &storev1.GetSessionRequest{Sources: live, SessionId: "20261017-162118"})
	_, missing["get, another source"] = s.GetSession(ctx, &storev1.GetSessionRequest{Sources: []string{"parity"}, SessionId: idSugo})
	_, missing["laps"] = s.ListLaps(ctx, &storev1.ListLapsRequest{Sources: live, SessionId: "nope"})
	_, missing["same track"] = s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{Sources: live, SessionId: "nope"})
	_, missing["trace, session"] = s.GetTrace(ctx, &storev1.GetTraceRequest{Sources: live, SessionId: "nope", Lap: 1})
	for name, err := range missing {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
}

// R2: an ended session is its row with its end's summary, laps left out; an open
// one says so, with what its start and laps say.
func TestGetSessionMapsWhatIsStored(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	ctx := t.Context()
	posted := time.Date(2026, 10, 16, 19, 36, 0, 0, time.UTC)
	if err := s.MarkPosted(ctx, SourceLive, &eventsv1.CardPosted{SessionId: idSugo, PostedAt: timestamppb.New(posted)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSession(ctx, &storev1.GetSessionRequest{Sources: []string{"parity", SourceLive}, SessionId: idSugo})
	if err != nil {
		t.Fatal(err)
	}
	ended := at(idSugo, day(1), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, sugo).ended
	ended.Summary.Laps = nil
	want := &storev1.SessionRow{
		SessionId: idSugo, Source: SourceLive, Closed: true, EndReason: eventsv1.EndReason_END_REASON_FINISHED,
		Summary: ended.GetSummary(), CardPostedAt: timestamppb.New(posted), LastLap: proto.Int32(3),
	}
	if d := diff(want, got.GetSession()); d != "" {
		t.Errorf("ended session :\n%s", d)
	}

	got, err = s.GetSession(ctx, &storev1.GetSessionRequest{Sources: live, SessionId: idOpen})
	if err != nil {
		t.Fatal(err)
	}
	want = &storev1.SessionRow{
		SessionId: idOpen, Source: SourceLive,
		Summary: &eventsv1.SessionSummary{
			StartedAt: timestamppb.New(day(4)), SessionType: eventsv1.SessionType_SESSION_TYPE_TIMED_FIELD,
			CarCode: 3431, CarName: "Mazda RX-VISION GT3", TotalLaps: proto.Int32(3), LapsCompleted: 1, FuelAtStart: 42,
		},
		LastLap: proto.Int32(1),
	}
	if d := diff(want, got.GetSession()); d != "" {
		t.Errorf("open session, its start and a lap:\n%s", d)
	}

	// What its events say from then on (schema 6): the kind, the track, the
	// best lap, the pauses, a continue's gap and the pit stops.
	gap := &eventsv1.SessionGap{StartedAt: timestamppb.New(day(4).Add(time.Minute)),
		EndedAt: timestamppb.New(day(4).Add(2 * time.Minute)), Lap: 2, Reason: eventsv1.GapReason_GAP_REASON_ENGINEER_RESTART}
	stops := []*eventsv1.PitStop{{Stop: 1, InLap: 1, FuelAdded: 20}, {Stop: 2, InLap: 3, OutLap: proto.Int32(4)}}
	for _, ev := range []proto.Message{
		&eventsv1.SessionKindChanged{SessionId: idOpen, Kind: &eventsv1.SessionKind{
			SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE, TotalLaps: proto.Int32(5)}},
		&eventsv1.SessionTrackChanged{SessionId: idOpen, Track: &eventsv1.TrackVerdict{
			TrackId: proto.Int32(10), Name: "Suzuka Circuit", Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED}},
		&eventsv1.LapBestChanged{SessionId: idOpen, Lap: 1, LapTimeMs: 98_100},
		&eventsv1.SessionPaused{SessionId: idOpen, PausedMs: 1500},
		&eventsv1.SessionContinued{SessionId: idOpen, RunId: "r", Gap: gap},
		&eventsv1.PitExited{SessionId: idOpen, Stop: stops[0]},
		&eventsv1.PitExited{SessionId: idOpen, Stop: stops[1]},
	} {
		if _, err := s.ApplyFact(ctx, SourceLive, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.GetSession(ctx, &storev1.GetSessionRequest{Sources: live, SessionId: idOpen})
	if err != nil {
		t.Fatal(err)
	}
	sum := want.GetSummary()
	sum.SessionType, sum.TotalLaps = eventsv1.SessionType_SESSION_TYPE_LAP_RACE, proto.Int32(5)
	// logbook keeps whether the track was sure, not its confidence.
	sum.Track = &eventsv1.TrackVerdict{TrackId: proto.Int32(10), Name: "Suzuka Circuit",
		Confidence: eventsv1.TrackConfidence_TRACK_CONFIDENCE_PROBABLE}
	sum.BestLap, sum.BestLapTimeMs, sum.PausedMs = proto.Int32(1), proto.Int32(98_100), 1500
	sum.Gaps, sum.PitStops = []*eventsv1.SessionGap{gap}, stops
	if d := diff(want, got.GetSession()); d != "" {
		t.Errorf("open session, with its facts:\n%s", d)
	}
	// The kind and the track filters read the facts too.
	list, err := s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Track: proto.String("Suzuka Circuit"),
		SessionType: eventsv1.SessionType_SESSION_TYPE_LAP_RACE})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idOpen, idSuzuka1}; !slices.Equal(ids(list.GetSessions()), want) {
		t.Errorf("lap races at Suzuka: %q, want %q", ids(list.GetSessions()), want)
	}
}

// A session started with what contracts' later SessionStarted carries: the lap
// it was joined on and the tank's capacity.
func TestAnOpenSessionKeepsItsStartsFacts(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race(SourceLive, sid)
	sess.started.JoinedPartWay, sess.started.JoinedOnLap, sess.started.FuelCapacity = true, proto.Int32(7), 100
	if err := s.StartSession(t.Context(), sess.started); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(t.Context(), &storev1.GetSessionRequest{Sources: live, SessionId: sid})
	if err != nil {
		t.Fatal(err)
	}
	if sum := got.GetSession().GetSummary(); sum.GetJoinedOnLap() != 7 || sum.GetFuelCapacity() != 100 || sum.BestLap != nil ||
		sum.GetTrack() != nil || got.GetSession().LastLap != nil {
		t.Errorf("joined on lap 7 with a 100 L tank: %v", got.GetSession())
	}
}

// R3: a session's laps in order, each its lap.completed as stored, with its
// frames and top speed, and the session.
func TestListLapsMapsTheEvents(t *testing.T) {
	s := open(t, t.TempDir())
	sess := race(SourceLive, sid)
	sess.laps[1].PitRole, sess.laps[1].CountsForBest = eventsv1.PitRole_PIT_ROLE_IN_LAP, false
	sess.laps[0].CountsForBest = true
	sess.write(t, s)
	got, err := s.ListLaps(t.Context(), &storev1.ListLapsRequest{Sources: live, SessionId: sid})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSession().GetSessionId() != sid || len(got.GetLaps()) != 3 {
		t.Fatalf("session %s with %d laps, want %s with 3", got.GetSession().GetSessionId(), len(got.GetLaps()), sid)
	}
	for i, l := range got.GetLaps() {
		if d := diff(sess.laps[i], l.GetLap()); d != "" {
			t.Errorf("lap %d :\n%s", i+1, d)
		}
		if l.GetFrames() == 0 || l.TopSpeedKph == nil {
			t.Errorf("lap %d: %d frames, top speed %v", i+1, l.GetFrames(), l.TopSpeedKph)
		}
	}
}

// R4, AC4: two sessions at the same sure track list each other; one the
// detector was not sure of lists none, and is listed by none.
func TestSameTrackOnlyWhereSure(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	for id, want := range map[string][]string{
		idSuzuka1: {idSuzuka2},
		idSuzuka2: {idSuzuka1},
		idSugo:    nil,
		idUnsure:  nil,
		idOpen:    nil,
	} {
		got, err := s.ListSameTrack(t.Context(), &storev1.ListSameTrackRequest{Sources: live, SessionId: id})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got.GetSessions()), want) || int(got.GetMatched()) != len(want) {
			t.Errorf("same track as %s: %q matched %d, want %q", id, ids(got.GetSessions()), got.GetMatched(), want)
		}
	}
}

// AC5 (#122 R7): the data version is unchanged by a read and changes when a lap
// is written; a caller that has it is told unchanged, and nothing else.
func TestTheDataVersion(t *testing.T) {
	s := open(t, t.TempDir())
	history(t, s)
	ctx := t.Context()
	req := &storev1.ListSessionsRequest{Sources: live}
	first, err := s.ListSessions(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.KnownDataVersion = proto.String(first.GetDataVersion())
	again, err := s.ListSessions(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d := diff(&storev1.ListSessionsResponse{DataVersion: first.GetDataVersion(), Unchanged: true}, again); d != "" {
		t.Errorf("a read with the current version :\n%s", d)
	}
	for name, unchanged := range map[string]func() (bool, error){
		"get": func() (bool, error) {
			r, err := s.GetSession(ctx, &storev1.GetSessionRequest{Sources: live, SessionId: idSugo, KnownDataVersion: req.KnownDataVersion})
			return r.GetUnchanged() && r.GetSession() == nil, err
		},
		"laps": func() (bool, error) {
			r, err := s.ListLaps(ctx, &storev1.ListLapsRequest{Sources: live, SessionId: idSugo, KnownDataVersion: req.KnownDataVersion})
			return r.GetUnchanged() && r.GetLaps() == nil, err
		},
		"same track": func() (bool, error) {
			r, err := s.ListSameTrack(ctx, &storev1.ListSameTrackRequest{Sources: live, SessionId: idSugo, KnownDataVersion: req.KnownDataVersion})
			return r.GetUnchanged() && r.GetSessions() == nil, err
		},
	} {
		if ok, err := unchanged(); err != nil || !ok {
			t.Errorf("%s with the current version: unchanged %v, %v", name, ok, err)
		}
	}

	open := at(idOpen, day(4), "Mazda RX-VISION GT3", eventsv1.SessionType_SESSION_TYPE_LAP_RACE, nil)
	if err := s.WriteLap(ctx, SourceLive, open.laps[1], open.frames); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListSessions(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if after.GetUnchanged() || after.GetDataVersion() == first.GetDataVersion() || len(after.GetSessions()) != 5 {
		t.Errorf("after a lap: version %s (was %s), unchanged %v, %d sessions",
			after.GetDataVersion(), first.GetDataVersion(), after.GetUnchanged(), len(after.GetSessions()))
	}
}

// A read after Close is refused, not a crash.
func TestAReadAfterCloseIsRefused(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListSessions(t.Context(), &storev1.ListSessionsRequest{Sources: live}); err == nil {
		t.Error("a read after Close answered")
	}
}

// A page of 500 large sessions (8 track candidates and 6 pit stops each) would
// pass NATS's 1 MiB limit: the reply stops short of it, and the pages that
// follow from offset plus what came give every session once.
func TestALargePageStopsShortOfTheLimit(t *testing.T) {
	s := open(t, t.TempDir())
	ctx := t.Context()
	const n = 500
	for i := range n {
		id := fmt.Sprintf("01JB8Z3K5Q9T2M7XW4R6N1%04d", i)
		sess := at(id, day(0).Add(time.Duration(i)*time.Minute), "Porsche 911 RSR", eventsv1.SessionType_SESSION_TYPE_LAP_RACE,
			trackOf(10, "Suzuka Circuit", eventsv1.TrackConfidence_TRACK_CONFIDENCE_CONFIRMED))
		sum := sess.ended.GetSummary()
		sum.Track.Evidence = strings.Repeat("the start line and the hairpin fit; ", 4)
		for c := range 8 {
			sum.Track.Candidates = append(sum.Track.Candidates, &eventsv1.TrackCandidate{
				TrackId: int32(c), Name: fmt.Sprintf("Candidate circuit layout %d", c), Fit: 0.123456789 * float64(c), LineCrossed: true,
			})
		}
		for p := range 6 {
			sum.PitStops = append(sum.PitStops, &eventsv1.PitStop{
				Stop: int32(p + 1), InLap: int32(p*5 + 4), OutLap: proto.Int32(int32(p*5 + 5)), PitLaneMs: proto.Int32(31234),
				StationaryMs: 12345, FuelBefore: 12.345678, FuelAfter: 98.765432, FuelAdded: 86.419754, TyresChanged: true,
				PitLaneKph: proto.Float64(59.87654321), PositionBefore: proto.Int32(3), PositionAfter: proto.Int32(7),
			})
		}
		sum.Kind = &eventsv1.SessionKind{SessionType: sum.GetSessionType(), Evidence: strings.Repeat("a lap total and a field; ", 4)}
		if err := s.StartSession(ctx, sess.started); err != nil {
			t.Fatal(err)
		}
		if err := s.EndSession(ctx, SourceLive, sess.ended, sess.endedAt); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for offset := int32(0); offset < n; {
		got, err := s.ListSessions(ctx, &storev1.ListSessionsRequest{Sources: live, Offset: offset, Limit: MaxLimit})
		if err != nil {
			t.Fatal(err)
		}
		b, err := protojson.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > replyBudget || len(got.GetSessions()) == 0 || got.GetMatched() != n {
			t.Fatalf("from %d: %d bytes, %d sessions, matched %d", offset, len(b), len(got.GetSessions()), got.GetMatched())
		}
		if offset == 0 && len(got.GetSessions()) == n {
			t.Errorf("the first page holds all %d sessions in %d bytes: the rows are not large enough to test", n, len(b))
		}
		for _, r := range got.GetSessions() {
			if seen[r.GetSessionId()] {
				t.Errorf("%s twice", r.GetSessionId())
			}
			seen[r.GetSessionId()] = true
		}
		offset += int32(len(got.GetSessions())) //nolint:gosec // at most 500
	}
	if len(seen) != n {
		t.Errorf("%d sessions across the pages, want %d", len(seen), n)
	}
}
