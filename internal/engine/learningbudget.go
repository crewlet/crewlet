package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The PRE-FLIGHT half of auxiliary spend: whether an optional pass may start.
// The record half, charging each auxiliary completion, is in auxiliary.go.

// auxGate is the PRE-FLIGHT gate every optional auxiliary pass asks before its
// first model call ([learning.BudgetGate]): reflection, episode compaction,
// clustered synthesis and skill promotion, each counting its declines under
// its pass ([metrics.AuxiliaryDeclined]). It reads the windows and spends
// nothing: a zero-token charge is answered OK unread, so it would never decline.
//
// NOT ASKED BY the provider probe (providerpark.go), the one background call
// that runs whatever the budget says: it is how a model entry parked at its
// usage limit is found usable again, so gating it would keep a seat whose own
// window filled parked past the window's turn.
func (e *Engine) auxGate(c *Company, pass string) learning.BudgetGate {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return func(ctx context.Context, handle string) (bool, error) {
		ok, err := e.auxRoom(ctx, c, handle)
		switch {
		case err != nil:
			e.metrics.Add(metrics.AuxiliaryDeclined, 1, metrics.Attrs{"pass": pass, "reason": learning.SkipBudgetUnknown})
		case !ok:
			e.metrics.Add(metrics.AuxiliaryDeclined, 1, metrics.Attrs{"pass": pass, "reason": learning.SkipNoBudget})
		}
		return ok, err
	}
}

// auxRoom is whether the seat named by handle may start optional auxiliary
// work now ([Engine.auxGate]): every capped window of the company and, for an
// agent seat, of the seat has room ([budgetMeter]).
//
// Three-valued: (true, nil) where nothing caps it or every capped window has
// room, (false, nil) where one has none, and (false, err) where the counter
// could not be read, which the caller treats as no room ([learning.BudgetGate]).
// The rule is the park's ([meter.refusing]). A no is recorded as the gate's
// refusal ([meter.turnAway]): the pass asks just before the work and makes no
// call on false, so the window's `refused_at` shows whose passes are declined.
func (e *Engine) auxRoom(ctx context.Context, c *Company, handle string) (bool, error) {
	m := e.budgetMeter(c, handle)
	if m == nil || !m.basis.capped() {
		return true, nil
	}
	r, refusing, err := m.refusing(ctx)
	if err != nil {
		return false, err
	}
	if refusing {
		m.turnAway(ctx, r)
		return false, nil
	}
	return true, nil
}

// budgetMeter is the meter whose windows bound work done for handle: an agent
// seat's ([Engine.meterFor]: its windows and the company's), and the company's
// alone for anyone else, a person whose unit a promotion runs for among them,
// whose spend the company's windows carry.
func (e *Engine) budgetMeter(c *Company, handle string) *meter {
	if m := e.meterFor(c, handle); m != nil {
		return m
	}
	if e.backends == nil || e.backends.Fleet == nil || c == nil {
		return nil
	}
	return &meter{budgets: e.budgets(), basis: basisOf(c, nil), now: e.now}
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}
