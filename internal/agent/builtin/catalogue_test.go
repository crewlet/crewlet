package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY TOOL A SEAT CAN BE OFFERED IS FILED IN THE CATALOGUE, ONCE, both
// ways, against the fully wired registry ([fullDeps]) plus the discovery
// pair: an unfiled built-in hides from a reader auditing what agents hold,
// and a filing nobody registers describes no tool.
//
// Less a registered tool whose row bars every agent ([builtin.Withheld]),
// asked of the authority table itself, so a row that starts admitting agents
// turns this red until its tool is filed.
func TestEveryToolASeatIsOfferedIsFiledOnce(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	registered, err := builtin.Register(reg, gated(fullDeps(t)))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	registered = slices.DeleteFunc(registered, func(name string) bool {
		humanOnly, _ := authz.HumanOnlyOf(authz.Action(name))
		return humanOnly
	})
	offered := slices.Concat(registered, toolname.Discovery())
	catalogued := builtin.Catalogued()

	for _, name := range offered {
		f, ok := builtin.FilingOf(name)
		if !ok {
			t.Errorf("%s is offered to a seat and filed nowhere in the catalogue", name)
			continue
		}
		if !f.Group.Valid() || !f.Gate.Valid() {
			t.Errorf("%s is filed under %q as %q, which this build does not know",
				name, f.Group, f.Gate)
		}
	}
	for _, name := range catalogued {
		if !slices.Contains(offered, name) {
			t.Errorf("%s is filed and no seat is offered it", name)
		}
	}
	if len(catalogued) != len(offered) {
		t.Errorf("the catalogue files %d tools and a seat is offered %d",
			len(catalogued), len(offered))
	}
	seen := map[string]bool{}
	for _, name := range catalogued {
		if seen[name] {
			t.Errorf("%s is filed twice", name)
		}
		seen[name] = true
	}
}

// AND EVERY GROUP HOLDS A TOOL, IN THE ORDER IT IS LISTED: an empty group is
// a rail entry opening an empty list, one without words is drawn as its key,
// and ranks run from zero with no gap because the dashboard sorts on them.
func TestEveryGroupHoldsItsToolsInOrder(t *testing.T) {
	t.Parallel()
	ranks := map[builtin.Group][]int{}
	for _, name := range builtin.Catalogued() {
		f, _ := builtin.FilingOf(name)
		ranks[f.Group] = append(ranks[f.Group], f.Rank)
	}
	for _, g := range builtin.Groups {
		if !g.Valid() {
			t.Errorf("group %q is listed and not valid", g)
		}
		if g.Label() == string(g) || g.Description() == "" {
			t.Errorf("group %q has no words: label %q, description %q",
				g, g.Label(), g.Description())
		}
		got := ranks[g]
		if len(got) == 0 {
			t.Errorf("no built-in is filed under %q", g)
			continue
		}
		for i, rank := range got {
			if rank != i {
				t.Errorf("%q lists its tools ranked %v, want 0 to %d in order",
					g, got, len(got)-1)
				break
			}
		}
	}
	if _, ok := builtin.FilingOf("an_mcp_servers_tool"); ok {
		t.Error("a tool the catalogue does not file was answered with a filing")
	}
}
