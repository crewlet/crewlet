package engine

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// answerEngine is an engine holding one epoch and an in-memory fleet, which is
// all a person's answer budget reads, on a clock pinned to budgetNow: the
// answer cuts its windows at the engine's instant, and the case cuts the ones
// it reads back in at the same one.
func answerEngine(t *testing.T, c *Company) (*Engine, *coordmem.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}
	e.epoch.current.Store(c)
	return e, fleet
}

// A PERSON'S ANSWER IS THE COMPANY'S SPEND, and nobody's seat's. Resolved
// through the seam on the operator stage, it reaches the company's day, week
// and month — so the next turn is judged against room the answer already used
// — and no seat's counter, nor any new scope, because a person has no seat
// budget and a row for them would be one no ceiling can ever judge. Even when
// the seat the person's credential is bound to runs as an agent.
func TestAnAnswerIsChargedToTheOrgWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 50}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(1000)}, lead)
	registry, err := phase.NewRegistry([]phase.Entry{{Key: "cheap",
		Provider: &answeringProvider{in: 400, out: 20}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = registry
	e, fleet := answerEngine(t, c)
	if AnswerBudget(e) == nil {
		t.Fatal("no answer budget with a fleet to count on")
	}
	member, err := AnswerModels(e).Auxiliary(lead, auxspend.Use{Stage: types.AuxStageOperator,
		Purpose: types.AuxAnswerKnowledge})
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(ctx, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	windows := coord.WindowsAt(budgetNow, time.UTC)
	rows, err := fleet.Usage(ctx, windows)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(rows) != 1 || rows[0].Scope != coord.OrgScope {
		t.Fatalf("counters = %+v, want the company's alone", rows)
	}
	for i, p := range period.Periods {
		// THE WINDOW AS WELL AS THE FIGURE: see spentIn.
		if got := rows[0].In(p); got.Used != 420 || got.Window.Label != windows[i].Label {
			t.Errorf("the company's %s holds %d in %q, want the answer's 420 in %q",
				p, got.Used, got.Window.Label, windows[i].Label)
		}
	}
}

// THE GATE IS THE COMPANY'S WINDOWS, by the rule the budget park uses: a
// spent company window refuses an answer, naming the window that ends last and
// when it resets — and a SEAT's spent ceiling does not, since the person
// asking is not that seat.
func TestAnAnswerIsRefusedOnlyByASpentCompanyWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 10}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100), Month: ceiling(100)}, lead)
	e, fleet := answerEngine(t, c)
	budget := AnswerBudget(e)
	windows := coord.WindowsAt(budgetNow, time.UTC)

	// The seat is out; the company is not.
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 20, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if _, refusing, err := budget.Refusing(ctx); err != nil || refusing {
		t.Fatalf("Refusing with the company's room = (%v, %v), want (false, nil)", refusing, err)
	}

	if _, err := fleet.PostChargeOrg(ctx, 80, windows); err != nil {
		t.Fatalf("PostChargeOrg: %v", err)
	}
	refusal, refusing, err := budget.Refusing(ctx)
	if err != nil || !refusing {
		t.Fatalf("Refusing with the company's day and month spent = (%v, %v), want (true, nil)",
			refusing, err)
	}
	month := windows[2]
	if refusal.Period != string(period.Month) || refusal.Window != month.Label ||
		!refusal.ResetsAt.Equal(month.End) || refusal.Used != 100 || refusal.Limit != 100 {
		t.Errorf("refusal = %+v, want the month (it ends last), 100 of 100, resetting at %v",
			refusal, month.End)
	}
}

// A REFUSED ANSWER IS THE GATE'S REFUSAL, AND THE COMPANY'S COUNTER RECORDS IT.
//
// The company's day was filled by post-charges — a person's earlier answers, a
// coding run, a background pass — which refuse nothing and stamp nothing, so
// no charge was ever refused there. The gate refuses the question before any
// call is made, and that is the call the window turned away: unrecorded, a
// company refusing every question asked of it read, on every screen and in
// `crewlet budgets show`, as one that had refused nothing. Only the company is
// stamped, since a person has no seat budget; nothing is counted; and a
// question the company has room for records nothing at all.
//
// Mutation: drop the gate's record, and the company's day carries no stamp.
func TestARefusedAnswerIsRecordedAsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, &org.Role{Name: "Lead"})
	e, fleet := answerEngine(t, c)
	logged, refusals := counted(fleet, nil)
	budget := answerBudget{engine: e, budgets: logged}
	windows := coord.WindowsAt(budgetNow, time.UTC)

	if _, err := fleet.PostChargeOrg(ctx, 99, windows); err != nil {
		t.Fatalf("PostChargeOrg: %v", err)
	}
	if _, refusing, err := budget.Refusing(ctx); err != nil || refusing {
		t.Fatalf("Refusing with a token of room = (%v, %v), want (false, nil)", refusing, err)
	}
	if got := refusals.refused(); len(got) != 0 {
		t.Fatalf("refusals recorded = %v for a question the company had room for", got)
	}

	if _, err := fleet.PostChargeOrg(ctx, 1, windows); err != nil {
		t.Fatalf("PostChargeOrg: %v", err)
	}
	from := time.Now()
	if _, refusing, err := budget.Refusing(ctx); err != nil || !refusing {
		t.Fatalf("Refusing with the company's day spent = (%v, %v), want (true, nil)", refusing, err)
	}
	if got := refusals.refused(); len(got) != 1 || got[0] != coord.OrgScope {
		t.Fatalf("refusals recorded = %v, want the company's one", got)
	}
	u, err := fleet.Used(ctx, coord.OrgScope, windows)
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	day := u.In(period.Day)
	if day.RefusedAt.Before(from) || day.RefusedAt.After(time.Now()) || day.Used != 100 {
		t.Fatalf("the company's day = %+v, want the 100 spent, refused at the question "+
			"(in [%v, now])", day, from)
	}
}

// A REFUSED ANSWER STAYS REFUSED WHEN ITS RECORD CANNOT BE WRITTEN: the stamp
// is the report of the refusal, and a counter that could not take it changes
// nothing about the question it was asked of.
func TestARefusedAnswerStandsWhenItsRecordFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, &org.Role{Name: "Lead"})
	e, fleet := answerEngine(t, c)
	logged, refusals := counted(fleet, errors.New("the counter is unreachable"))
	budget := answerBudget{engine: e, budgets: logged}
	if _, err := fleet.PostChargeOrg(ctx, 100, coord.WindowsAt(budgetNow, time.UTC)); err != nil {
		t.Fatalf("PostChargeOrg: %v", err)
	}
	refusal, refusing, err := budget.Refusing(ctx)
	if err != nil || !refusing || refusal.Period != string(period.Day) || refusal.Used != 100 {
		t.Fatalf("Refusing with an unwritable record = (%+v, %v, %v), want the day's "+
			"refusal all the same", refusal, refusing, err)
	}
	if got := refusals.refused(); len(got) != 1 {
		t.Errorf("record attempts = %v, want the one", got)
	}
}

// NO FLEET, NO TOOL: an answer that could spend against no counter at all is
// the shape the budget exists to refuse, so there is no budget to wire.
func TestWithNoFleetThereIsNoAnswerBudget(t *testing.T) {
	t.Parallel()
	if AnswerBudget(&Engine{backends: &Backends{}}) != nil {
		t.Fatal("an answer budget was built with no counter behind it")
	}
}

// THE KNOWLEDGE CORPUS MOVES WITH A WRITE AND WITH NO READ, on a real node.
//
// The answer cache keys an answer on where the tracker's, the pages' and the
// vectors' rows last changed. A linearizable read on a log appends a barrier
// there, which every data node applies and which changes no row: keyed on how
// far the logs were applied, every read anywhere in the fleet retired every
// cached answer — an answer's own reads of its sources the key it was stored
// under — and a repeated question paid the answering model again. A page
// written moves it.
func TestTheKnowledgeCorpusMovesWithAWriteAndWithNoRead(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, trashCompanyDoc))
	kb := e.PagesStore()
	if kb == nil || e.NativeSearcher() == nil {
		t.Fatal("the premise: the node writes and searches a native knowledge base")
	}
	jane := pages.Actor{Handle: "jane", Kind: pages.AuthorHuman}
	createEventually(t, kb, jane, pages.NewPage{
		Container: "DOCS", Title: "Runbook", Body: "the steps",
	})
	s := e.core.Load().log
	logs := []*runningLog{
		s.Domain(tracker.Domain{}.Name()), s.Domain(pages.Domain{}.Name()),
		s.Domain(search.Domain{}.Name()),
	}
	// AT REST: every log applied through its end, the page's write and the
	// boot's included.
	caughtUp := func() bool {
		for _, running := range logs {
			stats, err := running.log.Stats(t.Context())
			if err != nil || running.runner.Committed().Seq < stats.LastSeq {
				return false
			}
		}
		return true
	}
	waitUntil(t, 20*time.Second, "the node's logs to be applied through their ends", caughtUp)
	corpus := func() string {
		t.Helper()
		at, ok := e.KnowledgeCorpus()
		if !ok {
			t.Fatal("a node with a native knowledge base names no corpus")
		}
		return at
	}
	before := corpus()

	for _, running := range logs[:2] {
		if _, err := running.reader.Read(t.Context(), statelog.Query{
			Level: statelog.ReadLinearizable,
			Scope: statelog.ScopeSet{Paths: []string{"t"}},
		}, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("a linearizable read on %s: %v", running.domain.Name(), err)
		}
	}
	waitUntil(t, 20*time.Second, "the reads' barriers applied", caughtUp)
	if after := corpus(); after != before {
		t.Fatalf("linearizable reads on the tracker's and the pages' logs moved the "+
			"corpus from %q to %q — every read in the fleet retires every cached answer",
			before, after)
	}

	createEventually(t, kb, jane, pages.NewPage{
		Container: "DOCS", Title: "Second runbook", Body: "more steps",
	})
	waitUntil(t, 20*time.Second, "a page written to move the corpus", func() bool {
		return corpus() != before
	})
}

// A COMPANY WITH NO NATIVE KNOWLEDGE HAS NO CORPUS POSITION, so its answers
// are never cached: an external wiki moves without this node hearing.
func TestAnEngineWithNoNativeKnowledgeNamesNoCorpus(t *testing.T) {
	t.Parallel()
	if position, ok := (&Engine{}).KnowledgeCorpus(); ok || position != "" {
		t.Fatalf("KnowledgeCorpus = (%q, %v), want nothing", position, ok)
	}
}
