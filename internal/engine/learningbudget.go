package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// THE AUXILIARY SPEND, and why it was invisible.
//
// `token_budget` was enforced in exactly two places: the turn loop
// (run.go's meterFor) and the coding sandbox. Every other completion this
// engine makes on a seat's behalf — the persist decider on every completed
// turn, the counterparty profiler, the episode-compaction summarizer —
// resolved a model through learning.Models and called Provider.Complete
// directly. That spend was never charged, so a company sitting at its
// ceiling kept paying for auxiliary work forever AND the fleet counter an
// operator reads understated what the company had actually spent.
//
// The fix is one wrapper at the SEAM rather than a charge call at each site.
// Every learning worker resolves its model through Models.Head; wrapping
// that is what makes a worker added later charge without anyone remembering
// to wire it. A charge call per site is the shape that let this happen.

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

// meteredProvider charges a completion's tokens after the call returns.
//
// AFTER, not before, and that asymmetry with the turn loop is deliberate:
// the loop knows a round's size before it spends because it is about to send
// a request it built, while an auxiliary pass is one shot whose cost is only
// known from the answer. Charging after means the LAST auxiliary call of a
// company's life can overshoot the ceiling by one completion; refusing to
// charge at all — which is what this build did — overshoots it forever.
type meteredProvider struct {
	inner llm.Provider
	meter toolloop.BudgetMeter
}

func (p meteredProvider) Model() string { return p.inner.Model() }

func (p meteredProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
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
			// coordination blip into a reflection outage, and the
			// pre-flight gate is what actually stops the spending.
			log.Warn("auxiliary_spend_uncounted", "error", spendErr,
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
		// A ZERO-TOKEN CHARGE, which is how the counter is asked "would
		// you refuse?" without moving it. Charging a probe amount would
		// make the question cost what it is asking about.
		outcome, err := m.Spend(ctx, 0)
		if err != nil {
			// UNKNOWN is not "no". A coordination blip must not silently
			// stop a company learning; the charge on the way out is what
			// keeps an unreachable counter from also being a free one.
			return true, err
		}
		return outcome.OK, nil
	}
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// meteredModelsFor is the seat-model seam every learning worker resolves
// through, with charging attached when the epoch has a ceiling to enforce.
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
