package events_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events"
)

// TestCategoryChipsAreTheEngines holds the event log's filter chips against
// the categories this build actually assigns.
//
// A category added to the engine is a filter nobody can apply, and one removed
// is a chip that empties the log, which reads as a quiet engine, the worst
// answer a log can give. Both are silent, so both are checked: every chip names
// a category the engine has ([events.CategoryNames]), every category has a
// chip, and no chip is drawn twice. A chip for an empty category is useful, so
// emptiness is not asserted.
//
// ONE GATE: a second question about the chips belongs here, as
// `internal/clientsource`'s contract records.
func TestCategoryChipsAreTheEngines(t *testing.T) {
	t.Parallel()
	engine := events.CategoryNames()

	body, err := clientsource.Literal(clientsource.Tree(t), "CATEGORIES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	slices.Sort(client)
	if len(client) == 0 {
		t.Fatal("the event log offers no category chips, so this gate certifies nothing")
	}

	for _, name := range client {
		if !slices.Contains(engine, name) {
			t.Errorf("the event log offers a %q chip, which no event is registered "+
				"under: filtering on it empties the log and reads as a quiet engine. "+
				"The engine assigns %v", name, engine)
		}
	}
	var missing []string
	for _, name := range engine {
		if !slices.Contains(client, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the engine assigns %v, which the event log offers no chip for — "+
			"those rows can be read but not filtered to at all, which is the state "+
			"the chip list was introduced to end", missing)
	}
	if len(slices.Compact(slices.Clone(client))) != len(client) {
		t.Errorf("the event log offers %v, drawing a chip twice", client)
	}
}
