package cards_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
)

// person is an active person holding grants.
func person(grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
		Seat: "analyst", Stage: iam.StageActive, Grants: grants}
}

// A REVOKED APP HIDES A STORED CARD.
//
// A transcript outlives the grants it was read under: a table of tasks read
// while the person held Worklet is Worklet data, and once the app is
// withdrawn the card is a notice saying it is hidden, never the rows. An
// administrator holds every app, and an app name this build does not know
// opens nothing. Mutation: return the card whatever the grants and this fails.
func TestARevokedAppHidesAStoredCard(t *testing.T) {
	t.Parallel()
	table := wire.Card{Type: wire.CardTable, Table: &wire.TableCard{Title: "Tasks",
		Columns: []wire.Column{{Key: "key", Label: "Key", Type: wire.ColumnText}},
		Rows:    [][]any{{"ENG-1"}}, RowCount: 1, AsOf: evals.Epoch}}
	for name, c := range map[string]struct {
		apps  []string
		p     iam.Principal
		shown bool
	}{
		"still held":       {[]string{"worklet"}, person(iam.GrantWorklet), true},
		"withdrawn":        {[]string{"worklet"}, person(iam.GrantDoclet), false},
		"an administrator": {[]string{"worklet", "doclet"}, person(iam.GrantAdmin), true},
		"no app needed":    {nil, person(), true},
		"an unknown app":   {[]string{"forecasts"}, person(iam.GrantAdmin), false},
	} {
		got, shown := cards.Visible(c.apps, table, c.p)
		if shown != c.shown {
			t.Errorf("%s: shown = %v, want %v", name, shown, c.shown)
		}
		if !shown && (got.Type != wire.CardNotice || got.Notice.Text != cards.WithheldText) {
			t.Errorf("%s: the hidden card is %+v, want the withheld notice", name, got)
		}
		if shown && got.Table == nil {
			t.Errorf("%s: the shown card lost its table", name)
		}
	}
}

// taskSpec is a table over a list of task rows.
var taskSpec = cards.Spec{Table: &cards.Table{Title: "Tasks", Rows: "items",
	Columns: []cards.Column{
		{Key: "key", Label: "Key", Path: "key", Type: wire.ColumnText},
		{Key: "title", Label: "Title", Path: "title", Type: wire.ColumnText, Prose: true},
		{Key: "tokens", Label: "Tokens", Path: "spend.tokens", Type: wire.ColumnTokens},
		{Key: "due", Label: "Due", Path: "due", Type: wire.ColumnDatetime},
	},
	Open: &cards.Open{Screen: "worklet-tasks"}}}

func taskAnswer(rows int, title string) []byte {
	items := make([]map[string]any, rows)
	for i := range items {
		items[i] = map[string]any{"key": fmt.Sprintf("ENG-%d", i+1), "title": title,
			"spend": map[string]any{"tokens": 1200 + i}, "due": "2026-11-02T00:00:00Z"}
	}
	b, _ := json.Marshal(map[string]any{"items": items, "complete": true})
	return b
}

// A TABLE IS THE ANSWER'S OWN FIELDS, cut whole rows at a time.
//
// Each cell is read from its column's path, typed by the column (a token count
// stays a number, a datetime is an instant), a field the answer does not hold
// is empty rather than guessed, and a read past the row cap or the byte cap is
// cut to whole rows, marked truncated with the read's own row count, and
// followed by a notice. Mutation: drop the row cap, or cut a row mid-cell,
// and this fails.
func TestATableIsTheAnswersOwnFieldsCutWholeRows(t *testing.T) {
	t.Parallel()
	built, err := cards.Build(taskSpec, taskAnswer(3, "Ship the release"), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	tbl := built.Card.Table
	if built.Notice != nil || tbl.Truncated || tbl.RowCount != 3 || len(tbl.Rows) != 3 {
		t.Fatalf("a 3-row answer carded as %+v with notice %v", tbl, built.Notice)
	}
	if row := tbl.Rows[0]; row[0] != "ENG-1" || row[1] != "Ship the release" ||
		row[2] != json.Number("1200") || row[3] != "2026-11-02T00:00:00Z" {
		t.Errorf("the first row is %v", row)
	}
	if tbl.Open == nil || tbl.Open.Href != "#/worklet/tasks" {
		t.Errorf("the card opens %+v, want the tasks list", tbl.Open)
	}

	built, err = cards.Build(taskSpec, taskAnswer(cards.MaxRows+50, "x"), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if tbl := built.Card.Table; len(tbl.Rows) != cards.MaxRows || !tbl.Truncated ||
		tbl.RowCount != cards.MaxRows+50 || built.Notice == nil {
		t.Errorf("a %d-row answer carded %d rows (truncated %v, count %d, notice %v)",
			cards.MaxRows+50, len(tbl.Rows), tbl.Truncated, tbl.RowCount, built.Notice)
	}

	long := strings.Repeat("a long title ", 100)
	built, err = cards.Build(taskSpec, taskAnswer(900, long), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(built.Card)
	if len(encoded) > cards.MaxBytes || !built.Card.Table.Truncated || built.Notice == nil {
		t.Errorf("a %d-byte table was carded at %d bytes, truncated %v", 900*len(long),
			len(encoded), built.Card.Table.Truncated)
	}
	for _, row := range built.Card.Table.Rows {
		if row[1] != long {
			t.Fatal("a row was cut mid-cell")
		}
	}

	if _, err := cards.Build(taskSpec, []byte("<html>"), evals.Epoch); !errors.Is(err, cards.ErrNotJSON) {
		t.Errorf("an answer that is not JSON = %v, want ErrNotJSON", err)
	}
}

// AN ENTITY IS ONE RECORD, its facts and its list read from the answer, its link
// the engine's. Mutation: read the title from the wrong root and this fails.
func TestAnEntityIsOneRecordFromItsAnswer(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourcePage, Root: "page", ID: "id",
		Title: "title", Text: "body", Prose: true,
		Facts: []cards.Fact{{Label: "Version", Path: "version", Type: wire.ColumnNumber,
			Detail: true},
			{Label: "Took", Path: "took_ms", Type: wire.ColumnDuration},
			{Label: "Status", Path: "status", Type: wire.ColumnLabel},
			{Label: "Author", Path: "author", Type: wire.ColumnText}},
		Items: &cards.Items{Path: "comments", Kind: wire.ResourcePage, ID: "id", Label: "body",
			Noun: wire.Noun{One: "comment", Many: "comments"}},
		Open: &cards.Open{Screen: "page", Params: map[string]string{"id": "id"},
			LabelFrom: "title"}}}
	id := "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	answer, _ := json.Marshal(map[string]any{
		"page": map[string]any{"id": id, "title": "Runbook", "body": "Step one.",
			"version": 4, "took_ms": 61000, "status": "in_review", "author": "ada"},
		"comments": []any{map[string]any{"id": "c1", "body": "Looks right"}}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	e := built.Card.Entity
	if e.ID != id || e.Title != "Runbook" || e.Text != "Step one." || len(e.Items) != 1 ||
		e.Items[0].Label != "Looks right" {
		t.Errorf("the entity is %+v", e)
	}
	// TYPED VALUES, AS THE API ANSWERED THEM: the dashboard words each as its
	// screens word the same field. Text carries no type, and a detail fact
	// stays on the card for the model.
	want := []wire.Fact{{Label: "Version", Value: "4", Type: wire.ColumnNumber, Detail: true},
		{Label: "Took", Value: "61000", Type: wire.ColumnDuration},
		{Label: "Status", Value: "in_review", Type: wire.ColumnLabel},
		{Label: "Author", Value: "ada"}}
	if !reflect.DeepEqual(e.Facts, want) {
		t.Errorf("the facts are %+v, want %+v", e.Facts, want)
	}
	if e.Open == nil || e.Open.Href != "#/doclet/pages/"+id || e.Open.Label != "Runbook" {
		t.Errorf("the entity opens %+v", e.Open)
	}
	if e.ItemsNoun == nil || *e.ItemsNoun != (wire.Noun{One: "comment", Many: "comments"}) {
		t.Errorf("the items are called %+v", e.ItemsNoun)
	}
}

// A PLAIN LIST IS ITS VALUES, each said once: an item path the spec leaves
// unnamed reads nothing, where it used to read the item itself, so every
// standing policy came out as its label and again as its fact. And a record
// path left unnamed is no value either, so a card named by its spec takes no
// id from the record's own name. Mutation: read item paths with lookup and
// the fact is the policy again.
func TestAPlainListSaysEachValueOnce(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceUnit, Name: "Company principles",
		Prose: true, Facts: []cards.Fact{{Label: "Mission", Path: "mission", Type: wire.ColumnText}},
		Items: &cards.Items{Path: "policies", Kind: wire.ResourceUnit, Plain: true, Prose: true,
			Noun: wire.Noun{One: "policy", Many: "policies"}}}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(map[string]any{"name": "Acme", "mission": "Build trust.",
		"policies": []any{"Reply within a day.", "Never share a secret."}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	e := built.Card.Entity
	want := []wire.EntityItem{{Kind: wire.ResourceUnit, ID: "1", Label: "Reply within a day."},
		{Kind: wire.ResourceUnit, ID: "2", Label: "Never share a secret."}}
	if !reflect.DeepEqual(e.Items, want) {
		t.Errorf("the policies are %+v, want each once as its label: %+v", e.Items, want)
	}
	if e.ID != "" || e.Title != "Company principles" {
		t.Errorf("the card is %q named %q, want no id and the spec's name", e.ID, e.Title)
	}
	spec.Entity.Items.Noun = wire.Noun{}
	if spec.Validate() == nil {
		t.Error("a list with no noun to count it by validated")
	}
}

// A LINK IS THE ENGINE'S, and a parameter is held to its screen's grammar, so
// a value can never name a second segment or another screen: a project key
// where a task goes, a path where an id goes, a parameter the screen does not
// take. Mutation: drop the grammar check and the path one passes.
func TestALinkHoldsEachParameterToItsGrammar(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		screen string
		params map[string]string
	}{
		"a project key as a task":  {"item", map[string]string{"id": "ENG"}},
		"a path as a page":         {"page", map[string]string{"id": "../../settings/secrets"}},
		"a parameter not taken":    {"home", map[string]string{"id": "x"}},
		"a screen the table lacks": {"secrets", nil},
		"a turn named by a key":    {"turn", map[string]string{"id": "ENG-1"}},
	} {
		if _, err := cards.Link(c.screen, c.params, ""); !errors.Is(err, cards.ErrNoLink) {
			t.Errorf("%s: Link = %v, want ErrNoLink", name, err)
		}
	}
	link, err := cards.Link("item", map[string]string{"id": "ENG-42"}, "")
	if err != nil || link.Href != "#/worklet/ENG-42" || link.Label != "ENG-42" {
		t.Errorf("a task link = %+v, %v", link, err)
	}
}

// linkSamples are the values each link parameter is filled with in the
// fixture, of the shape the dashboard's router fixture fills them with.
var linkSamples = map[string]string{
	"id": "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f", "key": "ENG", "handle": "pm", "unit": "platform",
}

// THE LINK TABLE FIXTURE is every screen the engine links to, as the engine
// renders it, which the dashboard's assistant link suite replays through its
// own router: each rendered address must open the screen it names
// (dashboard/src/app/assistantLinks.test.ts). `make assistant-fixtures`
// rewrites it.
func TestTheLinkTableFixtureIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	var out []wire.Link
	for _, screen := range cards.Screens() {
		params := map[string]string{}
		for _, p := range cards.ScreenParams(screen) {
			params[p] = linkSamples[p]
		}
		link, err := cards.Link(screen, params, "")
		if err != nil {
			t.Fatalf("%s: %v", screen, err)
		}
		out = append(out, link)
	}
	if len(out) < 20 {
		t.Fatalf("the link table holds %d screens", len(out))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assistanttest.Golden(t, "testdata/links.json", append(b, '\n'))
}

// A SUMMARY FITS ITS BUDGET WHOLE ROWS AT A TIME and says how many it left out,
// and every list tells the model that what the person may not see is not
// there. Mutation: drop the access note, or cut a row, and this fails.
func TestASummaryFitsItsBudgetWholeRows(t *testing.T) {
	t.Parallel()
	built, err := cards.Build(taskSpec, taskAnswer(400, "Ship the release"), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cards.Summary(built.Card, 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Rows     [][]any `json:"rows"`
		RowCount int     `json:"row_count"`
		Shown    int     `json:"rows_shown"`
		Note     string  `json:"note"`
		Access   string  `json:"access"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	if len(out) > 4<<10 || d.Shown == 0 || d.Shown != len(d.Rows) || d.RowCount != 400 ||
		d.Note == "" || d.Access != cards.AccessNote {
		t.Errorf("the summary is %d bytes, %d of %d rows, note %q, access %q", len(out),
			d.Shown, d.RowCount, d.Note, d.Access)
	}
}

// THE DRAWER DRAWS A FEW COLUMNS, NAME FIRST, and the card keeps the rest after
// them as detail, so a download and the model still have every column.
func TestATableLeadsWithTheColumnsTheDrawerDraws(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Table: &cards.Table{Title: "Tasks", Rows: "items",
		Brief: []string{"title", "status"},
		Columns: []cards.Column{{Key: "key", Label: "Key", Path: "key", Type: wire.ColumnText},
			{Key: "title", Label: "Title", Path: "title", Type: wire.ColumnText, Prose: true},
			{Key: "status", Label: "Status", Path: "status", Type: wire.ColumnLabel},
			{Key: "due", Label: "Due", Path: "due", Type: wire.ColumnDatetime}}}}
	answer := []byte(`{"items":[{"key":"FAIR-1","title":"Settle up","status":"todo"}]}`)
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range built.Card.Table.Columns {
		got = append(got, fmt.Sprintf("%s:%t", c.Key, c.Detail))
	}
	want := []string{"title:false", "status:false", "key:true", "due:true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the columns are %v, want %v", got, want)
	}
	if row := built.Card.Table.Rows[0]; row[0] != "Settle up" || row[1] != "todo" || row[2] != "FAIR-1" {
		t.Errorf("the row is %v, in another order than its columns", row)
	}
}

// A TABLE DRAWS AT MOST FOUR COLUMNS in the drawer, each one of its own.
func TestATableDrawsAFewOfItsOwnColumns(t *testing.T) {
	t.Parallel()
	cols := []cards.Column{}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		cols = append(cols, cards.Column{Key: k, Label: k, Path: k, Type: wire.ColumnText})
	}
	for name, brief := range map[string][]string{
		"too many": {"a", "b", "c", "d", "e"},
		"unknown":  {"a", "z"},
		"twice":    {"a", "a"},
	} {
		spec := cards.Spec{Table: &cards.Table{Title: "T", Columns: cols, Brief: brief}}
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: a table drawing %v was accepted", name, brief)
		}
	}
}

// A RECORD LINK NAMED BY NOTHING IS NAMED BY WHAT IT OPENS, never by the raw
// value its address carries: a seat's handle ("pm", which a two-letter
// handle made a near-empty button) or a page's UUID is how the address spells
// the record, and "Open seat" is what a person reads. A task, a project or a
// space keeps its key, which people know. Mutation: fall back to the first
// path value again, and this fails.
func TestARecordLinkIsNamedByWhatItOpens(t *testing.T) {
	t.Parallel()
	for screen, params := range map[string]map[string]string{
		"seat": {"handle": "pm"}, "page": {"id": "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"},
		"turn": {"id": "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"},
	} {
		link, err := cards.Link(screen, params, "")
		if err != nil {
			t.Fatalf("%s: %v", screen, err)
		}
		for _, v := range params {
			if link.Label == v || !strings.HasPrefix(link.Label, "Open ") {
				t.Errorf("a %s link with no name reads %q", screen, link.Label)
			}
		}
	}
	if link, err := cards.Link("project", map[string]string{"key": "FAIR"}, ""); err != nil ||
		link.Label != "FAIR" {
		t.Errorf("a project link with no name reads %q (%v), want its key", link.Label, err)
	}
}

// A RECORD CARRIES THE KEY PROSE WRITES IT BY, read from the record as its
// spec says, so the drawer turns the answer's first "GOP-11" into the task's
// chip without guessing which of a card's strings is a key. A spec that names
// no key carries none: the subtitle is never taken for one. Mutation: drop
// the Key from buildEntity and the card names no key.
func TestARecordCarriesTheKeyProseWritesItBy(t *testing.T) {
	t.Parallel()
	answer, _ := json.Marshal(map[string]any{"task": map[string]any{
		"key": "GOP-11", "title": "Android 12 crash when lesson audio starts"}})
	keyed := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, Root: "task",
		ID: "key", Key: "key", Title: "title", Subtitle: "key"}}
	built, err := cards.Build(keyed, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if got := built.Card.Entity.Key; got != "GOP-11" {
		t.Errorf("the task's key is %q, want GOP-11", got)
	}
	unkeyed := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, Root: "task",
		ID: "key", Title: "title", Subtitle: "key"}}
	built, err = cards.Build(unkeyed, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if got := built.Card.Entity.Key; got != "" {
		t.Errorf("a spec with no key carries %q", got)
	}
}

// THE MODEL IS TOLD EACH RECORD'S KEY, beside its title, so it names the
// record the way the drawer finds it. Mutation: leave Key out of the digest.
func TestTheModelIsToldARecordsKey(t *testing.T) {
	t.Parallel()
	card := wire.Card{Type: wire.CardEntity, Entity: &wire.EntityCard{Kind: wire.ResourceTask,
		ID: "GOP-11", Key: "GOP-11", Title: "Android 12 crash"}}
	out, err := cards.Summary(card, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var digest map[string]any
	if err := json.Unmarshal(out, &digest); err != nil {
		t.Fatal(err)
	}
	if digest["key"] != "GOP-11" {
		t.Errorf("the model reads %s, with no key", out)
	}
}
