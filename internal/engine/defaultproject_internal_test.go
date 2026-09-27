package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// A PERSON'S CREATE LANDS WHERE THEIR SEAT'S WORK DOES, on the chart CURRENT
// when it is filed. The operator surface is built once and never rebuilt by
// an apply, and it used to be handed a stub answering "" for everybody — so a
// bound founder's create that named no project was refused while `viewer`
// promised it a project. Read per call: before any epoch there is no chart
// and no project, and an apply that moves the seat moves the answer.
func TestAPersonsDefaultProjectIsReadFromTheCurrentChart(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	project := LiveDefaultProject(e)
	if got := project("jane"); got != "" {
		t.Errorf("before any epoch: %q, want \"\"", got)
	}

	jane := &org.Role{Name: "Jane Founder", DeclaredHandle: "jane"}
	e.epoch.current.Store(&Company{Org: &org.Organization{
		Name:  "Acme",
		Units: []*org.Unit{{Name: "Engineering", Project: "ENG", Roles: []*org.Role{jane}}},
	}})
	if got := project("jane"); got != "ENG" {
		t.Errorf("in a team owning ENG: %q, want ENG", got)
	}

	e.epoch.current.Store(&Company{Org: &org.Organization{
		Name:  "Acme",
		Units: []*org.Unit{{Name: "Product", Project: "PROD", Roles: []*org.Role{jane}}},
	}})
	if got := project("jane"); got != "PROD" {
		t.Errorf("after an apply moved her: %q, want PROD, the chart now current", got)
	}
}
