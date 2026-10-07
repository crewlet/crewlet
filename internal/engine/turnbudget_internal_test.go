package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// A turn's own auxiliary spend, held by the turn's own meter.
//
// The meter learned only from its own charges' answers, and a turn's auxiliary
// calls — its context assembly, the conversation block's condensation, every
// rewrite its ledgers and tools ask for — reach the same counters by another
// route, a post-charge that refuses nothing. So a window one of them took past
// its ceiling was one the turn's next round was sent into, billed by the
// vendor, and then refused. These cases drive the REAL turn frame, because the
// property is about the order of its steps: the meter has to exist before the
// first auxiliary call and be the thing that call is charged through.

// countedProvider counts its calls and answers each the same way.
type countedProvider struct {
	mu    sync.Mutex
	calls int
	// answer is what a call returns; nil fails it, as a model a case
	// expects never to reach.
	answer *llm.Completion
}

func (p *countedProvider) Model() string { return "counted" }

func (p *countedProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.answer == nil {
		return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "counted"}
	}
	answer := *p.answer
	return &answer, nil
}

func (p *countedProvider) called() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// budgetedTurn is an engine over the in-memory fleet, running one seat whose
// day is capped at dayCeiling, with its executor and its auxiliary model on
// two providers a case can count.
type budgetedTurn struct {
	e          *Engine
	pub        *pub
	fleet      *coordmem.Fleet
	seat       *org.Role
	org        *org.Organization
	executor   *countedProvider
	auxiliary  *countedProvider
	dayCeiling int
}

func newBudgetedTurn(t *testing.T, dayCeiling, auxTokens int) *budgetedTurn {
	t.Helper()
	seat := &org.Role{
		Name: "SWE", DeclaredHandle: "swe",
		LLM: org.ProviderKeys{"main"}, LLMAuxiliary: org.ProviderKeys{"cheap"},
		TokenBudget: org.TokenCeilings{period.Day: dayCeiling},
	}
	r := &budgetedTurn{
		pub: &pub{}, fleet: coordmem.NewFleet(), seat: seat, dayCeiling: dayCeiling,
		org:      &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		executor: &countedProvider{},
		// A rewrite that fits whatever budget it is asked for, and costs
		// what the case says.
		auxiliary: &countedProvider{answer: &llm.Completion{
			Model: "cheap", Content: "Earlier: the seat answered the same question four times.",
			InputTokens: auxTokens - 100, OutputTokens: 100,
		}},
	}
	models, err := phase.NewRegistry([]phase.Entry{
		{Key: "main", Provider: r.executor}, {Key: "cheap", Provider: r.auxiliary},
	})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	r.e = &Engine{backends: &Backends{Queue: r.pub, Fleet: r.fleet}}
	r.e.epoch.current.Store(&Company{
		Org: r.org, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 3, MaxToolRounds: 3,
		}},
	})
	return r
}

// longConversation is a conversation whose older entries do not fit the
// block a turn is given, so the turn's context condenses them with one call
// on the seat's auxiliary model.
func longConversation() []ledger.Session {
	var history []ledger.Session
	for range 10 {
		history = append(history, ledger.Session{
			Trigger: "Message from Ana", Decision: "done",
			Reply: strings.Repeat("The deploy is green and the flag is on. ", 75),
		})
	}
	return history
}

func (r *budgetedTurn) run(t *testing.T) error {
	t.Helper()
	_, err := r.e.runTurn(t.Context(), Request{
		RunID: newRunID(), Handle: "swe", WorkKey: "wk-1", ConversationKey: "conv-1",
		History: longConversation(),
		Events: []*events.Event{events.New(types.TaskAssigned{
			TaskID: "t-1", RoleName: "SWE", Description: "answer Ana", Schedule: "s",
		}, events.TraceContext{})},
	})
	return err
}

// seatDay is what the seat's counter holds for today.
func (r *budgetedTurn) seatDay(t *testing.T) int {
	t.Helper()
	id, ok := r.org.AgentIDFor(r.seat)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	u, err := r.fleet.Used(t.Context(), coord.AgentScope(id.String()),
		coord.WindowsAt(time.Now(), time.UTC))
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	return u.In(period.Day).Used
}

// A TURN WHOSE CONTEXT TAKES ITS SEAT PAST ITS DAY SENDS NO ROUND.
//
// The budget park found room, so the turn started; condensing its
// conversation then cost more than the day had left. That spend is real — the
// counter holds it whole — and every round after it is certain to be refused,
// so the executor's first round must be held before it is sent rather than
// sent, billed and refused. The turn ends on the budget, naming the window the
// condensation filled.
func TestATurnWhoseContextSpendsItsDayIsHeldBeforeItsFirstRound(t *testing.T) {
	t.Parallel()
	r := newBudgetedTurn(t, 1000, 1600)

	err := r.run(t)
	if r.auxiliary.called() != 1 {
		t.Fatalf("the auxiliary model was called %d times, want the one condensation "+
			"this case is about (err: %v)", r.auxiliary.called(), err)
	}
	if got := r.seatDay(t); got != 1600 {
		t.Errorf("the seat's day holds %d, want the 1600 the condensation spent", got)
	}
	if n := r.executor.called(); n != 0 {
		t.Errorf("the executor was called %d times on a day the turn's own context had "+
			"already spent: each call was billed and then refused", n)
	}
	var refused *toolloop.BudgetError
	if !errors.As(err, &refused) {
		t.Fatalf("runTurn = %v, want the turn ended on the budget", err)
	}
	if refused.Scope != "agent" || refused.Period != period.Day || refused.Used != 1600 ||
		refused.Limit != 1000 {
		t.Errorf("refusal = %+v, want the seat's day the condensation took to 1600 of 1000", refused)
	}
	got := only[*types.BudgetExhausted](t, r.pub, "budget_exhausted")
	if got.BudgetType != types.BudgetScopeAgent || got.UsedTokens != 1600 || got.MaxTokens != 1000 {
		t.Errorf("budget_exhausted = %+v, want the seat's day at 1600 of 1000", got)
	}
}

// THE TURN'S JUDGE ASKS THE TURN'S METER, the one its attribution carries, so
// a window the judge's own evidence rewrite filled stops the judge's call
// rather than letting it be billed and refused.
func TestATurnsJudgeAsksTheTurnsMeter(t *testing.T) {
	t.Parallel()
	r := newBudgetedTurn(t, 1000, 1600)
	use := auxspend.Use{Stage: types.AuxStageTurn, TurnID: "run-1", Budget: &turnBudget{
		held: &toolloop.BudgetError{Scope: "agent", Used: 1600, Limit: 1000, Period: period.Day},
	}}
	judge := r.e.judgeFor(r.e.Company(), "swe", use)
	if judge == nil {
		t.Fatal("no judge for a seat with a model")
	}
	_, err := judge.Decide(t.Context(), extension.Request{Phase: phase.Execute, Task: "answer Ana"})
	if !errors.Is(err, extension.ErrHeld) || !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Decide = %v, want the judge held on the turn's refusal", err)
	}
	if n := r.executor.called(); n != 0 {
		t.Errorf("the judge's model was called %d times on a refusal the turn held", n)
	}
}

// A TURN WHOSE CONTEXT FITS RUNS ITS ROUND, so the case above is about the
// window being full and not about a turn that condensed anything at all.
func TestATurnWhoseContextFitsItsDayRunsItsFirstRound(t *testing.T) {
	t.Parallel()
	r := newBudgetedTurn(t, 100_000, 1600)

	_ = r.run(t)
	if r.auxiliary.called() != 1 {
		t.Fatalf("the auxiliary model was called %d times, want one condensation",
			r.auxiliary.called())
	}
	if r.executor.called() == 0 {
		t.Error("the executor was never called on a day with room left")
	}
}
