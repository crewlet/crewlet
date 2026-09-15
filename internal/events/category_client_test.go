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
// THE DRIFT ALREADY HAPPENED, and the client's own comment records it: the
// list held TEN where [events.CategoryNames] returns eight, so two chips could
// never match a row. A reader filtering a log down to nothing concludes the
// engine is quiet, which is the worst answer a log can give — it is
// indistinguishable from the true one.
//
// It was repaired by hand and nothing held it there. A category added to the
// engine is a filter nobody can apply, and one removed is a chip that empties
// the screen; both are silent, so both are checked.
//
// A chip for a real category with nothing in it is still useful — it says the
// category exists and is quiet — so what is asserted is that every chip names
// a category the engine has, and the ones it does not offer are reported.
func TestCategoryChipsAreTheEngines(t *testing.T) {
	engine := events.CategoryNames()

	body, err := clientsource.Declaration(clientsource.Tree,
		`(?s)const CATEGORIES = \[(.*?)\] as const`)
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
}
