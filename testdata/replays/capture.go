//go:build ignore

// capture writes the replay fixtures in this directory again: what ecu publishes
// for one of v1's recordings, as JSON lines, one message each, in the order the
// server took them: {"subject", "headers", "payload"}. The payload is the
// message's data, protobuf's JSON form (contracts' ce), compacted. The lines are
// gzipped, which keeps the file under the commit hooks' 1 MB limit.
//
// It installs ecu at ecuVersion into a directory of its own, starts nats-server
// with JetStream and the streams in internal/runner/testdata/streams (paddock's),
// subscribes to every subject logbook reads, and runs
//
//	ecu replay --nats URL --src SRC --speed max RECORDING
//
// with no --clock, so the replay dates its first frame 2026-01-01T00:00:00Z. It
// fails unless the recording's sha256 is the -sha256 flag's, which the person
// running it copies from paddock's recordings/SOURCES.yaml, and unless the
// subscription saw every message the streams stored.
//
// A capture is reproducible in content, not in bytes: the session and run ids
// are ULIDs ecu mints anew, and where the events fall among the frames depends
// on when each was published, so a new capture always changes the fixture's
// sha256, even from the same ecu. The frames, the events and their order among
// themselves stay the same.
//
// From the repository's root, the two fixtures here:
//
//	go run testdata/replays/capture.go -src replay-suzuka \
//	    -recording PADDOCK/recordings/real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz \
//	    -sha256 9684c2ee9e1da825b0870b22bfec81ecff833a3ae20e523c0efc61e8863e5bdb \
//	    > testdata/replays/suzuka-pit-stop-2026-10-15.jsonl.gz
//	go run testdata/replays/capture.go -src replay-suzuka-no-stop \
//	    -recording PADDOCK/recordings/real-nuc-suzuka-no-stop-2026-10-15.jsonl.gz \
//	    -sha256 0477116644a19ab97aac4c19dbecaa4129ee691c57378562cf36c7a094c4aeaa \
//	    > testdata/replays/suzuka-no-stop-2026-10-15.jsonl.gz
//
// PADDOCK is a paddock clone; its recordings/ holds v1's fixtures.
// nats-server is the one the runner tests download (TWR_TEST_NATS_SERVER, or the
// user's cache), or -nats-server. After a capture, update the fixture's sha256,
// and the ecu version and recording sum beside it, in replayFixtures in
// internal/store/replay_test.go, which TestTheReplayFixturesAreTheCaptures checks.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	ecuModule  = "github.com/those-who-run/ecu/cmd/ecu"
	ecuVersion = "v0.1.4"
)

type line struct {
	Subject string              `json:"subject"`
	Headers map[string][]string `json:"headers"`
	Payload json.RawMessage     `json:"payload"`
}

func main() {
	recording := flag.String("recording", "", "the v1 recording to replay")
	sum := flag.String("sha256", "", "the recording's sha256, from paddock's recordings/SOURCES.yaml")
	src := flag.String("src", "replay-suzuka", "the source the replay publishes under")
	natsServer := flag.String("nats-server", defaultNATSServer(), "nats-server v2.15.1")
	flag.Parse()
	if err := capture(*recording, *sum, *src, *natsServer); err != nil {
		fmt.Fprintln(os.Stderr, "capture:", err)
		os.Exit(1)
	}
}

func defaultNATSServer() string {
	if bin := os.Getenv("TWR_TEST_NATS_SERVER"); bin != "" {
		return bin
	}
	cache, _ := os.UserCacheDir()
	return filepath.Join(cache, "those-who-run", "nats-server-v2.15.1-"+runtime.GOOS+"-"+runtime.GOARCH, "nats-server")
}

func capture(recording, want, src, natsServer string) error {
	b, err := os.ReadFile(recording)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s has sha256 %x, want %s", recording, got, want)
	}
	tmp, err := os.MkdirTemp("", "capture-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	install := exec.CommandContext(ctx, "go", "install", ecuModule+"@"+ecuVersion)
	install.Env = append(os.Environ(), "GOBIN="+tmp)
	install.Stdout, install.Stderr = os.Stderr, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("go install %s@%s: %w", ecuModule, ecuVersion, err)
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	url := "nats://127.0.0.1:" + strconv.Itoa(port)
	srv := exec.CommandContext(ctx, natsServer, "-a", "127.0.0.1", "-p", strconv.Itoa(port), "-js", "-sd", filepath.Join(tmp, "js"))
	if err := srv.Start(); err != nil {
		return err
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	var nc *nats.Conn
	for range 100 {
		if nc, err = nats.Connect(url); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	for _, name := range []string{"events.json", "telemetry.json"} {
		b, err := os.ReadFile(filepath.Join("internal", "runner", "testdata", "streams", name))
		if err != nil {
			return err
		}
		var cfg jetstream.StreamConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return err
		}
		if _, err := js.CreateStream(ctx, cfg); err != nil {
			return err
		}
	}

	// One subscription, so the messages stay in the order the server took them.
	msgs := make(chan *nats.Msg, 1<<20)
	_, err = nc.ChanSubscribe("twr.v1.>", msgs)
	if err != nil {
		return err
	}
	if err := nc.Flush(); err != nil {
		return err
	}

	replay := exec.CommandContext(ctx, filepath.Join(tmp, "ecu"), "replay", "--nats", url, "--src", src, "--speed", "max", recording)
	replay.Stdout, replay.Stderr = os.Stderr, os.Stderr
	if err := replay.Run(); err != nil {
		return fmt.Errorf("ecu replay: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return err
	}

	var stored uint64
	for _, name := range []string{"EVENTS", "TELEMETRY"} {
		s, err := js.Stream(ctx, name)
		if err != nil {
			return err
		}
		info, err := s.Info(ctx)
		if err != nil {
			return err
		}
		stored += info.State.Msgs
	}

	var out []line
	for len(msgs) > 0 {
		m := <-msgs
		// What the streams hold, which is what logbook reads: not the 20 Hz copy.
		if !strings.HasSuffix(m.Subject, ".frame.gt7") && !strings.Contains(m.Subject, ".evt.") {
			continue
		}
		var payload bytes.Buffer
		if err := json.Compact(&payload, m.Data); err != nil {
			return fmt.Errorf("%s: %w", m.Subject, err)
		}
		out = append(out, line{Subject: m.Subject, Headers: m.Header, Payload: payload.Bytes()})
	}
	if uint64(len(out)) != stored {
		return fmt.Errorf("captured %d messages, the streams stored %d", len(out), stored)
	}
	zw, err := gzip.NewWriterLevel(os.Stdout, gzip.BestCompression)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(zw)
	for _, l := range out {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return zw.Close()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port, nil
	}
	return 0, errors.New("no TCP port")
}
