package jira_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/org"
)

// THE JIRA MAP READS THE JIRA FIELD.
//
// A unit declares a tracker project and a wiki space, and they are different
// places. A lead map that read the wrong field would route every Jira event
// by the Confluence space's ownership — and it would look right, because the
// two are usually the same team.
func TestTheLeadMapReadsTheTrackersOwnField(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "CTO", DeclaredHandle: "cto"},
			{Name: "Ops Lead", DeclaredHandle: "ops"},
		},
		Units: []*org.Unit{
			{Name: "Engineering", Lead: "CTO", Project: "eng", Space: "PLATFORM"},
			{Name: "Operations", Lead: "Ops Lead", Space: "OPS"},
		},
	}
	o.Normalize()

	leads := jira.LeadsFrom(o)
	if leads["ENG"] != "cto" {
		t.Fatalf("ENG → %q, want cto (leads = %v)", leads["ENG"], leads)
	}
	// The wiki-only unit contributes nothing here: it names no Jira
	// project, so a Jira event can never be routed by it.
	if len(leads) != 1 {
		t.Fatalf("a unit with no Jira project reached the Jira lead map: %v", leads)
	}
}

// AN INHERITED LEAD OWNS THE PROJECT. A lead set on a division owns a team
// three levels down that named nobody itself.
func TestAnInheritedLeadOwnsTheProject(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "nimbus",
		Roles: []*org.Role{{Name: "CTO", DeclaredHandle: "cto"}},
		Units: []*org.Unit{{
			Name: "Engineering", Lead: "CTO",
			Children: []*org.Unit{{Name: "Platform", Project: "PLAT"}},
		}},
	}
	o.Normalize()
	if got := jira.LeadsFrom(o)["PLAT"]; got != "cto" {
		t.Fatalf("PLAT → %q, want cto", got)
	}
}

// A ROOT SEAT OWNS ITS OWN PROJECT — it belongs to no unit, so there is no
// lead to resolve and the seat IS the owner.
func TestARootSeatOwnsItsProject(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "nimbus",
		Roles: []*org.Role{{Name: "CEO", DeclaredHandle: "ceo", Project: "BIZ"}},
	}
	o.Normalize()
	if got := jira.LeadsFrom(o)["BIZ"]; got != "ceo" {
		t.Fatalf("BIZ → %q, want ceo", got)
	}
}

// A UNIT WITH NO LEAD OWNS NOTHING, rather than routing to a guess. The
// warning is the operator's signal; a wrong owner would be silent.
func TestAUnitWithNoLeadOwnsNoProject(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "nimbus",
		Units: []*org.Unit{{Name: "Engineering", Project: "ENG"}},
	}
	o.Normalize()
	if leads := jira.LeadsFrom(o); len(leads) != 0 {
		t.Fatalf("leads = %v", leads)
	}
}

// EVERY DECLARED PROJECT IS CHECKED BY THE RECONCILE, whether a unit or a
// seat named it — a key with a typo routes nothing and reports nothing.
func TestEveryDeclaredProjectIsEnumerated(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "nimbus",
		Roles: []*org.Role{{Name: "CEO", DeclaredHandle: "ceo", Project: "biz"}},
		Units: []*org.Unit{{
			Name: "Engineering", Project: "ENG",
			Roles: []*org.Role{{Name: "SWE", DeclaredHandle: "swe", Project: "ENG"}},
		}},
	}
	o.Normalize()
	got := jira.ProjectsOf(o)
	if len(got) != 2 || got[0] != "BIZ" || got[1] != "ENG" {
		t.Fatalf("projects = %v", got)
	}
}
