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

	"github.com/those-who-run/logbook/internal/store"
)

// backfillSubjects are the events the backfill reads back: the three whose fields
// schema 5 added columns for.
var backfillSubjects = []string{
	"twr.v1.*.evt.*.session.ended",
	"twr.v1.*.evt.*.lap.completed",
	"twr.v1.*.evt.*.card.posted",
}

// backfillStore is what the backfill needs of the store.
type backfillStore interface {
	BackfillSummary(ctx context.Context, src string, ev *eventsv1.SessionEnded) (bool, error)
	BackfillLap(ctx context.Context, src string, lap *eventsv1.LapCompleted) (bool, error)
	BackfillPosted(ctx context.Context, src string, ev *eventsv1.CardPosted) (bool, error)
}

// BackfillCounts is what a backfill read and changed.
type BackfillCounts struct {
	Read     int // the events read
	Sessions int // sessions given their summary
	Laps     int // laps given their pit role
	Cards    int // sessions given their posted_at
}

// Backfill fills schema 5's columns for what logbook stored before it (the schema 5
// issue): it reads session.ended, lap.completed and card.posted back from EVENTS,
// from the stream's first message up to its last when the backfill began, with an
// ephemeral ordered consumer, and hands each to the store's backfill writes. Those
// fill only columns that are still NULL, on rows that exist, so it adds no session
// or lap and a second run changes nothing. An event that cannot be read is
// skipped with a warning.
func Backfill(ctx context.Context, natsURL string, st backfillStore, logger *slog.Logger) (BackfillCounts, error) {
	var n BackfillCounts
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
	if err := readBack(ctx, oc, last, st, logger, &n); err != nil {
		return n, err
	}
	logger.InfoContext(ctx, "backfill done", "read", n.Read, "sessions", n.Sessions, "laps", n.Laps, "cards", n.Cards)
	return n, nil
}

// readBack hands each event the consumer delivers to the store, up to stream
// sequence last, or until a fetch finds none left.
func readBack(ctx context.Context, oc jetstream.Consumer, last uint64, st backfillStore, logger *slog.Logger,
	n *BackfillCounts,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msgs, err := fetch(oc, eventsBatch, time.Second)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil // nothing of the three after the last read
		}
		for _, m := range msgs {
			md, err := m.Metadata()
			if err != nil {
				return err
			}
			if md.Sequence.Stream > last {
				return nil
			}
			n.Read++
			if err := backfillOne(ctx, st, m, n); err != nil {
				var bad badEvent
				if !errors.As(err, &bad) {
					return err
				}
				logger.WarnContext(ctx, "backfill: event skipped", "subject", m.Subject(), "err", bad.error)
			}
			if md.Sequence.Stream == last {
				return nil
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
	default:
		return nil
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
