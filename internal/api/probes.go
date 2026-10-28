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
	"github.com/crewlet/crewlet/internal/sandbox"
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
// to the network, and nothing else. api.port 0 binds nothing, here as
// everywhere.
//
// # And the two routes its own boxes dial, to this host alone
//
// A node that runs seats launches executor boxes, and a `direct` one is a
// process of this host: it reaches the seat's tools over the tool bridge and
// exports its telemetry to the receiver, and the address it is handed for both
// is this listener on loopback (cmd/crewlet's boxHostURL). So this surface
// serves those two routes too — the bridge under its prefix (internal/api/
// mcpbridge, its notes route included) and the receiver under `/otlp/` — to a
// caller on THIS HOST'S LOOPBACK ALONE ([mcpbridge.FromLoopback], read from the
// connection, never a header), and answers anybody else as their absence
// would: the probes are still all the network reaches here.
//
// The obvious alternative was the one in place: hand such a node's boxes the
// fleet's address (CREWLET_MCP_BRIDGE_URL) and let an ingress node relay each
// call back over the broker. That address is the one thing a seat pool may
// not have — a satellite on a private host, a Kubernetes seat pool with no
// route in — and without it every executor launch on the node was refused.
// The loopback address needs none, and is served by the same bridge, so a
// call for a seat this node has since handed on is relayed to its holder as
// on any ingress node.
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

	// ConfigAttempt is [Health]'s, absent on the same terms.
	ConfigAttempt *ConfigAttempt `json:"config_attempt,omitempty"`

	// IdentityDutySeconds is [Health]'s, always an object on the same
	// terms: a node without the ingress role may still hold the workers
	// role that arms the identity estate's duties, and this is where an
	// operator reads that it did.
	IdentityDutySeconds map[string]float64 `json:"identity_duty_seconds"`

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

	// Bridge and OtelReceiver are the two routes a box on this node's own
	// host dials — the tool bridge and the telemetry receiver — served to
	// this host's loopback alone. Nil serves that route to nobody, and is
	// what a node that runs no seats, and so launches no box, is given.
	Bridge       *mcpbridge.Bridge
	OtelReceiver *sandbox.OtelReceiver
}

// probes serves the probe surface.
type probes struct {
	runtime      ProbeRuntime
	nodeID       string
	roles        []string
	queueBackend string
}

// Probes is the HTTP handler of a node without the ingress role: /health and
// /ready, and, to this host's loopback alone, the routes its own boxes dial
// ([ProbeOptions.Bridge], [ProbeOptions.OtelReceiver]).
//
// It is wrapped in the same guard and security headers as the full [App], so a
// request for any other path is refused or answered 404 exactly as the full
// surface would answer an unknown path, never served by a bare mux. Every
// route it serves is exempt from that guard by path, as it is there: the
// boxes' routes authenticate by the signed token in their own path.
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
	if opts.Bridge != nil || opts.OtelReceiver != nil {
		// THE BOXES' ROUTES ON A MUX OF THEIR OWN, behind the host check as
		// a whole: a caller off this host meets the absence of everything
		// under both prefixes, whatever its method or path — never the
		// inner mux's 405 for a route that exists, which would tell the
		// network what is mounted here.
		own := http.NewServeMux()
		mountBridge(own, opts.Bridge)
		mountOTLP(own, opts.OtelReceiver)
		boxes := thisHostAlone(httpjson.Mux(own))
		mux.Handle(mcpbridge.PathPrefix, boxes)
		mux.Handle(auth.OTLPPrefix, boxes)
	}
	// NO CSRF GATE HERE: the probes are reads, and the boxes' routes are a
	// server's calls, which the full surface's check does not judge either
	// ([auth.Public]), so there is no cross-site request for a check to
	// refuse.
	return pagepolicy.Apply(auth.New(opts.Bootstrap).Middleware(httpjson.Mux(mux)),
		servedOverHTTPS(opts.Bootstrap, false)), nil
}

// thisHostAlone serves next to a caller on this host's loopback, and answers
// any other as the route's absence ([httpjson.NoRoute]): the routes a node's
// own boxes dial, on the listener of a node that publishes nothing but its
// probes. See the notes at the top of this file.
//
// ABSENT RATHER THAN FORBIDDEN, because a 403 tells whoever scans the port
// that something is mounted behind it, and to the network this listener
// serves the probes and nothing else.
func thisHostAlone(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mcpbridge.FromLoopback(r) {
			httpjson.NoRoute(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		Status:        healthStatus(state, configured),
		Node:          p.nodeID,
		Roles:         p.roles,
		Configured:    configured,
		Version:       version.String(),
		StartedAt:     state.StartedAt,
		Queue:         p.queueBackend,
		InFlight:      state.InFlight,
		ShuttingDown:  state.ShuttingDown,
		Posture:       state.Posture,
		AppliedEpoch:  state.AppliedEpoch,
		ConfigAttempt: state.ConfigAttempt,
		Seats:         heldSeats(state),

		IdentityDutySeconds: identityDutySeconds(state),
	}
	body.StallLagSeconds, body.UnprovenSeconds = stallAndStranded(state)
	body.Nodes, body.Alarms = fleetCounts(fleet)
	return body
}
