package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/period"
)

// collectedAt is the instant the runs here are collected at, on UTC.
var collectedAt = time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)

// accountant is a sandboxAccountant over fleet, judging a run against the
// given day ceilings (0 leaving that scope uncapped) and collecting it at
// collectedAt.
func accountant(fleet *coordmem.Fleet, org, seat coord.Caps) sandboxAccountant {
	return sandboxAccountant{
		budgets: fleet,
		basis:   func(string) budgetBasis { return budgetBasis{org: org, seat: seat, zone: time.UTC} },
		now:     func() time.Time { return collectedAt },
	}
}

// dayUsed is a scope's spend in the day of collectedAt.
func dayUsed(ctx context.Context, t *testing.T, fleet *coordmem.Fleet, scope string) int {
	t.Helper()
	u, err := fleet.Used(ctx, scope, coord.WindowsAt(collectedAt, time.UTC))
	if err != nil {
		t.Fatalf("Used(%s): %v", scope, err)
	}
	return u.In(period.Day).Used
}

// A COLLECTED RUN'S SPEND IS RECORDED EVEN WHEN IT OVERRAN THE CAP.
//
// The run already spent it, minutes or hours earlier and possibly on another
// node, so no answer can un-spend it. Charging it through the gate recorded
// NOTHING whenever it did not fit, which is exactly when the cap binds: the
// counter under-stated the company's spend by the whole run, the next round was
// admitted against room the run had already used, and the refusal the gate
// stamped told the dashboard the seat was refusing charges it would still take.
func TestACollectedRunIsRecordedEvenPastTheCap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	scope := coord.AgentScope("a-1")
	orgCaps, seatCaps := coord.Caps{period.Day: 1000}, coord.Caps{period.Day: 100}
	windows := coord.WindowsAt(collectedAt, time.UTC)
	if got, err := fleet.Charge(ctx, coord.ChargeRequest{
		Seat: scope, Tokens: 90, Windows: windows, OrgCaps: orgCaps, SeatCaps: seatCaps,
	}); err != nil || !got.OK {
		t.Fatalf("setup charge = (%+v, %v)", got, err)
	}

	over, err := accountant(fleet, orgCaps, seatCaps).Charge(ctx, "a-1", "lead", 50)
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if !over {
		t.Error("a run that took the seat past its cap was not reported over")
	}
	for _, s := range []string{coord.OrgScope, scope} {
		if used := dayUsed(ctx, t, fleet, s); used != 140 {
			t.Errorf("%s used = %d, want the 140 the company actually spent", s, used)
		}
	}
	rows, err := fleet.Usage(ctx, windows)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, row := range rows {
		if !row.In(period.Day).RefusedAt.IsZero() {
			t.Errorf("%s reads as refusing after a run was recorded: nothing was refused", row.Scope)
		}
	}
	// And the gate now judges the next round against what was really spent.
	if got, err := fleet.Charge(ctx, coord.ChargeRequest{
		Seat: scope, Tokens: 1, Windows: windows, OrgCaps: orgCaps, SeatCaps: seatCaps,
	}); err != nil || got.OK {
		t.Errorf("next round = (%+v, %v), want it refused against the recorded run", got, err)
	}
}

// A RUN INSIDE THE CAP IS RECORDED AND NOT OVER, and a company with no caps at
// all is never over, so the warning means something when it appears.
func TestACollectedRunInsideTheCapIsNotOver(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		name          string
		org, seat     coord.Caps
		wantOver      bool
		tokens, prior int
	}{
		{"inside both caps", coord.Caps{period.Day: 1000}, coord.Caps{period.Day: 100}, false, 10, 50},
		{"exactly at the seat cap", coord.Caps{period.Day: 1000}, coord.Caps{period.Day: 100}, false, 50, 50},
		{"past the company cap only", coord.Caps{period.Day: 120}, nil, true, 80, 50},
		{"past the company's month only", coord.Caps{period.Day: 10_000, period.Month: 120}, nil, true, 80, 50},
		{"uncapped", nil, nil, false, 5000, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fleet := coordmem.NewFleet()
			scope := coord.AgentScope("a-1")
			if _, err := fleet.PostCharge(ctx, scope, tc.prior, coord.WindowsAt(collectedAt, time.UTC)); err != nil {
				t.Fatalf("setup: %v", err)
			}
			over, err := accountant(fleet, tc.org, tc.seat).Charge(ctx, "a-1", "lead", tc.tokens)
			if err != nil {
				t.Fatalf("Charge: %v", err)
			}
			if over != tc.wantOver {
				t.Errorf("over = %v, want %v", over, tc.wantOver)
			}
			if used := dayUsed(ctx, t, fleet, scope); used != tc.prior+tc.tokens {
				t.Errorf("seat used = %d, want %d", used, tc.prior+tc.tokens)
			}
		})
	}
}

// A RUN IS COUNTED IN THE WINDOWS IT IS COLLECTED IN, on the company's clock.
//
// The collection is the only instant the counter is told about, so that is the
// day the run's spend lands in — and it is the company's day: collected at
// 23:30 in Los Angeles, the run is the 22nd's spend there, although UTC's clock
// reached the 23rd hours earlier. Cut on UTC, a company's evening would be
// charged to a day it has not started.
func TestACollectedRunIsCountedInTheCompanysCollectionDay(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fleet := coordmem.NewFleet()
	collected := time.Date(2026, time.September, 22, 23, 30, 0, 0, la)
	a := sandboxAccountant{
		budgets: fleet,
		basis:   func(string) budgetBasis { return budgetBasis{zone: la} },
		now:     func() time.Time { return collected },
	}
	if _, err := a.Charge(t.Context(), "a-1", "lead", 70); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	u, err := fleet.Used(t.Context(), coord.OrgScope, coord.WindowsAt(collected, la))
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	if day := u.In(period.Day); day.Window.Label != "2026-09-22" || day.Used != 70 {
		t.Fatalf("the collection day reads %+v, want 70 on the Los Angeles 22nd", day)
	}
}
