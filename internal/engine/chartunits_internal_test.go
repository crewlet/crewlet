package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A UNIT'S LEAD IS NAMED BY THE SEAT'S OWN HANDLE, in the document and in
// this resolver alike — never by a second derivation of its own.
//
// `lead:` holds the handle, so there is one identity and one lookup. It used
// to hold a DISPLAY NAME, which agrees with the handle on exactly the seats
// whose operator declared none and disagrees on every other: a unit led by
// "Ada Okonkwo" with `handle: ada` was resolved by slugifying the name to
// `ada-okonkwo`, which is nobody in the chart. Nothing downstream could tell
// that from a lead who simply has no seat — the handle could not be looked
// up, filtered on, asked, or opened as a person, and every one of those
// failed by finding nothing rather than by erroring.
//
// THE DECLARED HANDLE AND THE SLUG OF THE NAME DIFFER in every case here,
// which is the whole point: with them equal this test cannot fail, and the
// old slugify path would pass it.
func TestALeadWithAnExplicitHandleResolves(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", ID: "platform", Lead: "ada",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
	}}}
	o.Normalize()

	display, lead, found := ChartUnits(o).ResolveUnit("platform")
	if !found {
		t.Fatal("the unit did not resolve by its key")
	}
	if display != "Platform" {
		t.Errorf("display = %q, want Platform — the key resolves, the NAME is "+
			"what a person reads", display)
	}
	if lead.Handle != "ada" {
		t.Errorf("lead.Handle = %q, want ada — the seat's own handle, not a "+
			"slug of its display name", lead.Handle)
	}
	// A HUMAN LEAD IS SAID TO BE ONE: the kind decides whether a surface
	// offers to ask them through the engine or to page a person.
	if lead.Kind != tracker.AuthorHuman {
		t.Errorf("lead.Kind = %q, want %q", lead.Kind, tracker.AuthorHuman)
	}
	// THE CONTROL, stated rather than implied: the seat is not reachable by
	// the slug of its name, so a resolver taking that path answers nobody.
	if o.Role(org.Slugify("Ada Okonkwo")) != nil {
		t.Fatal("the slug of the display name resolves to a seat, so this " +
			"case cannot tell the two derivations apart")
	}
}

// A UNIT THAT DECLARES NO ID STILL RESOLVES ITS LEAD, because its KEY falls
// back to its name and the key is what a stored row holds.
//
// This is the other half of the same confusion, and it read as working: the
// live resolver compared the stored value against each unit's `id` FIELD,
// which is empty on every unit that has not been given one — so a company
// that had not adopted ids had a unit lead that answered nobody, and
// unassigned work routed to no one while looking correctly filed.
func TestAUnitWithNoExplicitIdResolvesItsLead(t *testing.T) {
	t.Parallel()
	unit := &org.Unit{
		Name: "Platform", Lead: "ada",
		Roles: []*org.Role{{Name: "Ada Okonkwo", DeclaredHandle: "ada"}},
	}
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{unit}}
	o.Normalize()

	// THE CONTROL: the id field is empty, so anything keyed on it matches
	// nothing, while the key is the name.
	if unit.ID != "" {
		t.Fatal("the unit declares an id, so this case proves nothing")
	}
	if unit.Key() != "Platform" {
		t.Fatalf("Key() = %q, want the name to stand in for an absent id", unit.Key())
	}

	if _, lead, found := ChartUnits(o).ResolveUnit(unit.Key()); !found ||
		lead.Handle != "ada" {
		t.Errorf("ResolveUnit(%q) lead = %+v (found %v), want ada",
			unit.Key(), lead, found)
	}
	if got := o.EffectiveLead(o.Unit(unit.Key())); got == nil || got.Handle() != "ada" {
		t.Errorf("the unit lead resolved to %v, want ada — a wake routed to "+
			"this unit would reach nobody", got)
	}
}

// AN INHERITED LEAD IS RESOLVED THE SAME WAY. The ancestor's seat is still a
// seat, and a sub-unit that declares no lead of its own is the ordinary case.
func TestAnInheritedLeadKeepsItsOwnHandleToo(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", ID: "platform", Lead: "ada",
		Roles:    []*org.Role{{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman}},
		Children: []*org.Unit{{Name: "Navigation", ID: "navigation"}},
	}}}
	o.Normalize()

	if _, lead, found := ChartUnits(o).ResolveUnit("navigation"); !found ||
		lead.Handle != "ada" {
		t.Errorf("Navigation's lead = %+v (found %v), want ada inherited from "+
			"Platform", lead, found)
	}
}
