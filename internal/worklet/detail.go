package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reading ONE task is not a board query with a filter: a board returns only
// the columns a card renders, over many rows, while one task's answer is
// everything about it, from several tables. What the two share is the
// COVERAGE they report (read level, how far this node has applied, what it
// could not account for), so a lagging node never looks caught up here.

// ErrNoTask reports a task this node has no row for.
//
// Its own sentinel: a tool says "no such task" and an API 404, and neither
// should when this node has merely not applied the create yet; the detail read
// tells those apart ([TaskDetail.Complete]).
var ErrNoTask = errors.New("worklet: no such task")

// ErrNoProject reports a project this node has no row for: an empty listing
// for an unknown key would tell a model the project is empty when it mistyped
// the key.
var ErrNoProject = errors.New("worklet: no such project")

// ErrNoType reports a `for_type` that names no type this company files.
//
// Not [ErrNoProject]: the project exists and the narrowing argument is wrong,
// so a reader classifying by sentinel must not answer "no such project" (a
// 404, a dead link). The refusal lists the types the caller may name.
var ErrNoType = errors.New("worklet: no such type")

// ErrNoComment reports a comment this node has no row for on that task: an id
// not on the named task is a mistyped id, not an empty thread.
var ErrNoComment = errors.New("worklet: no such comment")

// CommentThread is the conversation around one comment on a task: what
// replied to it, and what answered it when it is an ask.
//
// Narrowed in the query, never filtered after the page, so the cursor is
// present only when an older page of THIS conversation exists.
type CommentThread struct {
	// Root is the comment's id. Empty is no narrowing: the whole task.
	Root string

	// WithRoot keeps the root itself in the thread, for a reader that is
	// not already showing it in full. Ignored without a Root.
	WithRoot bool
}

// DetailWants says which parts of the answer to assemble: explicit, because
// the parts cost very differently (the task is one indexed read; comments and
// history grow for the task's life).
type DetailWants struct {
	Comments bool
	History  bool
	Links    bool

	// Fields asks for the task's custom-field values ANNOTATED with the
	// slug, name and type that explain each, and the two non-live states
	// a filter excludes (detailfields.go says why the document is not enough).
	Fields bool

	// HistoryLimit caps the feed, newest first. Zero takes
	// [DetailHistoryDefault].
	HistoryLimit int

	// CommentCursor pages the thread: a page's own cursor, handed back
	// unchanged. Empty starts at the newest; anything else is refused
	// ([ErrBadCursor]).
	CommentCursor string

	// CommentThread narrows the pages to the conversation around one
	// comment. Its zero value pages every comment on the task.
	CommentThread CommentThread

	// CommentLimit is how many comments one page holds. Zero takes
	// [DetailComments]; above [MaxCommentPage] is held to it. A caller
	// walking a thread (work_comments) asks for more, for fewer round trips.
	CommentLimit int

	// Units resolves the task's two unit references against the chart
	// ([Units], [TaskDetail.Units]). Nil renders both unresolved: honest
	// for a surface with no chart, a WIRING MISTAKE on one with a chart,
	// since `resolved: false` reports a team the chart no longer has. Not
	// a want flag: resolving costs no query, and a flag could be forgotten.
	//
	// Not serialised (an interface encodes as `{}`): a surface sending this
	// across a wire has the far side attach ITS OWN chart (internal/estate).
	Units Units `json:"-"`

	// Clock is the company's calendar the due date is stood against
	// ([TaskDetail.DueStanding]). Nil answers no standing, never a guess
	// on the process's own clock and zone, which would put nodes a day apart.
	Clock *DayClock

	// Comment names ONE comment to read, and REPLACES the page — the read
	// that answers "that comment, and nothing else", by id.
	Comment string
}

// DayClock is an instant and the company's zone: what "today" is, on the
// calendar every due band, overdue mark and `due=` token is cut on.
type DayClock struct {
	Now time.Time

	// Zone is never encoded (a *time.Location encodes as nothing); a read
	// crossing to another node carries its NAME beside the clock instead
	// (internal/estate's taskArgs).
	Zone *time.Location `json:"-"`
}

// DueStanding is where a task's due date stands against the company's today.
// It is on the answer, never re-derived by a renderer in the reader's own
// clock, so every screen agrees on whether a task is late.
type DueStanding struct {
	// Days is whole CALENDAR days from the company's today to the due
	// date's day, negative when the date has passed: 0 is due today, 1
	// tomorrow, -2 two days ago. Counted on labels rather than instants,
	// so a day DST made 23 or 25 hours long is still one day.
	Days int `json:"days"`

	// Overdue is [TaskRow.Overdue]'s predicate exactly — open work whose
	// due instant is before the company's midnight today — so the rail and
	// the row it was opened from cannot disagree. A finished task two days
	// past its date is not overdue; it is done.
	Overdue bool `json:"overdue,omitempty"`
}

// dueStanding stands a task's due date against clock, or answers nothing
// where there is no clock or no date.
func dueStanding(clock *DayClock, task Task) *DueStanding {
	if clock == nil || task.DueAt == nil {
		return nil
	}
	today := period.At(period.Day, clock.Now, clock.Zone)
	due := period.At(period.Day, *task.DueAt, clock.Zone)
	from, errFrom := time.Parse(time.DateOnly, today.Label)
	to, errTo := time.Parse(time.DateOnly, due.Label)
	if errFrom != nil || errTo != nil {
		// A DATE A LABEL CANNOT SPELL — past year 9999 in this zone —
		// has no standing to report rather than a wrong one.
		return nil
	}
	return &DueStanding{
		Days:    int(to.Sub(from).Hours() / 24),
		Overdue: task.StatusGroup.Open() && task.DueAt.Before(today.Start),
	}
}

// DetailHistoryDefault is how many history rows a detail read returns when the
// caller says nothing: the activity panel's first screen. A cap, not a page:
// `work_activity` pages with a cursor, and an unbounded read would put a
// task's whole life in one tool result.
const DetailHistoryDefault = 50

// TaskDetail is one task and everything asked for beside it.
type TaskDetail struct {
	Task Task `json:"task"`

	Comments []Comment      `json:"comments,omitempty"`
	History  []HistoryEntry `json:"history,omitempty"`

	// Keys names the tasks this answer's HISTORY points at, id to item key:
	// the map [ActivityAnswer.Keys] carries, resolved by the same walk so
	// the two reads name one delta alike. A delta names a relation's other
	// end by ID, because a key is a fact about ANOTHER task's row and
	// `tracker_history` is written once inside this domain's identity
	// claim (a node lacking that task would store a different string for
	// ever); this map is what lets a renderer print keys, not uuids.
	//
	// Only with [DetailWants.History]. An id this node holds no row for is
	// absent, and a renderer falls back to the id.
	Keys map[string]string `json:"keys,omitempty"`

	// CommentsCursor pages the thread, empty when this page is all of it.
	// A cursor rather than a count: the caller asks "is there more", and a
	// total would rescan a table that grows for the task's life.
	CommentsCursor string `json:"comments_cursor,omitempty"`

	// Fields are the task's custom-field values with the declaration that
	// explains each one, and with the values nothing explains reported as
	// exactly that. The raw map stays on [TaskDetail.Task] — it is the
	// record — and this is the reader's view of it.
	Fields []FieldValue `json:"fields,omitempty"`

	// Units is the task's two unit references RESOLVED against the chart,
	// as [TaskDetail.Fields] is the custom-field map's reader view: the
	// strings on [TaskDetail.Task] are the record and hold the unit's KEY
	// (an id chosen to survive a rename, which nobody reads); this is the
	// name a person reads.
	//
	// A pointer: a task filed into no unit reports an ABSENCE, as
	// [LastChange] does for a project nobody has filed work into.
	Units *TaskUnits `json:"units,omitempty"`

	// Links are BOTH DIRECTIONS, so a reader sees "blocks" and "blocked
	// by" without a second query and without knowing which end authored
	// which.
	Links []DetailLink `json:"links,omitempty"`

	// EntriesWithheld is how many atlas entries the task names that this
	// answer left out, for a reader who may not read the atlas
	// ([TaskDetail.WithoutEntries]); 0, stated, where it left none out.
	EntriesWithheld int `json:"entries_withheld"`

	// Parent is the task this one is filed under, as a reader names it
	// (key, title, status), so "Part of LEAD-12 · 2.4 release" needs no
	// second read. Read in the same transaction as the task, so the name
	// is never from a later instant. Absent for a top-level task and for
	// a parent this node holds no row for (the reader falls back to the id).
	Parent *TaskRef `json:"parent,omitempty"`

	// Blocked is [TaskRow.Blocked]'s predicate, an open dependency edge,
	// computed here rather than derived from Links (which do not say
	// whether a blocker is still open), so the item never contradicts its
	// board row and the browser holds no second definition of blocked.
	Blocked bool `json:"blocked,omitempty"`

	// KeyCollision is [TaskRow.KeyCollision] for this task, on the answer
	// because it is the applier's DERIVED column, not part of the record.
	// A screen needs it to hand the task onward (a link, a peek, a copy),
	// since the key opens the task that claimed it first. Read in the same
	// statement as `blocked`, so the two describe one instant.
	KeyCollision bool `json:"key_collision,omitempty"`

	// DueStanding is where the due date stands against the company's
	// today — see [DueStanding]. Only with [DetailWants.Clock], and absent
	// for a task with no due date.
	DueStanding *DueStanding `json:"due_standing,omitempty"`

	// ReassignmentBudget is [ReassignmentBudget], served beside the
	// counter it bounds ([Task.Reassignments]) so a screen saying "hand-off
	// 7 of 8" reads the limit from the engine that enforces it, never a
	// figure of its own.
	ReassignmentBudget int `json:"reassignment_budget"`

	// The coverage half, identical in meaning to a board's — see [Answer].
	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Address is the reference that opens the task this answer is about: its key,
// or its id where another task claimed the key first. See [ItemAddress].
func (d TaskDetail) Address() string {
	return ItemAddress(d.Task.ID, d.Task.Key, d.KeyCollision)
}

// Named is how prose names the task this answer is about. See [ItemNamed].
func (d TaskDetail) Named() string {
	return ItemNamed(d.Task.ID, d.Task.Key, d.KeyCollision)
}

// TaskRef is another task as a reader names it.
type TaskRef struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	// KeyCollision is the task's [TaskRow.KeyCollision]: its key is one
	// another task claimed first, so a link built from [TaskRef.Key] opens
	// that task and this one is reached by its id. See [ItemAddress].
	KeyCollision bool `json:"key_collision,omitempty"`

	Title  string `json:"title" untrusted:"name"`
	Status Status `json:"status"`
}

// taskRefStatement is [readTaskRef]'s read, named for the plan test.
const taskRefStatement = `SELECT key, key_collision, title, status FROM tracker_tasks WHERE id = ?`

// readTaskRef names one task, or nil for an id this node holds no row for.
func readTaskRef(ctx context.Context, tx *sql.Tx, id string) (*TaskRef, error) {
	ref := TaskRef{ID: id}
	var collision int
	err := tx.QueryRowContext(ctx, taskRefStatement, id).
		Scan(&ref.Key, &collision, &ref.Title, &ref.Status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("worklet: name task %s: %w", id, err)
	}
	ref.KeyCollision = collision == 1
	return &ref, nil
}

// TaskUnits is a task's two unit references as a reader renders them, both
// always: Filed is the team the work belongs to and never moves, Routing is
// whose lead hears about it now. Each carries what the ROW holds beside what
// the chart calls it today ([UnitRef]; [Units] for an unresolved one).
type TaskUnits struct {
	Filed   UnitRef `json:"filed"`
	Routing UnitRef `json:"routing"`
}

// DetailLink is one relation as a reader sees it.
type DetailLink struct {
	Kind  RelationKind `json:"kind"`
	Other string       `json:"other"`

	// Key, Title and Status are the other end's, resolved here so a
	// renderer does not fetch a row per link.
	Key    string `json:"key,omitempty"`
	Title  string `json:"title,omitempty" untrusted:"name"`
	Status Status `json:"status,omitempty"`

	// KeyCollision is the other end's [TaskRow.KeyCollision]: its key is
	// one another task claimed first, so a link built from [DetailLink.Key]
	// opens that task and this one is reached by [DetailLink.Other]. See
	// [ItemAddress]. Always false on a page link, which has no task row.
	KeyCollision bool `json:"key_collision,omitempty"`

	Note string `json:"note,omitempty" untrusted:"name"`

	// Derived marks the half nobody authored, so an editor knows which end
	// to change and a renderer can say so.
	Derived bool `json:"derived,omitempty"`

	// OneSided marks an edge whose mirror was never written, and
	// OneSidedFinal one whose mirror was refused permanently.
	OneSided      bool `json:"one_sided,omitempty"`
	OneSidedFinal bool `json:"one_sided_final,omitempty"`
}

// HistoryEntry is one change as a reader sees it.
type HistoryEntry struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	CommentID  string     `json:"comment_id,omitempty"`
	Excerpt    string     `json:"excerpt,omitempty" untrusted:"fence"`
	TurnID     string     `json:"turn_id,omitempty"`

	// Fields is what changed, as the notification snapshot recorded it.
	Fields map[string]any `json:"fields,omitempty" untrusted:"name"`

	// Reassignments is the task's hand-off counter as this commit left it:
	// the "3" of "hand-off 3 of 8" on a row that moved the assignee, the
	// zero a person's touch reset it to otherwise (migration 0028). ABSENT
	// on a row this node holds no count for, never an underived zero.
	Reassignments *int `json:"reassignments,omitempty"`

	// Quiet marks a commit that carried no notification at all (every
	// subtask a merge re-parents is quiet by construction), which an
	// activity feed renders differently. A loud commit may still have
	// reached nobody: recipients resolve against the live roster at wake
	// time, so this says what the change claimed, not what landed.
	Quiet bool `json:"quiet,omitempty"`

	// At is the EFFECTIVE instant: the fleet-agreed one rather than the
	// writer's own clock, so two nodes render one feed in one order.
	At time.Time `json:"at"`

	// LogSeq is where the change sits, which is the cursor a feed pages
	// on. It is a position on a stream, so it is never compared with one
	// from another.
	LogSeq uint64 `json:"log_seq"`
}

// detailFlagsStatement is [Reader.Task]'s read of the two badges a board row
// carries beside its own, named for the plan test.
const detailFlagsStatement = `
		SELECT EXISTS (SELECT 1 FROM tracker_task_deps d
		         WHERE d.task_id = ? AND d.blocker_open = 1),
		       COALESCE((SELECT key_collision FROM tracker_tasks
		                 WHERE id = ?), 0) = 1`

// Task reads one task by id or by key, with whatever else was asked for.
//
// Every part and the coverage probe are read in ONE transaction, so no comment
// appears under a task the answer says lacks it, and the completeness claim is
// about the state the rows came from.
func (r *Reader) Task(ctx context.Context, idOrKey string, want DetailWants,
	fresh statelog.Freshness) (TaskDetail, error) {

	idOrKey = strings.TrimSpace(idOrKey)
	if idOrKey == "" {
		return TaskDetail{}, fmt.Errorf("worklet: name a task by id or by key")
	}
	// THE CURSOR BEFORE THE READ, for the reason [feedPositions] gives: one
	// refused inside the transaction fails as the read does.
	after, refused := parseCommentCursor(want.CommentCursor)
	if refused != nil {
		return TaskDetail{}, refused
	}
	var out TaskDetail
	// A read that REPORTS its coverage rather than refusing: a record this
	// node cannot decode covering the task is served with
	// [TaskDetail.Incomplete] saying so, since a model or a person can act
	// on a caveat and a refusal would hand them nothing during the rolling
	// upgrade that puts such records on the wire. A write still refuses.
	//
	// The scope is the task itself, formed only once the reference is
	// resolved, so the framework resolves it in the answer's own
	// transaction ([taskReadScope]). The whole freshness, not the level
	// alone: a staleness bound is this node's lag, and a floor is where
	// the caller's own write landed.
	served, err := r.log.Read(ctx, fresh.Resolved(func(ctx context.Context,
		tx *sql.Tx) (statelog.ScopeSet, error) {
		return taskReadScope(ctx, tx, idOrKey)
	}, true), func(tx *sql.Tx) error {
		id, err := resolveTaskID(ctx, tx, idOrKey)
		if err != nil {
			return err
		}
		task, err := readTaskDocument(ctx, tx, id)
		if err != nil {
			return err
		}
		out.Task = task
		if task.Parent != nil && *task.Parent != "" {
			if out.Parent, err = readTaskRef(ctx, tx, *task.Parent); err != nil {
				return err
			}
		}
		switch {
		case want.Comment != "":
			// Read whether or not `comments` was asked for: naming one
			// comment IS asking for it, and requiring both would answer a
			// silently empty thread to a caller that forgot one.
			if out.Comments, err = readComment(ctx, tx, id, want.Comment); err != nil {
				return err
			}
		case want.Comments:
			out.Comments, out.CommentsCursor, err = readComments(ctx, tx, id,
				want.CommentThread, after, commentPage(want.CommentLimit))
			if err != nil {
				return err
			}
		}
		if want.History {
			limit := want.HistoryLimit
			if limit <= 0 {
				limit = DetailHistoryDefault
			}
			if out.History, err = readHistory(ctx, tx, id, limit); err != nil {
				return err
			}
			// In the same transaction as the rows it labels, so no
			// key is from a later instant than the history.
			if out.Keys, err = counterpartyKeys(ctx, tx,
				historyDeltaSides(out.History),
				r.db.Caps().MaxVariables); err != nil {
				return err
			}
		}
		if want.Links {
			if out.Links, err = readLinks(ctx, tx, id); err != nil {
				return err
			}
		}
		if want.Fields {
			if out.Fields, err = readFieldValues(ctx, tx, task); err != nil {
				return err
			}
		}
		// The board row's own EXISTS and collision column, in this
		// transaction, so the item's badges and its row's cannot disagree
		// at one instant.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := tx.QueryRowContext(ctx, detailFlagsStatement,
			id, id).Scan(&out.Blocked, &out.KeyCollision); err != nil {
			return fmt.Errorf("worklet: read %s's open blockers: %w", id, err)
		}

		// The coverage, in the same transaction as the rows, so it
		// describes THIS answer.
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied

		// Scoped to the task, not the company: a deferred record about
		// another task must not warn on every task. Resolved as the
		// framework probed it, so the count is about exactly the paths
		// that decided completeness, the key's own address included.
		scope, err := taskReadScope(ctx, tx, idOrKey)
		if err != nil {
			return err
		}
		incomplete, err := coverageOf(ctx, tx, scope)
		if err != nil {
			return err
		}
		if incomplete != nil {
			out.Incomplete = incomplete
		} else {
			out.Complete = true
		}
		return nil
	})
	if err != nil {
		return TaskDetail{}, err
	}
	// Units resolve outside the transaction: the resolver is the CALLER's
	// code, and holding a read open across it would make it a lock on this
	// node's estate. It reads no row, so nothing needs keeping consistent.
	out.Units = taskUnits(want.Units, out.Task)
	out.DueStanding = dueStanding(want.Clock, out.Task)
	out.ReassignmentBudget = ReassignmentBudget
	// The level SERVED, never the one asked for, or a refused read and a
	// quorum-barrier read would report the same word.
	out.Level = served.Level
	if !served.Complete {
		out.Complete = false
	}
	return out, nil
}

// taskReadScopeStatement is [taskReadScope]'s resolution, named for the plan
// test.
const taskReadScopeStatement = `
		SELECT id, project_key FROM tracker_tasks WHERE id = ?
		UNION ALL
		SELECT t.id, t.project_key FROM tracker_task_keys k
		  JOIN tracker_tasks t ON t.id = k.task_id WHERE k.key = ?
		UNION ALL
		SELECT id, project_key FROM tracker_tasks WHERE key = ?
		LIMIT 1`

// taskReadScope is what a point read of one task is about, resolved from the
// reference the caller named: the task's own path under its project, plus,
// for a key, the key's address term ([keyTerm]), since which task a key opens
// is decided by records filed under the KEY (claims, re-keys, purges), not
// under the task.
//
// Resolved in [resolveTaskID]'s order (id, key directory, a row's own key) so
// the scope probed is that of the task the read returns. A reference resolving
// to nothing still bounds a scope, since a deferred create may be the answer:
// a key names its project, an id nothing narrower than the domain.
func taskReadScope(ctx context.Context, tx *sql.Tx, idOrKey string) (statelog.ScopeSet, error) {
	ref := strings.TrimSpace(idOrKey)
	isID := uuid.Validate(ref) == nil
	var named *ScopeTerm
	if !isID {
		term := keyTerm(strings.ToUpper(ref))
		named = &term
	}
	var id, project string
	err := tx.QueryRowContext(ctx, taskReadScopeStatement,
		ref, strings.ToUpper(ref), strings.ToUpper(ref)).Scan(&id, &project)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if named == nil {
			return statelog.ScopeSet{Paths: []string{
				ScopeTerm{Kind: TermDomain}.Path(),
			}}, nil
		}
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermContainer, ID: named.Container}.Path(),
		}}.Normalised(), nil
	case err != nil:
		return statelog.ScopeSet{}, fmt.Errorf("worklet: resolve %q: %w", idOrKey, err)
	}
	paths := []string{ScopeTerm{Kind: TermObject, Container: project, ID: id}.Path()}
	if named != nil {
		paths = append(paths, named.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised(), nil
}

// resolveTaskIn is [resolveTaskID] with the project the task is filed in,
// resolved in the same order by the one statement [taskReadScope] resolves a
// reference with.
func resolveTaskIn(ctx context.Context, tx *sql.Tx, idOrKey string) (id, project string, err error) {
	key := strings.ToUpper(idOrKey)
	err = tx.QueryRowContext(ctx, taskReadScopeStatement, idOrKey, key, key).
		Scan(&id, &project)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("%w: %s", ErrNoTask, idOrKey)
	case err != nil:
		return "", "", fmt.Errorf("worklet: resolve %q: %w", idOrKey, err)
	}
	return id, project, nil
}

// resolveTaskStatement is [resolveTaskID]'s one statement, named for the plan
// test.
const resolveTaskStatement = `
		SELECT id FROM tracker_tasks WHERE id = ?
		UNION ALL
		SELECT task_id FROM tracker_task_keys WHERE key = ?
		UNION ALL
		SELECT id FROM tracker_tasks WHERE key = ?
		LIMIT 1`

// resolveTaskID turns an id or a key, current or former, into an id.
//
// A former key resolves too: a task moved between projects keeps its old key
// as an alias so every link, message and comment naming it still opens it. A
// key resolves through the directory first, so a key two tasks hold opens the
// one that claimed it on every data node, not whichever row an index returns;
// the task row is the fallback for a key whose claim is not written yet.
func resolveTaskID(ctx context.Context, tx *sql.Tx, idOrKey string) (string, error) {
	var id string
	key := strings.ToUpper(idOrKey)
	err := tx.QueryRowContext(ctx, resolveTaskStatement, idOrKey, key, key).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s", ErrNoTask, idOrKey)
	case err != nil:
		return "", fmt.Errorf("worklet: resolve %q: %w", idOrKey, err)
	}
	return id, nil
}

// keyOpensAnother is the SQL predicate "opening this key reaches a task other
// than this id" ([resolveTaskID]'s answer), over a row carrying a key and a
// task id side by side.
//
// The directory alone suffices: the resolver asks it first, and a task's
// current key always has its row ([Applier.maintainKeys], [handOnKey]). An
// empty key opens nothing and a key beside an empty id names no task, so
// neither is a collision; asked with an empty id, every directory row would
// count as "another". It serves a key that is not
// the task's current one (a notice stores the key of when it was written); a
// current key reads `key_collision`, derived from this same directory.
func keyOpensAnother(key, id string) string {
	return `(` + key + ` <> '' AND ` + id + ` <> '' AND EXISTS (
	          SELECT 1 FROM tracker_task_keys kd
	          WHERE kd.key = ` + key + ` AND kd.task_id <> ` + id + `))`
}

// taskDocumentStatement is [readTaskDocument]'s read, named for the plan test.
const taskDocumentStatement = `
		SELECT document, status_entered_at, rank, spend_turns, spend_rounds,
		       spend_input, spend_output, spend_cache_read, spend_cache_write,
		       spend_wall_ms, spend_tokens, spend_workers, spend_sent_back
		  FROM tracker_tasks WHERE id = ?`

// readTaskDocument decodes the task's own record from the DOCUMENT, not the
// columns: they are a cache extracted for filtering and sorting, and a rebuild
// from them would strip a newer node's fields during a rolling upgrade.
//
// Three exceptions come from their columns. The spend: a turn adds to the
// columns, never the document ([Applier.applyTurn]). `rank`: the column every
// board orders by ([Applier.applyRankOrder] writes both), so a board and a
// detail read name one place. `status_entered_at`: the applier derives it from
// the history after the document is marshalled, so the document always holds
// zero, and writing it in would rewrite the whole document per status change.
func readTaskDocument(ctx context.Context, tx *sql.Tx, id string) (Task, error) {
	var body []byte
	var entered int64
	var rank string
	var spend Spend
	err := tx.QueryRowContext(ctx, taskDocumentStatement,
		id).Scan(&body, &entered, &rank, &spend.Turns, &spend.Rounds, &spend.Input,
		&spend.Output, &spend.CacheRead, &spend.CacheWrite, &spend.WallMs,
		&spend.Tokens, &spend.Workers, &spend.SentBack)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, fmt.Errorf("%w: %s", ErrNoTask, id)
	case err != nil:
		return Task{}, fmt.Errorf("worklet: read task %s: %w", id, err)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return Task{}, fmt.Errorf("worklet: decode task %s: %w", id, err)
	}
	// ZERO STAYS ZERO rather than becoming the epoch: a task whose status
	// has never moved has no entered instant, and `omitzero` is what says
	// so. time.UnixMicro(0) is 1970 and would render as a date.
	if entered != 0 {
		task.StatusEnteredAt = store.DecodeTime(entered)
	}
	// AND THE RANK, from the column every board orders by — see the doc.
	task.Rank = Rank(rank)
	// AND THE SPEND, from the columns a turn adds to — see the doc.
	task.Spend = spend
	return task, nil
}

// DetailComments is how many comments one detail read returns: past twenty a
// reader is scrolling rather than catching up, and the cursor pages on. The
// thread is the ONE detail collection with no bound of its own, so it decides
// whether the answer fits [ToolAnswerBytes].
const DetailComments = 20

// MaxCommentPage is the most comments one page may hold: the ceiling every
// other paged tracker read holds ([inbox.MaxRows], [MaxFeedPage],
// [MaxSearchLimit]). What a page WEIGHS is [CommentPageBytes]'s to bound.
const MaxCommentPage = 50

// commentPage is the page size a detail read asked for, defaulted and held
// to [MaxCommentPage].
func commentPage(asked int) int {
	switch {
	case asked <= 0:
		return DetailComments
	case asked > MaxCommentPage:
		return MaxCommentPage
	default:
		return asked
	}
}

// CommentPageBytes is how much comment text one THREAD PAGE carries, in WHOLE
// comments, never one cut to fit: a cut comment reads as one that ended there.
// A page holds as many as fit, at least one ([MaxCommentBody] bounds it), and
// its cursor continues where it stopped.
//
// 16 KiB is a quarter of a tool answer's 64 KiB: beside a whole description
// ([MaxBody]) it leaves history, links and fields their room, and the answer's
// own ceiling refuses (naming `include`) rather than cuts in the rare worst case.
const CommentPageBytes = 16 << 10

// readComments is a page of the thread, read NEWEST FIRST so a page starts at
// the end a reader catching up needs, then reversed so the caller holds it in
// written order. A removed comment keeps its row with a blank body, so replies
// still resolve; a renderer reads its `removed` flag. A [CommentThread] root
// narrows the page to that conversation.
func readComments(ctx context.Context, tx *sql.Tx, taskID string, thread CommentThread,
	after commentCursor, limit int) ([]Comment, string, error) {
	return readCommentPage(ctx, tx, taskID, thread, after, limit, CommentPageBytes)
}

// readCommentPage is [readComments] with the byte budget a parameter, so the
// bound is exercised directly. The budget ends a page early, never cuts a
// comment, and the cursor names the last comment RETURNED, as with the count.
func readCommentPage(ctx context.Context, tx *sql.Tx, taskID string, thread CommentThread,
	after commentCursor, limit, maxBytes int) ([]Comment, string, error) {

	// One more than the page: how the cursor knows a next page exists
	// without a count over a growing table.
	rows, err := tx.QueryContext(ctx, `
		SELECT document, created_at, id FROM tracker_comments
		WHERE task_id = ? AND (? = '' OR (created_at, id) < (?, ?))
		  AND (? = '' OR reply_to = ? OR answers = ? OR (? AND id = ?))
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, taskID, after.id, after.at, after.id,
		thread.Root, thread.Root, thread.Root, thread.WithRoot, thread.Root,
		limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("worklet: read the thread on %s: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []Comment
		next string
		// The LAST ROW THIS PAGE RETURNS — the oldest on it — which is
		// what the next page continues strictly below.
		lastAt int64
		lastID string
		// weight is the comment text on the page so far.
		weight int
	)
	for rows.Next() {
		var body []byte
		var at int64
		var id string
		if err := rows.Scan(&body, &at, &id); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			// The extra row says there is a next page; the cursor is
			// the last row RETURNED, since the query reads strictly
			// below it and naming this row would lose it at every
			// page boundary.
			next = formatCommentCursor(lastAt, lastID)
			break
		}
		var comment Comment
		if err := json.Unmarshal(body, &comment); err != nil {
			return nil, "", fmt.Errorf("worklet: decode a comment on %s: %w",
				taskID, err)
		}
		// Whole, and the page ends before a comment past its budget, here
		// rather than at a caller that could forget. The first is always
		// taken, so a page is never empty while the thread is not.
		if len(out) > 0 && weight+len(comment.Body) > maxBytes {
			next = formatCommentCursor(lastAt, lastID)
			break
		}
		weight += len(comment.Body)
		out = append(out, comment)
		lastAt, lastID = at, id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	slices.Reverse(out)
	return out, next, nil
}

// readComment is ONE comment by id, WHOLE: the write refuses a body past
// [MaxCommentBody], so it fits a tool answer. Scoped to the task, so an id from
// another item answers [ErrNoComment]; the id is the primary key, and the
// task_id term is the check, not the lookup.
func readComment(ctx context.Context, tx *sql.Tx, taskID, commentID string) ([]Comment, error) {
	commentID = strings.TrimSpace(commentID)
	var body []byte
	err := tx.QueryRowContext(ctx, `
		SELECT document FROM tracker_comments
		WHERE id = ? AND task_id = ?`, commentID, taskID).Scan(&body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s on %s", ErrNoComment, commentID, taskID)
	case err != nil:
		return nil, fmt.Errorf("worklet: read comment %s on %s: %w",
			commentID, taskID, err)
	}
	var comment Comment
	if err := json.Unmarshal(body, &comment); err != nil {
		return nil, fmt.Errorf("worklet: decode comment %s on %s: %w",
			commentID, taskID, err)
	}
	return []Comment{comment}, nil
}

// The comment cursor is the pair the page is ordered by, because an instant
// alone is not unique: two comments share a created_at routinely, and a cursor
// that compared one would skip whichever the page boundary fell between.
func formatCommentCursor(at int64, id string) string {
	return strconv.FormatInt(at, 10) + ":" + id
}

// commentCursor is a thread page's place: the instant and the id of the last
// comment it returned. Its zero value is the newest page's, which has none.
type commentCursor struct {
	at int64
	id string
}

// parseCommentCursor reads [formatCommentCursor] back, and refuses anything
// else: read leniently, a cursor that was not one bounded the page below the
// zero instant and answered an empty thread as its last page.
func parseCommentCursor(raw string) (commentCursor, error) {
	if raw == "" {
		return commentCursor{}, nil
	}
	at, id, ok := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || id == "" {
		return commentCursor{}, fmt.Errorf("%w: %q is not a comment cursor; pass "+
			"a page's own cursor back unchanged", ErrBadCursor, raw)
	}
	return commentCursor{at: n, id: id}, nil
}

// taskHistoryStatement is [readHistory]'s one statement, named so the plan
// test explains exactly what the reader issues.
const taskHistoryStatement = `
		SELECT id, kind, actor, actor_kind, operator_id, comment_id,
		       excerpt, fields_json, turn_id, notified, effective_at,
		       log_seq, reassignments
		FROM tracker_history
		WHERE subject_id = ?
		ORDER BY log_seq DESC
		LIMIT ?`

// readHistory is the activity feed, NEWEST FIRST and capped.
func readHistory(ctx context.Context, tx *sql.Tx, taskID string, limit int) ([]HistoryEntry, error) {
	rows, err := tx.QueryContext(ctx, taskHistoryStatement, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the history of %s: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	// Sized to the page, as [readActivity] does, and never nil
	// ([readProjectRows]).
	out := make([]HistoryEntry, 0, limit)
	for rows.Next() {
		var e HistoryEntry
		var actorKind, fields string
		var notified int
		var effective, seq int64
		var handOffs sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Actor, &actorKind, &e.OperatorID,
			&e.CommentID, &e.Excerpt, &fields, &e.TurnID, &notified,
			&effective, &seq, &handOffs); err != nil {
			return nil, err
		}
		if handOffs.Valid {
			n := int(handOffs.Int64)
			e.Reassignments = &n
		}
		e.ActorKind = AuthorKind(actorKind)
		e.Quiet = notified == 0
		e.At = store.DecodeTime(effective)
		e.LogSeq = uint64(seq)
		if fields != "" && fields != "{}" {
			// A FIELD SET THAT DOES NOT DECODE IS DROPPED, not raised:
			// it is the render half of one change, and refusing the
			// whole feed because one card cannot show its diff would
			// take away the rest of the answer as well.
			_ = json.Unmarshal([]byte(fields), &e.Fields)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// linksStatement is one half of [readLinks], named for the plan test: the
// edges this task owns, or with mirror the edges pointing at it, each with the
// task at its other end.
func linksStatement(mirror bool) string {
	other, own := "r.other_id", "r.task_id"
	if mirror {
		other, own = "r.task_id", "r.other_id"
	}
	return `
		SELECT r.kind, ` + other + `, r.derived, r.one_sided, r.one_sided_final, r.note,
		       COALESCE(t.key, ''), COALESCE(t.title, ''), COALESCE(t.status, ''),
		       COALESCE(t.key_collision, 0)
		FROM tracker_relations r
		LEFT JOIN tracker_tasks t ON t.id = ` + other + `
		WHERE ` + own + ` = ?
		ORDER BY r.kind, ` + other
}

// readLinks is both directions of every relation, in two queries rather than a
// UNION: rows keyed on this task are the ones it owns and rows pointing AT it
// are the mirror (`derived`), which a merged result could no longer tell apart.
func readLinks(ctx context.Context, tx *sql.Tx, taskID string) ([]DetailLink, error) {
	// NEVER NIL — see [readHistory] and [readProjectRows].
	out := []DetailLink{}
	for _, q := range []struct {
		statement string
		derived   bool
	}{
		{linksStatement(false), false},
		{linksStatement(true), true},
	} {
		rows, err := tx.QueryContext(ctx, q.statement, taskID)
		if err != nil {
			return nil, fmt.Errorf("worklet: read the links on %s: %w", taskID, err)
		}
		for rows.Next() {
			var link DetailLink
			var kind, status string
			var derived, oneSided, oneSidedFinal, collision int
			if err := rows.Scan(&kind, &link.Other, &derived, &oneSided,
				&oneSidedFinal, &link.Note, &link.Key, &link.Title, &status,
				&collision); err != nil {
				_ = rows.Close()
				return nil, err
			}
			link.KeyCollision = collision == 1
			link.Kind = RelationKind(kind)
			link.Status = Status(status)
			// The mirror is derived whatever its column says: this side
			// is the end that did not author the edge, which is what a
			// renderer asking "may I edit this" needs.
			link.Derived = derived == 1 || q.derived
			link.OneSided = oneSided == 1
			link.OneSidedFinal = oneSidedFinal == 1
			out = append(out, link)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
