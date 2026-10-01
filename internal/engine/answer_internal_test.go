package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
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

// A PERSON'S ANSWER IS THE COMPANY'S SPEND, and nobody's seat's. It reaches
// the company's day, week and month — so the next turn is judged against room
// the answer already used — and no seat's counter, nor any new scope, because
// a person has no seat budget and a row for them would be one no ceiling can
// ever judge.
func TestAnAnswerIsChargedToTheOrgWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 50}}
	e, fleet := answerEngine(t, meteredCompany(config.TokenBudget{Day: ceiling(1000)}, lead))
	budget := AnswerBudget(e)
	if budget == nil {
		t.Fatal("no answer budget with a fleet to count on")
	}
	if err := budget.Charge(ctx, 420); err != nil {
		t.Fatalf("Charge: %v", err)
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
