package assistant_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// EVERY TABLE CARD IS DOWNLOADABLE: the card carries the export routes of its
// own id, which read the stored card again as its owner, so the drawer offers
// CSV and XLSX for every table a read builds.
//
// Mutation: drop the exports in emitCard and the card carries none.
func TestEveryTableCardCarriesItsDownloads(t *testing.T) {
	t.Parallel()
	list := assistanttest.Routes(map[string]exec.Response{
		"GET /worklet": evals.JSON(map[string]any{"items": []any{
			map[string]any{"key": "ENG-7", "title": "Rotate keys", "status": "todo"}}}),
	})
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "list_tasks")}, Handler: list,
		Rounds: []evals.Round{evals.Call("c1", "list_tasks", map[string]any{}),
			evals.Say("One task is open.")}})
	accepted, err := f.Ask("list", "What is open?", nil)
	if err != nil {
		t.Fatal(err)
	}
	var table *wire.CardEvent
	for _, fr := range f.Follow(accepted.TurnID, 0) {
		if fr.Kind == wire.EventCard && fr.Card.Card.Type == wire.CardTable {
			table = fr.Card
		}
	}
	if table == nil {
		t.Fatal("the read built no table card")
	}
	want := wire.ExportsOf(table.CardID)
	if got := table.Card.Table.Exports; got == nil || *got != *want {
		t.Errorf("the table card's downloads are %+v, want %+v", got, want)
	}
	if table.CardID != assistantdomain.CardID(accepted.TurnID, 1) {
		t.Errorf("the card is %s, want the turn's first card", table.CardID)
	}
}
