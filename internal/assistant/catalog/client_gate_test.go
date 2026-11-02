package catalog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// EVERY PAGE ACTION THE ENGINE ACCEPTS IS ONE A SCREEN MAY DECLARE, AND NO
// OTHER, WITH THE SAME CONFIRM.
//
// The page context offers only the ids the dashboard's ASSISTANT_PAGE_ACTIONS
// declares (`app/assistant/context.ts`), and the engine accepts a proposal only
// for an id catalog.PageActions holds. An id only the engine knows is one no
// screen can ever be asked for; one only the dashboard knows is a change the
// screen offers that every proposal for it is refused; and a confirm that
// differs puts an approval card in front of a view change the screen applies
// at once, or applies a draft change the person never approved. Mutation:
// drop an id from either list, or flip one's confirm, and this fails.
func TestEveryPageActionIsTheDashboardsToo(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ASSISTANT_PAGE_ACTIONS")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	client := map[string]string{}
	for _, id := range ids {
		row, err := clientsource.Property(body, id)
		if err != nil {
			t.Fatal(err)
		}
		confirm := clientsource.Field(row, "confirm")
		if len(confirm) != 1 {
			t.Fatalf("ASSISTANT_PAGE_ACTIONS[%q] states %d confirms, want one: %v", id,
				len(confirm), confirm)
		}
		client[id] = confirm[0]
	}
	if len(client) == 0 {
		t.Fatal("ASSISTANT_PAGE_ACTIONS declares no page action; the gate read nothing")
	}
	engine := map[string]string{}
	for _, a := range catalog.PageActions {
		engine[a.ID] = string(a.Confirm)
	}
	for id, confirm := range engine {
		got, ok := client[id]
		switch {
		case !ok:
			t.Errorf("catalog.PageActions holds %q and ASSISTANT_PAGE_ACTIONS does not: no "+
				"screen can be asked for it", id)
		case got != confirm:
			t.Errorf("%q confirms %q in the catalogue and %q in the dashboard", id, confirm, got)
		}
	}
	for id := range client {
		if _, ok := engine[id]; !ok {
			t.Errorf("ASSISTANT_PAGE_ACTIONS declares %q and catalog.PageActions does not: the "+
				"engine refuses every proposal for it", id)
		}
	}
}
