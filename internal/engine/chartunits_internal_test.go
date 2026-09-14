package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A UNIT'S LEAD IS NAMED BY THEIR SEAT'S OWN HANDLE — the one [org.Role.Handle]
// derives — and never by a second derivation of its own.
//
// This resolver slugified the lead's DISPLAY NAME, which agrees with the
// accessor on exactly the seats whose operator declared no handle and
// disagrees on every other: a unit led by "Ada Okonkwo" with `handle: ada`
// resolved to `ada-okonkwo`, which is nobody in the chart. Nothing downstream
// could tell that from a lead who simply has no seat — the handle could not be
// looked up, filtered on, asked, or opened as a person, and every one of those
// failed by finding nothing rather than by erroring.
func TestAProjectsLeadIsNamedByTheSeatsOwnHandle(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", Lead: "Ada Okonkwo",
		Roles: []*org.Role{
			// THE DECLARED HANDLE AND THE SLUG OF THE NAME DIFFER, which
			// is the whole case: with them equal this test cannot fail.
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
	}}}
	o.Normalize()

	display, lead, found := ChartUnits(o).ResolveUnit("Platform")
	if !found {
		t.Fatal("Platform did not resolve")
	}
	if display != "Platform" {
		t.Errorf("display = %q, want Platform", display)
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
}

// AN INHERITED LEAD IS RESOLVED THE SAME WAY. The ancestor's seat is still a
// seat, and a sub-unit that declares no lead of its own is the ordinary case.
func TestAnInheritedLeadKeepsItsOwnHandleToo(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", Lead: "Ada Okonkwo",
		Roles:    []*org.Role{{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman}},
		Children: []*org.Unit{{Name: "Navigation"}},
	}}}
	o.Normalize()

	if _, lead, found := ChartUnits(o).ResolveUnit("Navigation"); !found ||
		lead.Handle != "ada" {
		t.Errorf("Navigation's lead = %+v (found %v), want ada inherited from "+
			"Platform", lead, found)
	}
}
