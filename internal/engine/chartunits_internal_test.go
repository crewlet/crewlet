package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A UNIT'S LEAD IS NAMED BY THE SEAT'S OWN HANDLE, in the document and in
// this resolver alike, never by a second derivation: a slug of "Ada Okonkwo"
// (`handle: ada`) is `ada-okonkwo`, nobody in the chart, and every lookup of
// it fails by finding nothing rather than by erroring.
//
// THE DECLARED HANDLE AND THE SLUG OF THE NAME DIFFER in every case here: with
// them equal this test cannot fail, and a slugify path would pass it.
func TestALeadWithAnExplicitHandleResolves(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", ID: "platform", Lead: "ada",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
	}}}
	o.Normalize()

	unit, found := ChartUnits(o).ResolveUnit("platform")
	if !found {
		t.Fatal("the unit did not resolve by its key")
	}
	if unit.Name != "Platform" {
		t.Errorf("display = %q, want Platform — the key resolves, the NAME is "+
			"what a person reads", unit.Name)
	}
	lead := unit.Lead
	if lead.Handle != "ada" {
		t.Errorf("lead.Handle = %q, want ada — the seat's own handle, not a "+
			"slug of its display name", lead.Handle)
	}
	// A HUMAN LEAD IS SAID TO BE ONE: the kind decides whether a surface
	// offers to ask them through the engine or to page a person.
	if lead.Kind != worklet.AuthorHuman {
		t.Errorf("lead.Kind = %q, want %q", lead.Kind, worklet.AuthorHuman)
	}
	// THE CONTROL, stated rather than implied: the seat is not reachable by
	// the slug of its name, so a resolver taking that path answers nobody.
	if o.Role(org.Slugify("Ada Okonkwo")) != nil {
		t.Fatal("the slug of the display name resolves to a seat, so this " +
			"case cannot tell the two derivations apart")
	}
}

// A UNIT THAT DECLARES NO ID STILL RESOLVES ITS LEAD, because its KEY falls
// back to its name and the key is what a stored row holds. Comparing against
// the `id` FIELD, empty on such a unit, routes its unassigned work to no one
// while it looks correctly filed.
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

	if resolved, found := ChartUnits(o).ResolveUnit(unit.Key()); !found ||
		resolved.Lead.Handle != "ada" {
		t.Errorf("ResolveUnit(%q) lead = %+v (found %v), want ada",
			unit.Key(), resolved.Lead, found)
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

	if unit, found := ChartUnits(o).ResolveUnit("navigation"); !found ||
		unit.Lead.Handle != "ada" {
		t.Errorf("Navigation's lead = %+v (found %v), want ada inherited from "+
			"Platform", unit.Lead, found)
	}
}

// THE SEAM ANSWERS ANY SPELLING AND HANDS BACK THE KEY: a write stores the
// key and a screen renders the name, so `unit: engineering` on a unit named
// "Engineering" resolves rather than being refused.
func TestTheUnitSeamAnswersEitherSpellingWithTheKey(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		ID: "plat", Name: "Platform", Lead: "ada",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
	}, {
		// A UNIT WITH NO ID, whose key IS its name — a tree built in Go,
		// or a revision stored before the chart minted ids.
		Name: "Navigation", Lead: "ada",
	}}}
	o.Normalize()

	for _, tc := range []struct {
		ref  string
		key  string
		name string
	}{
		{"plat", "plat", "Platform"},
		{"Platform", "plat", "Platform"},
		{"PLATFORM", "plat", "Platform"},
		{"  plat  ", "plat", "Platform"},
		{"navigation", "Navigation", "Navigation"},
	} {
		unit, found := ChartUnits(o).ResolveUnit(tc.ref)
		switch {
		case !found:
			t.Errorf("%q did not resolve", tc.ref)
		case unit.Key != tc.key:
			t.Errorf("%q resolved to key %q, want %q — the key is what a "+
				"write stores", tc.ref, unit.Key, tc.key)
		case unit.Name != tc.name:
			t.Errorf("%q resolved to name %q, want %q", tc.ref, unit.Name, tc.name)
		}
	}
	if _, found := ChartUnits(o).ResolveUnit("Design"); found {
		t.Error("a unit nobody has resolved — a write refuses it by name")
	}
}

// THE UNIT-LEAD FALLBACK RESOLVES ON A UNIT THE CHART GAVE NO ID, whose key
// ([org.Unit.Key]), and so a task's routing unit, is its NAME. Matching the id
// alone compares against "" and reaches nobody, indistinguishable from a unit
// whose lead is unset.
func TestAUnitsLeadResolvesByItsNameWhenItHasNoID(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Platform", Lead: "ada",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
	}}}
	o.Normalize()

	if got := UnitLeadOf(o, "Platform"); got != "ada" {
		t.Errorf("the lead of Platform is %q, want ada — a unit with no id is "+
			"keyed by its name, and matching the id alone compares every "+
			"routing unit against the empty string", got)
	}
	if got := UnitLeadOf(o, "Navigation"); got != "" {
		t.Errorf("a unit nothing names resolved to %q", got)
	}
}

// AND BY EVERY SPELLING A ROW MAY HOLD: its key, the key folded, and its name.
// A task's routing unit is a record of what was true and nothing rewrites it,
// so every one of them has to reach the lead.
func TestAUnitsLeadResolvesByEverySpellingARowMayHold(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		ID: "platform", Name: "Platform", Lead: "ada",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		},
		// AN INHERITING CHILD, so the effective lead is exercised on the
		// same walk rather than only the declared one.
		Children: []*org.Unit{{ID: "nav", Name: "Navigation"}},
	}}}
	o.Normalize()

	for _, unit := range []string{"platform", "Platform", "PLATFORM", "nav", "Navigation"} {
		if got := UnitLeadOf(o, unit); got != "ada" {
			t.Errorf("the lead of %q is %q, want ada", unit, got)
		}
	}
}

// A PROJECT'S CHART-OWNED UNIT IS THE UNIT'S KEY, and its name is the
// project's own display name beside it: every task filed into the project
// takes its filed unit from this row and a filed unit is never rewritten, so
// the NAME would file the company's work under a spelling that moves the day
// somebody renames the team.
func TestTheChartFilesAProjectUnderTheUnitsKey(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		ID: "plat", Name: "Platform", Purpose: "the platform", Project: "PLAT",
		// A SEAT'S OWN PROJECT still says where in the company it sits,
		// and that home is the unit's key for the same reason.
		Roles: []*org.Role{{Name: "Ada", DeclaredHandle: "ada", Project: "ADA"}},
	}, {
		// A UNIT WITH NO ID is keyed by its name; the id is what makes the
		// two differ.
		Name: "Product", Project: "PROD",
	}}}
	o.Normalize()

	filed := map[string]worklet.ChartProject{}
	for _, project := range chartProjects(o) {
		filed[project.Key] = project
	}
	for _, tc := range []struct {
		project string
		unit    string
		name    string
	}{
		{"PLAT", "plat", "Platform"},
		{"ADA", "plat", "Ada"},
		{"PROD", "Product", "Product"},
	} {
		got, held := filed[tc.project]
		switch {
		case !held:
			t.Errorf("the chart names no project %s", tc.project)
		case got.Unit != tc.unit:
			t.Errorf("%s files into unit %q, want %q — what a task's filed "+
				"unit is taken from has to be the key", tc.project, got.Unit, tc.unit)
		case got.Name != tc.name:
			t.Errorf("%s is named %q, want %q", tc.project, got.Name, tc.name)
		}
	}
}
