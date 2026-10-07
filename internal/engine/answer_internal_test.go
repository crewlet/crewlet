package engine

import (
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
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// answerEngine is an engine holding one epoch and an in-memory fleet, which is
// all a person's answer budget reads.
func answerEngine(t *testing.T, c *Company) (*Engine, *coordmem.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
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
	windows := coord.WindowsAt(time.Now(), time.UTC)
	rows, err := fleet.Usage(ctx, windows)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(rows) != 1 || rows[0].Scope != coord.OrgScope {
		t.Fatalf("counters = %+v, want the company's alone", rows)
	}
	for _, p := range period.Periods {
		if got := rows[0].In(p).Used; got != 420 {
			t.Errorf("the company's %s holds %d, want the answer's 420", p, got)
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
	windows := coord.WindowsAt(time.Now(), time.UTC)

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
	budget := answerBudget{engine: e, budgets: logged, now: time.Now}
	windows := coord.WindowsAt(time.Now(), time.UTC)

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
	budget := answerBudget{engine: e, budgets: logged, now: time.Now}
	if _, err := fleet.PostChargeOrg(ctx, 100, coord.WindowsAt(time.Now(), time.UTC)); err != nil {
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

// A COMPANY WITH NO NATIVE KNOWLEDGE HAS NO CORPUS POSITION, so its answers
// are never cached: an external wiki moves without this node hearing.
func TestAnEngineWithNoNativeKnowledgeNamesNoCorpus(t *testing.T) {
	t.Parallel()
	if position, ok := (&Engine{}).KnowledgeCorpus(); ok || position != "" {
		t.Fatalf("KnowledgeCorpus = (%q, %v), want nothing", position, ok)
	}
}
