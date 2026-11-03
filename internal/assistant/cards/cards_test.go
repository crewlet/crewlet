package cards_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
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
	if tbl.Truncated || tbl.More || tbl.RowCount != 3 || len(tbl.Rows) != 3 {
		t.Fatalf("a 3-row answer carded as %+v", tbl)
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
		tbl.RowCount != cards.MaxRows+50 {
		t.Errorf("a %d-row answer carded %d rows (truncated %v, count %d)",
			cards.MaxRows+50, len(tbl.Rows), tbl.Truncated, tbl.RowCount)
	}

	long := strings.Repeat("a long title ", 100)
	built, err = cards.Build(taskSpec, taskAnswer(900, long), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(built.Card)
	if len(encoded) > cards.MaxBytes || !built.Card.Table.Truncated {
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

// A FLAG READS AS WORDS WHERE IT IS SET AND AS NOTHING WHERE IT IS NOT.
//
// A room's member list says which members follow every message: a bare "true"
// beside a handle tells neither the person nor the model what it means, and
// "false" on every other line is noise. Mutation: render the fact as its raw
// value and the first item reads "true".
func TestAFlagReadsAsWordsOnlyWhereItIsSet(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceRoom, ID: "id", Key: "name",
		KeyPrefix: "#", Title: "name",
		Items: &cards.Items{Path: "members", ID: "handle", Label: "handle",
			Fact: "follow_all", Flag: "follows every message",
			Noun: wire.Noun{One: "member", Many: "members"}}}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	answer, _ := json.Marshal(map[string]any{"id": "r1", "name": "engineering",
		"members": []any{
			map[string]any{"handle": "amir", "follow_all": true},
			map[string]any{"handle": "dev", "follow_all": false},
			map[string]any{"handle": "jane"},
		}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range built.Card.Entity.Items {
		got = append(got, it.Label+"="+it.Fact)
	}
	want := []string{"amir=follows every message", "dev=", "jane="}
	if !slices.Equal(got, want) {
		t.Errorf("the members read %v, want %v", got, want)
	}
	// A FLAG WITH NOTHING TO READ is a spec error, not a silent blank.
	spec.Entity.Items.Fact = ""
	if spec.Validate() == nil {
		t.Error("a flag with no fact path validated")
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

// A RECORD KNOWN ONLY BY ITS UUID IS HEADED BY ITS SPEC'S NAME, never by the
// id, which reads as nothing to anybody: a direct conversation has no name of
// its own, so its card is headed "Direct conversation", where a named room is
// headed by its name after the "#" every screen draws, and a record whose id
// is a key a person reads is still headed by that key, with no prefix. A
// column's prefix is drawn before every value and never before nothing.
// Mutation: head a card by its id whatever the id is, refuse a spec naming
// both a title and a name, or draw a prefix before an empty value, and this
// fails.
func TestARecordKnownOnlyByItsIDIsHeadedByItsName(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Root: "channel", ID: "id", Title: "name",
		TitlePrefix: "#", Name: "Direct conversation",
		Facts: []cards.Fact{{Label: "Kind", Path: "kind", Type: wire.ColumnLabel}}}}
	const room = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	for answer, want := range map[string]string{
		`{"channel":{"id":"` + room + `","kind":"dm"}}`:                     "Direct conversation",
		`{"channel":{"id":"` + room + `","name":"launch","kind":"public"}}`: "#launch",
		`{"channel":{"id":"ENG-4","kind":"public"}}`:                        "ENG-4",
	} {
		built, err := cards.Build(spec, []byte(answer), evals.Epoch)
		if err != nil || built.Empty || built.Card.Entity == nil ||
			built.Card.Entity.Title != want {
			t.Errorf("%s is headed %+v (%v), want %q", answer, built.Card.Entity, err, want)
		}
	}
	rooms := cards.Spec{Table: &cards.Table{Title: "Rooms", Rows: "rooms", Columns: []cards.Column{
		{Key: "room", Label: "Room", Path: "name", Type: wire.ColumnText, Prefix: "#"},
		{Key: "unread", Label: "Unread", Path: "unread", Type: wire.ColumnNumber}}}}
	built, err := cards.Build(rooms, []byte(`{"rooms":[{"name":"launch","unread":2},`+
		`{"name":"","unread":1}]}`), evals.Epoch)
	if err != nil || built.Card.Table == nil || len(built.Card.Table.Rows) != 2 ||
		built.Card.Table.Rows[0][0] != "#launch" || built.Card.Table.Rows[1][0] != "" {
		t.Errorf("a room column draws %+v (%v), want #launch and nothing", built.Card.Table, err)
	}
}

// A RECORD CARRIES THE KEY PROSE WRITES IT BY, read from the record as its
// spec says, so the drawer turns the answer's first "GOP-11" into the task's
// chip without guessing which of a card's strings is a key. A spec of a kind
// named by its title carries none: the subtitle is never taken for one.
// Mutation: drop the Key from buildEntity and the card names no key.
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
	// A PAGE IS NAMED BY ITS TITLE: its subtitle (here the same field) is
	// never taken for a key.
	unkeyed := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourcePage, Root: "task",
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

// A SPEC OF A KEYED KIND IS REFUSED WITHOUT ITS KEY, and one of any other kind
// with one, so a new read can never ship a task or a room the drawer cannot
// name; and only a room is headed by the room it is. Mutation: drop either
// check from Validate and its specs pass.
func TestAKeyedKindNeedsItsKeyAndNoOtherHasOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		e    cards.Entity
		ok   bool
	}{
		{"a task with its key", cards.Entity{Kind: wire.ResourceTask, ID: "key", Key: "key", Title: "title"}, true},
		{"a task with none", cards.Entity{Kind: wire.ResourceTask, ID: "key", Title: "title"}, false},
		{"a project with none", cards.Entity{Kind: wire.ResourceProject, ID: "key", Title: "name"}, false},
		{"a seat keyed by its handle", cards.Entity{Kind: wire.ResourceSeat, ID: "handle", Key: "handle", Title: "role"}, false},
		{"a seat with none", cards.Entity{Kind: wire.ResourceSeat, ID: "handle", Title: "role"}, true},
		{"a record of no kind", cards.Entity{ID: "id", Title: "title"}, true},
		{"a room keyed by its name", cards.Entity{Kind: wire.ResourceRoom, ID: "id", Key: "name",
			KeyPrefix: "#", Title: "name", Room: "id", Name: "Direct conversation"}, true},
		{"a room with none", cards.Entity{Kind: wire.ResourceRoom, ID: "id", Title: "name"}, false},
		{"a task headed by a room", cards.Entity{Kind: wire.ResourceTask, ID: "key", Key: "key",
			Title: "title", Room: "id"}, false},
	} {
		e := tc.e
		if err := (cards.Spec{Entity: &e}).Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v", tc.name, err)
		}
	}
}

// AN ADDRESS THE TABLE READS IS ONE IT BUILDS: a record address ScreenOf
// accepts, built again from what it read, is the same address. A chat link's
// preview reads an address with ScreenOf and builds its href with Link, and
// treats a refusal there as the node's own fault, which holds only while the
// two directions agree.
//
// Mutation: give Link a grammar ScreenOf does not share, and an address it
// read is refused.
func TestAnAddressTheTableReadsIsOneItBuilds(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"#/worklet/ENG-12",
		"#/doclet/pages/0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e31",
		"#/artifacts/0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e32"} {
		screen, params, ok := cards.ScreenOf(address)
		if !ok {
			t.Fatalf("%s reads as no record", address)
		}
		link, err := cards.Link(screen, params, "")
		if err != nil {
			t.Errorf("%s read as %s %v, which the table refuses to build: %v",
				address, screen, params, err)
			continue
		}
		if link.Href != address {
			t.Errorf("%s read and built again is %s", address, link.Href)
		}
	}
}

// AN ADDRESS READ BACK IS THE RECORD THE TABLE BUILT IT FOR: every record
// screen's link, read back, names that screen and those parameters, and an
// address naming no record, or a value outside its grammar, names none.
//
// Mutation: drop the grammar check and "#/worklet/ENG" reads as a task.
func TestAnAddressReadsBackAsTheRecordItOpens(t *testing.T) {
	t.Parallel()
	for screen, params := range map[string]map[string]string{
		"item":     {"id": "ENG-12"},
		"page":     {"id": "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e31"},
		"artifact": {"id": "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e32"},
		"project":  {"key": "ENG"},
		"seat":     {"handle": "pm"},
	} {
		link, err := cards.Link(screen, params, "")
		if err != nil {
			t.Fatalf("link %s: %v", screen, err)
		}
		got, read, ok := cards.ScreenOf(link.Href)
		if !ok || got != screen || !maps.Equal(read, params) {
			t.Errorf("%s read back as %q %v (%v), want %s %v", link.Href, got, read, ok,
				screen, params)
		}
	}
	for _, address := range []string{"#/worklet/tasks", "#/worklet/eng-12x", "#/nowhere/1",
		"#/doclet/pages/not-a-uuid"} {
		if screen, _, ok := cards.ScreenOf(address); ok {
			t.Errorf("%s read back as %s, want no record", address, screen)
		}
	}
}
