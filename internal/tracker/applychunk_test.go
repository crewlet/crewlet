package tracker_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// THE ROWS A COLLECTION PRODUCES DO NOT DEPEND ON HOW MANY STATEMENTS WROTE
// THEM.
//
// Every child collection an applied task carries — watchers, collaborators,
// tags, relations, dependents, checklists, dependency edges and the key
// directory — is written as a MULTI-ROW insert chunked to the estate's
// parameter limit. That limit is a property of the engine and of nothing the
// record says, so it is exactly the input a correct applier's OUTPUT cannot
// see: one row per statement, six rows per statement and the whole collection
// in one must leave byte-identical tables.
//
// It is the guard the conversion to [store.InsertRows] needs and could not get
// anywhere else. Every other case in this package runs at the probed limit,
// where the caps put every collection inside a single statement — so a bind
// that drifted one column at a chunk boundary, or a row template that is only
// valid when it appears once, would pass the whole suite. Here the SAME record
// is applied at five limits and the tables are compared against each other.
//
// Zero is one of them deliberately: an [statelog.ApplyOptions] with no probed
// limit degrades to a row per statement, which is the shape this applier had
// before the conversion and therefore the reference the other four are judged
// against.
func TestACollectionsRowsSurviveEveryChunkWidth(t *testing.T) {
	t.Parallel()

	// The tables every collection in the fixture below lands in, plus the
	// two derived from it — read in a fixed order so two dumps compare.
	// AND THE COUNT EACH ONE OWES, stated here rather than derived from the
	// fixture, because a comparison of five dumps against each other is
	// blind to a fault they all share: a bind swapped in a row template is
	// wrong identically at every width. These numbers are what the fixture
	// below declares, and they are what makes this case able to fail on a
	// defect that is not width-dependent at all.
	tables := []struct {
		name, order string
		want        int
	}{
		{"tracker_watchers", "task_id, handle", 9},
		{"tracker_collaborators", "task_id, handle", tracker.MaxCollaborators},
		{"tracker_task_tags", "task_id, slug", 7},
		// Six waiting_on edges and five linked ones.
		{"tracker_relations", "task_id, other_id, kind", 11},
		{"tracker_task_dependents", "task_id, dependent_id", 7},
		// ONLY THE WAITING_ON HALF reaches the dependency table.
		{"tracker_task_deps", "blocker_id, task_id", 6},
		{"tracker_checklist_items", "task_id, checklist_id, item_id", 12},
		// The current key and the two former ones.
		{"tracker_task_keys", "key", 3},
	}

	// The limits, in rows-per-statement terms for the widest template
	// here: 0 degrades to one row, 4 chunks a two-column collection two
	// rows at a time and a nine-column one at one row, 20 puts six rows of
	// a three-column collection in a statement and 200 covers the whole
	// fixture — with the probed limit last, which is what production runs.
	dumps := map[int][]string{}
	for _, limit := range []int{0, 4, 20, 200, -1} {
		h := newApplyHarness(t)
		if limit >= 0 {
			h.maxVariables = limit
		}
		effective := h.maxVariables
		if _, err := h.apply(
			taskRecord("t-1", tracker.OpCreate, chunkTask("t-1"), nil),
			time.Unix(1_700_000_100, 0).UTC()); err != nil {
			t.Fatalf("limit %d: create: %v", effective, err)
		}
		var lines []string
		for _, table := range tables {
			rows := dumpTable(t, h, table.name, table.order)
			if len(rows) != table.want {
				t.Errorf("at a limit of %d the task left %d rows in %s, want %d",
					effective, len(rows), table.name, table.want)
			}
			lines = append(lines, rows...)
		}
		dumps[effective] = lines

		// TWO SPOT CHECKS ON COLUMNS A BIND SWAP WOULD MOVE, because a
		// row count survives two columns trading places. `muted` is the
		// watcher template's third bind and comes from a set the task
		// carries separately; `one_sided` is the relations template's
		// CASE, which is 1 exactly for the waiting_on edges whose
		// blocker does not list this task back.
		if got := h.value(
			`SELECT COUNT(*) FROM tracker_watchers WHERE muted = 1`); got != 2 {
			t.Errorf("at a limit of %d, %d watchers are muted, want 2",
				effective, got)
		}
		if got := h.value(
			`SELECT COUNT(*) FROM tracker_relations WHERE one_sided = 1`); got != 6 {
			t.Errorf("at a limit of %d, %d relations are one-sided, want 6 — "+
				"the CASE in the row template is not reading the kind it binds",
				effective, got)
		}
	}

	// Every dump against the one-row-per-statement reference.
	reference, held := dumps[0]
	if !held {
		t.Fatal("no dump was taken at the degraded limit")
	}
	if len(reference) == 0 {
		t.Fatal("the fixture produced no child rows at all, so this case " +
			"compares nothing — the collections stopped being applied")
	}
	for limit, lines := range dumps {
		if limit == 0 {
			continue
		}
		if len(lines) != len(reference) {
			t.Fatalf("at a limit of %d the task left %d child rows, and at one "+
				"row per statement it left %d — a chunk boundary is dropping "+
				"or duplicating rows", limit, len(lines), len(reference))
		}
		for i := range lines {
			if lines[i] != reference[i] {
				t.Fatalf("at a limit of %d row %d is\n  %s\nand at one row per "+
					"statement it is\n  %s\n— the bind order does not survive "+
					"the chunk width", limit, i, lines[i], reference[i])
			}
		}
	}
	t.Logf("%d child rows, identical at limits %v", len(reference), keysOf(dumps))
}

// chunkTask is a task carrying every collection the applier explodes, each
// with enough members to cross a chunk boundary at the small limits above.
func chunkTask(id string) tracker.Task {
	task := newTask(id)
	task.FormerKeys = []string{"ENG-0", "OLD-7"}
	task.Watchers = longHandles(9, 20)
	task.Muted = []string{task.Watchers[1], task.Watchers[4]}
	task.Collaborators = longHandles(tracker.MaxCollaborators, 20)
	task.Tags = []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta"}

	at := time.Unix(1_700_000_000, 0).UTC()
	// BOTH RELATION KINDS, because only `waiting_on` reaches the CASE in
	// the relations template and only `waiting_on` writes a dependency
	// edge — a fixture of one kind would exercise one arm of each.
	for i := range 6 {
		task.Relations = append(task.Relations, tracker.Relation{
			Kind: tracker.RelationWaitingOn, Other: fmt.Sprintf("blocker-%02d", i),
			Note: "waits", CreatedBy: "ana", CreatedAt: at,
		})
	}
	for i := range 5 {
		task.Relations = append(task.Relations, tracker.Relation{
			Kind: tracker.RelationLinked, Other: fmt.Sprintf("linked-%02d", i),
			Note: "see also", CreatedBy: "ana", CreatedAt: at,
		})
	}
	for i := range 7 {
		task.Dependents = append(task.Dependents, fmt.Sprintf("dependent-%02d", i))
	}
	for l := range 3 {
		var items []tracker.ChecklistItem
		for i := range 4 {
			items = append(items, tracker.ChecklistItem{
				ID:   fmt.Sprintf("item-%d-%d", l, i),
				Name: fmt.Sprintf("step %d.%d", l, i),
				Done: i%2 == 0,
			})
		}
		task.Checklists = append(task.Checklists, tracker.Checklist{
			ID: fmt.Sprintf("list-%d", l), Name: fmt.Sprintf("list %d", l),
			Items: items,
		})
	}
	return task
}

// dumpTable renders one table as comparable text, every column included.
func dumpTable(t *testing.T, h *applyHarness, table, order string) []string {
	t.Helper()
	var out []string
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(h.t.Context(),
			`SELECT * FROM `+table+` ORDER BY `+order)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			cells := make([]any, len(columns))
			into := make([]any, len(columns))
			for i := range cells {
				into[i] = &cells[i]
			}
			if err := rows.Scan(into...); err != nil {
				return err
			}
			var b strings.Builder
			b.WriteString(table)
			for i, name := range columns {
				fmt.Fprintf(&b, " %s=%v", name, cells[i])
			}
			out = append(out, b.String())
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	return out
}

func keysOf[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
