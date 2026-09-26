package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
)

// Why a turn stopped has to be its OWN event, not only a field on the summary.
//
// The live projection's failure hold is taken from exactly three types —
// `llm_unavailable`, `turn.guard_breach`, `budget_exhausted`
// (internal/api/livestate) — and from nothing else. The projection's own tests
// cover that derivation and pass; for a long time nothing published the events
// they synthesise, so the state was unreachable in production and the seat
// screen's failure banner, the `broken` rail and the attention queue's "the engine
// stopped it" row were all dead branches. Both halves were tested, and nothing
// asserted they were connected. That is what these cases are for.

// pub is a queue that only records. publishFailure touches nothing but
// Publish, so the rest of the contract is embedded and never called — a
// deliberate nil embed, so a method this test starts depending on panics
// loudly rather than answering something plausible.
type pub struct {
	queue.EventQueue
	mu     sync.Mutex
	events []*events.Event
}

func (p *pub) Publish(_ context.Context, _ string, ev *events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

// only returns the single event of type T, failing if there is not exactly one.
func only[T events.Payload](t *testing.T, p *pub, wantType string) T {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var found []T
	for _, ev := range p.events {
		if got, ok := events.DataAs[T](ev); ok {
			found = append(found, got)
		}
	}
	if len(found) != 1 {
		t.Fatalf("published %d %s events, want exactly 1 (published: %s)",
			len(found), wantType, typesOf(p.events))
	}
	return found[0]
}

func typesOf(evs []*events.Event) string {
	if len(evs) == 0 {
		return "nothing"
	}
	out := ""
	for i, ev := range evs {
		if i > 0 {
			out += ", "
		}
		out += ev.Type
	}
	return out
}

func none[T events.Payload](t *testing.T, p *pub, name string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ev := range p.events {
		if _, ok := events.DataAs[T](ev); ok {
			t.Fatalf("published a %s event for a turn that did not fail that way", name)
		}
	}
}

// failing builds an engine whose only wired backend is a capturing queue,
// which is all publishFailure touches.
func failing(t *testing.T) (*Engine, *pub, turnTelemetry) {
	t.Helper()
	p := &pub{}
	e := &Engine{backends: &Backends{Queue: p}}
	return e, p, turnTelemetry{role: "CEO", agentID: "a-1", handle: "ceo", runID: "t-1", workKey: "wk-1"}
}

func TestAGuardBreachIsItsOwnEventNotJustAFieldOnTheSummary(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)

	e.publishFailure(context.Background(), tel, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardStall, Detail: "two rounds, one artifact"},
	}, nil)

	got := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	// THE KIND IS THE WHOLE VALUE. "stall" and "max_iter" send an operator
	// to different places; a bare "failed" sends them to neither.
	if got.Kind != types.GuardStall {
		t.Errorf("kind = %q, want %q", got.Kind, types.GuardStall)
	}
	if got.TurnID != "t-1" {
		t.Errorf("turn_id = %q, want t-1 — without it the Turn screen cannot "+
			"select the row", got.TurnID)
	}
	if got.Detail != "two rounds, one artifact" {
		t.Errorf("detail = %q", got.Detail)
	}
	if got.RoleName != "CEO" || got.Agent != "a-1" {
		t.Errorf("addressed to role=%q agent=%q, want CEO/a-1", got.RoleName, got.Agent)
	}
}

func TestAnExhaustedProviderChainSaysWhatItTried(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)

	exhausted := &chain.Error{
		Attempted: []string{"primary", "backup"},
		Err: &llm.Error{
			Kind: llm.KindRateLimit, Provider: "p", Model: "backup-model",
			Err: fmt.Errorf("rate limited"),
		},
	}
	e.publishFailure(context.Background(), tel,
		turn.Result{Decision: phase.Failed}, fmt.Errorf("runner: execute: %w", exhausted))

	got := only[*types.LLMUnavailable](t, p, "llm_unavailable")
	if len(got.ProviderChain) != 2 || got.ProviderChain[0] != "primary" {
		t.Errorf("provider_chain = %v, want the keys that were tried", got.ProviderChain)
	}
	if got.AttemptCount != 2 {
		t.Errorf("attempt_count = %d, want 2 — one flaky provider and a whole "+
			"misconfigured chain read identically without it", got.AttemptCount)
	}
	// The CLASSIFIED kind of the last failure, unwrapped through the chain's
	// own error. A chain that reported "error" for a rate limit would send an
	// operator looking at credentials instead of at quota.
	if got.LastErrorKind != llm.KindRateLimit.String() {
		t.Errorf("last_error_kind = %q, want %q", got.LastErrorKind, llm.KindRateLimit)
	}
	// THE RUN the chain ran out under, and the unit of work beside it: an
	// failure record that named only the trigger could not say which attempt
	// at it went dark. See ADR-0017.
	if got.TurnID != "t-1" || got.WorkKey != "wk-1" {
		t.Errorf("turn_id = %q work_key = %q, want the turn's own run and key",
			got.TurnID, got.WorkKey)
	}
	// No guard fired, so nothing may claim one did.
	none[*types.TurnGuardBreach](t, p, "turn.guard_breach")
}

func TestARefusedChargeIsABudgetEventNotAProviderOne(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)

	e.publishFailure(context.Background(), tel, turn.Result{Decision: phase.Failed},
		fmt.Errorf("execute: %w", &toolloop.BudgetError{
			Scope: string(types.BudgetScopeOrg), Used: 1_000_000, Limit: 900_000,
			Period: period.Month, Window: "2026-09",
			ResetsAt: time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC),
		}))

	got := only[*types.BudgetExhausted](t, p, "budget_exhausted")
	if got.BudgetType != types.BudgetScopeOrg {
		t.Errorf("budget_type = %q, want org — an org cap and a per-seat cap are "+
			"refused on the same seat and read identically otherwise", got.BudgetType)
	}
	if got.UsedTokens != 1_000_000 || got.MaxTokens != 900_000 {
		t.Errorf("used/max = %d/%d, want 1000000/900000", got.UsedTokens, got.MaxTokens)
	}
	// AND THE WINDOW, which is what says when the seat can run again: a
	// month that resets on the 1st is a different decision about raising a
	// ceiling from a day that resets tonight.
	if got.Period != "month" || got.Window != "2026-09" || got.ResetsAt != "2026-10-01T00:00:00Z" {
		t.Errorf("window = %q %q resets %q, want the month 2026-09 resetting 2026-10-01T00:00:00Z",
			got.Period, got.Window, got.ResetsAt)
	}
	// A refused charge never reaches a provider, so calling it unavailable
	// would blame a chain that was never walked.
	none[*types.LLMUnavailable](t, p, "llm_unavailable")
}

func TestAnUnhandledExceptionIsBothABreachAndItsCause(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)

	// The two are not exclusive: the guard names WHICH invariant ended the
	// turn, the error says what broke. Reporting only one drops half the
	// answer, and this is the case where a reader needs both.
	e.publishFailure(context.Background(), tel, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: "unhandled_exception", Detail: "nil map write"},
	}, errors.New("nil map write"))

	got := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	if got.Kind != "unhandled_exception" {
		t.Errorf("kind = %q", got.Kind)
	}
	// An error that is neither a budget refusal nor an exhausted chain gets
	// no second event: the summary's error/error_kind is the record of it,
	// and inventing a class here would file an ordinary crash as a provider
	// stop.
	none[*types.LLMUnavailable](t, p, "llm_unavailable")
	none[*types.BudgetExhausted](t, p, "budget_exhausted")
}

func TestATurnThatDidNotFailPublishesNoFailure(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)

	e.publishFailure(context.Background(), tel,
		turn.Result{Decision: phase.Done}, nil)

	// Every one of these types is a failure BY TYPE, so a spurious publish
	// does not merely add a row — it holds a failure on the seat and paints the
	// turn red on every surface that reads the taxonomy.
	none[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	none[*types.LLMUnavailable](t, p, "llm_unavailable")
	none[*types.BudgetExhausted](t, p, "budget_exhausted")
	if len(p.events) != 0 {
		t.Errorf("a clean turn published %s", typesOf(p.events))
	}
}

// The two halves have to be CONNECTED, which is the failure the rest of this
// file exists because of: the projection's tests passed, publishFailure's
// tests would have passed, and nothing asserted that closing a turn calls it.
func TestClosingAFailedTurnPublishesTheSummaryAndTheCause(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)
	tel.startedAt = time.Now().UTC().Add(-time.Second)

	e.publishTurnCompleted(context.Background(), tel, runner.Spend{}, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardMaxIter, Detail: "6 rounds, no done"},
	}, nil)

	// All FOUR: the dashboard's summary, the learning record, and the guard
	// that named the stop. Dropping any one of them takes a whole surface
	// with it — the seat's live row, the episode, or its failure hold.
	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	if !summary.Failed || summary.ErrorKind != string(types.GuardMaxIter) {
		t.Errorf("summary = failed:%v kind:%q, want failed with the guard's kind",
			summary.Failed, summary.ErrorKind)
	}
	_ = only[*types.TurnCompleted](t, p, "turn_completed")
	breach := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	if breach.Kind != types.GuardMaxIter {
		t.Errorf("breach kind = %q", breach.Kind)
	}
	if breach.TurnID != summary.TurnID || breach.WorkKey != summary.WorkKey {
		t.Errorf("breach names run %q key %q, summary names run %q key %q — the "+
			"two records of one turn have to join",
			breach.TurnID, breach.WorkKey, summary.TurnID, summary.WorkKey)
	}
}

// A PANIC CLOSES THE TURN UNDER THE GUARD'S NAME, not as a generic error.
//
// A panicking phase is the one result that carries both an error and a breach,
// and the summary named the error first: the Turn screen then said the engine
// stopped it with "error", for the one failure that is the engine's own defect.
// The breach is what the seat's failure hold reads, so it must be published too,
// exactly once.
func TestAPanickedTurnClosesUnderTheUnhandledExceptionGuard(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)
	tel.startedAt = time.Now().UTC().Add(-time.Second)
	cause := fmt.Errorf("turn: execute round 2: %w", turn.Recovered("nil map"))

	e.publishTurnCompleted(context.Background(), tel, runner.Spend{}, turn.Result{
		Decision: phase.Failed,
		Breach:   &turn.Breach{Kind: types.GuardUnhandledException, Detail: "panic: nil map"},
	}, cause)

	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	if !summary.Failed || summary.ErrorKind != string(types.GuardUnhandledException) {
		t.Errorf("summary = failed:%v kind:%q, want failed with %q",
			summary.Failed, summary.ErrorKind, types.GuardUnhandledException)
	}
	// The error's own text, which names the phase and round the breach
	// detail does not.
	if summary.Error != cause.Error() {
		t.Errorf("summary error = %q, want %q", summary.Error, cause.Error())
	}
	breach := only[*types.TurnGuardBreach](t, p, "turn.guard_breach")
	if breach.Kind != types.GuardUnhandledException || breach.TurnID != "t-1" {
		t.Errorf("breach = %+v, want unhandled_exception for run t-1", breach)
	}
}
