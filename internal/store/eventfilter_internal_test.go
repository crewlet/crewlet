package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// EVERY FILTER WITH AN INDEX OF ITS OWN SEEKS IT, rather than walking the whole
// log newest-first until a page fills.
//
// Three indexes are PARTIAL (`WHERE x <> ”`, schema/0029, 0033, 0034), and the
// planner uses one only when the query states its predicate: it cannot prove
// `x = ?` implies `x <> ”` for a value bound after planning. Without it the
// filter reads the primary key, every row of the thirty-day window. The plan is
// read for the EXACT statement [EventLog.List] runs.
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
		{"answered_by", ListQuery{AnsweredBy: "ana"}, "crewlet_events_answered_by_idx"},
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
// terms named the index's predicate. Read back for the EXACT statement
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
		{"origin", TurnQuery{Origin: "chat_message:m-1"}, "SEARCH crewlet_turn_origins"},
		{"work_key from a cursor", TurnQuery{WorkKey: "wk-1",
			Before: &TurnCursor{Start: now().Add(-time.Hour), TurnID: "t-1"}}, "crewlet_events_work_key_idx"},
		{"work_item from a cursor", TurnQuery{WorkItem: "native:t-1",
			Before: &TurnCursor{Start: now().Add(-time.Hour), TurnID: "t-1"}}, "crewlet_events_work_item_idx"},
		// AN MCP TOOL, on the turn's ending tally: the endings are sought
		// by their type over the window, and only their tallies walked.
		{"an MCP server's tool", TurnQuery{Server: "github", Tool: "github_get_issue"},
			"crewlet_events_type_time_idx"},
	} {
		query, args := c.q.partialsSQL(now(), readPage, DefaultTurnPage+1, time.Time{})
		plan := planOf(t, db, query, args)
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s: the turn list's plan is %q — it does not seek %s", c.name, plan, c.index)
		}
	}
}

// EVERY GROUPED READ SEEKS ITS FILTER'S INDEX, never the primary key's floor
// range. A GROUP BY under the history floor can plan on the primary key
// `event_time` leads: the floor as a range read whole, or intersected with the
// filter's index (`MULTI-INDEX AND`). A derived table grouped outside makes the planner seek a
// column's groups (see [EventLog]); the axis's bars group by an expression over
// `event_time` and seek as they are. The plan is read for each read's EXACT
// statement at every bucket width: an index seeks it, the key is never
// intersected, and a WINDOW rides every seek (the planner seeks `IN (…)` types
// alone). Mutation: read [turnTracesSQL], [turnNodesSQL], [ListQuery.facetSQL] or
// [TurnQuery.partialsSQL] from the table not a derived table; force the primary
// key under [HistogramQuery.barsSQL] for one width or the windowed category;
// spell [OutcomeQuery.countSQL]'s types as one `IN` (loses the window) or `OR`.
func TestEveryGroupedReadSeeksItsFiltersIndex(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := now()

	type read struct {
		name  string
		query string
		args  []any
		// index is the one the plan must seek; empty for a read whose
		// filter has none, which is held only to stay off the key's
		// intersection.
		index string
		// window says the read names both edges of a time window, which
		// every seek of the index must carry beside the filter.
		window bool
	}
	var reads []read
	q, a := turnTracesSQL("tn-1", at)
	reads = append(reads, read{"a turn's traces", q, a, "crewlet_events_turn_idx", false})
	q, a = turnNodesSQL("tn-1", at)
	reads = append(reads, read{"a turn's nodes", q, a, "crewlet_events_turn_idx", false})
	q, a = OutcomeQuery{Since: at.Add(-time.Hour), At: at}.countSQL(at)
	reads = append(reads, read{"the notification outcomes", q, a, "crewlet_events_type_time_idx", true})
	failed := true
	for _, c := range []struct {
		name  string
		q     TurnQuery
		kind  turnRead
		index string
	}{
		{"the turn list", TurnQuery{}, readPage, ""},
		{"the turn list by seat", TurnQuery{AgentID: "id-1"}, readPage, "crewlet_events_agent_id_time_idx"},
		{"the turn list by unit of work", TurnQuery{WorkKey: "wk-1"}, readPage, "crewlet_events_work_key_idx"},
		// FROM A CURSOR, whose keyset is a HAVING over the groups and must
		// leave the row read the filter's own.
		{"the turn list from a cursor", TurnQuery{Before: &TurnCursor{Start: at.Add(-time.Hour), TurnID: "tn-1"}},
			readPage, ""},
		{"the turn list by seat from a cursor", TurnQuery{AgentID: "id-1",
			Before: &TurnCursor{Start: at.Add(-time.Hour), TurnID: "tn-1"}}, readPage,
			"crewlet_events_agent_id_time_idx"},
		{"a share of named turns", TurnQuery{IDs: []string{"tn-1", "tn-2"}}, readShare, "crewlet_events_turn_idx"},
		{"the named turns a page lists", TurnQuery{IDs: []string{"tn-1", "tn-2"}, Failed: &failed},
			readListed, "crewlet_events_turn_idx"},
	} {
		q, a := c.q.partialsSQL(at, c.kind, DefaultTurnPage+1, time.Time{})
		reads = append(reads, read{c.name, q, a, c.index, false})
	}
	type filter struct {
		name  string
		q     ListQuery
		index string
	}
	filters := []filter{
		{"unfiltered", ListQuery{}, ""},
		{"type", ListQuery{Type: "turn_completed"}, "crewlet_events_type_time_idx"},
		{"source", ListQuery{Source: "engine"}, "crewlet_events_source_time_idx"},
		{"trace", ListQuery{TraceID: "tr-1"}, "crewlet_events_trace_idx"},
		{"actor", ListQuery{Actor: "PM"}, "crewlet_events_actor_time_idx"},
		{"turn", ListQuery{TurnID: "tn-1"}, "crewlet_events_turn_idx"},
		{"seat", ListQuery{AgentID: "id-1"}, "crewlet_events_agent_id_time_idx"},
		{"unit of work", ListQuery{WorkKey: "wk-1"}, "crewlet_events_work_key_idx"},
		{"work item", ListQuery{WorkItem: "native:t-1"}, "crewlet_events_work_item_idx"},
		{"channel", ListQuery{ChannelID: "ch-1"}, "crewlet_events_channel_idx"},
		{"failed", ListQuery{Failed: new(bool)}, ""},
	}
	for _, c := range filters {
		q, a := c.q.facetSQL("category", at)
		reads = append(reads, read{"the facet count by " + c.name, q, a, c.index, false})
	}
	// THE BARS, AT EVERY WIDTH, each over the widest window Histogram answers
	// at that width — the whole history for an hour or a day, a day of
	// minutes — and through the one function Histogram cuts its window and
	// builds its statement with, so the statement read back is the one that
	// runs, both edges and all. The category's own facet is lifted, so its
	// index is the bars' alone.
	bars := append(slices.Clone(filters), filter{"category", ListQuery{Category: "task"},
		"crewlet_events_category_time_idx"})
	for _, bucket := range EventBuckets {
		for _, c := range bars {
			hq := HistogramQuery{ListQuery: c.q, Bucket: bucket}
			hq.Since = at.Add(-time.Duration(MaxHistogramBuckets-1) * bucket.Step())
			since, until, q, a := hq.barsSQL(at)
			if n := until.Sub(since) / bucket.Step(); n > MaxHistogramBuckets {
				t.Fatalf("the %s bars by %s cover %d buckets, which Histogram refuses "+
					"rather than runs", bucket, c.name, n)
			}
			reads = append(reads, read{fmt.Sprintf("the %s bars by %s", bucket, c.name), q, a, c.index, false})
		}
	}

	for _, r := range reads {
		plan := planOf(t, db, r.query, r.args)
		if r.index != "" && !strings.Contains(plan, r.index) {
			t.Errorf("%s: the plan is %q — it does not seek %s", r.name, plan, r.index)
		}
		if strings.Contains(plan, "MULTI-INDEX") {
			t.Errorf("%s: the plan is %q — it intersects the primary key's floor range, "+
				"so it costs the thirty-day window", r.name, plan)
		}
		if r.index != "" && strings.Contains(plan, "sqlite_autoindex_crewlet_events_1") {
			t.Errorf("%s: the plan is %q — it reads the primary key although the filter "+
				"has an index of its own", r.name, plan)
		}
		if r.window {
			searches := 0
			for _, step := range strings.Split(plan, "\n") {
				if !strings.HasPrefix(step, "SEARCH crewlet_events ") {
					continue
				}
				searches++
				if !strings.Contains(step, "event_time") {
					t.Errorf("%s: the step %q seeks the filter without the window, so it "+
						"reads every row the filter matches in the log", r.name, step)
				}
			}
			if searches == 0 {
				t.Errorf("%s: the plan is %q — it seeks nothing", r.name, plan)
			}
		}
	}
}

// THE CUSTODY READS SEEK THE LOG BY IDENTITY, driven from the few rows they
// name rather than from the log.
//
// Every custody count (outcomes, the axis at every filter, a trace's and a
// turn's extent, a turn's sums) and a node's kept-rows read names a handful of
// rows against a month of log, so each must start from what it names and seek
// the log for every one; started from the log it would read every row. The plan
// is read for the EXACT statements each runs.
// Mutation: join the log first in [unsettledSQL] or [keptSQL], and the plan
// starts from the log — a seek of the count's own index, or a scan of every
// row; drop [keptRowSQL] from [keptSQL], and the kept rows no longer probe the
// in-flight key.
func TestTheCustodyReadsSeekTheLogByIdentity(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := now()
	floor := []any{"ev-1", EncodeTime(at.Add(-EventHistory))}
	reads := []struct {
		name  string
		where []string
		args  []any
	}{
		{name: "the trace's extent", where: []string{"trace_id = ?", "event_time >= ?"}, args: floor},
		{name: "the turn's extent", where: []string{"turn_id = ?", "event_time >= ?"}, args: floor},
	}
	where, args := OutcomeQuery{Since: at.Add(-time.Hour), At: at}.unsettledWhere(at)
	reads = append(reads, struct {
		name  string
		where []string
		args  []any
	}{"the outcome counts", where, args})
	yes := true
	for _, h := range []HistogramQuery{
		{Bucket: BucketHour},
		{Bucket: BucketMinute, ListQuery: ListQuery{Since: at.Add(-time.Hour), Until: at, Category: "agent"}},
		{Bucket: BucketDay, ListQuery: ListQuery{AgentID: "a-1", WorkItem: "native:1", Failed: &yes}},
	} {
		where, args := h.unsettledWhere(at)
		reads = append(reads, struct {
			name  string
			where []string
			args  []any
		}{fmt.Sprintf("the axis by %s", h.Bucket), where, args})
	}
	share := TurnQuery{IDs: []string{"t-1", "t-2"}, AgentID: "a-1", WorkKey: "k-1"}
	where, args = share.turnWhere(at.Add(-EventHistory), at.Add(-EventHistory), readShare)
	reads = append(reads, struct {
		name  string
		where []string
		args  []any
	}{"a turn's share", where, args})

	kept, keptArgs, err := keptSQL([]UnsettledRow{{Time: at, ID: "ev-1"}, {Time: at, ID: "ev-2"}})
	if err != nil {
		t.Fatal(err)
	}
	type statement struct {
		name, query, first string
		args               []any
	}
	statements := []statement{{"the kept rows", kept, "SCAN json_each", keptArgs}}
	for _, r := range reads {
		query, args := unsettledSQL(r.where, r.args)
		statements = append(statements, statement{"the unsettled rows of " + r.name, query,
			"SCAN custody_unsettled", args})
	}
	for _, r := range statements {
		plan := planOf(t, db, r.query, r.args)
		steps := strings.Split(plan, "\n")
		if !strings.HasPrefix(steps[0], r.first) {
			t.Errorf("%s: the plan is %q — it does not start from what it names", r.name, plan)
		}
		for _, step := range steps {
			if strings.HasPrefix(step, "SCAN e") || strings.HasPrefix(step, "SCAN crewlet_events") {
				t.Errorf("%s: the plan is %q — it scans the log", r.name, plan)
			}
		}
		if !strings.Contains(plan, "SEARCH e ") {
			t.Errorf("%s: the plan is %q — it never seeks the log", r.name, plan)
		}
	}

	// THE KEPT ROWS PROBE THE IN-FLIGHT KEY FROM EACH ROW NAMED, never read
	// the in-flight table whole: a coordination outage leaves tens of
	// thousands of identities there, and every fleet read asks this.
	plan := planOf(t, db, kept, keptArgs)
	if !strings.Contains(plan, "SEARCH u USING COVERING INDEX") {
		t.Errorf("the kept rows: the plan is %q — it does not probe the in-flight key "+
			"from the rows named", plan)
	}
	for _, step := range strings.Split(plan, "\n") {
		if strings.HasPrefix(step, "SCAN u") || strings.HasPrefix(step, "SCAN custody_unsettled_rows") {
			t.Errorf("the kept rows: the plan is %q — step %q reads the in-flight table "+
				"whole", plan, step)
		}
	}
}

// A LOOKUP BY ID SEEKS THE ID INDEX, and only the id index.
//
// A link carries an id and no time, so [EventLog.ByID] has only the id index,
// and the planner reaches for the primary key `event_time` leads: walking it to
// satisfy the ORDER BY (the whole log for a dead link), or intersecting the id
// index with its thirty-day floor range (`MULTI-INDEX AND`). The plan is read
// for the EXACT statement on an empty log as a full one: no node gathers
// statistics, so it is the same either way.
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

// A SERVER'S RECENT CALLS SEEK THEIR WINDOW AND PROBE THE BATCHES IN FLIGHT:
// the phase records are sought by their type over the window, newest first
// on the index's own order (so the read can stop early), and each is held to
// the in-flight custody key by its identity, as every count of the usage day
// is (TestEveryUsageCountSeeksItsWindow).
//
// Mutation: spell the type as an `IN` list, or drop the window's upper edge,
// and the seek loses the window; drop the kept-rows predicate, and no step
// probes the batches in flight.
func TestAServersRecentCallsSeekTheirWindow(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := now()
	query, args := toolCallsSQL("github", at.Add(-7*24*time.Hour), at)
	plan := planOf(t, db, query, args)
	sought, probed := false, false
	for _, step := range strings.Split(plan, "\n") {
		switch {
		case scansLog(step), strings.Contains(step, "USE TEMP B-TREE"):
			t.Errorf("the plan is %q: step %q walks or sorts what it should seek", plan, step)
		case strings.HasPrefix(step, "SEARCH e "):
			sought = strings.Contains(step, "crewlet_events_type_time_idx") &&
				strings.Contains(step, "event_time>") && strings.Contains(step, "event_time<")
		case strings.HasPrefix(step, "SEARCH u "):
			probed = strings.Contains(step, "(row_time=? AND row_id=?)")
		}
	}
	if !sought || !probed {
		t.Errorf("the plan is %q: it must seek the window on the type's index (%v) and "+
			"probe the in-flight key by each row's identity (%v)", plan, sought, probed)
	}
}
