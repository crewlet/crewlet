package runner

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tools"
)

// The required-skill guard, with telemetry.
//
// The guard itself decides; this adds the operator's view of it. They are
// separate because the guard is a pure decision over a registry and a
// surface — testable with neither a queue nor a turn — while the fact that
// somebody wants to WATCH those decisions belongs to the runner, which is
// where every other phase event is published from.

// reportingGuard publishes each refusal.
//
// OCCASIONAL BLOCKS ARE THE GUARD WORKING. What an operator needs to see is
// the chronic case: one skill blocked over and over says its catalogue
// summary is not landing, or that its trigger is over-scoped — and neither
// is visible from the turn's own record, where a block looks like any other
// failed tool call.
type reportingGuard struct {
	guard   *skills.Guard
	emit    emitter
	phase   phase.Phase
	round   int
	blocked func(context.Context, *events.Event)
}

var _ tools.Guard = (*reportingGuard)(nil)

// Check implements [tools.Guard].
func (g *reportingGuard) Check(tool, server string) string {
	block := g.guard.Blocking(tool, server)
	if block == nil {
		return ""
	}
	g.report(tool)
	return block.Error()
}

// Observe implements [tools.Guard].
func (g *reportingGuard) Observe(tool string, args map[string]any) {
	g.guard.Observe(tool, args)
}

// report publishes one refusal.
//
// It names EVERY pending skill rather than just the one that blocked this
// call: a model about to be blocked twice more is a model whose catalogue
// summary is not landing, and an operator reading one key at a time cannot
// see that.
func (g *reportingGuard) report(tool string) {
	if !g.emit.on() {
		return
	}
	ev := events.New(types.ToolSkillGuardBlocked{
		Agent:     g.emit.turn.AgentID,
		RoleName:  g.emit.role,
		Phase:     types.Phase(g.phase),
		ToolName:  tool,
		SkillKeys: g.guard.Pending(),
		TurnID:    g.emit.turn.ID,
		Iteration: g.round,
	}, g.emit.turn.Trace)
	// A BACKGROUND context, because tools.Guard has none to inherit — the
	// interface is called from a tool dispatch that passes only names. A
	// refusal is worth recording even when the call that triggered it is
	// being torn down, and the publish is fire-and-forget either way, so
	// there is nothing here a cancellation should reach.
	g.emit.publish(context.Background(), ev)
}
