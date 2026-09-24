package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

// counters is a token counter whose reads the test controls.
type counters struct {
	used map[string]int
	err  error
}

func (c counters) Used(_ context.Context, scope string) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.used[scope], nil
}

func (c counters) Charge(context.Context, string, int, int, int) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (c counters) PostCharge(context.Context, string, int) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (c counters) Usage(context.Context) ([]coord.Usage, error) { return nil, nil }
func (c counters) Reset(context.Context, string) (int, error)   { return 0, nil }

// THE HEADROOM IS THE TIGHTER OF THE TWO CAPS.
//
// A charge is checked against both, so a seat with room under its own cap and
// none under the company's has no room. Reporting the looser one would let a
// fan-out size itself against an allowance it cannot spend.
func TestTheHeadroomIsTheTighterCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		orgLimit, agentLimit int
		orgUsed, agentUsed   int
		want                 int
	}{
		{"the org is tighter", 1000, 1000, 900, 100, 100},
		{"the seat is tighter", 1000, 200, 100, 150, 50},
		{"only the org is capped", 1000, 0, 400, 0, 600},
		{"only the seat is capped", 0, 300, 0, 100, 200},
		// A scope that has spent its whole allowance HAS zero headroom.
		// Reading that as "not set yet" would let the other scope's room
		// overwrite it — an exhausted company reading as an uncapped one
		// at exactly the moment the cap matters.
		{"an exhausted org is zero, not unset", 500, 10_000, 500, 0, 0},
		{"an overspent scope is zero, never negative", 500, 0, 900, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &meter{
				budgets:    counters{used: map[string]int{coord.OrgScope: tc.orgUsed, "agent:x": tc.agentUsed}},
				agentScope: "agent:x", orgLimit: tc.orgLimit, agentLimit: tc.agentLimit,
			}
			got, err := m.Remaining(t.Context())
			if err != nil {
				t.Fatalf("Remaining: %v", err)
			}
			if got != tc.want {
				t.Errorf("headroom = %d, want %d", got, tc.want)
			}
		})
	}
}

// AN UNREACHABLE COUNTER IS AN ERROR, NEVER A ZERO.
//
// subagent reads a ParentRemaining of zero as UNCAPPED, so collapsing an
// unreadable store to 0 would hand a fan-out no ceiling on exactly the failure
// a budget exists for — the fail-OPEN direction, on the one path where money
// leaves the building per token.
func TestAnUnreachableCounterRefusesRatherThanReportingZero(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("the coordination store is unreachable")},
		agentScope: "agent:x", orgLimit: 1000,
	}
	if _, err := m.Remaining(t.Context()); err == nil {
		t.Fatal("an unreadable counter reported a headroom")
	}
}

// AN UNCAPPED SEAT READS ZERO WITH NO ERROR AND NO STORE ROUND TRIP, which is
// the same "no ceiling" a company that set no budget already has.
func TestAnUncappedSeatNeedsNoCounterRead(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("this must not be called")},
		agentScope: "agent:x",
	}
	got, err := m.Remaining(t.Context())
	if err != nil || got != 0 {
		t.Errorf("Remaining = (%d, %v), want (0, nil)", got, err)
	}
}

// A REFUSED ROUND IS COUNTED ALL THE SAME.
//
// A round is charged once its model call has answered, so the provider has
// billed it whatever the gate then says. The gate counts nothing it refuses,
// and a meter that stopped there left the counter below what the company had
// spent by exactly the rounds that found it at its cap, while every phase
// record reported them.
func TestARefusedRoundIsCountedAllTheSame(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	m := &meter{budgets: fleet, agentScope: "agent:x", orgLimit: 100}

	if got, err := m.Spend(t.Context(), 90); err != nil || !got.OK {
		t.Fatalf("Spend(90) = %+v, %v; want it admitted", got, err)
	}
	got, err := m.Spend(t.Context(), 20)
	if err != nil {
		t.Fatalf("Spend(20): %v", err)
	}
	// The REFUSAL is still the answer, and it names what the gate saw.
	if got.OK || got.Scope != "org" || got.Used != 90 || got.Limit != 100 {
		t.Fatalf("Spend(20) = %+v, want the company refusing at 90 of 100", got)
	}
	for _, scope := range []string{coord.OrgScope, "agent:x"} {
		used, err := fleet.Used(t.Context(), scope)
		if err != nil || used != 110 {
			t.Errorf("%s reads %d (%v), want the 110 the provider billed", scope, used, err)
		}
	}
	usage, err := fleet.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, row := range usage {
		if row.Scope == coord.OrgScope && row.RefusedAt.IsZero() {
			t.Error("the company's refusal was not stamped: the post-charge must leave it alone")
		}
	}
}

// failingPostCharge is a counter whose gate works and whose post-charge does
// not.
type failingPostCharge struct{ coord.Budgets }

func (failingPostCharge) PostCharge(context.Context, string, int) (coord.Spend, error) {
	return coord.Spend{}, errors.New("the coordination store is unreachable")
}

// A POST-CHARGE THAT FAILS LEAVES THE REFUSAL THE ANSWER. The caller acts on
// the refusal — the seat is out of budget — and an error in its place would
// report an outage instead, over a write whose only cost when lost is a
// counter reading low by one round.
func TestAFailedPostChargeLeavesTheRefusalTheAnswer(t *testing.T) {
	t.Parallel()
	m := &meter{budgets: failingPostCharge{coordmem.NewFleet()}, agentScope: "agent:x", orgLimit: 10}
	got, err := m.Spend(t.Context(), 20)
	if err != nil {
		t.Fatalf("a failed post-charge replaced the refusal with an error: %v", err)
	}
	if got.OK || got.Scope != "org" {
		t.Errorf("Spend = %+v, want the company's refusal", got)
	}
}

// ROOM IS THE SPEND AGAINST EACH CAP, the company's read first.
func TestRoomIsTheSpendAgainstEachCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		orgLimit, agentLimit int
		orgUsed, agentUsed   int
		want                 toolloop.SpendOutcome
	}{
		{"both under", 1000, 500, 999, 499, toolloop.SpendOutcome{OK: true}},
		{"the company at its cap", 1000, 500, 1000, 0,
			toolloop.SpendOutcome{Scope: "org", Used: 1000, Limit: 1000}},
		{"the seat past its cap", 1000, 500, 600, 520,
			toolloop.SpendOutcome{Scope: "agent", Used: 520, Limit: 500}},
		// "The company is out" is what an operator acts on: raising the
		// seat's ceiling against an exhausted company changes nothing.
		{"both out, the company named", 100, 100, 150, 150,
			toolloop.SpendOutcome{Scope: "org", Used: 150, Limit: 100}},
		{"only the seat capped, at it", 0, 300, 5000, 300,
			toolloop.SpendOutcome{Scope: "agent", Used: 300, Limit: 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &meter{
				budgets:    counters{used: map[string]int{coord.OrgScope: tc.orgUsed, "agent:x": tc.agentUsed}},
				agentScope: "agent:x", orgLimit: tc.orgLimit, agentLimit: tc.agentLimit,
			}
			got, err := m.Room(t.Context())
			if err != nil {
				t.Fatalf("Room: %v", err)
			}
			if got != tc.want {
				t.Errorf("Room = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// AN UNREADABLE ROOM IS AN ERROR, never a yes and never a refusal: each caller
// decides which way it fails, and it cannot if the meter decided for it.
func TestAnUnreadableRoomIsAnError(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("the coordination store is unreachable")},
		agentScope: "agent:x", orgLimit: 1000,
	}
	if got, err := m.Room(t.Context()); err == nil {
		t.Fatalf("an unreadable counter answered %+v", got)
	}
}

// A RAISED CAP HAS ROOM AGAIN, whatever the counter's refusal stamp says.
//
// The stamp clears only on an admitted charge, and nothing is charged that is
// never sent: a room that read the stamp would keep a seat stopped after its
// cap was raised, until somebody reset a counter that already had room.
func TestARaisedCapHasRoomAfterARefusal(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	pinned := &meter{budgets: fleet, agentScope: "agent:x", orgLimit: 100}
	if _, err := pinned.Spend(t.Context(), 90); err != nil {
		t.Fatalf("Spend(90): %v", err)
	}
	if got, err := pinned.Spend(t.Context(), 20); err != nil || got.OK {
		t.Fatalf("Spend(20) = %+v, %v; want a refusal", got, err)
	}
	// Under the cap that refused, the counter reads past it: no room.
	if got, err := pinned.Room(t.Context()); err != nil || got.OK || got.Used != 110 {
		t.Errorf("Room at the old cap = %+v, %v; want none, at the 110 spent", got, err)
	}
	// A revision raised it. The refusal stamp is still on the counter.
	raised := &meter{budgets: fleet, agentScope: "agent:x", orgLimit: 1000}
	if got, err := raised.Room(t.Context()); err != nil || !got.OK {
		t.Errorf("Room under a raised cap = %+v, %v; want room", got, err)
	}
}
