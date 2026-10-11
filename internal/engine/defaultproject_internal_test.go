package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A PERSON'S CREATE LANDS WHERE THEIR SEAT'S WORK DOES, on the chart CURRENT
// when it is filed. The operator surface is not rebuilt for a new chart, and
// it used to be handed a stub answering "" for everybody — so a bound
// founder's create that named no project was refused while `viewer` promised
// it a project. Read per call: before any epoch there is no chart and no
// project, and an apply that moves the seat moves the answer.
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

// THE CONTAINERS NO OPERATOR WRITE MAY TARGET ARE THE CURRENT COMPANY'S. Both
// are Tier B and the operator's surface is not rebuilt for a new company
// document, so they are read at each write: before any epoch nothing is
// reserved, and an apply that moves the skills container moves what is refused
// with it. The API used to evaluate its own copy of this rule once, when it
// built its catalogue.
func TestTheReservedContainersAreTheCurrentCompanys(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	reserved := LiveReserved(e)
	if got := reserved(); len(got) != 0 {
		t.Errorf("before any epoch: %v, want nothing reserved", got)
	}

	skills := "SKILLS"
	e.epoch.current.Store(&Company{Config: &config.Company{Name: "Acme"}})
	if got := reserved(); !slices.Equal(got, []string{config.DefaultSkillsContainer,
		config.DefaultRootSpace}) {
		t.Errorf("on the defaults: %v, want %s and %s", got,
			config.DefaultSkillsContainer, config.DefaultRootSpace)
	}
	e.epoch.current.Store(&Company{Config: &config.Company{
		Name: "Acme", Knowledge: config.Knowledge{SkillsContainer: &skills},
	}})
	if got := reserved(); !slices.Equal(got, []string{"SKILLS", config.DefaultRootSpace}) {
		t.Errorf("after an apply moved the skills container: %v, want SKILLS and %s, "+
			"the company now current", got, config.DefaultRootSpace)
	}
}
