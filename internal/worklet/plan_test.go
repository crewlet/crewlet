package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
// the set is held COMPLETE: every statement in this package's source that
// reads the table is one of them.
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

	used := map[string]bool{}
	for name, params := range registeredQueries() {
		t.Run(name, func(t *testing.T) {
			q, err := ParseQuery(MapParams(params), planNow, time.UTC)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			set, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			// THE SAME STATEMENT THE READER RUNS ([taskRowsStatement]).
			// Explaining the WHERE clause alone would explain a query
			// nobody issues — and the FROM and the ORDER BY are half of
			// what decides which index the planner reaches for.
			statement, args := taskRowsStatement("", nil, set,
				sortTerms(q, planFields(q)))
			plan := explain(t, db, statement, append(args, 51))
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			refuseHeapScans(t, plan)
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
			// [fieldValueIndex], which is what is under test.
			for _, field := range planFields(q) {
				index := "tracker_field_values_" + FieldValueColumn(field.Type) + "_idx"
				if !seeks(plan, index) {
					t.Errorf("this filter on a %s field does not SEEK %s:\n%s",
						field.Type, index, strings.Join(plan, "\n"))
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
	for name, read := range dutyReads() {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, read.sql, read.args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
			refuseHeapScans(t, plan)
			holdToIndex(t, plan, read, covered)
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
	// of this package's source that names the table as a source of rows
	// has to be part of a registered statement. An INSERT finds no rows,
	// which is why the history's own is not among them.
	registered := make([]string, 0, len(reads)+len(passes))
	for _, read := range reads {
		registered = append(registered, read.sql)
	}
	for _, statement := range passes {
		registered = append(registered, statement.sql)
	}
	for _, literal := range historyLiterals(t) {
		if !slices.ContainsFunc(registered, func(statement string) bool {
			return strings.Contains(statement, literal)
		}) {
			t.Errorf("this statement reads tracker_history and is registered in "+
				"neither historyReads nor historyPasses, so no plan of it is "+
				"taken — name it, as every other is named, and register it:\n%s",
				literal)
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

		// THE TRASH AS ITS TAB ASKS FOR IT ([ViewKeyTrash]), and with every
		// row filtered on its own, which is how the dashboard and
		// `list_work_items` run any view — at both scopes, because each is
		// read through an index of its own ([taskSource]). Every statement
		// over it, not only its page, is held to that index by
		// [TestTheTrashIsReadThroughItsOwnIndex].
		"the trash": {"container": "project:P01", "removed": "true",
			"show_closed": "true"},
		"the trash, flat": {"container": "project:P01", "removed": "true",
			"show_closed": "true", "subtasks": "separate"},
		"the trash everywhere": {"removed": "true", "show_closed": "true"},
		"the trash everywhere, flat": {"removed": "true", "show_closed": "true",
			"subtasks": "separate"},
	}
}

// dutyReads is every statement outside the query grammar that this schema's
// indexes claim a reader for — the duty selections and the apply-path probes —
// each with the index its plan must reach ([indexedRead]).
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
func dutyReads() map[string]indexedRead {
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
	return map[string]indexedRead{
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
		"key resolution":       seek(keys, resolveTaskStatement, "ENG-1", "ENG-1", "ENG-1"),
		"a point read's scope": seek(keys, taskReadScopeStatement, "ENG-1", "ENG-1", "ENG-1"),
		"one tag's tasks": seek("tracker_task_tags_slug_idx",
			`SELECT task_id FROM tracker_task_tags
			 WHERE project_key = ? AND slug = ?`, "P01", "tag-1"),
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
// join, an asker's next record after an ask closed, the unblocked repair, and
// the applier's own probes and rewrites — each with arguments of the shape its
// reader binds and the index it reaches. The ones that read the whole table
// are [historyPasses].
//
// THE STATEMENTS ARE THE READERS' OWN, never a copy: each is a named value of
// this package, and the gate's completeness walk ([historyLiterals]) is what
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
		project = "tracker_history_project_idx"
		seq     = "tracker_history_seq_idx"
		writers = "tracker_history_actor_kind_seq_idx"
		status  = "tracker_history_status_seq_idx"
		moves   = "tracker_history_moves_idx"
	)
	seek := func(index, sql string, args ...any) indexedRead {
		return indexedRead{plannedRead{sql, args}, index, false}
	}
	at := func(n uint64) int64 {
		return statelog.Position{Stream: historyStream, Generation: 1, Seq: n}.Packed()
	}
	position := statelog.Position{Stream: historyStream, Generation: 1, Seq: 9_000}
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

	// THE ACTIVITY FEED, one case per filter family the reader accepts,
	// spelled as its surfaces spell an [ActivityQuery] and compiled by its
	// own compiler. A CASE THE READER WOULD REFUSE IS NOT A READER, so each
	// passes the reader's own checks first. A filter no history index leads
	// with — a change kind, a writer, an assignee, the announcement flag, a
	// wall-clock window — is a residual on the walk of the feed's own order.
	yes := true
	window := planNow.AddDate(0, 0, -7)
	activity := map[string]struct {
		q     ActivityQuery
		index string
		walk  bool
	}{
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
	for name, c := range activity {
		if err := activityFilters(c.q); err != nil {
			t.Fatalf("%s: the reader refuses this case: %v", name, err)
		}
		if err := gateActivityQuery(c.q, planNow); err != nil {
			t.Fatalf("%s: the reader refuses this case: %v", name, err)
		}
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			where, args, err := compileActivity(t.Context(), tx, c.q)
			if err != nil {
				return err
			}
			reads["the activity feed: "+name] = indexedRead{plannedRead{
				activityStatement(where), append(args, MaxActivityRows+1)},
				c.index, c.walk}
			return nil
		}); err != nil {
			t.Fatalf("%s: compile: %v", name, err)
		}
	}
	return reads
}

// historyPasses is every statement that reads the WHOLE task history: the
// re-derivations [Applier.Rederive] runs when a node's derivation rules
// differ from this build's, each a function of every row by definition.
func historyPasses() map[string]plannedRead {
	every, args := restampStatement("")
	return map[string]plannedRead{
		"the hand-off re-derivation": {rederiveHandOffsStatement, []any{string(KindTask)}},
		"the reopen re-derivation":   {reopensStatement, []any{string(KindTask)}},
		"every task's last change":   {every, args},
	}
}

// historyLiterals is every string literal of this package's own source that
// names `tracker_history` as a source of rows — after FROM, JOIN or UPDATE —
// which is every statement fragment that reads it.
//
// THE SOURCE RATHER THAN A LIST, for the reason [dutyReads] gives against a
// copy: a list beside the readers is one a new reader is added to by memory.
// A literal is matched whole, so a statement assembled from several literals
// is held by each of them.
func historyLiterals(t *testing.T) []string {
	t.Helper()
	reads := regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE)\s+tracker_history\b`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && reads.MatchString(value) {
					out = append(out, value)
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("no literal of this package reads tracker_history, so this walk " +
			"is reading nothing")
	}
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

// explain is the planner's account of one statement.
//
// A WRITE IS PLANNED IN A WRITE TRANSACTION, because the read path runs
// query_only, which refuses even the EXPLAIN of one. Nothing is executed
// either way.
func explain(t *testing.T, db store.ReplicatedHandle, statement string, args []any) []string {
	t.Helper()
	in := db.Read
	if words := strings.Fields(statement); len(words) > 0 &&
		(strings.EqualFold(words[0], "UPDATE") || strings.EqualFold(words[0], "DELETE")) {
		in = db.Tx
	}
	var plan []string
	if err := in(t.Context(), func(tx *sql.Tx) error {
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
//
// AN INTERSECTION NAMES SEVERAL: this engine plans two equality terms on two
// indexes as `MULTI-INDEX AND h (a, b)`, a seek on each and the row ids they
// share, so both are read.
func indexesIn(plan []string) []string {
	var found []string
	for _, line := range plan {
		if _, tail, ok := strings.Cut(line, "MULTI-INDEX AND "); ok {
			_, list, _ := strings.Cut(tail, "(")
			list, _, _ = strings.Cut(list, ")")
			for _, name := range strings.Split(list, ",") {
				found = append(found, strings.TrimSpace(name))
			}
			continue
		}
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
			set, err := compile(q, planNow, planFields(q))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			where, args := set.where, set.args
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

// THE TRASH IS READ THROUGH ITS OWN INDEX, BY EVERY STATEMENT OVER IT.
//
// A removed task is a fraction of the company and only the trash reads one, so
// a trash costs what is in it — never what the company holds. That is a claim
// about every statement an answer over the trash issues, not only about its
// page: measured before [taskSource] named the index, the workspace trash's
// page walked the old index but its count, its totals, a board's column counts
// and the subtree rollup's own subquery each read every task in the company
// from the heap (this engine's planner reaches a partial index only for an
// order it provides or a seek on its leading column, and the tombstone is
// neither), a project's trash sought the project's whole board through
// `tracker_tasks_rank_idx` at every level and sorted it, and a click on the
// Title column sorted the company.
//
// So each statement here is the reader's own ([taskRowsStatement],
// [cappedCountStatement], [totalsStatement], [groupCountsStatement]) over the
// set [compile] produces, at both scopes and in both subtask modes, and every
// line of its plan that reads the task table reads the scope's index — at
// every level, the rollup's subquery included. The page in the trash's own
// order is held to it unsorted as well, because that order is the index's and
// a LIMIT stops it after a page; a page ordered by something else may sort,
// and sorts the trash.
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
			t.Run(name+" sorted by "+cmpOr(sort, "its removal"), func(t *testing.T) {
				params := map[string]any{"removed": "true", "show_closed": "true",
					"totals": "tasks:count,points:sum", "group_by": "status"}
				for key, value := range c.params {
					params[key] = value
				}
				if sort != "" {
					params["sort"] = sort
				}
				q, err := ParseQuery(MapParams(params), planNow, time.UTC)
				if err != nil {
					t.Fatalf("ParseQuery: %v", err)
				}
				fields := planFields(q)
				set, err := compile(q, planNow, fields)
				if err != nil {
					t.Fatalf("compile: %v", err)
				}
				terms := sortTerms(q, fields)
				page, pageArgs := taskRowsStatement("", nil, set, terms)
				_, exprs, totalArgs, err := compileTotals(q.Totals, fields)
				if err != nil {
					t.Fatalf("compileTotals: %v", err)
				}
				axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
				if err != nil {
					t.Fatalf("compileGroup: %v", err)
				}
				columns, columnArgs := groupCountsStatement(axis, set)
				clause, values := axis.joinedFilter(string(StatusTodo))
				column, columnRowArgs := taskRowsStatement(axis.Join, axis.JoinArgs,
					set.and(clause, values), terms)
				statements := map[string]plannedRead{
					"the page": {page, append(pageArgs, 51)},
					"the count it carries": {cappedCountStatement(set.from, set.where),
						append(append([]any{}, set.args...), TotalHintCeiling+1)},
					"its totals": {totalsStatement(set, exprs),
						append(append([]any{}, set.args...), totalArgs...)},
					"a board's columns": {columns, append(columnArgs, MaxGroups+1)},
					"one column's rows": {column, append(columnRowArgs, 21)},
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
					if what == "the page" && sort == "" && sorts(plan) {
						t.Errorf("the page in the trash's own order sorts what "+
							"it read, so it is the whole trash rather than a "+
							"page:\n%s", shown)
					}
				}
			})
		}
	}
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
