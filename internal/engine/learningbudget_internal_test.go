package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
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

// WITH NO CEILING TO ENFORCE NOTHING IS CHARGED AND EVERYTHING IS RECORDED. An
// unlimited company pays no round trip per auxiliary call to be told "yes" —
// the same reason meterFor returns nil rather than an always-allow meter — and
// what it spent is still a question with an answer, so the record is written
// whether or not a counter exists to charge.
func TestWithNoBudgetACompletionIsRecordedAndNotCharged(t *testing.T) {
	t.Parallel()
	var records []types.AuxiliaryCallCompleted
	models := meteredModels{
		inner: staticModels{provider: &answeringProvider{in: 5, out: 7}},
		record: func(_ context.Context, _ *org.Role, spend types.AuxiliaryCallCompleted) {
			records = append(records, spend)
		},
	}
	member, err := models.Head(&org.Role{Name: "Dev"}, phase.Auxiliary)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(records) != 1 || records[0].TotalTokens != 12 {
		t.Fatalf("records = %+v, want the one completion's 12 tokens — auxiliary "+
			"spend that reaches no record is spend no rollup can show", records)
	}
}

// A RECORD SAYS WHO SPENT IT, AND FOR WHICH TURN. The seat, the worker, the
// model the completion named, the configured key it was served under and the
// turn it answered — each the thing a per-worker, per-model or per-turn row is
// keyed on, bound once by the caller that knows it rather than read back out
// of the context.
func TestAnAuxiliaryRecordNamesItsSeatWorkerModelAndTurn(t *testing.T) {
	t.Parallel()
	var got []types.AuxiliaryCallCompleted
	base := meteredModels{
		inner: staticModels{provider: &answeringProvider{in: 30, out: 3}},
		record: func(_ context.Context, _ *org.Role, spend types.AuxiliaryCallCompleted) {
			got = append(got, spend)
		},
	}
	bound := base.For(learning.Attribution{Worker: learning.PersistSource, TurnID: "run-1", WorkKey: "wk-1"})
	member, err := bound.Head(&org.Role{Name: "Dev"}, phase.Auxiliary)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := types.AuxiliaryCallCompleted{
		RoleName: "Dev", TurnID: "run-1", WorkKey: "wk-1",
		Phase: types.PhaseAuxiliary, Worker: learning.PersistSource,
		Model: "test-model", ProviderKey: "aux",
		InputTokens: 30, OutputTokens: 3, TotalTokens: 33,
	}
	if len(got) != 1 {
		t.Fatalf("published %d records, want 1", len(got))
	}
	got[0].DurationMS = 0
	if got[0] != want {
		t.Errorf("record = %+v\nwant     %+v", got[0], want)
	}

	// BINDING IS A COPY: the wrapper it was bound from still names nobody,
	// so one worker's attribution cannot leak onto another's calls.
	member, _ = base.Head(&org.Role{Name: "Dev"}, phase.Auxiliary)
	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got[1].Worker != "" || got[1].TurnID != "" {
		t.Errorf("the unbound wrapper recorded %q/%q, want nobody's", got[1].Worker, got[1].TurnID)
	}
}

// A LEARNING WORKER'S CALL IS RECORDED UNDER ITS OWN NAME, through the one
// seam every worker resolves its model by: bound by learning's own resolution,
// never left for the call site to remember.
func TestALearningWorkersCallIsRecordedUnderItsName(t *testing.T) {
	t.Parallel()
	var got []types.AuxiliaryCallCompleted
	models := meteredModels{
		inner: staticModels{provider: &answeringProvider{in: 4, out: 1}},
		record: func(_ context.Context, _ *org.Role, spend types.AuxiliaryCallCompleted) {
			got = append(got, spend)
		},
	}
	decider, err := learning.NewPersistDecider(models, nopDiary{}, learning.PersistOptions{})
	if err != nil {
		t.Fatalf("NewPersistDecider: %v", err)
	}
	turn := learning.Turn{Role: &org.Role{Name: "Dev"}, Event: types.TurnCompleted{TurnID: "run-9", WorkKey: "wk-9"}}
	_, _ = decider.Decide(t.Context(), turn)
	if len(got) != 1 {
		t.Fatalf("published %d records, want the decider's one completion", len(got))
	}
	if got[0].Worker != learning.PersistSource || got[0].TurnID != "run-9" || got[0].WorkKey != "wk-9" {
		t.Errorf("record = %+v, want the persist decider's name and the turn it reflected on", got[0])
	}
}

// nopDiary is a diary with nothing in it that accepts every write.
type nopDiary struct{}

func (nopDiary) Write(context.Context, learning.DiaryEntry) error { return nil }

func (nopDiary) Recent(context.Context, string, time.Time, int) ([]learning.DiaryEntry, error) {
	return nil, nil
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

// NO AUXILIARY CALLER RESOLVES ITS MODEL OFF THE UNMETERED REGISTRY.
//
// The wrapper only helps if every auxiliary caller goes through it, and the way
// this leak returns is somebody wiring a new worker with `c.Models` because
// that is what the surrounding lines used to say. Nothing about a direct
// `c.Models` reference looks wrong in review — it is the obvious spelling — so
// this is derived from the source rather than left to a reader. A completion
// resolved off the bare registry is spend no record and no counter hears about.
//
// Scoped to where auxiliary machinery is built. Everywhere else in the engine
// `c.Models` is the correct thing to read; it is only auxiliary LLM work on a
// seat's behalf that has to be recorded and charged.
func TestLearningWorkersResolveModelsThroughTheMeter(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parse := func(file string) *ast.File {
		t.Helper()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		return parsed
	}

	// learning.go: the workers and background passes, which call Head.
	metered := map[string]bool{
		"buildReflectionWorkers": true,
		"learningPasses":         true,
		"auxSummarizer":          true,
	}
	for _, decl := range parse("learning.go").Decls {
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
			if registryRead(head.X) {
				t.Errorf("%s resolves a model with c.Models.Head at %s — spend "+
					"resolved that way reaches no record and no counter, which is "+
					"the leak meteredModelsFor exists to close. Use e.meteredModelsFor(c).",
					fn.Name.Name, fset.Position(call.Pos()))
			}
			return false
		})
	}

	// prefetch.go: the turn-start prefetch, which never calls Head itself —
	// it HANDS a registry to the prefetch package, so the leak there is the
	// value its Models field is set from, in a literal or an assignment.
	handed := 0
	check := func(fn string, value ast.Expr) {
		handed++
		if registryRead(value) {
			t.Errorf("%s hands the prefetch the unmetered registry at %s — its "+
				"briefing, knowledge and memory calls would reach no record and "+
				"no counter. Use e.auxiliaryModelsFor(company, who).",
				fn, fset.Position(value.Pos()))
		}
	}
	for _, decl := range parse("prefetch.go").Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "Models" {
					check(fn.Name.Name, node.Value)
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Models" && i < len(node.Rhs) {
						check(fn.Name.Name, node.Rhs[i])
					}
				}
			}
			return true
		})
	}
	// A GUARD THAT FOUND NOTHING TO CHECK CERTIFIES NOTHING: the prefetch's
	// sources moving out of this file must move this check with them.
	if handed == 0 {
		t.Error("prefetch.go sets no Models field any more — this guard is asserting nothing; " +
			"point it at wherever the prefetch's sources are built")
	}
}

// registryRead reports whether expr reads a company's model registry directly:
// a `<company>.Models` selector, the unmetered field itself.
func registryRead(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Models" {
		return false
	}
	_, ok = sel.X.(*ast.Ident)
	return ok
}

// THE ENGINE'S WIRING RECORDS WITH NO COUNTER AT ALL. A node with no fleet
// counter charges nothing, and every completion made through the seam is still
// published as the seat's auxiliary spend — attributed to the seat's agent id
// off the company the model was resolved for, and to whomever the caller
// bound.
func TestTheSeamRecordsOnANodeWithNoCounter(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	got := make(chan types.AuxiliaryCallCompleted, 1)
	if err := q.Subscribe(t.Context(), topics.Event(types.AuxiliaryCallCompleted{}.EventType()), "probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*types.AuxiliaryCallCompleted](ev); ok && p != nil {
				got <- *p
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	registry, err := phase.NewRegistry([]phase.Entry{{Key: "aux", Provider: &answeringProvider{in: 11, out: 2}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	lead := &org.Role{Name: "Tech Lead", DeclaredHandle: "lead"}
	company := &Company{
		Config: &config.Company{},
		Org:    &org.Organization{Name: "Nimbus", Roles: []*org.Role{lead}},
		Models: registry,
	}
	e := &Engine{backends: &Backends{Queue: q}}

	models := e.auxiliaryModelsFor(company, learning.Attribution{Worker: learning.PrefetchWorker, TurnID: "run-3"})
	member, err := models.Head(lead, phase.Auxiliary)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := member.Provider.Complete(t.Context(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	agentID, _ := company.Org.AgentIDFor(lead)
	select {
	case rec := <-got:
		if rec.Agent != agentID.String() || rec.RoleName != "Tech Lead" ||
			rec.Worker != learning.PrefetchWorker || rec.TurnID != "run-3" || rec.TotalTokens != 13 {
			t.Errorf("record = %+v, want the seat's agent id, the prefetch and 13 tokens", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no auxiliary_call_completed was published — the company's auxiliary spend reached no record")
	}
}
