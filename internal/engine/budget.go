package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
)

// Enforcing the token budget.
//
// The seam existed and nothing supplied it: runner.Config.Budget was nil on
// every turn, so a company with `token_budget: 100000` spent without limit and
// the number in its config was decoration. Money leaves the building for every
// token, which is why this fails CLOSED — a counter that cannot be reached
// stops the round rather than silently un-capping the company.
//
// CAPS ARE READ OFF THE EPOCH, usage off the fleet's shared counter, and the
// split is the design: a revision that raises a ceiling takes effect on the
// next turn (the cap travels in on every call), while the counter has to be
// one number across the fleet or N nodes each spend the whole allowance —
// which is exactly what a counter on the node's own database was.

// meter charges one seat's rounds against the shared counter.
//
// Per turn, holding the caps the turn was PINNED to — so a mid-turn config
// change cannot move the ceiling a round is judged against, which is the same
// rule every other epoch read follows.
type meter struct {
	budgets    coord.Budgets
	agentScope string
	orgLimit   int
	agentLimit int
}

var _ toolloop.BudgetMeter = (*meter)(nil)

// Spend checks and increments in ONE operation. See coord.Budgets.Charge.
func (m *meter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	got, err := m.budgets.Charge(ctx, m.agentScope, tokens, m.orgLimit, m.agentLimit)
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

// Remaining is this seat's headroom, in tokens.
//
// THREE-VALUED, and the third value is the whole reason this is not an int.
// [subagent.Config.ParentRemaining] reads ZERO AS UNCAPPED, so a counter that
// answered 0 for "I could not reach the store" would hand a fan-out no ceiling
// at all — the fail-OPEN direction, on the one path where money leaves the
// building per token. The error travels, and the caller refuses the spawn.
//
// The TIGHTER of the two headrooms, because a charge is checked against both:
// a seat with room under its own cap but none under the company's has no room.
// A limit of 0 is unlimited for that scope, matching the config; both
// unlimited answers zero with a nil error, which is the same "no ceiling" a
// company that set no budget already has.
func (m *meter) Remaining(ctx context.Context) (int, error) {
	headroom, capped := 0, false
	for _, scope := range []struct {
		key   string
		limit int
	}{
		{coord.OrgScope, m.orgLimit},
		{m.agentScope, m.agentLimit},
	} {
		if scope.limit <= 0 {
			continue
		}
		used, err := m.budgets.Used(ctx, scope.key)
		if err != nil {
			return 0, fmt.Errorf("engine: budget headroom for %s: %w", scope.key, err)
		}
		// TRACKED WITH A FLAG, not by testing headroom against zero: a
		// scope that has spent its whole allowance HAS zero headroom, and
		// reading that as "not set yet" would let the other scope's room
		// overwrite it — turning an exhausted company into an uncapped
		// one at exactly the moment the cap matters.
		if left := max(scope.limit-used, 0); !capped || left < headroom {
			headroom, capped = left, true
		}
	}
	return headroom, nil
}

// remainingFor is the seat's headroom reader, or nil.
//
// Nil where meterFor is nil and for the same reason: with no ceiling anywhere
// there is nothing to read, and the spawner treats that as uncapped — which is
// exactly what the seat itself is.
func (e *Engine) remainingFor(c *Company, handle string) runner.Remaining {
	m := e.meterFor(c, handle)
	if m == nil {
		return nil
	}
	// meterFor's contract is the interface; the concrete type is what
	// carries the headroom read. A meter it did not build is a
	// programming error rather than a runtime one.
	concrete, ok := m.(*meter)
	if !ok {
		return nil
	}
	return concrete
}

// meterFor builds the meter for one seat's turn, or nil.
//
// Nil when there is nothing to enforce — no coordination store, or no ceiling
// anywhere in the epoch. A meter over an unlimited budget would put a network
// round trip on every LLM round to answer "yes" every time, which is the cost
// of a check with no question behind it.
func (e *Engine) meterFor(c *Company, handle string) toolloop.BudgetMeter {
	if e.backends == nil || e.backends.Fleet == nil || c == nil || c.Org == nil {
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
		budgets: e.backends.Fleet, agentScope: coord.AgentScope(agentID.String()),
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
