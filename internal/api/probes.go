package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/version"
)

// The probe surface: what a node WITHOUT the ingress role serves on api.port.
//
// # Every node can be probed, whatever its roles
//
// The API, the dashboard and the webhooks are the ingress role's, and a node
// without it serves none of them — a satellite is placed on a private host
// precisely so that it terminates no traffic. But an orchestrator runs every
// node, and one it cannot probe is one it can neither restart when it wedges
// nor wait on during a rollout: a node that has not yet joined the fleet reads
// exactly like one that has, and the next one is replaced while the first is
// still claiming nothing. So api.port on such a node carries /health and /ready
// and nothing else — plus, on a node running seats with a bridge URL, the
// agent-mode tool bridge, which was never ingress's to serve ([mountBridge]).
// api.port 0 binds nothing, here as everywhere.
//
// # The same answers, sized to the node
//
// /health is liveness and answers 200 while the process is alive, through a
// drain, exactly as the full surface's does; its body is the part of the health
// envelope that describes a node rather than a dashboard ([ProbeHealth]).
//
// /ready answers a different question from the full surface's, because this
// node takes no traffic to steer: it is ready while it is DOING ITS WORK. Its
// broker link is up, it holds its presence lease, and — where it runs seats in
// a mode that publishes — its latest placement pass was admitted to claim. And
// beneath those, everything the full surface's /ready refuses on: a drain, no
// company, a diverged posture. One judgement ([judgeReadiness]) decides both,
// in one precedence, so the shared reasons can never mean different things on
// the two.

// WorkState is what a node without the ingress role must hold to be doing its
// work, as [ProbeRuntime.Work] reads it.
type WorkState struct {
	// Broker is nil while this node reaches the fleet's broker, and names
	// what is down otherwise — see engine.Engine.BrokerLink.
	Broker error

	// Presence is whether this node holds its presence lease: whether its
	// fleet can see it at all.
	Presence bool

	// Admission is whether seat admission applies here — a node running
	// seats in a mode that publishes — and Admitted whether its latest
	// placement pass was admitted to claim. Admitted means nothing where
	// Admission is false.
	Admission bool
	Admitted  bool
}

// ProbeRuntime is what the probe surface asks the engine.
//
// A SEAM OF ITS OWN rather than [NodeRuntime], because the probe surface asks
// for less — no tool catalogue — and one thing more, [ProbeRuntime.Work], which
// the full surface's /ready deliberately never reads.
type ProbeRuntime interface {
	// Snapshot and Fleet are [NodeRuntime]'s, with its contracts.
	Snapshot(ctx context.Context) RuntimeState
	Fleet(ctx context.Context) FleetState

	// Configured reports whether a company revision is active here.
	Configured() bool

	// Work is this node's [WorkState]. It does no I/O: a probe asks it on
	// every call, and each of its facts is something the engine already
	// tracks.
	Work() WorkState
}

// ProbeHealth is what /health answers on a node without the ingress role.
//
// THE NODE'S HALF OF [Health], field for field and under the same names, so a
// script reading one reads the other. What it leaves out describes a dashboard
// this node does not serve: the connected clients, the history floors its
// reads would be paged against, and the projection it never seeds. What it
// adds is the roles, because a probe body that does not say what the node is
// leaves its reader to guess why it holds no seats.
type ProbeHealth struct {
	Status       string   `json:"status"`
	Node         string   `json:"node"`
	Roles        []string `json:"roles"`
	Configured   bool     `json:"configured"`
	Version      string   `json:"version"`
	StartedAt    string   `json:"started_at"`
	Queue        string   `json:"queue"`
	InFlight     int      `json:"in_flight"`
	ShuttingDown bool     `json:"shutting_down"`
	Posture      string   `json:"posture"`
	AppliedEpoch int64    `json:"applied_epoch"`
	Seats        []string `json:"seats"`

	// The four below are [Health]'s, omitted on the same terms.
	StallLagSeconds *float64           `json:"stall_lag_seconds,omitempty"`
	UnprovenSeconds map[string]float64 `json:"unproven_seconds,omitempty"`
	Nodes           *int               `json:"nodes,omitempty"`
	Alarms          *HealthAlarms      `json:"alarms,omitempty"`
}

// ProbeOptions builds the probe surface.
type ProbeOptions struct {
	// Bootstrap is this node's Tier A, for the guard every listener here
	// stands behind. Required.
	Bootstrap *config.Bootstrap

	// Runtime answers the probes. Required.
	Runtime ProbeRuntime

	// NodeID is the name this node's engine runs under. Required.
	NodeID string

	// Roles are this node's roles, as its presence lease advertises them.
	Roles []string

	// QueueBackend names the event queue's backend, for display.
	QueueBackend string

	// Bridge is the node's agent-mode tool bridge. Nil serves none.
	Bridge *mcpbridge.Bridge
}

// probes serves the probe surface.
type probes struct {
	runtime      ProbeRuntime
	nodeID       string
	roles        []string
	queueBackend string
}

// Probes is the HTTP handler of a node without the ingress role: /health,
// /ready and, where there is one, the tool bridge — and no other route.
//
// It is wrapped in the same guard and security headers as the full [App], so a
// request for any other path is refused or answered 404 exactly as the full
// surface would answer an unknown path, never served by a bare mux. Every
// route it serves is exempt from that guard by path, as it is there.
func Probes(opts ProbeOptions) (http.Handler, error) {
	switch {
	case opts.Bootstrap == nil:
		return nil, errors.New("api: the probe surface needs the node's Tier A")
	case opts.Runtime == nil:
		return nil, errors.New("api: the probe surface needs the engine runtime it reports on")
	case opts.NodeID == "":
		return nil, errors.New("api: the probe surface needs the node id its answers name")
	}
	p := &probes{
		runtime: opts.Runtime, nodeID: opts.NodeID,
		roles: slices.Clone(opts.Roles), queueBackend: opts.QueueBackend,
	}
	if p.roles == nil {
		p.roles = []string{}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(p.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(p.serveReady))
	mountBridge(mux, opts.Bridge)
	return pagepolicy.Apply(auth.New(opts.Bootstrap).Middleware(httpjson.Mux(mux))), nil
}

func (p *probes) serveHealth(w http.ResponseWriter, r *http.Request) {
	// ALWAYS 200 while the process is alive, through a drain, for the
	// reason [App.serveHealth] gives.
	writeJSON(w, http.StatusOK, p.health(r.Context()))
}

func (p *probes) serveReady(w http.ResponseWriter, r *http.Request) {
	work := p.runtime.Work()
	body, status := judgeReadiness(p.nodeID, p.runtime.Configured(),
		p.runtime.Snapshot(r.Context()), &work)
	writeJSON(w, status, body)
}

// health builds the probe surface's /health body, from the same reads and the
// same derivations as [App.health].
func (p *probes) health(ctx context.Context) ProbeHealth {
	configured := p.runtime.Configured()
	state, fleet := readNode(ctx, p.runtime)
	body := ProbeHealth{
		Status:       healthStatus(state, configured),
		Node:         p.nodeID,
		Roles:        p.roles,
		Configured:   configured,
		Version:      version.String(),
		StartedAt:    state.StartedAt,
		Queue:        p.queueBackend,
		InFlight:     state.InFlight,
		ShuttingDown: state.ShuttingDown,
		Posture:      state.Posture,
		AppliedEpoch: state.AppliedEpoch,
		Seats:        heldSeats(state),
	}
	body.StallLagSeconds, body.UnprovenSeconds = stallAndStranded(state)
	body.Nodes, body.Alarms = fleetCounts(fleet)
	return body
}
