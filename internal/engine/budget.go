package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// Enforcing the token budget.
//
// The seam existed and nothing supplied it: runner.Config.Budget was nil on
// every turn, so a company with `token_budget: 100000` spent without limit and
// the number in its config was decoration. Money leaves the building for every
// token, which is why this fails CLOSED — a counter that cannot be reached
// stops the round rather than silently un-capping the company.
//
// CAPS ARE READ OFF THE EPOCH, usage off the shared counter, and the split is
// the design: a revision that raises a ceiling takes effect on the next turn
// (the cap travels in on every call), while the counter has to be one number
// across the fleet or N nodes each spend the whole allowance.

// meter charges one seat's rounds against the shared counter.
//
// Per turn, holding the caps the turn was PINNED to — so a mid-turn config
// change cannot move the ceiling a round is judged against, which is the same
// rule every other epoch read follows (rewrite/decisions/404).
type meter struct {
	budgets    *store.Budgets
	agentID    string
	orgLimit   int
	agentLimit int
}

var _ toolloop.BudgetMeter = (*meter)(nil)

// Spend checks and increments in ONE operation. See store.Budgets.Charge.
func (m *meter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	got, err := m.budgets.Charge(ctx, m.agentID, tokens, m.orgLimit, m.agentLimit)
	if err != nil {
		// NOT a refusal. The caller must tell "the company is out of
		// tokens" from "the counter is unreachable": the first is a
		// budget event an operator acts on, the second is an outage.
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	if !got.OK {
		return toolloop.SpendOutcome{
			Scope: got.RefusedScope, Used: got.RefusedUsed, Limit: got.RefusedLimit,
		}, nil
	}
	return toolloop.SpendOutcome{OK: true, Used: got.OrgUsed, Limit: m.orgLimit}, nil
}

// meterFor builds the meter for one seat's turn, or nil.
//
// Nil when there is nothing to enforce — no store, or no ceiling anywhere in
// the epoch. A meter over an unlimited budget would put a database round trip
// on every LLM round to answer "yes" every time, which is the cost of a check
// with no question behind it.
func (e *Engine) meterFor(c *Company, handle string) toolloop.BudgetMeter {
	if e.backends == nil || e.backends.Store == nil || c == nil || c.Org == nil {
		return nil
	}
	seat := c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	agentID, ok := c.Org.AgentIDFor(seat)
	if !ok {
		return nil
	}
	orgLimit, agentLimit := c.Config.TokenBudget, seatBudget(c.Org, seat)
	if orgLimit <= 0 && agentLimit <= 0 {
		return nil
	}
	return &meter{
		budgets: e.backends.Store.Budgets(), agentID: agentID.String(),
		orgLimit: orgLimit, agentLimit: agentLimit,
	}
}

// seatBudget is a seat's own ceiling, 0 for unlimited.
//
// The ROLE's, not the unit's: a unit budget would need a third counter scope
// and a rule for which of three caps a refusal names, and no config field
// declares one. Stated because the absence looks like an oversight otherwise.
func seatBudget(_ *org.Organization, seat *org.Role) int {
	if seat == nil {
		return 0
	}
	return seat.TokenBudget
}
