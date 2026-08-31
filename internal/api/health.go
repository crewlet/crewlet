package api

import (
	"context"
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

	// StallLagSeconds is how far behind this node's watched duty is,
	// present only when it is behind at all. It is the number that climbs
	// towards the seat lease TTL, at which the watchdog ends the process —
	// so an operator watching a node degrade sees it here before the
	// restart rather than only afterwards in the exit code.
	StallLagSeconds *float64 `json:"stall_lag_seconds,omitempty"`
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
func (a *App) health(ctx context.Context) Health {
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

	state := a.runtime.Snapshot(ctx)
	body.InFlight = &state.InFlight
	body.ShuttingDown = &state.ShuttingDown
	body.Posture = state.Posture
	body.AppliedEpoch = &state.AppliedEpoch
	body.EngineStartedAt = state.StartedAt
	if state.StallLag > 0 {
		// Only when there is something to say. A field that is always
		// present and always 0 trains a reader to skip it, which is the
		// one line of this body that must be read when it appears.
		lag := state.StallLag.Seconds()
		body.StallLagSeconds = &lag
	}
	body.Seats = state.Seats

	switch {
	case state.ShuttingDown:
		body.Status = StatusShuttingDown
	case state.Posture != "" && state.Posture != "serve" && state.Posture != "wait":
		body.Status = state.Posture
	}
	return body
}

// tickReadBudget bounds a read done for a push tick rather than a request.
//
// The dashboard's shared tick and its roster re-send have no request context
// to inherit, and what they call reaches the coordination plane. Five seconds
// is far longer than the read needs and far shorter than the tick's own
// cadence, so a wedged plane costs one stale push rather than a goroutine per
// tick for the life of the process.
const tickReadBudget = 5 * time.Second

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
func (a *App) readiness(ctx context.Context) (Readiness, int) {
	configured := a.Configured()
	body := Readiness{Node: a.nodeID, Configured: configured, Posture: "serve"}
	if a.runtime != nil {
		state := a.runtime.Snapshot(ctx)
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
//
// A CONTEXT OF ITS OWN, and this is one of the few places that is right: the
// push tick is a timer, not a request, so there is nothing to inherit. It is
// bounded rather than Background alone, because the posture read underneath
// reaches the coordination plane and a push tick must not outlive the
// interval that will fire the next one.
func (a *App) streamHealth() stream.Health {
	ctx, cancel := context.WithTimeout(context.Background(), tickReadBudget)
	defer cancel()
	full := a.health(ctx)
	return stream.Health{
		Status:       full.Status,
		InFlight:     full.InFlight,
		ShuttingDown: full.ShuttingDown,
	}
}

// nowISO stamps a time the way every other timestamp on this surface is
// spelled.
func nowISO(now time.Time) string { return now.UTC().Format(time.RFC3339Nano) }
