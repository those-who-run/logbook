package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/those-who-run/contracts/ce"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	"google.golang.org/protobuf/proto"

	"github.com/those-who-run/logbook/internal/store"
)

// backfillSubjects are the events the backfill reads back: those whose fields
// schema 5 added columns for.
var backfillSubjects = []string{
	"twr.v1.*.evt.*.session.started",
	"twr.v1.*.evt.*.session.ended",
	"twr.v1.*.evt.*.lap.completed",
	"twr.v1.*.evt.*.card.posted",
	"twr.v1.*.evt.*.session.continued",
	"twr.v1.*.evt.*.session.kind",
	"twr.v1.*.evt.*.session.track",
	"twr.v1.*.evt.*.session.paused",
	"twr.v1.*.evt.*.session.resumed",
	"twr.v1.*.evt.*.lap.best",
	"twr.v1.*.evt.*.pit.exited",
}

// backfillStore is what the backfill needs of the store.
type backfillStore interface {
	BackfillSummary(ctx context.Context, src string, ev *eventsv1.SessionEnded) (bool, error)
	BackfillLap(ctx context.Context, src string, lap *eventsv1.LapCompleted) (bool, error)
	BackfillPosted(ctx context.Context, src string, ev *eventsv1.CardPosted) (bool, error)
	BackfillStarted(ctx context.Context, src string, ev *eventsv1.SessionStarted) (bool, error)
	ApplyFact(ctx context.Context, src string, ev proto.Message, seq uint64) (bool, error)
	FillFromSummaries(ctx context.Context) (int, error)
}

// BackfillCounts is what a backfill read and changed.
type BackfillCounts struct {
	Read     int // the events read
	Sessions int // sessions given their summary
	Laps     int // laps given their pit role
	Cards    int // sessions given their posted_at
	Starts   int // sessions given their start's joined_on_lap or fuel_capacity
	Facts    int // events applied to a session still open
	Stored   int // sessions given schema 6's columns from their stored summary
}

// Backfill fills schema 5's columns for what logbook stored before it (the schema 5
// issue): it reads the events of backfillSubjects back from EVENTS,
// from the stream's first message up to its last when the backfill began, with an
// ephemeral ordered consumer, and hands each to the store's backfill writes. Those
// fill only columns that are still NULL, on rows that exist, so it adds no session
// or lap and a second run changes nothing. A session's own events (factTypes,
// session.continued) are applied as the subscriber applies them
// (store.ApplyFact): to a session still open, again to the same end, and to one
// that has ended not at all, its summary having the last word. An event that
// cannot be read is skipped with a warning.
func Backfill(ctx context.Context, natsURL string, st backfillStore, logger *slog.Logger) (BackfillCounts, error) {
	return backfill(ctx, natsURL, st, logger, nil)
}

// backfillTimeout bounds a whole backfill: 30 days of events are a few thousand
// messages, read in seconds, so a backfill still going after it is stuck.
const backfillTimeout = 10 * time.Minute

// backfill is Backfill, with wrap, when it is set, standing in front of the
// consumer: the tests make its fetches come back empty.
func backfill(ctx context.Context, natsURL string, st backfillStore, logger *slog.Logger,
	wrap func(jetstream.Consumer) jetstream.Consumer,
) (BackfillCounts, error) {
	var n BackfillCounts
	ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()
	// Sessions that ended under schema 5 have their summary stored: schema 6's
	// columns come from it, whatever EVENTS still holds.
	stored, err := st.FillFromSummaries(ctx)
	n.Stored = stored
	if err != nil {
		return n, err
	}
	nc, err := nats.Connect(natsURL, nats.Name("logbook backfill"))
	if err != nil {
		return n, fmt.Errorf("nats %s: %w", natsURL, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return n, err
	}
	stream, err := js.Stream(ctx, eventsStream)
	if err != nil {
		return n, fmt.Errorf("%s: %w", eventsStream, err)
	}
	last := stream.CachedInfo().State.LastSeq
	if last == 0 {
		logger.InfoContext(ctx, "backfill: EVENTS is empty")
		return n, nil
	}
	oc, err := js.OrderedConsumer(ctx, eventsStream, jetstream.OrderedConsumerConfig{
		FilterSubjects:    backfillSubjects,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: 30 * time.Second,
	})
	if err != nil {
		return n, err
	}
	defer func() {
		if info := oc.CachedInfo(); info != nil {
			_ = js.DeleteConsumer(context.WithoutCancel(ctx), eventsStream, info.Name)
		}
	}()
	cons := oc
	if wrap != nil {
		cons = wrap(cons)
	}
	read, err := readBack(ctx, cons, last, st, logger, &n)
	if err != nil {
		return n, fmt.Errorf("backfill stopped after stream sequence %d of %d: %w", read, last, err)
	}
	logger.InfoContext(ctx, "backfill done", "last", last, "last_read", read,
		"read", n.Read, "sessions", n.Sessions, "laps", n.Laps, "cards", n.Cards, "starts", n.Starts, "facts", n.Facts, "stored", n.Stored)
	return n, nil
}

// readBack hands each event the consumer delivers to the store, up to stream
// sequence last, and returns the stream sequence of the last event it read. An
// empty fetch ends it only once the server says none of its events is left
// (NumPending 0): on a loaded host a fetch can time out with events still to come.
func readBack(ctx context.Context, oc jetstream.Consumer, last uint64, st backfillStore, logger *slog.Logger,
	n *BackfillCounts,
) (uint64, error) {
	var read uint64
	for {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		msgs, err := fetch(oc, eventsBatch, time.Second)
		if err != nil {
			return read, err
		}
		if len(msgs) == 0 {
			info, err := oc.Info(ctx)
			if err != nil {
				return read, err
			}
			if info.NumPending == 0 {
				return read, nil // none of its events after the last read
			}
			logger.InfoContext(ctx, "backfill: a fetch came back empty with events pending; fetching again",
				"pending", info.NumPending, "last_read", read)
			continue
		}
		for _, m := range msgs {
			md, err := m.Metadata()
			if err != nil {
				return read, err
			}
			if md.Sequence.Stream > last {
				return read, nil
			}
			read = md.Sequence.Stream
			n.Read++
			if err := backfillOne(ctx, st, m, n); err != nil {
				var bad badEvent
				if !errors.As(err, &bad) {
					return read, err
				}
				logger.WarnContext(ctx, "backfill: event skipped", "subject", m.Subject(), "err", bad.error)
			}
			if md.Sequence.Stream == last {
				return read, nil
			}
		}
	}
}

// badEvent is an event the backfill cannot read; it is skipped.
type badEvent struct{ error }

func backfillOne(ctx context.Context, st backfillStore, m jetstream.Msg, n *BackfillCounts) error {
	src, ok := eventSource(m.Subject())
	if !ok {
		return badEvent{errors.New("not an event subject")}
	}
	h := ce.Header(m.Headers())
	var (
		did bool
		err error
		cnt *int
	)
	switch h.Get(ce.TypeHeader) {
	case typeEnded:
		var ev eventsv1.SessionEnded
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return badEvent{derr}
		}
		if err := checkEventKey(src, ev.GetSessionId()); err != nil {
			return err
		}
		did, err = st.BackfillSummary(ctx, src, &ev)
		cnt = &n.Sessions
	case typeLap:
		var ev eventsv1.LapCompleted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return badEvent{derr}
		}
		if err := checkEventKey(src, ev.GetSessionId()); err != nil {
			return err
		}
		did, err = st.BackfillLap(ctx, src, &ev)
		cnt = &n.Laps
	case typePosted:
		var ev eventsv1.CardPosted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return badEvent{derr}
		}
		if err := checkEventKey(src, ev.GetSessionId()); err != nil {
			return err
		}
		did, err = st.BackfillPosted(ctx, src, &ev)
		cnt = &n.Cards
	case typeStarted:
		var ev eventsv1.SessionStarted
		if _, derr := ce.Decode(h, m.Data(), &ev); derr != nil {
			return badEvent{derr}
		}
		if err := checkEventKey(src, ev.GetSessionId()); err != nil {
			return err
		}
		did, err = st.BackfillStarted(ctx, src, &ev)
		cnt = &n.Starts
	default:
		mk, ok := factTypes[h.Get(ce.TypeHeader)]
		if h.Get(ce.TypeHeader) == typeContinued {
			mk, ok = func() proto.Message { return &eventsv1.SessionContinued{} }, true
		}
		if !ok {
			return nil
		}
		ev := mk()
		if _, derr := ce.Decode(h, m.Data(), ev); derr != nil {
			return badEvent{derr}
		}
		if err := checkEventKey(src, ev.(interface{ GetSessionId() string }).GetSessionId()); err != nil { //nolint:forcetypeassert // each has one
			return err
		}
		// Of a session that has ended, the summary has the last word and this
		// changes nothing; of one the store does not hold, it is skipped.
		did, err = st.ApplyFact(ctx, src, ev, streamSeq(m))
		if errors.Is(err, store.ErrUnknownSession) {
			did, err = false, nil
		}
		cnt = &n.Facts
	}
	if err != nil {
		return err
	}
	if did {
		*cnt++
	}
	return nil
}

// checkEventKey is a badEvent for a source and session id the store would refuse.
func checkEventKey(src, sessionID string) error {
	if err := store.CheckKey(src, sessionID); err != nil {
		return badEvent{err}
	}
	return nil
}
