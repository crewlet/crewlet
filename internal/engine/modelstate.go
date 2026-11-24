package engine

import (
	"context"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// WHAT THIS NODE KNOWS ABOUT ITS MODELS, as its status record publishes it: one
// row per entry of the active revision, from the entry's sign-in [harness.Plan]
// (sit-out, last answer, last failure, limit windows), the provider park and the
// login watcher.
//
// On the status record for the reason [coord.NodeStatusRecord.MCP] is: this is
// the node's own memory, and one listing of the records answers the Models page
// for the fleet, with no per-node fan-out and its partial answers.
//
// Read within a pass of the record's writer: every input is in memory, the
// sign-in route reads the login's files, and nothing here runs a process.

// modelStatus is this node's model rows, nil for a node that runs no model.
func (e *Engine) modelStatus(ctx context.Context) []coord.ModelStatus {
	c := e.Company()
	if c == nil || c.Config == nil || c.Models == nil || len(c.Config.Providers.LLM) == 0 {
		return nil
	}
	parked := e.parkedByEntry()
	homes, running := e.fleetHomes(), e.fleetRunning()
	now := e.now()
	keys := slices.Sorted(maps.Keys(c.Config.Providers.LLM))
	rows := make([]coord.ModelStatus, 0, len(keys))
	for _, key := range keys {
		row := coord.ModelStatus{Key: key, SignIn: "none", Parked: parked[key], ParkedCount: len(parked[key]),
			Homes: homes[key], Running: running[key]}
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
		// A TEST'S ANSWER stands for the model's own until a call of its own is heard.
		if v := e.verifiedAt(key, now); v.After(row.LastAnswer) {
			row.LastAnswer = v
		}
		if f := state.LastFailure; f != nil {
			row.LastFailure = &coord.ModelFailure{Kind: f.Kind.String(), At: f.At, Said: f.Said}
		}
		row.HostLogin = e.hostLoginOf(key)
		for _, l := range state.Limits {
			row.Limits = append(row.Limits, coord.ModelLimit{Window: string(l.Window), Hours: l.Hours,
				Model: l.Model, State: string(l.State), Used: l.Used, ResetsAt: l.ResetsAt, Seen: l.Seen})
		}
		rows = append(rows, row)
	}
	return rows
}

// sitOutFor is the window an entry on model sits out: the plan-wide one, which
// holds every model, else the one scoped to the entry's own model argument; a
// window scoped to another model does not hold this entry.
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

// fleetHomes is every seat job's LLM fleet home this node keeps, under the
// member it names, the most recent first: what the status record's model rows
// publish ([coord.ModelStatus.Homes]).
func (e *Engine) fleetHomes() map[string][]coord.FleetHome {
	out := map[string][]coord.FleetHome{}
	if e.ledger == nil {
		return out
	}
	for _, h := range e.ledger.Homes() {
		out[h.Member] = append(out[h.Member], coord.FleetHome{Fleet: h.Fleet, Subject: h.Subject,
			Job: h.Job.Job, At: h.At})
	}
	return out
}

// fleetRunning is every job's work in flight on this node, under the entry it
// runs on, the longest running first ([coord.ModelStatus.Running]).
func (e *Engine) fleetRunning() map[string][]coord.FleetRunning {
	out := map[string][]coord.FleetRunning{}
	if e.ledger == nil {
		return out
	}
	for _, r := range e.ledger.Running() {
		out[r.Member] = append(out[r.Member], coord.FleetRunning{Fleet: r.Fleet, Subject: r.Subject,
			Job: r.Job.Job, Since: r.Since})
	}
	return out
}
