package cards_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// inferred is the card an answer is inferred into, and whether it drew one.
func inferred(t *testing.T, answer string) (wire.Card, bool) {
	t.Helper()
	doc, err := cards.Decode([]byte(answer))
	if err != nil {
		t.Fatal(err)
	}
	spec, rows, ok := cards.Infer(doc, "Things")
	if !ok {
		return wire.Card{}, false
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	built, err := cards.Build(spec, encoded, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if built.Empty {
		return wire.Card{}, false
	}
	return built.Card, true
}

// AN ANSWER'S SHAPE DECIDES ITS CARD, by one rule: a list is a table, a record
// wrapping one list beside plain values is that list's table, and any other
// record is one thing, an entity of its facts and its lists. Mutation: draw a
// record by its largest list, read a cursor beside a list as a second list,
// or take a record that names itself for a page, and a case here fails.
func TestAnAnswersShapeDecidesItsCard(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, answer string
		kind         wire.CardKind
		// rows is a table's row count, items an entity's list length.
		rows, items int
		facts       []string
	}{
		{name: "a list of records", answer: `[{"name":"a","n":1},{"name":"b","n":2}]`,
			kind: wire.CardTable, rows: 2},
		{name: "a page of a collection",
			answer: `{"items":[{"id":"1"},{"id":"2"},{"id":"3"}],"next_cursor":"x","total":3}`,
			kind:   wire.CardTable, rows: 3},
		{name: "a page with a cursor record",
			answer: `{"events":[{"id":"e1"}],"next":{"before_id":"e1","before_time":"t"}}`,
			kind:   wire.CardTable, rows: 1},
		{name: "a document holding two lists",
			answer: `{"name":"Nimbus","roles":[{"handle":"ceo"}],"units":[{"key":"eng"},{"key":"ops"}]}`,
			kind:   wire.CardEntity, items: 3, facts: []string{"Name"}},
		{name: "a record that names itself over one list",
			answer: `{"key":"ENG","name":"Engineering","statuses":[{"key":"todo"},{"key":"done"}]}`,
			kind:   wire.CardEntity, items: 2, facts: []string{"Name", "Key"}},
		{name: "a record of nested values",
			answer: `{"handle":"swe","llm":{"provider":"claude","model":"opus"},"tools":["a","b"]}`,
			kind:   wire.CardEntity, facts: []string{"Handle", "Tools", "LLM › model", "LLM › provider"}},
		{name: "a list of plain values", answer: `["alpha","beta"]`,
			kind: wire.CardEntity, items: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			card, ok := inferred(t, c.answer)
			if !ok {
				t.Fatal("no card was drawn")
			}
			if err := card.Validate(); err != nil {
				t.Fatal(err)
			}
			if card.Type != c.kind {
				t.Fatalf("a %s card, want %s", card.Type, c.kind)
			}
			switch card.Type {
			case wire.CardTable:
				if card.Table.RowCount != c.rows {
					t.Errorf("%d rows, want %d", card.Table.RowCount, c.rows)
				}
			case wire.CardEntity:
				if len(card.Entity.Items) != c.items {
					t.Errorf("%d items, want %d: %+v", len(card.Entity.Items), c.items,
						card.Entity.Items)
				}
				var labels []string
				for _, f := range card.Entity.Facts {
					labels = append(labels, f.Label)
				}
				if got := strings.Join(labels, ", "); got != strings.Join(c.facts, ", ") {
					t.Errorf("facts %q, want %q", got, strings.Join(c.facts, ", "))
				}
				if card.Entity.Title != "Things" {
					t.Errorf("titled %q, want the title it was given", card.Entity.Title)
				}
			}
		})
	}
	for _, empty := range []string{`[]`, `{"items":[],"next_cursor":null}`, `{"next":null}`, `"ok"`} {
		if card, ok := inferred(t, empty); ok {
			t.Errorf("%s drew a %s card; an answer holding nothing draws none", empty, card.Type)
		}
	}
}

// AN ENTITY NAMES NO KIND OF THE DASHBOARD'S where it is none of them, and a
// kind it does name is still one of the set. Mutation: refuse the empty kind,
// or accept any kind, and a case here fails.
func TestAnEntityNamesAKindOfTheSetOrNone(t *testing.T) {
	t.Parallel()
	none := wire.Card{Type: wire.CardEntity, Entity: &wire.EntityCard{Title: "Configuration",
		Items: []wire.EntityItem{{ID: "1", Label: "ceo"}}}}
	if err := none.Validate(); err != nil {
		t.Errorf("an entity of no kind is refused: %v", err)
	}
	bad := wire.Card{Type: wire.CardEntity, Entity: &wire.EntityCard{Kind: "planet", Title: "x"}}
	if err := bad.Validate(); err == nil {
		t.Error("an entity of a kind outside the set is accepted")
	}
	badItem := wire.Card{Type: wire.CardEntity, Entity: &wire.EntityCard{Title: "x",
		Items: []wire.EntityItem{{Kind: "planet", ID: "1", Label: "a"}}}}
	if err := badItem.Validate(); err == nil {
		t.Error("an entity item of a kind outside the set is accepted")
	}
}
