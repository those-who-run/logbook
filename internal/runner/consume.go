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

// storeError is a write the store could not make. It stops the runner, unacked, so
// the messages behind it come again to the next start rather than being dropped.
type storeError struct{ error }

// The ce-types the runner writes; every other one is acked and ignored.
var (
	typeStarted = mustType(&eventsv1.SessionStarted{})
	typeLap     = mustType(&eventsv1.LapCompleted{})
	typeEnded   = mustType(&eventsv1.SessionEnded{})
)

func mustType(m proto.Message) string {
	t, err := ce.Type(m)
	if err != nil {
		panic(err)
	}
	return t
}

// session is an open session as the runner tracks it: where its frames start, the
// ecu run they come from, the highest seq staged, and the lap events and end that
// wait for their frames.
type session struct {
	src, id  string
	firstSeq uint32
	run      string // "" until a frame is staged
	order    int    // when it was opened, against a tie on firstSeq
	high     int64  // the highest seq staged, -1 for none
	// lastFrame is when a frame of the session was last staged: a lap times out
	// only once its frames have stopped coming, not while a catch-up streams them.
	lastFrame time.Time
	laps      map[int32]*held[*eventsv1.LapCompleted]
	end       *held[*eventsv1.SessionEnded]
	endAt     time.Time
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
	st     *store.Store
	js     jetstream.JetStream
	h      *health
	log    *slog.Logger
	now    func() time.Time
	frames jetstream.Consumer
	events jetstream.Consumer
	open   map[[2]string]*session
	opened int
	// unplaced holds, per source, the latest frames that had no open session when
	// they came, acked and not staged. A session.started published just after its
	// first frames can reach logbook after them (the two streams are read apart),
	// so the session claims them from here when it opens.
	unplaced map[string][]unplacedFrame
}

// unplacedFrame is a frame that came while no open session would take it.
type unplacedFrame struct {
	run   string
	frame *telemetryv1.Frame
}

// keepUnplaced is how many unplaced frames are kept per source: 10 s of live
// frames, far more than the moment between a frame and its session.started.
const keepUnplaced = 600

func newConsumer(cfg Config, st *store.Store, js jetstream.JetStream, h *health, logger *slog.Logger) *consumer {
	return &consumer{cfg: cfg, st: st, js: js, h: h, log: logger, now: time.Now, open: map[[2]string]*session{},
		unplaced: map[string][]unplacedFrame{}}
}

// restore loads the sessions the store has open, with their runs and the frames
// staged for them, so frames go where they went before a restart.
func (c *consumer) restore(ctx context.Context) error {
	open, err := c.st.OpenSessions(ctx)
	if err != nil {
		return err
	}
	for _, o := range open {
		s := c.add(o.Src, o.ID, o.FirstSeq)
		s.run, s.high = o.Run, o.MaxStaged
	}
	if len(open) > 0 {
		c.log.InfoContext(ctx, "open sessions restored", "sessions", len(open))
	}
	return nil
}

func (c *consumer) add(src, id string, firstSeq uint32) *session {
	key := [2]string{src, id}
	if s, ok := c.open[key]; ok {
		return s
	}
	c.opened++
	s := &session{src: src, id: id, firstSeq: firstSeq, order: c.opened, high: -1, lastFrame: c.now(),
		laps: map[int32]*held[*eventsv1.LapCompleted]{}}
	c.open[key] = s
	return s
}

// run consumes until ctx is done, binding the consumers again whenever a fetch
// fails. A store failure stops it with the error.
func (c *consumer) run(ctx context.Context) error {
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

// step is one round: fetch a batch of frames, take every event waiting, stage the
// frames and ack them, then write what is ready. Events are taken after the frames
// are fetched and before they are placed, so a session.started published just
// after its first frame is known when that frame is placed. Writes run to the end
// even once ctx is done (R8): the next round stops.
func (c *consumer) step(ctx context.Context) error {
	msgs, ferr := fetch(c.frames, framesBatch, c.cfg.fetchWait)
	wctx := context.WithoutCancel(ctx)
	if err := c.drainEvents(wctx); err != nil {
		return err
	}
	if err := c.stage(wctx, msgs); err != nil {
		return err
	}
	if err := c.settle(wctx); err != nil {
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

func (c *consumer) drainEvents(ctx context.Context) error {
	for {
		msgs, err := fetch(c.events, eventsBatch, 0)
		for _, m := range msgs {
			if err := c.event(ctx, m); err != nil {
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
}

// event handles one message from EVENTS.
func (c *consumer) event(ctx context.Context, m jetstream.Msg) error {
	src, ok := eventSource(m.Subject())
	h := ce.Header(m.Headers())
	if !ok {
		c.log.WarnContext(ctx, "event on a subject logbook does not read; dropped", "subject", m.Subject())
		return term(m)
	}
	switch h.Get(ce.TypeHeader) {
	case typeStarted:
		var ev eventsv1.SessionStarted
		if _, err := ce.Decode(h, m.Data(), &ev); err != nil {
			return c.drop(ctx, m, err)
		}
		return c.started(ctx, src, &ev, m)
	case typeLap:
		var ev eventsv1.LapCompleted
		if _, err := ce.Decode(h, m.Data(), &ev); err != nil {
			return c.drop(ctx, m, err)
		}
		return c.lap(ctx, src, &ev, m)
	case typeEnded:
		var ev eventsv1.SessionEnded
		e, err := ce.Decode(h, m.Data(), &ev)
		if err != nil {
			return c.drop(ctx, m, err)
		}
		return c.ended(ctx, src, &ev, e.Time, m)
	}
	return ack(m)
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
	state, err := c.st.Session(ctx, src, ev.GetSessionId())
	if err != nil {
		return storeError{err}
	}
	if state == store.SessionEnded {
		// A redelivery of a session that has since ended: it changes nothing.
		return ack(m)
	}
	if err := c.st.StartSession(ctx, ev); err != nil {
		return storeError{err}
	}
	s := c.add(src, ev.GetSessionId(), ev.GetFirstSeq())
	if err := c.claim(ctx, s); err != nil {
		return err
	}
	c.log.InfoContext(ctx, "session started", "src", src, "session", ev.GetSessionId(), "first_seq", ev.GetFirstSeq())
	return ack(m)
}

// keep holds a frame no open session takes, dropping the oldest beyond
// keepUnplaced.
func (c *consumer) keep(src, run string, f *telemetryv1.Frame) {
	kept := append(c.unplaced[src], unplacedFrame{run: run, frame: f})
	if len(kept) > keepUnplaced {
		kept = slices.Clone(kept[len(kept)-keepUnplaced:])
	}
	c.unplaced[src] = kept
}

// claim stages for a session that has just opened the unplaced frames of its
// source that are its own: of the latest run, from its first seq on, and before
// the first seq of any later session of the source.
func (c *consumer) claim(ctx context.Context, s *session) error {
	kept := c.unplaced[s.src]
	if len(kept) == 0 {
		return nil
	}
	run := kept[len(kept)-1].run
	if s.run != "" && s.run != run {
		return nil
	}
	until := int64(-1)
	for _, o := range c.open {
		if o.src == s.src && o.firstSeq > s.firstSeq && (until < 0 || int64(o.firstSeq) < until) {
			until = int64(o.firstSeq)
		}
	}
	var staged []store.StagedFrame
	var rest []unplacedFrame
	for _, u := range kept {
		seq := int64(u.frame.GetSeq())
		if u.run == run && seq >= int64(s.firstSeq) && (until < 0 || seq < until) {
			staged = append(staged, store.StagedFrame{Src: s.src, SessionID: s.id, Run: run, Frame: u.frame})
			s.high = max(s.high, seq)
			continue
		}
		rest = append(rest, u)
	}
	if len(staged) == 0 {
		return nil
	}
	if err := c.st.StageFrames(ctx, staged); err != nil {
		return storeError{err}
	}
	s.run, s.lastFrame = run, c.now()
	c.unplaced[s.src] = rest
	return nil
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
		c.log.WarnContext(ctx, "lap of a session that is not open; not written",
			"src", src, "session", ev.GetSessionId(), "lap", ev.GetLap())
		return term(m)
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

func (c *consumer) ended(ctx context.Context, src string, ev *eventsv1.SessionEnded, at time.Time, m jetstream.Msg) error {
	if err := store.CheckKey(src, ev.GetSessionId()); err != nil {
		return c.drop(ctx, m, err)
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
		run := runOf(e.ID)
		s := c.place(src, run, f.GetSeq())
		if s == nil {
			c.keep(src, run, &f)
			continue
		}
		if s.run == "" {
			s.run = run
		}
		s.high = max(s.high, int64(f.GetSeq()))
		s.lastFrame = now
		staged = append(staged, store.StagedFrame{Src: src, SessionID: s.id, Run: run, Frame: &f})
	}
	if err := c.st.StageFrames(ctx, staged); err != nil {
		return storeError{err}
	}
	// Ack all: the last message acks the batch, and every one before it.
	if err := msgs[len(msgs)-1].Ack(); err != nil {
		c.log.WarnContext(ctx, "frames not acked; they will come again", "err", err)
	}
	return nil
}

// place is the open session of src a frame belongs to: of those whose frames start
// at or before seq and that take frames of its run, the one that starts last.
func (c *consumer) place(src, run string, seq uint32) *session {
	var best *session
	for _, s := range c.open {
		if s.src != src || s.firstSeq > seq || (s.run != "" && s.run != run) {
			continue
		}
		if best == nil || s.firstSeq > best.firstSeq || (s.firstSeq == best.firstSeq && s.order > best.order) {
			best = s
		}
	}
	return best
}

// settle writes every lap that is ready, in order, and then the end of every
// session with no lap left waiting (a session's laps before its end, so the end's
// fallbacks see the last lap). A lap is ready once a frame at or beyond its last
// seq is staged, or once its session's frames have stopped coming for the frames
// timeout.
func (c *consumer) settle(ctx context.Context) error {
	now := c.now()
	for _, s := range c.sorted() {
		for _, n := range slices.Sorted(maps.Keys(s.laps)) {
			p := s.laps[n]
			ready := s.high >= int64(p.ev.GetLastSeq())
			timedOut := !now.Before(later(p.since, s.lastFrame).Add(c.cfg.FramesTimeout))
			if !ready && !timedOut {
				break
			}
			if err := c.writeLap(ctx, s, p, !ready); err != nil {
				return err
			}
			delete(s.laps, n)
		}
		if s.end != nil && len(s.laps) == 0 {
			if err := c.st.EndSession(ctx, s.src, s.end.ev, s.endAt); err != nil {
				return storeError{err}
			}
			if err := c.st.DropStaged(ctx, s.src, s.id); err != nil {
				return storeError{err}
			}
			delete(c.open, [2]string{s.src, s.id})
			c.log.InfoContext(ctx, "session ended", "src", s.src, "session", s.id,
				"reason", s.end.ev.GetReason().String())
			if err := ack(s.end.msg); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *consumer) writeLap(ctx context.Context, s *session, p *held[*eventsv1.LapCompleted], timedOut bool) error {
	lap := p.ev
	if c.cfg.beforeLapWrite != nil {
		if err := c.cfg.beforeLapWrite(s.src, s.id, lap.GetLap()); err != nil {
			return storeError{err}
		}
	}
	staged, written, err := c.st.WriteStagedLap(ctx, s.src, lap)
	if err != nil {
		return storeError{err}
	}
	if c.cfg.afterLapWrite != nil {
		if err := c.cfg.afterLapWrite(s.src, s.id, lap.GetLap()); err != nil {
			return storeError{err}
		}
	}
	want := int(lap.GetLastSeq()) - int(lap.GetFirstSeq()) + 1
	if missing := want - staged; missing > 0 {
		c.log.WarnContext(ctx, "lap written without all its frames",
			"src", s.src, "session", s.id, "lap", lap.GetLap(), "missing", missing, "frames", staged,
			"timed_out", timedOut)
	}
	c.log.InfoContext(ctx, "lap written", "src", s.src, "session", s.id, "lap", lap.GetLap(), "frames", written)
	c.h.wrote(c.now())
	return ack(p.msg)
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
