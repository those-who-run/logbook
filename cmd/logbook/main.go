// Command logbook is the store for Those Who Run v2. `logbook run --data DIR`
// consumes frames and events from NATS into the store and serves GET /healthz
// until it gets SIGINT or SIGTERM; `logbook export --data DIR --laps` prints the
// stored laps as JSON lines; `logbook -version` prints its version. With no
// command it runs the template's example service, which serves GET /healthz.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/those-who-run/logbook/internal/service"
)

// version is what -version prints and GET /healthz reports: "dev" unless the build
// sets it with -ldflags "-X main.version=X.Y.Z". The Dockerfile does that from its
// VERSION build argument, which is "dev" unless the build passes one
// (docker build --build-arg VERSION=X.Y.Z). image.yml in those-who-run/.github
// passes VERSION=X.Y.Z when it builds on a release tag vX.Y.Z, so a released image
// reports its version; a pull request's image build passes none, and reports "dev".
var version = "dev"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process around it, so the tests can call it. It serves
// until ctx is done or the process gets SIGINT or SIGTERM, and returns the exit
// code: 0 after a clean shutdown or for -h, 1 when the service fails, 2 for a bad
// flag or configuration.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 {
		switch args[0] {
		case "export":
			return export(ctx, args[1:], getenv, stdout, stderr)
		case "run":
			return runCommand(ctx, args[1:], getenv, stdout, stderr)
		}
	}
	flags := flag.NewFlagSet("logbook", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		// -h and -help print the usage, which is what was asked for.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	cfg, err := service.ConfigFromEnv(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := service.NewLogger(stdout, cfg.LogLevel)
	if err := service.Run(ctx, cfg, logger, version); err != nil {
		logger.ErrorContext(ctx, "stopped", "err", err)
		return 1
	}
	return 0
}
