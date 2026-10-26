// Package runner is `logbook run`: it binds two durable pull consumers, one on
// TELEMETRY for frames and one on EVENTS for the session and lap events, and feeds
// the store with them (paddock#38). A fetched batch of frames is staged in the store
// and acked; a lap is written from its staged frames once a frame at or beyond its
// end is staged, or once its frames have stopped coming for the frames timeout; and
// every event is acked only after its write commits, so a restart picks up where
// the last commit left off and nothing is written twice.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/those-who-run/logbook/internal/service"
	"github.com/those-who-run/logbook/internal/store"
)

// The consumers logbook creates at start when they are missing, with their
// settings here, next to the code that relies on them (paddock#38).
const (
	FramesConsumer = "logbook-frames"
	EventsConsumer = "logbook-events"

	framesStream  = "TELEMETRY"
	eventsStream  = "EVENTS"
	framesSubject = "twr.v1.*.frame.gt7"
	eventsSubject = "twr.v1.*.evt.>"
)

// Defaults for the configuration.
const (
	DefaultNATSURL       = nats.DefaultURL
	DefaultHealthAddr    = ":8080"
	DefaultFramesTimeout = 10 * time.Second
)

// The consumers' limits. A batch of frames is staged in one transaction and acked
// with one ack-all; at 60 frames a second, 500 is about 8 s of live driving, and a
// replay at full speed fetches as fast as the store stages. Ack pending stays above
// a batch, so the server never stops a fetch short.
const (
	framesBatch   = 500
	eventsBatch   = 100
	maxAckPending = 1000
	ackWait       = 30 * time.Second
)

// Config is what `logbook run` needs.
type Config struct {
	// NATSURL is the server: --nats or TWR_NATS_URL, DefaultNATSURL when unset.
	NATSURL string
	// DataDir is the store's data directory: --data or TWR_DATA_DIR.
	DataDir string
	// HealthAddr is where GET /healthz listens: TWR_HEALTH_ADDR, DefaultHealthAddr
	// when unset.
	HealthAddr string
	// FramesTimeout is how long a lap waits for its frames after the last one came
	// before it is written with those there are: --frames-timeout or
	// TWR_FRAMES_TIMEOUT, DefaultFramesTimeout when unset.
	FramesTimeout time.Duration

	// The tests shorten these; zero is the default.
	ackWait       time.Duration // the consumers' ack wait
	fetchWait     time.Duration // how long a fetch of frames waits for one
	retryWait     time.Duration // the pause before binding or fetching again after a failure
	reconnectWait time.Duration // the pause between attempts to reach the server

	// beforeLapWrite, when set, runs before each lap is written; an error stops the
	// runner where it stands, with nothing acked, as a crash would.
	beforeLapWrite func(src, sessionID string, lap int32) error
	// afterLapWrite is the same after the lap's commit and before its ack.
	afterLapWrite func(src, sessionID string, lap int32) error
}

func (c Config) withDefaults() Config {
	if c.NATSURL == "" {
		c.NATSURL = DefaultNATSURL
	}
	if c.HealthAddr == "" {
		c.HealthAddr = DefaultHealthAddr
	}
	if c.FramesTimeout == 0 {
		c.FramesTimeout = DefaultFramesTimeout
	}
	if c.ackWait == 0 {
		c.ackWait = ackWait
	}
	if c.fetchWait == 0 {
		c.fetchWait = 250 * time.Millisecond
	}
	if c.retryWait == 0 {
		c.retryWait = time.Second
	}
	if c.reconnectWait == 0 {
		c.reconnectWait = 2 * time.Second
	}
	return c
}

// shutdownGrace bounds the health server's shutdown; the whole stop stays within
// the 10 s R8 allows.
const shutdownGrace = 3 * time.Second

// Run opens the store, serves GET /healthz, connects to the server (retrying until
// it answers) and consumes until ctx is done. Then it stops fetching, finishes the
// write in progress, closes the store and returns nil. It returns an error when the
// store cannot be opened or written, or the health address cannot be listened on.
func Run(ctx context.Context, cfg Config, logger *slog.Logger, version string) (err error) {
	cfg = cfg.withDefaults()
	if cfg.DataDir == "" {
		return errors.New("no data directory: pass --data or set TWR_DATA_DIR")
	}
	if cfg.FramesTimeout < 0 {
		return fmt.Errorf("the frames timeout %v is negative", cfg.FramesTimeout)
	}
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	sessions, err := st.Sessions(ctx)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "store open", "sessions", sessions, "schema_version", store.SchemaVersion)

	h := &health{version: version}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := service.NewServer(h.handler(), logger)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "serving", "addr", ln.Addr().String(), "version", version)
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		err = errors.Join(err, srv.Shutdown(shutdown))
		if serr := <-served; !errors.Is(serr, http.ErrServerClosed) {
			err = errors.Join(err, serr)
		}
	}()

	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name("logbook"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(cfg.reconnectWait),
		nats.ConnectHandler(func(*nats.Conn) { logger.InfoContext(ctx, "connected", "server", redact(cfg.NATSURL)) }),
		nats.ReconnectHandler(func(*nats.Conn) { logger.InfoContext(ctx, "reconnected") }),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.WarnContext(ctx, "disconnected", "err", err)
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("nats: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	h.nc.Store(nc)

	r := newConsumer(cfg, st, js, h, logger)
	if err := r.restore(ctx); err != nil {
		return err
	}
	return r.run(ctx)
}

// redact is a server URL without the user and password it may carry, for the log.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(the configured server)"
	}
	return u.Scheme + "://" + u.Host
}

// consumerConfigs are the two consumers' settings. Frames are acked all at once,
// after the batch they end is staged; events one at a time, after each one's write.
func consumerConfigs(wait time.Duration) (frames, events jetstream.ConsumerConfig) {
	frames = jetstream.ConsumerConfig{
		Durable:       FramesConsumer,
		Description:   "logbook: every frame, staged until its lap is written (paddock#38)",
		FilterSubject: framesSubject,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckAllPolicy,
		AckWait:       wait,
		MaxAckPending: maxAckPending,
		MaxDeliver:    -1,
	}
	events = jetstream.ConsumerConfig{
		Durable:       EventsConsumer,
		Description:   "logbook: session and lap events, each acked after its write (paddock#38)",
		FilterSubject: eventsSubject,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       wait,
		MaxAckPending: maxAckPending,
		MaxDeliver:    -1,
	}
	return frames, events
}
