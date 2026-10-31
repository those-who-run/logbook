package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedLog is a buffer the run writes from its own goroutine while the test reads.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestRunUsageErrors(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no data directory":  {nil, nil, "usage: logbook run"},
		"an argument left":   {[]string{"--data", t.TempDir(), "extra"}, nil, "usage: logbook run"},
		"a zero timeout":     {[]string{"--data", t.TempDir(), "--frames-timeout", "0s"}, nil, "usage: logbook run"},
		"an unknown flag":    {[]string{"--nope"}, nil, "-nope"},
		"a timeout of words": {nil, map[string]string{"TWR_DATA_DIR": t.TempDir(), "TWR_FRAMES_TIMEOUT": "soon"}, "TWR_FRAMES_TIMEOUT"},
		"a bad log level":    {nil, map[string]string{"TWR_DATA_DIR": t.TempDir(), "LOG_LEVEL": "loud"}, "LOG_LEVEL"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(t.Context(), append([]string{"run"}, c.args...), env(c.env), &bytes.Buffer{}, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q; want 2 and %q", code, stderr.String(), c.want)
			}
		})
	}
}

func TestRunHelpIsNotAnError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"run", "-h"}, env(nil), &bytes.Buffer{}, &stderr); code != 0 ||
		!strings.Contains(stderr.String(), "-frames-timeout") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

// `logbook run` with no server to reach serves /healthz, logs the store it opened,
// and exits 0 once stopped; a data directory it cannot use exits 1.
func TestRunStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	vars := map[string]string{
		"TWR_DATA_DIR": dir, "TWR_NATS_URL": "nats://127.0.0.1:1",
		"TWR_HEALTH_ADDR": "127.0.0.1:0", "TWR_FRAMES_TIMEOUT": "5s", "LOG_LEVEL": "debug",
	}
	var stdout lockedLog
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"run"}, env(vars), &stdout, &bytes.Buffer{}) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), `"msg":"serving"`) {
		if time.Now().After(deadline) {
			t.Fatalf("not serving after 10 s: %s", stdout.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), `"msg":"store open","sessions":0,"schema_version":7`) {
		t.Errorf("no store line in %s", stdout.String())
	}

	// The same directory again, while the first holds it.
	var busy bytes.Buffer
	if code := run(t.Context(), []string{"run", "--data", dir}, env(map[string]string{"TWR_HEALTH_ADDR": "127.0.0.1:0"}),
		&busy, &bytes.Buffer{}); code != 1 || !strings.Contains(busy.String(), "in use") {
		t.Errorf("a busy directory: exit %d, log %s", code, busy.String())
	}

	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d after a stop, log %s", code, stdout.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still running 10 s after the stop")
	}
}
