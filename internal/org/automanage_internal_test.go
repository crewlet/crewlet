package org

import (
	"fmt"
	"runtime"
	"testing"
)

// A LEAD'S MANAGED SET IS COMPUTED ONCE PER LEAD, NOT ONCE PER UNIT.
//
// # What this is guarding, and why it needs a gate of its own
//
// Lead inheritance makes a root seat the EFFECTIVE lead of every unit beneath
// it, and auto-management appends to that seat's `manages` for every direct
// member it claims — so by the end of the walk a root lead's list holds
// something close to every seat in the company. Rebuilding its resolved set at
// the top of each unit's iteration is therefore O(units × seats), and it
// measured at 95% of the whole derivation's allocation: 970 MB and 1.2 seconds
// for a fan-shaped company of twenty thousand seats, against 38 MB and 44 ms
// with the set kept.
//
// The equivalence gate cannot catch a regression here, and that is exactly why
// this exists: the slow path and the fast path produce the IDENTICAL tree, so
// every correctness test in this repository passes either way. A benchmark is
// not a gate — it has no threshold and nothing fails when it moves — so the
// property is pinned by counting instead.
//
// # Why allocated BYTES rather than time or allocation count
//
// Time on a shared runner is noise. Allocation COUNT is deterministic but
// blind to this particular fault: the recomputation builds one map per unit
// whose SIZE grows with the lead's list, and a map of a thousand entries is a
// handful of allocations exactly as a map of ten is. Counting them saw a
// twenty-fold regression as no change at all — which this case established by
// being written that way first and passing with the fix reverted.
//
// Bytes see it, because that is what actually grows: on the fixture below the
// two paths are 1.4 MB and 30.2 MB, a factor of twenty-two.
//
// NOT PARALLEL, and it is the one case in this package that is not: the
// measurement is of the whole process's heap, so another test's garbage would
// land in it.
func TestALeadsManagedSetIsBuiltOncePerLeadRatherThanPerUnit(t *testing.T) {
	// A COMPANY WHOSE ROOT LEADS EVERYTHING, which is the shape the
	// quadratic needs: one seat that inherits the lead of every unit, and
	// enough units for the recomputation to dominate.
	//
	// FIVE HUNDRED UNITS because the separation has to be real. At two
	// hundred the two paths were 4.1 MB and 4.9 MB — a bound between them
	// would be a flake waiting for a Go release to change a map's growth
	// factor. At five hundred they are 1.4 MB and 30.2 MB.
	const units = 500
	const perUnit = 5

	build := func() *Organization {
		o := &Organization{Name: "Acme", Roles: []*Role{
			{Name: "Chief", DeclaredHandle: "ceo"},
		}}
		parent := &Unit{Name: "Org", ID: "org", Lead: "ceo"}
		o.Units = []*Unit{parent}
		for i := range units {
			child := &Unit{
				Name: fmt.Sprintf("Unit %03d", i),
				ID:   fmt.Sprintf("unit-%03d", i),
			}
			for j := range perUnit {
				child.Roles = append(child.Roles, &Role{
					Name:           fmt.Sprintf("Seat %03d-%d", i, j),
					DeclaredHandle: fmt.Sprintf("seat-%03d-%d", i, j),
				})
			}
			parent.Children = append(parent.Children, child)
		}
		return o
	}

	// THE MEASUREMENT IS OF NORMALIZE ITSELF, over a freshly built tree
	// each time: Normalize is idempotent, but a second pass over an
	// already-normalized tree has a different profile and would measure the
	// wrong thing. The tree is built INSIDE the measured window and its
	// own cost is subtracted below, because building it allocates too.
	measure := func(body func()) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		body()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	baseline := measure(func() { build() })
	whole := measure(func() { build().Normalize() })
	derivation := whole - min(whole, baseline)

	// THE BOUND, AND BOTH NUMBERS IT SITS BETWEEN, measured on this exact
	// fixture — 500 units and 2,500 seats under one inherited lead:
	//
	//	keeping the lead's set:     1.4 MB
	//	rebuilding it per unit:    30.2 MB
	//
	// Eight mebibytes is roughly six times the first and a quarter of the
	// second, so it catches the regression with room on both sides. A
	// bound tight enough to notice an extra map would be one somebody
	// raises rather than reads.
	const bound = 8 << 20
	if derivation > bound {
		t.Errorf("normalizing a %d-unit company under one root lead "+
			"allocated %d bytes, and the bound is %d.\n"+
			"That bound is not a tuning target: it sits an order of "+
			"magnitude below what rebuilding the lead's resolved manages "+
			"set per unit costs. If this fired, check that autoManageByLead "+
			"still keeps a lead's set across the unit walk rather than "+
			"calling managesIndex.managed for it each time.",
			units, derivation, bound)
	}

	// AND THE RESULT IS STILL RIGHT, because a fast derivation that
	// produced a different chart would pass the bound above and be worse
	// than the slow one. Every seat in every unit is claimed by the lead
	// that inherits down to it.
	o := build()
	o.Normalize()
	lead := o.Role("ceo")
	if lead == nil {
		t.Fatal("the root lead is not in the company")
	}
	if got, want := len(lead.Manages), units*perUnit; got != want {
		t.Errorf("the root lead manages %d seats, want %d — auto-management "+
			"is what makes a lead's roster complete without an operator "+
			"listing every report twice", got, want)
	}
}
