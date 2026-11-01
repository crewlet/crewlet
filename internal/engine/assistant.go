package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// THE DASHBOARD ASSISTANT'S ORCHESTRATOR, wired: the seams internal/assistant
// declares, each satisfied by what this engine already runs. Its state log and
// its two sides are assistantregister.go's; this file is the model chain a turn
// runs on, the meter it charges and the records it writes.

// AssistantToolSessions is where a turn serves its tools to the harness its model
// runs in: this process's one [toolsession.Host], the seats' own (ADR-0049).
func AssistantToolSessions(e *Engine) *toolsession.Host { return e.sessions }

// AssistantModels is the assistant's model chain off the company this node
// runs NOW, per turn.
func AssistantModels(e *Engine) assistant.Models { return assistantModels{e: e} }

type assistantModels struct{ e *Engine }

// Chain resolves `assistant.llm` through the ONE accessor that hands it out
// ([config.Company.AssistantChain], which refuses a chain the assistant's rules
// do not admit), and wraps every member so each completion is RECORDED on the
// auxiliary-spend ledger under use, as the person's spend on the operator
// stage, and CHARGED NOWHERE: the turn's own meter ([AssistantBudget]) charges
// a round, so it is counted once.
//
// EVERY MEMBER IS RECORDED UNDER ITS OWN KEY, then chained, so a round a
// fallback served is filed under the entry that served it.
func (m assistantModels) Chain(_ context.Context, person iam.Principal,
	use auxspend.Use) (llm.Provider, string, error) {

	if err := use.Validate(); err != nil {
		return nil, "", err
	}
	c, keys, providers, err := m.entries()
	if err != nil {
		return nil, "", err
	}
	seat := auxspend.Seat{Person: person.Seat}
	if c.Org != nil {
		if role := c.Org.SeatByHandle(person.Seat); role != nil {
			seat.PersonRole = role.Name
		}
	}
	zone := basisOf(c, nil).zone
	members := make([]chain.Member, 0, len(keys))
	for i, key := range keys {
		provider := providers[i]
		members = append(members, chain.Member{Key: key, Provider: recordedProvider{
			inner: provider, key: key, use: use, seat: seat, ledger: m.e.auxSpend,
			zone: zone, now: m.e.now,
		}})
	}
	built, err := chain.New(members, chain.Options{OnFallback: func(f chain.Fallback) {
		log.Warn("assistant_model_fallback", "from", f.From, "to", f.To,
			"kind", f.Kind.String(), "error", f.Err)
	}})
	if err != nil {
		return nil, "", fmt.Errorf("engine: assistant chain: %w", err)
	}
	return built, keys[0], nil
}

// Unready is the failure the assistant's chain would meet as far as its
// members know without a call ([chain.Unready] over the entries
// `assistant.llm` names, as this node built them), or nil when any may answer
// or the chain does not resolve.
func (m assistantModels) Unready(ctx context.Context) error {
	_, _, providers, err := m.entries()
	if err != nil {
		// A chain that does not resolve is the turn's own refusal
		// ([assistantModels.Chain]) and `/viewer`'s no_model, not a state
		// of a model's.
		return nil //nolint:nilerr // see above: not a model's state
	}
	return chain.Unready(ctx, providers)
}

// entries resolves `assistant.llm` through the ONE accessor that hands it out
// ([config.Company.AssistantChain]) to the entries this node built, in order:
// the company, the keys and each key's provider as built, unwrapped.
func (m assistantModels) entries() (*Company, []string, []llm.Provider, error) {
	c := m.e.Company()
	if c == nil || c.Config == nil {
		return nil, nil, nil, errors.New("engine: this node runs no company yet, so it has " +
			"no assistant to run")
	}
	keys, err := c.Config.AssistantChain()
	if err != nil {
		return nil, nil, nil, err
	}
	if c.Models == nil {
		return nil, nil, nil, errors.New("engine: the company configures no providers.llm entry")
	}
	providers := make([]llm.Provider, 0, len(keys))
	for _, key := range keys {
		provider, ok := c.Models.Provider(key)
		if !ok {
			return nil, nil, nil, fmt.Errorf("engine: assistant.llm names %q, which the "+
				"running company does not build", key)
		}
		providers = append(providers, provider)
	}
	return c, keys, providers, nil
}

// AssistantBudget is the meter an assistant turn charges its rounds through.
func AssistantBudget(e *Engine) assistant.Budget { return assistantBudget{e: e} }

type assistantBudget struct{ e *Engine }

// orgCharger is the fleet counter's company-only post-charge.
type orgCharger interface {
	PostChargeOrg(ctx context.Context, tokens int, windows coord.Windows) (coord.Usage, error)
}

// Meter is a meter on the COMPANY's windows and, where the company caps a
// person (`assistant.token_budget`), on that PERSON's own
// ([coord.PersonScope]), pinned to the ceilings and clock as the turn starts,
// and having read the counter once, so a window already full, the company's
// or the person's, refuses the turn before its first round is sent. Nil on a
// node with no coordination store to count on.
//
// A CAPPED PERSON IS A SEAT'S METER on the person's scope: every round is
// charged to the company and the person in one judged charge, the company
// first, exactly as a seat's round is (one implementation of the gate, not a
// second one for people). An uncapped person is charged to the company alone
// ([companyMeter]), so the listing holds no row no ceiling can judge.
func (b assistantBudget) Meter(ctx context.Context, person iam.Principal) (toolloop.BudgetMeter, error) {
	if b.e.backends == nil || b.e.backends.Fleet == nil {
		return nil, nil
	}
	c := b.e.Company()
	m := &meter{budgets: b.e.budgets(), basis: basisOf(c, nil), now: b.e.now}
	if caps := personCaps(c); len(caps) > 0 {
		m.agentScope, m.basis.seat = coord.PersonScope(person.ID), caps
		m.observe(ctx)
		return personMeter{meter: m}, nil
	}
	m.observe(ctx)
	return companyMeter{meter: m, org: b.e.backends.Fleet}, nil
}

// personCaps is the ceilings a company sets each person's assistant spend to,
// empty where it sets none.
func personCaps(c *Company) coord.Caps {
	if c == nil {
		return nil
	}
	return AssistantCaps(c.Config)
}

// AssistantCaps is the ceilings the company document cfg holds every person's
// assistant spend to (`assistant.token_budget`), empty where it sets none: the
// ONE reading the turn's meter and the budgets answer share, so the ceiling a
// person is refused by is the ceiling the Budgets screen draws.
func AssistantCaps(cfg *config.Company) coord.Caps {
	if cfg == nil {
		return nil
	}
	return coord.Caps(cfg.Assistant.TokenBudget.Ceilings())
}

// personMeter is a [meter] whose seat scope is a person's: what it answers
// names that scope [assistant.PersonBudgetScope], so the person is told it is
// their own day (or week, or month) that is spent, not a seat's.
type personMeter struct{ *meter }

var _ toolloop.BudgetMeter = personMeter{}

// Spend is [meter.Spend], with the person's scope named as theirs.
func (m personMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	out, err := m.meter.Spend(ctx, tokens)
	return asPerson(out), err
}

// Refused is [meter.Refused], with the person's scope named as theirs.
func (m personMeter) Refused(ctx context.Context) (toolloop.SpendOutcome, bool) {
	out, refused := m.meter.Refused(ctx)
	return asPerson(out), refused
}

// Held is [meter.Held] on the renamed answer.
func (m personMeter) Held(ctx context.Context) error { return toolloop.Refusal(ctx, m) }

// asPerson names a seat-scope answer the person's.
func asPerson(o toolloop.SpendOutcome) toolloop.SpendOutcome {
	if o.Scope == "agent" {
		o.Scope = assistant.PersonBudgetScope
	}
	return o
}

// companyMeter is a [meter] with no seat scope: every round and every
// auxiliary call is post-charged to the company's counter and judged against
// its ceilings, and every full window is kept so the turn's next call is held.
type companyMeter struct {
	*meter
	org orgCharger
}

var _ toolloop.BudgetMeter = companyMeter{}

// Spend records a round already spent on the company's counter and judges
// whether it fitted: a round that took a capped window past its ceiling is
// refused, counted, so the turn's tools do not run and its next round is held
// ([meter.Refused]). On a context that outlives the caller's, for
// [meter.Spend]'s reason.
func (m companyMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	usage, err := m.org.PostChargeOrg(context.WithoutCancel(ctx), tokens, m.windows())
	if err != nil {
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	outcome := toolloop.SpendOutcome{OK: true}
	for p, ceiling := range m.basis.org {
		slot := usage.In(p)
		if slot.Used <= ceiling {
			continue
		}
		refused := toolloop.SpendOutcome{Scope: "org", Used: slot.Used, Limit: ceiling,
			Period: p, Window: slot.Window.Label, ResetsAt: slot.Window.End}
		if outcome.OK || namesBefore(refused, outcome) {
			outcome = refused
		}
	}
	m.keepFullIn("org", usage, m.basis.org)
	if cause := context.Cause(ctx); cause != nil {
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: the round is "+
			"recorded and the turn has ended: %w", cause)
	}
	return outcome, nil
}

// Record post-charges a call the turn made beside its rounds, the
// conversation's title, on the company's counter, refusing nothing.
func (m companyMeter) Record(ctx context.Context, tokens int, at time.Time) error {
	usage, err := m.org.PostChargeOrg(ctx, tokens, coord.WindowsAt(at, m.basis.zone))
	if err != nil {
		return fmt.Errorf("engine: record assistant spend on the company: %w", err)
	}
	m.keepFullIn("org", usage, m.basis.org)
	return nil
}

// AssistantRecords is the assistant's write side as the orchestrator writes
// it, over this node's assistant writer, or nil on an engine with no core
// runtime.
func AssistantRecords(e *Engine) assistant.Records {
	w := e.AssistantWriter()
	if w == nil {
		return nil
	}
	return assistantRecords{w: w}
}

// assistantRecords adapts the domain's writer: the operation a record is
// published under is derived here from the gesture's key and the record's
// place in it, and a write whose outcome nobody can confirm answers
// [assistant.ErrOutcomeUnknown] naming the operation a retry reuses.
type assistantRecords struct{ w *assistantdomain.Writer }

// call is the domain's caller for one record.
func (a assistantRecords) call(w assistant.Write) (assistantdomain.Call, error) {
	return assistantCall(w)
}

// assistantCall is the domain's caller for one record of a gesture: the
// operation derived from the gesture's key and the record's place in it. ONE
// DERIVATION for every adapter of the domain's writer (the records and the
// files), so a retry of a gesture is the same operation whichever records it.
func assistantCall(w assistant.Write) (assistantdomain.Call, error) {
	op := w.Key
	if w.Derived != nil {
		op = statelog.DeriveOpID(w.Derived.At, w.Derived.Name, w.Derived.Identity...)
	}
	if op == "" {
		return assistantdomain.Call{}, errors.New("engine: an assistant record names no " +
			"operation")
	}
	return assistantdomain.Call{Owner: w.Owner, By: w.By,
		OpID: statelog.StepOpID(op, w.Steps...)}, nil
}

// settled is a write's answer as the orchestrator reads it.
func settled(res statelog.Result, err error) error {
	switch {
	case err != nil:
		return err
	case res.Outcome == statelog.OutcomeUnknown:
		return fmt.Errorf("%w: operation %s", assistant.ErrOutcomeUnknown, res.OpID)
	}
	return nil
}

func (a assistantRecords) CreateConversation(ctx context.Context, w assistant.Write,
	id uuid.UUID, title string) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.CreateConversation(ctx, c, id, title))
}

func (a assistantRecords) TitleConversation(ctx context.Context, w assistant.Write,
	id uuid.UUID, title string) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.TitleConversation(ctx, c, id, title))
}

func (a assistantRecords) ClearConversation(ctx context.Context, w assistant.Write,
	id uuid.UUID) (assistant.Written, error) {
	c, err := a.call(w)
	if err != nil {
		return assistant.Written{}, err
	}
	res, err := a.w.ClearConversation(ctx, c, id)
	if err = settled(res, err); err != nil {
		return assistant.Written{}, err
	}
	out := assistant.Written{Outcome: string(res.Outcome)}
	if !res.Position.IsZero() {
		out.Position = res.Position.String()
	}
	return out, nil
}

func (a assistantRecords) OpenTurn(ctx context.Context, w assistant.Write,
	t assistantdomain.NewTurn) (int, error) {
	c, err := a.call(w)
	if err != nil {
		return 0, err
	}
	seq, res, err := a.w.OpenTurn(ctx, c, t)
	if err == nil && res.Collapsed {
		// A RETRY THE LEDGER ANSWERED: the first opening's runner has
		// the turn, wherever it runs.
		return seq, fmt.Errorf("%w: turn %s", assistantdomain.ErrTurnOpen, t.Turn)
	}
	return seq, settled(res, err)
}

func (a assistantRecords) RecordMessage(ctx context.Context, w assistant.Write,
	turn uuid.UUID, m assistantdomain.NewMessage) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.RecordMessage(ctx, c, turn, m))
}

func (a assistantRecords) RecordStep(ctx context.Context, w assistant.Write,
	turn uuid.UUID, s wire.StepEvent) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.RecordStep(ctx, c, turn, s))
}

func (a assistantRecords) RecordCard(ctx context.Context, w assistant.Write,
	turn uuid.UUID, card wire.CardEvent, apps []string) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.RecordCard(ctx, c, turn, card, apps))
}

func (a assistantRecords) ParkTurn(ctx context.Context, w assistant.Write,
	turn uuid.UUID, parked []byte, steps int) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.ParkTurn(ctx, c, turn, parked, steps))
}

func (a assistantRecords) ResumeTurn(ctx context.Context, w assistant.Write,
	turn uuid.UUID, node string) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.ResumeTurn(ctx, c, turn, node))
}

func (a assistantRecords) CloseTurn(ctx context.Context, w assistant.Write,
	turn uuid.UUID, end assistantdomain.TurnEnd) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.CloseTurn(ctx, c, turn, end))
}

func (a assistantRecords) CreateProposal(ctx context.Context, w assistant.Write,
	p assistantdomain.NewProposal) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.CreateProposal(ctx, c, p))
}

func (a assistantRecords) ApproveProposal(ctx context.Context, w assistant.Write,
	proposal uuid.UUID) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.MoveProposal(ctx, c, proposal, assistantdomain.ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalApproved}))
}
