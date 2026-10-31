package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/those-who-run/logbook/internal/store"
)

// export is `logbook export --data DIR --laps [--src SRC]`: the laps of one source
// (live unless --src names another) as JSON lines, one object per lap, for the
// parity harness. It opens the store as logbook does, lock and all, so it refuses
// while logbook runs on the same directory. It returns the exit code: 0 once the
// laps are printed, 1 when the store can't be read, 2 for a usage error.
func export(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logbook export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", getenv("TWR_DATA_DIR"), "the data directory (default $TWR_DATA_DIR)")
	laps := flags.Bool("laps", false, "print the laps, one JSON object per line")
	src := flags.String("src", store.SourceLive, "the source whose sessions to print: live, parity, v1-import or replay-<run>")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *data == "" || !*laps || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: logbook export --data DIR --laps [--src SRC]")
		return 2
	}
	s, err := store.Open(ctx, *data)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	err = s.ExportLaps(ctx, stdout, *src)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
