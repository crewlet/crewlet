package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

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
	if got, err := fleet.Charge(ctx, scope, 90, 1000, 100); err != nil || !got.OK {
		t.Fatalf("setup charge = (%+v, %v)", got, err)
	}
	account := sandboxAccountant{budgets: fleet, caps: func(string) (int, int) { return 1000, 100 }}

	over, err := account.Charge(ctx, "a-1", "lead", 50)
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if !over {
		t.Error("a run that took the seat past its cap was not reported over")
	}
	for _, s := range []string{coord.OrgScope, scope} {
		if used, err := fleet.Used(ctx, s); err != nil || used != 140 {
			t.Errorf("%s used = (%d, %v), want the 140 the company actually spent", s, used, err)
		}
	}
	rows, err := fleet.Usage(ctx)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	for _, row := range rows {
		if !row.RefusedAt.IsZero() {
			t.Errorf("%s reads as refusing after a run was recorded: nothing was refused", row.Scope)
		}
	}
	// And the gate now judges the next round against what was really spent.
	if got, err := fleet.Charge(ctx, scope, 1, 1000, 100); err != nil || got.OK {
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
		orgCap, seat  int
		wantOver      bool
		tokens, prior int
	}{
		{"inside both caps", 1000, 100, false, 10, 50},
		{"exactly at the seat cap", 1000, 100, false, 50, 50},
		{"past the company cap only", 120, 0, true, 80, 50},
		{"uncapped", 0, 0, false, 5000, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fleet := coordmem.NewFleet()
			scope := coord.AgentScope("a-1")
			if _, err := fleet.Charge(ctx, scope, tc.prior, 0, 0); err != nil {
				t.Fatalf("setup: %v", err)
			}
			account := sandboxAccountant{budgets: fleet,
				caps: func(string) (int, int) { return tc.orgCap, tc.seat }}
			over, err := account.Charge(ctx, "a-1", "lead", tc.tokens)
			if err != nil {
				t.Fatalf("Charge: %v", err)
			}
			if over != tc.wantOver {
				t.Errorf("over = %v, want %v", over, tc.wantOver)
			}
			if used, _ := fleet.Used(ctx, scope); used != tc.prior+tc.tokens {
				t.Errorf("seat used = %d, want %d", used, tc.prior+tc.tokens)
			}
		})
	}
}
