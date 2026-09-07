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
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// THE LEAK THIS FILE CLOSES.
//
// `token_budget` was charged in exactly two places — the turn loop and the
// coding sandbox — while the persist decider, the counterparty profiler and
// the episode-compaction summarizer each resolved a model and called
// Provider.Complete directly. That spend was real money and it reached no
// counter, so a company at its ceiling kept paying forever AND the number an
// operator reads understated what had been spent.
//
// The cases below are about the SEAM: a worker that resolves its model the
// ordinary way is charged without knowing it is being charged, which is what
// makes a worker added later charge too.

// countingMeter records what it was asked to spend and whether it refuses.
type countingMeter struct {
	spent  int
	calls  int
	refuse bool
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
// a reflection outage would be the wrong trade, and the pre-flight gate is
// what actually stops the spending.
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

// THE PRE-FLIGHT GATE ASKS WITHOUT SPENDING. A probe that charged a token to
// find out whether it may charge would make the question cost what it is
// asking about.
func TestTheBudgetGateProbesWithAZeroCharge(t *testing.T) {
	t.Parallel()
	meter := &countingMeter{}
	if _, err := meter.Spend(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if meter.spent != 0 {
		t.Errorf("the probe moved the counter by %d", meter.spent)
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
// Scoped to the two functions that build learning machinery. Everywhere else
// in the engine `c.Models` is the correct thing to read; it is only auxiliary
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
