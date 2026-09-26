package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE AUXILIARY SPEND.
//
// A learning worker, a background learning pass and the turn-start prefetch
// make their completions outside the turn loop: each resolves a model through
// [learningModels.Head] and calls Provider.Complete itself, so nothing the loop
// does to a round reaches them. Their spend is RECORDED and CHARGED at that
// SEAM instead — one wrapper around the resolution rather than a record and a
// charge at each call site — which is what records and charges a worker added
// later without anyone remembering to wire it.
//
// THE RECORD IS UNCONDITIONAL and the charge is not. Every completion publishes
// an [types.AuxiliaryCallCompleted], because what a company spent is a question
// with an answer whether or not it set a ceiling, and the spend rollups can
// fold only what was recorded. The budget half runs only where a ceiling
// exists: an unlimited company pays no round trip per auxiliary call to be told
// "yes".

// meteredModels records every completion made through a model it resolved,
// and charges it where the epoch has a ceiling to enforce.
//
// It wraps the phase registry rather than replacing it, so what runs
// underneath is still the seat's own configured auxiliary chain — the wrapper
// decides nothing about WHICH model, only that the tokens are recorded and
// counted.
type meteredModels struct {
	inner learningModels

	// charge is the seat's budget meter, and itself nil where the engine
	// has no fleet counter to charge; the meter it returns is nil for a
	// seat with no ceiling anywhere (see [Engine.meterFor]).
	charge func(seat *org.Role) toolloop.BudgetMeter

	// record publishes one completion's spend record. Nil publishes none,
	// which is the unit tests' wrapper and nothing else.
	record func(ctx context.Context, seat *org.Role, spend types.AuxiliaryCallCompleted)

	// who is whom the completions this resolves serve: set by
	// [meteredModels.For], and empty on the wrapper a caller that names
	// nobody resolves through.
	who learning.Attribution
}

// learningModels is the seam learning.Models describes, restated here so this
// file does not tie the engine's wiring to the learning package's name for it.
type learningModels interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

var _ learning.Attributing = meteredModels{}

// For binds whom the completions resolved through the result serve — see
// [learning.Attribution]. It resolves exactly as m does.
func (m meteredModels) For(who learning.Attribution) learning.Models {
	m.who = who
	return m
}

func (m meteredModels) Head(role *org.Role, ph phase.Phase) (chain.Member, error) {
	member, err := m.inner.Head(role, ph)
	if err != nil {
		return member, err
	}
	var meter toolloop.BudgetMeter
	if m.charge != nil {
		meter = m.charge(role)
	}
	member.Provider = meteredProvider{
		inner: member.Provider, key: member.Key, phase: ph, seat: role,
		meter: meter, record: m.record, who: m.who,
	}
	return member, nil
}

// meteredProvider records each completion's spend, and — where the seat has a
// meter — reads the budget's room before a call and charges the call's tokens
// after it returns.
//
// The same two questions the turn loop asks of every round, for the same
// reason: a call's size is known only from its answer, so the charge comes
// after it, and a budget already at its cap is read for room BEFORE the call is
// sent rather than discovered by the charge of a call already billed. What an
// exhausted company can still spend on auxiliary work is then the calls already
// in flight when its counter reached the cap: a call started after that, on a
// counter that can be read, finds no room and is never sent.
type meteredProvider struct {
	inner  llm.Provider
	key    string
	phase  phase.Phase
	seat   *org.Role
	meter  toolloop.BudgetMeter
	record func(ctx context.Context, seat *org.Role, spend types.AuxiliaryCallCompleted)
	who    learning.Attribution
}

func (p meteredProvider) Model() string { return p.inner.Model() }

func (p meteredProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if p.meter != nil {
		room, roomErr := p.meter.Room(ctx)
		switch {
		case roomErr != nil:
			// UNKNOWN IS NOT "NO" here, as it is not at the reflection
			// pass's gate: learning is best effort, and a coordination
			// blip must not silently stop a company learning. The charge
			// on the way out is what keeps an unreadable counter from also
			// being a free one.
			log.WarnContext(ctx, "auxiliary_budget_unreadable", "error", roomErr,
				"model", p.inner.Model(),
				"detail", "the call is sent without knowing whether the budget has room")
		case !room.OK:
			return nil, fmt.Errorf("engine: auxiliary call on %s not sent: %w",
				p.inner.Model(), toolloop.Refusal(room))
		}
	}
	began := time.Now()
	completion, err := p.inner.Complete(ctx, req)
	if completion == nil {
		// Nothing answered, so nothing reported a spend to record or to
		// charge: a figure invented for a call that produced nothing would
		// be spend nobody billed.
		return completion, err
	}
	p.recordSpend(ctx, completion, time.Since(began))
	if tokens := completion.TotalTokens(); tokens > 0 && p.meter != nil {
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

// recordSpend publishes the completion's spend record.
//
// The model is the one the completion reported serving it, and the configured
// model of the provider that served it where it named none — the rule the turn
// loop's own per-model split follows (see [toolloop.ModelTokens]).
func (p meteredProvider) recordSpend(ctx context.Context, completion *llm.Completion, took time.Duration) {
	if p.record == nil || p.seat == nil {
		return
	}
	model := completion.Model
	if model == "" {
		model = p.inner.Model()
	}
	p.record(ctx, p.seat, types.AuxiliaryCallCompleted{
		RoleName: p.seat.Name,
		TurnID:   p.who.TurnID, WorkKey: p.who.WorkKey,
		Phase:  types.Phase(p.phase),
		Worker: p.who.Worker,
		Model:  model, ProviderKey: p.key,
		InputTokens: completion.InputTokens, OutputTokens: completion.OutputTokens,
		TotalTokens: completion.TotalTokens(),
		DurationMS:  int(took / time.Millisecond),
	})
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

// meteredModelsFor is the seat-model seam every auxiliary caller resolves
// through: the learning workers, the background passes and the turn-start
// prefetch (prefetch.go). Every completion made through it is recorded, and
// charged where the epoch has a ceiling to enforce.
//
// Nil for a company with no model registry, which is a valid company (see
// nomodels.go) whose auxiliary callers are waiting for a provider.
func (e *Engine) meteredModelsFor(c *Company) learningModels {
	m, ok := e.auxiliaryModels(c)
	if !ok {
		return nil
	}
	return m
}

// auxiliaryModelsFor is [Engine.meteredModelsFor] with the attribution bound —
// the form a caller that knows whom it serves resolves through. Nil where
// meteredModelsFor is.
func (e *Engine) auxiliaryModelsFor(c *Company, who learning.Attribution) learningModels {
	m, ok := e.auxiliaryModels(c)
	if !ok {
		return nil
	}
	return m.For(who)
}

// auxiliaryModels builds the wrapper, and reports false for a company with no
// model registry to wrap.
func (e *Engine) auxiliaryModels(c *Company) (meteredModels, bool) {
	if c == nil || c.Models == nil {
		return meteredModels{}, false
	}
	m := meteredModels{inner: c.Models, record: e.auxiliaryRecorder(c)}
	if e.backends != nil && e.backends.Fleet != nil {
		m.charge = func(seat *org.Role) toolloop.BudgetMeter { return e.meterFor(c, seatHandle(seat)) }
	}
	return m, true
}

// auxiliaryRecorder publishes an auxiliary completion's spend record for c's
// seats, or is nil on a node with no broker to publish to.
//
// The seat's agent id is read off the company the model was resolved for, so
// the record names the instance that epoch named. A seat the organization
// gives no agent id keeps its role alone, which is what every rollup keys a
// seat on.
func (e *Engine) auxiliaryRecorder(c *Company) func(context.Context, *org.Role, types.AuxiliaryCallCompleted) {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return func(ctx context.Context, seat *org.Role, spend types.AuxiliaryCallCompleted) {
		if c.Org != nil {
			if id, ok := c.Org.AgentIDFor(seat); ok {
				spend.Agent = id.String()
			}
		}
		e.publishEvent(ctx, events.New(spend, tracing.TraceOf(ctx)), seat.Name)
	}
}
