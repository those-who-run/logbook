package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/those-who-run/logbook/internal/importv1"
	"github.com/those-who-run/logbook/internal/store"
)

// importV1 is `logbook import-v1 --from DIR --data DIR [--dry-run]` (paddock#264):
// it imports v1's data directory, a copy taken with v1 stopped, into the store
// under the source v1-import, and prints a line per v1 session and the counts. It
// opens the store as logbook does, lock and all, so it refuses while `logbook
// run` holds it (R4). --dry-run prints the same summary and opens no store. It
// returns the exit code: 0 once done, 1 when a session failed to read or the
// import failed, 2 for a usage error.
func importV1(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logbook import-v1", flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "a copy of v1's data directory (index.sqlite and sessions/), read only")
	data := flags.String("data", getenv("TWR_DATA_DIR"), "the data directory (default $TWR_DATA_DIR)")
	dryRun := flags.Bool("dry-run", false, "print the summary and write nothing")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *from == "" || (*data == "" && !*dryRun) || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: logbook import-v1 --from DIR --data DIR [--dry-run]")
		return 2
	}
	var (
		st  *store.Store
		dst importv1.Store
	)
	if !*dryRun {
		var err error
		if st, err = store.Open(ctx, *data); err != nil {
			_, _ = fmt.Fprintln(stderr, "logbook import-v1:", err)
			return 1
		}
		dst = st
	}
	sum, err := importv1.Run(ctx, *from, dst)
	if st != nil {
		err = errors.Join(err, st.Close())
	}
	if werr := sum.Write(stdout); err == nil {
		err = werr
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "logbook import-v1:", err)
		return 1
	}
	if n := sum.Count(importv1.Failed); n > 0 {
		_, _ = fmt.Fprintf(stderr, "logbook import-v1: %d sessions failed to read\n", n)
		return 1
	}
	return 0
}
