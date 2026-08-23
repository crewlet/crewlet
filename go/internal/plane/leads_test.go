package plane

import (
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

func company(units []*org.OrgUnit, roots ...*org.Role) *org.Organization {
	o := &org.Organization{Name: "Nimbus", Units: units, Roles: roots}
	o.Normalize()
	return o
}

func TestAUnitsProjectIsOwnedByItsLead(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "eng",
		Roles: []*org.Role{{Name: "VP Eng"}, {Name: "Engineer"}},
	}})
	want := map[string]string{"ENG": "vp-eng"}
	if got := LeadsFrom(o); !maps.Equal(got, want) {
		t.Fatalf("leads = %v, want %v", got, want)
	}
}

// AN INHERITED LEAD IS A FULL LEAD. A lead set on a division owns a team
// three levels down that named nobody itself — and that team is exactly the
// one whose project would otherwise route to nobody.
func TestAnInheritedLeadOwnsTheProject(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng",
		Roles: []*org.Role{{Name: "VP Eng"}},
		Children: []*org.OrgUnit{{
			Name: "Platform", Children: []*org.OrgUnit{{
				Name: "Runtime", PlaneProject: "RUN",
				Roles: []*org.Role{{Name: "Runtime Engineer"}},
			}},
		}},
	}})
	if got := LeadsFrom(o)["RUN"]; got != "vp-eng" {
		t.Fatalf("RUN is owned by %q, want the inherited lead", got)
	}
}

// A ROOT SEAT IS ITS OWN OWNER: it belongs to no unit, so there is no lead
// to resolve.
func TestARootSeatOwnsItsOwnProject(t *testing.T) {
	t.Parallel()
	o := company(nil, &org.Role{Name: "Chief of Staff", PlaneProject: "OPS"})
	if got := LeadsFrom(o)["OPS"]; got != "chief-of-staff" {
		t.Fatalf("OPS is owned by %q", got)
	}
}

// A SEAT INSIDE A UNIT DECLARES WHERE IT WRITES, NOT WHO OWNS THE PROJECT.
// The unit's lead already answers ownership; letting a member override it
// would hand the fallback to whichever teammate the walk reached first.
func TestAUnitMemberDoesNotBecomeTheOwner(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "ENG",
		Roles: []*org.Role{
			{Name: "VP Eng"},
			{Name: "Engineer", PlaneProject: "ENG"},
		},
	}})
	if got := LeadsFrom(o)["ENG"]; got != "vp-eng" {
		t.Fatalf("ENG is owned by %q, want the unit lead", got)
	}
}

// A root seat carrying unit: is MOVED into that unit by Normalize, so it is
// no longer a root seat and no longer an owner — the same rule as above,
// reached the other way round.
func TestASeatAttachedToAUnitStopsBeingARootOwner(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng",
		Roles: []*org.Role{{Name: "VP Eng"}},
	}}, &org.Role{Name: "Engineer", UnitRef: "Engineering", PlaneProject: "ENG"})
	if got := LeadsFrom(o)["ENG"]; got != "" {
		t.Fatalf("ENG is owned by %q, want nobody — the seat joined a unit", got)
	}
}

// IDENTIFIERS ARE CASE-FOLDED. Plane accepts one in any case and an operator
// writes it however it was shown to them, so a config saying "eng" and a
// webhook saying "ENG" must be the same project.
func TestIdentifiersFoldToOneCase(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "  eNg  ",
		Roles: []*org.Role{{Name: "VP Eng"}},
	}})
	leads := LeadsFrom(o)
	if got := leads["ENG"]; got != "vp-eng" {
		t.Fatalf("the folded key resolves to %q; map = %v", got, leads)
	}
}

// SHARED PROJECTS ARE A DOCUMENTED PATTERN: several units under one lead all
// naming one project must resolve, silently, to that lead.
func TestSeveralUnitsUnderOneLeadShareTheProject(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "ENG",
		Roles: []*org.Role{{Name: "VP Eng"}},
		Children: []*org.OrgUnit{
			{Name: "Backend", PlaneProject: "ENG"},
			{Name: "Frontend", PlaneProject: "ENG"},
		},
	}})
	want := map[string]string{"ENG": "vp-eng"}
	if got := LeadsFrom(o); !maps.Equal(got, want) {
		t.Fatalf("leads = %v, want %v", got, want)
	}
}

// WHICH declaration wins is a decision, not an accident. A unit is walked
// before a root seat, so a project a unit owns is not stolen by a seat that
// names it — and pinning it is what makes "first wins" a rule rather than a
// description of today's loop order.
func TestAUnitOutranksARootSeatForTheSameProject(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "ENG",
		Roles: []*org.Role{{Name: "VP Eng"}},
	}}, &org.Role{Name: "Chief of Staff", PlaneProject: "ENG"})
	if got := LeadsFrom(o)["ENG"]; got != "vp-eng" {
		t.Fatalf("ENG is owned by %q, want the unit lead", got)
	}
}

// Parents are walked before children, so a division that names a project
// owns it even when a team beneath it names the same one under its own lead.
func TestAParentUnitOutranksItsChildForTheSameProject(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Engineering", Lead: "VP Eng", PlaneProject: "ENG",
		Roles: []*org.Role{{Name: "VP Eng"}},
		Children: []*org.OrgUnit{{
			Name: "Backend", Lead: "Tech Lead", PlaneProject: "ENG",
			Roles: []*org.Role{{Name: "Tech Lead"}},
		}},
	}})
	if got := LeadsFrom(o)["ENG"]; got != "vp-eng" {
		t.Fatalf("ENG is owned by %q, want the parent's lead", got)
	}
}

// A GENUINE CONFLICT still resolves — a project with no owner is worse than
// one owned by the first of two candidates — and it resolves the SAME way
// every time, because a routing target that moved between applies would be
// a fault nobody could reproduce.
func TestAnAmbiguousProjectResolvesTheSameWayEveryTime(t *testing.T) {
	t.Parallel()
	build := func() *org.Organization {
		return company([]*org.OrgUnit{
			{Name: "Engineering", Lead: "VP Eng", PlaneProject: "SHARED",
				Roles: []*org.Role{{Name: "VP Eng"}}},
			{Name: "Product", Lead: "VP Product", PlaneProject: "SHARED",
				Roles: []*org.Role{{Name: "VP Product"}}},
		})
	}
	first := LeadsFrom(build())["SHARED"]
	if first == "" {
		t.Fatal("an ambiguous project resolved to nobody")
	}
	for range 8 {
		if got := LeadsFrom(build())["SHARED"]; got != first {
			t.Fatalf("the owner moved between applies: %q then %q", first, got)
		}
	}
}

// A unit that owns a project and has nobody leading it contributes NOTHING
// rather than a guess: every unassigned item there routes to nobody, which
// is a config gap an operator has to close — a misroute would teach a seat
// that work it does not own is its problem.
func TestAUnitWithNoLeadOwnsNothing(t *testing.T) {
	t.Parallel()
	o := company([]*org.OrgUnit{{
		Name: "Skunkworks", PlaneProject: "SKUNK",
		Roles: []*org.Role{{Name: "Researcher"}},
	}})
	if leads := LeadsFrom(o); len(leads) != 0 {
		t.Fatalf("leads = %v, want none", leads)
	}
}

func TestNoOrgYieldsNoLeads(t *testing.T) {
	t.Parallel()
	if leads := LeadsFrom(nil); leads != nil {
		t.Fatalf("leads = %v, want nil", leads)
	}
	if leads := LeadsFrom(company(nil)); len(leads) != 0 {
		t.Fatalf("leads = %v, want none", leads)
	}
}
