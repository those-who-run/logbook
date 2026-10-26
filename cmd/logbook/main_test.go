package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// readyLog collects the service's log and closes ready once it logs "serving", so a
// test stops the service when it is listening rather than after a guessed delay.
type readyLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func newReadyLog() *readyLog { return &readyLog{ready: make(chan struct{})} }

// Write takes one record per call, as slog's handlers write them.
func (l *readyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytes.Contains(p, []byte(`"msg":"serving"`)) {
		l.once.Do(func() { close(l.ready) })
	}
	return l.buf.Write(p)
}

func (l *readyLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// heldAddress returns an address another listener holds for the rest of the test,
// so listening on it fails at once, with no name to resolve.
func heldAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("printed %q, want %q", got, version)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "-help"} {
		t.Run(arg, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{arg}, env(nil), &bytes.Buffer{}, &stderr); code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if !strings.Contains(stderr.String(), "-version") {
				t.Errorf("stderr %q has no usage", stderr.String())
			}
		})
	}
}

func TestAnUnknownFlagIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"-nope"}, env(nil), &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-nope") {
		t.Errorf("stderr %q does not name the flag", stderr.String())
	}
}

func TestABadConfigurationIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"LOG_LEVEL": "loud"}), &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "LOG_LEVEL") {
		t.Errorf("exit %d, stderr %q; want 2 and LOG_LEVEL named", code, stderr.String())
	}
}

func TestAServiceThatCannotStartExitsOne(t *testing.T) {
	var stdout bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"ADDR": heldAddress(t)}), &stdout, &bytes.Buffer{})
	if code != 1 || !strings.Contains(stdout.String(), `"msg":"stopped"`) {
		t.Errorf("exit %d, log %q; want 1 and the failure logged", code, stdout.String())
	}
}

func TestServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := newReadyLog()
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, nil, env(map[string]string{"ADDR": "127.0.0.1:0"}), stdout, &bytes.Buffer{})
	}()
	// The timeouts only stop a hung test; the service is cancelled once it serves.
	select {
	case <-stdout.ready:
	case code := <-exit:
		t.Fatalf("exit %d before serving, log %q", code, stdout.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	cancel()
	select {
	case code := <-exit:
		if code != 0 || !strings.Contains(stdout.String(), `"msg":"shutting down"`) {
			t.Errorf("exit %d, log %q; want 0 and the shutdown logged", code, stdout.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still running 10 s after cancel")
	}
}
