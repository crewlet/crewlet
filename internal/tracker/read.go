package tracker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The reader, and the two facts it never conflates.
//
// `read_level` is HOW FRESH what was read is. `complete` is whether everything
// that should have been read was there to read. They are different questions
// with different answers, and folding them into one name is the mistake the
// pair exists to prevent: a set read is served at the level asked for FOR THE
// ROWS IT RETURNS, and makes no completeness claim at all whenever a deferred
// record's declared scope could intersect the question.
//
// The reason is precise rather than cautious. A filter tests columns —
// project, status, assignee, a field value, archived, removed — that live
// INSIDE a record this node could not decode, so a deferred create is not a
// stale row but an ABSENCE. Nothing about the rows that ARE returned says so.

// TaskRow is one row of an answer.
//
// A FIXED SHAPE rather than the whole task, because the answer is read by a
// model as often as by a screen and a body would put a task's 64 KiB into
// every row of a fifty-row page. What a caller needs beyond this is a
// single-task read, which is flat at any age.
type TaskRow struct {
	ID          string      `json:"id"`
	Key         string      `json:"key"`
	Title       string      `json:"title"`
	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group"`
	Priority    Priority    `json:"priority"`
	Assignee    string      `json:"assignee,omitempty"`
	Project     string      `json:"project"`
	Sprint      *int        `json:"sprint,omitempty"`
	Parent      string      `json:"parent,omitempty"`
	Depth       int         `json:"depth,omitempty"`
	Start       *time.Time  `json:"start,omitempty"`
	Due         *time.Time  `json:"due,omitempty"`

	// Overdue is the row's own copy of the preset's predicate, so a
	// renderer never re-derives it differently — which is how one screen
	// shows a task as overdue and another does not.
	Overdue bool `json:"overdue,omitempty"`

	EstimateMinutes int     `json:"estimate_min,omitempty"`
	Points          float64 `json:"points,omitempty"`

	// Blocked is DATA a filter and a badge read. It gates nothing: closing
	// a task with open blockers is allowed, and the facts a reviewer would
	// want are returned beside the status rather than enforced.
	Blocked  bool      `json:"blocked,omitempty"`
	Archived bool      `json:"archived,omitempty"`
	Rank     Rank      `json:"rank,omitempty"`
	Updated  time.Time `json:"updated"`
	Version  uint64    `json:"version"`
}

// Incomplete says what an answer could not account for.
type Incomplete struct {
	// Records is how many this node holds that it cannot decode and whose
	// scope could intersect the question.
	Records int `json:"records"`

	// From is where the lowest of them sits, which is the position a build
	// that can read them resumes at.
	From statelog.Position `json:"from"`

	// Scope names what is affected. It does NOT say in which direction,
	// and this build cannot compute the direction — that is what "cannot
	// decode" means.
	Scope []string `json:"scope"`

	// Version is the record version this node could not read, which is the
	// one number an operator needs to pick a build.
	Version int `json:"version"`
}

// Answer is one query's result and everything a caller needs to judge it.
type Answer struct {
	Rows []TaskRow `json:"rows"`

	// TotalHint is capped by construction, because an exact count over an
	// unbounded set is the query that turns a poll into a scan.
	TotalHint  int    `json:"total_hint"`
	NextCursor string `json:"next_cursor,omitempty"`

	// Totals are the aggregates the query asked for, over the WHOLE
	// matched set rather than over this page — a number on a header that
	// changed as somebody scrolled would be the one thing it must not do.
	Totals []Total `json:"totals,omitempty"`

	// Groups is the board's columns, and [Answer.Rows] is EMPTY whenever
	// it is set: returning both would be the same rows twice, and a
	// caller rendering the flat half would draw a board with no columns
	// and nothing to say so. Each group's own count is over the whole set
	// rather than over the rows it carries.
	Groups []Group `json:"groups,omitempty"`

	// GroupsDropped is how many columns did not fit [MaxGroups]. A board
	// that drew sixty-four of two hundred and said nothing would look
	// like a company with sixty-four assignees.
	GroupsDropped int `json:"groups_dropped,omitempty"`

	// GroupsOverlap marks an axis on which one task is on several columns
	// — a label board — so a reader knows the counts do not sum to
	// [Answer.TotalHint] rather than concluding the answer is wrong.
	GroupsOverlap bool `json:"groups_overlap,omitempty"`

	// View and Preset are what this answer was EXPANDED FROM, echoed so
	// the answer describes itself the way every other field here does.
	//
	// A request knows what it sent; an ANSWER does not otherwise, and this
	// domain's answers travel detached from their requests — a socket
	// frame, a cached payload, a screen restored from a URL. Without the
	// echo a board cannot say which saved view it is showing, and a
	// caller cannot tell an expansion that resolved from one that was
	// quietly dropped.
	View   string `json:"view,omitempty"`
	Preset string `json:"preset,omitempty"`

	// Level is the level ACTUALLY served, set from the statement that
	// satisfied the barrier — never the level asked for. A level never
	// silently downgrades, so the two can only differ by a refusal.
	Level statelog.ReadLevel `json:"read_level"`

	// LogSeq is this node's committed position and AppliedThrough the
	// prefix whose consequences it holds. Two numbers because a node
	// applying nothing while its position advances looks identical to one
	// that is caught up.
	LogSeq         uint64 `json:"log_seq"`
	AppliedThrough uint64 `json:"applied_through"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached: a read asks how far behind an answer may be, and an
	// unreachable broker answers "not at all".
	LogLag *uint64 `json:"log_lag,omitempty"`

	Complete   bool        `json:"complete"`
	Incomplete *Incomplete `json:"incomplete,omitempty"`
}

// TotalHintCeiling is where the count stops.
//
// Ten thousand, and the answer says "10000+" past it rather than counting on:
// an exact total over an unbounded set is the one query in this grammar that
// turns a sixty-second poll into a scan, and nobody reading a board needs the
// difference between eleven thousand and twelve.
const TotalHintCeiling = 10_000

// PageDefault and PageMax bound one page.
const (
	PageDefault = 50
	PageMax     = 500
)

// Reader answers queries from this node's own tables, AT THE LEVEL THEY ASK
// FOR.
//
// # The level is honoured here or it is honoured nowhere
//
// Every read goes through [statelog.Reader], which is what turns the level
// from a word in the answer into a property of it: the refusal ladder first
// (an evicted node, one below the trim floor, one whose applier has stopped
// serves nothing), then the coverage probe, then the freshness target — a
// quorum-committed barrier for `linearizable`, the caller's own high-water
// mark for `session`, a declared bound for `stale` — and only then one read
// transaction whose first statement is the coverage probe again.
//
// This reader USED TO OPEN ITS OWN TRANSACTION and set `answer.Level` from the
// query, which made the level a label rather than a guarantee: a seat tool
// asking for `session` got whatever this node happened to hold, and
// `max_lag_seconds` bounded nothing at all.
type Reader struct {
	db  *store.DB
	log *statelog.Reader
}

// NewReader builds the read surface over a node's replicated estate.
//
// The framework reader is REQUIRED rather than optional. A nil one would mean
// a build where every level silently degrades to whatever the local rows say,
// which is the state this type is being moved out of — and a degradation that
// is invisible in the answer is worse than a refusal.
func NewReader(db *store.DB, log *statelog.Reader) (*Reader, error) {
	if db == nil {
		return nil, fmt.Errorf("tracker: a reader needs a store")
	}
	if log == nil {
		return nil, fmt.Errorf("tracker: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than a " +
			"guarantee, which is silent at every surface that renders one")
	}
	return &Reader{db: db, log: log}, nil
}

// Tasks answers one query.
//
// ONE READ TRANSACTION for the rows, the count and the coverage probe — which
// is what stops the total describing a different snapshot from the rows it
// counts, and stops a completeness claim being made against state the rows
// were not read from.
func (r *Reader) Tasks(ctx context.Context, q Query, now time.Time) (Answer, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = PageDefault
	}
	if limit > PageMax {
		limit = PageMax
	}
	if q.Level == "" {
		// ABSENT IS NOT A FOURTH STATE and it is not a default either —
		// it resolves to the SURFACE's own, which is what makes the
		// default a property of the surface rather than of whichever
		// caller happened to omit the key. So an unset level is a
		// PROGRAMMING error here rather than a quietly-degraded read:
		// both surfaces reached this function with one unset, and the
		// answer they got carried the empty string as its level.
		return Answer{}, fmt.Errorf("tracker: this read names no level — a " +
			"surface resolves an absent read_level to its own default (a seat " +
			"tool linearizable, a dashboard poll stale) before it reads")
	}

	answer := Answer{View: q.View, Preset: q.Preset}
	// A SET READ, and that word decides what a deferred scope does to it.
	// A point read refuses, because the one object it is about may be
	// stale; a set read cannot enumerate what would have ENTERED the set —
	// a deferred create is an absence with no local row — so it is served
	// at the level asked for and makes no completeness claim.
	served, err := r.log.Read(ctx, statelog.Query{
		Level:           q.Level,
		Scope:           ReadScope(q),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		// THE DECLARATIONS FIRST, and inside this transaction: an
		// `f.<ref>` filter resolves against the catalogue, and a
		// resolution read outside the read's own snapshot could answer
		// from a catalogue the rows below were never filtered against.
		fields, err := resolveFields(ctx, tx, q)
		if err != nil {
			return err
		}
		// AND THE PRIORITY LIST, inside the same transaction and for the
		// same reason: it lives on another object, so a parser that read
		// it would be a parser that could fail on a store — and a list
		// read outside this snapshot could name a task the rows below
		// were never filtered against.
		if q.PriorityListOf != "" {
			q.PriorityList, err = readPriorityList(ctx, tx, q.PriorityListOf)
			if err != nil {
				return err
			}
		}
		where, args, err := compile(q, now, fields)
		if err != nil {
			return err
		}
		// THE ORDER IS COMPILED AFTER THE RESOLUTION TOO, because a
		// `sort=f.<slug>` term is a JOIN onto the field's own values and
		// the field is only known once the catalogue has been read.
		terms := sortTerms(q, fields)
		// AND THE PAGE BOUNDARY IS THE ROW READ'S ALONE — see
		// [pageClause]. The hint and the totals are about the whole
		// matched set, and neither carries a sort join.
		page, pageArgs, err := pageClause(q, fields)
		if err != nil {
			return err
		}
		rowWhere, rowArgs := andPage(where, args, page, pageArgs)
		if q.GroupBy != "" {
			// A GROUPED ANSWER IS A DIFFERENT SHAPE, and the flat
			// rows stay empty — see [Answer.Groups].
			// A GROUPED ANSWER MINTS NO CURSOR AND TAKES NONE —
			// see [Answer.Groups] — so it reads the unpaged
			// predicate, which is also what its per-column counts
			// are over.
			groups, err := readGroups(ctx, tx, q, fields, where, args, terms)
			if err != nil {
				return err
			}
			answer.Groups = groups.Groups
			answer.GroupsDropped = groups.Dropped
			answer.GroupsOverlap = groups.Overlap
		} else {
			rows, cursor, err := readTasks(ctx, tx, rowWhere, rowArgs, terms,
				limit, q.DayStart)
			if err != nil {
				return err
			}
			answer.Rows, answer.NextCursor = orderByList(rows, q), cursor
		}

		hint, err := countHint(ctx, tx, where, args)
		if err != nil {
			return err
		}
		answer.TotalHint = hint

		// THE SAME PREDICATE AND THE SAME TRANSACTION as the rows, so a
		// total and the page it sits above describe one instant.
		totals, exprs, totalArgs, err := compileTotals(q.Totals, fields)
		if err != nil {
			return err
		}
		answer.Totals, err = readTotals(ctx, tx, where, args, totals, exprs, totalArgs)
		if err != nil {
			return err
		}

		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return Answer{}, err
	}
	// THE FRAMEWORK'S OWN VERDICT, never the query's. `Level` here is what
	// the read was SERVED at, which is the level asked for or a refusal —
	// and assigning the request's level to it, which is what this function
	// did, is exactly how a level becomes a label.
	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// incompleteFrom renders the framework's coverage gap in this domain's own
// shape, which is what the answer's JSON has always carried.
func incompleteFrom(in *statelog.Incomplete) *Incomplete {
	if in == nil {
		return nil
	}
	return &Incomplete{
		Records: int(in.Records), From: in.From,
		Scope: in.Scope.Closure(),
	}
}

// compile turns a parsed query into a predicate and its arguments.
//
// EVERY VALUE IS BOUND, never interpolated: the only thing this function
// composes into SQL is its own column and operator names, from closed sets
// declared in the grammar. A filter's value reaching a statement as text is
// how a saved view becomes a way to run a query nobody wrote.
func compile(q Query, now time.Time, fields map[string]resolvedField) (string, []any, error) {
	return compileWhere(q, now, fields, false)
}

// compileWhere is [compile], with `branch` telling it whether it is compiling
// one arm of a disjunction rather than the query itself.
//
// # Why a branch is not simply the same compiler run again
//
// A branch is a PREDICATE, and four of the decisions this function makes are
// not predicates the caller wrote — they are the shape of the ANSWER, applied
// by default whether or not anybody asked: a removed task is excluded, a
// finished one is excluded, an archived project's tasks are excluded, and a
// subtask mode rewrites the whole predicate to be about roots.
//
// Run again inside a branch, each of those emits its DEFAULT and is ANDed
// inside the OR — so `show_closed=true` at the top level was defeated by every
// branch's own `status_group IN (not_started, active)`, and the identical
// predicate written as a disjunction answered a strictly smaller set than the
// one written flat. [Query.parseAny] refuses those keys in a branch for the
// same reason; this is the other half, because refusing the key does not stop
// the default the absent key resolves to.
//
// The scope IS a predicate and stays: a branch naming a container is asking
// about that container's rows, which is exactly what the unassigned-work arm
// of a personal queue is for.
func compileWhere(q Query, now time.Time, fields map[string]resolvedField,
	branch bool) (string, []any, error) {

	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}

	// THE SCOPE IS KEPT SEPARATELY as well as added, because the subtask
	// rollup below replaces the whole predicate with a subquery and the
	// container has to survive that — see the rewrite for what it costs
	// when it does not.
	scope, scopeArgs := "", []any(nil)
	switch {
	case q.Scope.Project != "":
		scope, scopeArgs = "t.project_key = ?", []any{q.Scope.Project}
		add(scope, scopeArgs...)
	case q.Scope.Workspace:
		// The Everything level, and it is explicit: no predicate.
	}

	// EVERY QUERY EXCLUDES A REMOVED TASK except the ones that are about
	// them. ONE predicate for a removed task's whole life, at any age —
	// which is what makes a restore at any age answer exactly what it
	// answered before.
	//
	// `removed=true` is the OTHER side of it and the only way to reach the
	// trash: a removal hides a task, and a restore is a gesture somebody
	// makes about a task they can see. It is `IS NOT NULL` rather than a
	// date bound so `tracker_tasks_removed_idx`, which is partial on
	// exactly that, is the plan.
	if !branch {
		if q.Removed != nil && *q.Removed {
			add("t.removed_at IS NOT NULL")
		} else {
			add("t.removed_at IS NULL")
		}
	}

	if len(q.Keys) > 0 {
		add("t.key IN ("+placeholders(len(q.Keys))+")", anyOf(q.Keys)...)
	}
	if q.PriorityListOf != "" {
		if len(q.PriorityList) == 0 {
			// AN EMPTY LIST IS AN EMPTY ANSWER, stated rather than
			// omitted: dropping the clause would answer every task in
			// scope as though it were on somebody's list.
			add("0")
		} else {
			add("t.id IN ("+placeholders(len(q.PriorityList))+")",
				anyOf(q.PriorityList)...)
		}
	}

	if len(q.Status) > 0 {
		add("t.status IN ("+placeholders(len(q.Status))+")", anyOf(q.Status)...)
	}
	if len(q.StatusNot) > 0 {
		add("t.status NOT IN ("+placeholders(len(q.StatusNot))+")", anyOf(q.StatusNot)...)
	}
	if len(q.StatusGroups) > 0 {
		add("t.status_group IN ("+placeholders(len(q.StatusGroups))+")",
			anyOf(q.StatusGroups)...)
	}
	if len(q.Priorities) > 0 {
		add("t.priority IN ("+placeholders(len(q.Priorities))+")",
			anyOf(q.Priorities)...)
	}
	if len(q.Types) > 0 {
		add("t.type IN ("+placeholders(len(q.Types))+")", anyOf(q.Types)...)
	}
	if len(q.Unit) > 0 {
		add("t.filed_unit IN ("+placeholders(len(q.Unit))+")", anyOf(q.Unit)...)
	}
	if len(q.RoutingUnit) > 0 {
		add("t.routing_unit IN ("+placeholders(len(q.RoutingUnit))+")",
			anyOf(q.RoutingUnit)...)
	}
	if handles := q.Assignee; len(handles) > 0 {
		// `none` is a VALUE rather than a missing filter: "unassigned"
		// is a real question and the commonest half of the queue preset.
		clause, values := handleClause("t.assignee", handles)
		add(clause, values...)
	}
	if len(q.Reporter) > 0 {
		clause, values := handleClause("t.reporter", q.Reporter)
		add(clause, values...)
	}
	for _, join := range []struct {
		handles []string
		table   string
	}{
		{q.Collaborator, "tracker_collaborators"},
		{q.Watcher, "tracker_watchers"},
	} {
		if len(join.handles) == 0 {
			continue
		}
		add("EXISTS (SELECT 1 FROM "+join.table+" x WHERE x.task_id = t.id "+
			"AND x.handle IN ("+placeholders(len(join.handles))+"))",
			anyOf(join.handles)...)
	}
	if len(q.ChecklistAssignee) > 0 {
		add("EXISTS (SELECT 1 FROM tracker_checklist_items x "+
			"WHERE x.task_id = t.id AND x.assignee IN ("+
			placeholders(len(q.ChecklistAssignee))+"))", anyOf(q.ChecklistAssignee)...)
	}
	if len(q.Tags.Tags) > 0 {
		clause, values := tagClause(q.Tags)
		add(clause, values...)
	}
	if q.Parent != "" {
		add("t.parent_id = ?", q.Parent)
	}
	if q.Root != "" {
		add("EXISTS (SELECT 1 FROM tracker_task_closure c "+
			"WHERE c.descendant_id = t.id AND c.ancestor_id = ?)", q.Root)
	}
	if q.Batch != "" {
		add("t.batch_id = ?", q.Batch)
	}
	if q.References != "" {
		add("EXISTS (SELECT 1 FROM tracker_references r "+
			"JOIN tracker_task_keys k ON k.task_id = r.to_task "+
			"WHERE r.from_task = t.id AND k.key = ?)", q.References)
	}
	if q.LinkedPage != "" {
		add("EXISTS (SELECT 1 FROM tracker_relations x WHERE x.task_id = t.id "+
			"AND x.kind = ? AND x.other_id = ?)", string(RelationPage), q.LinkedPage)
	}
	if q.Goal != "" {
		// BOTH REFERENCE KINDS, because a target names TASKS AND
		// PROJECTS and the goal's own progress counts a task reached
		// either way (see [targetTasks]). Matching task refs alone made
		// `goal=<id>` answer nothing for a goal whose target is a
		// project while that same goal scored over every task in it —
		// two readings of one row set, disagreeing silently.
		add("EXISTS (SELECT 1 FROM tracker_goal_target_refs g "+
			"WHERE g.goal_id = ? AND ((g.kind = 'task' AND g.ref = t.id) "+
			"OR (g.kind = 'project' AND g.ref = t.project_key)))", q.Goal)
	}
	if q.Blocked != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.task_id = t.id AND d.blocker_open = 1)"
		if !*q.Blocked {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasChildren != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_tasks c WHERE c.parent_id = t.id)"
		if !*q.HasChildren {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasParent != nil {
		if *q.HasParent {
			add("t.parent_id IS NOT NULL")
		} else {
			add("t.parent_id IS NULL")
		}
	}
	if q.HasOpenAsks != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = t.id " +
			"AND m.ask <> '' AND m.resolved = 0 AND m.answered_by IS NULL AND m.removed = 0)"
		if !*q.HasOpenAsks {
			clause = "NOT " + clause
		}
		add(clause)
	}
	// THE TWO ENDS OF AN OPEN ASK, and they are different questions: what
	// is waiting on me, and what I am waiting for. Both narrow the same
	// predicate [Query.HasOpenAsks] uses, because an ask that is resolved
	// or answered is nobody's queue.
	if q.AskedOf != "" {
		add("EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = t.id "+
			"AND m.ask = ? AND m.resolved = 0 AND m.answered_by IS NULL "+
			"AND m.removed = 0)", q.AskedOf)
	}
	if q.AskedBy != "" {
		add("EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = t.id "+
			"AND m.ask <> '' AND m.author = ? AND m.resolved = 0 "+
			"AND m.answered_by IS NULL AND m.removed = 0)", q.AskedBy)
	}
	// BLOCKING IS THE OTHER END OF BLOCKED, not its negation: "this task
	// is holding something up" reads the dependency table by BLOCKER_ID
	// where blocked reads it by task_id, and a task can be both.
	if q.Blocking != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.blocker_id = t.id AND d.blocker_open = 1)"
		if !*q.Blocking {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasDependencies != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d WHERE d.task_id = t.id)"
		if !*q.HasDependencies {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.Text != "" {
		// THE FIND, and it is a substring of a KEY or a TITLE — what
		// the tool that carries it promises, and the only thing this
		// grammar can honestly do: ranked search over the company's
		// prose is `search_knowledge`'s, and nothing has ever put a task
		// in the inverted list.
		//
		// THE KEY MATCHES FROM THE FRONT and the title anywhere. A key
		// is `PROJECT-N`, so it is only ever typed from its start —
		// while unanchored, every bare number in a find becomes a key
		// match: `2` would hand back ENG-2, ENG-12 and ENG-20 beside
		// whatever the person actually meant. A title is prose, where
		// the useful match is in the middle.
		pattern := likeEscape(q.Text)
		add(`(t.key LIKE ? ESCAPE '\' OR t.title LIKE ? ESCAPE '\')`,
			pattern+"%", "%"+pattern+"%")
	}

	for _, filter := range q.Fields {
		field, held := fields[filter.Ref]
		if !held {
			// UNREACHABLE THROUGH [Reader.Tasks], which refuses an
			// unresolved ref before it reaches here — and a refusal
			// rather than a skip anyway, because a filter nobody
			// resolved is a board showing more than the person asked
			// for, silently.
			return "", nil, fmt.Errorf("tracker: f.%s was never resolved, so "+
				"this answer would be wider than the caller asked for",
				filter.Ref)
		}
		clause, values, err := fieldClause(filter, field)
		if err != nil {
			return "", nil, err
		}
		add(clause, values...)
	}
	if len(q.Sprint) > 0 {
		clause, values, err := sprintClause(q)
		if err != nil {
			return "", nil, err
		}
		add(clause, values...)
	}
	for column, filter := range q.Dates {
		clause, values, err := dateClause(column, filter)
		if err != nil {
			return "", nil, err
		}
		add(clause, values...)
		if filter.Overdue {
			// THE ALIAS CARRIES ITS OPEN CONDITION, which is what makes
			// it the same predicate as the preset that means the same
			// thing rather than a second one that drifts.
			add("t.status_group IN ('not_started','active')")
		}
	}
	for column, filter := range map[string]*NumFilter{
		"t.estimate_min": q.Estimate, "t.points": q.Points,
		"t.spend_tokens": q.Spend,
	} {
		if filter == nil {
			continue
		}
		clause, values := numClause(column, *filter)
		add(clause, values...)
	}
	if len(q.Flags) > 0 {
		// THE INDEX'S OWN PREDICATE, stated because it is what makes the
		// index reachable. The attention index is partial over the four
		// flags together — a task with any of them is a fraction of a
		// percent of the table — and `cycle = 1` alone does not imply
		// that disjunction to a planner, so a flag filter without it
		// reads every task in the company.
		add(anyFlagSet)
	}
	for _, flag := range q.Flags {
		column, ok := flagColumn(flag)
		if !ok {
			return "", nil, fmt.Errorf("tracker: %q is not an attention flag", flag)
		}
		add(column + " = 1")
	}

	switch {
	case branch:
		// THE ANSWER'S SHAPE, NOT THIS ARM'S — see [compileWhere].
	case q.Archived == ArchivedExclude:
		// A JOIN RATHER THAN A COLUMN: a project's archive is the
		// project's own fact, and copying it onto every task was the one
		// unbounded cross-object write this design removed.
		add("t.archived = 0 AND NOT EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.archived = 1)")
	case q.Archived == ArchivedOnly:
		add("(t.archived = 1 OR EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.archived = 1))")
	}

	switch {
	case branch:
		// THE ANSWER'S SHAPE, NOT THIS ARM'S — see [compileWhere].
	case q.ShowClosed.All:
	case q.ShowClosed.Recent > 0:
		// THE WINDOW APPLIES TO THE WHOLE FINISHED SET, so a task
		// cancelled inside it is in the answer exactly as one done
		// inside it is — which is what the finish stamp being by GROUP
		// buys, and why the board's Cancelled column is not empty.
		add("(t.status_group IN ('not_started','active') OR "+
			"(t.finished_at IS NOT NULL AND t.finished_at >= ?))",
			store.EncodeTime(now.Add(-q.ShowClosed.Recent)))
	default:
		add("t.status_group IN ('not_started','active')")
	}

	if len(q.Any) > 0 {
		var branches []string
		for i, arm := range q.Any {
			clause, values, err := compileWhere(arm, now, fields, true)
			if err != nil {
				return "", nil, err
			}
			if clause == "" {
				// AN EMPTY ARM IS "EVERYTHING", which makes the whole
				// disjunction true and every other arm decoration. It
				// is refused rather than rendered, because `()` is not
				// SQL and "you selected nothing" is a thing a caller
				// fixes.
				return "", nil, fmt.Errorf("tracker: any branch %d selects "+
					"nothing — a branch is a predicate, and an empty one "+
					"matches every task and makes the other branches "+
					"decoration", i)
			}
			branches = append(branches, "("+clause+")")
			args = append(args, values...)
		}
		// ONE LEVEL, ANDed with the top-level keys — which is what makes
		// a disjunction a filter rather than a second query.
		where = append(where, "("+strings.Join(branches, " OR ")+")")
	}

	// THE SUBTASK MODE, and it is a predicate on the ROOT rather than on
	// the row.
	//
	// `collapsed` and `expanded` filter ROOT tasks and let their subtrees
	// ride along unfiltered; `separate` filters every task on its own.
	// That is [SubtaskMode]'s own contract, and the difference between the
	// first two is a RENDER hint — both answer the same set, and the
	// caller folds or does not.
	//
	// WRITTEN AS A SELF-SCOPED SUBQUERY, with the predicate built so far
	// repeated inside it. The inner `tracker_tasks t` shadows the outer
	// alias, so every `t.` in that predicate binds to the inner row and
	// nothing has to be rewritten — which is what makes this possible at
	// all without an alias-parameterised compiler.
	//
	// AND IT DOES NOT APPLY WHEN THE CALLER ASKED FOR A SUBTREE: `parent`
	// and `root` are questions ABOUT subtasks, so filtering their roots
	// would answer the parent's siblings instead.
	if !branch && q.Subtasks != SubtasksSeparate && q.Parent == "" &&
		q.Root == "" && len(where) > 0 {

		inner := strings.Join(where, " AND ")
		rooted := "t.root_id IN (SELECT t.id FROM tracker_tasks t WHERE " +
			inner + ")"
		// THE ROW'S OWN TOMBSTONE STILL APPLIES: a removed subtask must
		// not ride along on a live root, and the predicate above tests
		// the ROOT's.
		// THE ARGUMENTS ARE UNCHANGED: every placeholder built so far
		// moves INTO the subquery, and the two clauses that replace them
		// out here bind nothing.
		//
		// AND THE ROW'S OWN TOMBSTONE IS THE QUERY'S OWN, not a constant.
		// A removed subtask must not ride along on a live root, which is
		// what this clause is for — but spelled `IS NULL` unconditionally
		// it also excluded every row from the TRASH, whose whole
		// predicate is the opposite one. `removed=true` in the default
		// subtask mode therefore answered nothing at all, which is the
		// one query that has to work for a removal to be reversible.
		tomb := "t.removed_at IS NULL"
		if q.Removed != nil && *q.Removed {
			tomb = "t.removed_at IS NOT NULL"
		}
		where = []string{tomb}
		if scope != "" {
			// AND THE CONTAINER SURVIVES THE REWRITE.
			//
			// Without it the outer row carried no scope at all, so
			// EVERY container-scoped query in the default subtask
			// mode — which is every board — was a scan of every task
			// in the COMPANY, filtered afterwards by the subquery.
			// The planner has nothing else to enter on out here: the
			// only other outer predicate is a tombstone every row
			// shares.
			//
			// It narrows no legitimate answer either. A subtree lives
			// in one project — a cross-project move takes the
			// descendants with it — and a subtask whose project
			// differs from its root's is the `inconsistent_project`
			// anomaly the attention set exists to surface, not a
			// shape a board should quietly return rows from another
			// container for.
			//
			// THE ARGUMENT IS PREPENDED, because placeholders bind in
			// textual order and this clause now precedes the subquery
			// every other argument moved into.
			where = append(where, scope)
			args = append(append([]any{}, scopeArgs...), args...)
		}
		where = append(where, rooted)
	}

	if q.Group != "" && !branch {
		// A COLUMN FILTER IS A PREDICATE OF THE WHOLE QUERY, in its
		// JOIN-FREE form: the count hint and the totals share this
		// predicate and carry no join, so an axis expressed only as one
		// would leave a header adding up the whole board while the rows
		// showed a single column of it.
		axis, err := compileGroup(q.GroupBy, fields)
		if err != nil {
			return "", nil, err
		}
		clause, values := axis.filter(q.Group)
		add(clause, values...)
	}

	return strings.Join(where, " AND "), args, nil
}

// pageClause is the keyset resume, and it is DELIBERATELY NOT PART OF
// [compile].
//
// A cursor says where this PAGE starts. The count hint and the totals are
// about the whole matched set, and they share the compiled predicate — so a
// cursor folded into it made page two's header report the sum of page two
// ONWARDS. A number on a header that changes as somebody pages is the same
// failure as one that changes as they scroll, which is what the totals were
// built to avoid.
//
// It is also the only clause that can name a SORT JOIN's alias: a
// `sort=f.<slug>` cursor compares `fs0.num`, and the hint and the totals carry
// no join at all — so a cursor in the shared predicate made page two of every
// custom-field-sorted query a hard error.
func pageClause(q Query, fields map[string]resolvedField) (string, []any, error) {
	if q.Cursor == "" {
		return "", nil, nil
	}
	return cursorClause(q, fields)
}

// andPage folds the page boundary into a predicate for the ROW read alone.
func andPage(where string, args []any, clause string, values []any) (string, []any) {
	if clause == "" {
		return where, args
	}
	return where + " AND " + clause, append(append([]any{}, args...), values...)
}

// handleClause builds a handle filter, with `none` as a real value.
func handleClause(column string, handles []string) (string, []any) {
	var named []any
	none := false
	for _, handle := range handles {
		if handle == "none" {
			none = true
			continue
		}
		named = append(named, handle)
	}
	switch {
	case none && len(named) > 0:
		return "(" + column + " = '' OR " + column + " IN (" +
			placeholders(len(named)) + "))", named
	case none:
		return column + " = ''", nil
	}
	return column + " IN (" + placeholders(len(named)) + ")", named
}

// tagClause builds the three tag modes.
func tagClause(filter TagFilter) (string, []any) {
	args := make([]any, 0, len(filter.Tags)+1)
	for _, tag := range filter.Tags {
		args = append(args, tag)
	}
	exists := "EXISTS (SELECT 1 FROM tracker_task_tags x WHERE x.task_id = t.id " +
		"AND x.slug IN (" + placeholders(len(filter.Tags)) + "))"
	switch filter.Mode {
	case TagNone:
		return "NOT " + exists, args
	case TagAll:
		// EVERY tag, which is a count rather than a membership: a task
		// carrying two of three is not a match, and an IN test cannot
		// say so.
		return "(SELECT COUNT(DISTINCT x.slug) FROM tracker_task_tags x " +
				"WHERE x.task_id = t.id AND x.slug IN (" +
				placeholders(len(filter.Tags)) + ")) = ?",
			append(args, len(filter.Tags))
	}
	return exists, args
}

// dateColumns maps a grammar key to the column it filters.
//
// `status_entered` reads the EFFECTIVE instant because it is the start of a
// duration; the rest read the AUTHORED one, because each is a wall-clock bound
// the caller typed and a clamp would answer a different question from the one
// on the screen.
var dateColumns = map[string]string{
	"due": "t.due_at", "start": "t.start_at", "created": "t.created_at",
	"updated": "t.updated_at", "done": "t.done_at", "closed": "t.closed_at",
	"finished": "t.finished_at", "status_entered": "t.status_entered_at",
}

func dateClause(key string, filter DateFilter) (string, []any, error) {
	column, ok := dateColumns[key]
	if !ok {
		return "", nil, fmt.Errorf("tracker: %q names no date column", key)
	}
	from := store.EncodeTime(filter.From.At)

	// THE NULL PREDICATE IS SPELLED OUT, and it is not redundant.
	//
	// Every date column here is indexed PARTIALLY — `WHERE due_at IS NOT
	// NULL` — because most tasks have no due date and an index over the
	// whole table would be mostly empty entries. A partial index is usable
	// only when the query IMPLIES its predicate, and this engine's planner
	// does not infer `x IS NOT NULL` from `x < ?`: without the clause the
	// index its own comment names is never reached and every date filter
	// reads the table. Measured by TestEveryIndexServesARegisteredQuery,
	// which is why it is a clause here rather than a sentence in the DDL.
	notNull := column + " IS NOT NULL AND "
	switch filter.Op {
	case DateLT:
		return notNull + column + " < ?", []any{from}, nil
	case DateLTE:
		return notNull + column + " <= ?", []any{from}, nil
	case DateGT:
		return notNull + column + " > ?", []any{from}, nil
	case DateGTE:
		return notNull + column + " >= ?", []any{from}, nil
	case DateRange:
		// HALF-OPEN, which is what makes "this week" contain every
		// instant of Sunday.
		return notNull + column + " >= ? AND " + column + " < ?",
			[]any{from, store.EncodeTime(filter.To.At)}, nil
	}
	return "", nil, fmt.Errorf("tracker: %q is not a date comparison", filter.Op)
}

// sprintClause compiles `sprint=` — the one filter whose values are words as
// often as numbers.
//
// # A TASK CAN BE IN MORE THAN ONE SPRINT
//
// That is what a carry-over IS, so membership is an EXISTS over the STAY table
// rather than a column on the task: a column could name only the sprint a task
// is in now, and every sprint report is about the ones it was in.
//
// # The words, and why a number alone was not a grammar
//
// `active`, `future` and `closed` are STATES, resolved against the project's
// own sprint rows; `next` is the earliest future one, which is a different
// question from "any future one" and the one a person means. A bare number or
// a sprint NAME names one sprint. `none` is the BACKLOG — not in any sprint —
// and it is the value that had no spelling at all, which is why the implicit
// Backlog view could not be expressed and carried a comment saying so. Values
// are ORed, because that is what a csv filter means here and what "this sprint
// or the next" asks for.
//
// # Why a sprint needs a project
//
// "active" is a fact about ONE project's sprints, and at workspace scope there
// are as many active sprints as there are projects. A NUMBER is the same: they
// are minted per project, so `sprint=4` across a company names several. Both
// are refused there naming the key that fixes it, rather than silently
// answering about every project at once. `none` is the exception and needs no
// project, because "in no sprint" means the same thing everywhere.
func sprintClause(q Query) (string, []any, error) {
	project := q.Scope.Project
	var states, numbers, names []any
	backlog := false
	for _, value := range q.Sprint {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none", "backlog":
			backlog = true
		case string(SprintActive), string(SprintFuture), string(SprintClosed), "next":
			states = append(states, strings.ToLower(strings.TrimSpace(value)))
		default:
			if number, err := strconv.Atoi(value); err == nil {
				numbers = append(numbers, number)
				continue
			}
			names = append(names, value)
		}
	}
	if project == "" && len(states)+len(numbers)+len(names) > 0 {
		return "", nil, fmt.Errorf("tracker: sprint=%s names a sprint, and "+
			"sprints are numbered and named PER PROJECT — so at company scope "+
			"it names one in each. Scope the query with container=project:<key>, "+
			"or ask for sprint=none, which is the backlog everywhere",
			strings.Join(q.Sprint, ","))
	}

	// THE PROJECT IS ON THE STAY ROW, and naming it is what puts the
	// query on `tracker_task_sprints_sprint_idx` — `(project_key, sprint,
	// task_id)`, which the DDL ships for exactly this. Without it the
	// EXISTS drives on `task_id` and takes the primary key, so a sprint
	// filter walked one row per task rather than seeking the sprint.
	member := "EXISTS (SELECT 1 FROM tracker_task_sprints s " +
		"WHERE s.task_id = t.id"
	var scoped []any
	if project != "" {
		member += " AND s.project_key = ?"
		scoped = []any{project}
	}
	var branches []string
	var args []any
	if len(numbers) > 0 {
		branches = append(branches,
			member+" AND s.sprint IN ("+placeholders(len(numbers))+"))")
		args = append(args, scoped...)
		args = append(args, numbers...)
	}
	for _, state := range states {
		clause, values := sprintStateClause(member, state.(string), project)
		branches = append(branches, clause)
		args = append(args, scoped...)
		args = append(args, values...)
	}
	if len(names) > 0 {
		branches = append(branches, member+
			" AND EXISTS (SELECT 1 FROM tracker_sprints p "+
			"WHERE p.project_key = ? AND p.number = s.sprint "+
			"AND p.name IN ("+placeholders(len(names))+")))")
		args = append(args, scoped...)
		args = append(args, project)
		args = append(args, names...)
	}
	if backlog {
		// THE BACKLOG IS AN ABSENCE OF AN OPEN STAY, not an absence of
		// every stay: a task pulled back out of sprint 4 is in the
		// backlog again, and its closed stay is history rather than
		// membership.
		branches = append(branches, "NOT "+member+" AND s.to_at IS NULL)")
		args = append(args, scoped...)
	}
	return "(" + strings.Join(branches, " OR ") + ")", args, nil
}

// sprintStateClause is one sprint STATE as a predicate on the stay table.
//
// The state lives on the SPRINT row and membership on the stay, so every one
// of these is a join between the two — which is what makes `sprint=active`
// keep meaning "the sprint that is active now" as sprints open and close,
// rather than freezing whichever number was active when somebody saved a view.
func sprintStateClause(member, state, project string) (string, []any) {
	if state == "next" {
		// THE EARLIEST FUTURE SPRINT, by number: they are minted in
		// order, so the smallest future number is the one after this.
		// An archived one is excluded — it is a sprint nobody will run.
		return member + " AND s.sprint = (SELECT MIN(p.number) " +
				"FROM tracker_sprints p WHERE p.project_key = ? " +
				"AND p.state = ? AND p.archived = 0))",
			[]any{project, string(SprintFuture)}
	}
	return member + " AND EXISTS (SELECT 1 FROM tracker_sprints p " +
			"WHERE p.project_key = ? AND p.number = s.sprint AND p.state = ?))",
		[]any{project, state}
}

func numClause(column string, filter NumFilter) (string, []any) {
	switch filter.Op {
	case NumLT:
		return column + " < ?", []any{filter.From}
	case NumLTE:
		return column + " <= ?", []any{filter.From}
	case NumGT:
		return column + " > ?", []any{filter.From}
	case NumGTE:
		return column + " >= ?", []any{filter.From}
	case NumRange:
		return column + " >= ? AND " + column + " <= ?", []any{filter.From, filter.To}
	case NumNull:
		return column + " = 0", nil
	case NumNotNull:
		return column + " <> 0", nil
	}
	return "1", nil
}

// flagColumn maps an attention flag to its column, from a CLOSED SET — the one
// place a grammar value reaches a statement as a name rather than a bound
// value, and therefore the one that has to be enumerated.
// anyFlagSet is the attention index's own partial predicate, written once so
// the DDL and the query cannot disagree about what it is.
const anyFlagSet = `(t.inconsistent_project = 1 OR t.cycle = 1 OR ` +
	`t.too_deep = 1 OR t.key_collision = 1)`

func flagColumn(flag string) (string, bool) {
	switch flag {
	case "cycle":
		return "t.cycle", true
	case "too_deep":
		return "t.too_deep", true
	case "inconsistent_project":
		return "t.inconsistent_project", true
	case "key_collision":
		return "t.key_collision", true
	}
	return "", false
}

// orderBy compiles the sort, always ending in the id.
//
// THE TRAILING ID IS WHAT MAKES TWO NODES RENDER ONE BOARD IN ONE ORDER even
// if a duplicate rank ever occurs — and it is free, because the three
// rank-bearing indexes carry it as a trailing column and still serve the range
// scan with no sort step.
// sortTerm is one column of the order, with the direction it is read in.
//
// THE ORDER IS A VALUE, not a string, because the page cursor is derived from
// it: a keyset resume has to compare exactly the columns the order sorts by,
// in exactly their directions, and a rendered string cannot be taken apart
// again without parsing SQL.
type sortTerm struct {
	Column     string
	Descending bool

	// Join is the LEFT JOIN a custom-field sort needs, empty for a plain
	// column. LEFT because a field the task never set must still appear:
	// an inner join would silently drop every task with no value, which
	// is a sort that also filters.
	Join string

	// JoinArgs are the join's bound values, which ride AHEAD of the
	// predicate's: a join is written before the WHERE.
	JoinArgs []any
}

// sortColumns is what a caller may order by.
var sortColumns = map[string]string{
	"rank": "t.rank", "updated": "t.updated_at", "due": "t.due_at",
	"priority": "t.prio_rank", "created": "t.created_at",
	"title": "t.title", "estimate": "t.estimate_min", "points": "t.points",
	"spend": "t.spend_tokens", "status_entered": "t.status_entered_at",
	// THE TRASH'S OWN ORDER, and the only sort key that is about a row's
	// removal rather than about its work. It sorts NULLS anywhere, because
	// the only query that names it is one already filtered to removed rows.
	"removed": "t.removed_at",
}

// sortTerms compiles the sort, ALWAYS ENDING IN THE ID.
//
// The id is the tiebreak that makes a page stable: two tasks with one rank, or
// one update instant, would otherwise come back in whatever order the storage
// felt like, and a page boundary between them would drop one and repeat the
// other on every poll.
func sortTerms(q Query, fields map[string]resolvedField) []sortTerm {
	var terms []sortTerm
	for i, sort := range q.Sort {
		if ref, ok := strings.CutPrefix(sort.Key, FieldKeyPrefix); ok && ref != "" {
			field, held := fields[ref]
			if !held {
				continue
			}
			// ONE ALIAS PER TERM, numbered, because two field sorts
			// in one order are two joins and a shared alias would
			// make the second silently re-use the first's field.
			//
			// AND `seq = 0` PINS IT TO ONE ROW: a labels field has
			// several, and a join that matched them all would
			// multiply every task by its own value count — a sort
			// that duplicates rows.
			alias := "fs" + strconv.Itoa(i)
			terms = append(terms, sortTerm{
				Column:     alias + "." + FieldValueColumn(field.Type),
				Descending: sort.Descending,
				Join: " LEFT JOIN tracker_field_values " + alias +
					" ON " + alias + ".task_id = t.id AND " + alias +
					".field_id = ? AND " + alias + ".hidden = 0 AND " +
					alias + ".kind <> '" + FieldValueForeign + "' AND " +
					alias + ".seq = 0",
				JoinArgs: []any{field.ID},
			})
			continue
		}
		column, known := sortColumns[sort.Key]
		if !known {
			continue
		}
		terms = append(terms, sortTerm{Column: column, Descending: sort.Descending})
	}
	if len(terms) == 0 {
		switch {
		case q.Removed != nil && *q.Removed:
			// THE TRASH IS ORDERED BY WHEN IT WAS REMOVED, never by
			// rank: a removed task's rank is its position on a board
			// it is no longer on, so ordering a trash listing by it
			// is ordering by a stale number — and the only index over
			// removed rows is the partial one on this column, so the
			// board default also made the listing a heap scan.
			terms = append(terms, sortTerm{
				Column: "t.removed_at", Descending: true,
			})
		case q.Scope.Project != "":
			// The default is the manual order inside a project and
			// the most recently touched everywhere else, because a
			// rank is only an order within the container that owns
			// it.
			terms = append(terms, sortTerm{Column: "t.rank"})
		default:
			terms = append(terms, sortTerm{Column: "t.updated_at", Descending: true})
		}
	}
	return append(terms, sortTerm{Column: "t.id"})
}

// orderBy renders a query's sort as SQL.
func orderBy(q Query, fields map[string]resolvedField) string {
	return renderOrder(sortTerms(q, fields))
}

// sortJoins is the join clause and arguments a compiled order needs.
func sortJoins(terms []sortTerm) (string, []any) {
	var clause strings.Builder
	var args []any
	for _, term := range terms {
		if term.Join == "" {
			continue
		}
		clause.WriteString(term.Join)
		args = append(args, term.JoinArgs...)
	}
	return clause.String(), args
}

// renderOrder renders compiled sort terms as SQL.
func renderOrder(terms []sortTerm) string {
	rendered := make([]string, 0, len(terms))
	for _, term := range terms {
		if term.Descending {
			rendered = append(rendered, term.Column+" DESC")
			continue
		}
		rendered = append(rendered, term.Column)
	}
	return strings.Join(rendered, ", ")
}

// page is what a cursor carries: the last row's value for EVERY column the
// order sorts by, ending in its id — and the ORDER those values belong to.
//
// THE ORDER TRAVELS BECAUSE THE ARITY IS NOT THE ORDER. Two different sorts of
// equal length both pass a count check, and the cursor's values are then bound
// against columns of a different type — which SQLite resolves by affinity
// rather than refusing, so the boundary is meaningless in both directions
// rather than an error: an `updated` cursor fed to `sort=rank` returns rank's
// own first page again, and a `rank` cursor fed to `sort=updated` compares TEXT
// against INTEGER, which orders every row after it, so every page is the first.
type page struct {
	Keys []any `json:"k"`

	// Order is the compiled order this cursor was minted under. Compiled
	// rather than the caller's `sort=` spelling, because the default order
	// depends on the scope and two queries that named no sort at all can
	// still be sorted differently.
	Order string `json:"o,omitempty"`
}

// cursorClause turns an opaque page cursor back into a predicate.
//
// KEYSET RATHER THAN OFFSET, because an offset re-reads and re-sorts every row
// it skips: page fifty of a board costs fifty times page one, and a row
// inserted between two polls shifts every page after it.
//
// # Why it compares the WHOLE order and not just the id
//
// The predicate has to be "after the last row IN THIS ORDER", and for any
// order but by-id that is not `id > last`. On a board ordered by rank, every
// task with a later rank and a smaller id is SKIPPED — it sorts after the page
// boundary and fails the predicate — while every task with an earlier rank and
// a larger id is REPEATED on every subsequent page. Both at once, silently: a
// caller paging a board would see some of its tasks twice and never see
// others, and nothing in the answer would say so.
//
// So the comparison is lexicographic over the sort terms, expanded rather than
// written as a row value because the directions differ: for `a ASC, b DESC,
// id ASC` it is `a > ? OR (a = ? AND (b < ? OR (b = ? AND id > ?)))`. The
// nesting is the definition of "later in this order" and nothing shorter is
// correct for a mixed-direction sort.
func cursorClause(q Query, fields map[string]resolvedField) (string, []any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil {
		return "", nil, fmt.Errorf("tracker: the cursor is not one this surface "+
			"minted: %w", err)
	}
	var resume page
	if err := json.Unmarshal(raw, &resume); err != nil {
		return "", nil, fmt.Errorf("tracker: the cursor names no row to resume " +
			"after")
	}
	terms := sortTerms(q, fields)
	order := renderOrder(terms)
	if len(resume.Keys) != len(terms) || resume.Order != order {
		// A CURSOR FROM A DIFFERENT ORDER IS REFUSED, never applied to
		// this one: resuming a rank-ordered page inside an
		// update-ordered query is a page boundary computed against a
		// column the query does not sort by, which drops rows without
		// saying so. The ORDER is what decides that, not the key count —
		// see [page].
		return "", nil, fmt.Errorf("tracker: this cursor was minted for the "+
			"order %q and this query sorts by %q — a cursor belongs to the "+
			"order that minted it, and re-sorting starts a new page",
			resume.Order, order)
	}
	clause, args := keysetAfter(terms, resume.Keys)
	return clause, args, nil
}

// keysetAfter builds the lexicographic "strictly after" predicate.
//
// # NULL IS A POSITION IN THE ORDER, not a missing value
//
// `sort=due` and every `sort=f.<slug>` reach columns that are genuinely NULL
// for some rows — the custom-field join is a LEFT JOIN precisely so a task that
// set no value still appears — and a comparison written with bare `>`, `<` and
// `=` is NULL for every one of them. Written that way the boundary matched
// nothing the moment a page ended on a row with no value, so the caller was
// handed a short list with no next_cursor and no way to tell; and in the
// descending direction the NULL rows were unreachable at every page, because
// `col < ?` excludes them.
//
// SQLite sorts NULL below every value, so it leads an ascending order and
// trails a descending one, and each direction needs both halves spelled out:
//
//   - ascending, after a value: `col > ?` — the NULLs are already behind.
//   - ascending, after a NULL: `col IS NOT NULL` — every value is ahead, and
//     the remaining NULL rows are reached through the equality leg.
//   - descending, after a value: `col < ? OR col IS NULL` — the NULLs are the
//     tail, so they are still ahead.
//   - descending, after a NULL: nothing is ahead but other NULLs, so the
//     "after" leg is false and only the tie-break carries the page.
//
// The equality leg is `IS NULL` rather than `= NULL` for the same reason, and
// the recursion always bottoms out on `t.id`, which is NOT NULL.
func keysetAfter(terms []sortTerm, keys []any) (string, []any) {
	after, afterArgs := keysetAfterOne(terms[0], keys[0])
	if len(terms) == 1 {
		return after, afterArgs
	}
	equal, equalArgs := keysetEqualOne(terms[0], keys[0])
	rest, restArgs := keysetAfter(terms[1:], keys[1:])
	args := append(append([]any{}, afterArgs...), equalArgs...)
	args = append(args, restArgs...)
	return "(" + after + " OR (" + equal + " AND " + rest + "))", args
}

// keysetAfterOne is "strictly after this key on this column", NULLs included.
func keysetAfterOne(term sortTerm, key any) (string, []any) {
	switch {
	case key == nil && term.Descending:
		// NOTHING IS AFTER A NULL IN A DESCENDING ORDER, and `0` says so
		// literally rather than through a comparison that would be NULL.
		return "0", nil
	case key == nil:
		return term.Column + " IS NOT NULL", nil
	case term.Descending:
		return "(" + term.Column + " < ? OR " + term.Column + " IS NULL)",
			[]any{key}
	}
	return term.Column + " > ?", []any{key}
}

// keysetEqualOne is the tie-break leg: the same position on this column.
func keysetEqualOne(term sortTerm, key any) (string, []any) {
	if key == nil {
		return term.Column + " IS NULL", nil
	}
	return term.Column + " = ?", []any{key}
}

// mintCursor encodes the page's own resume point.
func mintCursor(keys []any, order string) string {
	body, err := json.Marshal(page{Keys: keys, Order: order})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func readTasks(ctx context.Context, tx *sql.Tx, where string, args []any,
	terms []sortTerm, limit int, dayStart time.Time) ([]TaskRow, string, error) {

	return readTasksJoined(ctx, tx, "", nil, where, args, terms, limit, dayStart)
}

// readTasksJoined is the same with one more join in front.
//
// A GROUPED ANSWER'S COLUMN IS THE ORDINARY STATEMENT narrowed to one value,
// so it takes the same sort, the same row shape and the same cursor mint —
// with the grouping axis's own join, which the predicate that selects the
// column refers to.
func readTasksJoined(ctx context.Context, tx *sql.Tx, extraJoin string,
	extraArgs []any, where string, args []any, terms []sortTerm, limit int,
	dayStart time.Time) ([]TaskRow, string, error) {

	// THE SORT COLUMNS ARE SELECTED TOO, because the page cursor is their
	// values: a keyset resume compares exactly the columns the order sorts
	// by, and a row whose sort value was never read cannot be resumed
	// after. They are scanned as opaque values — nothing here needs to
	// know what a rank or an instant IS, only what the next page must be
	// strictly after.
	keys := make([]string, 0, len(terms))
	for _, term := range terms {
		keys = append(keys, term.Column)
	}

	// ONE MORE THAN THE PAGE, which is how the answer knows whether there
	// is another page without a second count.
	// THE JOINS COME FIRST IN THE STATEMENT AND SO DO THEIR ARGUMENTS: a
	// custom-field sort is a LEFT JOIN written before the WHERE, and a
	// placeholder is bound in statement order rather than by name.
	joins, joinArgs := sortJoins(terms)
	// THE EXTRA JOIN COMES FIRST, with its arguments, because the
	// predicate below may refer to it and a placeholder binds in
	// statement order.
	joins = extraJoin + joins
	joinArgs = append(append([]any{}, extraArgs...), joinArgs...)
	query := `SELECT t.id, t.key, t.title, t.status, t.status_group, t.priority,
	                 t.assignee, t.project_key, t.sprint_number, t.parent_id,
	                 t.depth, t.start_at, t.due_at, t.estimate_min, t.points,
	                 t.archived, t.rank, t.updated_at, t.version,
	                 EXISTS (SELECT 1 FROM tracker_task_deps d
	                         WHERE d.task_id = t.id AND d.blocker_open = 1), ` +
		strings.Join(keys, ", ") + `
	          FROM tracker_tasks t` + joins + `
	          WHERE ` + where + `
	          ORDER BY ` + renderOrder(terms) + `
	          LIMIT ?`
	bound := append(append([]any{}, joinArgs...), args...)
	rows, err := tx.QueryContext(ctx, query, append(bound, limit+1)...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRow
	var pageKeys [][]any
	for rows.Next() {
		var row TaskRow
		var sprint, parent, start, due sql.NullInt64
		var parentID sql.NullString
		var archived, blocked int
		var updated int64
		var version int64
		sortValues := make([]any, len(terms))
		targets := []any{&row.ID, &row.Key, &row.Title, &row.Status,
			&row.StatusGroup, &row.Priority, &row.Assignee, &row.Project,
			&sprint, &parentID, &row.Depth, &start, &due, &row.EstimateMinutes,
			&row.Points, &archived, &row.Rank, &updated, &version, &blocked}
		for i := range sortValues {
			targets = append(targets, &sortValues[i])
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, "", fmt.Errorf("tracker: read a task row: %w", err)
		}
		pageKeys = append(pageKeys, sortValues)
		_ = parent
		if sprint.Valid {
			n := int(sprint.Int64)
			row.Sprint = &n
		}
		if parentID.Valid {
			row.Parent = parentID.String
		}
		if start.Valid {
			at := store.DecodeTime(start.Int64)
			row.Start = &at
		}
		if due.Valid {
			at := store.DecodeTime(due.Int64)
			row.Due = &at
		}
		row.Archived = archived == 1
		row.Blocked = blocked == 1
		row.Updated = store.DecodeTime(updated)
		row.Version = uint64(version)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: walk the task rows: %w", err)
	}
	cursor := ""
	if len(out) > limit {
		out = out[:limit]
		// THE ORDER TRAVELS WITH THE KEYS — see [page].
		cursor = mintCursor(pageKeys[limit-1], renderOrder(terms))
	}
	// AND THE OVERDUE FLAG IS DERIVED ONCE, here, so every renderer agrees
	// — against the QUERY's own day boundary rather than against this
	// process's clock. The two are different predicates: `due=overdue`
	// compiles to "before midnight today, in the company's zone", and a
	// flag compared against `time.Now()` marked a task due at 09:00 today
	// overdue from 09:01 while the filter that means the same thing
	// excluded it all day. A renderer showing both then contradicted
	// itself on the same row, which is the one thing this field exists to
	// prevent.
	for i := range out {
		out[i].Overdue = out[i].Due != nil &&
			out[i].StatusGroup.Open() && out[i].Due.Before(dayStart)
	}
	return out, cursor, nil
}

// countHint counts to the ceiling and stops.
func countHint(ctx context.Context, tx *sql.Tx, where string, args []any) (int, error) {
	query := `SELECT COUNT(*) FROM (SELECT 1 FROM tracker_tasks t WHERE ` +
		where + ` LIMIT ?)`
	var n int
	if err := tx.QueryRowContext(ctx, query,
		append(append([]any{}, args...), TotalHintCeiling+1)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("tracker: count the answer: %w", err)
	}
	return n, nil
}

// readCheckpoint reads this node's own position and applied prefix.
func readCheckpoint(ctx context.Context, tx *sql.Tx) (position, applied uint64, err error) {
	var generation, seq int64
	err = tx.QueryRowContext(ctx,
		`SELECT generation, seq FROM statelog_cursor WHERE stream = ?`,
		trackerStream).Scan(&generation, &seq)
	switch {
	case err == sql.ErrNoRows:
		return 0, 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("tracker: read the checkpoint: %w", err)
	}
	packed := uint64(generation)*uint64(statelog.GenerationStride) + uint64(seq)

	// APPLIED THROUGH is the lowest deferred position minus one, or the
	// checkpoint when this node holds none. Two numbers, because a node
	// applying nothing while its position advances looks identical to one
	// that is caught up.
	var lowest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(position) FROM tracker_log_deferred`).Scan(&lowest); err != nil {
		return 0, 0, fmt.Errorf("tracker: read the deferred floor: %w", err)
	}
	if lowest.Valid && uint64(lowest.Int64) > 0 {
		return packed, uint64(lowest.Int64) - 1, nil
	}
	return packed, packed, nil
}

// coverage answers whether this node holds a record it cannot decode whose
// scope could intersect the question.
//
// IT NAMES WHAT IS AFFECTED AND NOT IN WHICH DIRECTION, because this build
// cannot compute the direction — that is what "cannot decode" means. Rows may
// be missing, rows that should have left may still be present, and values on
// the rows returned may be behind.
func coverage(ctx context.Context, tx *sql.Tx, q Query) (*Incomplete, error) {
	return coverageOf(ctx, tx, ReadScope(q))
}

// coverageOf is the same probe over a scope the caller built.
//
// SPLIT FROM THE QUERY, because a detail read's scope is one OBJECT and a
// board's is a container — and running a board's probe for a single task
// would put a permanent warning on every task in a company holding one
// undecodable record about one other task.
func coverageOf(ctx context.Context, tx *sql.Tx, scope statelog.ScopeSet) (*Incomplete, error) {
	closure := scope.Closure()
	roots := scope.Roots()
	if len(closure) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(closure)+len(roots)*2)
	for _, path := range closure {
		args = append(args, path)
	}
	query := `SELECT COUNT(*), MIN(d.position), MIN(d.version)
	          FROM tracker_log_deferred_scope s
	          JOIN tracker_log_deferred d ON d.position = s.position
	          WHERE s.path IN (` + placeholders(len(closure)) + `)`
	for _, root := range roots {
		query += ` OR s.path = ? OR s.path LIKE ? ESCAPE '\'`
		args = append(args, root, store.LikePrefix(root+statelog.ScopeSeparator))
	}
	var count int
	var lowest, version sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, args...).
		Scan(&count, &lowest, &version); err != nil {
		return nil, fmt.Errorf("tracker: probe the answer's coverage: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return &Incomplete{
		Records: count,
		From: statelog.Position{
			Stream:     trackerStream,
			Generation: uint32(lowest.Int64 / statelog.GenerationStride),
			Seq:        uint64(lowest.Int64 % statelog.GenerationStride),
		},
		Scope:   scope.Paths,
		Version: int(version.Int64),
	}, nil
}

// anyOf widens a typed slice for a bound IN list.
func anyOf[T ~string](values []T) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}

// trackerStream is the domain's stream name, read from the declaration rather
// than written again — one spelling, so the framework and every reader compare
// against the same one.
var trackerStream = Domain{}.Stream().Name

// placeholders is a bound-parameter list of n slots.
//
// THE ONLY THING THIS PACKAGE COMPOSES INTO SQL besides its own column names:
// a value never reaches a statement as text, so a filter cannot become a way
// to run a query nobody wrote.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// readPriorityList reads one person's stored order.
//
// A MISSING PERSON IS AN EMPTY LIST, not an error: everybody starts without a
// row, so "no row" is the ordinary state of every human on their first day and
// every seat for ever — and refusing would make a turn-start read fail on a
// seat that had simply never been given a priority.
func readPriorityList(ctx context.Context, tx *sql.Tx, handle string) ([]string, error) {
	person, held, err := readPerson(ctx, tx, handle)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return person.Priorities, nil
}

// orderByList restores a stored order SQL cannot express.
//
// THE ORDER IS THE CONTENT of a priority list — it is what somebody decided —
// and there is no column to sort by: the sequence lives in a JSON array on
// another object, and a keyset cursor over a CASE expression built from it
// would be a page boundary that changed every time the list was reordered.
// The list is bounded at [MaxPriorities], so this is a re-order of at most
// thirty-two rows.
//
// A ROW THE LIST DOES NOT NAME KEEPS ITS PLACE at the end, because a caller
// that combined `priorities=` with another filter still asked for those rows.
func orderByList(rows []TaskRow, q Query) []TaskRow {
	if q.PriorityListOf == "" || len(q.PriorityList) == 0 || len(rows) == 0 {
		return rows
	}
	at := make(map[string]int, len(q.PriorityList))
	for i, id := range q.PriorityList {
		at[id] = i
	}
	slices.SortStableFunc(rows, func(a, b TaskRow) int {
		x, inA := at[a.ID]
		y, inB := at[b.ID]
		switch {
		case inA && inB:
			return x - y
		case inA:
			return -1
		case inB:
			return 1
		}
		return 0
	})
	return rows
}
