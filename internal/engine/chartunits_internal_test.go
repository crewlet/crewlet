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

// THE SEAM ANSWERS ANY SPELLING AND HANDS BACK THE KEY, which is the whole of
// what the two halves of this need: a write stores the key, and a screen
// renders the name.
//
// It resolved through an EXACT, case-sensitive match, so `unit: engineering`
// on a company with a unit named "Engineering" was refused with "This company
// has no team".
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

// THE UNIT-LEAD FALLBACK RESOLVES ON A UNIT THE CHART GAVE NO ID, whose key is
// its name.
//
// `id` falls back to the name ([org.Unit.Key]), so the string a task's routing
// unit holds is the NAME on such a unit. Matching the id alone compared it
// against "" and found nothing: the fallback that reaches a unit's lead when a
// change named nobody else reached NOBODY, and a wake that went to no one is
// indistinguishable from a unit whose lead is unset.
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
// project's own display name beside it.
//
// The project row is what every task filed into it takes its filed unit from,
// and a filed unit is never rewritten. So writing the NAME here filed the
// company's work under a spelling that moves the day somebody corrects the
// team's name, on the one path that writes most of it.
func TestTheChartFilesAProjectUnderTheUnitsKey(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		ID: "plat", Name: "Platform", Purpose: "the platform", Project: "PLAT",
		// A SEAT'S OWN PROJECT still says where in the company it sits,
		// and that home is the unit's key for the same reason.
		Roles: []*org.Role{{Name: "Ada", DeclaredHandle: "ada", Project: "ADA"}},
	}, {
		// A UNIT WITH NO ID is keyed by its name, which is the same
		// string this wrote before — the id is what makes the two differ.
		Name: "Product", Project: "PROD",
	}}}
	o.Normalize()

	filed := map[string]tracker.ChartProject{}
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
