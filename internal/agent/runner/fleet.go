package runner

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// FleetDecision is the decision record of key's pick among picks: empty
// where no LLM fleet picked it (a pinned chain).
func FleetDecision(picks []*fleet.Pick, key string) types.FleetDecision {
	for _, p := range picks {
		if p.Key == key {
			return types.FleetDecision{Fleet: p.Fleet, Strategy: string(p.Strategy),
				ShareAtPick: p.Share(), FleetReason: string(p.Reason)}
		}
	}
	return types.FleetDecision{}
}

// withDecisions stamps each invocation with the decision record of the
// member that served it.
func withDecisions(runs []types.PhaseInvocation, picks []*fleet.Pick) []types.PhaseInvocation {
	for i := range runs {
		runs[i].FleetDecision = FleetDecision(picks, runs[i].ProviderKey)
	}
	return runs
}

// fleetPicked publishes the move of a piece of work whose first pick is not
// its seat job's home: its next turn starts on the new member.
func (e emitter) fleetPicked(ctx context.Context, ph phase.Phase, pick *fleet.Pick) {
	if !e.on() || pick == nil || !pick.Moved() {
		return
	}
	to := pick.Key
	e.fleetMoved(ctx, ph, pick, pick.Home, &to, pick.HomeWhy, fleet.WorkBetweenTurns)
}

// fleetHandOff publishes a fleet's hand-off as a move: on to the next pick
// mid-turn, or to nobody where the rest of the walk is members at their
// limit, whose refusals park the seat. A member at its limit handing on to
// the next is the walk confirming the wait, and says nothing.
func (e emitter) fleetHandOff(ctx context.Context, ph phase.Phase, f chain.Fallback) {
	if !e.on() || f.FromFleet.State == fleet.Limit {
		return
	}
	why := fleet.WhyFailed(f.Err)
	if f.ToFleet == nil || f.ToFleet.State == fleet.Limit {
		e.fleetMoved(ctx, ph, f.FromFleet, f.From, nil, why, fleet.WorkWaits)
		return
	}
	to := f.To
	e.fleetMoved(ctx, ph, f.FromFleet, f.From, &to, why, e.taskLabel()+fleet.WorkMidTurn)
}

// fleetMoved publishes one llm_fleet_moved.
func (e emitter) fleetMoved(ctx context.Context, ph phase.Phase, pick *fleet.Pick, from string,
	to *string, why, work string) {
	e.publish(ctx, events.New(types.LLMFleetMoved{
		Agent: e.turn.AgentID, RoleName: e.role, Fleet: pick.Fleet, Subject: e.handle,
		Job: ph.String(), From: from, To: to, Why: why, Work: work, TurnID: e.turn.RunID,
	}, e.traceFor(ctx)))
}

// taskLabel is what a mid-turn move names as the work: the item the turn is
// charged to, else the turn.
func (e emitter) taskLabel() string {
	if item := e.workItem(); item != nil && item.Key != "" {
		return item.Key
	}
	return "Its turn"
}
