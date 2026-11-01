package engine

import (
	"context"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// WHAT THIS NODE KNOWS ABOUT ITS MODELS, as its heartbeat publishes it (M9 of
// the Models design): one row per entry of the active revision, built from the
// entry's sign-in [harness.Plan] (its sit-out, last answer and last failure),
// the provider park (the seats waiting on it), and the login watcher.
//
// ON THE HEARTBEAT, for the reason [coord.NodeStatus.MCP] is: each of these is
// this node's own memory, every node re-sends its status anyway, and one lease
// listing answers the Models page for the whole fleet, where asking each node
// in turn would be a fan-out whose partial answers the status design refused.
//
// Read under the beat's own budget: every input is in memory, the sign-in
// route reads the login's files, and nothing here runs a process.

// modelStatus is this node's model rows, nil for a node that runs no model.
func (e *Engine) modelStatus(ctx context.Context) []coord.ModelStatus {
	c := e.Company()
	if c == nil || c.Config == nil || c.Models == nil || len(c.Config.Providers.LLM) == 0 {
		return nil
	}
	parked := e.parkedByEntry()
	now := e.now()
	keys := slices.Sorted(maps.Keys(c.Config.Providers.LLM))
	rows := make([]coord.ModelStatus, 0, len(keys))
	for _, key := range keys {
		row := coord.ModelStatus{Key: key, SignIn: "none", Parked: parked[key], ParkedCount: len(parked[key])}
		for _, p := range row.Parked {
			if row.ParkedSince.IsZero() || p.Since.Before(row.ParkedSince) {
				row.ParkedSince = p.Since
			}
		}
		provider, ok := c.Models.Provider(key)
		if !ok {
			rows = append(rows, row)
			continue
		}
		agent, isHarness := provider.(*harness.Provider)
		if !isHarness {
			rows = append(rows, row)
			continue
		}
		row.Plan = agent.SignInID().Short()
		route, resolves := agent.SignInRoute(ctx)
		row.SignIn, row.Resolves = string(route), resolves
		state := agent.Plan().State(now)
		row.SittingOut = sitOutFor(state, agent.ModelArgument())
		row.LastAnswer = state.LastAnswer
		if f := state.LastFailure; f != nil {
			row.LastFailure = &coord.ModelFailure{Kind: f.Kind.String(), At: f.At, Said: f.Said}
		}
		row.HostLogin = e.hostLoginOf(key)
		rows = append(rows, row)
	}
	return rows
}

// sitOutFor is the window an entry on model sits out: the plan-wide one, which
// holds every model, else the one scoped to the entry's own model argument. A
// window scoped to another model does not hold this entry, so it is not this
// entry's sit-out.
func sitOutFor(state harness.PlanState, model string) *coord.ModelSitOut {
	for _, out := range state.SittingOut {
		if out.Model != "" && out.Model != model {
			continue
		}
		return &coord.ModelSitOut{Until: out.Until, ResetsAt: out.ResetsAt, Since: out.Since,
			Window: string(out.Window), Said: out.Said}
	}
	return nil
}
