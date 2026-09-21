package api

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
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
	switch {
	case e == nil:
		return nil, errors.New("api: the engine runtime needs the engine it reports on")
	case reconciler == nil:
		return nil, errors.New("api: the engine runtime needs the engine's reconciler: " +
			"it is the only source of the config posture and the applied epoch")
	}
	return engineRuntime{engine: e, reconciler: reconciler}, nil
}

// Tools is the catalogue this node serves, for the dashboard's tool screen.
//
// THE EPOCH'S SHARED CATALOGUE, not a seat's. A per-role MCP server gives each
// seat its own child and its own registry, so there is no single "the tools" a
// company has, and picking one seat's would render a catalogue that is right
// for one row of the agent screen and wrong for the rest. The shared surface
// is the one every seat has, which is the honest answer to "what does this
// company run".
func (r engineRuntime) Tools() []ToolInfo {
	company := r.engine.Company()
	if company == nil || company.Tools == nil {
		return nil
	}
	entries := company.Tools.List()
	// THE REGISTRY'S OWN PREDICATE for where a tool delivers, never
	// re-derived here: it is not "was this served by MCP" — a proven
	// read-only MCP tool delivers nowhere, and the native tracker's
	// comment tool delivers although it is a builtin. A second derivation
	// would show one answer on the screen and enforce another at the fence.
	delivers := company.Tools.Deliveries()
	out := make([]ToolInfo, 0, len(entries))
	for _, entry := range entries {
		out = append(out, toolInfo(entry, delivers[entry.Name()]))
	}
	return out
}

// toolInfo is one registry entry as the catalogue carries it.
//
// A FUNCTION RATHER THAN A LOOP BODY so it can be exercised without an engine.
// The mapping had a defect nothing could reach: it re-spelled the origin
// instead of passing the registry's own, and every reader of the prefix — the
// screen's origin column, its per-origin counts, the published reference —
// silently read a company running MCP servers as one running none.
func toolInfo(entry tools.Entry, delivers string) ToolInfo {
	return ToolInfo{
		Name:        entry.Name(),
		Description: entry.Tool.Description(),
		// THE REGISTRY'S OWN GRAMMAR, passed through rather than
		// re-spelled: `builtin`, or `mcp:` and the bare server name.
		// This stripped the prefix and sent the server's name alone,
		// which disagreed with every reader of it — the screen's
		// origin column splits on the colon and rendered the server
		// as if it were the grammar's own word, its "from MCP
		// servers" count tested for the prefix and therefore read
		// zero on a company running servers, and the published API
		// reference documents the prefixed form.
		Source: entry.Origin,
		Annotations: ToolAnnotations{
			Title:       entry.Annotations.Title,
			ReadOnly:    entry.Annotations.ReadOnly.String(),
			Destructive: entry.Annotations.Destructive.String(),
			Idempotent:  entry.Annotations.Idempotent.String(),
			OpenWorld:   entry.Annotations.OpenWorld.String(),
		},
		Delivers: delivers,
		// The schema the MODEL is offered, read rather than rebuilt. The
		// map is the tool's own and this path only serialises it; an MCP
		// server restarting replaces the whole Tool rather than writing
		// into its schema, so there is nothing here for a running turn to
		// race with.
		InputSchema: entry.Tool.Parameters(),
	}
}

// ShuttingDown is the engine's own flag, which is set at the first moment of
// its drain rather than when the drain reaches the seat host.
func (r engineRuntime) ShuttingDown() bool { return r.engine.ShuttingDown() }

// Snapshot is this node's live state.
func (r engineRuntime) Snapshot(ctx context.Context) RuntimeState {
	host := r.engine.Node().Host()
	return RuntimeState{
		InFlight: r.engine.Backends().Queue.InFlightCount(),
		// THE SAME FLAG the drain gate refuses work on, so a probe can
		// never report a node in rotation while its routes refuse.
		ShuttingDown: r.ShuttingDown(),
		Seats:        host.Held(),
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
		// Read live, on every probe, rather than cached: a cached posture
		// is a node that reports healthy through the whole window in which
		// it stopped being so. This is the ONLY place an operator can see
		// why a node left rotation, since /ready answers a bare 503 either
		// way and "draining" and "cannot apply epoch 41" call for opposite
		// responses.
		Posture:      string(r.reconciler.Posture(ctx)),
		AppliedEpoch: r.reconciler.Applied(),
	}
}
