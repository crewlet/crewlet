package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// Restarter asks the fleet to restart an MCP server now: every instance of it,
// or with a seat (a handle) that seat's instance on the node holding the seat.
// It records the request and returns; each node restarts what it runs as it
// hears it. Adapted from the engine where the API is wired, with this
// package's own refusals ([ErrNoSuchServer] and its neighbours).
type Restarter interface {
	RestartServer(ctx context.Context, name, seat string, by iam.Actor) (Restart, error)
}

// Restart is what a restart request recorded.
type Restart struct {
	// Server is the server, and Seat the seat's handle for one seat's
	// instance ("" for every instance).
	Server string
	Seat   string
	// At is the request's stamp: every node restarts what began before it.
	At time.Time
	// Node is the node that took the request.
	Node string
}

// The refusals of a restart, each answered with what the person can change.
var (
	// ErrNoSuchServer is a server the active configuration does not declare.
	ErrNoSuchServer = errors.New("mcpapi: no such MCP server")
	// ErrServerOff is a server the configuration turns off.
	ErrServerOff = errors.New("mcpapi: the MCP server is turned off")
	// ErrSeatOnShared is a seat named for a shared server.
	ErrSeatOnShared = errors.New("mcpapi: a shared MCP server runs one instance for the company")
	// ErrNoSuchSeat is a seat the organization does not have.
	ErrNoSuchSeat = errors.New("mcpapi: no such agent seat")
	// ErrSeatNotGranted is a seat the server is not granted to.
	ErrSeatNotGranted = errors.New("mcpapi: the seat is not granted the MCP server")
)

// restartRequest is the body `POST /mcp-servers/{name}/retry` reads: empty for
// every instance, or the seat whose instance to restart.
type restartRequest struct {
	Seat string `json:"seat"`
}

// RestartAnswer is what `POST /mcp-servers/{name}/retry` answers: the request
// as recorded. The restart itself happens on each node as it hears it, so a
// reader watches the server's detail for instances whose `since` is after
// `at`.
type RestartAnswer struct {
	Server string    `json:"server"`
	Seat   string    `json:"seat"`
	At     time.Time `json:"at"`
	Node   string    `json:"node"`
}

// maxRestartBody bounds the body: one seat's handle.
const maxRestartBody = 4 << 10

// serveRestart asks every node, or the node holding one seat, to restart the
// server its path names: 202 with the request as recorded.
func (s *Service) serveRestart(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	var req restartRequest
	raw, err := httpjson.ReadBody(w, r, maxRestartBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&req); decodeErr != nil {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				httpjson.Detail{"detail": `the body is {"seat": "<handle>"}, or empty to restart every instance`})
			return
		}
	}
	rec.Seat = req.Seat
	name := r.PathValue("name")
	done, err := s.restarter.RestartServer(r.Context(), name, req.Seat, auth.AttributionOf(r.Context()))
	switch {
	case err == nil:
		httpjson.Write(w, http.StatusAccepted, RestartAnswer(done))
	case errors.Is(err, ErrNoSuchServer):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "the active configuration declares no MCP server " + quoted(name)})
	case errors.Is(err, ErrServerOff):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "MCP server " + quoted(name) + " is turned off, so nothing runs it: " +
				"turn it on (enabled: true) and it starts"})
	case errors.Is(err, ErrSeatOnShared):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "MCP server " + quoted(name) + " is shared: it runs one instance " +
				"for the company, not one per seat, so restart it without naming a seat"})
	case errors.Is(err, ErrNoSuchSeat):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNoSuchSeat,
			httpjson.Detail{"detail": "the organization has no agent seat " + quoted(req.Seat)})
	case errors.Is(err, ErrSeatNotGranted):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "seat " + quoted(req.Seat) + " is not granted MCP server " +
				quoted(name) + ", so it runs no instance of it"})
	default:
		log.WarnContext(r.Context(), "mcp_server_restart_failed", "server", name, "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node could not record the restart for the fleet; ask again"})
	}
}

// quoted is s in double quotes, for a refusal's sentence.
func quoted(s string) string { return `"` + s + `"` }

// audited wraps a gesture's handler so its call is recorded as one
// `mcp_server_acted`, whatever became of it: the outcome and the refusal are
// read off the answer it wrote, so no gesture can forget to record either.
func (s *Service) audited(action types.MCPServerAction,
	h func(http.ResponseWriter, *http.Request, *types.MCPServerActed)) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		rec := s.record(r, action)
		watched := operator.RecordAnswer(w)
		h(watched, r, &rec)
		rec.Outcome, rec.Refusal = watched.Outcome()
		operator.Audit(r.Context(), s.audit, types.NewMCPServerActed(rec))
	}
}

// refusedAudited renders a refusal the guard made before the handler ran, in
// the router's own envelope, and records it as action's `mcp_server_acted`:
// refused for a 403, failed for a node that could not decide.
func (s *Service) refusedAudited(action types.MCPServerAction) authz.Refusal {
	return func(w http.ResponseWriter, r *http.Request, p authz.Policy, d authz.Decision) {
		rec := s.record(r, action)
		watched := operator.RecordAnswer(w)
		authz.EnvelopeRefusal(watched, r, p, d)
		rec.Outcome, rec.Refusal = watched.Outcome()
		operator.Audit(r.Context(), s.audit, types.NewMCPServerActed(rec))
	}
}

// record is the audit record of one call, before its outcome: who made it,
// which gesture, on which server.
func (s *Service) record(r *http.Request, action types.MCPServerAction) types.MCPServerActed {
	by := auth.AttributionOf(r.Context())
	return types.MCPServerActed{ActorName: by.Name, ActorKind: string(by.Kind), OperatorID: by.OperatorID,
		Action: action, Server: r.PathValue("name")}
}
