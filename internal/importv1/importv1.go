// Package importv1 is `logbook import-v1` (paddock#264, ADR 0009): it brings v1's
// history into the store under the source v1-import, each session under v1's own
// id. A session's stored frames go back through salsa's v1 mapping and ecu's race
// state, the same code as live, and the store keeps what ecu makes of them. When
// ecu does not make exactly one session of them, or the frames lack what it needs,
// the session is taken from v1's own rows instead, flagged from_v1_rows (ADR 0009,
// points 4 and 5). There is no NATS: the import writes the store directly.
package importv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"github.com/those-who-run/ecu/race"
	"github.com/those-who-run/salsa"

	"github.com/those-who-run/logbook/internal/store"
)

// fuelTolerance is parity's (paddock#40): fuel is compared to 0.001 L. Lap times
// and lap counts compare exactly.
const fuelTolerance = 0.001

// Store is what the import writes through: the store's own writes, the same as
// `logbook run`'s, so an imported session is stored as a live one is.
type Store interface {
	StartSession(ctx context.Context, ev *eventsv1.SessionStarted) error
	WriteLap(ctx context.Context, src string, lap *eventsv1.LapCompleted, frames []*telemetryv1.Frame) error
	EndSession(ctx context.Context, src string, ev *eventsv1.SessionEnded, endedAt time.Time) error
	MarkImported(ctx context.Context, sessionID string, fromRows bool) error
	Imported(ctx context.Context, sessionID string) (imported, fromRows bool, err error)
}

// Outcome is what became of one v1 session.
type Outcome int

const (
	// Derived is a session re-derived from v1's frames through ecu.
	Derived Outcome = iota
	// FromRows is a session taken from v1's rows, flagged (ADR 0009, points 4 and 5).
	FromRows
	// LeftOut is a session not imported; Reason says why.
	LeftOut
	// Already is a session an earlier import wrote; this one changed nothing.
	Already
	// Failed is a session a file of which could not be read; it is left out, and the import
	// exits 1 (R5).
	Failed
)

var outcomeNames = map[Outcome]string{
	Derived: "re-derived", FromRows: "from v1's rows", LeftOut: "left out", Already: "already imported",
	Failed: "failed to read",
}

func (o Outcome) String() string { return outcomeNames[o] }

// Session is one v1 session's line in the summary (R5).
type Session struct {
	ID      string
	Outcome Outcome
	// Reason says why a session is from v1's rows, left out or failed.
	Reason string
	// Laps is the complete laps imported, and V1Laps v1's.
	Laps, V1Laps int32
	// Differences are where ecu's session differs from v1's rows beyond the
	// parity tolerances: lap count, lap times and fuel.
	Differences []string
}

// Summary is the import's: one line per v1 session, oldest first.
type Summary struct {
	Sessions []Session
}

// Count is how many sessions had the outcome.
func (s Summary) Count(o Outcome) int {
	n := 0
	for _, x := range s.Sessions {
		if x.Outcome == o {
			n++
		}
	}
	return n
}

// Write prints the summary: a line per session, its differences under it, and
// the counts last. The counts line alone says nothing of any one session.
func (s Summary) Write(w io.Writer) error {
	var b strings.Builder
	for _, x := range s.Sessions {
		fmt.Fprintf(&b, "%s %s", x.ID, x.Outcome)
		switch x.Outcome {
		case Derived, FromRows:
			fmt.Fprintf(&b, ": %d laps (v1 %d)", x.Laps, x.V1Laps)
		}
		if x.Reason != "" {
			fmt.Fprintf(&b, ": %s", x.Reason)
		}
		b.WriteString("\n")
		for _, d := range x.Differences {
			fmt.Fprintf(&b, "  differs: %s\n", d)
		}
	}
	differs := 0
	for _, x := range s.Sessions {
		if len(x.Differences) > 0 {
			differs++
		}
	}
	fmt.Fprintf(&b, "v1 sessions: %d; re-derived: %d (differing from v1: %d); from v1's rows: %d; left out: %d; "+
		"already imported: %d; failed to read: %d\n",
		len(s.Sessions), s.Count(Derived), differs, s.Count(FromRows), s.Count(LeftOut), s.Count(Already), s.Count(Failed))
	_, err := io.WriteString(w, b.String())
	return err
}

// Run imports v1's data directory from into st, or with st nil (--dry-run) works
// out the same summary and writes nothing. It reads from and never writes there.
// It refuses a v1 index still being written (R1). An error is the import failing
// as a whole; a session that failed is in the summary.
func Run(ctx context.Context, from string, st Store) (Summary, error) {
	if err := checkV1Dir(from); err != nil {
		return Summary{}, err
	}
	ids, err := v1SessionIDs(from)
	if err != nil {
		return Summary{}, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = db.Close() }()
	var sum Summary
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		s, err := importSession(ctx, db, from, id, st)
		if err != nil {
			return sum, fmt.Errorf("session %s: %w", id, err)
		}
		sum.Sessions = append(sum.Sessions, s)
	}
	return sum, nil
}

// importSession imports one v1 session. Its error is the store failing; what
// becomes of the session is its Session.
func importSession(ctx context.Context, db *sql.DB, from, id string, st Store) (Session, error) {
	out := Session{ID: id}
	if st != nil {
		done, fromRows, err := st.Imported(ctx, id)
		if err != nil {
			return out, err
		}
		if done {
			out.Outcome, out.Reason = Already, map[bool]string{true: "from v1's rows", false: "re-derived"}[fromRows]
			return out, nil
		}
	}
	v1, err := readSummary(from, id)
	switch {
	case errors.Is(err, os.ErrNotExist):
		out.Outcome, out.Reason = LeftOut, "no summary.json: v1 never ended it"
		return out, nil
	case err != nil:
		out.Outcome, out.Reason = Failed, err.Error()
		return out, nil
	}
	out.V1Laps = v1.LapsCompleted
	if len(v1.Laps) == 0 {
		out.Outcome, out.Reason = LeftOut, "no lap files"
		return out, nil
	}
	var (
		frames  []v1Frame
		lapEnds []int // the index in frames after each of v1.Laps' files
		missing []string
	)
	for _, l := range v1.Laps {
		path := filepath.Join(from, v1Sessions, id, filepath.Base(l.File))
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			out.Outcome, out.Reason = LeftOut, l.File+" is missing"
			return out, nil
		}
		f, lacking, err := readLapFile(ctx, db, path)
		if err != nil {
			out.Outcome, out.Reason = Failed, fmt.Sprintf("%s: %v", l.File, err)
			return out, nil
		}
		frames = append(frames, f...)
		lapEnds = append(lapEnds, len(frames))
		for _, c := range lacking {
			if !contains(missing, c) {
				missing = append(missing, c)
			}
		}
	}
	d, err := derive(frames, v1, id)
	if err != nil {
		out.Outcome, out.Reason = Failed, err.Error()
		return out, nil
	}
	reason := d.oneToOne()
	if len(missing) > 0 {
		reason = "its lap files lack " + strings.Join(missing, ", ")
	}
	if reason == "" {
		out.Outcome, out.Laps = Derived, d.ended.GetSummary().GetLapsCompleted()
		out.Differences = differences(d, v1)
		if st != nil {
			err = writeDerived(ctx, st, d)
		}
		return out, err
	}
	out.Outcome, out.Reason, out.Laps = FromRows, reason, v1.LapsCompleted
	if st != nil {
		err = writeRows(ctx, st, v1, d.frames, lapEnds)
	}
	return out, err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// derived is what ecu's race state made of a v1 session's frames: every frame,
// and the session events the store keeps, with ecu's session id replaced by v1's.
type derived struct {
	frames    []*telemetryv1.Frame
	starts    int
	discarded bool
	started   *eventsv1.SessionStarted
	laps      []*eventsv1.LapCompleted
	ended     *eventsv1.SessionEnded
	endedAt   time.Time
}

// derive runs a v1 session's frames through salsa's v1 mapping and ecu's race
// state, as `ecu replay` does at --speed max: each frame dated v1's start plus its
// time since the first, the open session ended as the input ends.
func derive(frames []v1Frame, v1 *v1SessionSummary, id string) (*derived, error) {
	rec, err := recordingOf(frames, factsOf(v1))
	if err != nil {
		return nil, err
	}
	src, err := salsa.NewRecording(bytes.NewReader(rec), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	start := time.Unix(0, int64(math.Round(v1.StartedAt*1e9))).UTC()
	now := start
	seed := sha256.Sum256([]byte("logbook import-v1\x00" + id))
	m := race.New(race.NewIDs(func() time.Time { return now }, rand.NewChaCha8(seed)))
	d := &derived{}
	first, t0 := true, 0.0
	for {
		r, err := src.Next(context.Background())
		if errors.Is(err, io.EOF) {
			d.see(m.EndOfInput(), id)
			return d, nil
		}
		if err != nil {
			return nil, err
		}
		if first {
			first, t0 = false, r.T
		}
		now = start.Add(time.Duration(math.Round((r.T - t0) * 1e9)))
		d.see(m.Step(race.Received{Received: r, Time: now}), id)
	}
}

// see keeps what the store needs of a step's outputs, under v1's id.
func (d *derived) see(outs []race.Output, id string) {
	for _, o := range outs {
		switch {
		case o.Frame != nil:
			d.frames = append(d.frames, o.Frame)
		case o.Started != nil:
			d.starts++
			if d.started == nil {
				d.started = o.Started
				d.started.SessionId, d.started.Source = id, store.SourceV1Import
			}
		case o.Discarded != nil:
			d.discarded = true
		case o.Lap != nil:
			if d.starts == 1 {
				o.Lap.SessionId = id
				d.laps = append(d.laps, o.Lap)
			}
		case o.Ended != nil:
			if d.starts == 1 && d.ended == nil {
				d.ended, d.endedAt = o.Ended, o.Event.Time
				d.ended.SessionId = id
				for _, l := range d.ended.GetSummary().GetLaps() {
					l.SessionId = id
				}
			}
		}
	}
}

// oneToOne is "" when ecu made exactly one session of v1's, which it ended, and
// otherwise why not (ADR 0009, point 4).
func (d *derived) oneToOne() string {
	switch {
	case d.discarded:
		return "ecu discarded it as a GT7 replay or demo"
	case d.starts == 0:
		return "ecu started no session from its frames"
	case d.starts > 1:
		return fmt.Sprintf("ecu split it into %d sessions", d.starts)
	case d.ended == nil:
		return "ecu did not end it"
	}
	return ""
}

// differences are where ecu's session differs from v1's rows beyond the parity
// tolerances: the complete laps, each lap's time, and each lap's fuel used.
func differences(d *derived, v1 *v1SessionSummary) []string {
	var out []string
	if n := d.ended.GetSummary().GetLapsCompleted(); n != v1.LapsCompleted {
		out = append(out, fmt.Sprintf("complete laps %d, v1 %d", n, v1.LapsCompleted))
	}
	ours := map[int32]*eventsv1.LapCompleted{}
	for _, l := range d.laps {
		ours[l.GetLap()] = l
	}
	for _, l := range v1.Laps {
		o, ok := ours[l.Lap]
		if !ok {
			out = append(out, fmt.Sprintf("lap %d: none, v1 has it", l.Lap))
			continue
		}
		v1Time := int32(0)
		if l.LapTimeMs != nil {
			v1Time = *l.LapTimeMs
		}
		if o.GetLapTimeMs() != v1Time {
			out = append(out, fmt.Sprintf("lap %d: time %d ms, v1 %d ms", l.Lap, o.GetLapTimeMs(), v1Time))
		}
		if l.FuelUsed != nil && math.Abs(o.GetFuelUsed()-*l.FuelUsed) > fuelTolerance+1e-9 {
			out = append(out, fmt.Sprintf("lap %d: fuel used %.3f L, v1 %.3f L", l.Lap, o.GetFuelUsed(), *l.FuelUsed))
		}
		delete(ours, l.Lap)
	}
	for _, l := range d.laps {
		if _, extra := ours[l.GetLap()]; extra {
			out = append(out, fmt.Sprintf("lap %d: v1 has none", l.GetLap()))
		}
	}
	return out
}

// writeDerived stores what ecu made of the session, through the writes `logbook
// run` makes for its events, then marks it imported.
func writeDerived(ctx context.Context, st Store, d *derived) error {
	if err := st.StartSession(ctx, d.started); err != nil {
		return err
	}
	for _, l := range d.laps {
		if err := st.WriteLap(ctx, store.SourceV1Import, l, d.frames); err != nil {
			return err
		}
	}
	if err := st.EndSession(ctx, store.SourceV1Import, d.ended, d.endedAt); err != nil {
		return err
	}
	return st.MarkImported(ctx, d.started.GetSessionId(), false)
}
