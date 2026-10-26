// Package service is the template's example service, kept to the conventions every
// Go component starts from (README.md, "Conventions"): configuration from the
// environment, log/slog writing JSON, and a health endpoint. A component replaces
// what it serves and keeps the shape.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Config is the service's configuration, read from the environment by
// ConfigFromEnv.
type Config struct {
	// Addr is the address the HTTP server listens on: ADDR, ":8080" when unset.
	Addr string
	// LogLevel is the lowest level logged: LOG_LEVEL (debug, info, warn or error),
	// info when unset.
	LogLevel slog.Level
}

// ConfigFromEnv reads the configuration through getenv, which is os.Getenv outside
// the tests. An unset or empty variable takes its default; a value that can't be
// read is an error, so a typo stops the service rather than being ignored.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: ":8080", LogLevel: slog.LevelInfo}
	if addr := getenv("ADDR"); addr != "" {
		cfg.Addr = addr
	}
	if level := getenv("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	return cfg, nil
}

// NewLogger returns a logger that writes JSON lines to w, one object per record,
// from level up.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Handler serves the health endpoint, GET /healthz, which answers 200 with the
// service's version while the process can serve requests.
func Handler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// An error here is the client gone; there is no one left to tell.
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version})
	})
	return mux
}

// shutdownGrace is how long Run lets requests in flight finish once ctx is done.
const shutdownGrace = 10 * time.Second

// The server's limits, for a small JSON service whose requests are short: a client
// that is slow to send its headers or its whole request, or slow to read the
// response, is cut off rather than holding a connection, and an idle keep-alive
// connection is closed after a minute. A component with long requests or
// streaming responses raises them.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// Run listens on cfg.Addr and serves Handler until ctx is done, then shuts the
// server down gracefully. It returns nil after a clean shutdown, or why it could
// not listen or serve.
func Run(ctx context.Context, cfg Config, logger *slog.Logger, version string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return serve(ctx, ln, logger, version)
}

// newServer is the HTTP server for h, with the limits above, logging its own errors
// through logger.
func newServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

func serve(ctx context.Context, ln net.Listener, logger *slog.Logger, version string) error {
	srv := newServer(Handler(version), logger)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "serving", "addr", ln.Addr().String(), "version", version)

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	logger.InfoContext(ctx, "shutting down")
	// ctx is already done, so the grace period needs a context of its own.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	return shutdown(grace, srv, served)
}

// shutdown stops srv gracefully within ctx and waits for its Serve to return the
// error that served carries. Once Shutdown is called Serve returns
// http.ErrServerClosed, which is a clean stop. Anything else is an error Serve hit
// on its own, as ctx was done, and is not lost: it is returned with any error from
// Shutdown.
func shutdown(ctx context.Context, srv *http.Server, served <-chan error) error {
	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, fmt.Errorf("serve: %w", err))
	}
	return errors.Join(errs...)
}
