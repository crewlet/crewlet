package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// EVERY FILTER WITH AN INDEX OF ITS OWN SEEKS IT, rather than walking the whole
// log newest-first until a page fills.
//
// Three of these indexes are PARTIAL (`WHERE x <> ”`, schema/0029, 0033 and
// 0034), and the planner uses a partial index only when the query itself
// states the index's predicate: it cannot prove `x = ?` implies `x <> ”` for a
// value bound after planning. Until the listing stated it, every one of those
// filters read the primary key instead — which, for an item, a unit of work or
// a channel with fewer rows than a page, is every row of the thirty-day window
// on every read. The plan is read back for the EXACT statement [EventLog.List]
// runs, so a filter that stops naming its predicate goes red here.
//
// Mutation: drop the `<> ”` term from [ListQuery.predicate]'s addIndexed and
// the three partial cases name the primary key.
func TestEveryPartiallyIndexedFilterSeeksItsIndex(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, c := range []struct {
		name  string
		q     ListQuery
		index string
	}{
		{"work_key", ListQuery{WorkKey: "wk-1"}, "crewlet_events_work_key_idx"},
		{"work_item", ListQuery{WorkItem: "native:t-1"}, "crewlet_events_work_item_idx"},
		{"channel_id", ListQuery{ChannelID: "ch-1"}, "crewlet_events_channel_idx"},
		{"agent_id", ListQuery{AgentID: "id-sre"}, "crewlet_events_agent_id_time_idx"},
	} {
		query, args := c.q.listSQL(DefaultListLimit, now())
		plan := planOf(t, db, query, args)
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s: the listing's plan is %q — it does not seek %s, so it "+
				"walks the log until a page fills", c.name, plan, c.index)
		}
	}
}

// THE TURN LIST'S FILTERS SEEK THEIR INDEXES TOO, for the listing's reason:
// its unit-of-work filter reads schema/0029's partial index and its work-item
// filter selects turns through schema/0033's, and neither was used until the
// terms named the index's predicate. Read back for the WHERE
// [EventLog.TurnPartials] runs.
//
// Mutation: drop either `<> ”` term from [TurnQuery.turnWhere].
func TestEveryTurnFilterSeeksItsIndex(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, c := range []struct {
		name  string
		q     TurnQuery
		index string
	}{
		{"work_key", TurnQuery{WorkKey: "wk-1"}, "crewlet_events_work_key_idx"},
		{"work_item", TurnQuery{WorkItem: "native:t-1"}, "crewlet_events_work_item_idx"},
	} {
		where, args := c.q.turnWhere(now().Add(-time.Hour), false)
		plan := planOf(t, db, "SELECT turn_id, COUNT(*) FROM crewlet_events WHERE "+
			strings.Join(where, " AND ")+" GROUP BY turn_id", args)
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s: the turn list's plan is %q — it does not seek %s", c.name, plan, c.index)
		}
	}
}

// A LOOKUP BY ID SEEKS THE ID INDEX, and only the id index.
//
// A link carries an id and no time, so [EventLog.ByID] has the id index to
// seek and nothing else — and the planner kept reaching for the primary key
// that `event_time` leads: walking it newest first to satisfy the ORDER BY (the
// whole log for an old id or a dead link), or, once the history floor was a
// term, intersecting the id index with that key's entire thirty-day range
// (`MULTI-INDEX AND`), which costs every lookup the size of the window. Read
// back for the EXACT statement ByID runs, on an empty log as on a full one:
// without statistics, which no node gathers, the plan is the same either way.
//
// Mutation: drop the ORDER BY's unary plus from [byIDSQL] and the plan walks
// the primary key's autoindex; drop both and it intersects that key's floor
// range with the id index (`MULTI-INDEX AND`).
func TestALookupByIDSeeksTheIDIndex(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	query, args := byIDSQL("ev-1", now())
	plan := planOf(t, db, query, args)
	if !strings.Contains(plan, "crewlet_events_id_idx") {
		t.Errorf("ByID's plan is %q — it does not seek crewlet_events_id_idx", plan)
	}
	for _, refused := range []string{"MULTI-INDEX", "sqlite_autoindex_crewlet_events_1"} {
		if strings.Contains(plan, refused) {
			t.Errorf("ByID's plan is %q — it reads %s, so every lookup costs the "+
				"thirty-day window rather than the rows that share the id", plan, refused)
		}
	}
}

// planOf is the planner's account of one statement, one line per step.
func planOf(t *testing.T, db *DB, query string, args []any) string {
	t.Helper()
	rows, err := db.sql.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("read the plan: %v", err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, "\n")
}
