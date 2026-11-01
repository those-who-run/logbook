// Package serve is logbook's services: the store's reads, and the pit wall's two
// writes, deleting a session and restoring it (paddock ADR 0014), as NATS micro
// endpoints on the svc.logbook.* subjects contracts' subjects.yaml lists
// (paddock#122 and #123, ADR 0003 point 1). Each request and reply is a
// twr.store.v1 message in protobuf's JSON form; an error is NATS micro's error
// headers, 400 for a bad request, 404 for what the sources asked for do not
// have, 409 for a write the session's state refuses (deleting an open session),
// 500 for logbook's own failure.
package serve

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/those-who-run/logbook/internal/store"
)

// Store is what the services answer from: *store.Store's reads and writes.
type Store interface {
	ListSessions(ctx context.Context, req *storev1.ListSessionsRequest) (*storev1.ListSessionsResponse, error)
	GetSession(ctx context.Context, req *storev1.GetSessionRequest) (*storev1.GetSessionResponse, error)
	ListLaps(ctx context.Context, req *storev1.ListLapsRequest) (*storev1.ListLapsResponse, error)
	GetTrace(ctx context.Context, req *storev1.GetTraceRequest) (*storev1.GetTraceResponse, error)
	ListSameTrack(ctx context.Context, req *storev1.ListSameTrackRequest) (*storev1.ListSameTrackResponse, error)
	DeleteSession(ctx context.Context, req *storev1.DeleteSessionRequest) (*storev1.DeleteSessionResponse, error)
	RestoreSession(ctx context.Context, req *storev1.RestoreSessionRequest) (*storev1.RestoreSessionResponse, error)
}

// Endpoint is one service: its subject, its name among the service's endpoints,
// and the messages it takes and answers, which its metadata carries as
// subjects.yaml lists them.
type Endpoint struct {
	Name, Subject, Request, Response string
	handle                           func(Store, *slog.Logger) micro.HandlerFunc
}

// Endpoints are the services, as contracts' subjects.yaml lists them.
var Endpoints = []Endpoint{
	endpoint("sessions-list", "svc.logbook.sessions.list", Store.ListSessions),
	endpoint("sessions-get", "svc.logbook.sessions.get", Store.GetSession),
	endpoint("laps-list", "svc.logbook.laps.list", Store.ListLaps),
	endpoint("laps-trace", "svc.logbook.laps.trace", Store.GetTrace),
	endpoint("sessions-same-track", "svc.logbook.sessions.same_track", Store.ListSameTrack),
	endpoint("sessions-delete", "svc.logbook.sessions.delete", Store.DeleteSession),
	endpoint("sessions-restore", "svc.logbook.sessions.restore", Store.RestoreSession),
}

// timeout bounds one request's read or write.
const timeout = 10 * time.Second

// Start adds the logbook service to nc with its endpoints, answering from st. The
// caller stops it before closing the store.
func Start(nc *nats.Conn, st Store, version string, logger *slog.Logger) (micro.Service, error) {
	svc, err := micro.AddService(nc, micro.Config{
		Name:        "logbook",
		Version:     semver(version),
		Description: "logbook's store: sessions, laps and lap traces, and deleting and restoring a session (twr.store.v1)",
	})
	if err != nil {
		return nil, err
	}
	for _, e := range Endpoints {
		err := svc.AddEndpoint(e.Name, e.handle(st, logger),
			micro.WithEndpointSubject(e.Subject),
			micro.WithEndpointMetadata(map[string]string{"request": e.Request, "response": e.Response}))
		if err != nil {
			return nil, errors.Join(err, svc.Stop())
		}
	}
	return svc, nil
}

// semverish is the semantic version NATS micro takes (micro's own check).
var semverish = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// semver is version as NATS micro takes it, which must be a semantic version: a
// release's own, as "0.1.6", and a build's that is none, such as "dev", as a
// pre-release of 0.0.0, "0.0.0-dev"; one that still is not, "0.0.0-dev".
func semver(version string) string {
	for _, v := range []string{version, "0.0.0-" + regexp.MustCompile(`[^0-9A-Za-z-]+`).ReplaceAllString(version, "-")} {
		if semverish.MatchString(v) {
			return v
		}
	}
	return "0.0.0-dev"
}

// endpoint makes an Endpoint of one of Store's reads or writes.
func endpoint[Req, Resp proto.Message](name, subject string, read func(Store, context.Context, Req) (Resp, error)) Endpoint {
	var req Req
	var resp Resp
	return Endpoint{
		Name: name, Subject: subject,
		Request:  string(req.ProtoReflect().Descriptor().FullName()),
		Response: string(resp.ProtoReflect().Descriptor().FullName()),
		handle: func(st Store, logger *slog.Logger) micro.HandlerFunc {
			return func(r micro.Request) {
				in := req.ProtoReflect().New().Interface().(Req) //nolint:forcetypeassert // New of a Req is a Req
				if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(r.Data(), in); err != nil {
					respondError(r, logger, subject, "400", "the request is not a "+string(in.ProtoReflect().Descriptor().FullName())+": "+err.Error())
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				out, err := read(st, ctx, in)
				switch {
				case errors.Is(err, store.ErrBadRequest):
					respondError(r, logger, subject, "400", err.Error())
				case errors.Is(err, store.ErrNotFound):
					respondError(r, logger, subject, "404", err.Error())
				case errors.Is(err, store.ErrConflict):
					respondError(r, logger, subject, "409", err.Error())
				case err != nil:
					logger.Error("service request failed", "subject", subject, "err", err)
					respondError(r, logger, subject, "500", err.Error())
				default:
					respond(r, logger, subject, out)
				}
			}
		},
	}
}

// respond sends m, or a 500 when it cannot be sent: a reply the server refuses,
// such as one over its payload limit, would leave the caller waiting out its
// timeout.
func respond(r micro.Request, logger *slog.Logger, subject string, m proto.Message) {
	b, err := protojson.Marshal(m)
	if err == nil {
		err = r.Respond(b)
	}
	if err != nil {
		logger.Error("service reply not sent", "subject", subject, "bytes", len(b), "err", err)
		respondError(r, logger, subject, "500", "the reply could not be sent: "+err.Error())
	}
}

func respondError(r micro.Request, logger *slog.Logger, subject, code, description string) {
	if err := r.Error(code, description, nil); err != nil {
		logger.Warn("service error not sent", "subject", subject, "code", code, "err", err)
	}
}
