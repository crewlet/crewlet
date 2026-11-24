package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
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

// helperRun is the helper's run a helper field resolves to on this node: the
// entry, its run, and an LLM fleet's decision record where one picked it.
type helperRun struct {
	key      string
	provider llm.Provider
	fleet    types.FleetDecision
}

// helperPick resolves a helper's field ref (field names it) to the helper's
// run of an entry: the entry itself, or its fleet's first pick a helper can
// run on, taken where take is so it becomes the helper's home. A fleet with
// no such member is its [*phase.EmptyFleetError].
func helperPick(c *Company, field, ref, subject string, take bool) (helperRun, error) {
	fleetKey, onFleet := config.FleetOf(ref)
	if !onFleet {
		provider, ok := c.Helpers[ref]
		if !ok {
			return helperRun{}, fmt.Errorf("engine: %s names %q, which the running company does "+
				"not build as a helper's run", field, ref)
		}
		return helperRun{key: ref, provider: provider}, nil
	}
	picks, err := c.Models.FleetWalk(fleetKey, fleet.Job{Fleet: fleetKey, Subject: subject, Job: subject})
	if err != nil {
		return helperRun{}, fmt.Errorf("engine: %s: %w", field, err)
	}
	for _, p := range picks {
		provider, ok := c.Helpers[p.Key]
		if !ok {
			continue
		}
		if take {
			p.Take()
		}
		return helperRun{key: p.Key, provider: provider,
			fleet: runner.FleetDecision([]*fleet.Pick{p}, p.Key)}, nil
	}
	return helperRun{}, fmt.Errorf("engine: %s: %w", field, &phase.EmptyFleetError{Fleet: fleetKey})
}
