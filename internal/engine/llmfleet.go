package engine

import (
	"context"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// publishFleetFull announces that every member of basis's LLM fleet is at its
// limit, naming every seat its probe holds and the first room ([chain.Limit]).
func (e *Engine) publishFleetFull(ctx context.Context, basis providerBasis, limit chain.Limit) {
	full := types.LLMFleetFull{Fleet: basis.fleet, Waiting: e.probeHandles(basis),
		FirstRoomMember: limit.First}
	if !limit.RetryAt.IsZero() {
		at := limit.RetryAt.UTC()
		full.FirstRoomAt = &at
	}
	e.publishFleetEvent(ctx, full)
}

// publishFleetRoom announces that member of probe's LLM fleet answered, and
// the seats the answer releases.
func (e *Engine) publishFleetRoom(ctx context.Context, probe *limitProbe, member string) {
	e.publishFleetEvent(ctx, types.LLMFleetRoom{Fleet: probe.basis.fleet, Member: member,
		Released: e.probeHandles(probe.basis)})
}

// probeHandles is the handles of the seats on basis's probe, sorted.
func (e *Engine) probeHandles(basis providerBasis) []string {
	p := &e.limitProbes
	p.mu.Lock()
	defer p.mu.Unlock()
	probe := p.probes[probeKeyOf(basis)]
	if probe == nil {
		return []string{}
	}
	return slices.Sorted(maps.Values(probe.seats))
}

// publishFleetEvent publishes one of a fleet's events, best effort: the park
// holds whether or not a screen hears of it.
func (e *Engine) publishFleetEvent(ctx context.Context, payload events.Payload) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "llm_fleet_event_publish_failed", "type", ev.Type, "error", err.Error())
	}
}
