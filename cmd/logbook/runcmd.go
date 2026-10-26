package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/those-who-run/logbook/internal/runner"
	"github.com/those-who-run/logbook/internal/service"
)

// runCommand is `logbook run --data DIR [--nats URL] [--frames-timeout D]`: it
// consumes frames and events into the store until SIGINT or SIGTERM (paddock#38).
// The environment stands in for the flags: TWR_DATA_DIR, TWR_NATS_URL and
// TWR_FRAMES_TIMEOUT, with TWR_HEALTH_ADDR for GET /healthz and LOG_LEVEL. It
// returns the exit code: 0 after a clean stop, 1 when the store or the health
// address fails, 2 for a usage error.
func runCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logbook run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfg := runner.Config{HealthAddr: getenv("TWR_HEALTH_ADDR")}
	flags.StringVar(&cfg.DataDir, "data", getenv("TWR_DATA_DIR"), "the data directory ($TWR_DATA_DIR)")
	flags.StringVar(&cfg.NATSURL, "nats", or(getenv("TWR_NATS_URL"), runner.DefaultNATSURL),
		"the NATS server ($TWR_NATS_URL)")
	timeout := runner.DefaultFramesTimeout
	if v := getenv("TWR_FRAMES_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			_, _ = fmt.Fprintf(stderr, "TWR_FRAMES_TIMEOUT: %q is not a positive duration such as 10s\n", v)
			return 2
		}
		timeout = d
	}
	flags.DurationVar(&cfg.FramesTimeout, "frames-timeout", timeout,
		"how long a lap waits for missing frames before it is written without them ($TWR_FRAMES_TIMEOUT)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if cfg.DataDir == "" || flags.NArg() > 0 || cfg.FramesTimeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "usage: logbook run --data DIR [--nats URL] [--frames-timeout D]")
		return 2
	}
	level := slog.LevelInfo
	if v := getenv("LOG_LEVEL"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			_, _ = fmt.Fprintf(stderr, "LOG_LEVEL: %v\n", err)
			return 2
		}
	}
	logger := service.NewLogger(stdout, level)
	if err := runner.Run(ctx, cfg, logger, version); err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	return 0
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
