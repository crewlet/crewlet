package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// Enforcing the token budget.
//
// The seam existed and nothing supplied it: runner.Config.Budget was nil on
// every turn, so a company with a `token_budget:` spent without limit and the
// number in its config was decoration. Money leaves the building for every
// token, which is why this fails CLOSED — a counter that cannot be reached
// stops the round rather than silently un-capping the company.
//
// CAPS AND THE CLOCK ARE READ OFF THE EPOCH, usage off the fleet's shared
// counters, and the split is the design: a revision that raises a ceiling or
// moves the company's clock takes effect on the next turn (both travel in on
// every call), while each window's counter has to be one number across the
// fleet or N nodes each spend the whole allowance — which is exactly what a
// counter on the node's own database was. The windows a round is charged in
// are cut at the moment it is charged, on the clock of the epoch the turn is
// pinned to, so a turn that runs across midnight charges its later rounds to
// the new day (ADR-0019).
//
// EVERY SEAT IS COUNTED, a seat nothing caps included ([Engine.meterFor]), so a
// ceiling set mid-window judges the spend the window already holds. And a seat
// whose capped window refuses is not handed work it cannot run: it is PARKED
// on its inbox until the window turns over (budgetpark.go).

// budgetCounter is the slice of the fleet's counters a meter calls.
//
// Declared here, by the consumer: a turn's meter charges and reads, and a
// meter that could reach the whole of [coord.Budgets] would one day be given
// a reason to post-charge.
type budgetCounter interface {
	Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error)
	Used(ctx context.Context, scope string, windows coord.Windows) (coord.Usage, error)
}

// budgetBasis is what one epoch says a seat's spend is judged by: the
// company's ceilings, the seat's own, and the clock their windows are cut on.
type budgetBasis struct {
	org, seat coord.Caps
	zone      *time.Location
}

// basisOf reads a seat's budget basis off one epoch.
//
// The ROLE's ceilings, not the unit's: a unit budget would need a third
// counter scope and a rule for which of three caps a refusal names, and no
// config field declares one. Stated because the absence looks like an
// oversight otherwise.
func basisOf(c *Company, seat *org.Role) budgetBasis {
	b := budgetBasis{zone: time.UTC}
	if c == nil {
		return b
	}
	if c.Config != nil {
		b.zone = c.Config.Location()
	}
	if c.Org != nil {
		b.org = coord.Caps(c.Org.TokenBudget)
	}
	if seat != nil {
		b.seat = coord.Caps(seat.TokenBudget)
	}
	return b
}

// capped reports whether the basis caps any window of either scope.
func (b budgetBasis) capped() bool { return len(b.org) > 0 || len(b.seat) > 0 }

// meter charges one seat's rounds against the shared counters.
//
// Per turn, holding the caps and the clock the turn was PINNED to — so a
// mid-turn config change cannot move the ceiling a round is judged against or
// the day it is counted in, which is the same rule every other epoch read
// follows. What it does not pin is the instant: each round is charged to the
// windows current when it is charged.
type meter struct {
	budgets    budgetCounter
	agentScope string
	basis      budgetBasis
	now        func() time.Time
}

var _ toolloop.BudgetMeter = (*meter)(nil)

// windows is the day, week and month this instant falls in on the pinned
// clock.
func (m *meter) windows() coord.Windows {
	return coord.WindowsAt(m.now(), m.basis.zone)
}

// Spend checks and increments in ONE operation. See coord.Budgets.Charge.
func (m *meter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	got, err := m.budgets.Charge(ctx, coord.ChargeRequest{
		Seat: m.agentScope, Tokens: tokens, Windows: m.windows(),
		OrgCaps: m.basis.org, SeatCaps: m.basis.seat,
	})
	if err != nil {
		// NOT a refusal. The caller must tell "the company is out of
		// tokens" from "the counter is unreachable": the first is a
		// budget event an operator acts on, the second is an outage.
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	if !got.OK {
		return toolloop.SpendOutcome{
			Scope: got.RefusedScope, Used: got.RefusedUsed, Limit: got.RefusedLimit,
			Period: got.RefusedPeriod, Window: got.RefusedWindow.Label,
			ResetsAt: got.RefusedWindow.End,
		}, nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

// Remaining is this seat's headroom, in tokens: the least room left in any
// capped window of either scope.
//
// THREE-VALUED, and the third value is the whole reason this is not an int.
// [subagent.Config.ParentRemaining] reads ZERO AS UNCAPPED, so a counter that
// answered 0 for "I could not reach the store" would hand a fan-out no ceiling
// at all — the fail-OPEN direction, on the one path where money leaves the
// building per token. The error travels, and the caller refuses the spawn.
//
// The TIGHTEST window of both scopes, because a charge is admitted only while
// every capped window of both has room: a seat with a week to spare under its
// own cap but nothing left in the company's day has no room.
//
// A meter over a basis that caps nothing answers 0 with no counter read, which
// is the "no ceiling" [runner.Remaining]'s readers take a zero for — but they
// never ask it: [Engine.remainingFor] hands them no reader at all for such a
// seat, so a zero with a nil error from a reader they ARE handed always means
// an exhausted window.
func (m *meter) Remaining(ctx context.Context) (int, error) {
	windows := m.windows()
	headroom, capped := 0, false
	for _, scope := range []struct {
		key  string
		caps coord.Caps
	}{
		{coord.OrgScope, m.basis.org},
		{m.agentScope, m.basis.seat},
	} {
		if len(scope.caps) == 0 {
			continue
		}
		usage, err := m.budgets.Used(ctx, scope.key, windows)
		if err != nil {
			return 0, fmt.Errorf("engine: budget headroom for %s: %w", scope.key, err)
		}
		for p, ceiling := range scope.caps {
			// TRACKED WITH A FLAG, not by testing headroom against
			// zero: a window that has spent its whole allowance HAS
			// zero headroom, and reading that as "not set yet" would
			// let another window's room overwrite it — turning an
			// exhausted company into an uncapped one at exactly the
			// moment the cap matters.
			if left := max(ceiling-usage.In(p).Used, 0); !capped || left < headroom {
				headroom, capped = left, true
			}
		}
	}
	return headroom, nil
}

// refusal is the capped window a scope is waiting out, as [Engine.budgetPark]
// parks a seat on it.
type refusal struct {
	// Scope is the counter that refuses, coord.OrgScope or the seat's own.
	Scope string

	// Window is the refusing window, cut on the pinned clock. Its End is
	// when the scope next has room without a ceiling being raised.
	Window period.Window
	Used   int
	Limit  int
}

// refusing reports the capped window of either scope that turns the seat's
// next charge away, if any.
//
// A window REFUSES when the gate has said so — its refusal stamp is set, and
// only an admitted charge or the window turning over clears one — or when it
// has no room left for a single token, which the gate would refuse on the next
// charge whatever its size. Where several windows refuse it is the one that
// ENDS LAST, across every capped window of both scopes, by [coord.Outlasts] —
// the tie-break the counter's own refusal names its window by: the seat can run
// nothing until that one turns over, and naming an earlier one would wake it
// into a refusal. Every window rather than only the stamped ones, because a
// full window that ends later may carry no stamp yet — a month a collected
// coding run post-charged past its ceiling carries none until a charge is
// refused against it.
//
// THREE-VALUED. An unreachable counter is an error, never "not refusing" and
// never "refusing": the caller decides what an unknown answer is worth, and
// for the park the answer is to let the turn run, because its own meter is the
// gate and fails closed.
func (m *meter) refusing(ctx context.Context) (refusal, bool, error) {
	windows := m.windows()
	var out refusal
	found := false
	for _, scope := range []struct {
		key  string
		caps coord.Caps
	}{
		{coord.OrgScope, m.basis.org},
		{m.agentScope, m.basis.seat},
	} {
		if len(scope.caps) == 0 {
			continue
		}
		usage, err := m.budgets.Used(ctx, scope.key, windows)
		if err != nil {
			return refusal{}, false, fmt.Errorf("engine: budget windows of %s: %w", scope.key, err)
		}
		for p, ceiling := range scope.caps {
			slot := usage.In(p)
			if !windowRefuses(slot, ceiling) {
				continue
			}
			if !found || coord.Outlasts(slot.Window, out.Window) {
				out = refusal{Scope: scope.key, Window: slot.Window, Used: slot.Used, Limit: ceiling}
				found = true
			}
		}
	}
	return out, found, nil
}

// remainingFor is the seat's headroom reader, or nil.
//
// Nil where the seat has no ceiling in any window of either scope, and where
// there is no meter at all: with no ceiling there is no headroom to read, and
// the spawner and the sandbox's floor treat nil as uncapped — which is exactly
// what the seat is. The meter itself still COUNTS such a seat ([Engine.meterFor]);
// what it has no answer for is how much room is left under a ceiling nobody
// set.
//
// A NIL INTERFACE, never an interface holding a nil *meter: the spawner and
// the sandbox's floor both test the reader against nil to mean "uncapped", and
// a typed nil passes that test and panics on its first read.
func (e *Engine) remainingFor(c *Company, handle string) runner.Remaining {
	m, ok := e.meterFor(c, handle).(*meter)
	if !ok || m == nil || !m.basis.capped() {
		return nil
	}
	return m
}

// meterFor builds the meter for one seat's turn, or nil.
//
// EVERY SEAT IS COUNTED, capped or not, wherever there is a fleet to count on.
// Nil only with no coordination store, or for a handle the epoch does not name
// as an agent seat — there is then no shared counter or no scope to charge.
//
// It used to be nil for a company with no ceiling too, on the argument that a
// meter answering "yes" to every round is a round trip with no question behind
// it. The question was the COUNT. A ceiling added mid-window then started from
// zero, because nothing had counted the window before it existed, so a company
// that capped its day at noon was handed the whole day again on top of what
// the morning had already spent; and every reader of the counters — the budgets
// answer, the live meter, `crewlet budgets` — showed an uncapped company as
// having spent nothing at all. A charge with no caps is admitted by the counter
// without a refusal to decide, and the round trip is the price of a figure that
// is true when somebody sets a ceiling on it.
//
// The basis — the company's ceilings, the seat's own and the clock the windows
// are cut on — is the epoch's c, which is the one the turn was PINNED to.
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
	return &meter{
		budgets: e.backends.Fleet, agentScope: coord.AgentScope(agentID.String()),
		basis: basisOf(c, seat), now: time.Now,
	}
}

// rfc3339 is an instant as RFC 3339 in UTC, the wire's spelling of one, and
// "" for the zero time — a refusal with no calendar window, a sub-agent's own
// slice, has no reset to name.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
