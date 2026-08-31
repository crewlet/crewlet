package runner

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// The sub-agent spawner, wired onto Execute.
//
// internal/agent/subagent was imported by nothing outside its own test, so
// spawn_subagent was never registered and every turn_engine.subagent_* knob
// was validated, schema'd, documented and read by nobody. The package's whole
// contract — the grant, the caps, the batch, the panic containment — could
// not run.

// Remaining is the seat's remaining token allowance, as the spawner needs it.
//
// THREE-VALUED, and the third value is why this is an interface rather than
// an int: [subagent.Config.ParentRemaining] reads ZERO AS UNCAPPED, so a
// counter that answered 0 for "I could not reach the store" would un-cap every
// child in the fleet on exactly the failure the rest of coordination fails
// closed for. An error here refuses the spawn instead.
type Remaining interface {
	Remaining(ctx context.Context) (int, error)
}

// SubagentConfig is what a turn needs to spawn sub-agents. Nil on
// [Config.Subagent] leaves the tool off the surface entirely, which is the
// honest shape for a build with no budget source and for a test.
type SubagentConfig struct {
	// Limits are the company's own caps, mapped from turn_engine.
	Limits subagent.Limits

	// Remaining reads the seat's headroom. Nil means the seat is uncapped,
	// which is the same thing a company with no token budget already is —
	// distinct from a read that FAILED, which refuses the spawn.
	Remaining Remaining
}

// spawnEntry is the sub-agent tool as it goes onto a phase surface, or the
// zero Entry when this turn cannot spawn.
//
// EXECUTE ONLY. Plan is choosing what to do and Review is judging what was
// done; a spawner on either is a phase that can spend a batch of model calls
// on work the turn has not decided to do or has already finished. Onboarding
// is a seat reading its own team's pages, which is not fan-out work.
func (r *Runner) spawnEntry(ctx context.Context, ph phase.Phase, round int,
	snapshot tools.Snapshot, surface func() *tools.Surface,
) tools.Entry {
	if r.cfg.Subagent == nil || ph != phase.Execute {
		return tools.Entry{}
	}
	remaining, ok := r.parentRemaining(ctx)
	if !ok {
		return tools.Entry{}
	}
	tool := subagent.NewTool(subagent.Config{
		Seat: r.cfg.Seat, Models: r.cfg.Models,
		// The parent's UNIVERSE and its LIVE active list. The second is a
		// getter because an executor that discovers a tool mid-phase and
		// activates it has widened what it may call, and a child spawned
		// afterwards inherits that.
		Universe: snapshot,
		Parent: func() []string {
			s := surface()
			if s == nil {
				return nil
			}
			return s.Active()
		},
		Discovery: DiscoveryTools,
		Skills:    r.catalogue(),
		Budget:    r.cfg.Budget,
		// Read ONCE, here, rather than per child: the fraction has to be
		// one number for a whole batch, or later children get a share of
		// a total their siblings have already spent from.
		ParentRemaining: remaining,
		Limits:          r.cfg.Subagent.Limits,
		Publisher:       r.cfg.Publisher,
		Trace:           r.cfg.Turn.Trace,
		// The parent turn, so every seat-scoped tool in the grant works.
		// Without it a child is handed tools that always fail.
		Turn: r.cfg.Turn.Context,
		// And the parent's own load-before-use gate, so a child cannot
		// reach by being spawned what its parent would have had to load a
		// skill for.
		// Built from the CHILD's own surface, for the same reason the
		// parent's is built from a finished one: what the guard enforces
		// and what the catalogue showed must come from the same active
		// list, and the child's does not exist until subagent builds it.
		Guard: func(child *tools.Surface) tools.Guard {
			return r.guardFor(phase.Subagent, child)
		},
		Telemetry: r.emitter().nestedAt(round).subagentCompleted,
	})
	return tools.Entry{
		Tool: tool, Origin: tools.OriginBuiltin,
		// NOT a known read, or the delivery gate would count a turn that
		// planned to act and then only spawned as having delivered
		// nothing. NOT a shared-surface write either: a spawn is
		// in-process work under the parent's own name, and marking it one
		// would say something about the child's effects rather than about
		// this call. It is already on subagent's own denylist, so a child
		// can never reach it whatever these say.
		Annotations: tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No,
		},
	}
}

// parentRemaining reads the seat's headroom, reporting whether spawning is
// allowed at all.
//
// FAILS CLOSED, which is the opposite of what the surrounding package does
// with a nil budget and is deliberate: nil means the operator configured no
// cap, while an ERROR means the counter that enforces one could not be read.
// Answering zero for the second would read as UNCAPPED and let a fan-out spend
// without a ceiling on exactly the failure a budget exists for.
func (r *Runner) parentRemaining(ctx context.Context) (int, bool) {
	if r.cfg.Subagent == nil || r.cfg.Subagent.Remaining == nil {
		// No counter configured: the seat itself runs uncapped, so its
		// children do too. Zero is what subagent reads as uncapped.
		return 0, true
	}
	remaining, err := r.cfg.Subagent.Remaining.Remaining(ctx)
	if err != nil {
		log.WarnContext(ctx, "subagent_budget_unreadable", "error", err.Error(),
			"detail", "the spawner is left off this phase's surface rather than "+
				"offered with no ceiling")
		return 0, false
	}
	if remaining < 0 {
		return 0, false
	}
	return remaining, true
}
