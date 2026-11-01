package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PRESENCE HUB'S ENGINE WIRING: one hub per node, on EVERY node, over the
// queue's ephemeral broadcast and the fleet keyring. Every node, because both
// halves of presence happen everywhere: a node serving the API holds sockets
// in rooms, and a node running seats holds the agents whose tool calls put
// them in rooms, and a `seats`-only node is the second without the first.

// newPresenceWire is the one-way sealed channel presence frames cross the
// fleet on, built from the keyring every state-log record is signed under. It
// fails only on a bootstrap [config.Bootstrap.Validate] would already have
// refused.
func newPresenceWire(ring statelog.Keyring) (*presence.Wire, error) {
	wire, err := signedask.NewOneWay[presence.Frame](presence.Channel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the presence channel's seal: %w", err)
	}
	return wire, nil
}

// armPresence starts this node's presence hub: an answerer of its peers' asks
// and the rebroadcast of its rooms. A node with no broker has no peer to tell
// and no room anybody else could see, so it runs none.
func (e *Engine) armPresence(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	opts := presence.Options{Queue: e.backends.Queue, Wire: e.presenceWire, Node: e.id,
		// THE FLEET'S READ BUDGET, the one every scatter a person waits
		// on is bounded by: a newly opened room waits no longer for its
		// peers than a history read does.
		AskBudget: eventfan.FleetReadBudget}
	// A NIL RECORDER STAYS A NIL INTERFACE, for the reason the datastore
	// writer's does.
	if e.metrics != nil {
		opts.Metrics = e.metrics
	}
	// THE NODE'S OBJECTIVE METER, which presence's joins and moves are
	// timed into.
	if e.objectives != nil {
		opts.Objectives = e.objectives
	}
	hub, err := presence.New(opts)
	if err != nil {
		return fmt.Errorf("engine: presence: %w", err)
	}
	if err := hub.Start(ctx); err != nil {
		return fmt.Errorf("engine: presence: %w", err)
	}
	e.presence = hub
	return nil
}

// stopPresence tells the fleet this node's members left and ends the hub,
// before the broker closes.
func (e *Engine) stopPresence(ctx context.Context) {
	if e.presence == nil {
		return
	}
	e.presence.Stop(context.WithoutCancel(ctx))
}

// Presence is this node's presence hub, or nil on an engine with no broker.
func (e *Engine) Presence() *presence.Hub { return e.presence }

// PresenceDomain is the key domain this node's presence frames are sealed
// under, for the gate that holds every channel's apart.
func (e *Engine) PresenceDomain() string { return e.presenceWire.Domain() }

// ArtifactDataToolDeps are the datastore tools' own half on this node: the
// datastore through the estate router and this node's turn guardrail. The
// tools act only beside the artifacts tools' half ([ArtifactToolDeps]), which
// carries the presence hub and is the zero value where the node takes no
// artifact uploads, so they are served exactly where those are.
func ArtifactDataToolDeps(e *Engine) builtin.ArtifactDataDeps {
	if e == nil || e.router == nil {
		return builtin.ArtifactDataDeps{}
	}
	return builtin.ArtifactDataDeps{Store: e.router.ArtifactData(), Turns: e.dataTurns}
}
