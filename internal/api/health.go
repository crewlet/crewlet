package api

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/version"
)

// The statuses a health body reports.
//
// Precedence is shutting_down > a posture other than serve or wait >
// unconfigured > ok. A draining engine is draining first, whatever else is true
// of it. The posture outranks unconfigured because it names the CAUSE: a node
// stuck applying its first revision is unconfigured because it is stuck, and
// `configured: false` still says the rest beside it.
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
// Every field the engine answers is ALWAYS PRESENT, and a zero here is a real
// zero: every process that serves the API runs the engine beside it, so there is
// no answer this body could honestly leave out.
type Health struct {
	Status string `json:"status"`

	// Node names the process that answered — the field that turns "the
	// config apply failed" into "the config apply failed on node-2" once a
	// load balancer sits in front of more than one process, and the only
	// way a caller can tell which one it reached.
	Node string `json:"node"`

	// Configured is true once a company revision is active. It has been on
	// the wire since this surface existed and nothing rendered it, which
	// meant an engine with no active revision — one refusing every inbound
	// webhook with a 503 the sender retries, never dropping one — looked
	// exactly like a healthy idle one, just with empty screens.
	Configured bool `json:"configured"`

	Version string `json:"version"`

	// StartedAt is when this node's engine was built, which is the node's
	// start: the API runs inside the engine's process. ONE start rather
	// than one per surface, and the same instant the fleet view reports
	// for this node off its presence heartbeat.
	StartedAt string `json:"started_at"`

	Queue   string `json:"queue"`
	Clients int    `json:"clients"`

	// EventHistorySeconds is how far back the event log can be read, which
	// is the hard bottom of paging: once a cursor crosses it every page is
	// empty forever. The dashboard has to be able to SAY that floor, and
	// three of its screens restated it as literal copy ("the store keeps 30
	// days") because nothing on the wire carried it. Seconds rather than
	// days, because the retention is a duration and a client that has to
	// re-derive the unit is a second place the number can be wrong.
	EventHistorySeconds int `json:"event_history_seconds"`

	InFlight     int      `json:"in_flight"`
	ShuttingDown bool     `json:"shutting_down"`
	Posture      string   `json:"posture"`
	AppliedEpoch int64    `json:"applied_epoch"`
	Seats        []string `json:"seats"`

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

	// Reason names what took this node out of rotation, and is absent while
	// it is in rotation: [ReasonDraining], [ReasonUnconfigured], or the
	// diverged posture itself. ONE FIELD, decided here in the precedence the
	// health status uses, so a load balancer's record of a failed probe says
	// why without its reader re-deriving it from the three fields above.
	Reason string `json:"reason,omitempty"`
}

// The reasons a refused /ready names, beside the diverged postures, which are
// named as themselves.
//
// ReasonDraining is the drain gate's own code, spelled once: a node that
// refuses a webhook because it is draining and a probe that reports it out of
// rotation are describing one fact.
const (
	ReasonDraining     = string(httpjson.CodeDraining)
	ReasonUnconfigured = StatusUnconfigured
)

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
	state := a.runtime.Snapshot(ctx)
	seats := state.Seats
	if seats == nil {
		// A node holding no seats holds an empty list, and says so as one:
		// a null here would read as "cannot say", which this node can.
		seats = []string{}
	}
	body := Health{
		Status:       StatusOK,
		Node:         a.nodeID,
		Configured:   configured,
		Version:      version.String(),
		StartedAt:    state.StartedAt,
		Queue:        a.queueBackend,
		Clients:      a.stream.Hub().Clients(),
		InFlight:     state.InFlight,
		ShuttingDown: state.ShuttingDown,
		Posture:      state.Posture,
		AppliedEpoch: state.AppliedEpoch,
		Seats:        seats,
		// The floor is the store's own, not a number this package picked:
		// it is what every read is bounded by.
		EventHistorySeconds: int(store.EventHistory.Seconds()),
	}
	if state.StallLag > 0 {
		// Only when there is something to say. A field that is always
		// present and always 0 trains a reader to skip it, which is the
		// one line of this body that must be read when it appears.
		lag := state.StallLag.Seconds()
		body.StallLagSeconds = &lag
	}

	// IN THE PRECEDENCE THE STATUSES DECLARE. The posture case used to be
	// reached whether or not the node was configured, so a node with no
	// revision that had also concluded `shed` reported the posture, and the
	// one fact that matters on such a node, that it refuses every
	// delivery, was the one its status did not say.
	switch {
	case state.ShuttingDown:
		body.Status = StatusShuttingDown
	case !configured:
		body.Status = StatusUnconfigured
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
	state := a.runtime.Snapshot(ctx)
	body := Readiness{
		Node: a.nodeID, Configured: configured,
		Draining: state.ShuttingDown, Posture: state.Posture,
	}
	_, diverged := divergedPostures[body.Posture]
	switch {
	case body.Draining:
		body.Reason = ReasonDraining
	case !configured:
		body.Reason = ReasonUnconfigured
	case diverged:
		body.Reason = body.Posture
	}
	body.Ready = body.Reason == ""
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
