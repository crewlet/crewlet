package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// THE AUXILIARY SPEND, and why it was invisible.
//
// `token_budget` was enforced in exactly two places: the turn loop
// (run.go's meterFor) and the coding sandbox. Every other completion this
// engine makes on a seat's behalf — the persist decider on every completed
// turn, the counterparty profiler, the episode-compaction summarizer, and
// the turn-start prefetch's memory filter, knowledge query and episode
// summary on EVERY turn — resolved a model through a Models seam and called
// Provider.Complete directly. That spend was never charged, so a company
// sitting at its ceiling kept paying for auxiliary work forever AND the
// fleet counter an operator reads understated what the company had actually
// spent.
//
// The fix is one wrapper at the SEAM rather than a charge call at each site.
// Every learning worker and the prefetch resolve their model through
// Models.Head, handed [Engine.meteredModelsFor]; wrapping that is what makes
// a worker added later charge without anyone remembering to wire it. A
// charge call per site is the shape that let this happen. What is charged
// elsewhere: the turn loop's own rounds through meterFor's meter, the
// round-cap extension judge through that same meter (the runner charges it
// after the call), and a coding run when its spend is collected.

// meteredModels charges every completion a learning worker makes.
//
// It wraps the phase registry rather than replacing it, so what runs
// underneath is still the seat's own configured auxiliary chain — the
// wrapper decides nothing about WHICH model, only that the tokens are
// counted.
type meteredModels struct {
	inner  learningModels
	charge func(seat *org.Role) spendRecorder
}

// learningModels is the seam learning.Models describes, restated here so this
// file does not import the learning package to satisfy it.
type learningModels interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

// spendRecorder records tokens a completion has ALREADY spent, refusing
// nothing. See [coord.Budgets.PostCharge].
type spendRecorder interface {
	Record(ctx context.Context, tokens int) error
}

func (m meteredModels) Head(role *org.Role, ph phase.Phase) (chain.Member, error) {
	member, err := m.inner.Head(role, ph)
	if err != nil {
		return member, err
	}
	charge := m.charge(role)
	if charge == nil {
		// No counter to charge: no coordination store, or a role the epoch
		// does not name as an agent seat. A seat with no ceiling IS
		// charged — meterFor counts every seat, so an auxiliary pass is on
		// the window a ceiling set later will judge.
		return member, nil
	}
	member.Provider = meteredProvider{inner: member.Provider, meter: charge}
	return member, nil
}

// meteredProvider records a completion's tokens after the call returns.
//
// AFTER, as every charge in this engine is: a completion's size is known only
// from its answer, and the turn loop charges each of its rounds after the
// reply too. What differs is what waits on the charge. A round's verdict
// decides whether the tools it asked for run and whether another round
// starts, so the loop's charge judges it (and counts it either way); an
// auxiliary pass is one shot whose answer is already in hand, so nothing waits
// on a verdict and this records what happened without one. The PRE-FLIGHT
// GATE is [Engine.learningBudget]; this is the record.
//
// A RECORD, NEVER THE GATE. It used to put the spend through Charge, when a
// refused Charge counted nothing: a completion that took a window past its
// ceiling was refused there and recorded not at all, so the counter stayed
// below the ceiling, the pre-flight gate still read room, and the next pass
// ran and was refused its record in turn — a company at its ceiling paid for
// every reflection pass after it and its counter heard about none of them.
// Charge counts a refused round now, but it would still be the wrong verb: a
// refusal stamps the window as the gate turning a charge away and an
// admission clears every stamp, and neither is true of a pass nobody stopped.
// Spend that has happened is recorded whole, past the ceiling included, which
// is what makes the gate read "no room" afterwards.
type meteredProvider struct {
	inner llm.Provider
	meter spendRecorder
}

func (p meteredProvider) Model() string { return p.inner.Model() }

func (p meteredProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	completion, err := p.inner.Complete(ctx, req)
	if completion == nil {
		return completion, err
	}
	if tokens := completion.TotalTokens(); tokens > 0 {
		// context.WithoutCancel: the tokens are already spent at the
		// vendor. A record skipped because the caller's deadline expired
		// between the answer and the write is money the counter never
		// hears about — exactly the leak this file exists to close.
		if spendErr := p.meter.Record(context.WithoutCancel(ctx), tokens); spendErr != nil {
			// Logged, never propagated. The completion SUCCEEDED and the
			// caller's work is valid; failing it here would turn a
			// coordination blip into a reflection outage, and the
			// pre-flight gate is what actually stops the spending.
			log.WarnContext(ctx, "auxiliary_spend_uncounted", "error", spendErr,
				"tokens", tokens, "model", completion.Model,
				"detail", "the fleet counter now understates this company's spend")
		}
	}
	return completion, err
}

// seatSpend records one seat's auxiliary spend in the seat's counter and the
// company's, in the windows current when it is recorded.
type seatSpend struct {
	budgets interface {
		PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error)
	}
	agentScope string
	zone       *time.Location
	now        func() time.Time
}

func (s seatSpend) Record(ctx context.Context, tokens int) error {
	_, err := s.budgets.PostCharge(ctx, s.agentScope, tokens, coord.WindowsAt(s.now(), s.zone))
	if err != nil {
		return fmt.Errorf("engine: record auxiliary spend: %w", err)
	}
	return nil
}

// spendFor is the recorder for one seat's auxiliary spend, or nil where
// [Engine.meterFor] would build no meter: no coordination store, or a handle
// the epoch does not name as an agent seat. The same scope and clock as the
// seat's turns, so a pass and a round are counted in one window.
func (e *Engine) spendFor(c *Company, handle string) spendRecorder {
	m, ok := e.meterFor(c, handle).(*meter)
	if !ok || m == nil {
		return nil
	}
	return seatSpend{budgets: e.backends.Fleet, agentScope: m.agentScope,
		zone: m.basis.zone, now: m.now}
}

// learningBudget is the reflection pass's pre-flight gate.
//
// Reflection is best effort, so it does not FAIL on an exhausted budget — it
// declines to start. That distinction is the whole point: a pass that runs
// and fails has already made its auxiliary calls.
//
// IT ASKS FOR THE HEADROOM, which is a read and moves nothing. It used to ask
// with a charge of zero tokens, and the counter answers every such charge OK
// without looking — a phase whose provider reported no usage still ran, and
// refusing it would stop a company over a backend that omits the field — so
// the gate had never declined a pass: a company at its ceiling went on
// starting reflection passes, and paying for their auxiliary calls, until
// each one's first charge was refused mid-pass.
func (e *Engine) learningBudget(c *Company) func(context.Context, *org.Role) (bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return func(ctx context.Context, seat *org.Role) (bool, error) {
		headroom := e.remainingFor(c, seatHandle(seat))
		if headroom == nil {
			// Nothing in the epoch caps this seat's spend.
			return true, nil
		}
		left, err := headroom.Remaining(ctx)
		if err != nil {
			// UNKNOWN is not "no". A coordination blip must not silently
			// stop a company learning; the charge on the way out is what
			// keeps an unreachable counter from also being a free one.
			return true, err
		}
		// A capped window with nothing left refuses the next token, so
		// no pass that needs one may start.
		return left > 0, nil
	}
}

// seatHandle is a seat's handle, tolerating the nil the gate may be handed.
func seatHandle(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Handle()
}

// meteredModelsFor is the seat-model seam every learning worker and the
// turn-start prefetch resolve through, with charging attached wherever there
// is a fleet to count on.
func (e *Engine) meteredModelsFor(c *Company) learningModels {
	if c == nil || c.Models == nil {
		return nil
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return c.Models
	}
	return meteredModels{
		inner:  c.Models,
		charge: func(seat *org.Role) spendRecorder { return e.spendFor(c, seatHandle(seat)) },
	}
}

// compactorFor is the compactor a turn of this epoch rewrites over-budget
// text with: the seat's own auxiliary chain through the metered seam every
// learning worker uses — so a rewrite is charged to the seat's token windows
// like any other auxiliary call — over the engine's one cache.
//
// Built per call rather than held on the epoch, because it is two pointers:
// what makes it worth keeping is the cache, and that is the engine's.
func (e *Engine) compactorFor(c *Company) *compact.Compactor {
	models := e.meteredModelsFor(c)
	if models == nil {
		return nil
	}
	return compact.New(models, e.rewrites)
}

// seatCompactor is the epoch's compactor bound to one seat — the zero
// [compact.Bound], which rewrites nothing, where the handle names no agent
// seat or the company has no models.
func (e *Engine) seatCompactor(c *Company, handle string) compact.Bound {
	if c == nil || c.Org == nil {
		return compact.Bound{}
	}
	return e.compactorFor(c).For(c.Org.AgentSeatByHandle(handle))
}

// conversationBlock is the prior turns of this conversation as a turn is
// given them: the newest whole, and the older ones that will not fit
// [ledger.InjectedMaxChars] condensed by the seat's auxiliary model into one
// account — or, where none can be had, left out and counted.
//
// A REWRITE WHERE A DROP WAS. The block used to leave its oldest entries out,
// and the deliveries they recorded with them, so a seat on a long thread was
// told it had said nothing it could not see. The rewrite keeps them, in fewer
// words; the drop is now the fallback, and still says so.
func (e *Engine) conversationBlock(ctx context.Context, c *Company, handle string,
	history []ledger.Session,
) string {
	opts := ledger.HistoryOptions{MaxChars: ledger.InjectedMaxChars}
	if overflow, _ := ledger.SplitHistory(history, opts.MaxChars); len(overflow) > 0 {
		res, err := e.seatCompactor(c, handle).Fit(ctx, compact.KindConversation,
			ledger.RenderSessions(overflow), opts.MaxChars/ledger.EarlierShare)
		switch {
		case err == nil && !res.Compacted:
			// THE OLDER TURNS FIT THEIR SHARE AS THEY ARE — the newest
			// entry alone was what pushed the block over — so every entry
			// renders verbatim. Labelling them condensed would be a lie
			// about text nobody rewrote.
			opts.MaxChars = 0
		case err == nil:
			opts.Earlier = res.Text
		default:
			log.WarnContext(ctx, "conversation_history_not_condensed", "seat", handle,
				"turns", len(overflow), "error", err.Error(),
				"detail", "the older turns are left out of this turn's block, and it says how many")
		}
	}
	return ledger.RenderHistory(history, opts)
}
