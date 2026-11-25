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
// entry and its run, and on an LLM fleet every member a helper can run on, in
// the walk's order, with the fleet's picks of them and its decision record.
type helperRun struct {
	key      string
	provider llm.Provider
	// members and picks are the fleet's walk, index for index; nil when pinned.
	members []chain.Member
	picks   []*fleet.Pick
	fleet   types.FleetDecision
	// job is the helper's work as the ledger records it in flight: under its
	// fleet, none when pinned.
	job fleet.Job
}

// inFlight is what records each call on member key in flight on ledger, nil
// where there is no ledger.
func (run helperRun) inFlight(ledger *fleet.Ledger, key string) func() (end func()) {
	if ledger == nil {
		return nil
	}
	return func() func() { return ledger.Begin(run.job, key) }
}

// called is run as the provider a helper calls, each member wrapped by wrap: the
// entry itself when pinned, and on an LLM fleet a chain over every member, so a
// limit or failure before an answer moves the call to the next one and only a
// walk no member answers fails ([chain.Chain.Complete]).
func (run helperRun) called(wrap func(key string, inner llm.Provider) llm.Provider) (llm.Provider, error) {
	if len(run.members) == 0 {
		return wrap(run.key, run.provider), nil
	}
	members := make([]chain.Member, len(run.members))
	for i, m := range run.members {
		members[i] = chain.Member{Key: m.Key, Provider: wrap(m.Key, m.Provider)}
	}
	walk, err := chain.New(members, chain.Options{Fleet: run.picks})
	if err != nil {
		return nil, err
	}
	return helperChain{Chain: walk, fleet: run.fleet}, nil
}

// helperChain is a helper's walk over its LLM fleet, which says why its first
// member was picked.
type helperChain struct {
	*chain.Chain
	fleet types.FleetDecision
}

// FleetDecision is why the fleet picked the walk's first member.
func (c helperChain) FleetDecision() types.FleetDecision { return c.fleet }

// helperPick resolves a helper's field ref (field names it) to the helper's
// run of an entry: the entry itself, or every member of its fleet a helper can
// run on, in the walk's order (the head first, its home where it keeps one).
// take makes the head the helper's home now; a call takes the member that
// serves it. A fleet with no such member is its [*phase.EmptyFleetError].
func helperPick(c *Company, field, ref, subject string, take bool) (helperRun, error) {
	fleetKey, onFleet := config.FleetOf(ref)
	if !onFleet {
		provider, ok := c.Helpers[ref]
		if !ok {
			return helperRun{}, fmt.Errorf("engine: %s names %q, which the running company does "+
				"not build as a helper's run", field, ref)
		}
		return helperRun{key: ref, provider: provider, job: fleet.Job{Subject: subject, Job: subject}}, nil
	}
	job := fleet.Job{Fleet: fleetKey, Subject: subject, Job: subject}
	picks, err := c.Models.FleetWalk(fleetKey, job)
	if err != nil {
		return helperRun{}, fmt.Errorf("engine: %s: %w", field, err)
	}
	run := helperRun{job: job}
	for _, p := range picks {
		provider, ok := c.Helpers[p.Key]
		if !ok {
			continue
		}
		run.members = append(run.members, chain.Member{Key: p.Key, Provider: provider})
		run.picks = append(run.picks, p)
	}
	if len(run.members) == 0 {
		return helperRun{}, fmt.Errorf("engine: %s: %w", field, &phase.EmptyFleetError{Fleet: fleetKey})
	}
	head := run.picks[0]
	if take {
		head.Take()
	}
	run.key, run.provider = head.Key, run.members[0].Provider
	run.fleet = runner.FleetDecision([]*fleet.Pick{head}, head.Key)
	return run, nil
}

// unready is the failure a call through run would meet as far as its members
// know without a call, nil where any may answer; on an LLM fleet, the fleet's
// walk exhausted ([chain.Error]), as a call through it would be.
func (run helperRun) unready(ctx context.Context) error {
	first := chain.Unready(ctx, run.providers())
	if first == nil || len(run.members) == 0 {
		return first
	}
	exhausted := &chain.Error{Err: first, Fleet: run.job.Fleet}
	for _, member := range run.members {
		exhausted.Attempted = append(exhausted.Attempted, member.Key)
		exhausted.Failures = append(exhausted.Failures, chain.Unready(ctx, []llm.Provider{member.Provider}))
	}
	return exhausted
}

// providers is every provider a call through run may reach, in order.
func (run helperRun) providers() []llm.Provider {
	if len(run.members) == 0 {
		return []llm.Provider{run.provider}
	}
	out := make([]llm.Provider, len(run.members))
	for i, m := range run.members {
		out[i] = m.Provider
	}
	return out
}

// inFlightProvider is a provider whose every call is recorded in flight on
// its entry while it runs ([fleet.Ledger.Begin]).
type inFlightProvider struct {
	inner llm.Provider
	begin func() (end func())
}

func (p inFlightProvider) Model() string { return p.inner.Model() }

func (p inFlightProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	end := p.begin()
	defer end()
	return p.inner.Complete(ctx, req)
}
