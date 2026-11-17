package engine

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/org"
)

// THE KNOWLEDGE BASE DECLARES EXACTLY THE SPACES THE CHART MAKES CONTAINERS OF.
//
// A page is created only in a declared space (doclet.Reserved.Declared), and
// the containers are [chartContainers]'s; a second walk of the chart would
// drift from it (a seat's `space:` left out, a key spelled differently) and a
// page would be refused a space the Doclet rail lists, or created in one
// nobody made. So the declared list IS
// that walk's keys, the reserved two still come from their own accessors, and
// a node running no company declares nothing, refusing every create.
func TestReservedContainersDeclareEveryChartSpace(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Space: "eng", Purpose: "Build it",
		Roles: []*org.Role{
			{Name: "SWE", DeclaredHandle: "swe"},
			{Name: "DevRel", DeclaredHandle: "devrel", Space: " devrel "},
		},
	}, {
		// TWO UNITS ON ONE SPACE declare it once.
		Name: "Platform", Space: "ENG",
		Roles: []*org.Role{{Name: "SRE", DeclaredHandle: "sre"}},
	}}}
	o.Normalize()
	skills, root := "SKILLS", "ROOT"
	company := &Company{Org: o, Config: &config.Company{
		Knowledge: config.Knowledge{SkillsContainer: &skills, RootSpace: &root},
	}}

	got := reservedContainers(company)
	if want := keysOf(chartContainers(company)); !reflect.DeepEqual(got.Declared, want) {
		t.Fatalf("declared = %v, want the chart's containers %v", got.Declared, want)
	}
	if want := []string{"ENG", "DEVREL", "ROOT", "SKILLS"}; !reflect.DeepEqual(got.Declared, want) {
		t.Errorf("declared = %v, want every unit's and seat's space, the root "+
			"and the skills container, once each, in canonical form", got.Declared)
	}
	if got.Skills != "SKILLS" || got.Root != "ROOT" {
		t.Errorf("reserved = (skills %q, root %q), want the company's own two",
			got.Skills, got.Root)
	}

	for name, none := range map[string]*Company{
		"no company": nil, "a company with no document": {Org: o},
	} {
		if held := reservedContainers(none); !reflect.DeepEqual(held, doclet.Reserved{}) {
			t.Errorf("%s declares %+v, want nothing at all", name, held)
		}
	}
}
