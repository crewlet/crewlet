package org

import (
	"fmt"
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
	return fmt.Sprintf("%q", r.Name)
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
