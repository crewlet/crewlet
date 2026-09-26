package tracker_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The lists the dashboard has to keep its own copy of, held against the
// engine's.
//
// Every one is a closed set the engine owns and the dashboard cannot import:
// it is a separate build in a separate language, so `WorkViewShape`, the
// grid's and the directory's sort keys, the change kinds and the grouping axes
// are copies. Every copy in this tree has drifted at least once — see
// `internal/clientsource`'s own doc — and each drifts silently, so each gate
// below says which direction it holds and why.

// EVERY SHAPE THE ENGINE MINTS HAS A RENDERER, AND EVERY RENDERER HAS A SHAPE.
//
// The strip is built from what [tracker.Views] returns, and the client picks a
// renderer by the row's `type`. A shape the engine mints that the client does
// not name is a TAB THAT DRAWS NOTHING: the strip shows it, a reader clicks it
// and the body is blank, with no error anywhere — the switch simply has no
// arm. That is what would have happened the day `table` landed if this file
// had not come with it.
//
// The other direction is quieter still: a shape the client names and the
// engine refuses is a branch nothing can reach, carrying a renderer somebody
// will maintain for a tab that cannot exist.
func TestEveryViewShapeTheEngineMintsHasARenderer(t *testing.T) {
	t.Parallel()
	engine := tracker.ViewTypeNames()

	client, err := clientsource.Union(clientsource.Tree, "WorkViewShape")
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
// `DataGrid` writes a column's key into `sort=`, the tracker screen sends that
// key to `work_items`, and [ParseQuery] REFUSES a sort key it does not know
// rather than dropping it. So a wrong header is not a mis-sorted column: it is
// a refusal on the click, and the whole board goes with it.
//
// ONE DECLARATION COVERING BOTH COLUMN SETS. The list and the table are one
// grid drawn in two sets of columns, and every sortable column in either is
// keyed to this union — so the list's heads, which could not be sorted at all
// while it was a hand-rolled panel, are held against the grammar by the same
// gate the table's always were. What is NOT held here is the rest of a column
// set: a column that names no sort key names a field of `WorkSummary` through
// its own cell, which is the dashboard's own typed copy of the wire format and
// a compile error when it is wrong. Only the strings that reach the engine
// need a gate, and a sort key is the only one a column mints.
//
// One direction only, deliberately. The engine has sort keys the grid has no
// column for — `rank` is a board's manual order and `spend_tokens` and
// `status_entered` are not on the row at all — and a column for every key
// would be a grid nobody asked for. What must never happen is a head that
// names a key the grammar has never heard of.
func TestEveryGridSortKeyIsOneTheGrammarTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree, "COLUMN_SORT_KEYS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the grid sorts on nothing at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, not against a copy of its list: what decides
		// whether a header works is `ParseQuery`'s own answer, and a test
		// comparing two slices would pass on a key the parser refuses for
		// some reason other than the list — a bare `f.`, say.
		if _, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
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
// Unlike the table's sort keys above, this pair has to match exactly. The
// directory's answer is a PAGE — the engine stops at [MaxProjectsPerAnswer] —
// so the ordering is applied by the engine and the grid is told not to re-sort
// what it was handed; which means the dashboard's list is not a subset of the
// engine's, it is a second copy of it.
//
// Both directions fail silently. A key the directory offers and the engine
// refuses is not a mis-sorted column: [Reader.Projects] refuses the read, the
// route answers `bad_params`, and the frame draws that as the SCREEN being at
// fault with no retry — a whole directory lost to one header. A key the engine
// grew with no header for it is an ordering nobody can reach, and it is the
// half that goes unnoticed for as long as nobody misses it.
func TestTheProjectsDirectorySortsOnExactlyTheOrderingsTheEngineTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree, "PROJECT_SORT_KEYS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the directory sorts on nothing at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, not against a copy of its list — the same
		// reason the table's gate gives: what decides whether a header
		// works is the engine's own answer to the key the click sends.
		q, err := tracker.ParseProjectQuery(tracker.MapParams{"sort": key})
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

	for _, key := range tracker.ProjectSortNames() {
		if !slices.Contains(client, key) {
			t.Errorf("the engine orders projects by %q and the directory has no "+
				"header for it, so nothing can ask for it. The directory names "+
				"%v", key, client)
		}
	}
}

// AND EVERY CHANGE KIND THE ENGINE WRITES HAS A MARK AND A PHRASE.
//
// The item's history draws a kind as a mark and names it in a phrase, and the
// dashboard cannot import [tracker.ChangeKinds] — a separate build in a
// separate language — so it carries its own copy. A kind added in Go and not
// there is a history row drawing the neutral fallback and reading as a bare
// word after somebody's name ("ada watchers"), with no error anywhere; a kind
// named there that the engine never writes is a mark and a sentence nobody can
// reach. Both are silent, which is why this is a test.
func TestEveryChangeKindTheEngineWritesHasAMarkAndAPhrase(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree, "CHANGES")
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

	for _, kind := range tracker.ChangeKinds {
		if !slices.Contains(client, string(kind)) {
			t.Errorf("the engine writes the change kind %q and the dashboard has no "+
				"mark or phrase for it: its history rows draw the neutral fallback "+
				"and read as a bare word after the actor's name. The dashboard "+
				"names %v", kind, client)
		}
	}
	for _, name := range client {
		if !tracker.ChangeKind(name).Valid() {
			t.Errorf("the dashboard draws the change kind %q, which this engine "+
				"never writes — the mark and the sentence are unreachable", name)
		}
	}
}

// AND EVERY GROUPING THE DISPLAY MENU OFFERS IS ONE THE GRAMMAR TAKES — and
// every grouping the grammar takes is offered, or left out on the record.
//
// `GROUP_AXES` is the dashboard's copy of the engine's grouping keys. A value
// there the parser refuses does not mis-draw a board, it refuses the read and
// takes the board down with it; a key the engine takes that the menu never
// lists is an arrangement only a hand-edited URL can reach, which is what
// `project` was until this gate named it. The keys the menu deliberately does
// not offer are listed HERE with their reasons, so a grouping added to the
// grammar has to land in the menu or in this list — and an entry here for a
// key the grammar no longer takes is stale and fails too.
func TestEveryGroupingTheDashboardOffersIsOneTheGrammarTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Literal(clientsource.Tree, "GROUP_AXES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Field(body, "value")
	if len(client) == 0 {
		t.Fatal("the Display menu offers no grouping at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, for the reason the sort gate gives: what
		// decides whether a row works is ParseQuery's own answer.
		if _, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
			"container": "workspace", "group_by": key,
		}), wednesday, time.UTC); err != nil {
			t.Errorf("the Display menu offers group_by=%q and the grammar refuses "+
				"it: %v — the board is not mis-drawn, the read is refused", key, err)
		}
	}

	// WHAT THE MENU LEAVES OUT, each on the record.
	unlisted := map[string]string{
		"routing_unit": "a routing move is the exception the item's own rail names; a board of it is a board of exceptions",
		"parent":       "a tree is what the subtasks panel draws, not a set of columns",
		"due:day":      "a day is the calendar's axis, and that shape is where a due date is read",
		"due:week":     "a week is the timeline's axis",
		"start:week":   "likewise the timeline's",
	}
	for _, key := range tracker.GroupKeys() {
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
		if !slices.Contains(tracker.GroupKeys(), key) {
			t.Errorf("this gate excuses group_by=%q, which the grammar no longer takes", key)
		}
	}
}
