package cards_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
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
// while the person held the tracker is tracker data, and once the app is
// withdrawn the card is a notice saying it is hidden, never the rows. An
// administrator holds every app, and an app name this build does not know
// opens nothing. Mutation: return the card whatever the grants and this fails.
func TestARevokedAppHidesAStoredCard(t *testing.T) {
	t.Parallel()
	table := wire.Card{Type: wire.CardTable, Table: &wire.TableCard{Title: "Tasks",
		Columns: []wire.Column{{Key: "key", Label: "Key", Type: wire.ColumnText}},
		Rows:    [][]any{{"ENG-1"}}, RowCount: 1, AsOf: assistanttest.Epoch}}
	for name, c := range map[string]struct {
		apps  []string
		p     iam.Principal
		shown bool
	}{
		"still held":       {[]string{"tracker"}, person(iam.GrantTracker), true},
		"withdrawn":        {[]string{"tracker"}, person(iam.GrantKnowledge), false},
		"an administrator": {[]string{"tracker", "knowledge"}, person(iam.GrantAdmin), true},
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
	Open: &cards.Open{Screen: "work-tasks"}}}

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
	built, err := cards.Build(taskSpec, taskAnswer(3, "Ship the release"), assistanttest.Epoch)
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
	if tbl.Open == nil || tbl.Open.Href != "#/work/tasks" {
		t.Errorf("the card opens %+v, want the tasks list", tbl.Open)
	}

	built, err = cards.Build(taskSpec, taskAnswer(cards.MaxRows+50, "x"), assistanttest.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if tbl := built.Card.Table; len(tbl.Rows) != cards.MaxRows || !tbl.Truncated ||
		tbl.RowCount != cards.MaxRows+50 || built.Notice == nil {
		t.Errorf("a %d-row answer carded %d rows (truncated %v, count %d, notice %v)",
			cards.MaxRows+50, len(tbl.Rows), tbl.Truncated, tbl.RowCount, built.Notice)
	}

	long := strings.Repeat("a long title ", 100)
	built, err = cards.Build(taskSpec, taskAnswer(900, long), assistanttest.Epoch)
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

	if _, err := cards.Build(taskSpec, []byte("<html>"), assistanttest.Epoch); !errors.Is(err, cards.ErrNotJSON) {
		t.Errorf("an answer that is not JSON = %v, want ErrNotJSON", err)
	}
}

// AN ENTITY IS ONE RECORD, its facts and its list read from the answer, its link
// the engine's. Mutation: read the title from the wrong root and this fails.
func TestAnEntityIsOneRecordFromItsAnswer(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourcePage, Root: "page", ID: "id",
		Title: "title", Text: "body", Prose: true,
		Facts: []cards.Fact{{Label: "Version", Path: "version", Type: wire.ColumnNumber},
			{Label: "Took", Path: "took_ms", Type: wire.ColumnDuration}},
		Items: &cards.Items{Path: "comments", Kind: wire.ResourcePage, ID: "id", Label: "body"},
		Open: &cards.Open{Screen: "page", Params: map[string]string{"id": "id"},
			LabelFrom: "title"}}}
	id := "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	answer, _ := json.Marshal(map[string]any{
		"page": map[string]any{"id": id, "title": "Runbook", "body": "Step one.",
			"version": 4, "took_ms": 61000},
		"comments": []any{map[string]any{"id": "c1", "body": "Looks right"}}})
	built, err := cards.Build(spec, answer, assistanttest.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	e := built.Card.Entity
	if e.ID != id || e.Title != "Runbook" || e.Text != "Step one." || len(e.Items) != 1 ||
		e.Items[0].Label != "Looks right" {
		t.Errorf("the entity is %+v", e)
	}
	if len(e.Facts) != 2 || e.Facts[0].Value != "4" || e.Facts[1].Value != "1m1s" {
		t.Errorf("the facts are %+v", e.Facts)
	}
	if e.Open == nil || e.Open.Href != "#/knowledge/pages/"+id || e.Open.Label != "Runbook" {
		t.Errorf("the entity opens %+v", e.Open)
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
	if err != nil || link.Href != "#/work/ENG-42" || link.Label != "ENG-42" {
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
	built, err := cards.Build(taskSpec, taskAnswer(400, "Ship the release"), assistanttest.Epoch)
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
