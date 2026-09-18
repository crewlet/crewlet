package tracker_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The two lists the dashboard has to keep its own copy of, held against the
// engine's.
//
// Both are closed sets the engine owns and the dashboard cannot import: it is
// a separate build in a separate language, so `WorkViewShape` and the table's
// sort keys are copies. Every copy in this tree has drifted at least once —
// see `internal/clientsource`'s own doc — and these two drift silently in
// opposite directions, which is why each is checked in both.

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

	body, err := clientsource.Declaration(clientsource.Tree,
		`export type WorkViewShape = ([^;]*);`)
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
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

// AND EVERY COLUMN THE TABLE SORTS AT ITS HEAD IS A KEY THE GRAMMAR TAKES.
//
// `DataGrid` writes a column's key into `sort=`, the tracker screen sends that
// key to `work_items`, and [ParseQuery] REFUSES a sort key it does not know
// rather than dropping it. So a wrong header is not a mis-sorted column: it is
// a refusal on the click, and the whole board goes with it.
//
// One direction only, deliberately. The engine has sort keys the table has no
// column for — `rank` is a board's manual order and `spend` and
// `status_entered` are not on the row at all — and a column for every key
// would be a table nobody asked for. What must never happen is a head that
// names a key the grammar has never heard of.
func TestEveryTableSortKeyIsOneTheGrammarTakes(t *testing.T) {
	t.Parallel()

	body, err := clientsource.Declaration(clientsource.Tree,
		`(?s)const TABLE_SORT_KEYS = \[(.*?)\] as const`)
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the table sorts on nothing at all, so this gate certifies nothing")
	}

	for _, key := range client {
		// THROUGH THE PARSER, not against a copy of its list: what decides
		// whether a header works is `ParseQuery`'s own answer, and a test
		// comparing two slices would pass on a key the parser refuses for
		// some reason other than the list — a bare `f.`, say.
		if _, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
			"container": "workspace", "sort": key,
		}), wednesday, time.UTC); err != nil {
			t.Errorf("the table offers a %q header and the grammar refuses it: %v — "+
				"the click does not mis-sort the column, it refuses the read and "+
				"takes the board down with it", key, err)
		}
	}
}
