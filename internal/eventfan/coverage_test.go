package eventfan_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/eventfan"
)

// THE DASHBOARD DECLARES EXACTLY THE COVERAGE THE ENGINE SENDS.
//
// One shape rides on every fanned answer, and it is the one place a screen
// learns that a node did not answer. A member the client declared and the
// engine never sends reads as undefined — every node "not answered" — and a key
// the engine sends that the client never declared is a missing node no screen
// can name.
func TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends(t *testing.T) {
	t.Parallel()
	sample := eventfan.Coverage{
		Nodes:    []eventfan.NodeCoverage{{ID: "node-a", Answered: true}},
		Complete: true,
	}
	for _, c := range []struct {
		iface string
		value any
	}{
		{"Coverage", sample},
		{"NodeCoverage", sample.Nodes[0]},
	} {
		raw, err := json.Marshal(c.value)
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatal(err)
		}
		members, err := clientsource.Interface(clientsource.Tree, c.iface)
		if err != nil {
			t.Fatalf("read %s: %v", c.iface, err)
		}
		declared := map[string]bool{}
		for _, m := range members {
			declared[m.Name] = true
			if m.Optional {
				t.Errorf("%s declares %s optional, and the engine sends it on every "+
					"answer — a screen guarding for its absence guards for nothing",
					c.iface, m.Name)
			}
		}
		if got, want := slices.Sorted(maps.Keys(declared)), slices.Sorted(maps.Keys(sent)); !slices.Equal(got, want) {
			t.Errorf("%s declares %v and the engine sends %v", c.iface, got, want)
		}
	}
}
