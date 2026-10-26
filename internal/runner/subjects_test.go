package runner

import (
	"testing"

	eventsv1 "github.com/those-who-run/contracts/gen/go/twr/events/v1"
)

func TestSubjectsNameTheirSource(t *testing.T) {
	for _, c := range []struct {
		subject string
		frame   bool
		src     string
		ok      bool
	}{
		{"twr.v1.live.frame.gt7", true, "live", true},
		{"twr.v1.replay-x.frame.gt7", true, "replay-x", true},
		{"twr.v1.live.frame.gt7.lo", true, "", false},
		{"twr.v2.live.frame.gt7", true, "", false},
		{"twr.v1.live.evt.01JB8Z3K5Q9T2M7XW4R6N1PDHE.lap.completed", false, "live", true},
		{"twr.v1.parity.evt._.session.started", false, "parity", true},
		{"twr.v1.live.cmd.x", false, "", false},
		{"other", false, "", false},
	} {
		parse := eventSource
		if c.frame {
			parse = frameSource
		}
		if src, ok := parse(c.subject); src != c.src || ok != c.ok {
			t.Errorf("%s: %q, %v; want %q, %v", c.subject, src, ok, c.src, c.ok)
		}
	}
}

func TestRunOfAFrameID(t *testing.T) {
	if got := runOf(runA + ":frame:42"); got != runA {
		t.Errorf("runOf = %q", got)
	}
	if got := runOf("no-run-here"); got != "no-run-here" {
		t.Errorf("runOf without :frame: = %q", got)
	}
}

// The log names the server without the user and password its URL may carry.
func TestRedactLeavesOutTheCredentials(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // made-up credentials, which the test checks are left out
		"nats://user:secret@nats.example:4222": "nats://nats.example:4222",
		"nats://127.0.0.1:4222":                "nats://127.0.0.1:4222",
		"user:secret@nats.example":             "(the configured server)",
		"nats://[::1":                          "(the configured server)",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOnlyAMessageThatTravelsHasAType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mustType of a SessionSummary did not panic")
		}
	}()
	mustType(&eventsv1.SessionSummary{})
}
