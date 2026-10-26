package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// readyLog collects a log and keeps the "serving" record once it is written, so a
// test talks to the server and stops it when it is listening rather than after a
// guessed delay.
type readyLog struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	serving map[string]any
	ready   chan struct{}
}

func newReadyLog() *readyLog { return &readyLog{ready: make(chan struct{})} }

// Write takes one JSON record per call, as slog's handlers write them.
func (l *readyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var record map[string]any
	if l.serving == nil && json.Unmarshal(p, &record) == nil && record["msg"] == "serving" {
		l.serving = record
		close(l.ready)
	}
	return l.buf.Write(p)
}

func (l *readyLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// addr is the address the "serving" record names; call it once ready is closed.
func (l *readyLog) addr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	addr, _ := l.serving["addr"].(string)
	return addr
}

func get(t *testing.T, url string) int {
	t.Helper()
	code, err := fetch(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// fetch is get for a goroutine other than the test's, where t.Fatal would end only
// that goroutine and leave the test waiting: it returns the error instead.
func fetch(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func TestConfigFromEnv(t *testing.T) {
	for _, c := range []struct {
		name string
		vars map[string]string
		want Config
	}{
		{"defaults", nil, Config{Addr: ":8080", LogLevel: slog.LevelInfo}},
		{"set", map[string]string{"ADDR": "127.0.0.1:9000", "LOG_LEVEL": "debug"},
			Config{Addr: "127.0.0.1:9000", LogLevel: slog.LevelDebug}},
		{"upper case level", map[string]string{"LOG_LEVEL": "WARN"},
			Config{Addr: ":8080", LogLevel: slog.LevelWarn}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ConfigFromEnv(env(c.vars))
			if err != nil || got != c.want {
				t.Errorf("ConfigFromEnv = %+v, %v; want %+v, nil", got, err, c.want)
			}
		})
	}
}

func TestConfigFromEnvRefusesAnUnknownLevel(t *testing.T) {
	_, err := ConfigFromEnv(env(map[string]string{"LOG_LEVEL": "loud"}))
	if err == nil || !strings.HasPrefix(err.Error(), "LOG_LEVEL: ") {
		t.Errorf("ConfigFromEnv(LOG_LEVEL=loud) error = %v, want one naming LOG_LEVEL", err)
	}
}

func TestNewLoggerWritesJSONFromItsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelWarn)
	logger.Info("dropped")
	logger.Warn("kept", "lap", 3)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("not JSON: %v: %q", err, lines[0])
	}
	if record["msg"] != "kept" || record["level"] != "WARN" || record["lap"] != float64(3) {
		t.Errorf("record = %v", record)
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler("1.2.3").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != "1.2.3" {
		t.Errorf("body = %v", body)
	}
}

func TestHealthzIsGetOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler("1.2.3").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz status = %d, want 405", rec.Code)
	}
}

func TestRunServesUntilCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, NewLogger(&logs, slog.LevelInfo), "test") }()

	if code := get(t, "http://"+ln.Addr().String()+"/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz status = %d", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
	if !strings.Contains(logs.String(), `"msg":"shutting down"`) {
		t.Errorf("no shutdown logged: %s", logs.String())
	}
}

func TestRunReportsAnAddressItCannotListenOn(t *testing.T) {
	// An address another listener holds fails at once, with no name to resolve.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	err = Run(t.Context(), Config{Addr: held.Addr().String()}, NewLogger(io.Discard, slog.LevelInfo), "test")
	if err == nil || !strings.HasPrefix(err.Error(), "listen: ") {
		t.Errorf("Run error = %v, want a listen error", err)
	}
}

func TestRunListensOnTheConfiguredAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := newReadyLog()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Config{Addr: "127.0.0.1:0"}, NewLogger(logs, slog.LevelInfo), "test") }()
	// The timeouts only stop a hung test; Run is cancelled once it serves.
	select {
	case <-logs.ready:
	case err := <-done:
		t.Fatalf("Run = %v before serving, log %q", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("not serving after 10 s")
	}
	addr := logs.addr()
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Fatalf("serving on %q, want the configured 127.0.0.1", addr)
	}
	if code := get(t, "http://"+addr+"/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz on %s status = %d", addr, code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run still serving 10 s after cancel")
	}
}

func TestTheServerCutsOffSlowAndIdleClients(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), NewLogger(io.Discard, slog.LevelInfo))
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 10 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if srv.ErrorLog == nil {
		t.Error("the server's own errors are not logged")
	}
}

func TestServeReportsAListenerThatFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // Serve fails at once on a closed listener
	err = serve(t.Context(), ln, NewLogger(io.Discard, slog.LevelInfo), "test")
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("serve on a closed listener = %v, want an error", err)
	}
}

func TestShutdownTakesTheServerClosedAsACleanStop(t *testing.T) {
	for _, servedErr := range []error{http.ErrServerClosed, nil} {
		srv := newServer(http.NotFoundHandler(), NewLogger(io.Discard, slog.LevelInfo))
		served := make(chan error, 1)
		served <- servedErr
		if err := shutdown(t.Context(), srv, served); err != nil {
			t.Errorf("shutdown with Serve returning %v = %v, want nil", servedErr, err)
		}
	}
}

func TestShutdownReturnsWhatServeFailedWith(t *testing.T) {
	// Serve can fail on its own as ctx is done, and serve's select then takes ctx.
	failed := errors.New("accept: too many open files")
	srv := newServer(http.NotFoundHandler(), NewLogger(io.Discard, slog.LevelInfo))
	served := make(chan error, 1)
	served <- failed
	err := shutdown(t.Context(), srv, served)
	if !errors.Is(err, failed) || !strings.HasPrefix(err.Error(), "serve: ") {
		t.Errorf("shutdown = %v, want the serve error %q", err, failed)
	}
}

func TestShutdownReturnsBothItsOwnAndServesErrors(t *testing.T) {
	// A request still in flight when the grace period is over makes Shutdown fail.
	entered, release := make(chan struct{}), make(chan struct{})
	srv := newServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}), NewLogger(io.Discard, slog.LevelInfo))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(ln) }()
	answered := make(chan error, 1)
	go func() {
		_, err := fetch(t.Context(), "http://"+ln.Addr().String()+"/")
		answered <- err
	}()
	select {
	case <-entered:
	case err := <-answered:
		t.Fatalf("the request was answered before it reached the handler: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the request did not reach the handler in 10 s")
	}

	failed := errors.New("accept: too many open files")
	served := make(chan error, 1)
	served <- failed
	over, cancel := context.WithCancel(t.Context())
	cancel()
	err = shutdown(over, srv, served)
	close(release)
	if err := <-answered; err != nil {
		t.Errorf("GET in flight during shutdown: %v", err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, failed) {
		t.Errorf("shutdown = %v, want both the shutdown and the serve error", err)
	}
	if err := <-stopped; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve = %v after Shutdown, want http.ErrServerClosed", err)
	}
}

func TestShutdownWaitsForServeToReturn(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), NewLogger(io.Discard, slog.LevelInfo))
	called := make(chan struct{})
	srv.RegisterOnShutdown(func() { close(called) })
	// Unbuffered, so Serve's error is sent only while shutdown is waiting for it.
	served := make(chan error)
	done := make(chan error, 1)
	go func() { done <- shutdown(t.Context(), srv, served) }()
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown not called in 10 s")
	}

	// Serve has not returned yet, so shutdown must not have either. A shutdown that
	// stops waiting returns within the window; one that waits never does, so the
	// window can only make this test pass wrongly, never fail wrongly.
	select {
	case err := <-done:
		t.Fatalf("shutdown = %v before Serve returned", err)
	case <-time.After(100 * time.Millisecond):
	}
	failed := errors.New("accept: too many open files")
	select {
	case served <- failed:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown no longer waiting for Serve after 10 s")
	}
	if err := <-done; !errors.Is(err, failed) {
		t.Errorf("shutdown = %v, want the serve error %q", err, failed)
	}
}
