package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE AUXILIARY SEAM, and the two things it does.
//
// Every completion this engine makes for a seat — or for a person — that is not
// a phase of a turn runs on the seat's AUXILIARY chain (`llm_auxiliary`): the
// turn-start memory filter, knowledge query and episode summary, every rewrite
// a compaction makes, the reflection workers after a turn, the background
// learning passes, and a person's question answered on the operator surface.
// Every one of them resolves its model through ONE seam,
// [auxiliarySeam.Auxiliary], and the seam does what none of them should do for
// itself:
//
//   - it CHARGES each completion to the fleet's token counters after the call,
//     whatever its caller makes of the answer — an agent seat's spend to the
//     seat and the company, anything else (a person's question, a pass on a
//     unit a person leads) to the company alone;
//   - it RECORDS each completion in this node's auxiliary-spend ledger
//     (internal/auxspend) under the attribution its caller states, so every
//     spend figure read from events — the usage history, the live rollup, the
//     turn list, a turn's page, a task's spend — holds what the counters hold.
//
// ONE SEAM rather than a charge call and a record call at each site, because a
// call per site is the shape that let this spend go uncounted: the turn loop
// and the sandbox charged, and every other completion resolved a model and
// called Provider.Complete directly. Wrapping the seam is what makes a caller
// added later charge and record without anyone remembering to — and the
// attribution in its signature is what makes it impossible to call without
// saying whose cost the call is. What is charged elsewhere: the turn loop's
// own rounds through meterFor's meter, the round-cap extension judge and a
// delegated worker's rounds through that same meter, and a coding run when its
// spend is collected — each of which reaches its event as a phase record.
//
// A CALL A TURN MAKES IS CHARGED THROUGH THAT SAME METER, which its
// attribution carries ([auxspend.Use.Budget]): the counters are the seat's and
// the company's either way, but only the turn's meter remembers what their
// answer said, and the turn's next call asks it. See [recordedProvider].
//
// # The record does not wait on the counter
//
// The counter exists only where a coordination store does, so a call with no
// counter is still recorded, and one whose charge FAILED is still recorded: the
// counter then understates, and the rollups do not. A failed CALL is recorded
// too, with whatever tokens it reported — a provider that bills a timed-out
// request is spend like any other, and a figure that dropped it would hide a
// model that times out.
//
// # The day is the charge's
//
// The instant a completion returned is read ONCE, and the counter's windows and
// the record's company day are both cut from it, so a call that returned a
// moment before midnight is on the same day in both.

// phaseHeads is the phase registry's resolution as the seam reads it: the head
// of a seat's chain for a phase. *phase.Registry satisfies it as written, so
// which model answers a seat's cheap questions is still decided in one place —
// the one the config validator checks.
type phaseHeads interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

// auxiliaryModels is the seam's own method, which every consumer's Models
// interface (learning, prefetch, compact, builtin's answer) is spelled as.
// Declared here so [Engine.auxiliaryFor] can return a NIL interface for a
// company with no models — a nil *auxiliarySeam inside a non-nil interface is
// a seam its callers cannot tell from a working one.
type auxiliaryModels interface {
	Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error)
}

// spendRecorder records tokens a completion has ALREADY spent, refusing
// nothing, in the windows current at the instant given (see
// [coord.Budgets.PostCharge]).
type spendRecorder interface {
	Record(ctx context.Context, tokens int, at time.Time) error
}

// auxiliarySeam is one epoch's auxiliary seam: its registry, its org — for the
// derived agent ids a record is filed under — and its clock, with this node's
// ledger and the fleet's counters behind it.
type auxiliarySeam struct {
	heads  phaseHeads
	org    *org.Organization
	zone   *time.Location
	ledger *auxspend.Ledger
	// charge is the counter one call is recorded in, or nil where there is
	// none — no coordination store.
	charge func(role *org.Role, stage types.AuxStage) spendRecorder
	now    func() time.Time
}

// Auxiliary resolves the seat's auxiliary chain head, wrapped so every
// completion made through it is charged and recorded under use.
//
// AN ATTRIBUTION THE RECORD CANNOT BE FILED UNDER IS REFUSED here, before a
// model is resolved: spend filed under no stage or no purpose is spend every
// breakdown would show as nothing in particular, and this is the one place
// that can stop it rather than count it.
func (s auxiliarySeam) Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error) {
	if err := use.Validate(); err != nil {
		return chain.Member{}, err
	}
	member, err := s.heads.Head(role, phase.Auxiliary)
	if err != nil {
		return member, err
	}
	var meter spendRecorder
	switch {
	case use.Budget != nil:
		// THE TURN'S OWN METER, for a call the turn makes: the same
		// counters the seat's bare recorder would reach, through the one
		// frame that remembers what their answer said — see
		// [auxspend.Budget].
		meter = use.Budget
	case s.charge != nil:
		meter = s.charge(role, use.Stage)
	}
	member.Provider = recordedProvider{
		inner: member.Provider, key: member.Key, use: use, seat: s.seatOf(role, use.Stage),
		meter: meter, ledger: s.ledger, zone: s.zone, now: s.now,
	}
	return member, nil
}

// seatOf is whose spend a call is, as its record names it.
//
// AN AGENT SEAT'S, by its derived id, except on the operator stage; anything
// else is a PERSON's — the human seat a person's credential is bound to, or the
// one leading a unit whose background pass runs on its chain. A human seat has
// no agent id and no seat budget, and the record's `role` is what the live
// projection keys a SEAT on, so a person is named on its own two keys instead.
func (s auxiliarySeam) seatOf(role *org.Role, stage types.AuxStage) auxspend.Seat {
	if role == nil {
		return auxspend.Seat{}
	}
	if stage != types.AuxStageOperator && s.org != nil {
		if id, ok := s.org.AgentIDFor(role); ok {
			return auxspend.Seat{AgentID: id.String(), Handle: role.Handle(), Role: role.Name}
		}
	}
	return auxspend.Seat{Person: role.Handle(), PersonRole: role.Name}
}

// recordedProvider charges and records every completion after it returns.
//
// AFTER, as every charge in this engine is: a completion's size is known only
// from its answer. And a RECORD, NEVER A VERDICT: spend that has happened is
// recorded whole, past a ceiling included, which is what makes every gate read
// "no room" afterwards. It used to go through the turn loop's Charge, which
// refused a completion that did not fit and so recorded nothing: the counter
// stayed under the ceiling, the gate still read room, and a company at its
// ceiling paid for every pass after it with its counter hearing about none of
// them.
//
// THE GATE IS ASKED BEFORE THE CALL, and is the caller's: for the reflection
// stage, [Engine.reflectionRoom], asked by the pass through
// [Engine.learningBudget] and by a conversation entry's rewrites through
// [Dispatcher.ReflectionRoom]; for a person's question, the answer's own; and
// for a call a TURN makes, the turn's own meter ([auxspend.Use.Budget]), asked here because
// this is the one frame every in-turn call passes. That meter holds every
// window the turn has seen full, its own auxiliary calls' included, so a call
// it holds is one whose turn's next round is refused before it is sent: the
// rewrite would be bought for a prompt nobody sends. Such a call is not made,
// so nothing is charged or recorded, and its caller takes the error as it
// takes any rewrite it cannot have.
type recordedProvider struct {
	inner  llm.Provider
	key    string
	use    auxspend.Use
	seat   auxspend.Seat
	meter  spendRecorder
	ledger *auxspend.Ledger
	zone   *time.Location
	now    func() time.Time
}

func (p recordedProvider) Model() string { return p.inner.Model() }

func (p recordedProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if p.use.Budget != nil {
		if held := p.use.Budget.Held(ctx); held != nil {
			return nil, fmt.Errorf("engine: auxiliary %s call not made: %w", p.use.Purpose, held)
		}
	}
	started := p.now()
	completion, err := p.inner.Complete(ctx, req)
	ended := p.now()

	// What was BILLED, which is not only an answer: a refused call returns no
	// completion and a refusal error carrying the response the vendor charged
	// for ([llm.Billed]). Recorded all the same, or every refusal would be a
	// call no gate ever heard about. The caller still gets exactly what the
	// provider returned.
	spent := auxspend.Spent{Calls: 1}
	model := p.inner.Model()
	if billed := llm.Billed(completion, err); billed != nil {
		spent.Input, spent.Output = billed.InputTokens, billed.OutputTokens
		spent.CacheRead, spent.CacheWrite = billed.CacheRead, billed.CacheWrite
		if billed.Model != "" {
			model = billed.Model
		}
	}
	if tokens := spent.Tokens(); tokens > 0 && p.meter != nil {
		// context.WithoutCancel: the tokens are already spent at the
		// vendor. A record skipped because the caller's deadline expired
		// between the answer and the write is money the counter never
		// hears about — exactly the leak this seam exists to close.
		if spendErr := p.meter.Record(context.WithoutCancel(ctx), tokens, ended); spendErr != nil {
			// Logged, never propagated. The completion SUCCEEDED and the
			// caller's work is valid; failing it here would turn a
			// coordination blip into a reflection outage, and the
			// pre-flight gates are what actually stop the spending.
			//
			// WHICH COUNTER IS SHORT is in the detail, because the two
			// failures send an operator to different places: a record
			// that reached the company and not the seat leaves the
			// company exact (coord.SeatUncountedError), and only one
			// seat's own ceiling judges less than it spent.
			detail := "the fleet counter now understates this company's spend; "
			var partial *coord.SeatUncountedError
			if errors.As(spendErr, &partial) {
				detail = "the company's counter holds this call and the seat's does not, so " +
					"only the seat's own ceiling understates its spend; "
			}
			log.WarnContext(ctx, "auxiliary_spend_uncounted", "error", spendErr,
				"tokens", tokens, "model", model, "purpose", string(p.use.Purpose),
				"detail", detail+
					"the spend history still records it")
		}
	}
	p.use.Tally.Add(spent)
	p.ledger.Add(auxspend.Call{
		Seat: p.seat, Use: p.use, Model: model, ProviderKey: p.key,
		Day:     period.At(period.Day, ended, p.zone).Label,
		Started: started, Ended: ended, Failed: err != nil, Spent: spent,
		Trace: tracing.TraceOf(ctx),
	})
	return completion, err
}

// seatSpend records one agent seat's auxiliary spend in the seat's counter and
// the company's.
type seatSpend struct {
	budgets interface {
		PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error)
	}
	agentScope string
	zone       *time.Location
}

func (s seatSpend) Record(ctx context.Context, tokens int, at time.Time) error {
	if _, err := s.budgets.PostCharge(ctx, s.agentScope, tokens, coord.WindowsAt(at, s.zone)); err != nil {
		return fmt.Errorf("engine: record auxiliary spend: %w", err)
	}
	return nil
}

// orgSpend records auxiliary spend no seat budget can name — a person's
// question, a pass on a unit a person leads — in the company's counter alone.
type orgSpend struct {
	budgets interface {
		PostChargeOrg(ctx context.Context, tokens int, windows coord.Windows) (coord.Usage, error)
	}
	zone *time.Location
}

func (s orgSpend) Record(ctx context.Context, tokens int, at time.Time) error {
	if _, err := s.budgets.PostChargeOrg(ctx, tokens, coord.WindowsAt(at, s.zone)); err != nil {
		return fmt.Errorf("engine: record auxiliary spend on the company: %w", err)
	}
	return nil
}

// spendFor is the recorder for one agent seat's auxiliary spend, or nil where
// [Engine.meterFor] would build no meter: no coordination store, or a handle
// the epoch does not name as an agent seat. The same scope and clock as the
// seat's turns, so a pass and a round are counted in one window.
func (e *Engine) spendFor(c *Company, handle string) spendRecorder {
	m := e.meterFor(c, handle)
	if m == nil {
		return nil
	}
	return seatSpend{budgets: e.backends.Fleet, agentScope: m.agentScope, zone: m.basis.zone}
}

// auxiliaryCharge is which counter one auxiliary call is recorded in, or nil
// where there is no coordination store to count on.
//
// THE COMPANY'S ALONE WHERE NO SEAT BUDGET NAMES THE CALL: a person's question
// (the operator stage), and a call on a seat the epoch does not run as an agent
// — a unit's background pass resolved on the chain of the person who leads it.
// The second used to reach no counter at all, because the seat's meter is all
// there was and a human seat has none, so a company whose skills were promoted
// on a founder-led unit's chain paid for every promotion with its counter
// hearing about none of them.
func (e *Engine) auxiliaryCharge(c *Company) func(*org.Role, types.AuxStage) spendRecorder {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	company := orgSpend{budgets: e.backends.Fleet, zone: basisOf(c, nil).zone}
	return func(role *org.Role, stage types.AuxStage) spendRecorder {
		if stage != types.AuxStageOperator {
			if seat := e.spendFor(c, seatHandle(role)); seat != nil {
				return seat
			}
		}
		return company
	}
}

// auxiliaryFor is the epoch's auxiliary seam — the ONE every learning worker,
// the turn-start prefetch, every compaction and a person's answer resolve a
// model through — or nil for a company with no models, which every caller
// reads as a company whose auxiliary work waits for a provider.
func (e *Engine) auxiliaryFor(c *Company) auxiliaryModels {
	if c == nil || c.Models == nil {
		return nil
	}
	return auxiliarySeam{
		heads: c.Models, org: c.Org, zone: basisOf(c, nil).zone, ledger: e.auxSpend,
		charge: e.auxiliaryCharge(c), now: e.now,
	}
}

// compactorFor is the compactor this epoch rewrites over-budget text with: the
// seat's own auxiliary chain through the seam every learning worker uses — so a
// rewrite is charged and recorded like any other auxiliary call — over the
// engine's one cache.
//
// Built per call rather than held on the epoch, because it is two pointers:
// what makes it worth keeping is the cache, and that is the engine's.
func (e *Engine) compactorFor(c *Company) *compact.Compactor {
	models := e.auxiliaryFor(c)
	if models == nil {
		return nil
	}
	return compact.New(models, e.rewrites)
}

// seatCompactor is the epoch's compactor bound to one seat and one attribution
// — the zero [compact.Bound], which rewrites nothing, where the handle names no
// agent seat or the company has no models.
func (e *Engine) seatCompactor(c *Company, handle string, use auxspend.Use) compact.Bound {
	if c == nil || c.Org == nil {
		return compact.Bound{}
	}
	return e.compactorFor(c).For(c.Org.AgentSeatByHandle(handle), use)
}

// auxSpendStopBudget is how long an orderly stop waits for the ledger's last
// flush.
//
// FIVE SECONDS. A flush holds at most one [auxspend.FlushInterval] of records —
// a few dozen on a busy node — and each is one publish to a broker that is up,
// so five seconds is orders of magnitude over what it needs; and its first
// refusal ends it, so a stop with no member reachable spends at most this
// before the custody's own flush.
const auxSpendStopBudget = 5 * time.Second

// stopAuxSpend flushes the ledger one last time. Ordered by the teardown after
// every producer of auxiliary calls has stopped and before the custody flush
// and the broker's close, so the last records reach a stream that is still
// there. Nil-safe.
func (e *Engine) stopAuxSpend(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), auxSpendStopBudget)
	defer cancel()
	e.auxSpend.Stop(bounded)
}

// conversationBlock is the prior turns of this conversation as a turn is given
// them: the newest whole, and the older ones that will not fit
// [ledger.InjectedMaxChars] condensed by the seat's auxiliary model into one
// account — or, where none can be had, left out and counted. use is the turn's
// attribution: the rewrite is part of its context, so it is part of its cost.
//
// A REWRITE WHERE A DROP WAS. The block used to leave its oldest entries out,
// and the deliveries they recorded with them, so a seat on a long thread was
// told it had said nothing it could not see. The rewrite keeps them, in fewer
// words; the drop is now the fallback, and still says so.
func (e *Engine) conversationBlock(ctx context.Context, c *Company, handle string,
	history []ledger.Session, use auxspend.Use,
) string {
	opts := ledger.HistoryOptions{MaxChars: ledger.InjectedMaxChars}
	if overflow, _ := ledger.SplitHistory(history, opts.MaxChars); len(overflow) > 0 {
		res, err := e.seatCompactor(c, handle, use).Fit(ctx, compact.KindConversation,
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
