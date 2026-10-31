package runner

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
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
