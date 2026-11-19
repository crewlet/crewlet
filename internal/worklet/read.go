package worklet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/keyset"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The reader, and the two facts it never conflates.
//
// `read_level` is HOW FRESH what was read is; `complete` is whether everything
// that should have been read was there. A set read is served at the level asked
// for FOR THE ROWS IT RETURNS, and claims no completeness whenever a deferred
// record's declared scope could intersect the question: a filter tests columns
// inside a record this node could not decode, so a deferred create is an
// ABSENCE, which nothing about the returned rows reveals.

// TaskRow is one row of an answer.
//
// A FIXED SHAPE rather than the whole task: a model reads answers as often as
// a screen does, and a body would put 64 KiB into every row of a page. More
// than this is a single-task read.
type TaskRow struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	// KeyCollision marks a task holding a key ANOTHER task claimed first:
	// a key resolves to its claimant ([resolveTaskID]), so this row's key
	// opens somebody else's task; [TaskRow.Address] reaches this one.
	//
	// THE COLUMN, never re-derived here: [Applier.maintainKeys] derives it
	// on every apply that moves the directory, so it is the same on every
	// data node and follows the claim through a purge.
	KeyCollision bool `json:"key_collision,omitempty"`

	Title string `json:"title" untrusted:"name"`

	// Type is the slug a card is drawn under, on the ROW because a board
	// draws an icon per card and a column per type; otherwise each card
	// would need a document read.
	Type string `json:"type"`

	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group"`
	Priority    Priority    `json:"priority"`
	Assignee    string      `json:"assignee,omitempty"`
	Project     string      `json:"project"`
	Parent      string      `json:"parent,omitempty"`
	Depth       int         `json:"depth,omitempty"`
	Start       *time.Time  `json:"start,omitempty"`
	Due         *time.Time  `json:"due,omitempty"`

	// Overdue is the row's copy of the preset's predicate, so no renderer
	// re-derives it differently.
	Overdue bool `json:"overdue,omitempty"`

	EstimateMinutes int     `json:"estimate_min,omitempty"`
	Points          float64 `json:"points,omitempty"`

	// Blocked is DATA a filter and a badge read. It gates nothing: closing
	// a task with open blockers is allowed.
	Blocked bool `json:"blocked,omitempty"`

	// WaitingOn is the edges [TaskRow.Blocked] summarises, naming WHICH
	// task holds this one up. On the ROW because a renderer cannot derive
	// a relation from either end without a read per bar. Both facts come
	// from one statement, so `Blocked` is exactly "some entry here is
	// Open" ([TestTheBlockedFlagIsTheEdgesItCarries]).
	//
	// EVERY edge, not only open ones: a timeline that dropped a cleared
	// edge would redraw its history every time a blocker closed.
	WaitingOn []Blocker `json:"waiting_on,omitempty"`

	Archived bool      `json:"archived,omitempty"`
	Rank     Rank      `json:"rank,omitempty"`
	Updated  time.Time `json:"updated"`
	Version  uint64    `json:"version"`

	// Flow is why an item in progress is or is not moving ([Flow]), read
	// in the page's own statement; null on any other item.
	Flow *Flow `json:"flow"`

	// THE OPT-IN FACTS, each ABSENT unless named in `fields=` ([RowField]).
	// When asked for, each is PRESENT even at zero, so "0" is told rather
	// than left ambiguous: hence `omitzero` on a slice that is non-nil and
	// empty when asked, and pointers for the numbers.
	Tags            []string  `json:"tags,omitzero"`
	DependentsCount *int      `json:"dependents_count,omitempty"`
	OpenAsks        *int      `json:"open_asks,omitempty"`
	Spend           *RowSpend `json:"spend,omitempty"`
}

// RowSpend is what one task has cost: the applier's running totals ([Spend]),
// narrowed to the five a row draws. Tokens only — nothing here is a price.
type RowSpend struct {
	Tokens   int `json:"tokens"`
	Turns    int `json:"turns"`
	Workers  int `json:"workers"`
	SentBack int `json:"sent_back"`
	Reopens  int `json:"reopens"`
}

// Address is the reference that opens this row's task. See [ItemAddress].
func (r TaskRow) Address() string { return ItemAddress(r.ID, r.Key, r.KeyCollision) }

// ItemAddress is the one rule for what a caller hands back to open a task: its
// key, unless the key is not this task's to answer to, and then its id.
//
// THE KEY BY DEFAULT, because it is what a person reads and types. THE ID WHEN
// ANOTHER TASK CLAIMED THE KEY FIRST (`key_collision`), because the key
// resolves to its claimant and would never reach this task; and the id when
// there is no key.
//
// The dashboard's `itemAddress` is a copy of this rule; keep the two equal.
func ItemAddress(id, key string, collision bool) string {
	if key == "" || collision {
		return id
	}
	return key
}

// ItemNamed is how PROSE names a task to somebody who may act on the name: its
// address ([ItemAddress]), with the key beside an id that stands in for it.
//
// THE KEY TOO, because an id alone says nothing about which task it is, and
// SAYING WHY, so a reader does not take the id for a fault and go back to the
// key, which opens the claimant.
//
// A tool's ARGUMENT is never built from this: it takes the address alone.
func ItemNamed(id, key string, collision bool) string {
	address := ItemAddress(id, key, collision)
	if key == "" || address == key {
		return address
	}
	return address + " (its key " + key + " opens another task)"
}

// Blocker is one dependency edge as the task that waits on it sees it.
//
// THE STATE TRAVELS WITH THE EDGE, because the blocker is routinely not on the
// renderer's page, so there would be nothing to look it up from.
type Blocker struct {
	// ID is the blocking task's, never its key: rows are matched on
	// [TaskRow.ID]. A blocker the caller's filter excluded is an id with no
	// row on the page, which is the honest answer.
	ID string `json:"id"`

	// Key is the blocking task's key, so a page holding no row for the
	// blocker can still name it. Empty for an edge to something that is
	// not a task (a pull request's address).
	Key string `json:"key,omitempty"`
	// KeyCollision is [TaskRow.KeyCollision] for the blocker: its key is one
	// another task claimed first, so a link to it goes by [Blocker.ID].
	KeyCollision bool `json:"key_collision,omitempty"`

	// Open is whether the blocker still holds this task up. FALSE is a
	// settled fact, the blocker has finished, so it is always stated: a
	// reader cannot tell an absent value from either one.
	Open bool `json:"open"`

	// OneSided is the blocker not listing this task back: a dependency
	// gesture whose mirror did not land, which the repair duty is still
	// working on. DERIVED from the other end's rows, not a record.
	OneSided bool `json:"one_sided,omitempty"`

	// OneSidedFinal is the repair duty's DECISION that this edge will
	// never be mirrored, as opposed to "not mirrored yet".
	OneSidedFinal bool `json:"one_sided_final,omitempty"`

	// Dead is a change the task waits on that nothing moves: open, with no
	// reviewer asked, no review of its head and no merge asked (ADR-0128).
	Dead bool `json:"dead,omitempty"`
}

// Incomplete says what an answer could not account for.
type Incomplete struct {
	// Records is how many this node holds that it cannot decode and whose
	// scope could intersect the question.
	Records int `json:"records"`

	// From is the lowest one's position, where a build that reads them
	// resumes.
	From statelog.Position `json:"from"`

	// Scope names what is affected, not in which direction: this build
	// cannot decode the records to know.
	Scope []string `json:"scope"`

	// Version is the record version this node could not read: what an
	// operator needs to pick a build.
	Version int `json:"version"`
}

// Answer is one query's result and everything a caller needs to judge it.
type Answer struct {
	Rows []TaskRow `json:"rows"`

	// TotalHint stops at [TotalHintCeiling], and TotalCapped makes that
	// VISIBLE, so no renderer needs the ceiling to tell "exactly ten
	// thousand" from "more". The hint is clamped TO the ceiling when
	// capped, so a reader ignoring the flag is imprecise rather than wrong.
	TotalHint   int    `json:"total_hint"`
	TotalCapped bool   `json:"total_capped,omitempty"`
	NextCursor  string `json:"next_cursor,omitempty"`

	// Totals are the asked aggregates over the WHOLE matched set, not this
	// page, so a header does not change as somebody scrolls.
	Totals []Total `json:"totals,omitempty"`

	// Groups is the board's columns, and [Answer.Rows] is EMPTY whenever
	// it is set, so no caller draws the flat half as a column-less board.
	// Each group's count is over the whole set, not the rows it carries.
	Groups []Group `json:"groups,omitempty"`

	// GroupsDropped is how many columns did not fit [MaxGroups], so a
	// truncated board says so.
	GroupsDropped int `json:"groups_dropped,omitempty"`

	// GroupsOverlap marks an axis on which one task is on several columns
	// (a label board), so the counts do not sum to [Answer.TotalHint].
	GroupsOverlap bool `json:"groups_overlap,omitempty"`

	// View and Preset are what this answer was EXPANDED FROM, echoed
	// because answers travel detached from their requests (a socket frame,
	// a cache, a URL): without them a board cannot name its saved view or
	// tell a resolved expansion from a dropped one.
	View   string `json:"view,omitempty"`
	Preset string `json:"preset,omitempty"`

	// Level is the level ACTUALLY served, never the one asked for. A level
	// never silently downgrades, so the two differ only by a refusal.
	Level statelog.ReadLevel `json:"read_level"`

	// LogSeq is this node's committed position and AppliedThrough the
	// prefix whose consequences it holds: two numbers, because a stalled
	// applier otherwise looks caught up.
	LogSeq         uint64 `json:"log_seq"`
	AppliedThrough uint64 `json:"applied_through"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached, so an unreachable broker never reads as "not behind".
	LogLag *uint64 `json:"log_lag,omitempty"`

	Complete   bool        `json:"complete"`
	Incomplete *Incomplete `json:"incomplete,omitempty"`

	// FlowLive says the rows' flow read the seats' live facts ([Posture]):
	// false on a node whose source is not seeded, where no row reads
	// working or a seat's stop.
	FlowLive bool `json:"flow_live"`

	// Around is where [Query.Around]'s task sits in this answer's drawing
	// order, and NIL when the caller asked and the task is not in it — or
	// did not ask, which the caller knows. See [Around].
	Around *Around `json:"around,omitempty"`
}

// TotalHintCeiling is where the count stops.
//
// Past it the answer reports the ceiling with [Answer.TotalCapped] set: an
// exact total over an unbounded set turns a sixty-second poll into a scan, and
// no board needs eleven thousand told from twelve.
//
// THE FLAG IS THE CONTRACT, not the number: comparing the hint to this
// constant reads an exact ten thousand as capped.
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
// Every read goes through [statelog.Reader], which makes the level a property
// of the answer rather than a label: the refusal ladder, the coverage probe,
// the freshness target, then one read transaction. A reader opening its own
// transaction would serve whatever this node holds and bound no lag.
type Reader struct {
	db       store.ReplicatedReader
	log      *statelog.Reader
	postures Postures
}

// NewReader builds the read surface over a node's replicated estate.
//
// The framework reader is REQUIRED: without it every level silently degrades
// to whatever the local rows say, which is worse than a refusal. The postures
// are this node's live facts for the items' flow ([Postures]); nil reads every
// flow without them, and each answer says so (`flow_live: false`).
func NewReader(db store.ReplicatedReader, log *statelog.Reader, postures Postures) (*Reader, error) {
	if db.IsZero() {
		return nil, fmt.Errorf("worklet: a reader needs a store")
	}
	if log == nil {
		return nil, fmt.Errorf("worklet: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than a " +
			"guarantee, which is silent at every surface that renders one")
	}
	return &Reader{db: db, log: log, postures: postures}, nil
}

// Tasks answers one query.
//
// ONE READ TRANSACTION for the rows, the count and the coverage probe, so the
// total and the completeness claim describe the snapshot the rows came from.
func (r *Reader) Tasks(ctx context.Context, q Query, now time.Time) (Answer, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = PageDefault
	}
	if limit > PageMax {
		limit = PageMax
	}
	if q.Level == "" {
		// An absent level resolves to the SURFACE's own default before
		// it gets here, so an unset one is a PROGRAMMING error, never a
		// quietly-degraded read.
		return Answer{}, fmt.Errorf("worklet: this read names no level; a " +
			"surface resolves an absent read_level to its own default (a seat " +
			"tool linearizable, a dashboard poll stale) before it reads")
	}

	at := rowsAt{dayStart: q.DayStart, flow: r.flowAt(now)}
	answer := Answer{View: q.View, Preset: q.Preset, FlowLive: at.flow.live()}
	// A SET READ: under a deferred scope a point read refuses, but a set
	// read cannot enumerate what would have ENTERED the set, so it is
	// served at the level asked for and claims no completeness.
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     ReadScope(q),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		// THE DECLARATIONS FIRST, inside this transaction, so an
		// `f.<ref>` filter resolves against the catalogue of the
		// snapshot the rows are read from.
		fields, err := resolveFields(ctx, tx, q)
		if err != nil {
			return err
		}
		// THE PRIORITY LIST, here for the same reason, and not in the
		// parser, which must not read a store.
		if q.PriorityListOf != "" {
			q.PriorityList, err = readPriorityList(ctx, tx, q.PriorityListOf)
			if err != nil {
				return err
			}
		}
		set, err := compile(q, now, fields)
		if err != nil {
			return err
		}
		// THE ORDER IS COMPILED AFTER THE RESOLUTION, because a
		// `sort=f.<slug>` term joins the field's values; a derived
		// order's basis is read once for page, cursor and neighbours
		// ([resolveOrder]).
		basis, err := resolveOrder(ctx, tx, q)
		if err != nil {
			return err
		}
		terms := sortTerms(q, fields, basis)
		// THE PAGE BOUNDARY IS THE ROW READ'S ALONE ([pageClause]),
		// except under a priority list, which SQL cannot resume and
		// [readListOrdered] pages itself.
		listed := listOrdered(q)
		var resume []keyset.Arm
		if !listed {
			resume, err = pageClause(q, fields, basis)
			if err != nil {
				return err
			}
		}
		// The list-ordered set in drawn order, kept for [readAround].
		var listedRows []TaskRow
		switch {
		case q.GroupBy != "":
			// A GROUPED ANSWER leaves the flat rows empty and mints
			// no cursor ([Answer.Groups]), so it reads the unpaged
			// predicate its per-column counts are over.
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			groups, err := readGroups(ctx, tx, q, fields, set, terms, at)
			if err != nil {
				return err
			}
			answer.Groups = groups.Groups
			answer.GroupsDropped = groups.Dropped
			answer.GroupsOverlap = groups.Overlap
		case listed:
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			all, _, err := readTasks(ctx, tx, set, terms,
				MaxPriorities, at)
			if err != nil {
				return err
			}
			listedRows = orderByList(all, q)
			answer.Rows, answer.NextCursor, err = pageListed(listedRows,
				q.Cursor, limit)
			if err != nil {
				return err
			}
		default:
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			rows, cursor, err := readPage(ctx, tx, set, resume, terms,
				limit, at)
			if err != nil {
				return err
			}
			answer.Rows, answer.NextCursor = rows, cursor
		}
		// THE OPT-IN ROW FACTS ([RowField]), over every row the answer
		// carries, in this transaction so they match the rows' snapshot.
		if err = fillRowFields(ctx, tx, q, answerRows(&answer)); err != nil {
			return err
		}

		hint, capped, err := countHint(ctx, tx, set)
		if err != nil {
			return err
		}
		answer.TotalHint, answer.TotalCapped = hint, capped

		// WHERE ONE TASK SITS, after the count its total reads.
		if q.Around != "" {
			answer.Around, err = readAround(ctx, tx, q, fields, set,
				terms, &answer, listedRows)
			if err != nil {
				return err
			}
		}

		// THE SAME PREDICATE AND THE SAME TRANSACTION as the rows, so a
		// total and the page it sits above describe one instant.
		totals, exprs, totalArgs, err := compileTotals(q.Totals, fields)
		if err != nil {
			return err
		}
		answer.Totals, err = readTotals(ctx, tx, set, totals, exprs, totalArgs)
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
	// THE FRAMEWORK'S VERDICT, never the query's: the level SERVED, so it
	// is a guarantee rather than a label.
	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// incompleteFrom renders the framework's coverage gap in this domain's shape.
func incompleteFrom(in *statelog.Incomplete) *Incomplete {
	if in == nil {
		return nil
	}
	return &Incomplete{
		Records: int(in.Records), From: in.From,
		Scope: in.Scope.Closure(), Version: in.Version,
	}
}

// compile turns a parsed query into the set of tasks it reads: where they are
// read from, the predicate that keeps them, and its arguments.
//
// EVERY VALUE IS BOUND, never interpolated: only column and operator names
// from the grammar's closed sets are composed into SQL, so a saved view can
// never run a query nobody wrote.
func compile(q Query, now time.Time, fields map[string]resolvedField) (taskSet, error) {
	where, args, err := compileWhere(q, now, fields)
	if err != nil {
		return taskSet{}, err
	}
	return taskSet{from: taskSource(q), where: where, args: args}, nil
}

// taskSet is a compiled query's tasks: the source every statement over them
// reads from ([taskSource]) and the predicate that keeps them.
//
// ONE VALUE, because source and predicate are one decision shared by every
// statement over an answer (page, count, totals, columns, around): a statement
// spelling its own `FROM tracker_tasks t` could read the trash predicate from
// the heap, the scan [taskSource] prevents.
type taskSet struct {
	// from is the FROM clause: the task table under alias `t`, through the
	// index the query names, if any ([taskSource]).
	from string
	// where and args are the predicate and its arguments, in order.
	where string
	args  []any
}

// taskTable is the plain source: the task table under [rowAlias], through
// whatever index the planner chooses.
const taskTable = "tracker_tasks t"

// alias is the name a compiled predicate calls the task it tests.
//
// A VALUE THE COMPILER IS HANDED, because one predicate is asked of the row
// ([rowAlias]) and, under the subtask rollup, of its anchor ([anchorAlias]). A
// clause spelling `t.` would not fail inside the anchor's subquery: it would
// silently test the outer row instead
// ([TestAPredicateCompiledForAnAliasNamesNoOther]).
//
// Aliases for OTHER tables (`x`, `c`, `r`, `k`, `m`, `d`, `p`, `v`) are each
// clause's own; none is one of these.
type alias string

const (
	// rowAlias is the rows a statement over a compiled set returns.
	rowAlias alias = "t"
	// anchorAlias is the task a row's subtree hangs from, read by the
	// rollup's probe ([compileWhere]).
	anchorAlias alias = "a"
	// sideAlias is that task again, read only to ask which side of the
	// trash it is on ([anchorOf]).
	sideAlias alias = "s"
)

// col is one column of the task this alias names.
func (on alias) col(name string) string { return string(on) + "." + name }

// tasksWhere is a set read from the plain table, for a predicate this package
// writes itself rather than compiles from a query.
func tasksWhere(where string, args []any) taskSet {
	return taskSet{from: taskTable, where: where, args: args}
}

// byKey is the set as a statement about ONE task named by id reads it: the
// same predicate over the plain table, so the planner seeks the row by key.
//
// NOT THE SET'S OWN SOURCE: a named index ([taskSource]) is the only one its
// statements may read, and the trash's leads with the removal, so a probe of
// one removed task through it walks every removal in scope.
func (s taskSet) byKey() taskSet {
	return taskSet{from: taskTable, where: s.where, args: s.args}
}

// and is the set narrowed by one more clause, whose arguments bind after the
// set's own. THE PREDICATE IS PARENTHESISED, so the clause narrows all of it,
// not the last term of a top-level disjunction.
func (s taskSet) and(clause string, values []any) taskSet {
	return taskSet{
		from:  s.from,
		where: "(" + s.where + ") AND " + clause,
		args:  append(append([]any{}, s.args...), values...),
	}
}

// The trash's two indexes, one per listing scope (replicated 0056), the
// subtasks' (replicated 0058), and the task and tag tables' keys as this engine
// names them, for [anchorProbe] and [compileGroup]'s tag axis.
const (
	removedIndex        = "tracker_tasks_removed_idx"
	projectRemovedIndex = "tracker_tasks_project_removed_idx"
	parentIndex         = "tracker_tasks_parent_idx"
	tasksKey            = "sqlite_autoindex_tracker_tasks_1"
	tagsKey             = "sqlite_autoindex_tracker_task_tags_1"
)

// peopleClause is a filter on the tasks a person is on — as a collaborator,
// a watcher or the owner of a checklist item — given the subquery that finds
// them by the person.
//
// Driven from the person's rows (one seek, then each task by key), not as an
// EXISTS probe per task: this engine's planner stops seeking at an IN list, so
// the probe read every task in scope times every task the person is on.
//
// THE CHECKLIST'S SUBQUERY STATES ITS PARTIAL INDEX'S PREDICATE (an owner is
// set): the planner does not prove `assignee IN (…)` implies it, and without
// it reads every checklist item from the heap.
func peopleClause(on alias, subquery string) string {
	return on.col("id") + " IN (" + subquery + ")"
}

// taskSource is where a query's tasks are read from: the task table, the
// tree's index for ONE TASK'S SUBTASKS, the trash's index for the TRASH.
//
// Left to the planner, `parent=` read a whole board; named, it seeks the
// parent, and comes FIRST since narrowing further never grows it. The trash's
// partial index is reached by this engine's planner, with no statistics, only
// for an ORDER it provides or a SEEK on its leading column, so unnamed a trash
// statement not ordered by removal read the heap. Its index follows the scope (project key, or
// the order at the workspace). A GUARD too: a dropped index fails with `no
// such index`, a listing that stopped stating [tombClause] `no query solution`.
func taskSource(q Query) string {
	switch {
	case q.Parent != "":
		return taskTable + " INDEXED BY " + parentIndex
	case q.Removed == nil || !*q.Removed:
		return taskTable
	case q.Scope.Project != "":
		return taskTable + " INDEXED BY " + projectRemovedIndex
	default:
		return taskTable + " INDEXED BY " + removedIndex
	}
}

// compileWhere is [compile]'s predicate over the rows a statement returns: the
// query's own ([compilePredicate]), the subtask rollup when the query rolls
// its subtasks up, and a board column's narrowing.
func compileWhere(q Query, now time.Time, fields map[string]resolvedField) (string, []any, error) {
	// THE SUBTASK ROLLUP ([SubtaskMode], [rollsUp]) is a predicate on the
	// row's ANCHOR ([anchorOf]), not on the row. It does not apply to
	// `parent` or `root`, which ask ABOUT subtasks.
	//
	// Each row PROBES its anchor by key ([anchorProbe]), never an
	// `IN (<list of roots>)`, which the planner materialises whole before
	// the first row ([TestARolledUpRowProbesItsAnchorByKey]).
	if rollsUp(q) {
		flat, flatArgs, err := compilePredicate(q, now, fields, false,
			anchorAlias)
		if err != nil {
			return "", nil, err
		}
		// THE ROW'S OWN TOMBSTONE STILL APPLIES, so a removed subtask
		// does not ride on a live root — and it is the query's
		// ([tombClause]), not `IS NULL`, or `removed=true` answers
		// nothing.
		where := []string{tombClause(q, rowAlias)}
		var args []any
		if scope, scopeArgs := scopeClause(q, rowAlias); scope != "" {
			// THE CONTAINER IS THE ROW'S TOO: without it every board
			// walked the company, filtered afterwards by its root. It
			// narrows nothing legitimate: a subtree lives in one
			// project, and one that does not is the
			// `inconsistent_project` anomaly.
			//
			// ITS ARGUMENT COMES FIRST: placeholders bind in textual
			// order and this clause precedes the probe.
			where = append(where, scope)
			args = append(args, scopeArgs...)
		}
		where = append(where, anchorProbe(anchorOf(q),
			strings.Join(flat, " AND ")))
		args = append(args, flatArgs...)
		return groupNarrowed(q, fields, where, args)
	}
	where, args, err := compilePredicate(q, now, fields, false, rowAlias)
	if err != nil {
		return "", nil, err
	}
	return groupNarrowed(q, fields, where, args)
}

// groupNarrowed is a compiled predicate narrowed to one board column, when the
// query asks for one, and joined.
func groupNarrowed(q Query, fields map[string]resolvedField, where []string,
	args []any) (string, []any, error) {

	if q.Group != nil {
		// A COLUMN FILTER IS PART OF THE PREDICATE, JOIN-FREE, because
		// the count hint and totals share it and carry no join. It is
		// the ROW's, never its root's: a column is where a card is drawn.
		axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
		if err != nil {
			return "", nil, err
		}
		clause, values := axis.filter(*q.Group)
		where = append(where, clause)
		args = append(args, values...)
	}
	return strings.Join(where, " AND "), args, nil
}

// anchorOf is the task a row rides along with under the subtask rollup: what
// its question is asked of, as an expression over the row.
//
// Live, it is the tree's root. In the trash it is the REMOVAL that took the
// row (`removed_with`), or the row if removed on its own, since a restore
// brings back a removal ([Writer.RestoreTask]); a root anchor would hide a
// subtask removed alone. And it is the row itself when the anchor is across
// the trash (a root removed alone, a partial restore), else such rows are on
// no default-mode listing; a second probe by key asks the anchor's side.
func anchorOf(q Query) string {
	ref := rowAlias.col("root_id")
	if q.Removed != nil && *q.Removed {
		ref = "IFNULL(" + rowAlias.col("removed_with") + ", " +
			rowAlias.col("id") + ")"
	}
	return "COALESCE((SELECT " + sideAlias.col("id") + " FROM tracker_tasks " +
		string(sideAlias) + " INDEXED BY " + tasksKey + " WHERE " +
		sideAlias.col("id") + " = " + ref + " AND " + tombClause(q, sideAlias) +
		"), " + rowAlias.col("id") + ")"
}

// anchorProbe is "the task ref names matches this predicate", as a probe of
// that one task by the task table's key: predicate is compiled for
// [anchorAlias], and ref is an expression over the row naming its anchor.
//
// THE KEY IS NAMED because, with no statistics (nothing runs ANALYZE), the
// planner steals the probe for any equality or `IN` the predicate puts on an
// indexed column: `assignee=ana` re-read every task Ana holds per outer row.
// Named, the anchor is one row read by id. It is a GUARD too: a dropped key
// fails with `no such index` rather than turning every listing into a walk.
func anchorProbe(ref, predicate string) string {
	return "EXISTS (SELECT 1 FROM tracker_tasks " + string(anchorAlias) +
		" INDEXED BY " + tasksKey + " WHERE " + anchorAlias.col("id") + " = " +
		ref + " AND " + predicate + ")"
}

// rollsUp reports a query whose subtasks ride along on the task they hang
// from rather than being filtered on their own — see [SubtaskMode].
func rollsUp(q Query) bool {
	return q.Subtasks != SubtasksSeparate && q.Parent == "" && q.Root == ""
}

// scopeClause is the container a query is about, as a predicate on the task
// on names; empty at the workspace.
func scopeClause(q Query, on alias) (string, []any) {
	if q.Scope.Project != "" {
		return on.col("project_key") + " = ?", []any{q.Scope.Project}
	}
	return "", nil
}

// tombClause is the side of the trash a query reads, as a predicate on the
// task on names.
//
// EVERY QUERY EXCLUDES A REMOVED TASK, with one predicate at any age, so a
// restore answers exactly what it did before. `removed=true` is the only way
// into the trash, and is `IS NOT NULL` rather than a date bound because the
// trash's partial indexes ([taskSource]) are partial on exactly that.
func tombClause(q Query, on alias) string {
	if q.Removed != nil && *q.Removed {
		return on.col("removed_at") + " IS NOT NULL"
	}
	return on.col("removed_at") + " IS NULL"
}

// compilePredicate is the query's own predicate, every clause of it asked of
// the task `on` names, with `branch` telling it whether it is compiling one
// arm of a disjunction rather than the query itself.
//
// A BRANCH SKIPS THE ANSWER-SHAPE DEFAULTS (removed, finished and archived
// excluded): emitted inside each arm of an OR they would defeat the top
// level's `show_closed=true`. [Query.parseAny] refuses those keys in a
// branch; this stops the defaults their absence resolves to. The scope IS a
// predicate and stays.
func compilePredicate(q Query, now time.Time, fields map[string]resolvedField,
	branch bool, on alias) ([]string, []any, error) {

	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}

	if scope, scopeArgs := scopeClause(q, on); scope != "" {
		add(scope, scopeArgs...)
	}
	if !branch {
		add(tombClause(q, on))
	}

	if len(q.Keys) > 0 {
		add(on.col("key")+" IN ("+placeholders(len(q.Keys))+")", anyOf(q.Keys)...)
	}
	if q.PriorityListOf != "" {
		if len(q.PriorityList) == 0 {
			// AN EMPTY LIST IS AN EMPTY ANSWER; dropping the clause
			// would answer every task in scope.
			add("0")
		} else {
			add(on.col("id")+" IN ("+placeholders(len(q.PriorityList))+")",
				anyOf(q.PriorityList)...)
		}
	}

	if len(q.Status) > 0 {
		add(on.col("status")+" IN ("+placeholders(len(q.Status))+")", anyOf(q.Status)...)
	}
	if len(q.StatusNot) > 0 {
		add(on.col("status")+" NOT IN ("+placeholders(len(q.StatusNot))+")",
			anyOf(q.StatusNot)...)
	}
	if len(q.StatusGroups) > 0 {
		add(on.col("status_group")+" IN ("+placeholders(len(q.StatusGroups))+")",
			anyOf(q.StatusGroups)...)
	}
	if len(q.Priorities) > 0 {
		add(on.col("priority")+" IN ("+placeholders(len(q.Priorities))+")",
			anyOf(q.Priorities)...)
	}
	if len(q.Types) > 0 {
		add(on.col("type")+" IN ("+placeholders(len(q.Types))+")", anyOf(q.Types)...)
	}
	// BOTH UNIT FILTERS RESOLVE THROUGH THE CHART ([unitKeys]).
	if units := unitKeys(q.Units, q.Unit); len(units) > 0 {
		add(on.col("filed_unit")+" IN ("+placeholders(len(units))+")", anyOf(units)...)
	}
	if units := unitKeys(q.Units, q.RoutingUnit); len(units) > 0 {
		add(on.col("routing_unit")+" IN ("+placeholders(len(units))+")", anyOf(units)...)
	}
	if handles := q.Assignee; len(handles) > 0 {
		// `none` is a VALUE ("unassigned"), not a missing filter.
		clause, values := handleClause(on.col("assignee"), handles)
		add(clause, values...)
	}
	if len(q.AssigneeNot) > 0 {
		// THE SAME CLAUSE, NEGATED, so `!none` leaves out exactly the rows
		// `none` admits and a handle exactly the rows it names.
		clause, values := handleClause(on.col("assignee"), q.AssigneeNot)
		add("NOT ("+clause+")", values...)
	}
	if len(q.Reporter) > 0 {
		clause, values := handleClause(on.col("reporter"), q.Reporter)
		add(clause, values...)
	}
	// A PERSON'S WORK IS READ FROM THEIR OWN ROWS ([peopleClause]).
	for _, join := range []struct {
		handles []string
		table   string
	}{
		{q.Collaborator, "tracker_collaborators"},
		{q.Watcher, "tracker_watchers"},
	} {
		if len(join.handles) > 0 {
			add(peopleClause(on, "SELECT x.task_id FROM "+join.table+
				" x WHERE x.handle IN ("+placeholders(len(join.handles))+")"),
				anyOf(join.handles)...)
		}
	}
	if len(q.ChecklistAssignee) > 0 {
		add(peopleClause(on, "SELECT x.task_id FROM tracker_checklist_items x "+
			"WHERE x.assignee IN ("+placeholders(len(q.ChecklistAssignee))+
			") AND x.assignee <> ''"), anyOf(q.ChecklistAssignee)...)
	}
	if len(q.Tags.Tags) > 0 {
		clause, values := tagClause(q.Tags, q.Scope.Project, on)
		add(clause, values...)
	}
	if q.Parent != "" {
		add(on.col("parent_id")+" = ?", q.Parent)
	}
	if q.Root != "" {
		add("EXISTS (SELECT 1 FROM tracker_task_closure c "+
			"WHERE c.descendant_id = "+on.col("id")+" AND c.ancestor_id = ?)", q.Root)
	}
	if q.References != "" {
		add("EXISTS (SELECT 1 FROM tracker_references r "+
			"JOIN tracker_task_keys k ON k.task_id = r.to_task "+
			"WHERE r.from_task = "+on.col("id")+" AND k.key = ?)", q.References)
	}
	if q.LinkedPage != "" {
		// THE PAGE'S OWN EDGES, through `tracker_relations_other_idx`:
		// the few tasks linking the page. A probe of each row's own
		// relations walks the whole listing.
		add(on.col("id")+" IN (SELECT x.task_id FROM tracker_relations x "+
			"WHERE x.other_id = ? AND x.kind = ?)", q.LinkedPage,
			string(RelationPage))
	}
	if q.Blocked != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.task_id = " + on.col("id") + " AND d.blocker_open = 1)"
		if !*q.Blocked {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasChildren != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_tasks c WHERE c.parent_id = " +
			on.col("id") + ")"
		if !*q.HasChildren {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasParent != nil {
		if *q.HasParent {
			add(on.col("parent_id") + " IS NOT NULL")
		} else {
			add(on.col("parent_id") + " IS NULL")
		}
	}
	if q.HasOpenAsks != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = " +
			on.col("id") + " AND " + openAskSQL + ")"
		if !*q.HasOpenAsks {
			clause = "NOT " + clause
		}
		add(clause)
	}
	// THE TWO ENDS OF AN OPEN ASK (waiting on me, waiting for), each the
	// open-ask predicate [Query.HasOpenAsks] uses.
	if q.AskedOf != "" {
		add("EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = "+
			on.col("id")+" AND m.ask = ? AND m.resolved = 0 AND "+
			"m.answered_by IS NULL AND m.removed = 0 AND m.overtaken = '')", q.AskedOf)
	}
	if q.AskedBy != "" {
		add("EXISTS (SELECT 1 FROM tracker_comments m WHERE m.task_id = "+
			on.col("id")+" AND m.ask <> '' AND m.author = ? AND m.resolved = 0 "+
			"AND m.answered_by IS NULL AND m.removed = 0 AND m.overtaken = '')", q.AskedBy)
	}
	// BLOCKING IS THE OTHER END OF BLOCKED, not its negation: it reads by
	// blocker_id, and a task can be both.
	if q.Blocking != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.blocker_id = " + on.col("id") + " AND d.blocker_open = 1)"
		if !*q.Blocking {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.HasDependencies != nil {
		clause := "EXISTS (SELECT 1 FROM tracker_task_deps d WHERE d.task_id = " +
			on.col("id") + ")"
		if !*q.HasDependencies {
			clause = "NOT " + clause
		}
		add(clause)
	}
	if q.Text != "" {
		// THE FIND: a substring of a KEY or a TITLE; ranked search is
		// [Searcher]'s. THE KEY MATCHES FROM THE FRONT, or `2` would
		// match ENG-12 and ENG-20; the title, prose, matches anywhere.
		pattern := likeEscape(q.Text)
		add("("+on.col("key")+` LIKE ? ESCAPE '\' OR `+on.col("title")+
			` LIKE ? ESCAPE '\')`, pattern+"%", "%"+pattern+"%")
	}

	for _, filter := range q.Fields {
		field, held := fields[filter.Ref]
		if !held {
			// UNREACHABLE THROUGH [Reader.Tasks]; a refusal rather than
			// a skip, which would silently widen the answer.
			return nil, nil, fmt.Errorf("worklet: f.%s was never resolved, so "+
				"this answer would be wider than the caller asked for",
				filter.Ref)
		}
		clause, values, err := fieldClause(filter, field, on)
		if err != nil {
			return nil, nil, err
		}
		add(clause, values...)
	}
	for key, filter := range q.Dates {
		clause, values, err := dateClause(on, key, filter)
		if err != nil {
			return nil, nil, err
		}
		add(clause, values...)
		if filter.Overdue {
			// THE ALIAS CARRIES ITS OPEN CONDITION, so it is the
			// preset's predicate rather than a drifting second one.
			add(on.col("status_group") + " IN (" + openGroupsSQL + ")")
		}
	}
	for column, filter := range map[string]*NumFilter{
		"estimate_min": q.Estimate, "points": q.Points, "spend_tokens": q.Spend,
	} {
		if filter == nil {
			continue
		}
		clause, values := numClause(on.col(column), *filter)
		add(clause, values...)
	}
	if len(q.Flags) > 0 {
		clause, err := flagsClause(on, q.Flags)
		if err != nil {
			return nil, nil, err
		}
		add(clause)
	}

	switch {
	case branch:
		// THE ANSWER'S SHAPE, NOT THIS ARM'S — see above.
	case q.Archived == ArchivedExclude:
		// A JOIN RATHER THAN A COLUMN: a project's archive is the
		// project's fact, and copying it onto every task would be an
		// unbounded cross-object write.
		add(on.col("archived") + " = 0 AND NOT EXISTS (SELECT 1 FROM " +
			"tracker_projects p WHERE p.key = " + on.col("project_key") +
			" AND p.archived = 1)")
	case q.Archived == ArchivedOnly:
		add("(" + on.col("archived") + " = 1 OR EXISTS (SELECT 1 FROM " +
			"tracker_projects p WHERE p.key = " + on.col("project_key") +
			" AND p.archived = 1))")
	}

	switch {
	case branch:
		// THE ANSWER'S SHAPE, NOT THIS ARM'S — see above.
	case q.ShowClosed.All:
	case q.ShowClosed.Recent > 0 || !q.ShowClosed.Since.IsZero():
		// THE WINDOW APPLIES TO THE WHOLE FINISHED SET (the finish
		// stamp is by GROUP), so cancelled tasks are in it as done
		// ones are. One clause for both spellings of the bound: a
		// duration back from now, or a resolved calendar instant.
		since := now.Add(-q.ShowClosed.Recent)
		if !q.ShowClosed.Since.IsZero() {
			since = q.ShowClosed.Since
		}
		add("("+on.col("status_group")+" IN ("+openGroupsSQL+") OR "+
			"("+on.col("finished_at")+" IS NOT NULL AND "+on.col("finished_at")+
			" >= ?))", store.EncodeTime(since))
	default:
		add(on.col("status_group") + " IN (" + openGroupsSQL + ")")
	}

	if len(q.Any) > 0 {
		var branches []string
		for i, arm := range q.Any {
			clauses, values, err := compilePredicate(arm, now, fields, true, on)
			if err != nil {
				return nil, nil, err
			}
			if len(clauses) == 0 {
				// AN EMPTY ARM IS "EVERYTHING", making the other arms
				// decoration: refused, since a caller can fix it.
				return nil, nil, fmt.Errorf("worklet: any branch %d selects "+
					"nothing — a branch is a predicate, and an empty one "+
					"matches every task and makes the other branches "+
					"decoration", i)
			}
			branches = append(branches, "("+strings.Join(clauses, " AND ")+")")
			args = append(args, values...)
		}
		// ONE LEVEL, ANDed with the top-level keys.
		where = append(where, "("+strings.Join(branches, " OR ")+")")
	}
	return where, args, nil
}

// pageClause is the keyset resume — the arms the rows after the cursor are
// read through ([cursorArms]), none for a first page — and it is DELIBERATELY
// NOT PART OF [compile].
//
// The count hint and totals share the compiled predicate and are about the
// whole set, so a cursor folded into it would make page two's header count
// from page two onwards. And only the row read carries a SORT JOIN, whose alias
// a `sort=f.<slug>` cursor compares.
func pageClause(q Query, fields map[string]resolvedField, basis orderBasis) ([]keyset.Arm, error) {
	if q.Cursor == "" {
		return nil, nil
	}
	return cursorArms(q, fields, basis)
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

// tagClause builds the three tag modes, over the project a scoped query is
// about ("" at the workspace).
//
// `any:` and `all:` read FROM the tag rows (`tracker_task_tags_slug_idx`), not
// a probe per task. ONE ARM PER SLUG, the project in each
// ([Applier.explodeTask] keeps it on the tag row), since this engine's planner
// stops seeking at an `IN` list. `any:` is the arms' `UNION ALL`, `all:` their
// `INTERSECT` (slugs are unique, [Query.parseTags]): a compound SELECT, never
// one `IN` per slug ANDed, whose expression tree fails past depth 100 where an
// `all:` longer than [MaxTagsPerTask] must answer nothing. `none:` is about ABSENT rows: a probe by primary key.
func tagClause(filter TagFilter, project string, on alias) (string, []any) {
	if filter.Mode == TagNone {
		probe := "NOT EXISTS (SELECT 1 FROM tracker_task_tags x WHERE x.task_id = " +
			on.col("id") + " AND x.slug IN (" + placeholders(len(filter.Tags)) + "))"
		return probe, anyOf(filter.Tags)
	}
	arms := make([]string, 0, len(filter.Tags))
	var args []any
	for _, slug := range filter.Tags {
		arm := "SELECT x.task_id FROM tracker_task_tags x WHERE x.slug = ?"
		args = append(args, slug)
		if project != "" {
			arm += " AND x.project_key = ?"
			args = append(args, project)
		}
		arms = append(arms, arm)
	}
	if filter.Mode == TagAll {
		return on.col("id") + " IN (" + strings.Join(arms, " INTERSECT ") + ")", args
	}
	return on.col("id") + " IN (" + strings.Join(arms, " UNION ALL ") + ")", args
}

// dateColumns maps a grammar key to the column it filters, by the column's
// own name: [dateClause] reads it on the task its alias names.
//
// `status_entered` reads the EFFECTIVE instant, the start of a duration; the
// rest read the AUTHORED one, the wall-clock bound the caller typed.
var dateColumns = map[string]string{
	"due": "due_at", "start": "start_at", "created": "created_at",
	"updated": "updated_at", "done": "done_at", "closed": "closed_at",
	"finished": "finished_at", "archived_at": "archived_at",
	"status_entered": "status_entered_at",
}

func dateClause(on alias, key string, filter DateFilter) (string, []any, error) {
	name, ok := dateColumns[key]
	if !ok {
		return "", nil, fmt.Errorf("worklet: %q names no date column", key)
	}
	column := on.col(name)
	from := store.EncodeTime(filter.From.At)

	// THE NULL PREDICATE IS NOT REDUNDANT: every date index is partial on
	// `IS NOT NULL`, and this engine's planner does not infer it from
	// `x < ?`, so without it every date filter reads the table
	// (TestEveryIndexServesARegisteredQuery).
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
		// HALF-OPEN, so "this week" holds every instant of Sunday.
		return notNull + column + " >= ? AND " + column + " < ?",
			[]any{from, store.EncodeTime(filter.To.At)}, nil
	}
	return "", nil, fmt.Errorf("worklet: %q is not a date comparison", filter.Op)
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

// flagColumn maps an attention flag to its column, from a CLOSED SET: the one
// place a grammar value reaches a statement as a name, not a bound value.
func flagColumn(on alias, flag string) (string, bool) {
	switch flag {
	case "cycle", "too_deep", "inconsistent_project", "key_collision":
		return on.col(flag) + " = 1", true
	}
	return "", false
}

// flagsClause is the whole flag filter, and what it settles.
//
// THE SET IS AN OR: the attention queue names every flag and wants tasks
// carrying any; an AND answers nothing and looks like a healthy company.
//
// `one_sided` and `one_sided_final` are on a dependency EDGE
// (`tracker_relations`), so they are an EXISTS over the row's edges.
//
// No index holds any flag: they are tested as the listing is walked. A list
// of edge-flagged tasks would not drive the read either; the planner still
// walks the listing.
func flagsClause(on alias, flags []string) (string, error) {
	terms := make([]string, 0, len(flags))
	for _, flag := range flags {
		switch term, onTask := flagColumn(on, flag); {
		case onTask:
			terms = append(terms, term)
		case flag == "one_sided":
			terms = append(terms, "EXISTS (SELECT 1 FROM tracker_relations x "+
				"WHERE x.task_id = "+on.col("id")+" AND x.one_sided = 1 "+
				"AND x.one_sided_final = 0)")
		case flag == "one_sided_final":
			terms = append(terms, "EXISTS (SELECT 1 FROM tracker_relations x "+
				"WHERE x.task_id = "+on.col("id")+" AND x.one_sided_final = 1)")
		default:
			return "", fmt.Errorf("worklet: %q is not an attention "+
				"flag. The flags are: %s", flag, strings.Join(AttentionFlags, ", "))
		}
	}
	return "(" + strings.Join(terms, " OR ") + ")", nil
}

// AttentionFlags is the closed set, in the order the attention queue reads
// them: the four a task carries on its own row, then the two a dependency
// edge carries.
var AttentionFlags = []string{
	"cycle", "too_deep", "inconsistent_project", "key_collision",
	"one_sided", "one_sided_final",
}

// sortTerm is one column of the order, with the direction it is read in.
//
// THE ORDER IS A VALUE, not a string, because the keyset cursor must compare
// exactly its columns in their directions.
type sortTerm struct {
	Column     string
	Descending bool

	// NeverNull marks a NOT NULL column. Not cosmetic: an ascending
	// `NULLS LAST` is not the order an index stores, so the planner sorts
	// the scope in a temp b-tree ([renderOrder]); the id tiebreak on every
	// order is the case that matters.
	NeverNull bool

	// Join is the LEFT JOIN a custom-field sort needs, empty for a plain
	// column. LEFT, or the sort would drop every task with no value.
	Join string

	// JoinArgs are the join's bound values, which ride AHEAD of the
	// predicate's: a join is written before the WHERE.
	JoinArgs []any

	// Basis is what the term's values are positions in beyond its text
	// (the seats' order, [assigneeOrder]): digested by [cursorOrder],
	// never rendered.
	Basis string
}

// sortColumn is a column a caller may order by, and whether it can be absent.
type sortColumn struct {
	Column string
	// Nullable says the schema permits NULL here ([renderOrder]).
	Nullable bool
}

// sortColumns is what a caller may order by.
//
// THE NULLABILITY IS HERE, beside the column, for [renderOrder]. BOTH
// DIRECTIONS ARE GATED: a nullable column marked NOT NULL fails
// [TestSortingByADateLeavesTheUndatedLast], and an unmarked NOT NULL one fails
// TestEveryIndexServesARegisteredQuery. A migration relaxing a constraint
// touches this map.
var sortColumns = map[string]sortColumn{
	// NOT NULL in `0002_the_tracker_lands.sql`, each with a default where
	// the writer may omit a value.
	"rank":           {Column: "t.rank"},
	"updated":        {Column: "t.updated_at"},
	"priority":       {Column: "t.prio_rank"},
	"created":        {Column: "t.created_at"},
	"title":          {Column: "t.title"},
	"estimate":       {Column: "t.estimate_min"},
	"points":         {Column: "t.points"},
	"spend_tokens":   {Column: "t.spend_tokens"},
	"reopens":        {Column: "t.reopens"},
	"status_entered": {Column: "t.status_entered_at"},

	// NULLABLE: an undated task sorts last, not as the soonest due.
	"due":   {Column: "t.due_at", Nullable: true},
	"start": {Column: "t.start_at", Nullable: true},

	// THE TRASH'S COLUMN, nullable because NULL is "not removed": outside
	// the trash the id tiebreak decides. In the trash, whose predicate is
	// `removed_at IS NOT NULL`, [sortTerms] marks it never NULL, so the
	// trash's indexes serve the trash tab's ([ViewKeyTrash]) sort either way.
	"removed": {Column: "t.removed_at", Nullable: true},
}

// EVERY KEY HERE IS ONE [sortKeys] ADMITS, checked both ways because neither
// half fails loudly: a key the parser refuses is unreachable, and one this map
// lacks is silently DROPPED by [sortTerms].

// sortTerms compiles the sort, ALWAYS ENDING IN THE ID.
//
// The id tiebreak makes a page stable (a boundary between equal values would
// drop one and repeat the other) and every node's order the same. It is free:
// each ordering index carries the id as its trailing column.
func sortTerms(q Query, fields map[string]resolvedField, basis orderBasis) []sortTerm {
	trash := q.Removed != nil && *q.Removed
	var terms []sortTerm
	for i, sort := range q.Sort {
		if ref, ok := strings.CutPrefix(sort.Key, FieldKeyPrefix); ok && ref != "" {
			field, held := fields[ref]
			if !held {
				continue
			}
			// ONE ALIAS PER TERM, or a second field sort silently
			// re-uses the first's join. `seq = 0` PINS ONE ROW, or a
			// labels field multiplies every task by its value count.
			alias := "fs" + strconv.Itoa(i)
			terms = append(terms, sortTerm{
				Column:     alias + "." + FieldValueColumn(field.Type),
				Descending: sort.Descending,
				Join: " LEFT JOIN tracker_field_values " + alias +
					" INDEXED BY " + fieldValuesKey +
					" ON " + alias + ".task_id = t.id AND " + alias +
					".field_id = ? AND " + alias + ".hidden = 0 AND " +
					alias + ".kind <> '" + FieldValueForeign + "' AND " +
					alias + ".seq = 0",
				JoinArgs: []any{field.ID},
			})
			continue
		}
		if derived, ok := derivedOrders[sort.Key]; ok {
			// ITS OWN ALIAS for the reason a field sort's is above.
			terms = append(terms, derived(basis, sort.Descending,
				"ord"+strconv.Itoa(i))...)
			continue
		}
		column, known := sortColumns[sort.Key]
		if !known {
			continue
		}
		terms = append(terms, sortTerm{
			Column: column.Column, Descending: sort.Descending,
			NeverNull: !column.Nullable || (sort.Key == "removed" && trash),
		})
	}
	if len(terms) == 0 {
		switch {
		case trash:
			// THE TRASH IS ORDERED BY REMOVAL, never by a stale rank,
			// the order its indexes ([taskSource]) hold. NEVER NULL
			// here ([sortColumns]' `removed`), so the resume reads no
			// NULL tail.
			terms = append(terms, sortTerm{
				Column: "t.removed_at", Descending: true, NeverNull: true,
			})
		case q.Scope.Project != "":
			// Manual order inside a project, most recently touched
			// elsewhere: a rank orders only within its container.
			terms = append(terms, sortTerm{Column: "t.rank", NeverNull: true})
		default:
			terms = append(terms, sortTerm{
				Column: "t.updated_at", Descending: true, NeverNull: true,
			})
		}
	}
	// The primary key is NOT NULL, so no NULL clause, and an index serves.
	return append(terms, sortTerm{Column: "t.id", NeverNull: true})
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
//
// AN ABSENT VALUE SORTS LAST, IN BOTH DIRECTIONS: descending does so by
// SQLite's default, ascending needs `NULLS LAST`.
//
// THE CLAUSE IS WRITTEN ONLY WHERE IT CHANGES THE ANSWER: a NULL ordering an
// index does not store turns a seek into a temp b-tree sort of the scope. A
// term is nullable unless marked NeverNull, the safe default because a
// custom-field sort's LEFT JOIN makes any value nullable.
//
// [keyset]'s arms must put NULLs last exactly as this does, or a resume lands
// in the middle of them.
func renderOrder(terms []sortTerm) string {
	rendered := make([]string, 0, len(terms))
	for _, term := range terms {
		switch {
		case term.Descending:
			rendered = append(rendered, term.Column+" DESC")
		case term.NeverNull:
			rendered = append(rendered, term.Column)
		default:
			rendered = append(rendered, term.Column+" NULLS LAST")
		}
	}
	return strings.Join(rendered, ", ")
}

// page is what a cursor carries: the last row's value for EVERY column the
// order sorts by, ending in its id — and the ORDER those values belong to.
//
// THE ORDER TRAVELS BECAUSE THE ARITY IS NOT THE ORDER: two sorts of equal
// length pass a count check, and SQLite compares mismatched types by affinity
// rather than refusing, so a foreign cursor silently pages wrong.
type page struct {
	Keys []any `json:"k"`

	// Order is the compiled order the cursor was minted under, digested by
	// [cursorOrder]. Compiled, not the `sort=` spelling, because the
	// default order depends on the scope.
	Order string `json:"o,omitempty"`
}

// cursorOrder is what a cursor carries of its order: a digest of the order as
// compiled, its joins and what they bind included, which names it as exactly
// as the statement does.
//
// WHAT A TERM BINDS AND IS RELATIVE TO IS THE ORDER TOO (the field a sort
// reads, the roster a seat's place is in, [sortTerm.Basis]), or `sort=f.effort`
// would accept a `sort=f.cost` cursor.
//
// A DIGEST RATHER THAN THE TEXT, which can name every type and would ride in
// every URL and every paging model's context.
func cursorOrder(terms []sortTerm) string {
	digest := sha256.New()
	digest.Write([]byte(renderOrder(terms)))
	for _, term := range terms {
		if term.Join == "" && term.Basis == "" {
			continue
		}
		digest.Write([]byte("\x00" + term.Join + "\x00" + term.Basis))
		for _, arg := range term.JoinArgs {
			fmt.Fprintf(digest, "\x00%T:%v", arg, arg)
		}
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil)[:16])
}

// sortSpelling is an order as a caller wrote it, for a refusal to name.
func sortSpelling(sorts []Sort) string {
	if len(sorts) == 0 {
		return "the default order"
	}
	out := make([]string, len(sorts))
	for i, s := range sorts {
		out[i] = s.Key
		if s.Descending {
			out[i] = "-" + s.Key
		}
	}
	return "sort=" + strings.Join(out, ",")
}

// cursorArms turns an opaque page cursor back into the arms the rows after it
// are read through ([keyset.After]).
//
// KEYSET RATHER THAN OFFSET: an offset re-reads every skipped row, and an
// insert between polls shifts every later page.
//
// It compares the WHOLE order, not just the id: under any other order
// `id > last` skips some rows and repeats others, silently.
//
// ARMS, not one lexicographic predicate, which the planner cannot seek on:
// each arm is one seek of the order's index. The arithmetic and its NULL rule
// are [keyset]'s; [readPage] reads the arms in turn until the page is full.
func cursorArms(q Query, fields map[string]resolvedField, basis orderBasis) ([]keyset.Arm, error) {
	resume, err := decodePage(q.Cursor)
	if err != nil {
		return nil, err
	}
	terms := sortTerms(q, fields, basis)
	if len(resume.Keys) == 0 || resume.Keys[len(resume.Keys)-1] == nil {
		// THE LAST KEY IS THE ROW'S ID, never NULL: without it the
		// cursor names no row and would resume onto nothing.
		return nil, fmt.Errorf("%w: it names no row to resume after", ErrBadCursor)
	}
	if len(resume.Keys) != len(terms) || resume.Order != cursorOrder(terms) {
		// A CURSOR FROM A DIFFERENT ORDER IS REFUSED, never applied,
		// which would silently drop rows; the ORDER decides, not the
		// key count ([page]).
		return nil, fmt.Errorf("%w: it was minted for another order than this "+
			"query's (%s). A cursor belongs to the order that minted it, and "+
			"re-sorting, or an edit to the type catalogue or the org chart an "+
			"order is built from, starts again from the first page",
			ErrBadCursor, sortSpelling(q.Sort))
	}
	return keyset.After(keysetTerms(terms), resume.Keys), nil
}

// ErrBadCursor refuses a cursor a listing cannot resume: one it did not mint,
// or one minted for another order than the query's, the chart's or the type
// catalogue's before an edit included. A [ErrBadQuery], since the same cursor
// is refused on every attempt; the remedy is the listing's first page.
//
// A SENTINEL, so the refusal is answered as the caller's, not as this node's
// fault (a 500, or "try again" to a model, for ever).
var ErrBadCursor = fmt.Errorf("%w: the cursor does not resume this listing", ErrBadQuery)

// decodePage reads a cursor [mintCursor] minted back into its page.
func decodePage(cursor string) (page, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return page{}, fmt.Errorf("%w: it is not one this listing minted", ErrBadCursor)
	}
	var resume page
	if err := json.Unmarshal(raw, &resume); err != nil {
		return page{}, fmt.Errorf("%w: it is not one this listing minted", ErrBadCursor)
	}
	return resume, nil
}

// keysetTerms is an order as [keyset] reads one.
func keysetTerms(terms []sortTerm) []keyset.Term {
	out := make([]keyset.Term, len(terms))
	for i, term := range terms {
		out[i] = keyset.Term{Column: term.Column, Descending: term.Descending,
			NeverNull: term.NeverNull}
	}
	return out
}

// pageArmSets is the set narrowed to each arm of a resume, in read order, or
// the set alone for a first page; named for the plan test.
//
// AN ORDER ON A JOINED COLUMN IS ONE READ OF ALL ITS ARMS: no task index holds
// a custom field's value, so no arm is a seek, and an arm alone bounds the
// joined column, letting the planner drive the LEFT JOIN from the value table.
// The disjunction keeps the NULL arm, so the join stays driven from the task.
func pageArmSets(set taskSet, resume []keyset.Arm, terms []sortTerm) []taskSet {
	if len(resume) == 0 {
		return []taskSet{set}
	}
	reads := armReads(terms, resume)
	out := make([]taskSet, len(reads))
	for i, read := range reads {
		out[i] = set.and(read.clause, read.args)
	}
	return out
}

// armRead is one read of a side of a row: the predicate an arm, or every arm
// of an order on a joined column, is rendered to, with its arguments.
type armRead struct {
	clause string
	args   []any
}

// armReads is the reads one side of a row is taken through: an arm each,
// nearest first, or one disjunction on a joined column ([pageArmSets]).
func armReads(terms []sortTerm, arms []keyset.Arm) []armRead {
	order := keysetTerms(terms)
	if joinsColumn(terms) {
		clause, args := keyset.Disjunction(order, arms)
		return []armRead{{clause, args}}
	}
	out := make([]armRead, len(arms))
	for i, arm := range arms {
		out[i].clause, out[i].args = arm.SQL(order)
	}
	return out
}

// joinsColumn reports an order with a term on a joined column rather than on
// the task row's own.
func joinsColumn(terms []sortTerm) bool {
	for _, term := range terms {
		if term.Join != "" {
			return true
		}
	}
	return false
}

// mintCursor encodes the page's own resume point.
func mintCursor(keys []any, order string) string {
	body, err := json.Marshal(page{Keys: keys, Order: order})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

// rowsAt is what a page of rows is read against: the company's midnight its
// overdue mark is cut on, and the instant and posture its flow is read at.
type rowsAt struct {
	dayStart time.Time
	flow     flowAt
}

// readTasks reads one page of a set in an order.
//
// A GROUPED ANSWER'S COLUMN is this statement narrowed to one value
// ([columnSet]), with the same sort, row shape and cursor.
func readTasks(ctx context.Context, tx *sql.Tx, set taskSet,
	terms []sortTerm, limit int, at rowsAt) ([]TaskRow, string, error) {

	return readPage(ctx, tx, set, nil, terms, limit, at)
}

// readPage reads one page of a set in an order, resuming after a cursor's row
// when resume holds its arms ([cursorArms]).
//
// ARM BY ARM, IN ONE TRANSACTION, until the page is full: the arms in turn are
// the order from the cursor onwards, each a seek its LIMIT stops ([keyset]).
// ONE MORE THAN THE PAGE tells whether another page exists without a count.
//
// An order no index serves reads the scope once per arm reached (usually two):
// bounded by the order, never by how far the caller paged.
func readPage(ctx context.Context, tx *sql.Tx, set taskSet,
	resume []keyset.Arm, terms []sortTerm, limit int,
	at rowsAt) ([]TaskRow, string, error) {

	// ALLOCATED RATHER THAN NIL, a wire contract: a nil slice marshals to
	// `null`, and a client's `rows.length` on an empty answer throws.
	out := []TaskRow{}
	var pageKeys [][]any
	for _, arm := range pageArmSets(set, resume, terms) {
		rows, keys, err := scanTaskRows(ctx, tx, arm, terms, limit+1-len(out), at.flow)
		if err != nil {
			return nil, "", err
		}
		out = append(out, rows...)
		pageKeys = append(pageKeys, keys...)
		if len(out) > limit {
			break
		}
	}
	cursor := ""
	if len(out) > limit {
		out = out[:limit]
		// THE ORDER TRAVELS WITH THE KEYS — see [page].
		cursor = mintCursor(pageKeys[limit-1], cursorOrder(terms))
	}
	// THE OVERDUE FLAG IS DERIVED ONCE, here, against the QUERY's day
	// boundary, never this process's clock, so it agrees with
	// `due=overdue` ("before midnight today, in the company's zone").
	for i := range out {
		out[i].Overdue = out[i].Due != nil &&
			out[i].StatusGroup.Open() && out[i].Due.Before(at.dayStart)
	}
	// THE DEPENDENCY EDGES, in ONE statement over the page: a correlated
	// subquery in the SELECT cannot return a collection.
	if err := loadBlockers(ctx, tx, out); err != nil {
		return nil, "", err
	}
	return out, cursor, nil
}

// scanTaskRows reads up to n rows of a set in an order, each with its value
// for every column the order sorts by.
func scanTaskRows(ctx context.Context, tx *sql.Tx, set taskSet,
	terms []sortTerm, n int, at flowAt) ([]TaskRow, [][]any, error) {

	query, bound := taskRowsStatement(set, terms, at)
	rows, err := tx.QueryContext(ctx, query, append(bound, n)...)
	if err != nil {
		return nil, nil, fmt.Errorf("worklet: read tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRow
	var pageKeys [][]any
	for rows.Next() {
		var row TaskRow
		var start, due sql.NullInt64
		var parentID sql.NullString
		var archived, blocked, collision int
		var updated int64
		var version int64
		var flow []byte
		sortValues := make([]any, len(terms))
		targets := []any{&row.ID, &row.Key, &collision, &row.Title, &row.Type, &row.Status,
			&row.StatusGroup, &row.Priority, &row.Assignee, &row.Project,
			&parentID, &row.Depth, &start, &due, &row.EstimateMinutes,
			&row.Points, &archived, &row.Rank, &updated, &version, &blocked, &flow}
		for i := range sortValues {
			targets = append(targets, &sortValues[i])
		}
		if err = rows.Scan(targets...); err != nil {
			return nil, nil, fmt.Errorf("worklet: read a task row: %w", err)
		}
		pageKeys = append(pageKeys, sortValues)
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
		row.KeyCollision = collision == 1
		row.Updated = store.DecodeTime(updated)
		row.Version = uint64(version)
		if row.Flow, err = decodeFlow(flow); err != nil {
			return nil, nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("worklet: walk the task rows: %w", err)
	}
	return out, pageKeys, nil
}

// taskRowsStatement is the statement [readTasks] reads a page with, and its
// arguments up to the trailing LIMIT; named for the plan test.
func taskRowsStatement(set taskSet, terms []sortTerm, at flowAt) (string, []any) {

	// THE SORT COLUMNS ARE SELECTED TOO, as opaque values: the page cursor
	// is their values.
	keys := make([]string, 0, len(terms))
	for _, term := range terms {
		keys = append(keys, term.Column)
	}

	// THE FLOW'S ARGUMENTS COME FIRST, then the joins': the select list
	// precedes the joins, the joins the WHERE, and placeholders bind in
	// statement order.
	flow, flowArgs := flowExpr(at, flowWhole)
	joins, joinArgs := sortJoins(terms)
	query := `SELECT t.id, t.key, t.key_collision, t.title, t.type, t.status, t.status_group, t.priority,
	                 t.assignee, t.project_key, t.parent_id,
	                 t.depth, t.start_at, t.due_at, t.estimate_min, t.points,
	                 t.archived, t.rank, t.updated_at, t.version,
	                 EXISTS (SELECT 1 FROM tracker_task_deps d
	                         WHERE d.task_id = t.id AND d.blocker_open = 1),
	                 ` + flow + `, ` +
		strings.Join(keys, ", ") + `
	          FROM ` + set.from + joins + `
	          WHERE ` + set.where + `
	          ORDER BY ` + renderOrder(terms) + `
	          LIMIT ?`
	return query, append(append(flowArgs, joinArgs...), set.args...)
}

// blockersStatement is [loadBlockers]' read of n tasks' dependency edges,
// named for the plan test.
func blockersStatement(n int) string {
	return `SELECT r.task_id, r.other_id, r.one_sided, r.one_sided_final,
	                 COALESCE(d.blocker_open, 0), COALESCE(d.dead_head, '') <> '',
	                 COALESCE(b.key, ''), COALESCE(b.key_collision, 0)
	          FROM tracker_relations r
	          LEFT JOIN tracker_task_deps d
	                 ON d.task_id = r.task_id AND d.blocker_id = r.other_id
	          LEFT JOIN tracker_tasks b ON b.id = r.other_id
	          WHERE r.kind = ? AND r.task_id IN (` + placeholders(n) + `)
	          ORDER BY r.task_id, r.other_id`
}

// loadBlockers fills [TaskRow.WaitingOn] for one page, in one statement.
//
// TWO TABLES: `tracker_relations` is the AUTHORED edge with the mirror flags,
// `tracker_task_deps` the DERIVED one with whether the blocker is open. The
// join is exact ([Applier.maintainDeps]), and LEFT anyway: dropping an edge it
// could not pair would report fewer dependencies, which no caller can detect.
func loadBlockers(ctx context.Context, tx *sql.Tx, rows []TaskRow) error {
	if len(rows) == 0 {
		return nil
	}
	at := make(map[string]int, len(rows))
	ids := make([]any, 0, len(rows))
	for i := range rows {
		at[rows[i].ID] = i
		ids = append(ids, rows[i].ID)
	}
	args := append([]any{string(RelationWaitingOn)}, ids...)
	found, err := tx.QueryContext(ctx, blockersStatement(len(ids)), args...)
	if err != nil {
		return fmt.Errorf("worklet: read the dependencies of a page: %w", err)
	}
	defer func() { _ = found.Close() }()
	for found.Next() {
		var task string
		var edge Blocker
		var oneSided, oneSidedFinal, open, dead, collision int
		if err := found.Scan(&task, &edge.ID, &oneSided, &oneSidedFinal,
			&open, &dead, &edge.Key, &collision); err != nil {
			return fmt.Errorf("worklet: read a dependency edge: %w", err)
		}
		i, ok := at[task]
		if !ok {
			continue
		}
		edge.Open = open == 1
		edge.OneSided = oneSided == 1
		edge.OneSidedFinal = oneSidedFinal == 1
		edge.Dead = open == 1 && dead == 1
		edge.KeyCollision = collision == 1
		rows[i].WaitingOn = append(rows[i].WaitingOn, edge)
	}
	if err := found.Err(); err != nil {
		return fmt.Errorf("worklet: walk the dependency edges: %w", err)
	}
	return nil
}

// countHint counts to the ceiling and stops, reporting whether it stopped.
//
// IT COUNTS ONE PAST THE CEILING and clamps: only the extra row tells exactly
// [TotalHintCeiling] from more.
func countHint(ctx context.Context, tx *sql.Tx, set taskSet) (int, bool, error) {
	counted, capped, err := countCapped(ctx, tx, set.from, set.where, set.args)
	if err != nil {
		return 0, false, fmt.Errorf("worklet: count the answer: %w", err)
	}
	return counted, capped, nil
}

// countCapped is [countHint] over any FROM clause: the rows `from` joins and
// `where` selects, counted one past [TotalHintCeiling] and clamped.
//
// ONE CEILING FOR EVERY COUNT this package answers, so no two totals disagree
// on where "more than we counted" begins.
func countCapped(ctx context.Context, tx *sql.Tx, from, where string,
	args []any) (int, bool, error) {

	query := cappedCountStatement(from, where)
	var n int
	if err := tx.QueryRowContext(ctx, query,
		append(append([]any{}, args...), TotalHintCeiling+1)...).Scan(&n); err != nil {
		return 0, false, err
	}
	counted, capped := capHint(n)
	return counted, capped, nil
}

// cappedCountStatement counts the rows `from` joins and `where` selects up to
// the bound its one trailing placeholder takes — the statement [countCapped]
// and the group-breadth gate count with, named for the plan test.
func cappedCountStatement(from, where string) string {
	return `SELECT COUNT(*) FROM (SELECT 1 FROM ` + from + ` WHERE ` + where +
		` LIMIT ?)`
}

// capHint clamps a count taken one past the ceiling, and says whether it was
// clamped.
//
// SEPARATE FROM THE QUERY so the boundary is testable without seeding ten
// thousand and one tasks.
func capHint(counted int) (int, bool) {
	if counted > TotalHintCeiling {
		return TotalHintCeiling, true
	}
	return counted, false
}

// readCheckpoint reads this node's own position and applied prefix, both
// packed, inside the caller's snapshot.
//
// TWO NUMBERS, because the checkpoint moves past a record this node RETAINS
// and only the applied prefix stops below it.
//
// THE FRAMEWORK'S RULE ([statelog.PrefixIn]), never a copy, so every domain's
// feed means the same thing by the two numbers.
func readCheckpoint(ctx context.Context, tx *sql.Tx) (position, applied uint64, err error) {
	prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
	if err != nil {
		return 0, 0, fmt.Errorf("worklet: read the checkpoint: %w", err)
	}
	return uint64(prefix.Settled.Packed()), uint64(prefix.Applied().Packed()), nil
}

// coverageOf probes whether this node holds a record it cannot decode whose
// scope could intersect one read.
//
// IT NAMES WHAT IS AFFECTED, NOT IN WHICH DIRECTION, which this build cannot
// decode: rows may be missing, stale or due to leave.
//
// A SCOPE THE CALLER BUILT, because a detail read's scope is one OBJECT: a
// board's container probe would warn on every task over one undecodable
// record. A board's probe is the framework's ([statelog.Reader]).
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
		return nil, fmt.Errorf("worklet: probe the answer's coverage: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return &Incomplete{
		Records: count,
		From:    statelog.Unpack(trackerStream, lowest.Int64),
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

// trackerStream is the name of the log this domain's rows are keyed to, read
// from the domain's declaration so there is one spelling.
var trackerStream = Domain{}.Stream().Name

// placeholders is a bound-parameter list of n slots.
//
// With its own column names, the only thing this package composes into SQL
// ([compile]).
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// joinAnd is a WHERE clause's conjunction, over at least one clause.
func joinAnd(clauses []string) string {
	out := clauses[0]
	for _, clause := range clauses[1:] {
		out += " AND " + clause
	}
	return out
}

// readPriorityList reads one person's stored order.
//
// A MISSING PERSON IS AN EMPTY LIST, not an error: "no row" is everyone's
// ordinary state until given a priority, and a refusal would fail turn starts.
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
// The sequence lives in a JSON array on another object, with no column to
// sort by, and a cursor over a CASE built from it would shift on every
// reorder. Bounded at [MaxPriorities] rows.
//
// A ROW THE LIST DOES NOT NAME KEEPS ITS PLACE at the end: a caller combining
// `priorities=` with another filter still asked for it.
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
