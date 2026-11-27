package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The reflection pass's PRE-FLIGHT gate, and the record that closes it. The
// seam's own cases are in auxiliary_internal_test.go.

// SPEND PAST THE CEILING IS RECORDED, AND IT IS WHAT CLOSES THE GATE. The
// spend has happened at the vendor, so it is recorded whole rather than refused
// for not fitting; otherwise the counter stays under the ceiling and the gate
// keeps reading room for passes that go uncounted.
func TestAnAuxiliaryCompletionPastTheCeilingIsRecordedAndClosesTheGate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	// ONE INSTANT for the 90 spent before, both gates and the completion's
	// record: a wall-clock midnight between any two would move the record or
	// the last gate into a fresh day.
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}
	windows := coord.WindowsAt(budgetNow, time.UTC)
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 90, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	gate := e.auxGate(c, "reflection")
	if ok, err := gate(ctx, lead.Handle()); err != nil || !ok {
		t.Fatalf("gate with 10 left = (%v, %v), want (true, nil)", ok, err)
	}

	seam := auxiliarySeam{heads: staticHeads{provider: &answeringProvider{in: 30, out: 12}},
		org: c.Org, zone: time.UTC, charge: e.auxiliaryCharge(c), now: e.now}
	member, err := seam.Auxiliary(lead, auxspend.Use{Stage: types.AuxStageReflection,
		Purpose: types.AuxPersistDecider, TurnID: "run-1"})
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(ctx, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, lead)} {
		if day := spentIn(t, fleet, scope, windows, period.Day); day != 132 {
			t.Errorf("%s spent %d today, want 132: the 42 the completion spent "+
				"past a ceiling of 100 is still spent", scope, day)
		}
	}
	if ok, err := gate(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("gate after the ceiling was passed = (%v, %v), want (false, nil): "+
			"a pass that starts now spends more past it, uncounted", ok, err)
	}
}

// THE PRE-FLIGHT GATE DECLINES A SEAT WITH NO ROOM LEFT, AND ASKS WITHOUT
// SPENDING: a zero-token charge would be answered OK without looking
// ([Engine.auxGate]), so the gate must read the windows instead.
func TestTheLearningGateDeclinesASeatWithNoRoomLeft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	// The gate reads its windows at the instant the day is filled at.
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}
	gate := e.auxGate(c, "reflection")

	if ok, err := gate(ctx, lead.Handle()); err != nil || !ok {
		t.Fatalf("gate with the whole day left = (%v, %v), want (true, nil)", ok, err)
	}
	windows := coord.WindowsAt(budgetNow, time.UTC)
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 100, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if ok, err := gate(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("gate with the day spent = (%v, %v), want (false, nil): a pass that "+
			"starts now spends past the ceiling", ok, err)
	}
	// And asking moved nothing.
	if day := spentIn(t, fleet, scopeOf(t, c, lead), windows, period.Day); day != 100 {
		t.Fatalf("the seat spent %d today after two questions, want the 100 spent", day)
	}
	// A seat nothing caps is never declined.
	free := &org.Role{Name: "Free"}
	if ok, err := e.auxGate(meteredCompany(config.TokenBudget{}, free), "reflection")(ctx, free.Handle()); err != nil || !ok {
		t.Fatalf("gate for an uncapped seat = (%v, %v), want (true, nil)", ok, err)
	}
}

// A DECLINED REFLECTION IS THE GATE'S REFUSAL, AND THE COUNTER RECORDS IT.
// The seat's day is filled by a post-charge, which stamps nothing, so without
// the gate's record a seat declining every pass reads as having refused
// nothing (on every screen and in `crewlet budgets show`). The seat is stamped,
// not the company, which caps nothing; nothing is counted; a gate with room,
// or an unreadable counter, records nothing.
//
// Mutation: drop the gate's record, and the seat's day carries no stamp.
func TestADeclinedReflectionIsRecordedAsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	base := coordmem.NewFleet()
	fleet, refusals := counted(base, nil)
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	// The gate reads its windows at the instant the day is filled at; the
	// refusal's stamp is the counter's clock, the wall's.
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}
	e.epoch.current.Store(c)
	scope, windows := scopeOf(t, c, lead), coord.WindowsAt(budgetNow, time.UTC)
	gate := e.auxGate(c, "reflection")

	if ok, err := gate(ctx, lead.Handle()); err != nil || !ok {
		t.Fatalf("gate with the whole day left = (%v, %v), want (true, nil)", ok, err)
	}
	if got := refusals.refused(); len(got) != 0 {
		t.Fatalf("refusals recorded = %v for a pass the seat had room for", got)
	}

	if _, err := base.PostCharge(ctx, scope, 100, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	from := time.Now()
	if ok, err := gate(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("gate with the day spent = (%v, %v), want (false, nil)", ok, err)
	}
	if got := refusals.refused(); len(got) != 1 || got[0] != scope {
		t.Fatalf("refusals recorded = %v, want the seat's one", got)
	}
	day := stampOf(t, base, scope, period.Day, windows)
	if day.Before(from) || day.After(time.Now()) {
		t.Fatalf("the seat's day refused_at = %v, want the instant the pass was declined "+
			"(in [%v, now])", day, from)
	}
	if company := stampOf(t, base, coord.OrgScope, period.Day, windows); !company.IsZero() {
		t.Errorf("the company was stamped at %v; it caps nothing and refused nothing", company)
	}

	// UNKNOWN IS NO ROOM (#1838), but no refusal: nothing was read to refuse
	// by, so nothing is stamped.
	unread := &Engine{backends: &Backends{Fleet: unreadableCounted{fleet}}}
	if ok, err := unread.auxGate(c, "reflection")(ctx, lead.Handle()); err == nil || ok {
		t.Fatalf("gate over an unreadable counter = (%v, %v), want (false, an error)", ok, err)
	}
	if got := refusals.refused(); len(got) != 1 {
		t.Errorf("refusals recorded = %v after an unreadable counter, want no more", got)
	}
}

// unreadableCounted is a counted fleet whose counters cannot be read.
type unreadableCounted struct{ countedFleet }

func (unreadableCounted) Used(context.Context, string, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("the coordination store is unreachable")
}

// A PERSON'S WORK IS HELD TO THE COMPANY'S WINDOWS (#1838): a promotion for a
// unit a person leads runs on the lead's chain, charges the company, and has
// no seat window, so the gate reads the company's alone; before, it read
// nothing for anyone not an agent seat and never declined such a pass.
//
// Mutation: answer a person with no meter, and the full company is not read.
func TestAPersonsBackgroundPassIsHeldToTheCompanysWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Founder", Kind: org.KindHuman}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}
	gate := e.auxGate(c, "skill_promotion")
	if ok, err := gate(ctx, lead.Handle()); err != nil || !ok {
		t.Fatalf("gate with the company's day left = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := fleet.PostCharge(ctx, coord.OrgScope, 100, coord.WindowsAt(budgetNow, time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if ok, err := gate(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("gate with the company's day spent = (%v, %v), want (false, nil)", ok, err)
	}
}
