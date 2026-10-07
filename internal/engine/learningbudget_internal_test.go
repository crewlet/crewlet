package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
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
// seam's own cases — what is charged where, and what is recorded — are in
// auxiliary_internal_test.go.

// SPEND PAST THE CEILING IS RECORDED, AND IT IS WHAT CLOSES THE GATE.
//
// The record used to go through the gate's own Charge, which, when a refused
// charge counted nothing, REFUSED a completion that did not fit and so
// recorded nothing: the counter stayed under the ceiling, the pre-flight gate
// still read room, and every later reflection pass ran and went uncounted in
// its turn. The spend has happened at the vendor, so it is recorded whole —
// and the gate then reads no room.
func TestAnAuxiliaryCompletionPastTheCeilingIsRecordedAndClosesTheGate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 90, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	gate := e.learningBudget(c)
	if ok, err := gate(ctx, lead); err != nil || !ok {
		t.Fatalf("gate with 10 left = (%v, %v), want (true, nil)", ok, err)
	}

	seam := auxiliarySeam{heads: staticHeads{provider: &answeringProvider{in: 30, out: 12}},
		org: c.Org, zone: time.UTC, charge: e.auxiliaryCharge(c), now: time.Now}
	member, err := seam.Auxiliary(lead, auxspend.Use{Stage: types.AuxStageReflection,
		Purpose: types.AuxPersistDecider, TurnID: "run-1"})
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(ctx, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, lead)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 132 {
			t.Errorf("%s's day = (%+v, %v), want 132: the 42 the completion spent "+
				"past a ceiling of 100 is still spent", scope, u.In(period.Day), err)
		}
	}
	if ok, err := gate(ctx, lead); err != nil || ok {
		t.Fatalf("gate after the ceiling was passed = (%v, %v), want (false, nil): "+
			"a pass that starts now spends more past it, uncounted", ok, err)
	}
}

// THE PRE-FLIGHT GATE DECLINES A SEAT WITH NO ROOM LEFT, AND ASKS WITHOUT
// SPENDING.
//
// It used to ask with a charge of zero tokens, which the counter answers OK
// without looking — a phase whose provider reported no usage still ran — so
// the gate had never declined anything: a company at its ceiling kept starting
// reflection passes and paying for their auxiliary calls until each one's
// first charge was refused mid-pass.
func TestTheLearningGateDeclinesASeatWithNoRoomLeft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	gate := e.learningBudget(c)

	if ok, err := gate(ctx, lead); err != nil || !ok {
		t.Fatalf("gate with the whole day left = (%v, %v), want (true, nil)", ok, err)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 100, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if ok, err := gate(ctx, lead); err != nil || ok {
		t.Fatalf("gate with the day spent = (%v, %v), want (false, nil): a pass that "+
			"starts now spends past the ceiling", ok, err)
	}
	// And asking moved nothing.
	u, err := fleet.Used(ctx, scopeOf(t, c, lead), windows)
	if err != nil || u.In(period.Day).Used != 100 {
		t.Fatalf("the seat's day = (%+v, %v) after two questions, want the 100 spent",
			u.In(period.Day), err)
	}
	// A seat nothing caps is never declined.
	free := &org.Role{Name: "Free"}
	if ok, err := e.learningBudget(meteredCompany(config.TokenBudget{}, free))(ctx, free); err != nil || !ok {
		t.Fatalf("gate for an uncapped seat = (%v, %v), want (true, nil)", ok, err)
	}
}

// A DECLINED REFLECTION IS THE GATE'S REFUSAL, AND THE COUNTER RECORDS IT.
//
// The seat's day was filled by a post-charge — its own coding run collected,
// or its turn's auxiliary calls — which refuses nothing and stamps nothing.
// The gate then turns away the reflection stage's calls before any is made,
// the pass's and a conversation entry's rewrites alike: unrecorded, a seat
// declining every pass read, on every screen and in `crewlet budgets show`,
// as one that had refused nothing. The seat is stamped, not the company,
// which caps nothing; nothing is counted; a gate with room records nothing;
// and a counter that cannot be read is not a refusal, so it records nothing
// either.
//
// Mutation: drop the gate's record, and the seat's day carries no stamp.
func TestADeclinedReflectionIsRecordedAsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	base := coordmem.NewFleet()
	fleet, refusals := counted(base, nil)
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(c)
	scope, windows := scopeOf(t, c, lead), coord.WindowsAt(time.Now(), time.UTC)
	gate := e.learningBudget(c)

	if ok, err := gate(ctx, lead); err != nil || !ok {
		t.Fatalf("gate with the whole day left = (%v, %v), want (true, nil)", ok, err)
	}
	if got := refusals.refused(); len(got) != 0 {
		t.Fatalf("refusals recorded = %v for a pass the seat had room for", got)
	}

	if _, err := base.PostCharge(ctx, scope, 100, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	from := time.Now()
	if ok, err := gate(ctx, lead); err != nil || ok {
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

	// The conversation entry's rewrites ask the same gate, and are a
	// refusal of their own.
	d := e.buildDispatcher(Options{Dispatch: &Dispatcher{
		NoteDeferred:  func(string) {},
		Completions:   ledgerstore.NewMemoryCompletions(),
		Conversations: ledgerstore.NewMemoryConversations(),
	}}, e.backends)
	if ok, err := d.ReflectionRoom(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("the entry's gate with the day spent = (%v, %v), want (false, nil)", ok, err)
	}
	if got := refusals.refused(); len(got) != 2 {
		t.Errorf("refusals recorded = %v, want a second for the entry's rewrites", got)
	}

	// UNKNOWN IS NOT NO: an unreadable counter declines nothing and records
	// nothing.
	unread := &Engine{backends: &Backends{Fleet: unreadableCounted{fleet}}}
	if ok, err := unread.learningBudget(c)(ctx, lead); err == nil || !ok {
		t.Fatalf("gate over an unreadable counter = (%v, %v), want (true, an error)", ok, err)
	}
	if got := refusals.refused(); len(got) != 2 {
		t.Errorf("refusals recorded = %v after an unreadable counter, want no more", got)
	}
}

// unreadableCounted is a counted fleet whose counters cannot be read.
type unreadableCounted struct{ countedFleet }

func (unreadableCounted) Used(context.Context, string, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("the coordination store is unreachable")
}

// A CONVERSATION ENTRY'S REWRITES ASK THE REFLECTION STAGE'S GATE, on the live
// epoch: they are filed under that stage, and a seat with no room left starts
// no reflection pass and makes none of them either.
//
// Mutation: leave the dispatcher's gate unwired, and a seat whose day is spent
// still reads as having room.
func TestTheDispatcherAsksTheReflectionGateBeforeAnEntrysRewrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(c)
	d := e.buildDispatcher(Options{Dispatch: &Dispatcher{
		NoteDeferred:  func(string) {},
		Completions:   ledgerstore.NewMemoryCompletions(),
		Conversations: ledgerstore.NewMemoryConversations(),
	}}, e.backends)
	if d.ReflectionRoom == nil {
		t.Fatal("the dispatcher has no gate on a conversation entry's rewrites")
	}
	if ok, err := d.ReflectionRoom(ctx, lead.Handle()); err != nil || !ok {
		t.Fatalf("gate with the whole day left = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := fleet.PostCharge(ctx, scopeOf(t, c, lead), 100, coord.WindowsAt(time.Now(), time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if ok, err := d.ReflectionRoom(ctx, lead.Handle()); err != nil || ok {
		t.Fatalf("gate with the day spent = (%v, %v), want (false, nil): the entry's "+
			"rewrites would be spend past the ceiling", ok, err)
	}
}
