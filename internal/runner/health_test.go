package runner

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// awaitHealth polls GET /healthz until it answers code, and returns the body.
func awaitHealth(t *testing.T, addr string, code int) healthBody {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, body := getHealth(t, addr)
		if got == code {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /healthz still %d after 30 s, want %d: %+v", got, code, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AC5: /healthz answers 503 before the server is up, 200 once both consumers are
// bound, and 503 again when the server goes.
func TestHealthzFollowsTheServerAndTheConsumers(t *testing.T) {
	port := freePort(t)
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	in := start(t, testConfig(url, t.TempDir()))
	addr := in.healthAddr(t)

	code, body := getHealth(t, addr)
	if code != http.StatusServiceUnavailable || body.Connected || body.Consumers[FramesConsumer] ||
		body.Consumers[EventsConsumer] || body.Status != "unavailable" {
		t.Errorf("before the server: %d %+v, want 503, not connected, no consumers", code, body)
	}

	srv := startNATS(t, port)
	// Up, but without the streams the consumers need: still unavailable.
	time.Sleep(300 * time.Millisecond)
	if code, body := getHealth(t, addr); code != http.StatusServiceUnavailable {
		t.Errorf("with no streams: %d %+v, want 503", code, body)
	}
	createStreams(t, connect(t, srv.ClientURL()))
	body = awaitHealth(t, addr, http.StatusOK)
	if !body.Connected || !body.Consumers[FramesConsumer] || !body.Consumers[EventsConsumer] ||
		body.Status != "ok" || body.Version != "test" || body.Laps != 0 || body.LastWrite != nil {
		t.Errorf("once bound: %+v", body)
	}

	srv.Shutdown()
	srv.WaitForShutdown()
	body = awaitHealth(t, addr, http.StatusServiceUnavailable)
	if body.Connected {
		t.Errorf("after the server went: %+v", body)
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
}

// /healthz counts the laps written and says when the last was.
func TestHealthzCountsTheLapsWritten(t *testing.T) {
	srv, js := testServer(t)
	in := start(t, testConfig(srv.ClientURL(), t.TempDir()))
	addr := in.healthAddr(t)
	r := newRace("live", runA, sidA)
	before := time.Now()
	publish(t, js, r.messages().msgs, false)
	in.logs.await(t, 1, "session ended", nil)
	code, body := getHealth(t, addr)
	if code != http.StatusOK || body.Laps != int64(r.laps) || body.LastWrite == nil || body.LastWrite.Before(before.Add(-time.Second)) {
		t.Errorf("after the race: %d %+v, want 200 with %d laps", code, body, r.laps)
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
}

// Only GET.
func TestHealthzIsGetOnly(t *testing.T) {
	in := start(t, testConfig("nats://127.0.0.1:1", t.TempDir()))
	addr := in.healthAddr(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: %d, want 405", resp.StatusCode)
	}
}
