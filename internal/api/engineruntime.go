package api

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
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
	out := make([]ToolInfo, 0, len(entries))
	for _, entry := range entries {
		source := "builtin"
		if server, ok := entry.FromMCP(); ok {
			source = server
		}
		out = append(out, ToolInfo{
			Name:        entry.Name(),
			Description: entry.Tool.Description(),
			Source:      source,
		})
	}
	return out
}

// Snapshot is this node's live state.
func (r engineRuntime) Snapshot(ctx context.Context) RuntimeState {
	host := r.engine.Node().Host()
	return RuntimeState{
		InFlight:     r.engine.Backends().Queue.InFlightCount(),
		ShuttingDown: host.Draining(),
		Seats:        host.Held(),
		StartedAt:    r.engine.StartedAt().Format(time.RFC3339),
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
