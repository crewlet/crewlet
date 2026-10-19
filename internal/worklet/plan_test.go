package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/sourcetree"
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
//
// # And the history, whose readers are statements rather than a grammar
//
// `tracker_history` is one row per applied commit and nothing sweeps it, so it
// is the table where a read with no index costs the most and grows for ever —
// and until it was covered here most of its indexes were held by their
// comments alone. One of them, `tracker_history_comment_idx`, had lost its only
// reader with the column that reader read (`actor_seat`, replicated 0041) and
// was maintained on every commit for nobody until replicated 0054 dropped it.
// The table's readers are every statement this package runs that finds
// history rows ([historyReads]), each planned over a company's history, and
// the set is held COMPLETE: every statement in the module's source that reads
// the table is one of them ([statementLiterals]).
func TestEveryIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	history := historyPlanStore(t)

	// THE TABLES THIS TEST CAN HONESTLY SPEAK FOR: the ones the task
	// query set reads, and the history. Every other tracker table's
	// indexes are claimed by their comments and not by a plan, and
	// pretending otherwise would make this test pass by not looking.
	covered := []string{"tracker_tasks", "tracker_task_tags",
		"tracker_task_deps", "tracker_field_values", "tracker_history"}

	// NO REGISTERED READER READS A COVERED TABLE WITH NO INDEX AT ALL —
	// at any scope, a query's own subqueries and a duty's statement
	// included. An index SCAN is a legitimate plan when the order is what
	// the read is about and the limit stops it early; a bare table scan is
	// every row the company has read from the heap, and there is no reader
	// here for which that is right.
	//
	// EVERY COVERED TABLE, AND THE DUTIES TOO, because the inventory below
	// cannot see what this sees: an index somebody DROPS is no longer in
	// the inventory to go unclaimed, so a duty whose index went with it
	// fell back to a heap scan and this test stayed green — measured by
	// dropping tracker_tasks_key_idx, which left key resolution reading
	// every task and passed.
	refuseHeapScans := func(t *testing.T, plan []string) {
		t.Helper()
		for _, table := range covered {
			if scansHeap(plan, table) {
				t.Errorf("this read reads %s with no index:\n%s",
					table, strings.Join(plan, "\n"))
			}
		}
	}

	// THE INDEX OF THE TREE (replicated 0058), spelled rather than taken
	// from [parentIndex], which is what is under test.
	const tree = "tracker_tasks_parent_idx"

	// EVERY STATEMENT THE GATE PLANS, which is what the completeness walks at
	// the end hold the source to.
	var planned []string

	used := map[string]bool{}
	for name, params := range registeredQueries() {
		q := planQuery(t, params)
		// EVERY STATEMENT THE READER RUNS FOR IT ([queryStatements]): the
		// page with every check below, and the count, the totals, the
		// board and the around read held to the ones that apply to any
		// read of the answer's set.
		statements := queryStatements(t, q)
		for _, statement := range statements {
			planned = append(planned, statement.sql)
		}
		t.Run(name, func(t *testing.T) {
			// THE SAME STATEMENT THE READER RUNS ([taskRowsStatement]).
			// Explaining the WHERE clause alone would explain a query
			// nobody issues — and the FROM and the ORDER BY are half of
			// what decides which index the planner reaches for.
			page := statements["the page"]
			plan := explain(t, db, page.sql, page.args)
			for what, statement := range statements {
				tree := explainTree(t, db, statement.sql, statement.args)
				other := planDetails(tree)
				for _, index := range indexesIn(other) {
					used[index] = true
				}
				refuseHeapScans(t, other)
				refuseSetWalks(t, what, tree)
			}
			// A CUSTOM-FIELD FILTER SEEKS ITS OWN COLUMN'S INDEX,
			// which the inventory below cannot see: the four lead
			// with `field_id` alike, so a filter seeking another
			// column's on `field_id` alone reads every value its
			// field holds while the index it should have used is
			// claimed by its neighbour. Measured: the unset filter
			// sought the choice column's index even over the
			// statistics this fixture used to take, and passed,
			// because the number filter claimed the number index.
			// Spelled from the column rather than through
			// [fieldValueIndex], which is what is under test. A
			// FILTER'S, because a sort, a board or a total over a
			// field reads each task's own value by the key instead
			// ([TestAFieldValueIsReadByItsTaskOrByItsFilter]).
			for _, filter := range q.Fields {
				field := planFields(q)[filter.Ref]
				index := "tracker_field_values_" + FieldValueColumn(field.Type) + "_idx"
				if !seeks(plan, index) {
					t.Errorf("this filter on a %s field does not SEEK %s:\n%s",
						field.Type, index, strings.Join(plan, "\n"))
				}
			}
			holdTagFilter(t, plan, q)
			holdPeopleFilter(t, plan, q)
			// ONE ITEM'S SUBTASKS ARE READ THROUGH THE TREE ([taskSource]),
			// because the planner otherwise answers `parent=` from the
			// project's board or the listing's order — measured: a
			// project's whole board sought, and at the workspace every
			// task in the company walked newest first, to keep one
			// parent's children.
			if q.Parent != "" {
				if others, _ := tasksReadOtherwise(plan, tree); len(others) > 0 {
					t.Errorf("this listing of one item's subtasks reads the task "+
						"table other than through %s:\n%s", tree,
						strings.Join(plan, "\n"))
				}
			}
		})
	}

	// THE DUTIES AND THE APPLY PROBES ARE REGISTERED READERS TOO, and
	// they are run here rather than exempted by a list: an index whose
	// only justification is "a duty uses it" is one nobody has checked,
	// and a duty's own statement is as much a reader as a board is.
	//
	// EACH HELD TO THE INDEX IT NAMES, as the history reads below are and
	// for their reason: a read that loses its index walks another one,
	// which is no heap scan and which the inventory cannot see while a
	// neighbour still reaches the index. Measured: the straggler gate,
	// whose comment promises one probe of its partial index, walked every
	// live task as a root through `tracker_tasks_type_idx` on every tick.
	for name, read := range taskReads() {
		planned = append(planned, read.sql)
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, read.sql, read.args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			refuseHeapScans(t, plan)
			holdToIndex(t, plan, read, covered)
		})
	}

	// THE CENSUSES AND THE WHOLE-TABLE PASSES. A census reads every live
	// task in its scope by definition ([taskCensuses]) and is held to no
	// heap scan; a pass reads the whole table by definition
	// ([taskPasses]) and justifies nothing, as the history's passes below
	// do not.
	for name, census := range taskCensuses() {
		planned = append(planned, census.sql)
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, census.sql, census.args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			refuseHeapScans(t, plan)
		})
	}
	for name, pass := range taskPasses() {
		planned = append(planned, pass.sql)
		t.Run(name, func(t *testing.T) {
			if plan := explain(t, db, pass.sql, pass.args); sorts(plan) {
				t.Errorf("this pass sorts the table it reads, holding every "+
					"row before the first is used:\n%s", strings.Join(plan, "\n"))
			}
		})
	}

	// EVERY STATEMENT THAT FINDS HISTORY ROWS, planned over a company's
	// history rather than over the task corpus, because the history is
	// the table each of them is about — and each held to the index it
	// names, which is what makes a dropped index fail here: without it a
	// read falls back to walking another index in that index's order,
	// which is not a heap scan and which the inventory, no longer listing
	// the dropped one, cannot see either. Measured: with the project, the
	// author-kind or the repair's index dropped, every read that needed it
	// fell back to `tracker_history_seq_idx` — the company's whole history
	// walked newest first, or all of it past a position, filtered row by
	// row — and passed.
	reads := historyReads(t, history)
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, history, read.sql, read.args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			refuseHeapScans(t, plan)
			holdToIndex(t, plan, read, covered)
		})
	}

	// THE WHOLE-HISTORY PASSES ARE PLANNED AND JUSTIFY NOTHING. Each
	// reads every task history row by definition, once, when a node's
	// derivation rules change — so a scan of the heap is the honest plan
	// rather than a missing index, and an index one reaches is not one it
	// needs: a pass is never the reason an index is kept.
	//
	// BUT A PASS DOES NOT SORT. A sorter holds every row it selects before
	// it returns the first, and a pass selects the company's whole task
	// history — with the record documents, for the hand-off walk — inside
	// the one transaction a re-derivation runs in. Measured: the hand-off
	// walk ordered `subject_id, log_seq` sorted all of it.
	passes := historyPasses()
	for name, statement := range passes {
		t.Run(name, func(t *testing.T) {
			if plan := explain(t, history, statement.sql, statement.args); sorts(plan) {
				t.Errorf("this pass sorts the history it reads, holding every "+
					"row before the first is used:\n%s", strings.Join(plan, "\n"))
			}
		})
	}

	// AND THE SET IS COMPLETE. A statement that reads the history and is
	// in neither list is a reader this test cannot see, and an index kept
	// for it — or a scan it makes — would pass here unread. Every literal
	// of the module's source that names the table as a source of rows
	// has to be part of a registered statement. An INSERT finds no rows,
	// which is why the history's own is not among them.
	registered := make([]string, 0, len(reads)+len(passes))
	for _, read := range reads {
		registered = append(registered, read.sql)
	}
	for _, statement := range passes {
		registered = append(registered, statement.sql)
	}
	for _, literal := range statementLiterals(t, "tracker_history") {
		if !slices.ContainsFunc(registered, func(statement string) bool {
			return strings.Contains(statement, literal.text)
		}) {
			t.Errorf("this statement in %s reads tracker_history and is "+
				"registered in neither historyReads nor historyPasses, so no plan "+
				"of it is taken — name it, as every other is named, and register "+
				"it:\n%s", literal.file, literal.text)
		}
	}

	// AND THE SAME FOR THE TASK TABLES, against every statement planned
	// here: the grammar's, through the registered queries; the readers
	// outside it, the censuses and the passes — the search package's
	// among them; and the history's, several of which join a task. Until this walk existed most of the task
	// tables' readers were planned by nobody — and among them were the
	// apply's own read of a task's children, which walked the company
	// through the primary key, the catalogue's settle, which read every
	// field value from the heap once per field, and the subtree walk,
	// which read the table from the heap once per level.
	planned = append(planned, registered...)
	for _, literal := range statementLiterals(t, covered[:4]...) {
		if !slices.ContainsFunc(planned, func(statement string) bool {
			return strings.Contains(statement, literal.text)
		}) {
			t.Errorf("this statement in %s reads a task table and no statement "+
				"this test plans holds it, so no plan of it is taken — name it, "+
				"as every other is named (from outside this package, as an "+
				"exported function returning the statement and its arguments), "+
				"and register it in taskReads, taskCensuses or taskPasses, or "+
				"as a query in registeredQueries:\n%s", literal.file, literal.text)
		}
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

// refuseSetWalks fails a plan over a compiled task set that walks a task's
// child table, reads the task table under any alias but the answer's own
// other than by a probe, or reads it into a list.
//
// A WALK OF THE TASK TABLE in an order a LIMIT stops is a plan; a walk of a
// task's tags, dependencies or field values is every such row in the company
// read to answer about some of them, and it is an index scan, so the heap
// check cannot see it. Measured: `tag=all:` walked every tag row through the
// primary key, and passed. NOR THE TASK TABLE UNDER ANY ALIAS BUT THE
// ANSWER'S OWN: `t` is the rows being read, and any other alias is a probe made
// once per row, so a walk there is the company read once per task. Measured:
// `has_children` walked every task through the primary key for each task it
// filtered, and passed.
//
// AND NO READ OF THE TASK TABLE SITS BENEATH A LIST SUBQUERY. This engine plans
// an `IN (SELECT …)` whose subquery stands alone as a LIST SUBQUERY: evaluated
// once, and held whole before the first outer row is read. Over a child table
// that is a filter's own matches — a field's values, a person's rows, a tag's —
// and is the plan those filters are written for; over the task table it is
// every task the subquery selects, however few rows the page then returns.
// Measured: the subtask rollup, written as `root_id IN (SELECT t.id …)`, held
// every live task in the company for the workspace's default listing, a
// project's whole board for a board and the whole trash at either scope, and
// passed every other check here, because its subquery reused the alias `t`.
func refuseSetWalks(t *testing.T, what string, tree []planLine) {
	t.Helper()
	plan := planDetails(tree)
	listed := underList(tree)
	for _, line := range tree {
		for _, table := range []string{"tracker_task_tags", "tracker_task_deps",
			"tracker_field_values"} {
			if strings.HasPrefix(line.detail, "SCAN "+table+" ") ||
				line.detail == "SCAN "+table {
				t.Errorf("%s walks every row of %s:\n%s", what, table,
					strings.Join(plan, "\n"))
			}
		}
		if strings.HasPrefix(line.detail, "SCAN tracker_tasks AS ") &&
			!strings.HasPrefix(line.detail, "SCAN tracker_tasks AS t ") {
			t.Errorf("%s walks the task table once per row it filters:\n%s",
				what, strings.Join(plan, "\n"))
		}
		if readsTaskTable(line.detail) && listed(line) {
			t.Errorf("%s reads the task table into a list it holds whole "+
				"before the first row:\n%s", what, strings.Join(plan, "\n"))
		}
	}
}

// readsTaskTable reports a plan line that reads `tracker_tasks`: a scan names
// the table, and a seek names only its alias, so a seek is the table's when
// the index it seeks is one of the table's.
func readsTaskTable(detail string) bool {
	if strings.HasPrefix(detail, "SCAN tracker_tasks ") || detail == "SCAN tracker_tasks" {
		return true
	}
	if !strings.HasPrefix(detail, "SEARCH ") {
		return false
	}
	for _, index := range indexesIn([]string{detail}) {
		if strings.HasPrefix(index, "tracker_tasks_") ||
			index == "sqlite_autoindex_tracker_tasks_1" {
			return true
		}
	}
	return false
}

// underList reports, for each line of a plan, whether it sits inside an IN
// list's subquery — which this engine plans once for the statement rather
// than once per row, and holds whole.
func underList(tree []planLine) func(planLine) bool {
	byID := map[int]planLine{}
	for _, line := range tree {
		byID[line.id] = line
	}
	return func(line planLine) bool {
		for at := line.parent; at != 0; at = byID[at].parent {
			if strings.HasPrefix(byID[at].detail, "LIST SUBQUERY") {
				return true
			}
		}
		return false
	}
}

// planDetails is a plan tree's lines, in order.
func planDetails(tree []planLine) []string {
	details := make([]string, len(tree))
	for i, line := range tree {
		details[i] = line.detail
	}
	return details
}

// registeredQueries is the grammar's own coverage set: one entry per filter
// family §6 declares, spelled as a caller spells it — and each one that leaves
// the subtask mode to the grammar planned IN BOTH MODES ([inBothModes]).
//
// It is a MAP RATHER THAN A LIST because each case names what it is for, and
// the failure this test reports is "this index has no reader" — which is only
// actionable when the reader set has names.
func registeredQueries() map[string]map[string]any {
	return inBothModes(map[string]map[string]any{
		"the board":         {"container": "project:P01", "sort": "rank"},
		"recently updated":  {"container": "project:P01", "sort": "-updated"},
		"the workspace":     {"sort": "-updated"},
		"my queue":          {"assignee": "ana", "status_group": "active"},
		"the children":      {"container": "project:P01", "parent": "t-00001"},
		"with subtasks":     {"container": "project:P01", "has_children": "true"},
		"one subtree":       {"container": "project:P01", "root": "t-00001"},
		"by spend":          {"container": "project:P01", "sort": "-spend_tokens"},
		"by estimate":       {"container": "project:P01", "estimate": "gt:30"},
		"by points":         {"container": "project:P01", "points": "gt:1"},
		"a unit's filing":   {"unit": "eng"},
		"a lead's queue":    {"routing_unit": "eng"},
		"by type":           {"container": "project:P01", "type": "bug"},
		"overdue":           {"container": "project:P01", "due": "lt:today"},
		"starting":          {"container": "project:P01", "start": "gt:today"},
		"finished recently": {"container": "project:P01", "show_closed": "recent:168h"},
		"the attention set": {"container": "project:P01", "flag": "cycle"},
		"unarchived":        {"container": "project:P01", "archived": "false"},
		"one key":           {"key": "ENG-1"},
		"several keys":      {"key": "ENG-1,ENG-7"},
		"tagged":            {"container": "project:P01", "tag": "urgent"},
		"blocked":           {"container": "project:P01", "blocked": "true"},
		"with dependencies": {"container": "project:P01", "has_dependencies": "true"},
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
		"overdue everywhere":          {"due": "lt:today"},
		"starting everywhere":         {"start": "gt:today"},
		"finished everywhere":         {"show_closed": "recent:168h"},
		"flagged everywhere":          {"flag": "cycle"},
		"one status everywhere":       {"status": "in_progress"},
		"unarchived everywhere":       {"archived": "false"},
		"by estimate everywhere":      {"estimate": "gt:30"},
		"by points everywhere":        {"points": "gt:1"},
		"the children everywhere":     {"parent": "t-00001"},
		"without subtasks everywhere": {"has_children": "false"},
		"one subtree everywhere":      {"root": "t-00001"},
		"by spend everywhere":         {"sort": "-spend_tokens"},

		// EVERY TAG MODE AT BOTH SCOPES, each held to the plan its mode
		// calls for by [holdTagFilter]: `any:` and `all:` seek the tag
		// rows one slug at a time, and `none:` probes each task's own.
		"tagged with either": {"container": "project:P01",
			"tag": "any:tag-1,tag-2"},
		"tagged with both": {"container": "project:P01",
			"tag": "all:tag-1,tag-2"},
		"tagged with neither": {"container": "project:P01",
			"tag": "none:tag-1,tag-2"},
		"tagged everywhere":           {"tag": "tag-1"},
		"tagged with both everywhere": {"tag": "all:tag-1,tag-2"},

		// A PERSON'S WORK, each held by [holdPeopleFilter] to the person's
		// own rows — at the workspace read flat, which is how
		// `list_work_items` reads every filter, and their tasks by key.
		"a collaborator's work": {"collaborator": "h-1", "subtasks": "separate"},
		"two watchers' work":    {"watcher": "h-1,h-2", "subtasks": "separate"},
		"a checklist owner's work": {"checklist_assignee": "h-1",
			"subtasks": "separate"},
		"a watcher's work in a project": {"container": "project:P01", "watcher": "h-1"},

		// THE GRAMMAR'S OTHER SHAPES, each for the statements only it
		// builds ([queryStatements]): a sort by a field and the around
		// read's values, a board of a field and of the tags with one
		// column of each, the work an item blocks, and totals over fields.
		"by a field":             {"container": "project:P01", "sort": "f.impact"},
		"a board of a field":     {"container": "project:P01", "group_by": "f.impact"},
		"a field's one column":   {"container": "project:P01", "group_by": "f.impact", "group": "o-high"},
		"a tag board everywhere": {"group_by": "tag"},
		"one tag's column":       {"container": "project:P01", "group_by": "tag", "group": "tag-1"},
		"blocking everywhere":    {"blocking": "true"},
		"totals of fields": {"container": "project:P01",
			"totals": "f.effort:sum,f.effort:count,f.effort:median,f.ship:max"},

		// THE TRASH AS ITS TAB ASKS FOR IT ([ViewKeyTrash]) — at both
		// scopes, because each is read through an index of its own
		// ([taskSource]). Every statement over it, not only its page, is held
		// to that index by [TestTheTrashIsReadThroughItsOwnIndex].
		"the trash": {"container": "project:P01", "removed": "true",
			"show_closed": "true"},
		"the trash everywhere": {"removed": "true", "show_closed": "true"},
	})
}

// inBothModes is a set of queries with every one that names no subtask mode,
// and is not about one tree, planned a second time with every row filtered on
// its own (`subtasks=separate`), as `<name>, flat`.
//
// BOTH, because the two modes are two different statements and each is a
// reader somebody runs. The grammar's default rolls subtasks up onto their
// root ([compileWhere]) — what a caller of `work_items` that sends no mode
// gets — while every first-party surface overrules it: the dashboard's
// listings and counts, `list_work_items` and a pinned view's count all send
// `separate`. Planned in the default alone, a filter's own index was read only
// through the rollup's probe of each row's root, so the statement every screen
// issues was certified by nothing; measured, with the rollup's root read by
// its key the queue, the unit filing, the routing, the key, the estimate and
// the points indexes had no other reader here. A query about one tree —
// `parent=` or `root=` — is the same statement in either mode, since the
// rollup does not apply to it.
func inBothModes(queries map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, 2*len(queries))
	for name, params := range queries {
		out[name] = params
		_, moded := params["subtasks"]
		_, parent := params["parent"]
		_, root := params["root"]
		if moded || parent || root {
			continue
		}
		flat := make(map[string]any, len(params)+1)
		for key, value := range params {
			flat[key] = value
		}
		flat["subtasks"] = "separate"
		out[name+", flat"] = flat
	}
	return out
}

// taskReads is every statement outside the query grammar that reads a covered
// task table — the duty selections, the apply path's probes and writes, the
// write path's decides, the detail and decoration reads — each with arguments
// of the shape its reader binds and the index its plan must reach
// ([indexedRead]). The whole-table passes are [taskPasses] and the censuses,
// which read every live task in their scope by definition, [taskCensuses].
//
// THEY ARE PLANNED, NOT DECLARED. An index kept because "a duty uses it" is an
// index nobody has checked: the duty's statement is as much a reader as a
// board's, and the planner is the only thing that knows whether it reaches
// what its comment names.
//
// AND THEY ARE THE STATEMENTS THE READERS RUN, never a copy written here. The
// embed duty's entry used to be one — `embed_rev < ? ORDER BY embed_rev`, over
// a column the applier only ever wrote a zero into — and it kept an index on
// that column certified for a selection nothing ran, while the selection the
// duty does run was certified by nothing; the trash and one tag's tasks were
// two more, each a statement no reader issued. Every one is a named value of
// this package (the search package exports its own), and the completeness
// walk ([statementLiterals]) is what stops a reader being written inline where
// no plan of it is taken.
//
// AN UPDATE, A DELETE OR AN INSERT … SELECT IS A READER TOO: it finds its rows
// by its WHERE exactly as a SELECT does.
func taskReads() map[string]indexedRead {
	const (
		key      = "sqlite_autoindex_tracker_tasks_1"
		keys     = "tracker_tasks_key_idx"
		live     = "tracker_tasks_type_idx"
		recent   = "tracker_tasks_updated_idx"
		respread = "tracker_tasks_respread_idx"
		merging  = "tracker_tasks_merging_idx"
		moving   = "tracker_tasks_moving_idx"
		stranded = "tracker_tasks_stranded_idx"
		ranks    = "tracker_tasks_rank_idx"
		tree     = "tracker_tasks_parent_idx"
	)
	seek := func(index, sql string, args ...any) indexedRead {
		return indexedRead{plannedRead{sql, args}, index, false}
	}
	// A WALK HERE IS A GATE'S EXISTS, which stops at the first row; a
	// page whose order is the index's, which its LIMIT stops; or the
	// coverage count, which is every live task by definition.
	walk := func(index, sql string, args ...any) indexedRead {
		return indexedRead{plannedRead{sql, args}, index, true}
	}
	// The search package's statements come with their arguments.
	read := func(sql string, args []any) plannedRead { return plannedRead{sql, args} }
	reads := map[string]indexedRead{
		"the embed duty's selection":    {read(search.TaskSelection("m", 8, 1024)), recent, true},
		"the embed duty's opening read": {read(search.TaskOpeningRead("t-00001")), key, false},
		"the embed duty's withdrawals":  {read(search.TaskWithdrawals(1024)), key, false},
		"the embed duty's coverage":     {read(search.TaskCoverageCount("m", 8)), live, true},
		"the re-spread walk":            seek(respread, respreadRowsStatement, "P01"),
		"the re-spread hand-off":        seek(respread, respreadPendingStatement, "P01", "P01"),
		"the abandoned merge gate":      walk(merging, mergePendingStatement),
		"the abandoned merge walk":      seek(merging, mergingAfterStatement, "", WalkBatch),
		"the abandoned move gate":       walk(moving, movePendingStatement),
		"the abandoned move walk":       walk(moving, movingStatement, WalkBatch),
		"the move stragglers gate":      walk(stranded, strandedPendingStatement),
		"the move stragglers walk":      seek(stranded, strandedAfterStatement, "", WalkBatch),
		"the apply's duplicate probe": seek(ranks, rankDuplicateStatement,
			"P01", "P01", "a000001", "t-00002"),
		// THE RESTORE'S OWN STATEMENT, because whether a partial index may
		// serve a read is a property of the statement rather than of the
		// schema: this entry was a copy that spelled the index's predicate
		// out while the restore did not, so the copy sought the index and
		// the restore read every task.
		"the subtree restore": seek("tracker_tasks_removed_with_idx",
			removedWithStatement, "t-00001"),
		// EVERY READ OF A TASK'S SUBTASKS, each a seek on the parent: the
		// apply's, a move's walk in batches, the subtree a move, a removal
		// and a restore walk — every level of it — and a purge's reach.
		"a task's children":           seek(tree, childrenStatement, "t-00001"),
		"a walk's batch of children":  seek(tree, childBatchStatement, "t-00001", "P01", "", WalkBatch),
		"a subtree, level by level":   seek(tree, subtreeStatement, "t-00001", "t-00001", "t-00001"),
		"what a purge writes, by kin": seek(tree, purgeReachStatement, "t-00001", "t-00001", "t-00001", "t-00001", "t-00001", "t-00001", "t-00001"),
		"key resolution":              seek(keys, resolveTaskStatement, "ENG-1", "ENG-1", "ENG-1"),
		"a point read's scope":        seek(keys, taskReadScopeStatement, "ENG-1", "ENG-1", "ENG-1"),

		// THE LEXICAL INDEX'S WALK OF THE LIVE TASKS, a batch at a time by
		// id from a cursor — a seek of the key, which every partial index
		// on the live tasks would otherwise take from it — and its other
		// reads, and the backlinks', each a batch of tasks by their key.
		"the lexical index's walk": {read(search.TaskVersionsRead("t-00010", 500)), key, false},
		"the lexical index's fetch": {read(search.TaskFetchRead(
			[]string{"t-00001", "t-00002", "t-00003"})), key, false},
		"the lexical index's live check": {read(search.TaskLiveRead(
			[]string{"t-00001", "t-00002", "t-00003"})), key, false},
		"a page's backlinked tasks": {read(search.LinkedTasksRead(
			[]string{"t-00001", "t-00002", "t-00003"})), key, false},
	}
	const (
		task, other = "t-00001", "t-00002"
		person      = "h-7"
		tags        = "sqlite_autoindex_tracker_task_tags_1"
		values      = "sqlite_autoindex_tracker_field_values_1"
		blockers    = "sqlite_autoindex_tracker_task_deps_1"
		dependents  = "tracker_task_deps_task_idx"
		open        = "tracker_task_deps_open_idx"
		board       = "tracker_tasks_board_idx"
		queue       = "tracker_tasks_queue_idx"
	)
	for name, read := range map[string]indexedRead{
		// THE APPLY PATH: a task read and rewritten by its key, its
		// collections cleared by it, and what its commit settles on
		// others — each blocker's state read by the blocker's key, every
		// edge naming it as the blocker by the blocker, a key's holders by
		// the key.
		"an applied task read":         seek(key, readTaskStatement, task),
		"a turn's spend added":         seek(key, turnSpendStatement, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, task),
		"an ancestry stamped":          seek(key, ancestryStampStatement, task, 0, 0, 0, 0, task),
		"a task against its root":      seek(key, rootsProjectStatement, other, task),
		"a task's parent":              seek(key, parentStatement, task),
		"a reopen counted":             seek(key, countReopenStatement, task),
		"a status's entered instant":   seek(key, statusEnteredStatement, 1, task),
		"an unblocked dependent told":  seek(key, unblockedToldStatement, 1, task, 1),
		"another task rewritten":       seek(key, rewriteOtherStatement(movedColumns{parent: true, rank: true}), []byte(`{}`), 1, other, "a1", task, 1),
		"another task's document only": seek(key, rewriteOtherStatement(movedColumns{}), []byte(`{}`), 1, task, 1),
		"a task's values cleared":      seek(values, clearFieldValuesStatement, task),
		"a task's tags cleared":        seek(tags, childClearStatement("tracker_task_tags"), task),
		"a task's edges cleared":       seek(dependents, clearDepsStatement, task),
		"a task's edges written": seek(key, store.InsertStatement(depsInsertPrefix,
			depsInsertRow, depsInsertConflict, 1), other, task, other, other),
		"a blocker's edges settled": seek(blockers, clearDependentsStatement, "done", 1, task),
		"a key's holders flagged":   seek(keys, keyCollisionStatement, "ENG-1"),
		"a key handed on":           seek(keys, handOnKeyStatement, "ENG-1"),
		"a re-derived last change":  seek(key, restampUpdatedStatement, 1, []byte(`{}`), task),
		"a re-derived reopen count": seek(key, setReopensStatement, 1, task),
		// THE WRITE PATH'S DECIDES.
		"a create's guarding row":    seek(key, taskPresentStatement, task),
		"a write's guards":           seek(key, taskGuardsStatement, task, task),
		"a task's project":           seek(key, taskProjectStatement, task),
		"where dependents are filed": seek(key, tasksFiledStatement(3), task, other, "t-00003"),
		"an ancestor mid-move":       seek(moving, movingAwayStatement, task, "P01"),
		"a descendant left behind":   seek(key, uncarriedStatement, task, "P01"),
		// THE RANK DUTY'S PROJECT WALK, which reads a flagged project's
		// live board and probes each card's rank; the next rank above a
		// shared one; and the probe the duty clears behind it.
		"a project's shared ranks":    seek(ranks, duplicateRanksStatement, "P01", WalkBatch),
		"the rank above a shared one": seek(ranks, rankAboveStatement, "P01", "a000001"),
		"a duplicate probe cleared":   seek(ranks, clearDuplicateProbeStatement, "P01", "P01"),
		"a project's lowest rank":     seek(board, lowestRankStatement, "P01"),
		// THE ONE-SIDED REPAIR'S SELECTION, a walk of the partial index
		// that holds exactly the edges it repairs, in its own order — and
		// so not sorted, or every waiting edge is read for each batch.
		"a dependency's missing mirror": walk("tracker_relations_repair_idx",
			oneSidedStatement, 1, WalkBatch),
		// THE DETAIL AND THE DECORATIONS: one task by its key, and a
		// page's rows each by theirs.
		"a task named":               seek(key, taskRefStatement, task),
		"a task's badges":            seek(open, detailFlagsStatement, task, task),
		"a task's document":          seek(key, taskDocumentStatement, task),
		"a task's foreign values":    seek(values, foreignFieldsStatement, task, FieldValueForeign),
		"a task's own links":         seek(key, linksStatement(false), task),
		"the links to a task":        seek(key, linksStatement(true), task),
		"the task a turn list is on": seek(key, turnsTaskStatement, task),
		"a task's flow state":        seek(key, taskStateStatement, task),
		"a page's edges":             seek(dependents, blockersStatement(3), string(RelationWaitingOn), task, other, "t-00003"),
		"what each row blocks":       seek(blockers, dependentsCountStatement(3), task, other, "t-00003"),
		"each row's labels":          seek(tags, rowTagsStatement(3), task, other, "t-00003"),
		"each row's spend":           seek(key, rowSpendStatement(3), task, other, "t-00003"),
		"ranked hits":                seek(key, rankedItemsStatement(3), task, other, "t-00003"),
		"an activity page's keys":    seek(key, taskKeysStatement(3), task, other, "t-00003"),
		"the tasks of runs":          seek(key, turnPlacesStatement(3), "r-1", "r-2", "r-3"),
		// A PERSON'S OWN WORK: the asks put to them, waiting and closed
		// lately, and the checklist items they own, each probing its task
		// by its key.
		"the oldest ask waiting": seek(key, oldestAskStatement, person),
		"the asks waiting":       seek(key, asksPageStatement, person, 21),
		"the asks closed lately": seek(key, answeredStatement, person,
			store.EncodeTime(planNow), MaxAnswered+1),
		"the checklist items owned": seek("tracker_checklist_items_assignee_idx",
			claimsPageStatement, person, 21),
		"the checklist items counted": seek("tracker_checklist_items_assignee_idx",
			cappedCountStatement(claimsFrom, claimsWhere), person, TotalHintCeiling+1),
	} {
		reads[name] = read
	}
	// THE FOUR TASK BLOCKS OF MY WORK, each its page and its count.
	for name, block := range map[string]struct {
		query blockQuery
		index string
	}{
		"assigned":      {assignedBlock(person), queue},
		"collaborating": {collaboratingBlock(person), "tracker_collaborators_handle_idx"},
		"watching":      {watchingBlock(person), "tracker_watchers_handle_idx"},
		"unblocked":     {unblockedBlock(person), queue},
	} {
		set := tasksWhere(block.query.where, block.query.args)
		page, args := taskRowsStatement(set, block.query.terms)
		reads["my work's "+name+" block"] = seek(block.index, page, append(args, 21)...)
		reads["my work's "+name+" count"] = seek(block.index,
			cappedCountStatement(set.from, set.where),
			append(append([]any{}, set.args...), TotalHintCeiling+1)...)
	}
	// A PURGE'S DELETES OF THE COVERED TABLES, each by the purged task.
	for _, statement := range purgeDeletes(task) {
		for _, table := range []struct{ name, index string }{
			{"tracker_task_tags ", tags}, {"tracker_task_deps ", dependents},
			{"tracker_field_values ", values}, {"tracker_tasks ", key},
		} {
			if strings.HasPrefix(statement.sql, "DELETE FROM "+table.name) {
				reads["a purge's delete from "+strings.TrimSpace(table.name)] =
					seek(table.index, statement.sql, statement.args...)
			}
		}
	}
	// A DROP'S TWO SIDES, every way a drop reads them ([gapSideStatement]).
	for _, above := range []bool{false, true} {
		for _, inclusive := range []bool{false, true} {
			reads[fmt.Sprintf("a drop's gap, above=%t inclusive=%t", above,
				inclusive)] = seek(ranks, gapSideStatement(above, inclusive),
				"P01", "t-00002", "a000100", "a000100", "t-00001", RankRespreadInline/2+1)
		}
	}
	// THE CATALOGUE'S SETTLE, for each shape of declaration: each side of
	// the column through the index that holds that side — the field's own
	// value index for the rows it hides, the hidden rows' for the rows it
	// shows again ([settleStatements]).
	for name, field := range map[string]FieldDef{
		"an archived field": {ID: "field-effort", Slug: "effort",
			Type: FieldNumber, Archived: true},
		"an unrestricted field": {ID: "field-effort", Slug: "effort",
			Type: FieldNumber},
		"a field of some types": {ID: "field-effort", Slug: "effort",
			Type: FieldNumber, AppliesTo: []string{"Bug", "epic"}},
	} {
		for _, write := range settleStatements(field) {
			side, index := "shows", "tracker_field_values_hidden_idx"
			if strings.Contains(write.sql, "SET hidden = 1") {
				side, index = "hides", "tracker_field_values_num_idx"
			}
			reads["the settle of "+name+": what it "+side] = seek(index,
				write.sql, write.args...)
		}
	}
	return reads
}

// taskCensuses is every statement that counts the live work in its scope —
// today's census of the flow, its blocked and overdue counts, and the
// company's workload by person — each a function of every live task in the
// scope by definition, at the workspace and inside a project or a team.
//
// AND THE SEARCH PACKAGE'S TWO: the lexical index's count of the live tasks,
// and the window report's read of every live task's text, which an operator
// runs against a backup's copy.
//
// A CENSUS IS PLANNED AND HELD TO NO HEAP SCAN, and to nothing else: its walk
// of the task table is the whole of its scope, which is the question it
// answers, and the sort it ends with is of its groups rather than its rows.
// The indexes it reaches count as read, because it does read them.
func taskCensuses() map[string]plannedRead {
	out := map[string]plannedRead{}
	for scope, project := range map[string]string{"the company": "", "a project": "P01"} {
		census, censusArgs := flowCensusStatement(project)
		out["the flow's census of "+scope] = plannedRead{census, censusArgs}
		now, nowArgs := flowNowStatement(project, planNow)
		out["the flow's blocked and overdue in "+scope] = plannedRead{now, nowArgs}
	}
	for scope, units := range map[string][]string{"the company": nil,
		"two teams": {"eng", "ops"}} {
		statement, args := workloadStatement(units, store.EncodeTime(planNow))
		out["the workload of "+scope] = plannedRead{statement, args}
	}
	// AND THE SEARCH PACKAGE'S: the lexical index's count of the live
	// tasks, and the window report's read of every live task's text.
	statement, args := search.TaskCountRead()
	out["the lexical index's count"] = plannedRead{statement, args}
	statement, args = search.TaskTextRead()
	out["the window report's read of every task"] = plannedRead{statement, args}
	return out
}

// taskPasses is every statement that reads the WHOLE task table: the
// re-derivations [Applier.Rederive] runs when a node's derivation rules differ
// from this build's, each a function of every row by definition.
func taskPasses() map[string]plannedRead {
	return map[string]plannedRead{
		"every reopen count cleared":     {clearReopensStatement, nil},
		"every project's active recount": {activeCountsStatement, []any{string(GroupActive), string(GroupActive)}},
	}
}

// holdTagFilter fails a plan whose tag filter is not read the way its mode
// calls for ([tagClause]).
//
// `any:` and `all:` SEEK the tag rows once per slug — and inside a project on
// the project too, which is what makes a project's tag filter that project's
// tagged work rather than the company's — while `none:`, which no index over
// present rows can drive, probes each task's own rows by the primary key.
// Counted per slug, because a list compiled to `slug IN (…)` still reaches the
// index, seeking the slug column alone and filtering the project out of every
// task in the company that carries it.
func holdTagFilter(t *testing.T, plan []string, q Query) {
	t.Helper()
	if len(q.Tags.Tags) == 0 {
		return
	}
	shown := strings.Join(plan, "\n")
	const slugs, key = "tracker_task_tags_slug_idx", "sqlite_autoindex_tracker_task_tags_1"
	if q.Tags.Mode == TagNone {
		if !seeks(plan, key) {
			t.Errorf("tag=none: does not probe each task's own tag rows:\n%s", shown)
		}
		return
	}
	scoped := q.Scope.Project != ""
	sought := 0
	for _, line := range plan {
		if !seeks([]string{line}, slugs) {
			continue
		}
		sought++
		if !strings.Contains(line, "(slug=?") || scoped != strings.Contains(line, "project_key=?") {
			t.Errorf("tag=%s:%v seeks %s on other than one slug (and, in a "+
				"project, the project):\n%s", q.Tags.Mode, q.Tags.Tags, slugs, shown)
		}
	}
	if sought != len(q.Tags.Tags) {
		t.Errorf("tag=%s:%v seeks %s %d times, want once per slug:\n%s",
			q.Tags.Mode, q.Tags.Tags, slugs, sought, shown)
	}
}

// holdPeopleFilter fails a plan whose filter on a person — `collaborator=`,
// `watcher=`, `checklist_assignee=` — is not read from the person's own rows
// ([peopleClause]): a seek of the person's index, and then each of their tasks
// by its key, with the task table read no other way. Measured: written as a
// probe of each task, the filter walked every live task in the company and
// sought the person's index for each — which seeks the index, so a check of
// the seek alone passed it.
//
// THE TASK TABLE'S RULE HOLDS A FLAT LISTING AT THE WORKSPACE, which is where
// the probe it replaced read every live task in the company. Inside a project
// the planner may page the project's board in its own order and keep the
// person's tasks, which is bounded by the board; and rolled up, the filter is
// asked of each row's root by the root's key while the outer statement reads
// the listing in its own order ([compileWhere]) — a read of the rollup's
// rather than of the filter's. The person's index is sought in all of them.
func holdPeopleFilter(t *testing.T, plan []string, q Query) {
	t.Helper()
	shown := strings.Join(plan, "\n")
	for _, filter := range []struct {
		named int
		index string
	}{
		{len(q.Collaborator), "tracker_collaborators_handle_idx"},
		{len(q.Watcher), "tracker_watchers_handle_idx"},
		{len(q.ChecklistAssignee), "tracker_checklist_items_assignee_idx"},
	} {
		if filter.named == 0 {
			continue
		}
		if !seeks(plan, filter.index) {
			t.Errorf("this filter on a person does not seek %s:\n%s", filter.index, shown)
		}
		others, _ := tasksReadOtherwise(plan, "sqlite_autoindex_tracker_tasks_1")
		if q.Scope.Project == "" && q.Subtasks == SubtasksSeparate && len(others) > 0 {
			t.Errorf("this filter on a person reads the task table other than "+
				"by the key of each task the person is on:\n%s", shown)
		}
	}
}

// plannedRead is one statement the gate explains, with arguments of the
// shape its reader binds.
type plannedRead struct {
	sql  string
	args []any
}

// indexedRead is one statement and the index its plan must reach: by a SEEK on
// the index's leading columns, or — for a read whose point is the index's own
// order, which its LIMIT or its EXISTS stops after a page — by a WALK of it.
type indexedRead struct {
	plannedRead
	index string
	walk  bool
}

// holdToIndex fails a plan that does not reach read's index the way read
// declares, or that walks an index of a covered table it does not name.
func holdToIndex(t *testing.T, plan []string, read indexedRead, covered []string) {
	t.Helper()
	shown := strings.Join(plan, "\n")
	// AND IT WALKS NOTHING IT DOES NOT NAME. A walk is every row of its
	// table in its index's order, which is a heap scan by another route,
	// and the one walk a read may make is the one its own order is for.
	// Measured: the unblocked repair's dependents, written as a three-way
	// join, walked every live task through `tracker_tasks_type_idx` on
	// every tick and sought the history only per task — reaching the
	// subject index it then named, and passing.
	for _, line := range plan {
		for _, table := range covered {
			walked := indexesIn([]string{line})
			if !strings.HasPrefix(line, "SCAN "+table+" ") || len(walked) == 0 ||
				(read.walk && walked[0] == read.index) {
				continue
			}
			t.Errorf("this read walks %s through %s, which it does not "+
				"name — every row of that table, in that index's "+
				"order:\n%s", table, walked[0], shown)
		}
	}
	switch {
	case !read.walk && !seeks(plan, read.index):
		t.Errorf("this read does not SEEK %s:\n%s", read.index, shown)
	case read.walk && !walks(plan, read.index):
		t.Errorf("this read does not walk %s in its order:\n%s",
			read.index, shown)
	case read.walk && sorts(plan):
		// A WALK IS RIGHT ONLY WHILE ITS ORDER IS THE ANSWER'S, because
		// then the LIMIT stops it after a page; sorted, it reads every
		// row it selects first.
		t.Errorf("this read walks %s and then sorts what it read, so "+
			"the walk is every row it selects rather than a page:\n%s",
			read.index, shown)
	}
}

// historyKey is the history's primary key as this engine's planner names it:
// a read by a row's own id seeks it.
const historyKey = "sqlite_autoindex_tracker_history_1"

// historyReads is every statement this package runs that finds rows of
// `tracker_history` by what it asks — the activity feed in each of its filter
// families, a task's own history, the company-wide flow and feed, the inbox's
// join, what resolved an ask and what its asker did next, the unblocked repair,
// and the applier's own probes and rewrites — each with arguments of the shape
// its reader binds and the index it reaches. The ones that read the whole
// table are [historyPasses].
//
// THE STATEMENTS ARE THE READERS' OWN, never a copy: each is a named value of
// this package, and the gate's completeness walk ([statementLiterals]) is what
// stops one being written inline where no plan of it is taken.
//
// AN UPDATE OR A DELETE IS A READER TOO. It finds its rows by its WHERE exactly
// as a SELECT does, and the late record's raise and the purge's rewrites run
// against a table that grows for the life of the company.
//
// db is the history fixture: the activity feed resolves a task the way its
// reader does, inside a transaction, before it has a statement to explain.
func historyReads(t *testing.T, db store.ReplicatedHandle) map[string]indexedRead {
	t.Helper()
	const (
		subject = "tracker_history_subject_idx"
		status  = "tracker_history_status_seq_idx"
		moves   = "tracker_history_moves_idx"
	)
	seek := func(index, sql string, args ...any) indexedRead {
		return indexedRead{plannedRead{sql, args}, index, false}
	}
	at := func(n uint64) int64 {
		return statelog.Position{Stream: historyStream, Generation: 1, Seq: n}.Packed()
	}
	instant := store.EncodeTime(planNow)
	const task, purged, record = "t-0007", "t-0050", "h-00007"
	kind := string(KindTask)
	reads := map[string]indexedRead{
		"a task's own history": seek(subject, taskHistoryStatement, task, DetailHistoryDefault),
		"a purged task's rows": seek(subject, purgedHistoryStatement, purged),
		"one change's routing": seek(historyKey, routingRecordStatement, record),
		"where a change sits":  seek(historyKey, recordPositionStatement, record),
		"a person's inbox": seek(historyKey, inboxStatement([]string{"n.recipient = ?"}),
			"ana", 51),
		"the repair's horizon": seek(status, unblockHorizonStatement, at(9_000), at(9_000)),
		"the repair's dependents": seek(status, unblockedDependentsStatement,
			at(9_000), at(9_500), 64),
		"a subject's effective instant": seek(subject, effectiveAtStatement,
			task, at(9_000), task, at(9_000), instant),
		"a late record raises its successors": seek(subject, raiseHistoryStatement,
			instant, task, at(9_000), instant),
		"a task's newest status": seek(subject, newestStatusStatement, task),
		"an asker's next record after an ask closed": seek(subject, followUpStatement,
			task, "ana", record),
		"what resolved an ask": seek(subject, resolvedStatement,
			task, string(ChangeCommentResolved), record),
		"the hand-offs before a commit": seek(subject, handOffsBeforeStatement,
			task, kind, at(9_000)),
		"a task's hand-offs from a commit on": seek(subject, handOffsFromStatement,
			task, kind, at(9_000)),
		"a hand-off count written": seek(historyKey, writeHandOffStatement, 1, record),
		"a purge reads whose inbox it empties": seek(subject, forgottenNoticesStatement,
			purged, kind, record),
		"a purge empties those inboxes": seek(subject, forgetNoticesStatement,
			purged, kind, record),
		"a purge reads what the census keeps": seek(subject, censusRowsStatement,
			purged, kind, record),
		"a purge deletes what the census does not walk": seek(subject, forgetHistoryStatement,
			purged, kind, record),
		"a purge empties a kept row": seek(historyKey, forgetSkeletonStatement,
			[]byte(`{}`), `{}`, record),
	}
	one, oneArgs := restampStatement(task)
	reads["one task's last change"] = seek(subject, one, oneArgs...)
	for name, read := range companyWideReads() {
		// AN UNBOUNDED PAGE WALKS the index newest first and stops after
		// it; a bounded one seeks its range.
		reads[name] = indexedRead{read.plannedRead, moves, !read.bounded}
	}

	// THE ACTIVITY FEED ([activityCases]), compiled by its own compiler. A
	// CASE THE READER WOULD REFUSE IS NOT A READER, so each passes the
	// reader's own checks first ([activityRead]).
	activity := activityCases()
	for name, c := range activity {
		statement, args := activityRead(t, db, c.q)
		reads["the activity feed: "+name] = indexedRead{plannedRead{statement, args},
			c.index, c.walk}
	}
	return reads
}

// activityCase is one family of the activity feed's filters, spelled as its
// surfaces spell an [ActivityQuery], and the index its plan must reach.
type activityCase struct {
	q     ActivityQuery
	index string
	walk  bool
}

// activityCases is the activity feed, one case per filter family the reader
// accepts. A filter no history index leads with — a change kind, a writer, an
// assignee, the announcement flag, a wall-clock window — is a residual on the
// walk of the feed's own order.
func activityCases() map[string]activityCase {
	const (
		subject = "tracker_history_subject_idx"
		project = "tracker_history_project_idx"
		seq     = "tracker_history_seq_idx"
		writers = "tracker_history_actor_kind_seq_idx"
	)
	const task = "t-0007"
	position := statelog.Position{Stream: historyStream, Generation: 1, Seq: 9_000}
	yes := true
	window := planNow.AddDate(0, 0, -7)
	return map[string]activityCase{
		"one task's activity":                {ActivityQuery{Task: task}, subject, false},
		"one task's activity in its project": {ActivityQuery{Task: task, Project: "P07"}, subject, false},
		"a project's activity":               {ActivityQuery{Project: "P01"}, project, false},
		"a project's activity since":         {ActivityQuery{Project: "P01", Since: position}, project, false},
		"one kind in a project": {ActivityQuery{Project: "P01",
			Kinds: []ChangeKind{ChangeAssignee}}, project, false},
		"a project's text": {ActivityQuery{Project: "P01", Q: "login", SinceAt: window},
			project, false},
		"a task's text":                {ActivityQuery{Task: task, Q: "login"}, subject, false},
		"the company's activity":       {ActivityQuery{Workspace: true}, seq, true},
		"the company's activity since": {ActivityQuery{Workspace: true, Since: position}, seq, false},
		"a later page of the company's": {ActivityQuery{Workspace: true,
			Cursor: position.String()}, seq, false},
		"the audit": {ActivityQuery{Workspace: true, From: window, To: planNow,
			ActorKinds: []AuthorKind{AuthorOperator, AuthorHuman}}, writers, false},
		"one author kind": {ActivityQuery{Workspace: true,
			ActorKinds: []AuthorKind{AuthorAgent}}, writers, false},
		"one change kind": {ActivityQuery{Workspace: true,
			Kinds: []ChangeKind{ChangeStatus}}, seq, true},
		"one writer":          {ActivityQuery{Workspace: true, Actor: "h-7"}, seq, true},
		"the work one holds":  {ActivityQuery{Workspace: true, Assignee: "h-7"}, seq, true},
		"what was announced":  {ActivityQuery{Workspace: true, Notified: &yes}, seq, true},
		"a wall-clock window": {ActivityQuery{Workspace: true, From: window, To: planNow}, seq, true},
	}
}

// A FEED READ IS ITS INDEX IN THE FEED'S OWN ORDER: no sort, no intersection.
//
// The feed is newest first and paged by a LIMIT, so a read whose index is in
// that order stops after a page and one that sorts reads everything it selects
// before the first row. [TestEveryIndexServesARegisteredQuery] holds each case
// to the index it names, which an INTERSECTION satisfies too — this engine
// plans two equalities on two indexes as `MULTI-INDEX AND`, a seek on each —
// so it passed the task-plus-project feed while that read the container's
// whole history to intersect with the task's, sorted the result, and hid the
// task's rows from before it moved. Every case here is planned as the reader
// issues it and held to neither; the audit's several kinds, whose one sort is
// the merge of a page per kind, are [TestTheAuditReadsOnePageOfEachKind]'s.
func TestAFeedReadIsItsIndexInTheFeedsOrder(t *testing.T) {
	t.Parallel()
	db := historyPlanStore(t)
	for name, c := range activityCases() {
		if len(slices.Compact(slices.Sorted(slices.Values(c.q.ActorKinds)))) > 1 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			statement, args := activityRead(t, db, c.q)
			plan := explain(t, db, statement, args)
			shown := strings.Join(plan, "\n")
			if sorts(plan) {
				t.Errorf("this feed read sorts what it selected rather than "+
					"reading %s in the feed's order:\n%s", c.index, shown)
			}
			for _, line := range plan {
				if strings.HasPrefix(line, "MULTI-INDEX AND ") {
					t.Errorf("this feed read intersects two indexes' rows "+
						"rather than reading one:\n%s", shown)
				}
			}
		})
	}
}

// activityRead is the statement and arguments the feed's reader issues for q,
// compiled by its own compiler inside a read — the way [readActivity] resolves
// a task before it has a statement — after the reader's own checks, since a
// query the reader would refuse is not a reader.
func activityRead(t *testing.T, db store.ReplicatedHandle, q ActivityQuery) (string, []any) {
	t.Helper()
	if err := activityFilters(q); err != nil {
		t.Fatalf("the reader refuses %+v: %v", q, err)
	}
	if err := gateActivityQuery(q, planNow); err != nil {
		t.Fatalf("the reader refuses %+v: %v", q, err)
	}
	var statement string
	var args []any
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		predicate, err := compileActivity(t.Context(), tx, q)
		if err != nil {
			return err
		}
		statement, args = activityStatement(predicate, MaxActivityRows+1)
		return nil
	}); err != nil {
		t.Fatalf("compile %+v: %v", q, err)
	}
	return statement, args
}

// THE AUDIT READS ONE PAGE OF EACH AUTHOR KIND, AND SORTS NOTHING ELSE.
//
// `actor_kind IN (…)` sought each kind and then sorted every commit the seeks
// found — for the audit screen's `operator,human`, every commit any person or
// token had ever made, on every load — while the comment above it claimed the
// order came out of the index. One kind seeks `(actor_kind, log_seq DESC)` in
// the feed's own order and needs no sort at all; several are one such arm each
// ([activityStatement]), and the one sort left is the merge of the arms, at the
// top of the statement, over at most one page per kind. So each case here is
// held to one seek of the index per DISTINCT kind and to no sort below the top
// — a sort inside an arm, or an arm that is not a seek, is the old plan back.
func TestTheAuditReadsOnePageOfEachKind(t *testing.T) {
	t.Parallel()
	db := historyPlanStore(t)
	const writers = "tracker_history_actor_kind_seq_idx"
	window := planNow.AddDate(0, 0, -7)
	position := statelog.Position{Stream: historyStream, Generation: 1, Seq: 9_000}
	people := []AuthorKind{AuthorOperator, AuthorHuman}
	for name, q := range map[string]ActivityQuery{
		"the audit screen":          {Workspace: true, ActorKinds: people},
		"the audit over a week":     {Workspace: true, ActorKinds: people, From: window, To: planNow},
		"a later page of the audit": {Workspace: true, ActorKinds: people, Cursor: position.String()},
		"every author kind":         {Workspace: true, ActorKinds: AuthorKinds},
		"one author kind":           {Workspace: true, ActorKinds: []AuthorKind{AuthorAgent}},
		"one kind named twice": {Workspace: true,
			ActorKinds: []AuthorKind{AuthorOperator, AuthorOperator}},
	} {
		t.Run(name, func(t *testing.T) {
			statement, args := activityRead(t, db, q)
			plan := explainTree(t, db, statement, args)
			details := make([]string, len(plan))
			for i, line := range plan {
				details[i] = line.detail
			}
			shown := strings.Join(details, "\n")
			kinds := len(slices.Compact(slices.Sorted(slices.Values(q.ActorKinds))))
			sought := 0
			for _, line := range plan {
				if seeks([]string{line.detail}, writers) {
					sought++
				}
				if !strings.Contains(line.detail, "FOR ORDER BY") {
					continue
				}
				switch {
				case kinds == 1:
					t.Errorf("one author kind sorts what it read, where the "+
						"index is in the feed's order:\n%s", shown)
				case line.parent != 0:
					t.Errorf("an arm sorts what it read rather than reading "+
						"one kind's range in its order:\n%s", shown)
				}
			}
			if sought != kinds {
				t.Errorf("the audit of %d author kind(s) seeks %s %d times, "+
					"want once per kind:\n%s", kinds, writers, sought, shown)
			}
			// AND EVERY ARM STOPS AT A PAGE, which no plan says: this
			// engine's plan names no LIMIT, so an arm that lost its own is
			// the same seek in the same order, read to the end of its kind
			// before the merge keeps a page — every commit of those kinds
			// again, and every check above still passes. Measured. So the
			// statement carries a LIMIT for each arm beside the merge's.
			if limits := strings.Count(statement, "LIMIT ?"); kinds > 1 && limits != kinds+1 {
				t.Errorf("the audit of %d author kinds carries %d LIMITs, want one "+
					"per arm and the merge's:\n%s", kinds, limits, statement)
			}
		})
	}
}

// historyPasses is every statement that reads the WHOLE task history: the
// re-derivations [Applier.Rederive] runs when a node's derivation rules
// differ from this build's, each a function of every row by definition.
func historyPasses() map[string]plannedRead {
	every, args := restampStatement("")
	return map[string]plannedRead{
		"the hand-off re-derivation": {rederiveHandOffsStatement, []any{string(KindTask)}},
		"the answer re-derivation":   {answersStatement, nil},
		"the reopen re-derivation":   {reopensStatement, []any{string(KindTask)}},
		"every task's last change":   {every, args},
	}
}

// statementLiterals is every statement fragment of this module's non-test Go
// source that names one of tables as a source of rows — after FROM, JOIN or
// UPDATE — which is every fragment of every statement that reads them.
//
// THE SOURCE RATHER THAN A LIST, for the reason [taskReads] gives against a
// copy: a list beside the readers is one a new reader is added to by memory.
//
// THE WHOLE TREE RATHER THAN THIS PACKAGE, because the tables are read from
// outside it: the search package walks, fetches and counts the live tasks for
// its lexical index, names the tasks a page's backlinks cite and reads every
// task's body for the window report — and walked over this directory alone,
// none of those was a reader this gate could see, so the lexical index's walk
// read every live task in the company and sorted it for each batch while the
// gate passed. A package that starts reading a task table fails here until
// its statement is named, exported and registered, as search's are
// ([search.TaskVersionsRead] and the rest). The walk is [sourcetree.Walk]'s,
// so a nested checkout's copy of a file is never read as this tree's, and a
// walk that read no file outside this package fails rather than passing on
// this package alone.
//
// A FRAGMENT IS A RUN OF ADJACENT LITERALS, folded: `"… FROM " +
// "tracker_task_deps d …"` is one fragment, because a statement written across
// two literals reads the table exactly as one written in one does, and matched
// literal by literal neither half names it — which is how the unblocked block
// of `my_work` read the dependency table unseen. A run ends wherever the
// expression stops being literal (a placeholder list, a compiled clause, a
// named constant), and the fragment is matched WHOLE against the registered
// statements, so a statement assembled from several runs is held by each of
// them. A table name that is not spelled in any literal — a statement over a
// table the code chooses at run time, or over a compiled set, whose source is
// the set's own — is invisible here, which is why every such builder is
// registered by hand ([childClearStatement], and [queryStatements] for every
// statement over a set).
func statementLiterals(t *testing.T, tables ...string) []sourceLiteral {
	t.Helper()
	names := make([]string, len(tables))
	for i, table := range tables {
		names[i] = regexp.QuoteMeta(table)
	}
	reads := regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE)\s+(` +
		strings.Join(names, "|") + `)\b`)
	tree, err := moduleLiterals()
	if err != nil {
		t.Fatalf("read the module's source: %v", err)
	}
	if tree.outside == 0 {
		t.Fatalf("the walk read no Go file outside internal/tracker, so it "+
			"holds only this package's readers of %v", tables)
	}
	var out []sourceLiteral
	for _, literal := range tree.literals {
		if reads.MatchString(literal.text) {
			out = append(out, literal)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no literal of the module reads %v, so this walk is reading "+
			"nothing", tables)
	}
	return out
}

// sourceLiteral is one fragment [statementLiterals] found, and the file it is
// in, relative to the module's root.
type sourceLiteral struct {
	file, text string
}

// moduleLiterals is every literal run of the module's non-test Go source, and
// how many of the files it read are outside this package — read once for the
// test binary, since every walk asks the same tree.
var moduleLiterals = sync.OnceValues(func() (moduleSource, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return moduleSource{}, err
	}
	own := filepath.Join("internal", "tracker")
	fset := token.NewFileSet()
	var out moduleSource
	err = sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			// NOT THE MODULE'S CODE: nothing compiles it.
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go"):
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		if filepath.Dir(rel) != own {
			out.outside++
		}
		for _, run := range literalRuns(file) {
			out.literals = append(out.literals, sourceLiteral{rel, run})
		}
		return nil
	})
	return out, err
})

// moduleSource is what [moduleLiterals] read.
type moduleSource struct {
	literals []sourceLiteral
	outside  int
}

// literalRuns is every maximal run of adjacent string literals in a file's
// concatenations, each folded into the one string it spells.
func literalRuns(file *ast.File) []string {
	var out []string
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return true
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
		default:
			return true
		}
		// A CONCATENATION, flattened left to right: each run of literals
		// is one fragment, and every other operand is visited on its own,
		// because a call inside a concatenation can hold a statement too.
		var operands []ast.Expr
		var flatten func(ast.Expr)
		flatten = func(e ast.Expr) {
			switch y := e.(type) {
			case *ast.BinaryExpr:
				if y.Op == token.ADD {
					flatten(y.X)
					flatten(y.Y)
					return
				}
			case *ast.ParenExpr:
				flatten(y.X)
				return
			}
			operands = append(operands, e)
		}
		flatten(n.(ast.Expr))
		var run strings.Builder
		held := false
		end := func() {
			if held {
				out = append(out, run.String())
			}
			run.Reset()
			held = false
		}
		for _, operand := range operands {
			if lit, ok := operand.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil {
					run.WriteString(value)
					held = true
				}
				continue
			}
			end()
			ast.Inspect(operand, visit)
		}
		end()
		return false
	}
	ast.Inspect(file, visit)
	return out
}

var planNow = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)

// The corpus the task plans are taken against: twenty thousand tasks across
// thirty projects, and the tag, dependency and field values every fifth one
// carries.
//
// # What the rows decide, measured
//
// NOTHING, today. No node ever runs ANALYZE, so a deployment's planner has no
// statistics and plans from its built-in guesses whatever its tables hold —
// and this fixture is planned the same way ([seededPlanStore]). Those guesses
// do not read the rows on this engine: every plan the inventory takes over
// this corpus is the plan it takes over an empty estate.
//
// THEY ARE KEPT because that is a property of the ENGINE, not of the schema,
// and the engine is a dependency that moves. The shape is a deployment's — a
// project a thirtieth of the work, a due date a tenth of that, the skew that
// decides whether a partial index earns the write cost it charges on every
// commit on every node — so the day an engine release starts weighing a
// table's contents without being asked, the dependency update that brings it
// plans the inventory over what a deployment holds rather than over an empty
// file, and fails there rather than in production. What that costs is about
// seven of the ten seconds the inventory takes under the race detector.
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

// planStore is a replicated estate holding the task corpus ([corpusRows]),
// planned as a deployment plans it ([seededPlanStore]).
func planStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	return seededPlanStore(t, seedTaskCorpus)
}

// seededPlanStore is an empty replicated estate filled by seed in ONE
// transaction — the frame every plan fixture here shares, so each seeds the
// table its own statements read and nothing else.
//
// # Never ANALYZEd, because no deployment is
//
// Nothing in the engine runs ANALYZE, so every node's planner works from its
// built-in guesses, and a plan taken over statistics is a plan no node runs.
// This fixture used to ANALYZE, and that hid exactly the failure it exists to
// report: counted, the text, number, date and choice filters each sought its
// own column's index; uncounted, as on every node, each sought ANOTHER
// column's on `field_id` alone, and the text and number indexes served no
// read at all — certified as used for as long as the fixture counted what no
// deployment counts ([fieldClause] now names its index). Statistics were also
// the only way the rows ever reached a plan: with none, every plan the three
// plan tests take is the same over an empty estate.
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
// either way.
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
		i % 480, float64(i % 13), i * 100,
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
		 rank, assignee, estimate_min, points,
		 spend_tokens, due_at, start_at, finished_at,
		 created_at, updated_at, version, document)
	VALUES`

const taskValues = `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

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
		// UNDER THE TASK'S OWN PROJECT, as the applier writes a tag row
		// ([Applier.explodeTask]).
		{"tags", `INSERT INTO tracker_task_tags (task_id, project_key, slug) VALUES`,
			`(?,?,?)`, func(k int) []any {
				return []any{id(k), fmt.Sprintf("P%02d", k*tagEvery%projects),
					fmt.Sprintf("tag-%d", k*tagEvery%11)}
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

// explain is the planner's account of one statement.
//
// A WRITE IS PLANNED IN A WRITE TRANSACTION, because the read path runs
// query_only, which refuses even the EXPLAIN of one. Nothing is executed
// either way.
func explain(t *testing.T, db store.ReplicatedHandle, statement string, args []any) []string {
	t.Helper()
	tree := explainTree(t, db, statement, args)
	plan := make([]string, len(tree))
	for i, line := range tree {
		plan[i] = line.detail
	}
	return plan
}

// planLine is one line of a plan and the line it sits under — 0 for the
// statement's own top level — which is what tells a sort of a whole statement
// from a sort inside one of its subqueries.
type planLine struct {
	id, parent int
	detail     string
}

// explainTree is [explain] with each line's place in the plan's tree.
func explainTree(t *testing.T, db store.ReplicatedHandle, statement string,
	args []any) []planLine {

	t.Helper()
	in := db.Read
	if words := strings.Fields(statement); len(words) > 0 &&
		(strings.EqualFold(words[0], "UPDATE") || strings.EqualFold(words[0], "DELETE") ||
			strings.EqualFold(words[0], "INSERT")) {
		in = db.Tx
	}
	var plan []planLine
	if err := in(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			"EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line planLine
			var notUsed int
			if err := rows.Scan(&line.id, &line.parent, &notUsed, &line.detail); err != nil {
				return err
			}
			plan = append(plan, line)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN:\n%s\n%v", statement, err)
	}
	return plan
}

// indexesIn reads the index names out of a plan.
//
// AN INTERSECTION NAMES SEVERAL: this engine plans two equality terms on two
// indexes as `MULTI-INDEX AND h (a, b)`, a seek on each and the row ids they
// share, so both are read — and a disjunction of two as `MULTI-INDEX OR`, a
// seek on each and the row ids of either.
func indexesIn(plan []string) []string {
	var found []string
	for _, line := range plan {
		_, tail, ok := strings.Cut(line, "MULTI-INDEX AND ")
		if !ok {
			_, tail, ok = strings.Cut(line, "MULTI-INDEX OR ")
		}
		if ok {
			_, list, _ := strings.Cut(tail, "(")
			list, _, _ = strings.Cut(list, ")")
			for _, name := range strings.Split(list, ",") {
				found = append(found, strings.TrimSpace(name))
			}
			continue
		}
		_, tail, ok = strings.Cut(line, "USING INDEX ")
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

// walks reports a plan that reads through index in the index's own order —
// `SCAN … USING INDEX` — with no term on its leading columns to seek.
func walks(plan []string, index string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN ") &&
			slices.Contains(indexesIn([]string{line}), index) {
			return true
		}
	}
	return false
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
	"areas":  FieldLabels,
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
		out[ref] = resolvedField{ID: "field-" + ref, Slug: ref, Type: kind,
			Multi: MultiValued(kind)}
	}
	return out
}

// THE CONTAINER SURVIVES THE SUBTASK ROLLUP, and it is the only predicate the
// outer row has to enter on.
//
// The rollup asks the query's predicate of each row's ROOT, inside a probe of
// that root ([anchorProbe]), and it used to leave the row with nothing of its
// own but a tombstone every task shares — so every container-scoped query in
// the default subtask mode, which is every board, read every task in the
// company to probe each one's root. This asserts the compiled SQL rather than
// the plan because it is the CLAUSE that was missing: a planner that happened
// to pick some other index would hide it.
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
			set, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			where, args := set.where, set.args
			// THE PROBE OF THE ROOT, spelled rather than taken from
			// [anchorProbe], which is what is under test.
			outer, _, found := strings.Cut(where, "EXISTS (SELECT 1 FROM tracker_tasks a ")
			if !found {
				t.Fatalf("this query did not roll subtasks up at all:\n%s", where)
			}
			if !strings.Contains(outer, "t.project_key = ?") {
				t.Errorf("the rollup's OUTER predicate is %q and names no "+
					"container — the row itself is then every task in the "+
					"company, each probing its root", outer)
			}
			// AND EVERY ARGUMENT IS WHERE ITS PLACEHOLDER IS.
			//
			// Placeholders bind in TEXTUAL order, so a clause written
			// before the probe whose argument was appended after every
			// other binds the wrong value silently — and a clause-only
			// assertion cannot see it. The outer carries exactly one
			// placeholder, the container's; the probe's own first
			// placeholder is the root's copy of the same clause. So the
			// container has to be the first TWO arguments, and a
			// single-argument case would bind correctly whichever end it
			// was added at.
			if outers := strings.Count(outer, "?"); outers != 1 {
				t.Fatalf("the rollup's outer predicate %q carries %d "+
					"placeholders, want the container's one", outer, outers)
			}
			if len(args) < 2 || args[0] != "P01" || args[1] != "P01" {
				t.Errorf("the compiled arguments are %v, want the container "+
					"bound twice at the front — once for the row and once for "+
					"the root its probe reads", args)
			}
		})
	}
}

// A ROLLED-UP ROW PROBES ITS ANCHOR BY ITS KEY.
//
// The subtask rollup asks a query's predicate of each row's root, and the root
// is one row the row names, so the read is one seek of the table's key per row
// the statement visits ([anchorProbe]) — never a list of every root in scope,
// and never another index of the table entered on whatever the predicate
// offers. Both were measured. As a list (`root_id IN (SELECT t.id …)`) the
// planner held every live task in the company before the workspace's first
// row, a project's whole board before a board's, and the whole trash; and as
// a probe the key does not name, it stole the probe for the predicate's own
// equalities — `my queue` sought `tracker_tasks_queue_idx (assignee=?)` once
// per row, and a type, a unit, a lead's routing and a key filter each sought
// their own index the same way.
//
// So every statement the reader runs over a rolled-up set — the page, its
// count, its totals, a board's columns and rows, and where one task sits —
// seeks the anchor by its key, and reads the anchor no other way: the probe
// asking which side of the trash it is on reads it by its key too. The list
// form is refused for every statement by the gate's own [refuseSetWalks].
// Which queries roll up is spelled here from their parameters rather than
// asked of [rollsUp], which is part of what is under test.
func TestARolledUpRowProbesItsAnchorByKey(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	// THE ANCHOR AND ITS SIDE: the probe of the root, and the probe asking
	// which side of the trash it is on ([anchorOf]) — each one task by its
	// key, and neither read any other way.
	probe := regexp.MustCompile(`^SEARCH a USING (INDEX sqlite_autoindex_tracker_tasks_1|PRIMARY KEY) \(id=\?\)$`)
	side := regexp.MustCompile(`^SEARCH s USING (INDEX sqlite_autoindex_tracker_tasks_1|PRIMARY KEY) \(id=\?\)$`)
	anchor := regexp.MustCompile(`^(SEARCH [as] |SCAN tracker_tasks AS [as]( |$))`)
	rolled := 0
	for name, params := range registeredQueries() {
		_, moded := params["subtasks"]
		_, parent := params["parent"]
		_, root := params["root"]
		if moded || parent || root {
			continue
		}
		rolled++
		t.Run(name, func(t *testing.T) {
			for what, statement := range queryStatements(t, planQuery(t, params)) {
				// ONE TASK'S OWN VALUES, read by its id with no
				// predicate to roll up.
				if what == "the around task's sort values" {
					continue
				}
				plan := explain(t, db, statement.sql, statement.args)
				shown := strings.Join(plan, "\n")
				sought := false
				for _, line := range plan {
					switch {
					case probe.MatchString(line):
						sought = true
					case side.MatchString(line):
					case anchor.MatchString(line):
						t.Errorf("%s reads each row's root other than by its "+
							"key:\n%s", what, shown)
					}
				}
				if !sought {
					t.Errorf("%s does not probe each row's root by its key:\n%s",
						what, shown)
				}
			}
		})
	}
	if rolled == 0 {
		t.Fatal("no registered query rolls its subtasks up, so this measures nothing")
	}
}

// THE TRASH IS READ THROUGH ITS OWN INDEX, BY EVERY STATEMENT OVER IT.
//
// A removed task is a fraction of the company and only the trash reads one, so
// a trash costs what is in it — never what the company holds. That is a claim
// about every statement an answer over the trash issues, not only about its
// page: measured before [taskSource] named the index, the workspace trash's
// page walked the old index but its count, its totals, a board's column counts
// and the subtree rollup's own list of roots each read every task in the
// company from the heap (this engine's planner reaches a partial index only
// for an order it provides or a seek on its leading column, and the tombstone
// is neither), a project's trash sought the project's whole board through
// `tracker_tasks_rank_idx` at every level and sorted it, and a click on the
// Title column sorted the company.
//
// So each statement here is the reader's own ([taskRowsStatement],
// [cappedCountStatement], [totalsStatement], [groupCountsStatement]) over the
// set [compile] produces, at both scopes and in both subtask modes, and every
// line of its plan that reads the task table's rows reads the scope's index.
// The rollup's probe of each row's root reads that one root by its key, under
// an alias of its own, and is held to the key by
// [TestARolledUpRowProbesItsAnchorByKey]. The page in the trash's own order is
// held to it unsorted as well, because that order is the index's and a LIMIT
// stops it after a page — and so is every arm of the next page of the trash as
// a list, which resumes the same walk (a board takes no cursor), and every read
// of a task's neighbour, which walks it forwards or back: the trash's removal
// is never NULL inside the trash, so the order reversed is the index read
// backwards rather than an order that leads with whether the removal is NULL.
// A page ordered by something else may sort, and sorts the trash.
func TestTheTrashIsReadThroughItsOwnIndex(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	cases := map[string]struct {
		params map[string]any
		index  string
	}{
		"a project's trash": {map[string]any{"container": "project:P01"},
			projectRemovedIndex},
		"a project's trash, flat": {map[string]any{"container": "project:P01",
			"subtasks": "separate"}, projectRemovedIndex},
		"the company's trash": {map[string]any{}, removedIndex},
		"the company's trash, flat": {map[string]any{"subtasks": "separate"},
			removedIndex},
	}
	for name, c := range cases {
		for _, sort := range []string{"", "title"} {
			for _, group := range []string{"status", ""} {
				shape := ", as a board"
				if group == "" {
					shape = ", as a list"
				}
				t.Run(name+" sorted by "+cmpOr(sort, "its removal")+shape, func(t *testing.T) {
					params := map[string]any{"removed": "true", "show_closed": "true",
						"totals": "tasks:count,points:sum"}
					if group != "" {
						params["group_by"] = group
					}
					for key, value := range c.params {
						params[key] = value
					}
					if sort != "" {
						params["sort"] = sort
					}
					// EVERY STATEMENT THE READER RUNS FOR IT but the around
					// read's two of one task, which read it by its key — the
					// probe of which run holds it is held to that by
					// [TestWhereATaskSitsIsAskedByItsKey].
					statements := queryStatements(t, planQuery(t, params))
					delete(statements, "the around task's sort values")
					delete(statements, "the around task's run")
					if _, paged := statements[nextPageArm(0)]; group == "" && !paged {
						t.Fatalf("the trash as a list has no next page to plan")
					}
					// TWO ARMS IN ITS OWN ORDER — the removal's ties, then
					// past it — and never a third for a NULL removal, which
					// the listing's own predicate excludes ([sortTerms]).
					if _, third := statements[nextPageArm(2)]; group == "" && sort == "" && third {
						t.Errorf("the trash's next page reads an arm for the tasks " +
							"with no removal, which no trash listing holds")
					}
					for what, statement := range statements {
						plan := explain(t, db, statement.sql, statement.args)
						shown := strings.Join(plan, "\n")
						others, read := tasksReadOtherwise(plan, c.index)
						switch {
						case !read:
							t.Errorf("%s reads no task at all, so this case "+
								"measures nothing:\n%s", what, shown)
						case len(others) > 0:
							t.Errorf("%s reads the task table other than through "+
								"%s — every task of the company, or of the "+
								"project, to find the removed ones:\n%s",
								what, c.index, shown)
						}
						// THE PAGE, EVERY ARM OF THE NEXT ONE AND EVERY
						// READ OF A NEIGHBOUR, which walk the same index
						// forwards or back.
						walk := what == "the page" || what == runFirst ||
							what == runLast || strings.HasPrefix(what, nextPage) ||
							strings.HasPrefix(what, nextRow) ||
							strings.HasPrefix(what, previousRow)
						if walk && sort == "" && sorts(plan) {
							t.Errorf("%s in the trash's own order sorts what "+
								"it read, so it is the whole trash rather than a "+
								"page:\n%s", what, shown)
						}
					}
				})
			}
		}
	}
}

// A LATER PAGE SEEKS THE ORDER ITS FIRST PAGE WALKS.
//
// A listing an index serves — a board by its manual order, the workspace by
// its last update, a project by the same, the company by its spend, the trash
// by its removal — reads its first page as a walk of that index stopped by a
// LIMIT. Its later pages resume from a cursor, and a resume the planner cannot
// seek re-walks the index from its start to the cursor and filters every row
// on the way: page fifty reads fifty pages' worth, and paging to the end of a
// listing costs the square of its length. That was the cursor's shape, one
// lexicographic disjunction, and the gate could not see it, because it planned
// the first page alone.
//
// So for every registered query whose first page walks an index without
// sorting, every arm of its next page ([readPage], through a cursor
// [mintCursor] minted and [pageClause] read back, so the arithmetic under test
// is the reader's own) must read the task table through THAT index, SEEK it
// with a bound on the order's leading column, and not sort. A first page that
// sorts is exempt: no index serves its order in its scope, so every page reads
// the scope, and its arms are held to what every read is held to.
//
// The day an index leads with a NULL-able column, a resume past a value is two
// arms — the values beyond it and the NULL tail — and each must seek: that is
// [keyset]'s NULL rule, and this test is where an order breaking it shows.
//
// NOT VACUOUS: the indexes the checked first pages walk must include every one
// named below, so a fixture or a planner change that left this test checking
// nothing fails here instead of passing.
func TestALaterPageSeeksTheOrderItsFirstPageWalks(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	walked := map[string]bool{}
	for name, params := range registeredQueries() {
		q := planQuery(t, params)
		statements := queryStatements(t, q)
		if _, paged := statements[nextPageArm(0)]; !paged {
			continue
		}
		first := explainTree(t, db, statements["the page"].sql,
			statements["the page"].args)
		top, read := topTaskRead(first)
		if !read || sortsAtTop(first) {
			continue
		}
		indexes := indexesIn([]string{top.detail})
		if len(indexes) != 1 {
			continue
		}
		index := indexes[0]
		walked[index] = true
		terms := sortTerms(q, planFields(q))
		leading := strings.TrimPrefix(terms[0].Column, rowAlias.col(""))
		bound := regexp.MustCompile(`\(.*\b` + regexp.QuoteMeta(leading) + `[=<>]`)
		t.Run(name, func(t *testing.T) {
			for i := 0; ; i++ {
				arm, ok := statements[nextPageArm(i)]
				if !ok {
					break
				}
				tree := explainTree(t, db, arm.sql, arm.args)
				shown := strings.Join(planDetails(tree), "\n")
				line, read := topTaskRead(tree)
				switch {
				case !read:
					t.Errorf("%s reads no task at all:\n%s", nextPageArm(i), shown)
				case !strings.HasPrefix(line.detail, "SEARCH ") ||
					!slices.Contains(indexesIn([]string{line.detail}), index) ||
					!bound.MatchString(line.detail):
					t.Errorf("%s does not SEEK %s on %s, the index and the "+
						"column the first page walks — so it walks the order "+
						"from its start to the cursor:\n%s", nextPageArm(i),
						index, leading, shown)
				case sortsAtTop(tree):
					t.Errorf("%s sorts what it read, so it is everything after "+
						"the cursor rather than a page:\n%s", nextPageArm(i),
						shown)
				}
			}
		})
	}
	// SPELLED rather than taken from the constants, which name what is under
	// test.
	for _, index := range []string{"tracker_tasks_board_idx",
		"tracker_tasks_updated_idx", "tracker_tasks_recent_idx",
		"tracker_tasks_spend_idx", "tracker_tasks_removed_idx",
		"tracker_tasks_project_removed_idx"} {
		if !walked[index] {
			t.Errorf("no registered query's first page walks %s unsorted, so "+
				"this test never checks a later page of a listing in its "+
				"order", index)
		}
	}
}

// topTaskRead is the statement's own read of its rows — the line at the top
// of the plan that reads the task table under the row's alias.
func topTaskRead(tree []planLine) (planLine, bool) {
	for _, line := range tree {
		if line.parent != 0 {
			continue
		}
		if strings.HasPrefix(line.detail, "SCAN tracker_tasks AS "+string(rowAlias)+" ") ||
			strings.HasPrefix(line.detail, "SEARCH "+string(rowAlias)+" ") {
			return line, true
		}
	}
	return planLine{}, false
}

// sortsAtTop reports a plan that sorts the statement's own rows, as opposed to
// one of its subqueries' ([sorts] reads every line).
func sortsAtTop(tree []planLine) bool {
	for _, line := range tree {
		if line.parent == 0 && strings.Contains(line.detail, "FOR ORDER BY") {
			return true
		}
	}
	return false
}

// cmpOr is value, or otherwise when value is empty.
func cmpOr(value, otherwise string) string {
	if value == "" {
		return otherwise
	}
	return value
}

// tasksReadOtherwise is every line of a plan that reads the task table through
// anything but index — a heap scan, or another of its indexes — and whether
// any line reads the table at all.
//
// A LINE READS IT when it scans `tracker_tasks` or seeks under the alias `t`,
// which every statement over a compiled set gives it: this engine writes a
// scan under the table's name and a seek under the alias (see [seeks]).
func tasksReadOtherwise(plan []string, index string) ([]string, bool) {
	var others []string
	read := false
	for _, line := range plan {
		if !strings.HasPrefix(line, "SCAN tracker_tasks ") && line != "SCAN tracker_tasks" &&
			!strings.HasPrefix(line, "SEARCH t ") {
			continue
		}
		read = true
		if !slices.Contains(indexesIn([]string{line}), index) {
			others = append(others, line)
		}
	}
	return others, read
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
// # Reaching the index is not the claim
//
// The claim is that a poll costs the WINDOW's changes, and a plan can name the
// index while breaking it, so each read is held to the plan its shape calls
// for:
//
//   - A read that BOUNDS `effective_at` — the flow's window, a feed page past
//     its cursor — SEEKS the index on it ([seeks]). Measured: wrapping the
//     column in the flow's walk (`effective_at + 0 >= ?`) turns its SEARCH
//     into a SCAN of the index — every change that ever moved a count, in
//     the index's order — and that plan still names the index, which is all
//     this test used to ask.
//   - A read with NO bound — the newest page, filtered or not — is an order
//     and a LIMIT, and the right plan is the index walked in its own order and
//     stopped after a page. Right only while the index's order IS the read's,
//     so no read here SORTS ([sorts]): measured, ordering the feed by
//     `effective_at, id` kept the index, added a sort of every moving change
//     before the first row, and passed.
//
// PLANNED OVER A COMPANY'S HISTORY ([historyPlanStore]) and with no
// statistics, as a deployment plans it ([seededPlanStore]).
func TestTheFlowAndFeedReadsSearchTheirIndex(t *testing.T) {
	t.Parallel()
	db := historyPlanStore(t)
	const moves = "tracker_history_moves_idx"
	reads := companyWideReads()
	for name, r := range reads {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, r.sql, r.args)
			shown := strings.Join(plan, "\n")
			switch {
			case !slices.Contains(indexesIn(plan), moves) ||
				scansHeap(plan, "tracker_history"):
				t.Errorf("this read does not reach %s:\n%s", moves, shown)
			case r.bounded && !seeks(plan, moves):
				t.Errorf("this read bounds effective_at and does not SEEK %s "+
					"on it — it walks every change that ever moved a count, "+
					"which is the company's history rather than the "+
					"window's:\n%s", moves, shown)
			}
			if sorts(plan) {
				t.Errorf("this read sorts what it selected rather than reading "+
					"%s in its own order, so every moving change it selects is "+
					"read before the first row is returned:\n%s", moves, shown)
			}
		})
	}
}

// companyWideRead is one of the company-wide history reads, and whether it
// bounds `effective_at`, the moves index's leading column, and so has a range
// to seek.
type companyWideRead struct {
	plannedRead
	bounded bool
}

// companyWideReads is `work_flow`'s walk and `company_feed`'s tracker page in
// the shapes the feed is read in: the widest window the flow walks —
// [MaxFlowPoints] days back — and a feed page a month down, both inside the
// history [historyPlanStore] spans.
func companyWideReads() map[string]companyWideRead {
	from := planNow.AddDate(0, 0, -MaxFlowPoints)
	before := &FeedCursor{At: planNow.AddDate(0, -1, 0), Seq: 9}
	reads := map[string]companyWideRead{
		"the flow's walk": {plannedRead{flowRowsStatement,
			[]any{store.EncodeTime(from)}}, true},
	}
	for name, c := range map[string]struct {
		q       FeedQuery
		bounded bool
	}{
		"the newest feed page": {FeedQuery{Limit: 20}, false},
		"a later feed page":    {FeedQuery{Limit: 20, Before: before}, true},
		"one writer's hand-offs": {FeedQuery{Limit: 20, Actor: "h-7",
			Kinds: []FeedKind{FeedHandoff}}, false},
		"a later page of one writer's hand-offs": {FeedQuery{Limit: 20, Actor: "h-7",
			Kinds: []FeedKind{FeedHandoff}, Before: before}, true},
	} {
		kinds := c.q.Kinds
		if len(kinds) == 0 {
			kinds = FeedKinds
		}
		sql, args := companyFeedStatement(c.q, kinds)
		reads[name] = companyWideRead{plannedRead{sql, args}, c.bounded}
	}
	return reads
}

// The history the company-wide reads are planned against.
//
// # Why most of it moves nothing
//
// It decides no plan today, for the reason the task corpus decides none
// ([corpusRows]), and it is a company's shape for the reason that corpus is:
// an engine that weighs contents judges a PARTIAL index by how much of the
// table its predicate keeps, and most commits change nothing either reader
// draws — a title, a tag, a comment, a due date. So one commit in
// [historyMoveEvery] moves a count (a create, a status, an assignee, a
// project, a removal), one in [historyNotATask] is not about a task at all,
// and the rest are the quiet changes that make up a company's history. Twenty
// thousand rows over two years and two thousand tasks is ten commits a task,
// and a [MaxFlowPoints]-day window is an eighth of it.
const (
	historyRows      = 20_000
	historySubjects  = 2_000
	historyMoveEvery = 10
	historyNotATask  = 20
	historyPurged    = 50
)

// historyPlanStore is a replicated estate holding a company history, the
// tasks it is about and the deletion markers of the one in [historyPurged]
// that were purged.
//
// THE TASKS AND THE MARKERS ARE THERE FOR THE FEED'S JOINS: its page LEFT
// JOINs both on their primary keys, so the tables it joins hold what a
// company's do.
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

// A FIELD'S VALUES ARE READ BY THEIR TASK, OR BY THEIR FIELD AS A FILTER.
//
// `tracker_field_values` is read two ways and each has its own index. A FILTER
// asks which tasks hold a value, which the field's own column's index answers
// as one range, so its subquery is driven from there ([fieldClause]); every
// other read — a sort's join, a grouping's join and its column filter, a
// total over the answer — asks for the values of tasks the statement already
// holds, which the table's key answers one task at a time ([fieldValuesKey]).
//
// Both are held to it in every statement the reader runs for the query
// ([queryStatements]), because the four value indexes all lead with
// `field_id` and are partial on `hidden = 0`, which every one of these reads
// states: with no statistics the planner took one of them for a per-task
// probe and sought `field_id` alone — measured, every value the field holds,
// read once per task on a page sorted by a choice field, on a board grouped
// by one and in that board's column counts; one column's rows read the
// field's values and then the project's board once per value; and a total
// over a number field read the field's every value through the choice
// column's index; and a later page sorted by a field, read arm by arm, bounded
// the joined value, made its LEFT JOIN an inner one and walked every value in
// the company before a task ([pageArmSets]). Every one of those plans passed
// the inventory, which only asks that some plan reach each index.
//
// So a line seeking the key must seek it on a task, and a line reading a
// value index must be a filter's — the index of a field the query filters on,
// read inside the subquery that finds the filtered tasks.
func TestAFieldValueIsReadByItsTaskOrByItsFilter(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	for name, params := range map[string]map[string]any{
		"sorted by a field":            {"container": "project:P01", "sort": "f.impact"},
		"sorted by a field everywhere": {"sort": "-f.effort"},
		"a board of a field":           {"container": "project:P01", "group_by": "f.impact"},
		"a board of a field everywhere": {"group_by": "f.impact",
			"subtasks": "separate"},
		"a board of a labels field": {"container": "project:P01",
			"group_by": "f.areas"},
		"one column of a field's board": {"container": "project:P01",
			"group_by": "f.impact", "group": "o-high"},
		"the unset column of a field's board": {"container": "project:P01",
			"group_by": "f.impact", "group": ""},
		"totals of fields": {"container": "project:P01",
			"totals": "f.effort:sum,f.effort:count,f.effort:median,f.ship:max"},
		"totals of fields everywhere": {"totals": "f.effort:avg,f.ship:p90"},
		"a field filtered and another sorted": {"container": "project:P01",
			"f.owner": "platform", "sort": "f.effort"},
		"a field filtered, grouped and totalled": {"f.impact": "o-high",
			"group_by": "f.impact", "totals": "f.effort:sum"},
	} {
		t.Run(name, func(t *testing.T) {
			q := planQuery(t, params)
			filters := map[string]bool{}
			for _, filter := range q.Fields {
				filters["tracker_field_values_"+
					FieldValueColumn(planFieldTypes[filter.Ref])+"_idx"] = true
			}
			for what, statement := range queryStatements(t, q) {
				holdFieldReads(t, what, explainTree(t, db, statement.sql,
					statement.args), filters)
			}
		})
	}
}

// ONE COLUMN OF A BOARD IS THE ANSWER NARROWED BY A PROBE OF EACH TASK'S ROWS.
//
// A statement that reads one column or lane rather than counting them — the
// column's rows, and where one task sits in it — narrows the answer by the
// axis's join-free predicate ([columnSet]): a probe of each task's own tag or
// field rows by their key. Narrowed through the axis's join instead it was an
// inner join onto the child table, which the planner is free to drive from the
// child's side, and with no statistics did so as soon as anything moved the
// task side's estimate — measured at the workspace, a field column walked
// every value of the field in the company before the first task, and a tag
// column sought every task carrying the tag and walked the task table once for
// each. So in every such statement, at both scopes and in both subtask modes, a
// read of the axis's child table outside a filter's own IN list seeks it on a
// task.
func TestOneColumnIsTheAnswerNarrowedByAProbe(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	for name, params := range inBothModes(map[string]map[string]any{
		"a tag column":                 {"container": "project:P01", "group_by": "tag"},
		"a tag column everywhere":      {"group_by": "tag"},
		"a field column":               {"container": "project:P01", "group_by": "f.impact"},
		"a field column everywhere":    {"group_by": "f.impact"},
		"a field's lanes everywhere":   {"group_by": "assignee", "group_by2": "f.impact"},
		"a tag's lanes in a project":   {"container": "project:P01", "group_by": "status", "group_by2": "tag"},
		"a filtered column everywhere": {"f.impact": "o-high", "group_by": "f.impact"},
	}) {
		t.Run(name, func(t *testing.T) {
			for what, statement := range queryStatements(t, planQuery(t, params)) {
				if !strings.HasPrefix(what, "the rows of column") &&
					!strings.HasPrefix(what, "the around task's") &&
					!strings.HasPrefix(what, rowsAhead) &&
					what != runFirst && what != runLast {
					continue
				}
				tree := explainTree(t, db, statement.sql, statement.args)
				listed := underList(tree)
				shown := strings.Join(planDetails(tree), "\n")
				for _, line := range tree {
					if listed(line) || !readsChildOfTask(line.detail) {
						continue
					}
					if !strings.HasPrefix(line.detail, "SEARCH ") ||
						!strings.Contains(line.detail, "(task_id=?") {
						t.Errorf("%s reads a task's child table other than by "+
							"the task:\n%s", what, shown)
					}
				}
			}
		})
	}
}

// readsChildOfTask reports a plan line that reads a task's tags or field
// values: a scan names the table, and a seek its index, which is the table's.
func readsChildOfTask(detail string) bool {
	for _, table := range []string{"tracker_task_tags", "tracker_field_values"} {
		if strings.HasPrefix(detail, "SCAN "+table) {
			return true
		}
		for _, index := range indexesIn([]string{detail}) {
			if strings.HasPrefix(index, table+"_") ||
				index == "sqlite_autoindex_"+table+"_1" {
				return true
			}
		}
	}
	return false
}

// holdFieldReads fails a plan that reads `tracker_field_values` through the
// key on anything but a task, or through a value index anywhere but inside a
// filter's own subquery on a field the query filters by.
func holdFieldReads(t *testing.T, what string, plan []planLine, filters map[string]bool) {
	t.Helper()
	const key = "sqlite_autoindex_tracker_field_values_1"
	// INSIDE AN IN LIST'S SUBQUERY, which this engine plans once for the
	// statement rather than once per row.
	inList := underList(plan)
	shown := strings.Join(planDetails(plan), "\n")
	for _, line := range plan {
		for _, index := range indexesIn([]string{line.detail}) {
			switch {
			case index == key && !strings.Contains(line.detail, "(task_id=?"):
				t.Errorf("%s reads field values through the key without a "+
					"task to seek it on:\n%s", what, shown)
			case index != key && strings.HasPrefix(index, "tracker_field_values_") &&
				(!filters[index] || !inList(line)):
				t.Errorf("%s reads field values through %s, which is a "+
					"filter's index, other than as a filter — every value the "+
					"field holds, rather than the values of the tasks it "+
					"reads:\n%s", what, index, shown)
			}
		}
	}
}

// planQuery is params parsed as a caller's query.
func planQuery(t *testing.T, params map[string]any) Query {
	t.Helper()
	q, err := ParseQuery(MapParams(params), planNow, time.UTC)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	return q
}

// queryStatements is every statement the reader runs for one query, each
// built by the reader's own builder over the query's own compiled set: the
// page, every arm of the page after it, and the count it carries, the totals
// and each order statistic's read of one value, a board's columns and one
// column's rows, and the around read's sort values of one task and its four
// reads of where that task sits — so a plan of each is a plan of what a
// caller's request issues, never of a copy.
func queryStatements(t *testing.T, q Query) map[string]plannedRead {
	t.Helper()
	fields := planFields(q)
	set, err := compile(q, planNow, fields)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	terms := sortTerms(q, fields)
	page, pageArgs := taskRowsStatement(set, terms)
	values, valueArgs := sortValuesStatement(terms, "t-00001")
	out := map[string]plannedRead{
		"the page": {page, append(pageArgs, 51)},
		"the count it carries": {cappedCountStatement(set.from, set.where),
			append(append([]any{}, set.args...), TotalHintCeiling+1)},
		"the around task's sort values": {values, valueArgs},
	}
	// THE ROW EVERY BOUNDED READ BELOW IS AFTER OR BEFORE: a value for each
	// term of the order. They bind as values, and plan alike whatever they
	// are.
	keys := make([]any, len(terms))
	for i := range keys {
		keys[i] = "a000100"
	}
	// THE NEXT PAGE of a flat answer, arm by arm ([readPage]), resumed from
	// a cursor [mintCursor] minted on that row and read back by
	// [pageClause] — a grouped answer takes no cursor, and a list-ordered
	// one pages the list itself.
	if q.GroupBy == "" && !listOrdered(q) {
		next := q
		next.Cursor = mintCursor(keys, renderOrder(terms))
		resume, err := pageClause(next, fields)
		if err != nil {
			t.Fatalf("pageClause: %v", err)
		}
		for i, arm := range pageArmSets(set, resume, terms) {
			statement, args := taskRowsStatement(arm, terms)
			out[nextPageArm(i)] = plannedRead{statement, append(args, 51)}
		}
	}
	// WHERE ONE TASK SITS ([readAround]), over the run that holds it — the
	// whole answer, or one column of a board ([groupAxis.run]): which run it
	// is, how many of the run's rows are ahead of it, and its neighbour on
	// either side. Built over the answer's set, these name no table of their
	// own, so the completeness walk below cannot see them and only this
	// can. The task's sort values bind as values, and plan alike whatever
	// they are.
	run := segment{}
	if q.GroupBy != "" {
		axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
		if err != nil {
			t.Fatalf("compileGroup: %v", err)
		}
		run = axis.run("o-high", 1)
	}
	after, before := aroundSides(terms, keys)
	holds, holdsArgs := segmentHoldsStatement(run, set, "t-00001")
	out["the around task's run"] = plannedRead{holds, holdsArgs}
	forward, reverse := renderOrder(terms), renderReverse(terms)
	for i, arm := range before {
		ahead, aheadArgs := segmentCountStatement(run, terms, set, arm.clause,
			arm.args)
		out[aroundArm(rowsAhead, i)] = plannedRead{ahead, aheadArgs}
		previous, previousArgs := segmentFirstStatement(run, terms, set,
			arm.clause, arm.args, reverse)
		out[aroundArm(previousRow, i)] = plannedRead{previous, previousArgs}
	}
	for i, arm := range after {
		next, nextArgs := segmentFirstStatement(run, terms, set, arm.clause,
			arm.args, forward)
		out[aroundArm(nextRow, i)] = plannedRead{next, nextArgs}
	}
	// AND THE TWO UNBOUNDED READS beside them: a neighbour in the run
	// before or after the task's own is that run's last or first row.
	first, firstArgs := segmentFirstStatement(run, terms, set, "", nil, forward)
	out[runFirst] = plannedRead{first, firstArgs}
	last, lastArgs := segmentFirstStatement(run, terms, set, "", nil, reverse)
	out[runLast] = plannedRead{last, lastArgs}
	if len(q.Totals) > 0 {
		totals, exprs, totalArgs, err := compileTotals(q.Totals, fields)
		if err != nil {
			t.Fatalf("compileTotals: %v", err)
		}
		out["its totals"] = plannedRead{totalsStatement(set, exprs),
			append(append([]any{}, set.args...), totalArgs...)}
		for _, total := range totals {
			if total.ranked == "" {
				continue
			}
			statement, args := rankedStatement(set, total, 3)
			out["its "+total.Op+" of "+total.Column] = plannedRead{statement, args}
		}
	}
	if q.GroupBy != "" {
		axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
		if err != nil {
			t.Fatalf("compileGroup: %v", err)
		}
		columns, columnArgs := groupCountsStatement(axis, set)
		out["a board's columns"] = plannedRead{columns, append(columnArgs, MaxGroups+1)}
		for _, key := range []string{"o-high", ""} {
			rows, rowArgs := taskRowsStatement(columnSet(axis, key, set), terms)
			out[fmt.Sprintf("the rows of column %q", key)] = plannedRead{rows,
				append(rowArgs, 21)}
		}
	}
	return out
}

// nextPageArm names the statement [readPage] reads the i-th arm of a later
// page with.
func nextPageArm(i int) string {
	return nextPage + strconv.Itoa(i)
}

// nextPage is what every arm of a later page is named by.
const nextPage = "the next page, arm "

// What the around read's statements are named by: each side's arms, and the
// two unbounded reads of a run.
const (
	rowsAhead   = "the rows ahead of the around task"
	previousRow = "the around task's previous"
	nextRow     = "the around task's next"
	runFirst    = "the first row of a run"
	runLast     = "the last row of a run"
)

// aroundArm names the statement the around read reads one arm of a side with.
func aroundArm(side string, i int) string {
	return side + ", arm " + strconv.Itoa(i)
}

// A DROP READS THE GAP IT OPENED, NOT THE BOARD BETWEEN IT AND ONE END.
//
// A drop reads a dozen neighbours on each side of the pivot, nearest first,
// so its plan is a seek on the project AND the pivot's rank in the rank
// index's order. The comparison it makes — past the pivot in (rank, id)
// order — written as a disjunction left the planner the project alone to
// seek, and the plan read the project's rank index from the far end of the
// board towards the pivot, filtering every card on the way: a drop near the
// bottom of a long board read the whole board. That plan SEEKS the index the
// gate's taskReads names for it, so it passed there; the claim this holds is
// that the rank is part of the seek.
func TestADropReadsTheGapItOpened(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	for _, above := range []bool{false, true} {
		for _, inclusive := range []bool{false, true} {
			plan := explain(t, db, gapSideStatement(above, inclusive), []any{"P01",
				"t-00002", "a000100", "a000100", "t-00001", RankRespreadInline/2 + 1})
			shown := strings.Join(plan, "\n")
			sought := false
			for _, line := range plan {
				if seeks([]string{line}, "tracker_tasks_rank_idx") &&
					strings.Contains(line, "(project_key=? AND rank") {
					sought = true
				}
			}
			if !sought || sorts(plan) {
				t.Errorf("above=%t inclusive=%t: the gap is not a seek on the "+
					"project and the pivot's rank in the index's order:\n%s",
					above, inclusive, shown)
			}
		}
	}
}
