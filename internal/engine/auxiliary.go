package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE AUXILIARY SEAM: every completion made for a seat or a person outside a
// turn's phases (the turn-start memory filter, knowledge query and episode
// summary, compaction rewrites, reflection and learning passes, a person's
// operator question) resolves its model through [auxiliarySeam.Auxiliary],
// which CHARGES it to the fleet's counters after the call (an agent seat's to
// the seat and the company, anything else to the company alone) and RECORDS it
// in the auxiliary-spend ledger (internal/auxspend) under the attribution its
// caller must state, so every spend figure read from events holds what the
// counters hold. One seam rather than a call per site, so a caller added later
// cannot skip either. Charged elsewhere: the turn loop's rounds, the round-cap
// judge and delegated workers through meterFor's meter, and an executor's
// launch when its spend is collected (executor_collected).

// phaseHeads is the phase registry's resolution as the seam reads it: a seat's
// walk for a phase (an LLM fleet's picks). *phase.Registry satisfies it as
// written, so which model answers a seat's cheap questions is still decided in
// one place: the one the config validator checks.
type phaseHeads interface {
	Walk(role *org.Role, ph phase.Phase) (phase.Walk, error)
	Begin(role *org.Role, ph phase.Phase, key string) (end func())
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
	// share is the seat's learning share, or nil where none holds it.
	share func(role *org.Role) *learningShare
	now   func() time.Time
}

// Auxiliary resolves the seat's auxiliary chain head, wrapped so every
// completion made through it is charged and recorded under use. On an LLM
// fleet it is a chain over every member, the head first, so a limit before an
// answer moves the call on; a pinned list keeps its head alone.
//
// An attribution the record cannot be filed under (no stage or no purpose) is
// refused before a model is resolved: every breakdown would show its spend as
// nothing in particular.
func (s auxiliarySeam) Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error) {
	if err := use.Validate(); err != nil {
		return chain.Member{}, err
	}
	walk, err := s.heads.Walk(role, phase.Auxiliary)
	if err != nil {
		return chain.Member{}, err
	}
	members := walk.Members
	if len(walk.Picks) == 0 {
		members = members[:1]
	}
	for i, m := range members {
		member := s.recorded(role, m, use)
		recorded := member.Provider.(recordedProvider)
		key := member.Key
		recorded.begin = func() func() { return s.heads.Begin(role, phase.Auxiliary, key) }
		member.Provider = recorded
		members[i] = member
	}
	if len(walk.Picks) == 0 {
		return members[0], nil
	}
	walked, err := chain.New(members, chain.Options{Fleet: walk.Picks})
	if err != nil {
		return chain.Member{}, err
	}
	return chain.Member{Key: members[0].Key, Provider: walked}, nil
}

// recordedMembers wraps members of the seat's own chains as
// [auxiliarySeam.Auxiliary] wraps the head, refusing the same attributions, for
// a caller that asks a chain: the provider park's probe, which asks the very
// members that refused (providerpark.go).
func (s auxiliarySeam) recordedMembers(role *org.Role, members []chain.Member, use auxspend.Use) ([]chain.Member, error) {
	if err := use.Validate(); err != nil {
		return nil, err
	}
	out := make([]chain.Member, 0, len(members))
	for _, m := range members {
		out = append(out, s.recorded(role, m, use))
	}
	return out, nil
}

// recorded is member with its provider wrapped to charge and record under use.
func (s auxiliarySeam) recorded(role *org.Role, member chain.Member, use auxspend.Use) chain.Member {
	var meter spendRecorder
	switch {
	case use.Budget != nil:
		// THE TURN'S OWN METER, for a call the turn makes: the same
		// counters, through the one frame that remembers what their answer
		// said, which the turn's next call asks ([auxspend.Budget]).
		meter = use.Budget
	case s.charge != nil:
		meter = s.charge(role, use.Stage)
	}
	var share *learningShare
	if s.share != nil && use.Purpose.Learning() {
		share = s.share(role)
	}
	member.Provider = recordedProvider{
		inner: member.Provider, key: member.Key, use: use, seat: s.seatOf(role, use.Stage),
		meter: meter, ledger: s.ledger, zone: s.zone, now: s.now, share: share,
	}
	return member
}

// seatOf is whose spend a call is, as its record names it: an agent seat's by
// its derived id, except on the operator stage; anything else is a PERSON's
// (the human seat a credential is bound to, or the lead of a unit whose
// background pass runs on its chain), named on `actor_seat` and `actor_role`,
// because the record's `role` fills the store's `agent_role` column, which every
// per-seat read prints as an agent's name.
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

// recordedProvider charges and records every completion AFTER it returns, since
// its size is known only from its answer, as a RECORD, NEVER A VERDICT: spent
// tokens are recorded whole, past a ceiling included, so every gate then reads
// "no room". The gate is asked BEFORE the call, by its caller
// ([Engine.reflectionRoom] through [Engine.learningBudget] for reflection, the
// answer's own for a person's question), except for a call a turn makes: the
// turn's meter ([auxspend.Use.Budget]) is asked here, the one frame every
// in-turn call passes. That meter holds every window the turn has seen full, so
// a call it holds is a rewrite for a round that would be refused: it is not
// made, nothing is charged or recorded, and the caller takes the error.
type recordedProvider struct {
	inner  llm.Provider
	key    string
	use    auxspend.Use
	seat   auxspend.Seat
	meter  spendRecorder
	ledger *auxspend.Ledger
	zone   *time.Location
	now    func() time.Time
	share  *learningShare
	// fleet is the LLM fleet's decision record where one picked inner.
	fleet types.FleetDecision
	// begin records each call in flight on key ([fleet.Ledger.Begin]), nil
	// for none.
	begin func() (end func())
}

func (p recordedProvider) Model() string { return p.inner.Model() }

// FleetDecision is why the provider serves, where an LLM fleet picked it.
func (p recordedProvider) FleetDecision() types.FleetDecision { return p.fleet }

func (p recordedProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if p.use.Budget != nil {
		if held := p.use.Budget.Held(ctx); held != nil {
			return nil, fmt.Errorf("engine: auxiliary %s call not made: %w", p.use.Purpose, held)
		}
	}
	started := p.now()
	if p.use.Purpose.HeldByShare() && !p.share.room(ctx, started) {
		return nil, fmt.Errorf("engine: auxiliary %s call not made: %w", p.use.Purpose, learning.ErrLearningShare)
	}
	end := func() {}
	if p.begin != nil {
		end = p.begin()
	}
	completion, err := p.inner.Complete(ctx, req)
	end()
	ended := p.now()

	// What the answer BILLED. A failed call is recorded too, with whatever
	// tokens it reported: a vendor bills a timed-out request. ended is read
	// ONCE, so the counter's windows and the record's company day agree
	// across midnight. The caller gets exactly what the provider returned.
	spent := auxspend.Spent{Calls: 1}
	model := p.inner.Model()
	if completion != nil {
		spent.Input, spent.Output = completion.InputTokens, completion.OutputTokens
		spent.CacheRead, spent.CacheWrite = completion.CacheRead, completion.CacheWrite
		if completion.Model != "" {
			model = completion.Model
		}
	}
	p.share.record(context.WithoutCancel(ctx), spent.Tokens(), ended)
	if tokens := spent.Tokens(); tokens > 0 && p.meter != nil {
		// context.WithoutCancel: the tokens are already spent at the vendor,
		// and a caller's deadline expiring before the write must not hide them.
		if spendErr := p.meter.Record(context.WithoutCancel(ctx), tokens, ended); spendErr != nil {
			// Logged, never propagated: the completion succeeded, failing it
			// turns a coordination blip into a reflection outage, the
			// pre-flight gates are what stop spending, and the ledger below
			// records the call regardless. The detail names WHICH counter is
			// short: a coord.SeatUncountedError leaves the company exact and
			// only the seat's own ceiling understating.
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
// [Engine.meterFor] would build no meter. It takes the meter's scope and clock,
// so a pass and a round are counted in one window.
func (e *Engine) spendFor(c *Company, handle string) spendRecorder {
	m := e.meterFor(c, handle)
	if m == nil {
		return nil
	}
	return seatSpend{budgets: e.backends.Fleet, agentScope: m.agentScope, zone: m.basis.zone}
}

// auxiliaryCharge is which counter one auxiliary call is recorded in, or nil
// where there is no coordination store. It is the company's alone where no seat
// budget names the call: a person's question (the operator stage), or a call on
// a seat the epoch does not run as an agent, such as a unit's background pass on
// the chain of the person who leads it, since a human seat has no meter.
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
	seam, ok := e.seamFor(c)
	if !ok {
		return nil
	}
	return seam
}

// seamFor is the epoch's auxiliary seam as its concrete type, and false for a
// company with no models.
func (e *Engine) seamFor(c *Company) (auxiliarySeam, bool) {
	if c == nil || c.Models == nil {
		return auxiliarySeam{}, false
	}
	return auxiliarySeam{
		heads: c.Models, org: c.Org, zone: basisOf(c, nil).zone, ledger: e.auxSpend,
		charge: e.auxiliaryCharge(c), share: e.learningShareFor(c), now: e.now,
	}, true
}

// compactorFor is the compactor this epoch rewrites over-budget text with: the
// auxiliary seam, so a rewrite is charged and recorded, over the engine's one
// cache. Built per call because it is two pointers; the cache is what is kept.
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
