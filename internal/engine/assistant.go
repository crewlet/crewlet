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
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD ASSISTANT'S ORCHESTRATOR, wired: the seams internal/assistant
// declares, each satisfied by what this engine already runs. Its state log and
// its two sides are assistantregister.go's; this file is the model chain a turn
// runs on, the meter it charges and the records it writes.

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
	c := m.e.Company()
	if c == nil || c.Config == nil {
		return nil, "", errors.New("engine: this node runs no company yet, so it has no " +
			"assistant to run")
	}
	keys, err := c.Config.AssistantChain()
	if err != nil {
		return nil, "", err
	}
	if c.Models == nil {
		return nil, "", errors.New("engine: the company configures no providers.llm entry")
	}
	seat := auxspend.Seat{Person: person.Seat}
	if c.Org != nil {
		if role := c.Org.SeatByHandle(person.Seat); role != nil {
			seat.PersonRole = role.Name
		}
	}
	zone := basisOf(c, nil).zone
	members := make([]chain.Member, 0, len(keys))
	for _, key := range keys {
		provider, ok := c.Models.Provider(key)
		if !ok {
			return nil, "", fmt.Errorf("engine: assistant.llm names %q, which the running "+
				"company does not build", key)
		}
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

// AssistantBudget is the meter an assistant turn charges its rounds through.
func AssistantBudget(e *Engine) assistant.Budget { return assistantBudget{e: e} }

type assistantBudget struct{ e *Engine }

// orgCharger is the fleet counter's company-only post-charge.
type orgCharger interface {
	PostChargeOrg(ctx context.Context, tokens int, windows coord.Windows) (coord.Usage, error)
}

// Meter is a meter on the COMPANY's windows, the scope a person's spend is
// judged by (a person has no seat budget: see [coord.Budgets.PostChargeOrg]),
// pinned to the company's ceilings and clock as the turn starts, and having
// read the counter once, so a window already full refuses the turn before its
// first round is sent. Nil on a node with no coordination store to count on.
//
// `assistant.token_budget`, a person's own ceiling, is a scope of the fleet's
// counter of its own (internal/coord): this meter judges the company's windows
// alone.
func (b assistantBudget) Meter(ctx context.Context, _ iam.Principal) (toolloop.BudgetMeter, error) {
	if b.e.backends == nil || b.e.backends.Fleet == nil {
		return nil, nil
	}
	m := &meter{budgets: b.e.budgets(), basis: basisOf(b.e.Company(), nil), now: b.e.now}
	m.observe(ctx)
	return companyMeter{meter: m, org: b.e.backends.Fleet}, nil
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
	if err := settled(res, err); err != nil {
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
	turn uuid.UUID, parked []byte) error {
	c, err := a.call(w)
	if err != nil {
		return err
	}
	return settled(a.w.ParkTurn(ctx, c, turn, parked))
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
