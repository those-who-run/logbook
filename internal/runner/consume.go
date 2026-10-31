package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
	"google.golang.org/protobuf/proto"

	"github.com/those-who-run/logbook/internal/store"
)

// storeAPI is what the runner needs of the store. The tests put a store in front
// of the real one that refuses a chosen write, to check nothing is acked before
// its commit.
type storeAPI interface {
	Sessions(ctx context.Context) (int, error)
	OpenSessions(ctx context.Context) ([]store.OpenSession, error)
	Session(ctx context.Context, src, sessionID string) (store.SessionInfo, error)
	StartSession(ctx context.Context, ev *eventsv1.SessionStarted) error
	StageFrames(ctx context.Context, frames []store.StagedFrame) error
	HasLap(ctx context.Context, src, sessionID string, lap int32) (bool, error)
	LapsWritten(ctx context.Context, src, sessionID string) ([]int32, error)
	ContinueSession(ctx context.Context, src, sessionID string, c store.Continuation) error
	WriteStagedLap(ctx context.Context, src, run string, lap *eventsv1.LapCompleted) (int, int, error)
	EndSession(ctx context.Context, src string, ev *eventsv1.SessionEnded, endedAt time.Time) error
	DropStaged(ctx context.Context, src, sessionID string) error
	MarkPosted(ctx context.Context, src string, ev *eventsv1.CardPosted) error
	ApplyFact(ctx context.Context, src string, ev proto.Message, seq uint64) (bool, error)
}

// storeError is a write the store could not make. It stops the runner, unacked, so
// the messages behind it come again to the next start rather than being dropped,
// until one has been delivered giveUpAfter times (refused).
type storeError struct{ error }

// The ce-types the runner writes; every other one is acked and ignored.
var (
	typeStarted   = mustType(&eventsv1.SessionStarted{})
	typeContinued = mustType(&eventsv1.SessionContinued{})
	typeLap       = mustType(&eventsv1.LapCompleted{})
	typeEnded     = mustType(&eventsv1.SessionEnded{})
	typePosted    = mustType(&eventsv1.CardPosted{})
	// factTypes are the events whose facts an open session's row keeps
	// (store.ApplyFact, paddock#337); session.continued's are kept by continued.
	factTypes = map[string]func() proto.Message{
		mustType(&eventsv1.SessionKindChanged{}):  func() proto.Message { return &eventsv1.SessionKindChanged{} },
		mustType(&eventsv1.SessionTrackChanged{}): func() proto.Message { return &eventsv1.SessionTrackChanged{} },
		mustType(&eventsv1.SessionPaused{}):       func() proto.Message { return &eventsv1.SessionPaused{} },
		mustType(&eventsv1.SessionResumed{}):      func() proto.Message { return &eventsv1.SessionResumed{} },
		mustType(&eventsv1.LapBestChanged{}):      func() proto.Message { return &eventsv1.LapBestChanged{} },
		mustType(&eventsv1.PitExited{}):           func() proto.Message { return &eventsv1.PitExited{} },
	}
)

func mustType(m proto.Message) string {
	t, err := ce.Type(m)
	if err != nil {
		panic(err)
	}
	return t
}

// session is an open session as the runner tracks it: where its frames start, the
// ecu runs they come from (its legs), and the lap events and end that wait for
// their frames.
type session struct {
	src, id  string
	firstSeq uint32
	// startedAt is the session's started_at, to the microsecond the index keeps:
	// the ce-time of its first frame, which with firstSeq picks out that frame.
	startedAt time.Time
	// legs is the session's runs in order: the run of its first frame, then each
	// run that continued it after an ecu restart (session.continued, paddock#76).
	// Only frames of a leg's run, from its first seq on, join a session, so a
	// session whose frames are gone never takes another run's.
	legs  []*leg
	order int // when it was opened, against a tie on firstSeq
	// lastFrame is when a frame of the session was last staged: a lap times out
	// only once its frames have stopped coming, not while a catch-up streams them.
	lastFrame time.Time
	laps      map[int32]*held[*eventsv1.LapCompleted]
	skipped   map[int32]bool // laps not written because the session's frames are gone
	end       *held[*eventsv1.SessionEnded]
	endAt     time.Time
}

// leg is one ecu run's part of a session. A seq counts within its run, so each leg
// keeps its own frames and its own highest seq.
type leg struct {
	// run is the leg's ecu run. The first leg's is "" until its first frame is
	// seen; a continued leg's is SessionContinued's run_id.
	run string
	// from is the leg's first seq in its run: the session's first_seq, or
	// SessionContinued's at.
	from uint32
	// lap is the session's first lap the leg's run published: 1, or the lap the
	// gap fell in.
	lap int32
	// at is the ce-time of the leg's first frame, where a read-back starts.
	at       time.Time
	readBack bool  // the leg's first frames are still to be looked for in TELEMETRY
	high     int64 // the highest seq staged, -1 for none
}

// first is the leg of the session's first frame.
func (s *session) first() *leg { return s.legs[0] }

// legOf is the leg of run, or nil.
func (s *session) legOf(run string) *leg {
	for _, l := range s.legs {
		if l.run != "" && l.run == run {
			return l
		}
	}
	return nil
}

// lapLeg is the leg whose run published lap n: the last that starts at or before
// it. ecu numbers the laps on across a restart, so the numbers say which run's
// seqs a lap's bounds count in.
func (s *session) lapLeg(n int32) *leg {
	out := s.legs[0]
	for _, l := range s.legs[1:] {
		if l.lap <= n {
			out = l
		}
	}
	return out
}

// held is an event waiting for its frames, with the message to ack once it is
// written.
type held[T any] struct {
	ev       T
	msg      jetstream.Msg
	since    time.Time // when it arrived
	progress time.Time // when the server was last told it is in hand
}

type consumer struct {
	cfg    Config
	st     storeAPI
	js     jetstream.JetStream
	h      *health
	log    *slog.Logger
	now    func() time.Time
	frames jetstream.Consumer
	events jetstream.Consumer
	open   map[[2]string]*session
	opened int
	// stop is the context a stop cancels, which long reads check between
	// fetches; writes run on one it does not.
	stop context.Context
}

// readBackMargin is how far before a session's started_at the read-back of its
// frames starts: TELEMETRY's times are the server's, and ecu's clock may be a
// little apart from it.
const readBackMargin = time.Minute

// giveUpAfter is how many deliveries of a message the store may refuse before it
// is dropped with an error, rather than stopping logbook again and again.
const giveUpAfter = 5

func newConsumer(cfg Config, st storeAPI, js jetstream.JetStream, h *health, logger *slog.Logger) *consumer {
	return &consumer{cfg: cfg, st: st, js: js, h: h, log: logger, now: time.Now, open: map[[2]string]*session{},
		stop: context.Background()}
}

// restore loads the sessions the store has open, with their runs and the frames
// staged for them, so frames go where they went before a restart.
func (c *consumer) restore(ctx context.Context) error {
	open, err := c.st.OpenSessions(ctx)
	if err != nil {
		return err
	}
	for _, o := range open {
		s := c.add(o.Src, o.ID, o.FirstSeq, o.StartedAt)
		f := s.first()
		f.run, f.high = o.Run, o.MaxStaged
		f.readBack = f.run == ""
		for _, k := range o.Continued {
			// A stop between the continue's commit and its read-back would leave the
			// read-back undone: look again while the leg has nothing staged.
			s.legs = append(s.legs, &leg{run: k.Run, from: k.FromSeq, lap: k.FromLap, at: k.At,
				high: k.MaxStaged, readBack: k.MaxStaged < 0})
		}
	}
	if len(open) > 0 {
		c.log.InfoContext(ctx, "open sessions restored", "sessions", len(open))
	}
	return nil
}

func (c *consumer) add(src, id string, firstSeq uint32, startedAt time.Time) *session {
	key := [2]string{src, id}
	if s, ok := c.open[key]; ok {
		return s
	}
	c.opened++
	at := startedAt.UTC().Truncate(time.Microsecond)
	s := &session{src: src, id: id, firstSeq: firstSeq, startedAt: at,
		legs:  []*leg{{from: firstSeq, lap: 1, at: at, readBack: true, high: -1}},
		order: c.opened, lastFrame: c.now(),
		laps: map[int32]*held[*eventsv1.LapCompleted]{}, skipped: map[int32]bool{}}
	c.open[key] = s
	return s
}

// run consumes until ctx is done, binding the consumers again whenever a fetch
// fails. A store failure stops it with the error.
func (c *consumer) run(ctx context.Context) error {
	c.stop = ctx
	for {
		if ctx.Err() != nil {
			c.release(ctx)
			c.log.InfoContext(ctx, "shutting down")
			return nil
		}
		if c.frames == nil || c.events == nil {
			if err := c.bind(ctx); err != nil {
				c.log.WarnContext(ctx, "consumers not bound", "err", err)
				c.pause(ctx)
				continue
			}
			c.log.InfoContext(ctx, "consumers bound", "frames", FramesConsumer, "events", EventsConsumer)
		}
		err := c.step(ctx)
		var se storeError
		switch {
		case errors.As(err, &se):
			return se.error
		case err != nil:
			c.log.WarnContext(ctx, "fetch failed; binding again", "err", err)
			c.unbind()
			c.pause(ctx)
		}
	}
}

func (c *consumer) pause(ctx context.Context) {
	t := time.NewTimer(c.cfg.retryWait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// bind creates both consumers, or updates them to the settings in code.
func (c *consumer) bind(ctx context.Context) error {
	frames, events := consumerConfigs(c.cfg.ackWait)
	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fc, err := c.js.CreateOrUpdateConsumer(bctx, framesStream, frames)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", FramesConsumer, framesStream, err)
	}
	ec, err := c.js.CreateOrUpdateConsumer(bctx, eventsStream, events)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", EventsConsumer, eventsStream, err)
	}
	c.frames, c.events = fc, ec
	c.h.frames.Store(true)
	c.h.events.Store(true)
	return nil
}

func (c *consumer) unbind() {
	c.frames, c.events = nil, nil
	c.h.frames.Store(false)
	c.h.events.Store(false)
}

// step is one round: fetch a batch of frames, take every event waiting, look up
// the first frames of sessions that have just opened, stage the batch and ack it,
// then write what is ready. Events are taken after the frames are fetched and
// before they are placed, so a session.started published just after its first
// frame is known when that frame is placed.
//
// Once ctx is done (SIGTERM) a step starts no new write: the one in progress runs
// to its end, on a context of its own (R8), and whatever was fetched and not
// written comes again to the next start.
func (c *consumer) step(ctx context.Context) error {
	msgs, ferr := fetch(c.frames, framesBatch, c.cfg.fetchWait)
	wctx := context.WithoutCancel(ctx)
	if err := c.drainEvents(ctx, wctx); err != nil {
		return err
	}
	if err := c.readBacks(ctx, wctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := c.stage(wctx, msgs); err != nil {
		return err
	}
	if err := c.settle(ctx, wctx); err != nil {
		return err
	}
	c.keepAlive()
	return ferr
}

// fetch takes up to n messages, waiting up to wait for the first when wait is
// positive and not at all otherwise.
func fetch(cons jetstream.Consumer, n int, wait time.Duration) ([]jetstream.Msg, error) {
	var (
		batch jetstream.MessageBatch
		err   error
	)
	if wait > 0 {
		batch, err = cons.Fetch(n, jetstream.FetchMaxWait(wait))
	} else {
		batch, err = cons.FetchNoWait(n)
	}
	if err != nil {
		return nil, err
	}
	var msgs []jetstream.Msg
	for m := range batch.Messages() {
		msgs = append(msgs, m)
	}
	return msgs, batch.Error()
}

func (c *consumer) drainEvents(ctx, wctx context.Context) error {
	for ctx.Err() == nil {
		msgs, err := fetch(c.events, eventsBatch, 0)
		for i, m := range msgs {
			if ctx.Err() != nil {
				// Stopping: the rest go back to the server for the next start.
				for _, rest := range msgs[i:] {
					_ = rest.Nak()
				}
				return nil
			}
			if err := c.event(wctx, m); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
	}
	return nil
}

// event handles one message from EVENTS.
func (c *consumer) event(ctx context.Context, m jetstream.Msg) error {
	src, ok := eventSource(m.Subject())
	h := ce.Header(m.Headers())
	if !ok {
		c.log.WarnContext(ctx, "event on a subject logbook does not read; dropped", "subject", m.Subject())
		return term(m)
	}
	var err error
	switch h.Get(ce.TypeHeader) {
	case typeStarted:
		var ev eventsv1.SessionStarted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.started(ctx, src, &ev, m)
	case typeContinued:
		var ev eventsv1.SessionContinued
		e, derr := ce.Decode(h, m.Data(), &ev)
		if derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.continued(ctx, src, &ev, e.Time, m)
	case typeLap:
		var ev eventsv1.LapCompleted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.lap(ctx, src, &ev, m)
	case typeEnded:
		var ev eventsv1.SessionEnded
		e, derr := ce.Decode(h, m.Data(), &ev)
		if derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.ended(ctx, src, &ev, e.Time, m)
	case typePosted:
		var ev eventsv1.CardPosted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.posted(ctx, src, &ev, m)
	default:
		mk, ok := factTypes[h.Get(ce.TypeHeader)]
		if !ok {
			return ack(m)
		}
		ev := mk()
		if _, derr := ce.Decode(h, m.Data(), ev); derr != nil {
			return c.drop(ctx, m, derr)
		}
		err = c.fact(ctx, src, ev, m)
	}
	return c.refused(ctx, m, err)
}

// refused passes on a store error for m, unless m has been delivered giveUpAfter
// times already: then it is dropped with an error, so a message the store will
// never take does not stop logbook at every start.
func (c *consumer) refused(ctx context.Context, m jetstream.Msg, err error) error {
	var se storeError
	if !errors.As(err, &se) {
		return err
	}
	md, merr := m.Metadata()
	if merr != nil || md.NumDelivered < giveUpAfter {
		return err
	}
	c.log.ErrorContext(ctx, "the store refused a message on every delivery; dropped",
		"subject", m.Subject(), "deliveries", md.NumDelivered, "err", se.error)
	return term(m)
}

// drop terminates a message that cannot be read, so it is never delivered again.
func (c *consumer) drop(ctx context.Context, m jetstream.Msg, err error) error {
	c.log.WarnContext(ctx, "event dropped", "subject", m.Subject(), "err", err)
	return term(m)
}

func (c *consumer) started(ctx context.Context, src string, ev *eventsv1.SessionStarted, m jetstream.Msg) error {
	if ev.GetSource() != "" && ev.GetSource() != src {
		c.log.WarnContext(ctx, "session.started names another source than its subject; the subject's is kept",
			"subject_src", src, "event_src", ev.GetSource(), "session", ev.GetSessionId())
	}
	ev.Source = src
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil || ev.GetStartedAt() == nil {
		return c.drop(ctx, m, errors.Join(err, errIf(ev.GetStartedAt() == nil, "no started_at")))
	}
	info, err := c.st.Session(ctx, src, ev.GetSessionId())
	if err != nil {
		return storeError{err}
	}
	if info.State == store.SessionEnded {
		// A redelivery of a session that has since ended: it changes nothing.
		return ack(m)
	}
	if err := c.st.StartSession(ctx, ev); err != nil {
		return storeError{err}
	}
	c.add(src, ev.GetSessionId(), ev.GetFirstSeq(), ev.GetStartedAt().AsTime())
	c.log.InfoContext(ctx, "session started", "src", src, "session", ev.GetSessionId(), "first_seq", ev.GetFirstSeq())
	return ack(m)
}

// readBacks looks in TELEMETRY for the first frames of every session that has
// opened without them. The two streams are read apart, so a session's first
// frames can have passed through logbook, acked and not staged, before its
// session.started came: they are still in the stream. A stop leaves the rest for
// the next start.
func (c *consumer) readBacks(ctx, wctx context.Context) error {
	var last uint64
	asked := false
	for _, s := range c.sorted() {
		for i, l := range s.legs {
			if !l.readBack || (i == 0 && l.run != "") {
				l.readBack = false
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			if !asked {
				// Once a round: how far logbook's own consumer has been given frames.
				info, err := c.frames.Info(wctx)
				if err != nil {
					return err
				}
				last, asked = info.Delivered.Stream, true
			}
			if last > 0 {
				read := c.readBack
				if i > 0 {
					read = func(ctx context.Context, s *session, last uint64) error { return c.readBackLeg(ctx, s, l, last) }
				}
				err := read(wctx, s, last)
				if errors.Is(err, errStopped) {
					return nil
				}
				if err != nil {
					return err
				}
			}
			l.readBack = false
		}
	}
	return nil
}

// errStopped is a read of TELEMETRY cut short by a stop.
var errStopped = errors.New("stopped")

// readSource reads src's frames in TELEMETRY with an ephemeral ordered consumer,
// from the stream time from up to stream sequence last, and passes each to visit.
// It checks for a stop between fetches (errStopped), and deletes its consumer when
// it is done.
func (c *consumer) readSource(ctx context.Context, src string, from time.Time, last uint64,
	visit func(run string, at time.Time, f *telemetryv1.Frame),
) (err error) {
	oc, err := c.js.OrderedConsumer(ctx, framesStream, jetstream.OrderedConsumerConfig{
		FilterSubjects:    []string{"twr.v1." + src + ".frame.gt7"},
		DeliverPolicy:     jetstream.DeliverByStartTimePolicy,
		OptStartTime:      &from,
		InactiveThreshold: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer func() {
		if info := oc.CachedInfo(); info != nil {
			// The inactive threshold removes it anyway; this is sooner.
			_ = c.js.DeleteConsumer(ctx, framesStream, info.Name)
		}
	}()
	for {
		if c.cfg.beforeReadFetch != nil {
			c.cfg.beforeReadFetch()
		}
		if c.stop.Err() != nil {
			return errStopped
		}
		msgs, err := fetch(oc, framesBatch, 250*time.Millisecond)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, m := range msgs {
			md, err := m.Metadata()
			if err != nil {
				return err
			}
			if md.Sequence.Stream > last {
				return nil
			}
			var f telemetryv1.Frame
			e, err := ce.Decode(ce.Header(m.Headers()), m.Data(), &f)
			if err != nil {
				continue
			}
			visit(runOf(e.ID), e.Time, &f)
		}
	}
}

// readBack reads the session's source back from a little before its started_at
// up to the last frame logbook's own consumer has been given; later frames come
// to that consumer and are placed as they come. If the session's first frame is
// there, the session takes that frame's run, and the run's frames from it on are
// staged; failing it, the run of the earliest frame that fits its start
// (fitsStart). A run bound by the first frame is never replaced by one that only
// fits. Where two runs carry the first frame itself, as two replays of one
// recording under one source do, the later is taken.
func (c *consumer) readBack(ctx context.Context, s *session, last uint64) error {
	var (
		run   string
		exact bool // run came from the session's first frame itself
		found []*telemetryv1.Frame
	)
	err := c.readSource(ctx, s.src, s.startedAt.Add(-readBackMargin), last,
		func(r string, at time.Time, f *telemetryv1.Frame) {
			switch {
			case run != "" && r == run:
				found = append(found, f)
			case s.isFirst(f.GetSeq(), at):
				// The first frame itself. Another run's can match only as a copy:
				// a replay of the same recording under the same source, which is
				// published after it, so the later is the session's own.
				run, exact, found = r, true, []*telemetryv1.Frame{f}
			case run == "" && !exact && s.fitsStart(f.GetSeq(), at):
				// The earliest frame that fits; a later run that fits too never
				// replaces it.
				run, found = r, []*telemetryv1.Frame{f}
			}
		})
	if err != nil || run == "" {
		return err
	}
	s.first().run = run
	return c.stageRead(ctx, s, s.first(), found)
}

// readBackLeg reads back the first frames of a run that continued a session: its
// source from a little before the leg's first frame up to the last frame
// logbook's own consumer has been given, the run's frames from the leg's first
// seq on. session.continued comes on the run's first frame of the session, so
// those frames can have passed through, acked and not staged, before it.
func (c *consumer) readBackLeg(ctx context.Context, s *session, l *leg, last uint64) error {
	var found []*telemetryv1.Frame
	err := c.readSource(ctx, s.src, l.at.Add(-readBackMargin), last,
		func(r string, _ time.Time, f *telemetryv1.Frame) {
			if r == l.run {
				found = append(found, f)
			}
		})
	if err != nil {
		return err
	}
	return c.stageRead(ctx, s, l, found)
}

// stageRead stages the frames read back for a leg: those from its first seq up to
// the next session of its run.
func (c *consumer) stageRead(ctx context.Context, s *session, l *leg, found []*telemetryv1.Frame) error {
	until := c.nextFrom(s, l)
	var staged []store.StagedFrame
	for _, f := range found {
		if f.GetSeq() >= l.from && (until < 0 || int64(f.GetSeq()) < until) {
			staged = append(staged, store.StagedFrame{Src: s.src, SessionID: s.id, Run: l.run, Frame: f})
			l.high = max(l.high, int64(f.GetSeq()))
		}
	}
	if err := c.st.StageFrames(ctx, staged); err != nil {
		return storeError{err}
	}
	s.lastFrame = c.now()
	c.log.InfoContext(ctx, "first frames read back", "src", s.src, "session", s.id, "run", l.run, "frames", len(staged))
	return nil
}

// The fallback for a session whose first frame is missing (fitsStart): a frame
// within its first fitFrames frames (30 s at 60 Hz), whose ce-time is within
// fitSlack of where the session's start puts it.
const (
	fitFrames = 1800
	fitSlack  = 5 * time.Second
)

// isFirst is whether a frame of the session's source, with seq and ce-time at, is
// the session's first frame: its seq is first_seq and its ce-time started_at.
func (s *session) isFirst(seq uint32, at time.Time) bool {
	return seq == s.firstSeq && at.UTC().Truncate(time.Microsecond).Equal(s.startedAt)
}

// fitsStart is whether a frame can stand for a session's missing first frame. ecu
// drops frames while its connection is down, so the first frame itself may be
// missing. A frame fits when it is one of the session's first fitFrames frames by
// seq and its ce-time is no earlier than started_at and within fitSlack of where a
// 60 Hz run from the first frame puts its seq. The bound keeps an earlier session
// of a run that is still going from fitting that run's current frames.
func (s *session) fitsStart(seq uint32, at time.Time) bool {
	at = at.UTC().Truncate(time.Microsecond)
	if seq < s.firstSeq || seq-s.firstSeq > fitFrames || at.Before(s.startedAt) {
		return false
	}
	want := s.startedAt.Add(time.Duration(seq-s.firstSeq) * time.Second / 60)
	off := at.Sub(want)
	if off < 0 {
		off = -off
	}
	return off <= fitSlack
}

// nextFrom is the first seq of the next session of s's source in l's run, where
// the leg's frames end, or -1 for none.
func (c *consumer) nextFrom(s *session, l *leg) int64 {
	until := int64(-1)
	for _, o := range c.open {
		if o == s || o.src != s.src {
			continue
		}
		for _, ol := range o.legs {
			if ol.run == l.run && ol.from > l.from && (until < 0 || int64(ol.from) < until) {
				until = int64(ol.from)
			}
		}
	}
	return until
}

func (c *consumer) lap(ctx context.Context, src string, ev *eventsv1.LapCompleted, m jetstream.Msg) error {
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil || ev.GetLap() < 1 {
		return c.drop(ctx, m, errors.Join(err, errIf(ev.GetLap() < 1, "laps count from 1")))
	}
	done, err := c.st.HasLap(ctx, src, ev.GetSessionId(), ev.GetLap())
	if err != nil {
		return storeError{err}
	}
	if done {
		// Written already: a redelivery, perhaps after a stop between the commit
		// and the ack. It changes nothing.
		return ack(m)
	}
	s, ok := c.open[[2]string{src, ev.GetSessionId()}]
	if !ok {
		return c.lateLap(ctx, src, ev, m)
	}
	now := c.now()
	if p, ok := s.laps[ev.GetLap()]; ok {
		// Delivered again while it waits: keep its place, take the newer message.
		p.ev, p.msg, p.progress = ev, m, now
		return nil
	}
	s.laps[ev.GetLap()] = &held[*eventsv1.LapCompleted]{ev: ev, msg: m, since: now, progress: now}
	return nil
}

// lateLap is a lap.completed of a session that is not open. Of a session that has
// ended, it writes the lap's row, leaving the session's summary as its end made
// it: its frames were dropped with the session's at the end, so they are read
// back from TELEMETRY, with those the stream no longer holds missing. If no frame
// of the session ever came, there is nothing to write. Of a session never
// started, it is dropped.
func (c *consumer) lateLap(ctx context.Context, src string, ev *eventsv1.LapCompleted, m jetstream.Msg) error {
	info, err := c.st.Session(ctx, src, ev.GetSessionId())
	if err != nil {
		return storeError{err}
	}
	if info.State == store.SessionUnknown {
		c.log.WarnContext(ctx, "lap of a session never started; dropped",
			"src", src, "session", ev.GetSessionId(), "lap", ev.GetLap())
		return term(m)
	}
	// The run that published the lap: its bounds count in that run's seqs.
	lapRun := store.RunOf(info.Run, info.Continued, ev.GetLap())
	if lapRun == "" {
		c.log.WarnContext(ctx, "lap not written: its session's frames are gone",
			"src", src, "session", ev.GetSessionId(), "lap", ev.GetLap())
		return ack(m)
	}
	fi, err := c.frames.Info(ctx)
	if err != nil {
		return err
	}
	var frames []store.StagedFrame
	err = c.readSource(ctx, src, info.StartedAt.Add(-readBackMargin), fi.Delivered.Stream,
		func(run string, _ time.Time, f *telemetryv1.Frame) {
			if run == lapRun && f.GetSeq()+1 >= ev.GetFirstSeq() && f.GetSeq() <= ev.GetLastSeq() {
				frames = append(frames, store.StagedFrame{Src: src, SessionID: ev.GetSessionId(), Run: run, Frame: f})
			}
		})
	if errors.Is(err, errStopped) {
		_ = m.Nak() // for the next start
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.st.StageFrames(ctx, frames); err != nil {
		return storeError{err}
	}
	staged, written, err := c.st.WriteStagedLap(ctx, src, lapRun, ev)
	if err != nil {
		return storeError{err}
	}
	if err := c.st.DropStaged(ctx, src, ev.GetSessionId()); err != nil {
		return storeError{err}
	}
	c.wrote(ctx, src, ev, staged, written, false)
	return ack(m)
}

// continued binds an open session to the run that continued it after an ecu
// restart (paddock#76): that run's frames from the event's at on join the
// session, apart from the earlier run's, and its laps, from the lap the gap fell
// in, are written from them. ecu numbers the laps on across the gap; logbook
// takes them as they come. A run continues a session once, so a redelivery
// changes nothing.
func (c *consumer) continued(ctx context.Context, src string, ev *eventsv1.SessionContinued, at time.Time, m jetstream.Msg) error {
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil || ev.GetRunId() == "" {
		return c.drop(ctx, m, errors.Join(err, errIf(ev.GetRunId() == "", "no run_id")))
	}
	s, ok := c.open[[2]string{src, ev.GetSessionId()}]
	if !ok {
		info, err := c.st.Session(ctx, src, ev.GetSessionId())
		if err != nil {
			return storeError{err}
		}
		if info.State == store.SessionUnknown {
			c.log.WarnContext(ctx, "continue of a session never started; dropped", "src", src, "session", ev.GetSessionId())
			return term(m)
		}
		// Ended already: a redelivery after the end changes nothing.
		return ack(m)
	}
	if s.legOf(ev.GetRunId()) != nil {
		return ack(m) // delivered again
	}
	k := store.Continuation{Run: ev.GetRunId(), FromSeq: ev.GetAt(), FromLap: max(ev.GetGap().GetLap(), 1),
		At: at.UTC().Truncate(time.Microsecond)}
	// Its gap and carried track before the run: a stop between the two leaves
	// the continue unrecorded, so it comes again and both are applied again,
	// which changes nothing the second time.
	if _, err := c.st.ApplyFact(ctx, src, ev, streamSeq(m)); err != nil {
		return storeError{err}
	}
	if err := c.st.ContinueSession(ctx, src, ev.GetSessionId(), k); err != nil {
		return storeError{err}
	}
	s.legs = append(s.legs, &leg{run: k.Run, from: k.FromSeq, lap: k.FromLap, at: k.At, readBack: true, high: -1})
	s.lastFrame = c.now()
	c.log.InfoContext(ctx, "session continued", "src", src, "session", s.id, "run", k.Run, "at", k.FromSeq,
		"lap", k.FromLap)
	return ack(m)
}

func (c *consumer) ended(ctx context.Context, src string, ev *eventsv1.SessionEnded, at time.Time, m jetstream.Msg) error {
	// ecu always sends a summary; an end without one has nothing to apply.
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil || ev.GetSummary() == nil {
		return c.drop(ctx, m, errors.Join(err, errIf(ev.GetSummary() == nil, "no summary")))
	}
	s, ok := c.open[[2]string{src, ev.GetSessionId()}]
	if !ok {
		// Ended already (a redelivery, which EndSession leaves as it was) or never
		// started here.
		err := c.st.EndSession(ctx, src, ev, at)
		if errors.Is(err, store.ErrUnknownSession) {
			c.log.WarnContext(ctx, "end of a session never started; dropped", "src", src, "session", ev.GetSessionId())
			return term(m)
		}
		if err != nil {
			return storeError{err}
		}
		return ack(m)
	}
	now := c.now()
	if s.end == nil {
		s.end = &held[*eventsv1.SessionEnded]{since: now}
	}
	s.end.ev, s.end.msg, s.end.progress, s.endAt = ev, m, now, at
	return nil
}

// posted records when podium posted the session's results card (card.posted,
// paddock#67). It does not wait on the session's end: posted_at is a column of
// its own. A redelivery changes nothing; a card of a session never started here
// is dropped.
func (c *consumer) posted(ctx context.Context, src string, ev *eventsv1.CardPosted, m jetstream.Msg) error {
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil || ev.GetPostedAt() == nil {
		return c.drop(ctx, m, errors.Join(err, errIf(ev.GetPostedAt() == nil, "no posted_at")))
	}
	err := c.st.MarkPosted(ctx, src, ev)
	if errors.Is(err, store.ErrUnknownSession) {
		c.log.WarnContext(ctx, "card of a session never started; dropped", "src", src, "session", ev.GetSessionId())
		return term(m)
	}
	if err != nil {
		return storeError{err}
	}
	c.log.InfoContext(ctx, "card posted", "src", src, "session", ev.GetSessionId())
	return ack(m)
}

// fact keeps what a session event says in its session's row while the session is
// open (store.ApplyFact): it waits on no frames. Once the session has ended it
// changes nothing; an event of a session never started is dropped.
func (c *consumer) fact(ctx context.Context, src string, ev proto.Message, m jetstream.Msg) error {
	id := ev.(interface{ GetSessionId() string }).GetSessionId() //nolint:forcetypeassert // every factTypes event has one
	if err := store.CheckKey(src, id); err != nil {
		return c.drop(ctx, m, err)
	}
	_, err := c.st.ApplyFact(ctx, src, ev, streamSeq(m))
	if errors.Is(err, store.ErrUnknownSession) {
		c.log.WarnContext(ctx, "event of a session never started; dropped", "subject", m.Subject(), "session", id)
		return term(m)
	}
	if err != nil {
		return storeError{err}
	}
	c.log.DebugContext(ctx, "session fact kept", "subject", m.Subject(), "session", id)
	return ack(m)
}

// stage places each frame in its open session, stages those that have one, and
// acks the batch once they are committed. Frames outside every open session are
// acked with it and not staged.
func (c *consumer) stage(ctx context.Context, msgs []jetstream.Msg) error {
	if len(msgs) == 0 {
		return nil
	}
	now := c.now()
	var staged []store.StagedFrame
	for _, m := range msgs {
		src, ok := frameSource(m.Subject())
		if !ok {
			continue
		}
		var f telemetryv1.Frame
		e, err := ce.Decode(ce.Header(m.Headers()), m.Data(), &f)
		if err != nil {
			c.log.WarnContext(ctx, "frame dropped", "subject", m.Subject(), "err", err)
			continue
		}
		s, l := c.place(src, runOf(e.ID), f.GetSeq(), e.Time)
		if s == nil {
			continue
		}
		l.high = max(l.high, int64(f.GetSeq()))
		s.lastFrame = now
		staged = append(staged, store.StagedFrame{Src: src, SessionID: s.id, Run: l.run, Frame: &f})
	}
	last := msgs[len(msgs)-1]
	if err := c.st.StageFrames(ctx, staged); err != nil {
		md, merr := last.Metadata()
		if merr != nil || md.NumDelivered < giveUpAfter {
			return storeError{err}
		}
		c.log.ErrorContext(ctx, "the store refused a batch of frames on every delivery; dropped",
			"frames", len(msgs), "deliveries", md.NumDelivered, "err", err)
	}
	// Ack all: the last message acks the batch, and every one before it.
	if err := last.Ack(); err != nil {
		c.log.WarnContext(ctx, "frames not acked; they will come again", "err", err)
	}
	return nil
}

// place is the open session of src a frame of run, with seq and ce-time at, belongs
// to, and the session's leg it joins. Of the source's sessions whose leg in the
// frame's run starts at or before seq, or, with no leg in that run, whose frames
// start at or before seq, latest first: one with a leg in the frame's run takes
// it; one whose first leg has no run yet takes it, and the run, if the frame is
// its first frame or fits its start (fitsStart); one bound to other runs is passed
// over. So a session whose first frame is still to come takes it from an earlier
// session of the same run, a session that only fits never takes a frame its run's
// own session would, and a continued session takes its new run's frames from the
// continue on (paddock#76).
func (c *consumer) place(src, run string, seq uint32, at time.Time) (*session, *leg) {
	type candidate struct {
		s    *session
		from uint32
		l    *leg // the session's leg in the frame's run, or nil
	}
	var candidates []candidate
	for _, s := range c.open {
		if s.src != src {
			continue
		}
		if l := s.legOf(run); l != nil {
			if l.from <= seq {
				candidates = append(candidates, candidate{s, l.from, l})
			}
			continue
		}
		if s.firstSeq <= seq {
			candidates = append(candidates, candidate{s, s.firstSeq, nil})
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(b.from, a.from), cmp.Compare(b.s.order, a.s.order))
	})
	for _, k := range candidates {
		f := k.s.first()
		switch {
		case k.l != nil:
			return k.s, k.l
		case f.run == "" && (k.s.isFirst(seq, at) || k.s.fitsStart(seq, at)):
			f.run, f.readBack = run, false
			return k.s, f
		}
	}
	return nil, nil
}

// settle writes every lap that is ready, in order, and then the end of every
// session whose laps are all in (a session's laps before its end, so the end's
// fallbacks see the last lap). A lap is ready once a frame at or beyond its last
// seq is staged, or once its session's frames have stopped coming for the frames
// timeout. Once ctx is done it starts no new write.
func (c *consumer) settle(ctx, wctx context.Context) error {
	now := c.now()
	for _, s := range c.sorted() {
		for _, n := range slices.Sorted(maps.Keys(s.laps)) {
			p := s.laps[n]
			ready := s.lapLeg(n).high >= int64(p.ev.GetLastSeq())
			timedOut := !now.Before(later(p.since, s.lastFrame).Add(c.cfg.FramesTimeout))
			if !ready && !timedOut {
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			if err := c.writeLap(wctx, s, p, !ready); err != nil {
				if rerr := c.refused(wctx, p.msg, err); rerr != nil {
					return rerr
				}
				s.skipped[n] = true // dropped: the end does not wait for it
			}
			delete(s.laps, n)
		}
		if s.end == nil || len(s.laps) > 0 || ctx.Err() != nil {
			continue
		}
		waiting, err := c.lapsMissing(wctx, s, now)
		if err != nil {
			return err
		}
		if waiting {
			continue
		}
		if err := c.endSession(wctx, s); err != nil {
			if rerr := c.refused(wctx, s.end.msg, err); rerr != nil {
				return rerr
			}
			delete(c.open, [2]string{s.src, s.id})
		}
	}
	return nil
}

// lapsMissing is whether a session's end should wait on: its summary has
// complete laps (expectedLaps) that have no row yet. A lap.completed can come after its session's end, as when
// both come again after a stop, each at the end of its own ack wait; the end waits
// for it for an ack wait and a frames timeout from its own arrival, then applies
// without it. Frames that go on coming do not extend the wait: a missing event
// does not depend on them.
func (c *consumer) lapsMissing(ctx context.Context, s *session, now time.Time) (bool, error) {
	if !now.Before(s.end.since.Add(c.cfg.ackWait + c.cfg.FramesTimeout)) {
		return false, nil
	}
	written, err := c.st.LapsWritten(ctx, s.src, s.id)
	if err != nil {
		return false, storeError{err}
	}
	for _, n := range expectedLaps(s.end.ev.GetSummary()) {
		if !slices.Contains(written, n) && !s.skipped[n] {
			return true, nil
		}
	}
	return false, nil
}

// expectedLaps is the laps a session's end waits for: the complete laps its
// summary lists (contracts v0.2's laps), or, from an ecu that lists none, laps 1
// to laps_completed. A lap the line was crossed in during an ecu restart's gap is
// in the summary only, not complete and with no lap.completed (paddock#76), so
// counting by number would wait for a lap that never comes.
func expectedLaps(sum *eventsv1.SessionSummary) []int32 {
	var out []int32
	for _, l := range sum.GetLaps() {
		if l.GetComplete() {
			out = append(out, l.GetLap())
		}
	}
	if len(sum.GetLaps()) > 0 {
		return out
	}
	for n := int32(1); n <= sum.GetLapsCompleted(); n++ {
		out = append(out, n)
	}
	return out
}

func (c *consumer) endSession(ctx context.Context, s *session) error {
	if err := c.st.EndSession(ctx, s.src, s.end.ev, s.endAt); err != nil {
		return storeError{err}
	}
	if err := c.st.DropStaged(ctx, s.src, s.id); err != nil {
		return storeError{err}
	}
	delete(c.open, [2]string{s.src, s.id})
	c.log.InfoContext(ctx, "session ended", "src", s.src, "session", s.id,
		"reason", s.end.ev.GetReason().String())
	return ack(s.end.msg)
}

func (c *consumer) writeLap(ctx context.Context, s *session, p *held[*eventsv1.LapCompleted], timedOut bool) error {
	lap := p.ev
	run := s.lapLeg(lap.GetLap()).run
	if run == "" {
		// No frame of the session ever came: its frames have aged out of
		// TELEMETRY, as on a first start against a long EVENTS history. A lap row
		// with no frames would only mislead.
		c.log.WarnContext(ctx, "lap not written: its session's frames are gone",
			"src", s.src, "session", s.id, "lap", lap.GetLap())
		s.skipped[lap.GetLap()] = true
		return ack(p.msg)
	}
	if c.cfg.beforeLapWrite != nil {
		if err := c.cfg.beforeLapWrite(s.src, s.id, lap.GetLap()); err != nil {
			return storeError{err}
		}
	}
	staged, written, err := c.st.WriteStagedLap(ctx, s.src, run, lap)
	if err != nil {
		return storeError{err}
	}
	if c.cfg.afterLapWrite != nil {
		if err := c.cfg.afterLapWrite(s.src, s.id, lap.GetLap()); err != nil {
			return storeError{err}
		}
	}
	c.wrote(ctx, s.src, lap, staged, written, timedOut)
	return ack(p.msg)
}

// wrote logs a lap's write, and warns when frames of its range never came.
func (c *consumer) wrote(ctx context.Context, src string, lap *eventsv1.LapCompleted, staged, written int, timedOut bool) {
	want := int(lap.GetLastSeq()) - int(lap.GetFirstSeq()) + 1
	if missing := want - staged; missing > 0 {
		c.log.WarnContext(ctx, "lap written without all its frames",
			"src", src, "session", lap.GetSessionId(), "lap", lap.GetLap(), "missing", missing, "frames", staged,
			"timed_out", timedOut)
	}
	c.log.InfoContext(ctx, "lap written", "src", src, "session", lap.GetSessionId(), "lap", lap.GetLap(), "frames", written)
	c.h.wrote(c.now())
}

// sorted is the open sessions in the order they were opened.
func (c *consumer) sorted() []*session {
	out := slices.Collect(maps.Values(c.open))
	slices.SortFunc(out, func(a, b *session) int { return cmp.Compare(a.order, b.order) })
	return out
}

// keepAlive tells the server that held events are in hand, so it does not deliver
// them again while they wait for their frames.
func (c *consumer) keepAlive() {
	now := c.now()
	touch := func(m jetstream.Msg, at *time.Time) {
		if now.Sub(*at) >= c.cfg.ackWait/3 {
			_ = m.InProgress() // a failure is a redelivery, which changes nothing
			*at = now
		}
	}
	for _, s := range c.open {
		for _, p := range s.laps {
			touch(p.msg, &p.progress)
		}
		if s.end != nil {
			touch(s.end.msg, &s.end.progress)
		}
	}
}

// release hands the held events back on shutdown, so the next start has them at
// once rather than after the ack wait.
func (c *consumer) release(ctx context.Context) {
	for _, s := range c.open {
		for _, p := range s.laps {
			_ = p.msg.Nak()
		}
		if s.end != nil {
			_ = s.end.msg.Nak()
		}
		s.laps, s.end = map[int32]*held[*eventsv1.LapCompleted]{}, nil
	}
	c.log.DebugContext(ctx, "held events released")
}

func ack(m jetstream.Msg) error {
	if err := m.Ack(); err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	return nil
}

func term(m jetstream.Msg) error {
	if err := m.Term(); err != nil {
		return fmt.Errorf("term: %w", err)
	}
	return nil
}

func errIf(cond bool, msg string) error {
	if cond {
		return errors.New(msg)
	}
	return nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// frameSource is the source a frame subject names: twr.v1.<src>.frame.gt7.
func frameSource(subject string) (string, bool) {
	t := strings.Split(subject, ".")
	if len(t) != 5 || t[0] != "twr" || t[1] != "v1" || t[3] != "frame" || t[4] != "gt7" {
		return "", false
	}
	return t[2], true
}

// eventSource is the source an event subject names:
// twr.v1.<src>.evt.<sid>.<area>.<what>, or evt._ with no session open.
func eventSource(subject string) (string, bool) {
	t := strings.Split(subject, ".")
	if len(t) < 5 || t[0] != "twr" || t[1] != "v1" || t[3] != "evt" {
		return "", false
	}
	return t[2], true
}

// runOf is the ecu run in a frame's ce-id, `<run>:frame:<seq>`.
func runOf(id string) string {
	run, _, _ := strings.Cut(id, ":frame:")
	return run
}

// streamSeq is m's sequence in its stream, which orders the events of a session:
// 0 when the message has no metadata, which only a message not from JetStream
// lacks.
func streamSeq(m jetstream.Msg) uint64 {
	md, err := m.Metadata()
	if err != nil {
		return 0
	}
	return md.Sequence.Stream
}
