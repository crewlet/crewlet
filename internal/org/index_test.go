package org

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// THE INDEX ANSWERS WHAT THE CHART DOES, for every reference a chart can be
// asked: each unit's id, name and key and each seat's handle, in other cases
// and with space around them, references naming nothing, and the empty one —
// over a chart carrying every ambiguity an applied revision can (an id that is
// another unit's name, in both orders; one name on two units; one handle on
// two seats, which only an admission rule refuses). An Index that answered
// otherwise would make the code host record a different owner than the one
// its validation accepted.
//
// Mutation: let a later unit win a spelling, or look a name up before an id,
// and a row goes red.
func TestTheIndexAnswersWhatTheChartDoes(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "T",
		Roles: []*Role{{Name: "CEO", DeclaredHandle: "ceo"}, {Name: "Advisor"}},
		Units: []*Unit{
			{Name: "platform", Roles: []*Role{{Name: "A"}}},
			{Name: "Platform Services", ID: "platform", Roles: []*Role{{Name: "B"}},
				Children: []*Unit{
					{Name: "Infra", ID: "infra", Roles: []*Role{{Name: "C", DeclaredHandle: "ceo"}}},
				}},
			{Name: "Engineering", ID: "eng", Roles: []*Role{{Name: "Dev"}}},
			{Name: "Twice", Roles: []*Role{{Name: "D"}}},
			{Name: "twice", ID: "second-twice", Roles: []*Role{{Name: "E"}}},
			{Name: "Ops", ID: "Engineering", Roles: []*Role{{Name: "F"}}},
		},
	})
	x := o.Index()

	refs := []string{"", "   ", "nobody", "NOBODY", "infra/child"}
	for u := range o.AllUnits() {
		for _, s := range []string{u.ID, u.Name, u.Key()} {
			refs = append(refs, s, strings.ToUpper(s), strings.ToLower(s), "  "+s+"  ")
		}
	}
	for _, ref := range refs {
		if got, want := x.UnitByRef(ref), o.UnitByRef(ref); got != want {
			t.Errorf("UnitByRef(%q): the index answers %s, the chart %s", ref, unitName(got), unitName(want))
		}
	}

	handles := []string{"", "nobody", "CEO", " ceo"}
	for r := range o.AllRoles() {
		handles = append(handles, r.Handle(), strings.ToUpper(r.Handle()))
	}
	for _, h := range handles {
		if got, want := x.SeatByHandle(h), o.SeatByHandle(h); got != want {
			t.Errorf("SeatByHandle(%q): the index answers %s, the chart %s", h, roleName(got), roleName(want))
		}
	}

	var keys, all []string
	for u := range o.AllUnits() {
		keys = append(keys, u.Key())
	}
	for r := range o.AllRoles() {
		all = append(all, r.Handle())
	}
	if got := fmt.Sprint(x.UnitKeys()); got != fmt.Sprint(keys) {
		t.Errorf("UnitKeys() = %s, want every unit's key in walk order, %v", got, keys)
	}
	if got := fmt.Sprint(x.Handles()); got != fmt.Sprint(all) {
		t.Errorf("Handles() = %s, want every seat's handle in walk order, %v", got, all)
	}

	var none *Organization
	if u, r := none.Index().UnitByRef("eng"), none.Index().SeatByHandle("ceo"); u != nil || r != nil {
		t.Errorf("a nil chart's index answers %s and %s, want nothing", unitName(u), roleName(r))
	}

	// EVERY UNIT'S EFFECTIVE LEAD, on a chart with every shape the rule has
	// to tell apart: a lead among the unit's own seats, one in a grandchild,
	// one inherited from the division above (Normalize copies the name
	// down), one naming nobody, none at all, one in a sibling's subtree, and
	// a name three seats share — one in the unit's subtree after another in
	// the chart's walk, so the subtree's must win, and for a unit whose
	// subtree holds none of them, one before it and one after, so the
	// chart's first must, and not the next past the unit's own seats.
	//
	// Mutations: answer the chart's first seat of the name always (Dup goes
	// red), the subtree's alone (API, Left and Elsewhere), or read a subtree
	// as running to the chart's end (Elsewhere answers twin-after).
	led := normalized(&Organization{Name: "Led",
		Roles: []*Role{{Name: "Twin", DeclaredHandle: "twin-root"}},
		Units: []*Unit{
			{Name: "Eng", Lead: "Eng Lead", Roles: []*Role{{Name: "Eng Lead"}, {Name: "E1"}},
				Children: []*Unit{
					{Name: "API", Roles: []*Role{{Name: "A1"}}},
					{Name: "Web", Lead: "Web Lead", Roles: []*Role{{Name: "W1"}},
						Children: []*Unit{{Name: "Web Core", Roles: []*Role{{Name: "Web Lead"}}}}},
				}},
			{Name: "Ops", Lead: "Ghost", Roles: []*Role{{Name: "O1"}}},
			{Name: "Plain", Roles: []*Role{{Name: "P1"}}},
			{Name: "Left", Lead: "R1", Roles: []*Role{{Name: "L1"}}},
			{Name: "Right", Roles: []*Role{{Name: "R1"}}},
			{Name: "Dup", Lead: "Twin", Roles: []*Role{{Name: "D1"}},
				Children: []*Unit{{Name: "Dup Child", Roles: []*Role{{Name: "Twin", DeclaredHandle: "twin-dup"}}}}},
			{Name: "Elsewhere", Lead: "Twin", Roles: []*Role{{Name: "X1"}}},
			{Name: "After", Roles: []*Role{{Name: "Twin", DeclaredHandle: "twin-after"}}},
		},
	})
	lx := led.Index()
	answered := 0
	for u := range led.AllUnits() {
		got, want := lx.EffectiveLead(u), led.EffectiveLead(u)
		if got != want {
			t.Errorf("EffectiveLead(%s): the index answers %s, the chart %s", unitName(u), roleName(got),
				roleName(want))
		}
		if want != nil {
			answered++
		}
	}
	// AND THE CASES ARE THE ONES THEY CLAIM TO BE, or the comparison above
	// holds two nils equal and proves nothing.
	for unit, handle := range map[string]string{
		"API": "eng-lead", "Web": "web-lead", "Left": "r1", "Dup": "twin-dup", "Elsewhere": "twin-root",
	} {
		if got := lx.EffectiveLead(led.Unit(unit)); got == nil || got.Handle() != handle {
			t.Errorf("EffectiveLead(%q) = %s, want the seat %q", unit, roleName(got), handle)
		}
	}
	if answered != 8 {
		t.Errorf("%d units answered a lead, want 8 (Eng, API, Web, Web Core, Left, Dup, Dup Child, Elsewhere)", answered)
	}
	// A unit the Index was not built over is none of its units, and nil has
	// no lead.
	if got := lx.EffectiveLead(o.Unit("Engineering")); got != nil {
		t.Errorf("another chart's unit answered the lead %s, want none", roleName(got))
	}
	if got := none.Index().EffectiveLead(nil); got != nil {
		t.Errorf("a nil unit answered the lead %s, want none", roleName(got))
	}
}

func unitName(u *Unit) string {
	if u == nil {
		return "no unit"
	}
	return fmt.Sprintf("%q (id %q)", u.Name, u.ID)
}

func roleName(r *Role) string {
	if r == nil {
		return "no seat"
	}
	return fmt.Sprintf("%q (handle %q)", r.Name, r.Handle())
}

// BenchmarkResolvingADocumentsOwners is ten thousand owner lookups — the most
// repositories a code.repos list may declare — against a chart of 300 units
// and 900 seats, through the Index and through the chart's own one-off
// lookups, the cost the Index replaced in the code host's validation and
// reconcile.
func BenchmarkResolvingADocumentsOwners(b *testing.B) {
	o := &Organization{Name: "T"}
	for u := range 300 {
		unit := &Unit{Name: fmt.Sprintf("Unit %d", u), ID: fmt.Sprintf("unit-%d", u)}
		for s := range 3 {
			unit.Roles = append(unit.Roles, &Role{Name: fmt.Sprintf("Seat %d %d", u, s)})
		}
		o.Units = append(o.Units, unit)
	}
	o.Normalize()
	units, seats := make([]string, 10_000), make([]string, 10_000)
	for i := range units {
		units[i] = fmt.Sprintf("Unit %d", i%300)
		seats[i] = Slugify(fmt.Sprintf("Seat %d %d", i%300, i%3))
	}
	b.Run("index", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			x := o.Index()
			for i := range units {
				if x.UnitByRef(units[i]) == nil || x.SeatByHandle(seats[i]) == nil {
					b.Fatal("an owner did not resolve")
				}
			}
		}
	})
	b.Run("chart", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for i := range units {
				if o.UnitByRef(units[i]) == nil || o.SeatByHandle(seats[i]) == nil {
					b.Fatal("an owner did not resolve")
				}
			}
		}
	})
}

// ledChart is a chart of divisions, each led by a seat of its own, with ten
// teams of three seats under each that name no lead and so inherit the
// division's — the usual shape, and the one that makes the chart's own
// [Organization.EffectiveLead] dear: a team's inherited lead is outside its
// subtree, so it is found by a walk of every seat in the chart. The config
// warnings' and the engine roster's benchmarks build the same shape.
func ledChart(divisions int) *Organization {
	o := &Organization{Name: "T"}
	for d := range divisions {
		division := &Unit{Name: fmt.Sprintf("Division %d", d), Lead: fmt.Sprintf("Lead %d", d),
			Roles: []*Role{{Name: fmt.Sprintf("Lead %d", d)}}}
		for t := range 10 {
			team := &Unit{Name: fmt.Sprintf("Team %d %d", d, t)}
			for s := range 3 {
				team.Roles = append(team.Roles, &Role{Name: fmt.Sprintf("Seat %d %d %d", d, t, s)})
			}
			division.Children = append(division.Children, team)
		}
		o.Units = append(o.Units, division)
	}
	o.Normalize()
	return o
}

// BenchmarkAskingEveryUnitsLead is every unit's effective lead asked of a chart
// whose teams inherit their division's lead — 30 divisions (330 units, 930
// seats) and 100 (1 100 units, 3 100 seats) — through an [Index], whose build
// it includes, and through the chart's own [Organization.EffectiveLead]: the
// question the code host's grant roster asks of every unit once an epoch, and
// its `visibility: unit` warning of every repository's owning unit.
func BenchmarkAskingEveryUnitsLead(b *testing.B) {
	for _, divisions := range []int{30, 100} {
		o := ledChart(divisions)
		units := slices.Collect(o.AllUnits())
		name := fmt.Sprintf("%d_units", len(units))
		b.Run(name+"/index", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				x := o.Index()
				for _, u := range units {
					if x.EffectiveLead(u) == nil {
						b.Fatal("a unit has no lead")
					}
				}
			}
		})
		b.Run(name+"/chart", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, u := range units {
					if o.EffectiveLead(u) == nil {
						b.Fatal("a unit has no lead")
					}
				}
			}
		})
	}
}
