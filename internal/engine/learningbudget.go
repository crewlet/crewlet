package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// THE AUXILIARY SPEND.
//
// A learning worker makes its completions outside the turn loop: it resolves a
// model through [learningModels.Head] and calls Provider.Complete itself, so
// nothing the loop does to a round reaches it. The budget is enforced at that
// SEAM instead — one wrapper around the resolution rather than a charge call at
// each site — which is what charges a worker added later without anyone
// remembering to wire it.

// meteredModels charges every completion a learning worker makes.
//
// It wraps the phase registry rather than replacing it, so what runs
// underneath is still the seat's own configured auxiliary chain — the
// wrapper decides nothing about WHICH model, only that the tokens are
// counted.
type meteredModels struct {
	inner  learningModels
	charge func(seat *org.Role) toolloop.BudgetMeter
}

// learningModels is the seam learning.Models describes, restated here so this
// file does not import the learning package to satisfy it.
type learningModels interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

func (m meteredModels) Head(role *org.Role, ph phase.Phase) (chain.Member, error) {
	member, err := m.inner.Head(role, ph)
	if err != nil {
		return member, err
	}
	charge := m.charge(role)
	if charge == nil {
		// No ceiling anywhere in the epoch, or no coordination store. The
		// unwrapped member, so an unlimited company pays no round trip per
		// auxiliary call to be told "yes" — the same reason meterFor
		// returns nil rather than an always-allow meter.
		return member, nil
	}
	member.Provider = meteredProvider{inner: member.Provider, meter: charge}
	return member, nil
}

// meteredProvider reads the budget's room before a call and charges the call's
// tokens after it returns.
//
// The same two questions the turn loop asks of every round, for the same
// reason: a call's size is known only from its answer, so the charge comes
// after it, and a budget already at its cap is read for room BEFORE the call is
// sent rather than discovered by the charge of a call already billed. What an
// exhausted company can still spend on auxiliary work is then the calls already
// in flight when its counter reached the cap: a call started after that, on a
// counter that can be read, finds no room and is never sent.
type meteredProvider struct {
	inner llm.Provider
	meter toolloop.BudgetMeter
}

func (p meteredProvider) Model() string { return p.inner.Model() }

func (p meteredProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	room, roomErr := p.meter.Room(ctx)
	switch {
	case roomErr != nil:
		// UNKNOWN IS NOT "NO" here, as it is not at the reflection pass's
		// gate: learning is best effort, and a coordination blip must not
		// silently stop a company learning. The charge on the way out is
		// what keeps an unreadable counter from also being a free one.
		log.WarnContext(ctx, "auxiliary_budget_unreadable", "error", roomErr,
			"model", p.inner.Model(),
			"detail", "the call is sent without knowing whether the budget has room")
	case !room.OK:
		return nil, fmt.Errorf("engine: auxiliary call on %s not sent: %w",
			p.inner.Model(), toolloop.Refusal(room))
	}
	completion, err := p.inner.Complete(ctx, req)
	if completion == nil {
		return completion, err
	}
	if tokens := completion.TotalTokens(); tokens > 0 {
		// context.WithoutCancel: the tokens are already spent at the
		// vendor. A charge skipped because the caller's deadline expired
		// between the answer and the write is money the counter never
		// hears about — exactly the leak this file exists to close.
		if _, spendErr := p.meter.Spend(context.WithoutCancel(ctx), tokens); spendErr != nil {
			// Logged, never propagated. The completion SUCCEEDED and the
			// caller's work is valid; failing it here would turn a
			// coordination blip into a reflection outage, and the room read
			// before the next call is what actually stops the spending.
			log.WarnContext(ctx, "auxiliary_spend_uncounted", "error", spendErr,
				"tokens", tokens, "model", completion.Model,
				"detail", "the fleet counter now understates this company's spend")
		}
	}
	return completion, err
}

// learningBudget is the reflection pass's pre-flight gate.
//
// Reflection is best effort, so it does not FAIL on an exhausted budget — it
// declines to start. That distinction is the whole point: a pass that runs
// and fails has already made its auxiliary calls.
func (e *Engine) learningBudget(c *Company) func(context.Context, *org.Role) (bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return func(ctx context.Context, seat *org.Role) (bool, error) {
		m := e.meterFor(c, seatHandle(seat))
		if m == nil {
			return true, nil
		}
		// A READ OF THE ROOM, which asks "is any cap already reached?"
		// without moving the counter. A charge cannot ask it: a charge of
		// nothing is admitted without being checked, whatever the counter
		// says, and a charge of anything more would make the question cost
		// what it is asking about.
		room, err := m.Room(ctx)
		if err != nil {
			// UNKNOWN is not "no". A coordination blip must not silently
			// stop a company learning; the charge on the way out is what
			// keeps an unreachable counter from also being a free one.
			return true, err
		}
		return room.OK, nil
	}
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// meteredModelsFor is the seat-model seam the learning workers and the
// turn-start prefetch (prefetch.go) resolve through, with charging attached
// when the epoch has a ceiling to enforce.
func (e *Engine) meteredModelsFor(c *Company) learningModels {
	if c == nil || c.Models == nil {
		return nil
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return c.Models
	}
	return meteredModels{
		inner:  c.Models,
		charge: func(seat *org.Role) toolloop.BudgetMeter { return e.meterFor(c, seatHandle(seat)) },
	}
}
