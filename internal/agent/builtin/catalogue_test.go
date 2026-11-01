package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY TOOL A SEAT CAN BE OFFERED IS FILED IN THE CATALOGUE, ONCE.
//
// Both ways, against the registry a seat's tools come from with every
// dependency wired ([fullDeps]) and the discovery pair the runner adds to
// every phase that discovers. A built-in added without a filing is one the
// catalogue does not show, so a reader auditing what agents hold sees less
// than they hold; a filing for a tool nobody registers any more is a row
// describing a tool that does not exist.
func TestEveryToolASeatIsOfferedIsFiledOnce(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	registered, err := builtin.Register(reg, gated(fullDeps(t)))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
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

// AND EVERY GROUP HOLDS A TOOL, IN THE ORDER IT IS LISTED.
//
// A group with nothing filed under it is a rail entry that opens an empty
// list, and a group missing its words is one the dashboard draws as its key.
// The ranks are each group's own order, from zero, with no gap: the
// dashboard sorts on them.
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
