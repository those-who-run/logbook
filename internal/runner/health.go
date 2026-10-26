package runner

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// health is what GET /healthz reports. The runner writes it; the handler reads it
// from the server's goroutines.
type health struct {
	version        string
	nc             atomic.Pointer[nats.Conn]
	frames, events atomic.Bool  // each consumer bound
	laps           atomic.Int64 // laps written since start
	lastWrite      atomic.Int64 // the last lap's write, Unix nanoseconds; 0 for none
}

// healthBody is GET /healthz's JSON.
type healthBody struct {
	Status    string          `json:"status"`
	Version   string          `json:"version"`
	Connected bool            `json:"connected"`
	Consumers map[string]bool `json:"consumers"`
	Laps      int64           `json:"laps_written"`
	LastWrite *time.Time      `json:"last_write"`
}

func (h *health) wrote(at time.Time) {
	h.laps.Add(1)
	h.lastWrite.Store(at.UnixNano())
}

// handler serves GET /healthz: 200 while connected with both consumers bound, 503
// otherwise, with the same JSON either way.
func (h *health) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		nc := h.nc.Load()
		body := healthBody{
			Version:   h.version,
			Connected: nc != nil && nc.IsConnected(),
			Consumers: map[string]bool{FramesConsumer: h.frames.Load(), EventsConsumer: h.events.Load()},
			Laps:      h.laps.Load(),
		}
		if ns := h.lastWrite.Load(); ns != 0 {
			at := time.Unix(0, ns).UTC()
			body.LastWrite = &at
		}
		code := http.StatusOK
		body.Status = "ok"
		if !body.Connected || !body.Consumers[FramesConsumer] || !body.Consumers[EventsConsumer] {
			code = http.StatusServiceUnavailable
			body.Status = "unavailable"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		// An error here is the client gone; there is no one left to tell.
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}
