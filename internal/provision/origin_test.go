package provision_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provision"
)

// A SEAT WITH NO ORIGIN IS REFUSED RATHER THAN PROVISIONED.
//
// An empty origin does not fail at any third-party app: every derivation
// renders it as the bare prefix, so `crewlet-` is a perfectly good account
// name — and TWO seats missing it resolve to ONE account, holding one
// credential, acting as both agents. Nothing downstream could tell that from
// two seats legitimately sharing, which is why the refusal is here, at the
// one funnel every scan's seats pass through.
func TestASeatWithNoOriginIsRefusedRatherThanShared(t *testing.T) {
	t.Parallel()
	plan := &provision.Plan{}
	plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "T_SWE"})
	plan.Add(provision.Seat{Handle: "sre", Role: "SRE", TokenVar: "T_SRE"})

	if len(plan.Seats) != 0 {
		t.Fatalf("planned %+v, want nothing provisioned under a name two "+
			"seats would share", plan.Seats)
	}
	// SAID OUT LOUD, both times. A silently short plan reads as a company
	// with fewer agents than it has, and the note is where an operator
	// learns which seats the run left alone.
	if len(plan.Notes) != 2 {
		t.Fatalf("notes = %q, want one per refused seat", plan.Notes)
	}
	for _, want := range []string{"swe", "sre"} {
		if !strings.Contains(strings.Join(plan.Notes, "\n"), want) {
			t.Errorf("notes = %q, want the seat %q named", plan.Notes, want)
		}
	}
}

// AND A SEAT THAT HAS ONE IS PLANNED, ORDERED BY THE HANDLE A REPORT PRINTS.
//
// The control arm: without it the case above passes on a plan that refuses
// everything, which is the one mutation that would make the guard useless
// and silent at the same time.
func TestASeatWithAnOriginIsPlannedInHandleOrder(t *testing.T) {
	t.Parallel()
	plan := &provision.Plan{}
	plan.Add(provision.Seat{Handle: "swe", Origin: "swe", TokenVar: "T_SWE"})
	plan.Add(provision.Seat{Handle: "cto", Origin: "founder", TokenVar: "T_CTO"})

	if len(plan.Seats) != 2 {
		t.Fatalf("planned %+v, want both seats", plan.Seats)
	}
	if len(plan.Notes) != 0 {
		t.Errorf("notes = %q, want none", plan.Notes)
	}
	// ORDERED BY THE HANDLE, not the origin: the order is for a reader
	// diffing two runs, and what a report prints is the handle.
	if plan.Seats[0].Handle != "cto" || plan.Seats[1].Handle != "swe" {
		t.Errorf("order = %q, %q, want cto before swe",
			plan.Seats[0].Handle, plan.Seats[1].Handle)
	}
	// AND THE ORIGIN TRAVELS, which is the whole point of carrying it: a
	// renamed seat's account is named after a handle the plan no longer
	// prints anywhere else.
	if plan.Seats[0].Origin != "founder" {
		t.Errorf("origin = %q, want the handle the seat was created under",
			plan.Seats[0].Origin)
	}
}
