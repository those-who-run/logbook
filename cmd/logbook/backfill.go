package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/those-who-run/logbook/internal/runner"
	"github.com/those-who-run/logbook/internal/service"
	"github.com/those-who-run/logbook/internal/store"
)

// backfill is `logbook backfill --data DIR [--nats URL]`: it opens the store,
// which migrates it to the code's schema, and fills schema 5's columns from the
// events EVENTS still holds (the schema 5 issue), then exits. It takes the store's
// lock, so it refuses while logbook runs on the same directory. TWR_DATA_DIR and
// TWR_NATS_URL stand in for the flags. It returns the exit code: 0 once done, 1
// when the store or the server fails, 2 for a usage error.
func backfill(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logbook backfill", flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", getenv("TWR_DATA_DIR"), "the data directory ($TWR_DATA_DIR)")
	url := flags.String("nats", or(getenv("TWR_NATS_URL"), runner.DefaultNATSURL), "the NATS server ($TWR_NATS_URL)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *data == "" || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: logbook backfill --data DIR [--nats URL]")
		return 2
	}
	logger := service.NewLogger(stdout, slog.LevelInfo)
	s, err := store.Open(ctx, *data)
	if err != nil {
		logger.ErrorContext(ctx, "backfill failed", "err", err)
		return 1
	}
	_, err = runner.Backfill(ctx, *url, s, logger)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		logger.ErrorContext(ctx, "backfill failed", "err", err)
		return 1
	}
	return 0
}
