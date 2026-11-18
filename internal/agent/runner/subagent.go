package runner

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// The sub-agent spawner, wired onto Execute.

// Remaining is the seat's remaining token allowance, as the spawner needs it.
//
// THREE-VALUED, hence an interface: [subagent.Config.ParentRemaining] reads
// ZERO AS UNCAPPED, so answering 0 for an unreachable store would un-cap every
// child. An error refuses the spawn instead.
type Remaining interface {
	Remaining(ctx context.Context) (int, error)
}

// SubagentConfig is what a turn needs to spawn sub-agents. Nil on
// [Config.Subagent] leaves the tool off the surface entirely.
type SubagentConfig struct {
	// Limits are the company's own caps, mapped from
	// turn_engine.delegation.
	Limits subagent.Limits

	// Workers are the delegate templates this seat may name, already
	// narrowed by its role's visibility list. Empty: every task writes its
	// own prompt.
	Workers map[string]config.Worker

	// Remaining reads the seat's headroom. Nil means uncapped, distinct
	// from a read that FAILED, which refuses the spawn.
	Remaining Remaining
}

// workerCatalogue renders the `## Your workers` block, or "".
//
// SORTED, because it reaches a system prompt: lines reshuffled between turns
// cost the provider's prompt cache the whole prefix. The line's shape is
// config.DescribeWorker's, shared with the delegate tool's refusal.
func (r *Runner) workerCatalogue() string {
	if r.cfg.Subagent == nil || len(r.cfg.Subagent.Workers) == 0 {
		return ""
	}
	workers := r.cfg.Subagent.Workers
	lines := make([]string, 0, len(workers))
	for _, name := range config.WorkerNames(workers) {
		lines = append(lines, config.DescribeWorker(name, workers[name]))
	}
	return strings.Join(lines, "\n")
}

// spawnEntry is the sub-agent tool as it goes onto a phase surface, or the
// zero Entry when this turn cannot spawn.
//
// EXECUTE ONLY: review judges finished work, and onboarding reads the team's
// own pages; neither is fan-out work.
func (r *Runner) spawnEntry(ctx context.Context, m meter, ph phase.Phase, round int,
	snapshot tools.Snapshot, surface func() *tools.Surface,
) tools.Entry {
	if r.cfg.Subagent == nil || ph != phase.Execute {
		return tools.Entry{}
	}
	remaining, ok := r.parentRemaining(ctx)
	if !ok {
		return tools.Entry{}
	}
	// ONE OFFER FOR THE PHASE'S WORKERS, drained as each finishes, so each
	// worker prompt's catalogue is recorded once ([emitter.skillsInjected]):
	// completion is the one hook subagent calls on every path, after the
	// prompt is built.
	offer := r.cfg.Skills.Offer()
	// The workers' spend is the SURFACE's: a bridged executor's workers
	// run after this segment ended, and are paid by the one that resumes.
	emit := r.emitter().nestedAt(round).talliedOn(m.spend, m.mu)
	tool := subagent.NewTool(subagent.Config{
		Seat: r.cfg.Seat, Models: r.cfg.Models,
		// The parent's UNIVERSE and its LIVE active list: a getter, so a
		// child spawned after a mid-phase activation inherits it.
		Universe: snapshot,
		Parent: func() []string {
			s := surface()
			if s == nil {
				return nil
			}
			return s.Active()
		},
		Discovery: DiscoveryTools,
		Compact:   m.compact,
		Skills:    offer.Catalogue(),
		Budget:    r.cfg.Budget,
		// The parent's OWN fence: a worker has no grant, and a fresh check
		// could close on a different epoch than the spawning turn.
		Fence: r.cfg.Fence,
		// The same host the parent's tools are served from.
		Sessions: r.cfg.Sessions,
		// Read ONCE, not per child: the fraction is of one number for the
		// batch, not of a total siblings already spent from.
		ParentRemaining: remaining,
		Limits:          r.cfg.Subagent.Limits,
		Publisher:       r.cfg.Publisher,
		Trace:           r.cfg.Turn.Trace,
		// Every call the run makes is recorded on its one round ([runRound]).
		Round: runRound,
		// The parent turn, or every seat-scoped tool in the grant fails.
		Turn: m.turn,
		// The parent's load-before-use gate, so spawning reaches nothing
		// the parent would need a skill for; built from the CHILD's
		// surface, so the guard and the catalogue read one active list.
		Workers: r.cfg.Subagent.Workers,
		Guard: func(child *tools.Surface) tools.Guard {
			return r.guardFor(phase.Subagent, round, child, nil).tools()
		},
		Counter: r.counter(),
		Telemetry: func(ctx context.Context, res subagent.Result) {
			emit.subagentCompleted(ctx, res)
			emit.skillsInjected(ctx, phase.Subagent, offer.Drain())
		},
	})
	return tools.Entry{
		Tool: tool, Origin: tools.OriginBuiltin,
		// NOT a known read, or a turn that only spawned would count as
		// delivering; NOT a shared-surface write, since a spawn is in-process
		// work and the child's effects are not this call's. Subagent's
		// denylist keeps it from children regardless.
		Annotations: tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No,
		},
	}
}

// parentRemaining reads the seat's headroom, reporting whether spawning is
// allowed at all.
//
// FAILS CLOSED on an ERROR, unlike a nil reader (no cap configured): subagent
// reads zero as UNCAPPED, so answering zero would let a fan-out spend without
// a ceiling on exactly the failure a budget exists for.
//
// AND ON A READER ANSWERING NOTHING LEFT: zero from a reader is a capped seat
// with its tightest window spent (an uncapped seat gets no reader), and passed
// on it would read as uncapped too. Nothing is left to share: the phase's own
// first call is refused in that window anyway.
func (r *Runner) parentRemaining(ctx context.Context) (int, bool) {
	if r.cfg.Subagent == nil || r.cfg.Subagent.Remaining == nil {
		// No counter: the seat is uncapped, so its children are (zero).
		return 0, true
	}
	remaining, err := r.cfg.Subagent.Remaining.Remaining(ctx)
	if err != nil {
		log.WarnContext(ctx, "subagent_budget_unreadable", "error", err.Error(),
			"detail", "the spawner is left off this phase's surface rather than "+
				"offered with no ceiling")
		return 0, false
	}
	if remaining <= 0 {
		log.InfoContext(ctx, "subagent_budget_spent", "remaining", remaining,
			"detail", "the seat has no token budget left to share, so the spawner is left "+
				"off this phase's surface rather than offered with no ceiling")
		return 0, false
	}
	return remaining, true
}

// counter is the recorder a worker's surface counts into, or a nil interface
// when the runner holds none (never a nil pointer inside one).
func (r *Runner) counter() tools.Counter {
	if r.cfg.Metrics == nil {
		return nil
	}
	return r.cfg.Metrics
}
