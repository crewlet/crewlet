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
//
// AND THE ARTIFACT COPIES' SERVICE, which the socket's rooms follow: while
// this node's copy is out of service its appliers' movements describe a copy
// nobody vouches for, so the rooms are told to re-read as it leaves service
// and again as it returns ([Engine.SetOnArtifactsServing]).

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

// SetOnArtifactsServing registers what this node does when its artifact
// copies go out of service (false) or come back (true): the socket's rooms,
// which withhold this node's appliers' pushes while out and tell every room
// to re-read at each switch. Nil tells nobody. It must not block: it is called
// on the watcher's goroutine, once per switch.
func (e *Engine) SetOnArtifactsServing(fn func(serving bool)) {
	e.onArtifactsServing.Store(&fn)
}

// watchArtifactCopies tells [Engine.SetOnArtifactsServing]'s listener every
// time this node's copy leaves or returns to service, judged on the copy's
// own verdict at its own cadence ([servingRecheck]), until ctx ends. Only a
// data node holds a copy, so only a data node watches.
func (e *Engine) watchArtifactCopies(ctx context.Context) {
	serving := true
	for {
		faulted := e.local.faultedUnasked(ctx)
		if faulted == serving {
			serving = !faulted
			log.InfoContext(ctx, "artifact_copies_serving", "serving", serving,
				"detail", "the sockets' artifact rooms re-read through the estate router "+
					"at every switch, and hear this node's appliers only while it serves")
			if fn := e.onArtifactsServing.Load(); fn != nil && *fn != nil {
				(*fn)(serving)
			}
		}
		if !sleepCtx(ctx, servingRecheck) {
			return
		}
	}
}

// armArtifactWatch starts the copy watcher on a data node; its lifetime is the
// engine's, ended by [Engine.stopArtifactWatch].
func (e *Engine) armArtifactWatch(ctx context.Context) {
	if e.local == nil {
		return
	}
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.watchArtifactCopies(watchCtx)
	}()
	e.stopArtifactWatchFn = func() {
		cancel()
		<-done
	}
}

// stopArtifactWatch ends the copy watcher.
func (e *Engine) stopArtifactWatch() {
	if e.stopArtifactWatchFn != nil {
		e.stopArtifactWatchFn()
		e.stopArtifactWatchFn = nil
	}
}
