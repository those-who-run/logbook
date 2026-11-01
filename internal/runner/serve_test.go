package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// `logbook run` answers the store's services on NATS while it consumes
// (paddock#122 and #123): a replayed race is listed, its lap traced, and what is
// not there is a 404 in NATS micro's error headers.
func TestRunAnswersTheServices(t *testing.T) {
	srv, js := testServer(t)
	in := start(t, testConfig(srv.ClientURL(), t.TempDir()))
	r := newRace("replay-suzuka", runA, sidA)
	publish(t, js, r.messages().msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": r.sid})

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	ask := func(subject, body string) *nats.Msg {
		t.Helper()
		m, err := nc.Request(subject, []byte(body), 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		return m
	}

	var list storev1.ListSessionsResponse
	if err := protojson.Unmarshal(ask("svc.logbook.sessions.list", `{"sources":["replay-suzuka"]}`).Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.GetSessions()) != 1 || list.GetSessions()[0].GetSessionId() != sidA || !list.GetSessions()[0].GetClosed() {
		t.Errorf("sessions: %v", list.GetSessions())
	}
	var tr storev1.GetTraceResponse
	m := ask("svc.logbook.laps.trace", `{"sources":["replay-suzuka"],"sessionId":"`+sidA+`","lap":1,"points":10}`)
	if err := protojson.Unmarshal(m.Data, &tr); err != nil || len(tr.GetTrace().GetT()) == 0 {
		t.Errorf("trace: %s, %v", m.Data, err)
	}
	m = ask("svc.logbook.sessions.get", `{"sources":["live"],"sessionId":"`+sidA+`"}`)
	if code := m.Header.Get(micro.ErrorCodeHeader); code != "404" {
		t.Errorf("another source's session: code %q, %s", code, m.Data)
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
}

// ADR 0014, decision 3: a session deleted over svc.logbook.sessions.delete stays
// deleted when its events are delivered again (without their ids, so the server
// keeps each as a message of its own, as after the duplicate window), and a
// restore brings it back.
func TestADeleteSticksThroughRedeliveredEvents(t *testing.T) {
	srv, js := testServer(t)
	in := start(t, testConfig(srv.ClientURL(), t.TempDir()))
	r := newRace("live", runA, sidA)
	msgs := r.messages().msgs
	publish(t, js, msgs, false)
	in.logs.await(t, 1, "session ended", map[string]any{"session": r.sid})

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	ask := func(subject string, out proto.Message) {
		t.Helper()
		m, err := nc.Request(subject, []byte(`{"sources":["live"],"sessionId":"`+sidA+`"}`), 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		if code := m.Header.Get(micro.ErrorCodeHeader); code != "" {
			t.Fatalf("%s: %s %s", subject, code, m.Header.Get(micro.ErrorHeader))
		}
		if err := protojson.Unmarshal(m.Data, out); err != nil {
			t.Fatalf("%s: %s, %v", subject, m.Data, err)
		}
	}
	var del storev1.DeleteSessionResponse
	ask("svc.logbook.sessions.delete", &del)
	if del.GetSession().GetDeletedAt() == nil {
		t.Fatalf("delete answered %v", &del)
	}

	var events []published
	for _, m := range msgs {
		if strings.Contains(m.subject, ".evt.") {
			events = append(events, m)
		}
	}
	publish(t, js, events, true)
	waitStreamRead(t, js, srv)

	var got storev1.GetSessionResponse
	ask("svc.logbook.sessions.get", &got)
	if !proto.Equal(got.GetSession().GetDeletedAt(), del.GetSession().GetDeletedAt()) {
		t.Errorf("deleted_at %v after the events came again, want %v", got.GetSession().GetDeletedAt(), del.GetSession().GetDeletedAt())
	}
	var list storev1.ListSessionsResponse
	ask("svc.logbook.sessions.list", &list)
	if len(list.GetSessions()) != 0 || list.GetTotal() != 0 {
		t.Errorf("the deleted session is listed: %v", list.GetSessions())
	}
	var res storev1.RestoreSessionResponse
	ask("svc.logbook.sessions.restore", &res)
	ask("svc.logbook.sessions.list", &list)
	if res.GetSession().GetDeletedAt() != nil || len(list.GetSessions()) != 1 {
		t.Errorf("after the restore: %v, listed %v", res.GetSession(), list.GetSessions())
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
}
