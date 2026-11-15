package cards_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// options are a question's options as an answer carries them (decision.Option),
// the second with no detail and the third with a label over two lines.
var options = []any{
	map[string]any{"id": "origin-rule", "label": "Origin rule", "detail": "Needs the Pro plan"},
	map[string]any{"id": "worker", "label": "A Worker route"},
	map[string]any{"id": "defer", "label": "Defer\nuntil the release"},
}

// optionLines is how [options] are written: by the id an answer names, each on
// a line of its own, a detail in parentheses, every entry on one line.
const optionLines = "origin-rule: Origin rule (Needs the Pro plan)\n" +
	"worker: A Worker route\n" +
	"defer: Defer until the release"

// A list of records is written entry by entry, led by its id, and a column reads
// its Else where its Path holds nothing; the model reads the cell as written.
func TestAListOfRecordsIsWrittenByWhatAnAnswerNamesEachBy(t *testing.T) {
	t.Parallel()
	entries := &cards.Entries{Lead: "id", Words: []string{"label", "detail"}}
	spec := cards.Spec{Table: &cards.Table{Title: "Waiting on you", Rows: "items",
		Columns: []cards.Column{{Key: "kind", Label: "Kind", Path: "kind", Type: wire.ColumnText},
			{Key: "options", Label: "Options", Path: "ask.decision.options",
				Else: "run.decision.options", Type: wire.ColumnText, Prose: true,
				Entries: entries}}}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"kind": "ask", "ask": map[string]any{"decision": map[string]any{
			"options": options}}},
		map[string]any{"kind": "run", "run": map[string]any{"decision": map[string]any{
			"options": options[:2]}}},
		map[string]any{"kind": "ask", "ask": map[string]any{}},
	}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	rows := built.Card.Table.Rows
	want := []any{optionLines, "origin-rule: Origin rule (Needs the Pro plan)\nworker: A Worker route", nil}
	for i, w := range want {
		if rows[i][1] != w {
			t.Errorf("row %d's options read %q, want %q", i, rows[i][1], w)
		}
	}
	summary, err := cards.SummaryOf(built, cards.ModelBytes)
	if err != nil {
		t.Fatal(err)
	}
	var digest struct{ Rows [][]any }
	if err := json.Unmarshal(summary, &digest); err != nil {
		t.Fatal(err)
	}
	if digest.Rows[0][1] != optionLines {
		t.Errorf("the model reads the options as %q, want %q", digest.Rows[0][1], optionLines)
	}
}

// An item's facts are details the drawer leaves out and the model's summary keeps;
// an empty one is absent.
func TestAnItemCarriesItsDetailsToTheModel(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, ID: "key", Key: "key",
		Title: "title", Prose: true,
		Items: &cards.Items{Path: "comments", ID: "id", Label: "body", Fact: "author",
			Prose: true, Noun: wire.Noun{One: "comment", Many: "comments"},
			Facts: []cards.Fact{{Label: "Asks", Path: "ask", Type: wire.ColumnText},
				{Label: "Options", Path: "decision.options", Type: wire.ColumnText,
					Entries: &cards.Entries{Lead: "id", Words: []string{"label", "detail"}}}}}}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(map[string]any{"key": "OPS-7", "title": "Route the library",
		"comments": []any{
			map[string]any{"id": "c-1", "body": "Seen it.", "author": "qa"},
			map[string]any{"id": "c-2", "body": "Which way?", "author": "sre", "ask": "ana",
				"decision": map[string]any{"options": options}},
		}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	items := built.Card.Entity.Items
	if len(items[0].Facts) != 0 {
		t.Errorf("a plain comment carries %+v, want no facts", items[0].Facts)
	}
	got := map[string]string{}
	for _, f := range items[1].Facts {
		if !f.Detail {
			t.Errorf("the item fact %q is drawn, want a detail the drawer leaves out", f.Label)
		}
		got[f.Label] = f.Value
	}
	if got["Asks"] != "ana" || got["Options"] != optionLines {
		t.Errorf("the question's facts are %v, want whom it asks and its options by id", got)
	}
	summary, err := cards.SummaryOf(built, cards.ModelBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), `"Options":"origin-rule: Origin rule (Needs the Pro plan)`) {
		t.Errorf("the model's summary drops the item's options: %s", summary)
	}
}

// AN ENTRY LIST NAMES WHAT TO WRITE, and only text holds one: entries with no
// field, a word naming no field, or a list written in a number column are a
// spec's mistakes, refused before any answer is read.
func TestAnEntryListNamesWhatToWrite(t *testing.T) {
	t.Parallel()
	column := func(typ wire.ColumnType, en *cards.Entries) cards.Spec {
		return cards.Spec{Table: &cards.Table{Title: "T", Columns: []cards.Column{
			{Key: "k", Label: "K", Path: "k", Type: typ, Entries: en}}}}
	}
	for name, spec := range map[string]cards.Spec{
		"no field":      column(wire.ColumnText, &cards.Entries{}),
		"an empty word": column(wire.ColumnText, &cards.Entries{Lead: "id", Words: []string{""}}),
		"a number":      column(wire.ColumnNumber, &cards.Entries{Lead: "id"}),
		"an item fact": {Entity: &cards.Entity{Title: "title", Items: &cards.Items{Path: "x",
			Label: "l", Noun: wire.Noun{One: "x", Many: "xs"},
			Facts: []cards.Fact{{Label: "F", Path: "f", Type: wire.ColumnNumber,
				Entries: &cards.Entries{Lead: "id"}}}}}},
	} {
		if spec.Validate() == nil {
			t.Errorf("%s validated", name)
		}
	}
	if err := column(wire.ColumnText, &cards.Entries{Words: []string{"label"}}).Validate(); err != nil {
		t.Errorf("entries led by their first word were refused: %v", err)
	}
}

// A LINK IS LISTED BY WHAT IT IS WHERE IT HAS NO NAME, AND A FLAG FACT IS WORDS
// ONLY WHERE IT IS SET: an item reads its label at Else where Label holds
// nothing (a task's link to a page, which has no title here), and a fact that
// is a flag reads its words when true and is no fact otherwise, as an item's
// flag is. Mutation: drop the Else, or keep a false flag as "false", and this
// fails.
func TestALinkWithNoNameIsListedByWhatItIs(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, ID: "task.key",
		Key: "task.key", Title: "task.title",
		Items: &cards.Items{Path: "links", ID: "other", Label: "title", Else: "kind",
			Fact: "key", Noun: wire.Noun{One: "link", Many: "links"},
			Facts: []cards.Fact{{Label: "Held by", Path: "derived", Type: wire.ColumnText,
				Flag: "the other end"}}}}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(map[string]any{"task": map[string]any{"key": "ENG-7",
		"title": "Ship it"}, "links": []any{
		map[string]any{"kind": "waiting_on", "other": "t-2", "key": "ENG-2",
			"title": "Release the backend", "derived": true},
		map[string]any{"kind": "page", "other": "p-9", "derived": false},
	}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	items := built.Card.Entity.Items
	if items[0].Label != "Release the backend" || items[1].Label != "page" || items[1].ID != "p-9" {
		t.Errorf("the links are listed as %+v, want the task by its title and the page by "+
			"what it is, with its id", items)
	}
	if len(items[0].Facts) != 1 || items[0].Facts[0].Value != "the other end" ||
		len(items[1].Facts) != 0 {
		t.Errorf("the flag reads %+v and %+v, want its words where set and nothing where not",
			items[0].Facts, items[1].Facts)
	}
	spec.Entity.Items.Facts[0].Entries = &cards.Entries{Lead: "id"}
	if spec.Validate() == nil {
		t.Error("a fact both listing entries and wording a flag validated")
	}
}
