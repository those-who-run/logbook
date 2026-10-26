package runner

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
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

	srv.stop()
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

// R1: the consumers are logbook's own. One deleted while it runs is created again
// with its settings, and what comes after is written.
func TestADeletedConsumerIsCreatedAgain(t *testing.T) {
	srv, js := testServer(t)
	dir := t.TempDir()
	in := start(t, testConfig(srv.ClientURL(), dir))
	addr := in.healthAddr(t)
	awaitHealth(t, addr, http.StatusOK)
	in.logs.await(t, 1, "consumers bound", nil)
	for _, c := range [][2]string{{framesStream, FramesConsumer}, {eventsStream, EventsConsumer}} {
		if err := js.DeleteConsumer(t.Context(), c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	in.logs.await(t, 2, "consumers bound", nil)
	frames, events := consumerConfigs(2 * time.Second)
	for _, want := range []jetstream.ConsumerConfig{frames, events} {
		stream := framesStream
		if want.Durable == EventsConsumer {
			stream = eventsStream
		}
		cons, err := js.Consumer(t.Context(), stream, want.Durable)
		if err != nil {
			t.Fatal(err)
		}
		got := cons.CachedInfo().Config
		if got.FilterSubject != want.FilterSubject || got.AckPolicy != want.AckPolicy ||
			got.AckWait != want.AckWait || got.MaxAckPending != want.MaxAckPending || got.DeliverPolicy != want.DeliverPolicy {
			t.Errorf("%s: %+v, want %+v", want.Durable, got, want)
		}
	}
	r := newRace("live", runA, sidA)
	publish(t, js, r.messages().msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": sidA})
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if snap := takeSnapshot(t, dir); len(snap.laps) != r.laps {
		t.Errorf("%d laps, want %d", len(snap.laps), r.laps)
	}
}
