package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// The shared token counter's cases.
//
// Run against BOTH certified drivers, because the statement at the heart of it
// is exactly the kind that one accepts and the other does not: a conditional
// upsert whose WHERE clause decides whether the row is written at all, read
// through RowsAffected rather than RETURNING (rewrite/decisions/002).
//
// And because the property being asserted is EXCLUSION under contention, which
// a fake cannot have: "two nodes racing the last of a cap, one wins" is a fact
// about the statement, not about the code around it.

const agentA = "11111111-1111-1111-1111-111111111111"

func testBudgetChargeAccumulates(t *testing.T, db *store.DB) {
	b := db.Budgets()
	for i := range 3 {
		got, err := b.Charge(t.Context(), agentA, 10, 0, 0)
		if err != nil {
			t.Fatalf("charge %d: %v", i, err)
		}
		if !got.OK {
			t.Fatalf("charge %d refused with no limits set: %+v", i, got)
		}
		if want := (i + 1) * 10; got.OrgUsed != want || got.AgentUsed != want {
			t.Errorf("after %d charges: org %d, agent %d, want %d",
				i+1, got.OrgUsed, got.AgentUsed, want)
		}
	}
}

func testBudgetZeroLimitIsUnlimited(t *testing.T, db *store.DB) {
	// `token_budget: 0` is how an operator says "no ceiling". Reading it as
	// "no allowance" would stop every company that never set one.
	got, err := db.Budgets().Charge(t.Context(), agentA, 1_000_000, 0, 0)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if !got.OK {
		t.Errorf("a zero limit refused a charge: %+v", got)
	}
}

func testBudgetRefusalNamesTheScope(t *testing.T, db *store.DB) {
	// "The company is out" and "this seat is out" send an operator to
	// different places, and a bare refusal sends them to neither.
	b := db.Budgets()
	if _, err := b.Charge(t.Context(), agentA, 90, 100, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
	got, err := b.Charge(t.Context(), agentA, 20, 100, 0)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if got.OK {
		t.Fatal("a charge past the org cap was accepted")
	}
	if got.RefusedScope != "org" || got.RefusedUsed != 90 || got.RefusedLimit != 100 {
		t.Errorf("refusal = %+v", got)
	}
}

func testBudgetSeatRefusalUnwindsTheOrgCharge(t *testing.T, db *store.DB) {
	// ORG FIRST, THEN THE SEAT, in one transaction. Charging the org for a
	// turn that never ran would let a company exhaust its budget on work it
	// did not do — and the seat's own cap is the one most likely to bite.
	b := db.Budgets()
	got, err := b.Charge(t.Context(), agentA, 50, 1000, 10)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if got.OK || got.RefusedScope != "agent" {
		t.Fatalf("expected a seat refusal, got %+v", got)
	}
	used, err := b.Used(t.Context(), store.OrgScope)
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used != 0 {
		t.Errorf("the org was charged %d for a turn the seat refused", used)
	}
}

func testBudgetAnOversizeChargeIsScreenedBeforeTheInsert(t *testing.T, db *store.DB) {
	// The WHERE on DO UPDATE guards the UPDATE branch only: a scope with no
	// row yet takes the INSERT, which has no existing value to test. Without
	// the screen, a first-ever charge of a million against a cap of ten
	// would be written.
	b := db.Budgets()
	got, err := b.Charge(t.Context(), agentA, 1_000_000, 10, 0)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if got.OK {
		t.Fatal("a charge larger than the whole cap was accepted on an empty counter")
	}
	if got.RefusedScope != "org" || got.RefusedLimit != 10 {
		t.Errorf("refusal = %+v", got)
	}
	used, err := b.Used(t.Context(), store.OrgScope)
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used != 0 {
		t.Errorf("the screened charge still wrote %d", used)
	}
}

func testBudgetChargeIsExclusiveUnderContention(t *testing.T, db *store.DB) {
	// THE PROPERTY THE TABLE EXISTS FOR. Ten concurrent charges of 10
	// against a cap of 50: exactly five may land, and the counter must not
	// end above the cap. A read-then-write would let several read 40 and
	// all write 50.
	b := db.Budgets()
	const (
		workers = 10
		each    = 10
		cap     = 50
	)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := b.Charge(context.Background(), agentA, each, cap, 0)
			if err != nil {
				return
			}
			if got.OK {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != cap/each {
		t.Errorf("%d charges landed, want exactly %d", accepted, cap/each)
	}
	used, err := b.Used(t.Context(), store.OrgScope)
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used > cap {
		t.Errorf("the counter ended at %d, past the cap of %d", used, cap)
	}
}

func testBudgetZeroTokensIsNotACharge(t *testing.T, db *store.DB) {
	// A phase whose provider reported no usage still ran. Refusing it would
	// stop a company over a backend that omits the field.
	got, err := db.Budgets().Charge(t.Context(), agentA, 0, 10, 10)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if !got.OK {
		t.Errorf("a zero charge was refused: %+v", got)
	}
	used, _ := db.Budgets().Used(t.Context(), store.OrgScope)
	if used != 0 {
		t.Errorf("a zero charge wrote %d", used)
	}
}

func testBudgetSeatsAreSeparateCounters(t *testing.T, db *store.DB) {
	// One seat exhausting its own cap must not stop another. Keyed on the
	// DERIVED agent id, matching the diary — renaming a handle starts a
	// fresh budget rather than inheriting somebody else's spend.
	const agentB = "22222222-2222-2222-2222-222222222222"
	b := db.Budgets()
	if got, _ := b.Charge(t.Context(), agentA, 10, 0, 10); !got.OK {
		t.Fatalf("first seat refused: %+v", got)
	}
	if got, _ := b.Charge(t.Context(), agentA, 5, 0, 10); got.OK {
		t.Fatal("the first seat spent past its own cap")
	}
	got, err := b.Charge(t.Context(), agentB, 5, 0, 10)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if !got.OK {
		t.Errorf("a second seat was refused by the first seat's cap: %+v", got)
	}
}

func testBudgetListPutsTheOrgFirst(t *testing.T, db *store.DB) {
	// The operator surface reads this in order. "org" does not sort before
	// "agent:…" alphabetically, and the two drivers need not agree on
	// collation for a key like this — so the order is fixed in code.
	b := db.Budgets()
	if _, err := b.Charge(t.Context(), agentA, 10, 0, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
	got, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d scopes, want the org and one seat", len(got))
	}
	if got[0].Scope != store.OrgScope {
		t.Errorf("first scope = %q, want the org", got[0].Scope)
	}
	if got[0].UpdatedAt.IsZero() {
		t.Error("no timestamp, so an operator cannot tell a live counter from a stale one")
	}
}

func testBudgetResetIsScoped(t *testing.T, db *store.DB) {
	// An operator action, never a schedule: a table that rolled itself over
	// would silently re-arm a company somebody had stopped on purpose.
	b := db.Budgets()
	if _, err := b.Charge(t.Context(), agentA, 10, 0, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if _, err := b.Reset(t.Context(), store.AgentScope(agentA)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if used, _ := b.Used(t.Context(), store.AgentScope(agentA)); used != 0 {
		t.Errorf("the seat's counter survived its own reset: %d", used)
	}
	if used, _ := b.Used(t.Context(), store.OrgScope); used != 10 {
		t.Errorf("a scoped reset cleared the org too: %d", used)
	}
	if _, err := b.Reset(t.Context(), ""); err != nil {
		t.Fatalf("reset all: %v", err)
	}
	if used, _ := b.Used(t.Context(), store.OrgScope); used != 0 {
		t.Errorf("the org survived a full reset: %d", used)
	}
}
