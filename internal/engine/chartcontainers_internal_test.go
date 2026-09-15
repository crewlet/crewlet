package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

func keysOf(got []chartContainer) []string {
	out := make([]string, 0, len(got))
	for _, c := range got {
		out = append(out, c.Key)
	}
	return out
}

// THE KNOWLEDGE CONTAINERS A COMPANY NAMES ARE OBJECTS, and until this
// resolver had a caller they were not.
//
// [pages.Store.EnsureContainer] had none at all — its own doc says it "runs on
// every boot for every unit's space" and nothing ever ran it — so
// `pages_containers` was empty in every deployment: the Knowledge rail said
// "this node knows about no containers yet" beside a company whose agents had
// written pages, and `GET /containers` answered an empty list for ever.
//
// A page merely NAMES its container, so the pages were fine and only the thing
// that lists them was missing — which is exactly why nothing caught it. Every
// read that TAKES a container worked; only the read that ENUMERATES them was
// empty, and an empty enumeration is indistinguishable from a company that has
// no containers.
func TestEverySpaceTheChartNamesBecomesAContainer(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Space: "ENG", Purpose: "Build it",
		Roles: []*org.Role{
			{Name: "SWE", DeclaredHandle: "swe"},
			// A SEAT'S OWN SPACE, one level down from its unit's: a
			// founder naming one on a seat meant a container for that
			// seat's own writing.
			{Name: "DevRel", DeclaredHandle: "devrel", Space: "DEVREL"},
		},
	}}}
	o.Normalize()
	got := chartContainers(&Company{Org: o, Config: &config.Company{}})

	if want := []string{"ENG", "DEVREL", "HOME", "TS"}; !equal(keysOf(got), want) {
		t.Fatalf("containers = %v, want %v", keysOf(got), want)
	}
	if got[0].Name != "Engineering" || got[0].Purpose != "Build it" {
		t.Errorf("the unit's container = %+v, want its own name and purpose", got[0])
	}
	if got[1].Name != "DevRel" {
		t.Errorf("the seat's container = %+v, want the SEAT's name", got[1])
	}
}

// THE RESERVED TWO ARE MATERIALISED TOO, because the engine writes into them.
//
// A container the engine itself writes into and cannot list is the same defect
// one layer in: tool-skill pages and the org's own root pages would sit in
// containers the Knowledge rail refuses to name.
func TestTheReservedContainersAreMaterialisedFromTheirOwnAccessors(t *testing.T) {
	t.Parallel()
	skills := "SKILLS"
	root := "ROOT"
	got := chartContainers(&Company{Config: &config.Company{
		Knowledge: config.Knowledge{SkillsContainer: &skills, RootSpace: &root},
	}})
	if want := []string{"ROOT", "SKILLS"}; !equal(keysOf(got), want) {
		t.Fatalf("containers = %v, want %v", keysOf(got), want)
	}
}

// AND A COMPANY THAT TURNED THEM OFF GETS NEITHER.
//
// `skills_container: ""` is how an operator says "no container is reserved",
// and a backend of `none` has no container to name at all — answering with a
// key either way would have the engine creating a container nothing holds.
func TestAContainerNobodyNamedIsNotCreated(t *testing.T) {
	t.Parallel()
	off := ""
	got := chartContainers(&Company{Config: &config.Company{
		Knowledge: config.Knowledge{SkillsContainer: &off, RootSpace: &off},
	}})
	if len(got) != 0 {
		t.Errorf("containers = %v on a company that reserved none", keysOf(got))
	}
	if got := chartContainers(nil); got != nil {
		t.Errorf("containers = %v with no company at all", keysOf(got))
	}
}

// TWO UNITS SHARING A SPACE IS ONE CONTAINER, not a write that contends with
// itself at the broker and logs a failure for a configuration that is fine.
//
// And a key is normalised the way every container key is: a chart that wrote
// `eng` would create a second container no page is in.
func TestASharedSpaceIsWrittenOnceAndUpperCased(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{
		{Name: "Platform", Space: "eng"},
		{Name: "Tooling", Space: "ENG"},
	}}
	o.Normalize()
	got := chartContainers(&Company{Org: o, Config: &config.Company{}})

	if want := []string{"ENG", "HOME", "TS"}; !equal(keysOf(got), want) {
		t.Fatalf("containers = %v, want %v", keysOf(got), want)
	}
	// FIRST DECLARATION WINS, which matches how a shared project's lead is
	// chosen for the same key.
	if got[0].Name != "Platform" {
		t.Errorf("the shared container = %+v, want the first declaration", got[0])
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
