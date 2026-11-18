package runner

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// The required-skill guard, with telemetry. skills.Guard stays a pure
// decision, testable without a queue or a turn; publishing its refusals
// belongs to the runner, where every other phase event is published.

// reportingGuard publishes each refusal. Occasional blocks are the guard
// working; the event shows the chronic case (a skill blocked over and over:
// its summary is not landing or its trigger is over-scoped), which the
// turn's record shows only as another failed tool call.
type reportingGuard struct {
	guard *skills.Guard
	emit  emitter
	phase phase.Phase
	// iteration is the turn iteration the guarded surface was built for.
	iteration int
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
func (g *reportingGuard) Observe(tool string, args toolargs.Args) {
	g.guard.Observe(tool, args)
}

// report publishes one refusal, naming EVERY pending skill rather than the
// one that blocked: a model about to be blocked again is invisible to an
// operator reading one key at a time.
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
		TurnID:    g.emit.turn.RunID,
		WorkKey:   g.emit.turn.WorkKey,
		Iteration: g.iteration,
	}, g.emit.turn.Trace)
	// Background: tools.Guard passes no context, and a fire-and-forget
	// refusal is worth recording even while its call is torn down.
	g.emit.publish(context.Background(), ev)
}
