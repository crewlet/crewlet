package tracker

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
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
			order := orderBy(q)
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
		"one sprint":        {"container": "project:P01", "sprint": "3"},
		"the children":      {"container": "project:P01", "parent": "t-00001"},
		"one subtree":       {"container": "project:P01", "root": "t-00001"},
		"by spend":          {"container": "project:P01", "sort": "-spend"},
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
		"a field value":     {"container": "project:P01", "f.impact": "high"},

		// THE WORKSPACE-SCOPE VARIANTS, which is where a filter has to
		// carry the whole query: inside a project the container seek
		// already narrows to a thirtieth of the corpus and the residual
		// predicate rides along, so an index for that predicate is only
		// ever earned out here.
		"overdue everywhere":      {"due": "lt:today"},
		"starting everywhere":     {"start": "gt:today"},
		"finished everywhere":     {"show_closed": "recent:168h"},
		"flagged everywhere":      {"flag": "cycle"},
		"one sprint everywhere":   {"sprint": "3"},
		"one status everywhere":   {"status": "in_progress"},
		"unarchived everywhere":   {"archived": "false"},
		"by estimate everywhere":  {"estimate": "gt:30"},
		"by points everywhere":    {"points": "gt:1"},
		"the children everywhere": {"parent": "t-00001"},
		"one subtree everywhere":  {"root": "t-00001"},
		"by spend everywhere":     {"sort": "-spend"},
	}
}

// dutyReads is every statement outside the query grammar that this schema's
// indexes claim a reader for — the duty selections and the apply-path probes.
//
// THEY ARE PLANNED, NOT DECLARED. An index kept because "a duty uses it" is an
// index nobody has checked: the duty's statement is as much a reader as a
// board's, and the planner is the only thing that knows whether it reaches
// what its comment names.
func dutyReads() map[string]struct {
	sql  string
	args []any
} {
	return map[string]struct {
		sql  string
		args []any
	}{
		"the embed duty's selection": {
			`SELECT id FROM tracker_tasks WHERE removed_at IS NULL
			 AND embed_rev < ? ORDER BY embed_rev LIMIT 64`, []any{5}},
		"the re-spread walk": {
			`SELECT id, rank FROM tracker_tasks
			 WHERE project_key = ? AND length(rank) > 64
			 ORDER BY rank, id LIMIT 64`, []any{"P01"}},
		"the abandoned merge walk": {
			`SELECT id FROM tracker_tasks WHERE merging = 1 LIMIT 64`, nil},
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
		"a field's text values": {
			`SELECT task_id FROM tracker_field_values
			 WHERE hidden = 0 AND field_id = ? AND text = ?`, []any{"f-1", "high"}},
		"a field's numeric values": {
			`SELECT task_id FROM tracker_field_values
			 WHERE hidden = 0 AND field_id = ? AND num > ?`, []any{"f-1", 0}},
		"a field's date values": {
			`SELECT task_id FROM tracker_field_values
			 WHERE hidden = 0 AND field_id = ? AND at > ?`, []any{"f-1", 0}},
		"a field's references": {
			`SELECT task_id FROM tracker_field_values
			 WHERE hidden = 0 AND field_id = ? AND ref = ?`, []any{"f-1", "x"}},
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

// planStore is a replicated estate with the tracker's schema and enough rows
// for the planner to prefer an index.
//
// # Why rows at all
//
// SQLite's planner is free to scan a table it believes is tiny, and an empty
// one is the tiniest there is — so a plan taken against no data reports a scan
// for every query and this test would fail on a schema with no faults. The
// fixture is small and its shape is what matters: enough distinct values that
// a seek is cheaper than a scan, and ANALYZE run so the planner knows it.
func planStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	w, err := db.Replicated().Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	defer w.Close()
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range corpusRows {
			id := fmt.Sprintf("t-%04d", i)
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO tracker_tasks
					(id, key, project_key, filed_unit, routing_unit,
					 sprint_number, root_id, type, title, status, status_group,
					 rank, assignee, batch_id, estimate_min, points,
					 spend_tokens, due_at, start_at, finished_at,
					 created_at, updated_at, version, document)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, fmt.Sprintf("ENG-%d", i),
				fmt.Sprintf("P%02d", i%projects), "eng", "eng", i%7, id,
				[]string{"task", "bug", "epic"}[i%3], "a task",
				[]string{"todo", "in_progress", "done"}[i%3],
				[]string{"not_started", "active", "done"}[i%3],
				fmt.Sprintf("a%06d", i), fmt.Sprintf("h-%d", i%200),
				fmt.Sprintf("b-%d", i%97), i%480, float64(i%13), i*100,
				// SKEWED, because selectivity is the whole question: a
				// tenth of the corpus has a due date and a fiftieth is
				// finished, which is what a company's board looks like
				// and what decides whether a partial index earns its
				// write cost.
				nullableAt(i, 10), nullableAt(i, 10), nullableAt(i, 50),
				0, int64(i), int64(i), []byte(`{}`)); err != nil {
				return err
			}
			if i%tagEvery != 0 {
				continue
			}
			for _, statement := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO tracker_task_tags (task_id, project_key, slug)
					VALUES (?,?,?)`,
					[]any{id, "ENG", fmt.Sprintf("tag-%d", i%11)}},
				{`INSERT INTO tracker_task_deps
					(blocker_id, task_id, blocker_open, cleared_at) VALUES (?,?,?,?)`,
					[]any{fmt.Sprintf("t-%04d", (i+1)%400), id, i % 2, int64(i)}},
				{`INSERT INTO tracker_field_values
					(task_id, field_id, seq, kind, num, text, at, ref)
					VALUES (?,?,?,?,?,?,?,?)`,
					[]any{id, "f-1", 0, "text", float64(i % 97),
						fmt.Sprintf("v-%d", i%211), int64(i), fmt.Sprintf("r-%d", i%59)}},
			} {
				if _, err := tx.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
					return err
				}
			}
		}
		return nil
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

func explain(t *testing.T, db *store.DB, statement string, args []any) []string {
	t.Helper()
	var plan []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
func indexesOn(t *testing.T, db *store.DB, tables []string) []string {
	t.Helper()
	var found []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
func planFields(q Query) map[string]resolvedField {
	out := map[string]resolvedField{}
	for _, filter := range q.Fields {
		out[filter.Ref] = resolvedField{
			ID: "field-" + filter.Ref, Slug: filter.Ref, Type: FieldText,
		}
	}
	return out
}
