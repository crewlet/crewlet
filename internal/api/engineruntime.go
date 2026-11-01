package api

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// engineRuntime is the [NodeRuntime] over a running engine and its reconciler.
//
// HERE, beside the seam it satisfies, rather than in the command that wires the
// node, so that `crewlet run` and the end-to-end suite answer health from the
// same code. A second copy in the suite would be a harness that agrees with
// itself about what the engine reports.
type engineRuntime struct {
	engine     *engine.Engine
	reconciler *engine.Reconciler
}

// NewEngineRuntime is the runtime a node's API asks, over the engine it runs
// beside and the reconciler that owns that engine's config posture.
//
// Both are required. The reconciler used to be optional here, which left the
// posture and the applied epoch blank: exactly the two facts an operator reads
// to learn why a node left rotation. Every node builds one before it serves.
func NewEngineRuntime(e *engine.Engine, reconciler *engine.Reconciler) (NodeRuntime, error) {
	runtime, err := newEngineRuntime(e, reconciler)
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func newEngineRuntime(e *engine.Engine, reconciler *engine.Reconciler) (engineRuntime, error) {
	switch {
	case e == nil:
		return engineRuntime{}, errors.New("api: the engine runtime needs the engine it reports on")
	case reconciler == nil:
		return engineRuntime{}, errors.New("api: the engine runtime needs the engine's reconciler: " +
			"it is the only source of the config posture and the applied epoch")
	}
	return engineRuntime{engine: e, reconciler: reconciler}, nil
}

// EngineProbeOptions is the [ProbeOptions] of a node without the ingress role
// that runs e: its runtime over the same engine and reconciler, under the same
// requirements, as [NewEngineRuntime] — so a node's /health says the same thing
// whichever surface serves it — its node id, the roles its presence lease
// advertises, and, where it runs seats, the engine's own bridge and telemetry
// receiver for the boxes it launches on this host.
//
// ONE WIRING, here rather than in the command that serves it, so that
// `crewlet run` and the end-to-end suite probe a node through the same code: a
// copy in the suite is a harness that agrees with itself, and a regression in
// which runtime or which roles the command hands the surface
// would pass every suite that probed the copy.
func EngineProbeOptions(boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler,
) (ProbeOptions, error) {
	if boot == nil {
		return ProbeOptions{}, errors.New("api: the probe surface needs the node's Tier A")
	}
	runtime, err := newEngineRuntime(e, reconciler)
	if err != nil {
		return ProbeOptions{}, err
	}
	// THE NAME THE ENGINE RUNS UNDER, which is what its presence lease
	// carries, and the profile Tier A gives that name — the one the engine
	// itself placed seats by.
	nodeID := e.Node().ID()
	profile := boot.Profile(nodeID)
	opts := ProbeOptions{
		Bootstrap: boot, Runtime: runtime, NodeID: nodeID,
		Roles: profile.Roles.Names(), QueueBackend: e.Backends().Queue.Backend(),
	}
	// ONLY WHERE THE NODE LAUNCHES BOXES: the seats role. A node without it
	// may still build a bridge — the fleet's variable is the same on every
	// node, and its relay verifies a token — but no box on this host dials
	// it, so its listener serves the network's probes and nothing beside.
	if profile.RunsSeats() {
		opts.Bridge, opts.OtelReceiver = e.Bridge(), e.OtelReceiver()
	}
	return opts, nil
}

// Configured reports whether a company revision is active: whether the
// engine's live epoch holds one, read on every call so an apply that brings
// this node its first revision flips it.
func (r engineRuntime) Configured() bool { return r.engine.Company() != nil }

// Work is what a node without the ingress role must hold to be doing its work,
// each fact read from what already tracks it — see the engine's readiness.go.
func (r engineRuntime) Work() WorkState {
	admission, admitted := r.engine.SeatAdmission()
	return WorkState{
		Broker:    r.engine.BrokerLink(),
		Presence:  r.engine.HoldsPresence(),
		Admission: admission,
		Admitted:  admitted,
	}
}

// Tools is the catalogue this node serves, for the dashboard's tool screen.
//
// THE EPOCH'S SHARED CATALOGUE, not a seat's. A per-role MCP server gives each
// seat its own child and its own registry, so there is no single "the tools" a
// company has, and picking one seat's would render a catalogue that is right
// for one row of the agent screen and wrong for the rest. The shared surface
// is the one every seat has, which is the honest answer to "what does this
// company run".
func (r engineRuntime) Tools() []queries.ToolRow {
	company := r.engine.Company()
	if company == nil || company.Tools == nil {
		return nil
	}
	// ONE SNAPSHOT for both reads: the epoch's shared MCP tools are refiled
	// when a server comes back with a different catalogue, and a list and a
	// delivery map read apart could each describe a different one.
	snapshot := company.Tools.Snapshot()
	entries := snapshot.Entries()
	// THE REGISTRY'S OWN PREDICATE for where a tool delivers, never
	// re-derived here: it is not "was this served by MCP" — a proven
	// read-only MCP tool delivers nowhere, and Worklet's
	// comment tool delivers although it is a builtin. A second derivation
	// would show one answer on the screen and enforce another at the fence.
	delivers := snapshot.Deliveries()
	out := make([]queries.ToolRow, 0, len(entries))
	for _, entry := range entries {
		out = append(out, queries.CatalogueRow(entry, delivers[entry.Name()]))
	}
	// AND WHAT THE FLEET LISTS that no registry here holds: a per-agent
	// server's tools, whose children run on the nodes holding its seats
	// and on no other. From the engine's watched copy and the schemas it
	// has read, never the store, since a push builds this; a copy not yet
	// answered adds nothing, and the engine re-sends the catalogue when it
	// is.
	if live, ok := r.engine.LiveMCPListings(); ok {
		out = append(out, queries.ListedRows(out, live, r.engine.HeldMCPToolSchemas())...)
	}
	return out
}

// Listings is the engine's watched copy of the fleet's MCP listings.
func (r engineRuntime) Listings() queries.MCPListingReader { return r.engine }

// Builtins is the engine's own tools as this node's seats are offered them:
// the registry's built-ins, and the discovery pair.
//
// THE REGISTRY'S BUILT-INS, not a list of every tool the engine could offer,
// because a company on Jira holds no Worklet tool and a catalogue listing one
// would tell a reader its agents can call what they are never offered.
//
// THE PAIR READ OFF THE RUNNER, which builds it, bound to no surface: this
// reads each one's name, description and schema and calls nothing, and a
// second description of either written here would drift from the one the
// model is shown.
func (r engineRuntime) Builtins() []queries.ToolRow {
	company := r.engine.Company()
	if company == nil || company.Tools == nil {
		return nil
	}
	snapshot := company.Tools.Snapshot()
	delivers := snapshot.Deliveries()
	var out []queries.ToolRow
	for _, entry := range snapshot.Entries() {
		if entry.Origin == tools.OriginBuiltin {
			out = append(out, queries.CatalogueRow(entry, delivers[entry.Name()]))
		}
	}
	for _, tool := range runner.DiscoveryTools(func() *tools.Surface { return nil }) {
		out = append(out, queries.CatalogueRow(
			tools.Entry{Tool: tool, Origin: tools.OriginBuiltin}, ""))
	}
	return out
}

// ShuttingDown is the engine's own flag, which is set at the first moment of
// its drain rather than when the drain reaches the seat host.
func (r engineRuntime) ShuttingDown() bool { return r.engine.ShuttingDown() }

// Seats is the seats the engine's host holds, from memory.
func (r engineRuntime) Seats() []string { return r.engine.Node().Host().Held() }

// Snapshot is this node's live state.
func (r engineRuntime) Snapshot(ctx context.Context) RuntimeState {
	host := r.engine.Node().Host()
	state := RuntimeState{
		InFlight: r.engine.Backends().Queue.InFlightCount(),
		// THE SAME FLAG the drain gate refuses work on, so a probe can
		// never report a node in rotation while its routes refuse.
		ShuttingDown: r.ShuttingDown(),
		// And the same seats the placement read adds to its listing.
		Seats: r.Seats(),
		// WHICH IDENTITY DUTIES THIS NODE ARMED, off the engine's own
		// record of the loops it started — the same answer the arming
		// decision produced, never a second derivation from the roles.
		IdentityDuties: r.engine.IdentityDuties(),
		// The seats this node could not prove it let go of, and for how
		// long. The one fleet fault that is silent everywhere else: the
		// lease is still ours, so no peer claims the seat, and the host
		// will not run it.
		Unproven:  host.UnprovenAges(),
		StartedAt: r.engine.StartedAt().Format(time.RFC3339),
		// Which integrations have a PARSER, which is the only thing that
		// makes a verified delivery reach an agent. Read from the notify
		// service rather than from a list kept here: a hand-maintained
		// one is exactly what drifts, and it would drift towards claiming
		// more than the build does.
		RoutedSources: r.engine.RoutedSources(),
		// And which of them could actually verify a delivery, from the
		// RESOLVED secrets rather than from the config text. Same reason:
		// a list of what the document names would claim more than this
		// process can do.
		VerifiableSources: r.engine.VerifiableSources(),
		// The watchdog's own reading, so a node degrading towards its
		// self-terminate threshold is visible before it hits it.
		StallLag: r.engine.StallLag(),
		// Whether uploads may go straight to the bucket, as the object
		// store's boot probe found it.
		DirectUpload: r.engine.DirectUploadStatus(),
		// Read live, on every probe, rather than cached: a cached posture
		// is a node that reports healthy through the whole window in which
		// it stopped being so. This is the ONLY place an operator can see
		// why a node left rotation, since /ready answers a bare 503 either
		// way and "draining" and "cannot apply epoch 41" call for opposite
		// responses.
		Posture:      string(r.reconciler.Posture(ctx)),
		AppliedEpoch: r.reconciler.Applied(),
	}
	if attempt, ok := r.reconciler.Attempt(); ok {
		state.ConfigAttempt = &ConfigAttempt{Epoch: attempt.Epoch,
			RevisionID: attempt.RevisionID, Status: attempt.Status}
	}
	return state
}

// Fleet is the envelope's fleet counts: the standing alarms, from memory, and
// the presence count, from the coordination plane under the caller's context.
//
// THE ALARMS FIRST, and unconditionally: they cost no I/O, so a presence read
// that runs out of budget leaves the count absent without taking the alarm
// count with it.
func (r engineRuntime) Fleet(ctx context.Context) FleetState {
	var state FleetState
	if standing, evaluated := r.engine.Alarms(); evaluated {
		// NON-NIL ONCE EVALUATED, even with nothing firing: an empty
		// slice is the real claim that nothing holds, and nil is that no
		// evaluation has run — see [FleetState.Alarms].
		state.Alarms = append(make([]statelog.Kind, 0, len(standing)), standing...)
	}
	// THE FLEET'S SIZE, from the presence leases every fan-out on this node
	// divides its work by, read live for the posture's reason. A failed or
	// out-of-budget read leaves it nil — see [FleetState.LiveNodes].
	if nodes, err := r.engine.LiveNodes(ctx); err == nil {
		state.LiveNodes = &nodes
	}
	return state
}
