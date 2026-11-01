package serve

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/those-who-run/logbook/internal/store"
)

// request is a micro.Request a test makes: its data, and what the handler
// answered.
type request struct {
	data       []byte
	answer     []byte
	code, desc string
	refuse     error // what Respond returns, as a server over its payload limit would
}

func (r *request) Respond(b []byte, _ ...micro.RespondOpt) error {
	if r.refuse != nil {
		return r.refuse
	}
	r.answer = b
	return nil
}
func (r *request) RespondJSON(any, ...micro.RespondOpt) error { return errors.New("not used") }
func (r *request) Error(code, desc string, _ []byte, _ ...micro.RespondOpt) error {
	r.code, r.desc = code, desc
	return nil
}
func (r *request) Data() []byte           { return r.data }
func (r *request) Headers() micro.Headers { return nil }
func (r *request) Subject() string        { return "" }
func (r *request) Reply() string          { return "" }

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func call(t *testing.T, st Store, subject, body string) *request {
	t.Helper()
	for _, e := range Endpoints {
		if e.Subject == subject {
			r := &request{data: []byte(body)}
			e.handle(st, quiet()).Handle(r)
			return r
		}
	}
	t.Fatalf("no endpoint on %s", subject)
	return nil
}

// The endpoints are the services contracts' subjects.yaml lists, each with its
// request and response.
func TestTheEndpointsAreSubjectsYAMLs(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/those-who-run/contracts").Output()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(strings.TrimSpace(string(out)), "subjects.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// subjects.yaml's services, read line by line: a list of mappings under
	// `services:`, each starting `- subject:`.
	want := map[string][2]string{}
	in, subject := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, " ") && line != "" {
			in = line == "services:"
			continue
		}
		key, value, ok := strings.Cut(strings.TrimLeft(strings.TrimSpace(line), "- "), ": ")
		if !in || !ok {
			continue
		}
		switch key {
		case "subject":
			subject = value
		case "request":
			want[subject] = [2]string{value, want[subject][1]}
		case "response":
			want[subject] = [2]string{want[subject][0], value}
		}
	}
	if len(want) == 0 {
		t.Fatal("no services in subjects.yaml")
	}
	got := map[string][2]string{}
	for _, e := range Endpoints {
		got[e.Subject] = [2]string{e.Request, e.Response}
	}
	if len(got) != len(want) {
		t.Errorf("endpoints %v, want subjects.yaml's %v", got, want)
	}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("%s: %v, want %v", s, got[s], w)
		}
	}
}

// Each answer is protobuf JSON; a bad request is 400, what is not there 404.
func TestTheHandlersAnswerAndRefuse(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	r := call(t, st, "svc.logbook.sessions.list", `{"sources":["live"]}`)
	var list storev1.ListSessionsResponse
	if err := protojson.Unmarshal(r.answer, &list); err != nil || r.code != "" || list.GetDataVersion() == "" {
		t.Errorf("list: %s %s %s, %v", r.answer, r.code, r.desc, err)
	}
	for _, c := range []struct{ subject, body, code string }{
		{"svc.logbook.sessions.list", `{}`, "400"},
		{"svc.logbook.sessions.list", `not json`, "400"},
		{"svc.logbook.sessions.list", `{"sources":["live"],"limit":501}`, "400"},
		{"svc.logbook.sessions.get", `{"sources":["live"],"sessionId":"x"}`, "404"},
		{"svc.logbook.laps.list", `{"sources":["live"],"sessionId":"x"}`, "404"},
		{"svc.logbook.laps.trace", `{"sources":["live"],"sessionId":"x","lap":1}`, "404"},
		{"svc.logbook.sessions.same_track", `{"sources":["live"],"sessionId":"x"}`, "404"},
		{"svc.logbook.sessions.delete", `{"sources":["live"],"sessionId":"x"}`, "404"},
		{"svc.logbook.sessions.restore", `{"sources":["live"],"sessionId":"x"}`, "404"},
		{"svc.logbook.sessions.delete", `{"sessionId":"x"}`, "400"},
		{"svc.logbook.sessions.restore", `not json`, "400"},
	} {
		r := call(t, st, c.subject, c.body)
		if r.code != c.code || r.answer != nil || r.desc == "" {
			t.Errorf("%s %s: %q %q, answer %s; want %s", c.subject, c.body, r.code, r.desc, r.answer, c.code)
		}
	}
	// A field from a newer contract is ignored, not refused.
	if r := call(t, st, "svc.logbook.sessions.list", `{"sources":["live"],"later":1}`); r.code != "" {
		t.Errorf("an unknown field: %s %s", r.code, r.desc)
	}
}

// ADR 0014: a delete answers the session with deleted_at set, a restore with
// it cleared, and deleting an open session is 409.
func TestDeleteAndRestoreAnswer(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const id = "01JB8Z3K5Q9T2M7XW4R6N1PDHE"
	started := &eventsv1.SessionStarted{SessionId: id, Source: store.SourceLive, StartedAt: timestamppb.Now()}
	if err := st.StartSession(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	body := `{"sources":["live"],"sessionId":"` + id + `"}`
	if r := call(t, st, "svc.logbook.sessions.delete", body); r.code != "409" || r.answer != nil {
		t.Errorf("deleting an open session: %q %q, answer %s; want 409", r.code, r.desc, r.answer)
	}
	ended := &eventsv1.SessionEnded{SessionId: id, Summary: &eventsv1.SessionSummary{}}
	if err := st.EndSession(t.Context(), store.SourceLive, ended, time.Now()); err != nil {
		t.Fatal(err)
	}
	var del storev1.DeleteSessionResponse
	r := call(t, st, "svc.logbook.sessions.delete", body)
	if err := protojson.Unmarshal(r.answer, &del); err != nil || r.code != "" || del.GetSession().GetDeletedAt() == nil ||
		del.GetDataVersion() == "" {
		t.Errorf("delete: %s %s %s, %v", r.answer, r.code, r.desc, err)
	}
	var res storev1.RestoreSessionResponse
	r = call(t, st, "svc.logbook.sessions.restore", body)
	if err := protojson.Unmarshal(r.answer, &res); err != nil || r.code != "" || res.GetSession().GetDeletedAt() != nil ||
		res.GetDataVersion() == del.GetDataVersion() {
		t.Errorf("restore: %s %s %s, %v", r.answer, r.code, r.desc, err)
	}
}

// failing is a store whose reads fail.
type failing struct{ Store }

func (failing) ListSessions(context.Context, *storev1.ListSessionsRequest) (*storev1.ListSessionsResponse, error) {
	return nil, errors.New("disk on fire")
}

// logbook's own failure is 500, and logged.
func TestAFailureIs500(t *testing.T) {
	var logs bytes.Buffer
	r := &request{data: []byte(`{"sources":["live"]}`)}
	Endpoints[0].handle(failing{}, slog.New(slog.NewTextHandler(&logs, nil))).Handle(r)
	if r.code != "500" || !strings.Contains(logs.String(), "disk on fire") {
		t.Errorf("code %q, logs %s", r.code, logs.String())
	}
}

// NATS micro takes only a semantic version: a build that has none is a
// pre-release of 0.0.0.
func TestTheServiceVersionIsSemver(t *testing.T) {
	for in, want := range map[string]string{"0.1.6": "0.1.6", "dev": "0.0.0-dev", "test run/1": "0.0.0-test-run-1", "": "0.0.0-dev", "0.1": "0.0.0-0-1"} {
		if got := semver(in); got != want {
			t.Errorf("semver(%q) = %q, want %q", in, got, want)
		}
	}
}

// A reply the server refuses (nats.ErrMaxPayload) is answered 500, so the
// caller is not left to time out.
func TestARefusedReplyIs500(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &request{data: []byte(`{"sources":["live"]}`), refuse: nats.ErrMaxPayload}
	Endpoints[0].handle(st, quiet()).Handle(r)
	if r.code != "500" || !strings.Contains(r.desc, "maximum payload") {
		t.Errorf("code %q, %q", r.code, r.desc)
	}
}

// Start adds the service and its endpoints to a connection, here one still
// trying to reach its server, as logbook's is while NATS is down; Stop takes
// them off.
func TestStartAndStop(t *testing.T) {
	nc, err := nats.Connect("nats://127.0.0.1:1", nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	svc, err := Start(nc, failing{}, "dev", quiet())
	if err != nil {
		t.Fatal(err)
	}
	info := svc.Info()
	if info.Name != "logbook" || info.Version != "0.0.0-dev" || len(info.Endpoints) != len(Endpoints) {
		t.Errorf("info %+v", info)
	}
	for _, e := range info.Endpoints {
		if e.Metadata["request"] == "" || e.Metadata["response"] == "" {
			t.Errorf("%s: metadata %v", e.Subject, e.Metadata)
		}
	}
	if err := svc.Stop(); err != nil {
		t.Error(err)
	}
}

// An error that cannot be sent is logged.
func TestAnUnsentErrorIsLogged(t *testing.T) {
	var logs bytes.Buffer
	r := &erring{request{data: []byte(`{}`)}}
	Endpoints[0].handle(failing{}, slog.New(slog.NewTextHandler(&logs, nil))).Handle(r)
	if !strings.Contains(logs.String(), "service error not sent") {
		t.Errorf("logs %s", logs.String())
	}
}

type erring struct{ request }

func (*erring) Error(string, string, []byte, ...micro.RespondOpt) error {
	return nats.ErrConnectionClosed
}
