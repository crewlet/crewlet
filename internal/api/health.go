package api

import (
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/version"
)

// The statuses a health body reports.
//
// Precedence is shutting_down > unconfigured > the posture > ok: a draining
// engine is draining first, whatever else is true of it.
const (
	StatusOK           = "ok"
	StatusUnconfigured = "unconfigured"
	StatusShuttingDown = "shutting_down"
)

// Health is what the /health endpoint and the dashboard's health push both
// carry.
//
// ONE shape backing both, so a field added here reaches the endpoint, the
// snapshot and the periodic push together — and a reconnect restores it with no
// second round trip.
//
// The engine-only fields are pointers. ABSENT and ZERO are different answers: a
// standalone API has no engine to ask, and a client that could not tell those
// apart renders a confident zero for both.
type Health struct {
	Status string `json:"status"`

	// Node names the process that answered — the field that turns "the
	// config apply failed" into "the config apply failed on node-2" once a
	// load balancer sits in front of more than one process, and the only
	// way a caller can tell which one it reached.
	Node string `json:"node"`

	// Configured is true once a company revision is active. It has been on
	// the wire since this surface existed and nothing rendered it, which
	// meant an engine with no active revision — one dropping every inbound
	// webhook — looked exactly like a healthy idle one, just with empty
	// screens.
	Configured bool `json:"configured"`

	// Engine says whether the fields below can be known at all.
	Engine  bool   `json:"engine"`
	Version string `json:"version"`

	// StartedAt is the API process's own start, deliberately separate from
	// the engine's: on the standalone deployment they are two processes on
	// two clocks.
	StartedAt string `json:"started_at"`

	Queue   string `json:"queue"`
	Clients int    `json:"clients"`

	InFlight        *int     `json:"in_flight,omitempty"`
	ShuttingDown    *bool    `json:"shutting_down,omitempty"`
	Posture         string   `json:"posture,omitempty"`
	AppliedEpoch    *int64   `json:"applied_epoch,omitempty"`
	EngineStartedAt string   `json:"engine_started_at,omitempty"`
	Seats           []string `json:"seats,omitempty"`
}

// Readiness is what /ready answers.
type Readiness struct {
	Ready      bool   `json:"ready"`
	Node       string `json:"node"`
	Configured bool   `json:"configured"`
	Draining   bool   `json:"draining"`
	Posture    string `json:"posture"`
}

// divergedPostures take a node out of rotation.
//
// shed and stuck only. WAIT and ISOLATED deliberately stay ready, and both
// exclusions are load-bearing: wait is ordinary propagation during a rollout,
// so failing readiness there would make every successful rollout a fleet-wide
// outage; isolated means NO node applied the revision, so taking this one out
// would take the fleet out over one bad revision.
var divergedPostures = map[string]struct{}{"shed": {}, "stuck": {}}

// health builds the body every health surface shares.
func (a *App) health() Health {
	configured := a.Configured()
	body := Health{
		Status:     StatusOK,
		Node:       a.nodeID,
		Configured: configured,
		Engine:     a.runtime != nil,
		Version:    version.String(),
		StartedAt:  a.startedAt,
		Queue:      a.queueBackend,
		Clients:    a.stream.Hub().Clients(),
	}
	if !configured {
		body.Status = StatusUnconfigured
	}
	if a.runtime == nil {
		return body
	}

	state := a.runtime.Snapshot()
	body.InFlight = &state.InFlight
	body.ShuttingDown = &state.ShuttingDown
	body.Posture = state.Posture
	body.AppliedEpoch = &state.AppliedEpoch
	body.EngineStartedAt = state.StartedAt
	body.Seats = state.Seats

	switch {
	case state.ShuttingDown:
		body.Status = StatusShuttingDown
	case state.Posture != "" && state.Posture != "serve" && state.Posture != "wait":
		body.Status = state.Posture
	}
	return body
}

// readiness answers whether traffic should come here.
//
// Distinct from health on purpose. Liveness answers "is this process alive" and
// must stay 200 through a drain, so an orchestrator does not SIGKILL a node
// mid-turn. Readiness answers "should traffic come here", and during a drain
// the answer is no.
//
// Also not ready before the first config revision applies: an unconfigured node
// cannot verify a webhook signature, and taking it out of rotation is how a
// fleet avoids answering with a node that would only reject the delivery.
func (a *App) readiness() (Readiness, int) {
	configured := a.Configured()
	body := Readiness{Node: a.nodeID, Configured: configured, Posture: "serve"}
	if a.runtime != nil {
		state := a.runtime.Snapshot()
		body.Draining = state.ShuttingDown
		if state.Posture != "" {
			body.Posture = state.Posture
		}
	}
	_, diverged := divergedPostures[body.Posture]
	body.Ready = configured && !body.Draining && !diverged
	if body.Ready {
		return body, http.StatusOK
	}
	return body, http.StatusServiceUnavailable
}

// streamHealth is the shared tick's view of the same facts.
func (a *App) streamHealth() stream.Health {
	full := a.health()
	return stream.Health{
		Status:       full.Status,
		InFlight:     full.InFlight,
		ShuttingDown: full.ShuttingDown,
	}
}

// nowISO stamps a time the way every other timestamp on this surface is
// spelled.
func nowISO(now time.Time) string { return now.UTC().Format(time.RFC3339Nano) }
