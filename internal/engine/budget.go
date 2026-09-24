package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
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
//
// A REFUSED ROUND IS COUNTED ALL THE SAME, because it has been billed: the
// round's model call answered before it was charged, which is the only point
// its size is known. The gate counts nothing it refuses, so the refused tokens
// are post-charged ([coord.Budgets.PostCharge]) — without that the counter
// under-states the company by exactly the rounds that found it at its cap,
// and disagrees with every phase record, which reports what the provider
// billed. See [toolloop.BudgetMeter].
func (m *meter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	got, err := m.budgets.Charge(ctx, m.agentScope, tokens, m.orgLimit, m.agentLimit)
	if err != nil {
		// NOT a refusal. The caller must tell "the company is out of
		// tokens" from "the counter is unreachable": the first is a
		// budget event an operator acts on, the second is an outage.
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	if !got.OK {
		m.countRefused(ctx, tokens, got)
		return toolloop.SpendOutcome{
			Scope: got.RefusedScope, Used: got.RefusedUsed, Limit: got.RefusedLimit,
		}, nil
	}
	return toolloop.SpendOutcome{OK: true, Used: got.OrgUsed, Limit: m.orgLimit}, nil
}

// countRefused post-charges a round the gate refused.
//
// WITHOUT THE CALLER'S CANCELLATION: the tokens are spent at the vendor
// whatever becomes of the turn, and a write skipped because the turn's context
// ended between the refusal and here is spend no counter ever hears about.
//
// A post-charge that fails is LOGGED, not returned. The refusal is already the
// answer, and it is the one the caller acts on: returning an error in its place
// would report an outage instead of a budget the operator has to raise, for a
// write whose only cost when lost is a counter that reads low by this round.
func (m *meter) countRefused(ctx context.Context, tokens int, refusal coord.Spend) {
	if _, err := m.budgets.PostCharge(context.WithoutCancel(ctx), m.agentScope, tokens); err != nil {
		log.WarnContext(ctx, "budget_refused_round_uncounted", "scope", m.agentScope,
			"tokens", tokens, "refused_scope", refusal.RefusedScope, "error", err.Error(),
			"detail", "the round was billed and refused, and its tokens did not reach the "+
				"counter, which now reads low by them")
	}
}

// Room reports whether this seat's next model call may be sent, reading the
// counter and counting nothing.
//
// No room is a capped scope at or past its limit: the call's own charge would
// be refused whatever its size, and it would be billed before the refusal. The
// COMPANY is checked first, for the reason [coord.Budgets.Charge] reports it
// first: "the company is out" is the fact an operator acts on, and raising one
// seat's ceiling against an exhausted company changes nothing.
//
// Read off the spend alone, never off [coord.Usage.RefusedAt]. The stamp
// clears only on an admitted charge, so a room that consulted it would refuse
// a scope whose cap a later revision raised, and no charge could ever be
// admitted to clear it: the seat would stay stopped until an operator reset a
// counter that already had room.
func (m *meter) Room(ctx context.Context) (toolloop.SpendOutcome, error) {
	scopes, err := m.capped(ctx)
	if err != nil {
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget room: %w", err)
	}
	for _, s := range scopes {
		if s.used >= s.limit {
			return toolloop.SpendOutcome{Scope: s.name, Used: s.used, Limit: s.limit}, nil
		}
	}
	return toolloop.SpendOutcome{OK: true}, nil
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
	scopes, err := m.capped(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: budget headroom: %w", err)
	}
	headroom, capped := 0, false
	for _, s := range scopes {
		// TRACKED WITH A FLAG, not by testing headroom against zero: a
		// scope that has spent its whole allowance HAS zero headroom, and
		// reading that as "not set yet" would let the other scope's room
		// overwrite it — turning an exhausted company into an uncapped
		// one at exactly the moment the cap matters.
		if left := max(s.limit-s.used, 0); !capped || left < headroom {
			headroom, capped = left, true
		}
	}
	return headroom, nil
}

// cappedScope is one scope with a ceiling, and what it has spent.
type cappedScope struct {
	// name is what a refusal calls the scope: "org" or "agent", the words
	// [coord.Spend.RefusedScope] uses.
	name  string
	key   string
	limit int
	used  int
}

// capped reads the spend of every scope this seat is capped by, the company
// first. A limit of 0 is unlimited for that scope, matching the config, and is
// neither read nor returned.
func (m *meter) capped(ctx context.Context) ([]cappedScope, error) {
	var out []cappedScope
	for _, s := range []cappedScope{
		{name: string(types.BudgetScopeOrg), key: coord.OrgScope, limit: m.orgLimit},
		{name: string(types.BudgetScopeAgent), key: m.agentScope, limit: m.agentLimit},
	} {
		if s.limit <= 0 {
			continue
		}
		used, err := m.budgets.Used(ctx, s.key)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.key, err)
		}
		s.used = used
		out = append(out, s)
	}
	return out, nil
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
