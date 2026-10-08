package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY INDEX SERVES A REGISTERED QUERY, AND EVERY REGISTERED QUERY IS SERVED.
//
// # Why this is asserted rather than reviewed
//
// An index is a write cost on every commit, paid on every node, for ever. The
// hottest table here carries twenty-odd of them and the drain figure fourteen
// derived numbers rest on is a function of that count — so an index nobody
// reads is not clutter, it is a measured slowdown with no reader to justify
// it. The inverse costs more: a registered query with no index behind it is a
// full scan of every task in the company, which shows up as a slow board and
// never as a failure.
//
// Reading the DDL cannot establish either. A trailing comment naming a reader
// is a CLAIM, and the planner is the only thing that knows whether the query
// it names actually reaches the index. So this runs EXPLAIN QUERY PLAN over
// the registered query set and reads the answer out.
//
// It is an INTERNAL test deliberately: the registered query set is the
// grammar's own compiler, and going through the public reader would be
// asserting the plan of whatever SQL a caller happened to produce rather than
// of the queries this package registers.
func TestEveryIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := planStore(t)

	// THE TABLES THIS TEST CAN HONESTLY SPEAK FOR: the ones the task
	// query set reads. Every other tracker table's readers are the
	// activity feed, the reports and the knowledge search, which arrive
	// with their own steps and bring their own queries — an index there
	// is claimed by its comment and not by a plan, and pretending
	// otherwise would make this test pass by not looking.
	covered := []string{"tracker_tasks", "tracker_task_tags",
		"tracker_task_deps", "tracker_field_values"}

	used := map[string]bool{}
	for name, params := range registeredQueries() {
		t.Run(name, func(t *testing.T) {
			q, err := ParseQuery(MapParams(params), planNow, time.UTC)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			where, args, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			// THE SAME STATEMENT THE READER RUNS. Explaining the
			// WHERE clause alone would explain a query nobody
			// issues — and the ORDER BY is half of what decides
			// which index the planner reaches for.
			order := orderBy(q, planFields(q))
			plan := explain(t, db, `
				SELECT t.id, t.rank, t.updated_at FROM tracker_tasks t
				WHERE `+where+` ORDER BY `+order+` LIMIT 51`, args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			// A REGISTERED QUERY MUST NOT READ tracker_tasks WITH
			// NO INDEX AT ALL — at any scope. An index SCAN is a
			// legitimate plan when the order is what the query is
			// about and the limit stops it early; a bare table scan
			// is every task in the company read from the heap, and
			// there is no query here for which that is right.
			if scansHeap(plan, "tracker_tasks") {
				t.Errorf("this query reads tracker_tasks with no index:\n%s",
					strings.Join(plan, "\n"))
			}
		})
	}

	// THE DUTIES AND THE APPLY PROBES ARE REGISTERED READERS TOO, and
	// they are run here rather than exempted by a list: an index whose
	// only justification is "a duty uses it" is one nobody has checked,
	// and a duty's own statement is as much a reader as a board is.
	for name, statement := range dutyReads() {
		t.Run(name, func(t *testing.T) {
			for _, index := range indexesIn(explain(t, db, statement.sql, statement.args)) {
				used[index] = true
			}
		})
	}

	for _, index := range indexesOn(t, db, covered) {
		if !used[index] {
			t.Errorf("no registered query's plan reaches %s — an index is a "+
				"write cost on every commit on every node for ever, so one no "+
				"plan uses is deleted rather than kept for a reader somebody "+
				"might add", index)
		}
	}
}

// registeredQueries is the grammar's own coverage set: one entry per filter
// family §6 declares, spelled as a caller spells it.
//
// It is a MAP RATHER THAN A LIST because each case names what it is for, and
// the failure this test reports is "this index has no reader" — which is only
// actionable when the reader set has names.
func registeredQueries() map[string]map[string]any {
	return map[string]map[string]any{
		"the board":         {"container": "project:P01", "sort": "rank"},
		"recently updated":  {"container": "project:P01", "sort": "-updated"},
		"the workspace":     {"sort": "-updated"},
		"my queue":          {"assignee": "ana", "status_group": "active"},
		"the children":      {"container": "project:P01", "parent": "t-00001"},
		"one subtree":       {"container": "project:P01", "root": "t-00001"},
		"by spend":          {"container": "project:P01", "sort": "-spend_tokens"},
		"by estimate":       {"container": "project:P01", "estimate": "gt:30"},
		"by points":         {"container": "project:P01", "points": "gt:1"},
		"one batch":         {"batch": "b-1"},
		"a unit's filing":   {"unit": "eng"},
		"a lead's queue":    {"routing_unit": "eng"},
		"by type":           {"container": "project:P01", "type": "bug"},
		"overdue":           {"container": "project:P01", "due": "lt:today"},
		"starting":          {"container": "project:P01", "start": "gt:today"},
		"finished recently": {"container": "project:P01", "show_closed": "recent:168h"},
		"the trash":         {"container": "project:P01", "removed": "true"},
		"the attention set": {"container": "project:P01", "flag": "cycle"},
		"unarchived":        {"container": "project:P01", "archived": "false"},
		"one key":           {"key": "ENG-1"},
		"several keys":      {"key": "ENG-1,ENG-7"},
		"tagged":            {"container": "project:P01", "tag": "urgent"},
		"blocked":           {"container": "project:P01", "blocked": "true"},
		// ONE REGISTERED QUERY PER TYPED COLUMN, because the four
		// partial indexes on tracker_field_values are one per column and
		// a fixture that only ever filtered on text would leave three of
		// them claimed by nothing. The type each ref resolves to is
		// [planFields]' own, keyed on the ref's name.
		"a text field value":   {"container": "project:P01", "f.owner": "platform"},
		"a number field value": {"container": "project:P01", "f.effort": "gt:5"},
		"a date field value":   {"container": "project:P01", "f.ship": "lt:2031-06-30"},
		"a choice field value": {"container": "project:P01", "f.impact": "o-high"},
		"a field is unset":     {"container": "project:P01", "f.effort": "null"},

		// THE WORKSPACE-SCOPE VARIANTS, which is where a filter has to
		// carry the whole query: inside a project the container seek
		// already narrows to a thirtieth of the corpus and the residual
		// predicate rides along, so an index for that predicate is only
		// ever earned out here.
		"overdue everywhere":      {"due": "lt:today"},
		"starting everywhere":     {"start": "gt:today"},
		"finished everywhere":     {"show_closed": "recent:168h"},
		"flagged everywhere":      {"flag": "cycle"},
		"one status everywhere":   {"status": "in_progress"},
		"unarchived everywhere":   {"archived": "false"},
		"by estimate everywhere":  {"estimate": "gt:30"},
		"by points everywhere":    {"points": "gt:1"},
		"the children everywhere": {"parent": "t-00001"},
		"one subtree everywhere":  {"root": "t-00001"},
		"by spend everywhere":     {"sort": "-spend_tokens"},
	}
}

// dutyReads is every statement outside the query grammar that this schema's
// indexes claim a reader for — the duty selections and the apply-path probes.
//
// THEY ARE PLANNED, NOT DECLARED. An index kept because "a duty uses it" is an
// index nobody has checked: the duty's statement is as much a reader as a
// board's, and the planner is the only thing that knows whether it reaches
// what its comment names.
//
// AND THEY ARE THE STATEMENTS THE DUTY RUNS, never a copy written here. The
// embed duty's entry used to be one — `embed_rev < ? ORDER BY embed_rev`, over
// a column the applier only ever wrote a zero into — and it kept an index on
// that column certified for a selection nothing ran, while the selection the
// duty does run was certified by nothing. The search package exports its own.
func dutyReads() map[string]struct {
	sql  string
	args []any
} {
	read := func(sql string, args []any) struct {
		sql  string
		args []any
	} {
		return struct {
			sql  string
			args []any
		}{sql, args}
	}
	return map[string]struct {
		sql  string
		args []any
	}{
		"the embed duty's selection":    read(search.TaskSelection("m", 8, 1024)),
		"the embed duty's opening read": read(search.TaskOpeningRead("t-00001")),
		"the embed duty's withdrawals":  read(search.TaskWithdrawals(1024)),
		"the embed duty's coverage":     read(search.TaskCoverageCount("m", 8)),
		"the re-spread walk": {
			`SELECT id, rank FROM tracker_tasks
			 WHERE project_key = ? AND length(rank) > 64
			 ORDER BY rank, id LIMIT 64`, []any{"P01"}},
		"the abandoned merge walk": {
			`SELECT id FROM tracker_tasks
			 WHERE merging = 1 AND removed_at IS NULL ORDER BY id LIMIT ?`,
			[]any{64}},
		"the abandoned merge gate": {
			`SELECT EXISTS (SELECT 1 FROM tracker_tasks
			                WHERE merging = 1 AND removed_at IS NULL)`, nil},
		"the abandoned move walk": {
			`SELECT id FROM tracker_tasks
			 WHERE moving = 1 AND removed_at IS NULL ORDER BY id LIMIT ?`,
			[]any{64}},
		"the abandoned move gate": {
			`SELECT EXISTS (SELECT 1 FROM tracker_tasks
			                WHERE moving = 1 AND removed_at IS NULL)`, nil},
		"the apply's duplicate probe": {
			`SELECT 1 FROM tracker_tasks
			 WHERE project_key = ? AND rank = ? AND id <> ?`,
			[]any{"P01", "a000001", "t-00002"}},
		"the subtree restore": {
			// THE PARTIAL PREDICATE IS SPELLED OUT. This engine's
			// planner does not infer `x IS NOT NULL` from `x = ?`,
			// so a read that omits it cannot use the index its own
			// comment names — which is a property of the statement
			// rather than of the schema, and the reason the read is
			// planned here rather than assumed.
			`SELECT id FROM tracker_tasks
			 WHERE removed_with IS NOT NULL AND removed_with = ?`,
			[]any{"t-00001"}},
		"the trash": {
			`SELECT id FROM tracker_tasks WHERE removed_at IS NOT NULL
			 ORDER BY removed_at DESC LIMIT 51`, nil},
		"key resolution": {
			`SELECT id FROM tracker_tasks WHERE key = ?`, []any{"ENG-1"}},
		"the unblocked repair": {
			`SELECT MAX(cleared_at) FROM tracker_task_deps
			 WHERE task_id = ? AND cleared_at IS NOT NULL`, []any{"t-00001"}},
		"one task's blockers": {
			`SELECT blocker_id FROM tracker_task_deps
			 WHERE task_id = ? AND blocker_open = 0`, []any{"t-00001"}},
		"one tag's tasks": {
			`SELECT task_id FROM tracker_task_tags
			 WHERE project_key = ? AND slug = ?`, []any{"P01", "tag-1"}},
	}
}

var planNow = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)

// The corpus the plans are taken against.
//
// # Why it is not four hundred rows
//
// A planner reasons about SELECTIVITY, and at four hundred rows across three
// projects every predicate looks alike: a project seek and a due-date seek
// return the same order of magnitude, so whichever index the cost model
// happens to prefer wins and the answer says nothing about production. At
// twenty thousand rows across thirty projects a project holds a
// thirtieth of the corpus and a due date a tenth of that — which is the shape
// the inventory has to be right for, and the shape that decides whether an
// index earns the write cost it charges on every commit on every node.
const (
	corpusRows = 20_000
	projects   = 30
	tagEvery   = 5
)

// nullableAt gives one row in every n a value, so a partial index is a
// fraction of the table rather than a copy of it.
func nullableAt(i, n int) any {
	if i%n != 0 {
		return nil
	}
	return int64(i) * 1_000_000
}

// planStore is a replicated estate with the tracker's schema and enough task
// rows for the planner to prefer an index.
//
// # Why rows at all
//
// A planner chooses from what it has counted, and a table nobody counted
// leaves it its built-in guesses — which are not production's answer. Measured
// on this engine: against an empty estate, which ANALYZE has nothing to count
// in, every field-value filter seeks ANOTHER column's partial index on
// `field_id` alone (the choice column's, for a text filter), the text and
// number indexes are claimed by nothing, and this test fails on a schema with
// no faults. The fixture is small and its shape is what matters: enough
// distinct values that a seek is cheaper than a scan, and ANALYZE run so the
// planner knows it.
func planStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	return seededPlanStore(t, seedTaskCorpus)
}

// seededPlanStore is an empty replicated estate filled by seed in ONE
// transaction and then ANALYZEd — the frame every plan fixture here shares, so
// each seeds the table its own statements read and nothing else.
//
// seed is handed the estate's own parameter limit, which is what
// [store.InsertRows] chunks to — the multi-row insert every applier writes a
// collection through.
func seededPlanStore(t *testing.T,
	seed func(ctx context.Context, tx *sql.Tx, maxVariables int) error) store.ReplicatedHandle {

	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	defer w.Close()
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		return seed(t.Context(), tx, db.Caps().MaxVariables)
	}); err != nil {
		t.Fatalf("seed the fixture: %v", err)
	}
	// ANALYZE OUTSIDE THE TRANSACTION, on the same pinned connection: it
	// writes the statistics tables the planner reads, and without it the
	// planner reasons from its built-in guesses about a table it has never
	// counted.
	if _, err := w.Conn().ExecContext(t.Context(), `ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	return db
}

// insertAll writes n rows through [store.InsertRows] with no conflict clause.
//
// THE MULTI-ROW INSERT, not a statement per row: the task corpus is 44 000
// rows, and one ExecContext each — a round trip, a parse and a plan per row —
// was two thirds of what this fixture cost under the race detector (29 s
// alone, 11 s through this). What is left is the driver binding each of some
// 570 000 parameters and the engine maintaining the task table's twenty-odd
// indexes, which no statement shape removes. The rows are the same rows
// either way; what the planner is handed is the counted table, and ANALYZE
// counts it after the commit.
func insertAll(ctx context.Context, tx *sql.Tx, maxVariables int,
	prefix, row string, n int, args func(i int) []any) error {

	_, err := store.InsertRows(ctx, tx, maxVariables, prefix, row, "", n, args)
	return err
}

// taskRow is the i-th row of the task corpus, in [insertTask]'s column order.
func taskRow(i int) []any {
	id := fmt.Sprintf("t-%04d", i)
	return []any{
		id, fmt.Sprintf("ENG-%d", i),
		fmt.Sprintf("P%02d", i%projects), "eng", "eng", id,
		[]string{"task", "bug", "epic"}[i%3], "a task",
		[]string{"todo", "in_progress", "done"}[i%3],
		[]string{"not_started", "active", "done"}[i%3],
		fmt.Sprintf("a%06d", i), fmt.Sprintf("h-%d", i%200),
		fmt.Sprintf("b-%d", i%97), i % 480, float64(i % 13), i * 100,
		// SKEWED, because selectivity is the whole question: a
		// tenth of the corpus has a due date and a fiftieth is
		// finished, which is what a company's board looks like
		// and what decides whether a partial index earns its
		// write cost.
		nullableAt(i, 10), nullableAt(i, 10), nullableAt(i, 50),
		0, int64(i), int64(i), []byte(`{}`),
	}
}

// insertTask is the statement [taskRow] fills, as a prefix and one row.
const insertTask = `
	INSERT INTO tracker_tasks
		(id, key, project_key, filed_unit, routing_unit,
		 root_id, type, title, status, status_group,
		 rank, assignee, batch_id, estimate_min, points,
		 spend_tokens, due_at, start_at, finished_at,
		 created_at, updated_at, version, document)
	VALUES`

const taskValues = `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// seedTaskCorpus is the twenty thousand tasks the task query set is planned
// against, and the tags, dependencies and field values every fifth one
// carries.
func seedTaskCorpus(ctx context.Context, tx *sql.Tx, maxVariables int) error {
	if err := insertAll(ctx, tx, maxVariables, insertTask, taskValues,
		corpusRows, taskRow); err != nil {
		return fmt.Errorf("the tasks: %w", err)
	}
	// EVERY FIFTH TASK carries one row in each child table: the k-th
	// tagged task is task k*tagEvery.
	tagged := (corpusRows + tagEvery - 1) / tagEvery
	id := func(k int) string { return fmt.Sprintf("t-%04d", k*tagEvery) }
	for _, child := range []struct {
		what, prefix, row string
		args              func(k int) []any
	}{
		{"tags", `INSERT INTO tracker_task_tags (task_id, project_key, slug) VALUES`,
			`(?,?,?)`, func(k int) []any {
				return []any{id(k), "ENG", fmt.Sprintf("tag-%d", k*tagEvery%11)}
			}},
		{"dependencies", `INSERT INTO tracker_task_deps
			(blocker_id, task_id, blocker_open, cleared_at) VALUES`,
			`(?,?,?,?)`, func(k int) []any {
				i := k * tagEvery
				return []any{fmt.Sprintf("t-%04d", (i+1)%400), id(k), i % 2, int64(i)}
			}},
		// ONE ROW PER DECLARED FIELD, under the ids [planFields]
		// resolves to — a fixture whose only field id is one no
		// registered query names leaves the planner nothing to seek on.
		{"text values", `INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, text) VALUES`,
			`(?,?,?,?,0,?)`, func(k int) []any {
				return []any{id(k), "field-owner", 0, FieldValueNative,
					fmt.Sprintf("v-%d", k*tagEvery%211)}
			}},
		{"number values", `INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, num) VALUES`,
			`(?,?,?,?,0,?)`, func(k int) []any {
				return []any{id(k), "field-effort", 0, FieldValueNative,
					float64(k * tagEvery % 97)}
			}},
		{"date values", `INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, at) VALUES`,
			`(?,?,?,?,0,?)`, func(k int) []any {
				return []any{id(k), "field-ship", 0, FieldValueNative, int64(k * tagEvery)}
			}},
		{"choice values", `INSERT INTO tracker_field_values
			(task_id, field_id, seq, kind, hidden, ref) VALUES`,
			`(?,?,?,?,0,?)`, func(k int) []any {
				return []any{id(k), "field-impact", 0, FieldValueNative,
					fmt.Sprintf("r-%d", k*tagEvery%59)}
			}},
	} {
		if err := insertAll(ctx, tx, maxVariables, child.prefix, child.row,
			tagged, child.args); err != nil {
			return fmt.Errorf("the task %s: %w", child.what, err)
		}
	}
	return nil
}

func explain(t *testing.T, db store.ReplicatedHandle, statement string, args []any) []string {
	t.Helper()
	var plan []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			"EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN:\n%s\n%v", statement, err)
	}
	return plan
}

// indexesIn reads the index names out of a plan.
func indexesIn(plan []string) []string {
	var found []string
	for _, line := range plan {
		_, tail, ok := strings.Cut(line, "USING INDEX ")
		if !ok {
			_, tail, ok = strings.Cut(line, "USING COVERING INDEX ")
		}
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(tail, " ")
		found = append(found, strings.TrimSpace(name))
	}
	return found
}

// scansHeap reports a plan line that reads a whole table with no index.
//
// AN INDEX SCAN IS NOT THIS. "SCAN t USING INDEX x" walks an index in its own
// order and a LIMIT stops it early, which is the right plan for a query whose
// point is the order; "SCAN t" reads the table itself, which is every task in
// the company through the row heap.
func scansHeap(plan []string, table string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN "+table) && !strings.Contains(line, "INDEX") {
			return true
		}
	}
	return false
}

// indexesOn is every index the schema declares on these tables.
func indexesOn(t *testing.T, db store.ReplicatedHandle, tables []string) []string {
	t.Helper()
	var found []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT name, tbl_name FROM sqlite_master WHERE type = 'index'
			 AND sql IS NOT NULL ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, table string
			if err := rows.Scan(&name, &table); err != nil {
				return err
			}
			if slices.Contains(tables, table) {
				found = append(found, name)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the indexes: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the schema declares no index on any covered table, so this " +
			"test is measuring its own reader")
	}
	return found
}

// planFields resolves this test's own field refs without a catalogue.
//
// THE PLAN IS ABOUT THE STATEMENT, not about the declarations: what is under
// test is which index the planner reaches for, and a filter on a text field
// and one on a number field produce the same SHAPE of clause against two
// different columns. The fixture declares the type it means.
// planFieldTypes is what each ref in this fixture is declared as.
//
// A TYPE PER REF, because the value column a filter compares is the field's
// DECLARED type's — so a fixture that made every field text would exercise one
// of the four partial indexes and claim the other three by nothing.
var planFieldTypes = map[string]FieldType{
	"owner":  FieldText,
	"effort": FieldNumber,
	"ship":   FieldDate,
	"impact": FieldDropdown,
}

func planFields(q Query) map[string]resolvedField {
	out := map[string]resolvedField{}
	refs := map[string]bool{}
	collectFieldRefs(q, refs)
	for ref := range refs {
		kind, known := planFieldTypes[ref]
		if !known {
			kind = FieldText
		}
		out[ref] = resolvedField{ID: "field-" + ref, Slug: ref, Type: kind}
	}
	return out
}

// THE CONTAINER SURVIVES THE SUBTASK ROLLUP, and it is the only predicate the
// outer row has to enter on.
//
// The rollup replaces the whole predicate with `root_id IN (<the predicate>)`,
// and it used to drop the container with it — so every container-scoped query
// in the default subtask mode, which is every board, read the outer row with
// nothing but a tombstone every task shares. This asserts the compiled SQL
// rather than the plan because it is the CLAUSE that was missing: a planner
// that happened to pick some other index would hide it.
func TestTheSubtaskRollupKeepsItsContainer(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]map[string]any{
		"the board":          {"container": "project:P01"},
		"the trash":          {"container": "project:P01", "removed": "true"},
		"a filtered board":   {"container": "project:P01", "assignee": "ada"},
		"a searched backlog": {"container": "project:P01", "q": "login"},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := ParseQuery(MapParams(params), planNow, time.UTC)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			where, args, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			outer, _, found := strings.Cut(where, "t.root_id IN (")
			if !found {
				t.Fatalf("this query did not roll subtasks up at all:\n%s", where)
			}
			if !strings.Contains(outer, "t.project_key = ?") {
				t.Errorf("the rollup's OUTER predicate is %q and names no "+
					"container — the row itself is then every task in the "+
					"company, filtered afterwards by the subquery", outer)
			}
			// AND EVERY ARGUMENT IS WHERE ITS PLACEHOLDER IS.
			//
			// Placeholders bind in TEXTUAL order, so a clause added
			// before the subquery whose argument was appended after
			// every other binds the wrong value silently — and a
			// clause-only assertion cannot see it. The outer carries
			// exactly one placeholder, the container's; the subquery's
			// own first placeholder is its copy of the same clause. So
			// the container has to be the first TWO arguments, and a
			// single-argument case would bind correctly whichever end
			// it was added at.
			if outers := strings.Count(outer, "?"); outers != 1 {
				t.Fatalf("the rollup's outer predicate %q carries %d "+
					"placeholders, want the container's one", outer, outers)
			}
			if len(args) < 2 || args[0] != "P01" || args[1] != "P01" {
				t.Errorf("the compiled arguments are %v, want the container "+
					"bound twice at the front — once out here and once inside "+
					"the subquery every other argument moved into", args)
			}
		})
	}
}

// THE COMPANY-WIDE HISTORY READS SEARCH THEIR PARTIAL INDEX.
//
// `work_flow`'s backward walk and `company_feed`'s tracker page select on the
// instant a change took effect and on nothing that narrows them first, over a
// table nothing sweeps. Replicated 0029's `tracker_history_moves_idx` is a
// partial index whose WHERE the two statements state word for word
// ([historyMoves]); a planner that cannot prove the implication falls back to
// a scan of the company's whole history on every landing-screen poll, and
// nothing else would ever say so.
//
// PLANNED AGAINST A COUNTED HISTORY ([historyPlanStore]). It used to be
// planned against the task corpus, which writes no history row at all — so
// the verdict was taken over an empty, uncounted table, the one shape no
// company's history has.
func TestTheFlowAndFeedReadsSearchTheirIndex(t *testing.T) {
	t.Parallel()
	db := historyPlanStore(t)
	// THE WIDEST WINDOW THE FLOW WALKS — [MaxFlowPoints] days back — and a
	// feed page a month down, both inside the history the fixture spans.
	from := planNow.AddDate(0, 0, -MaxFlowPoints)
	before := planNow.AddDate(0, -1, 0)
	statements := map[string]struct {
		sql  string
		args []any
	}{
		"the flow's walk": {flowRowsStatement, []any{store.EncodeTime(from)}},
	}
	for name, q := range map[string]FeedQuery{
		"the newest feed page":   {Limit: 20},
		"a later feed page":      {Limit: 20, Before: &FeedCursor{At: before, Seq: 9}},
		"one writer's hand-offs": {Limit: 20, Actor: "h-7", Kinds: []FeedKind{FeedHandoff}},
	} {
		kinds := q.Kinds
		if len(kinds) == 0 {
			kinds = FeedKinds
		}
		sql, args := companyFeedStatement(q, kinds)
		statements[name] = struct {
			sql  string
			args []any
		}{sql, args}
	}
	for name, statement := range statements {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, statement.sql, statement.args)
			if !slices.Contains(indexesIn(plan), "tracker_history_moves_idx") ||
				scansHeap(plan, "tracker_history") {
				t.Errorf("this read does not search tracker_history_moves_idx:\n%s",
					strings.Join(plan, "\n"))
			}
		})
	}
}

// The history the company-wide reads are planned against.
//
// # Why most of it moves nothing
//
// Whether the planner searches a PARTIAL index rather than scanning the table
// turns on how much of the table the index's predicate keeps, and most commits
// change nothing either reader draws — a title, a tag, a comment, a due date.
// So one commit in [historyMoveEvery] moves a count (a create, a status, an
// assignee, a project, a removal), one in [historyNotATask] is not about a
// task at all, and the rest are the quiet changes that make up a company's
// history. Twenty thousand rows over two years and two thousand tasks is ten
// commits a task, and a [MaxFlowPoints]-day window is an eighth of it.
const (
	historyRows      = 20_000
	historySubjects  = 2_000
	historyMoveEvery = 10
	historyNotATask  = 20
	historyPurged    = 50
)

// historyPlanStore is a replicated estate holding a counted company history,
// the tasks it is about and the deletion markers of the one in
// [historyPurged] that were purged.
//
// THE TASKS AND THE MARKERS ARE THERE FOR THE FEED'S JOINS: its page LEFT
// JOINs both on their primary keys, and a join against an empty table is a
// plan nobody's company runs.
func historyPlanStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	return seededPlanStore(t, seedHistory)
}

// seedHistory is [historyPlanStore]'s seed.
func seedHistory(ctx context.Context, tx *sql.Tx, maxVariables int) error {
	var tasks, purged [][]any
	for s := range historySubjects {
		if s%historyPurged != 0 {
			tasks = append(tasks, taskRow(s))
			continue
		}
		purged = append(purged, []any{
			fmt.Sprintf("t-%04d", s), fmt.Sprintf("ENG-%d", s),
			fmt.Sprintf("P%02d", s%projects), "ana", "human",
			int64(s), historyStream, store.EncodeTime(planNow), []byte(`{}`),
		})
	}
	if err := insertAll(ctx, tx, maxVariables, insertTask, taskValues,
		len(tasks), func(i int) []any { return tasks[i] }); err != nil {
		return fmt.Errorf("the tasks: %w", err)
	}
	if err := insertAll(ctx, tx, maxVariables, `
		INSERT INTO tracker_deletions
			(task_id, task_key, project_key, by, by_kind,
			 committed_seq, log_stream, at, document)
		VALUES`, `(?,?,?,?,?,?,?,?,?)`,
		len(purged), func(i int) []any { return purged[i] }); err != nil {
		return fmt.Errorf("the deletion markers: %w", err)
	}
	if err := insertAll(ctx, tx, maxVariables, `
		INSERT INTO tracker_history
			(id, subject_kind, subject_id, project_key, kind, actor,
			 actor_kind, fields_json, log_seq, log_stream, created_at,
			 effective_at, document)
		VALUES`, `(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		historyRows, historyRow); err != nil {
		return fmt.Errorf("the history: %w", err)
	}
	return nil
}

// historyStream is the log every fixture row names: the domain's own.
var historyStream = Domain{}.Stream().Name

// historyRow is the i-th commit of the fixture's history, oldest first, two
// years of it ending at [planNow].
func historyRow(i int) []any {
	start := planNow.AddDate(-2, 0, 0)
	at := start.Add(planNow.Sub(start) / historyRows * time.Duration(i))
	subject := i % historySubjects
	project := fmt.Sprintf("P%02d", subject%projects)
	subjectKind, subjectID := "task", fmt.Sprintf("t-%04d", subject)

	// THE QUIET CHANGES, which neither reader draws.
	quiet := []struct{ kind, fields string }{
		{"fields", `{"title":{"from":"a task","to":"the task"}}`},
		{"comment", `{}`},
		{"tags", `{"tags":{"from":"a","to":"a,b"}}`},
		{"fields", `{"due":{"from":"","to":"2031-05-01"}}`},
	}
	// AND THE ONES THAT MOVE A COUNT, by each road the predicate admits a
	// row: a kind it names, and a status, assignee or project delta.
	moves := []struct{ kind, fields string }{
		{"created", `{}`},
		{"status", `{"status":{"from":"todo","to":"in_progress"}}`},
		{"status", `{"status":{"from":"in_progress","to":"done"}}`},
		{"assignee", `{"assignee":{"from":"h-1","to":"h-7"}}`},
		{"moved", `{"project":{"from":"P01","to":"P02"}}`},
		{"removed", `{}`},
	}
	change := quiet[i%len(quiet)]
	switch {
	case i%historyNotATask == historyNotATask-1:
		subjectKind, subjectID = "project", project
		change = struct{ kind, fields string }{"project_updated",
			`{"name":{"from":"Platform","to":"Platform team"}}`}
	case i%historyMoveEvery == 0:
		change = moves[i/historyMoveEvery%len(moves)]
	}
	return []any{
		fmt.Sprintf("h-%05d", i), subjectKind, subjectID, project, change.kind,
		fmt.Sprintf("h-%d", i%200), "human", change.fields,
		statelog.Position{Generation: 1, Seq: uint64(i + 1)}.Packed(), historyStream,
		store.EncodeTime(at), store.EncodeTime(at), []byte(`{}`),
	}
}
