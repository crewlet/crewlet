package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// The cases below are about the SEAM: a worker that resolves its model the
// ordinary way is read for room and charged without knowing it, which is what
// makes a worker added later subject to the budget too.

// countingMeter records what it was asked to spend and whether it refuses.
// full is a budget with no room left; an err fails both verbs.
type countingMeter struct {
	spent  int
	calls  int
	refuse bool
	full   bool
	err    error
}

func (m *countingMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.calls++
	if m.err != nil {
		return toolloop.SpendOutcome{}, m.err
	}
	m.spent += tokens
	return toolloop.SpendOutcome{OK: !m.refuse, Used: m.spent}, nil
}

func (m *countingMeter) Room(context.Context) (toolloop.SpendOutcome, error) {
	if m.err != nil {
		return toolloop.SpendOutcome{}, m.err
	}
	if m.full {
		return toolloop.SpendOutcome{Scope: "org", Used: 100, Limit: 100}, nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

// answeringProvider returns a completion with a known token cost.
type answeringProvider struct {
	in, out int
	err     error
	calls   int
}

func (p *answeringProvider) Model() string { return "test-model" }

func (p *answeringProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &llm.Completion{Model: "test-model", InputTokens: p.in, OutputTokens: p.out}, nil
}

// staticModels is the Models seam a learning worker resolves through.
type staticModels struct{ provider llm.Provider }

func (m staticModels) Head(*org.Role, phase.Phase) (chain.Member, error) {
	return chain.Member{Key: "aux", Provider: m.provider}, nil
}

func meteredHead(t *testing.T, inner llm.Provider, m toolloop.BudgetMeter) chain.Member {
	t.Helper()
	models := meteredModels{
		inner:  staticModels{provider: inner},
		charge: func(*org.Role) toolloop.BudgetMeter { return m },
	}
	member, err := models.Head(&org.Role{Name: "Dev"}, phase.Auxiliary)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return member
}

// AN AUXILIARY COMPLETION IS CHARGED. The whole finding, in one case.
func TestAnAuxiliaryCompletionChargesTheSharedCounter(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{}
	member := meteredHead(t, &answeringProvider{in: 700, out: 300}, meter)

	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if meter.spent != 1000 {
		t.Errorf("charged %d tokens, want the completion's 1000 — auxiliary "+
			"spend that reaches no counter is money the operator never sees",
			meter.spent)
	}
}

// WITH NO CEILING TO ENFORCE THE PROVIDER IS UNWRAPPED, so an unlimited
// company pays no round trip per auxiliary call to be told "yes" — the same
// reason meterFor returns nil rather than an always-allow meter.
func TestWithNoBudgetTheProviderIsNotWrapped(t *testing.T) {
	t.Parallel()
	inner := &answeringProvider{in: 5, out: 5}
	member := meteredHead(t, inner, nil)
	if member.Provider != llm.Provider(inner) {
		t.Fatal("a company with no ceiling still got a metered provider")
	}
}

// A FAILED CHARGE DOES NOT FAIL THE COMPLETION. The call already succeeded at
// the vendor and the caller's work is valid; turning a coordination blip into
// a reflection outage would be the wrong trade, and the room read before the
// next call is what actually stops the spending.
func TestAnUncountedSpendStillReturnsTheCompletion(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{err: errors.New("counter unreachable")}
	member := meteredHead(t, &answeringProvider{in: 10, out: 10}, meter)

	got, err := member.Provider.Complete(t.Context(), llm.Request{})
	if err != nil {
		t.Fatalf("a charge failure was propagated as a completion failure: %v", err)
	}
	if got == nil || got.TotalTokens() != 20 {
		t.Fatalf("completion = %+v, want the provider's answer intact", got)
	}
}

// A FAILED COMPLETION IS NOT CHARGED. There are no tokens to bill for a call
// that produced nothing, and charging a nil completion would be inventing
// spend.
func TestAFailedCompletionChargesNothing(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{}
	member := meteredHead(t, &answeringProvider{err: errors.New("upstream 500")}, meter)

	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err == nil {
		t.Fatal("the provider error was swallowed")
	}
	if meter.calls != 0 {
		t.Errorf("charged %d times for a call that returned nothing", meter.calls)
	}
}

// AN AUXILIARY CALL ON A SPENT BUDGET IS NOT SENT.
//
// Its charge comes after the answer, because that is when its size is known,
// so a budget read only by the charge pays for every call an exhausted company
// makes and refuses each one after the provider has billed it.
func TestAnAuxiliaryCallOnASpentBudgetIsNotSent(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{full: true}
	inner := &answeringProvider{in: 10, out: 10}
	member := meteredHead(t, inner, meter)

	_, err := member.Provider.Complete(t.Context(), llm.Request{})
	var refusal *toolloop.BudgetError
	if !errors.As(err, &refusal) || refusal.Scope != "org" {
		t.Fatalf("err = %v, want the company's budget named as the reason", err)
	}
	if inner.calls != 0 || meter.calls != 0 {
		t.Errorf("a call on a spent budget reached the provider %d times and the counter %d",
			inner.calls, meter.calls)
	}
}

// AN UNREADABLE ROOM DOES NOT STOP LEARNING. Unknown is not "no" for best-effort
// work, and the call's own charge is what keeps an unreadable counter from also
// being a free one.
func TestAnUnreadableRoomStillSendsTheAuxiliaryCall(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{err: errors.New("counter unreachable")}
	inner := &answeringProvider{in: 10, out: 10}
	member := meteredHead(t, inner, meter)

	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("an unreadable budget stopped a learning call: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("the provider was called %d times, want 1", inner.calls)
	}
}

// REFLECTION DECLINES TO START AT THE CEILING, and asks without moving the
// counter. A charge of nothing cannot ask it — the counter admits one without
// looking — and a pass that runs and then finds itself over budget has
// already made its auxiliary calls.
func TestReflectionDeclinesToStartAtTheCeiling(t *testing.T) {
	t.Parallel()
	dev := &org.Role{Name: "Dev"}
	c := meteredCompany(100, dev)
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	gate := e.learningBudget(c)

	if ok, err := gate(t.Context(), dev); err != nil || !ok {
		t.Fatalf("gate under the cap = %v, %v; want the pass to run", ok, err)
	}
	if _, err := fleet.PostCharge(t.Context(), scopeOf(t, c, dev), 100); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	ok, err := gate(t.Context(), dev)
	if err != nil {
		t.Fatalf("gate at the cap: %v", err)
	}
	if ok {
		t.Error("reflection started on a company already at its token_budget")
	}
	if used, _ := fleet.Used(t.Context(), coord.OrgScope); used != 100 {
		t.Errorf("asking moved the counter to %d", used)
	}
}

// NO LEARNING WORKER RESOLVES ITS MODEL OFF THE UNMETERED REGISTRY.
//
// The wrapper only helps if every worker goes through it, and the way this
// leak returns is somebody wiring a new worker with `c.Models` because that
// is what the surrounding lines used to say. Nothing about a direct
// `c.Models` reference looks wrong in review — it is the obvious spelling —
// so this is derived from the source rather than left to a reader.
//
// Scoped to the functions that build learning machinery. Everywhere else in
// the engine `c.Models` is the correct thing to read; it is only auxiliary
// LLM work on a seat's behalf that has to be charged.
func TestLearningWorkersResolveModelsThroughTheMeter(t *testing.T) {
	t.Parallel()
	const file = "learning.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	metered := map[string]bool{
		"buildReflectionWorkers": true,
		"learningPasses":         true,
		"auxSummarizer":          true,
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !metered[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			// A CALL, not a mention. `if c.Models == nil` is a fair
			// question to ask of the registry; `c.Models.Head(...)` is the
			// one that resolves a model and skips the counter.
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			head, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || head.Sel.Name != "Head" {
				return true
			}
			sel, ok := head.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Models" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "c" {
				return true
			}
			t.Errorf("%s resolves a model with c.Models.Head at %s — spend "+
				"resolved that way reaches no counter, which is the leak "+
				"meteredModelsFor exists to close. Use e.meteredModelsFor(c).",
				fn.Name.Name, fset.Position(call.Pos()))
			return false
		})
	}
}
