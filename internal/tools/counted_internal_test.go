package tools

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// attrsSeen records the attribute names each counter was handed.
type attrsSeen map[string][]string

func (a attrsSeen) Add(name string, _ uint64, attrs metrics.Attrs) {
	a[name] = slices.Sorted(maps.Keys(attrs))
}

// A COUNTER'S LABELS ARE THE CATALOGUE'S (#423 review): the recorder drops a
// label the catalogue does not declare without a word, so a "paths" label
// added to a recording stayed green. Each counter here is held to the
// catalogue when the package loads, and records exactly the declared labels
// through a typed method, so a recording of the wrong arity does not compile.
//
// Mutation: declare a counter with ("tool", "paths") and the package refuses
// to load; record a second label in toolCounted.add and this names it.
func TestEveryCounterRecordsTheCataloguesLabels(t *testing.T) {
	t.Parallel()
	seen := attrsSeen{}
	argsOutsideSchema.add(seen, "make_thing")
	policyRefusalLoops.add(seen, "make_thing")
	for _, inst := range metrics.Catalogue() {
		got, recorded := seen[inst.Name]
		if !recorded {
			continue
		}
		if want := slices.Sorted(slices.Values(inst.Attributes)); !slices.Equal(got, want) {
			t.Errorf("%s recorded %v, and the catalogue declares %v", inst.Name, got, want)
		}
	}
	if len(seen) != 2 {
		t.Errorf("recorded %d counters, want both", len(seen))
	}
	for _, bad := range []func(){
		func() { mustCount(metrics.ToolArgsOutsideSchema, "tool", "paths") },
		func() { mustCount(metrics.ToolArgsOutsideSchema) },
		func() { mustCount("crewlet.tools.nothing", "tool") },
		func() { countedByTool(metrics.ToolArgsOutsideSchema + ".unknown") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("a counter outside its catalogue declaration was accepted")
				}
			}()
			bad()
		}()
	}
}
