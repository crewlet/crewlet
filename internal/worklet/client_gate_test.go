package worklet_test

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The lists the dashboard keeps its own copy of, held against the engine's.
//
// Each is a closed set the engine owns and the dashboard, a separate build in
// a separate language, cannot import. Each copy drifts silently
// (`internal/clientsource`'s doc), so each gate says which direction it holds
// and why.

// EVERY SHAPE THE ENGINE MINTS HAS A RENDERER, AND EVERY RENDERER HAS A SHAPE.
//
// The strip is built from [worklet.Views] and the client picks a renderer by
// the row's `type`. A shape the client does not name is a TAB THAT DRAWS
// NOTHING, with no error anywhere; a shape the engine refuses is a renderer
// nothing can reach.
func TestEveryViewShapeTheEngineMintsHasARenderer(t *testing.T) {
	t.Parallel()
	engine := worklet.ViewTypeNames()

	client, err := clientsource.Union(clientsource.Tree(t), "WorkViewShape")
	if err != nil {
		t.Fatal(err)
	}
	if len(client) == 0 {
		t.Fatal("the dashboard names no view shapes at all, so this gate certifies nothing")
	}

	for _, name := range engine {
		if !slices.Contains(client, name) {
			t.Errorf("the engine mints the view shape %q and the dashboard has no "+
				"renderer for it: the strip draws the tab, a reader clicks it, and "+
				"the body is blank with no error anywhere. The dashboard names %v",
				name, client)
		}
	}
	for _, name := range client {
		if !slices.Contains(engine, name) {
			t.Errorf("the dashboard renders the view shape %q, which no view can "+
				"carry — the engine refuses it at the write, so the branch is "+
				"unreachable. The engine mints %v", name, engine)
		}
	}
}

// AND EVERY COLUMN THE GRID SORTS AT ITS HEAD IS A KEY THE GRAMMAR TAKES.
//
// `DataGrid` writes a column's key into `sort=` and [ParseQuery] REFUSES a
// sort key it does not know, so a wrong header refuses the read on the click
// and takes the whole board with it. One declaration covers the list's and the
// table's columns; a column with no sort key names a `WorkSummary` field
// through its cell, a compile error when wrong, so a sort key is the only
// string a column sends the engine.
//
// One direction only: the grid deliberately has no column for `rank` (a
// board's manual order) or `spend_tokens` and `status_entered` (not on the
// row).
func TestEveryGridSortKeyIsOneTheGrammarTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree(t), "COLUMN_SORT_KEYS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the grid sorts on nothing at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, not against a copy of its list: comparing
		// slices passes a key the parser refuses for another reason (a
		// bare `f.`, say).
		if _, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
			"container": "workspace", "sort": key,
		}), wednesday, time.UTC); err != nil {
			t.Errorf("the grid offers a %q header and the grammar refuses it: %v — "+
				"the click does not mis-sort the column, it refuses the read and "+
				"takes the board down with it", key, err)
		}
	}
}

// AND THE PROJECTS DIRECTORY'S ORDERINGS ARE THE ENGINE'S, IN BOTH DIRECTIONS.
//
// Unlike the grid's sort keys, these match exactly: the directory's answer is
// a PAGE ([MaxProjectsPerAnswer]) the engine orders and the grid does not
// re-sort. A key the engine refuses loses the whole directory
// ([Reader.Projects] answers `bad_params`, drawn as the screen at fault, with
// no retry); a key the engine grew with no header is an ordering nobody can
// reach. Both are silent.
func TestTheProjectsDirectorySortsOnExactlyTheOrderingsTheEngineTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree(t), "PROJECT_SORT_KEYS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the directory sorts on nothing at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, for the grid gate's reason.
		q, err := worklet.ParseProjectQuery(worklet.MapParams{"sort": key})
		if err != nil {
			t.Errorf("the directory offers a %q header and the engine refuses "+
				"it: %v — the click does not mis-sort the column, it refuses "+
				"the read and takes the directory down with it", key, err)
			continue
		}
		if !q.Sort.Valid() {
			t.Errorf("%q parsed to %q, which is not an ordering", key, q.Sort)
		}
	}

	for _, key := range worklet.ProjectSortNames() {
		if !slices.Contains(client, key) {
			t.Errorf("the engine orders projects by %q and the directory has no "+
				"header for it, so nothing can ask for it. The directory names "+
				"%v", key, client)
		}
	}
}

// AND EVERY CHANGE KIND THE ENGINE WRITES HAS A MARK AND A PHRASE.
//
// The item's history draws a kind as a mark and a phrase from the dashboard's
// copy of [worklet.ChangeKinds]. A kind missing there draws the neutral
// fallback and reads as a bare word after somebody's name ("ada watchers"); a
// kind the engine never writes is unreachable. Both are silent.
func TestEveryChangeKindTheEngineWritesHasAMarkAndAPhrase(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CHANGES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Field(body, "kind")
	marks := clientsource.Field(body, "mark")
	phrases := clientsource.Field(body, "phrase")
	if len(client) == 0 {
		t.Fatal("the dashboard names no change kinds at all, so this gate certifies nothing")
	}
	// A ROW THAT LOST HALF ITS DECLARATION would otherwise pass: the kind is
	// still named and the mark or the phrase silently falls back.
	if len(marks) != len(client) || len(phrases) != len(client) {
		t.Errorf("the dashboard names %d change kinds, %d marks and %d phrases: "+
			"every kind carries both, so a row is missing one of them",
			len(client), len(marks), len(phrases))
	}

	for _, kind := range worklet.ChangeKinds {
		if !slices.Contains(client, string(kind)) {
			t.Errorf("the engine writes the change kind %q and the dashboard has no "+
				"mark or phrase for it: its history rows draw the neutral fallback "+
				"and read as a bare word after the actor's name. The dashboard "+
				"names %v", kind, client)
		}
	}
	for _, name := range client {
		if !worklet.ChangeKind(name).Valid() {
			t.Errorf("the dashboard draws the change kind %q, which this engine "+
				"never writes — the mark and the sentence are unreachable", name)
		}
	}
}

// AND EVERY GROUPING THE DISPLAY MENU OFFERS IS ONE THE GRAMMAR TAKES — and
// every grouping the grammar takes is offered, or left out on the record.
//
// A `GROUP_AXES` value the parser refuses takes the board down with the read;
// a key the menu never lists is reachable only by a hand-edited URL. The keys
// the menu leaves out are listed HERE with their reasons, so a new grouping
// lands in the menu or this list, and a stale entry fails too.
func TestEveryGroupingTheDashboardOffersIsOneTheGrammarTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree(t), "GROUP_AXES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Field(body, "value")
	if len(client) == 0 {
		t.Fatal("the Display menu offers no grouping at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, for the grid gate's reason.
		if _, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
			"container": "workspace", "group_by": key,
		}), wednesday, time.UTC); err != nil {
			t.Errorf("the Display menu offers group_by=%q and the grammar refuses "+
				"it: %v — the board is not mis-drawn, the read is refused", key, err)
		}
	}

	// WHAT THE MENU LEAVES OUT, each on the record.
	unlisted := map[string]string{
		"routing_unit": "a routing move is the exception the item's own rail names; a board of it is a board of exceptions",
		"due:day":      "a day is the calendar's axis, and that shape is where a due date is read",
		"due:week":     "a week is the timeline's axis",
		"start:week":   "likewise the timeline's",
	}
	for _, key := range worklet.GroupKeys() {
		if slices.Contains(client, key) {
			continue
		}
		if _, ok := unlisted[key]; !ok {
			t.Errorf("the grammar takes group_by=%q and the Display menu neither "+
				"offers it nor records why not — an arrangement only a hand-edited "+
				"URL can reach", key)
		}
	}
	for key := range unlisted {
		if !slices.Contains(worklet.GroupKeys(), key) {
			t.Errorf("this gate excuses group_by=%q, which the grammar no longer takes", key)
		}
	}
}

// THE DASHBOARD BOUNDS THE TRACKER'S TEXT WHERE THE ENGINE DOES.
//
// The New task sheet and the Save view and Ask dialogs refuse a value past the
// engine's cap before the press, because the engine refuses rather than cuts
// and a refusal after the press lands on no field. A dashboard cap above the
// engine's lets through what the engine refuses; one below refuses what it
// would take. The Ask dialog's option counts are held the same way, and every
// screen's projects read to the most one listing answers: fewer, and a
// company's later projects are missing from every switcher and picker.
func TestTheDashboardBoundsWorkletTextAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"TASK_TITLE_MAX_BYTES":        worklet.MaxTitle,
		"TASK_BODY_MAX_BYTES":         worklet.MaxBody,
		"VIEW_NAME_MAX_BYTES":         worklet.MaxViewName,
		"PROJECT_FILE_MAX_BYTES":      worklet.MaxFileBytes,
		"DECISION_QUESTION_MAX_BYTES": decision.MaxQuestion,
		"DECISION_OPTION_MAX_BYTES":   decision.MaxOptionLabel,
		"DECISION_MIN_OPTIONS":        decision.MinOptions,
		"DECISION_MAX_OPTIONS":        decision.MaxOptions,
		"PROJECT_PAGE":                worklet.MaxProjectsPerAnswer,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d and the engine at %d — "+
				"change it in contract/work.ts to the engine's figure", name, client, engine)
		}
	}
}

// AND THE DASHBOARD NAMES EVERY CHAT SURFACE AN INFORM CAN, AND NO OTHER.
//
// A structured ask's `inform` reaches the dashboard whole, and its protocol
// type copies the engine's closed set ([decision.InformSurfaces]). Held whole
// and in the engine's order, so a new surface fails here rather than reaching
// a screen as a value the type says cannot occur.
func TestTheDashboardNamesEveryInformSurface(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "InformSurface")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, surface := range decision.InformSurfaces() {
		engine = append(engine, string(surface))
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard names the inform surfaces %v and the engine %v — "+
			"make InformSurface in contract/work.ts the engine's set, in its order",
			client, engine)
	}
}

// THE DASHBOARD KNOWS AN ITEM'S CONVERSATION BY THE KEY THE ENGINE STAMPS.
//
// A hold the engine puts on a conversation names it by its key, and the
// dashboard reads the item it is about off that key: the item page's banner
// and the Inbox row that opens the item. A copy that drifts from
// [worklet.ItemConversation] matches no hold, and the item says nothing.
func TestTheDashboardKnowsAnItemsConversationByTheEnginesKey(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "ITEM_CONVERSATION_PREFIX")
	if err != nil {
		t.Fatal(err)
	}
	const id = "6f1c2e04-6a51-4b8a-9d0e-2c7f3b1a9e55"
	if want := worklet.ItemConversation(id); got+id != want {
		t.Errorf("the dashboard names item %s's conversation %q, the engine %q", id, got+id, want)
	}
}
