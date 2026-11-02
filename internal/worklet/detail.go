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

// Reading ONE task, and why it is not a board query with a filter.
//
// A board answers "what is there" over many rows, narrowly: it returns the
// columns a card renders and nothing else, because a hundred rows carrying
// their whole documents is a page nobody can afford. One task is the opposite
// question — "tell me everything about this" — and every part of the answer
// comes from a different table.
//
// So they are two reads with two shapes, and the thing they share is the
// COVERAGE they report. Both answer at a read level, both say how far this
// node has applied, and both name what they could not account for. A detail
// read that skipped that would be the one screen in the product where a
// lagging node looks identical to a caught-up one.

// ErrNoTask reports a task this node has no row for.
//
// ITS OWN SENTINEL, because the caller's answer differs: a tool says "no such
// task" to a model, an API says 404, and neither should say either when what
// actually happened is that this node has not applied the record that creates
// it. The detail read distinguishes those — see [TaskDetail.Complete].
var ErrNoTask = errors.New("worklet: no such task")

// ErrNoProject reports a project this node has no row for.
//
// ITS OWN SENTINEL beside [ErrNoTask], for the same reason and with the same
// caveat: a project reader that answered an empty listing for an unknown key
// would tell a model the project is empty when what actually happened is that
// it typed the key wrong.
var ErrNoProject = errors.New("worklet: no such project")

// ErrNoType reports a `for_type` that names no type this company files.
//
// NOT [ErrNoProject], which is what it used to wrap: the project the caller
// named exists, and it is the NARROWING argument that is wrong — so every
// reader classifying by the sentinel answered "there is no such project" (a
// 404 on the query surface, a dead link to a person) about a project that was
// right there. The fix a caller needs is the argument, and the refusal lists
// the types it may name.
var ErrNoType = errors.New("worklet: no such type")

// ErrNoComment reports a comment this node has no row for on that task.
//
// ITS OWN SENTINEL beside the two above, and the caller's answer differs
// again: naming a comment that is not on the task the caller named is a
// mistyped id rather than an empty thread, and a reader told "no comments"
// would go looking for the wrong thing.
var ErrNoComment = errors.New("worklet: no such comment")

// CommentThread is the conversation around one comment on a task: what
// replied to it, and what answered it when it is an ask.
//
// PAGED BY THE READ, never filtered after it. A pane that drew one question's
// replies used to page the task's whole thread and keep the replies, so its
// cursor answered "the task has older comments" and the pane offered earlier
// replies to a question nobody had replied to. Narrowed in the query, the
// cursor is present only when an older page of THIS conversation exists.
type CommentThread struct {
	// Root is the comment's id. Empty is no narrowing: the whole task.
	Root string

	// WithRoot keeps the root itself in the thread, for a reader that is
	// not already showing it in full. Ignored without a Root.
	WithRoot bool
}

// DetailWants says which parts of the answer to assemble.
//
// EXPLICIT rather than "everything", because the parts have very different
// costs: the task itself is one indexed read, the comments are a thread that
// can be hundreds of rows, and the history is append-only and grows for the
// life of the task. A caller rendering a list of links does not want either.
type DetailWants struct {
	Comments bool
	History  bool
	Links    bool

	// Fields asks for the task's custom-field values ANNOTATED — each
	// with the slug, name and type that explain it, and with the two
	// non-live states a filter already excludes. See detailfields.go for
	// why the document alone is not an answer.
	Fields bool

	// HistoryLimit caps the feed, newest first. Zero takes
	// [DetailHistoryDefault].
	HistoryLimit int

	// CommentCursor pages the thread. Empty starts at the newest.
	CommentCursor string

	// CommentThread narrows the pages to the conversation around one
	// comment. Its zero value pages every comment on the task.
	CommentThread CommentThread

	// CommentLimit is how many comments one page holds. Zero takes
	// [DetailComments], the detail's own page; anything above
	// [MaxCommentPage] is held to it. A caller that walks a thread
	// rather than catching up on one — the work_comments question —
	// asks for more per page so a long thread is fewer round trips.
	CommentLimit int

	// Units resolves the task's two unit references against the chart —
	// see [Units] and [TaskDetail.Units]. Nil renders both raw and
	// unresolved, which is honest for a surface holding no chart and a
	// WIRING MISTAKE on one that has one: `resolved: false` is the
	// finding "this names a team the chart no longer has", so a surface
	// that could have resolved and did not reports every task as orphaned.
	//
	// It is not one of the wants above: resolving costs no query, and a
	// flag would be one more thing a surface can forget while rendering
	// an id where a person expects their team's name.
	//
	// NOT SERIALISED: an interface a JSON encoder writes as `{}` and
	// cannot decode back. A surface that sends this across a wire has the
	// far side attach ITS OWN chart — see internal/estate.
	Units Units `json:"-"`

	// Clock is the company's calendar the task's due date is stood
	// against — see [TaskDetail.DueStanding]. Nil answers no standing,
	// which is honest for a caller that draws no due date (a tool reading
	// a task before it writes one) and never a guess on the process's own
	// clock and zone, which would put one node's answer a day out from
	// another's.
	Clock *DayClock

	// Comment names ONE comment to read, and REPLACES the page — the read
	// that answers "that comment, and nothing else", by id.
	Comment string
}

// DayClock is an instant and the company's zone: what "today" is, on the
// calendar every due band, overdue mark and `due=` token is cut on.
type DayClock struct {
	Now time.Time

	// Zone is never encoded — a *time.Location is a table of transitions
	// a JSON encoder writes as nothing at all — and a read that crosses to
	// another node carries its NAME beside the clock instead
	// (internal/estate's taskArgs).
	Zone *time.Location `json:"-"`
}

// DueStanding is where a task's due date stands against the company's today.
//
// ON THE ANSWER, never re-derived by a renderer: the task page's rail read
// "1d ago" in the reader's own clock for a task two company days late that
// every board and the calendar drew as overdue — two screens, one task, two
// answers about whether it was late.
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
// caller says nothing.
//
// FIFTY, which is the activity panel's own first screen. It is a cap rather
// than a page because the caller that wants more has `work_activity`, which
// pages properly with a cursor — and a detail read that grew without bound
// would put a task's whole life inside one tool result.
const DetailHistoryDefault = 50

// TaskDetail is one task and everything asked for beside it.
type TaskDetail struct {
	Task Task `json:"task"`

	Comments []Comment      `json:"comments,omitempty"`
	History  []HistoryEntry `json:"history,omitempty"`

	// Keys names the tasks this answer's HISTORY points at, id to item key
	// — the same map [ActivityAnswer.Keys] carries, asked of one task's own
	// rows, and resolved by the same walk so the two reads cannot name one
	// delta differently.
	//
	// It is here for exactly the reason it is there: a delta names the
	// other end of a relation by its ID, because a key is a fact about
	// ANOTHER task's row and `tracker_history` is inside this domain's
	// identity claim, written once and repaired by nothing — so a node
	// that had not applied that task would store a different string there
	// for ever. Neither side could fix "Parent: 1d573f85… → 50a01576…"
	// alone: the engine may not put a key on the record, and a surface
	// holds no map to resolve one with. Without it the company-wide log
	// resolved a re-parent to two keys while the item's own History tab —
	// the screen a reader opens to see what happened to THIS task —
	// printed two uuids for the same commit.
	//
	// ONLY WITH [DetailWants.History], since the history rows are what it
	// labels, and AN ID THIS NODE HOLDS NO ROW FOR IS SIMPLY ABSENT: a
	// renderer falls back to the id, which is the honest degradation.
	Keys map[string]string `json:"keys,omitempty"`

	// CommentsCursor pages the thread, and is empty when this page is the
	// whole of it.
	//
	// PRESENT RATHER THAN A COUNT, because the caller's question is "is
	// there more" and the answer that lets them act is the cursor itself —
	// a total would need a second scan over a table that grows for the
	// life of the task, to tell them something the cursor already says.
	CommentsCursor string `json:"comments_cursor,omitempty"`

	// Fields are the task's custom-field values with the declaration that
	// explains each one, and with the values nothing explains reported as
	// exactly that. The raw map stays on [TaskDetail.Task] — it is the
	// record — and this is the reader's view of it.
	Fields []FieldValue `json:"fields,omitempty"`

	// Units is the task's two unit references RESOLVED against the chart,
	// on exactly the terms [TaskDetail.Fields] is the custom-field map's
	// reader view: the strings stay on [TaskDetail.Task] because they are
	// the record, and this is what a person reads.
	//
	// It exists because what those strings hold is the unit's KEY — an id
	// on any company that gave its units one, which is a word chosen to
	// survive a rename precisely because nobody reads it. A screen
	// rendering the document raw showed `eng` where the same company's
	// board column said `Engineering`.
	//
	// A POINTER, because a task filed into no unit has nothing to resolve
	// and the honest report of that is an ABSENCE — the same rule
	// [LastChange] follows for a project nobody has filed work into.
	Units *TaskUnits `json:"units,omitempty"`

	// Links are BOTH DIRECTIONS, so a reader sees "blocks" and "blocked
	// by" without a second query and without knowing which end authored
	// which.
	Links []DetailLink `json:"links,omitempty"`

	// Parent is the task this one is filed under, as a reader names it:
	// its key, its title and where it stands. A task carries its parent's
	// ID — the record — and a page that says "Part of LEAD-12 · 2.4
	// release" needed a second read of the whole parent to say it.
	//
	// IN THE SAME TRANSACTION as the task, so the name cannot come from a
	// later instant than the child it heads. ABSENT for a top-level task,
	// and for a parent this node holds no row for — an id the reader falls
	// back to rather than a blank name.
	Parent *TaskRef `json:"parent,omitempty"`

	// Blocked is the SAME predicate [TaskRow.Blocked] carries — an open
	// dependency edge — computed here rather than derived from Links,
	// which say what the relations ARE and not whether any blocker is
	// still open.
	//
	// It is on the answer because a renderer that showed the badge on a
	// board row and could not show it on the item itself is one that
	// contradicts its own list a click later, and because deriving it in
	// the browser would be a second definition of blocked.
	Blocked bool `json:"blocked,omitempty"`

	// KeyCollision is [TaskRow.KeyCollision] for the task this answer is
	// about, on the ANSWER for [TaskDetail.Blocked]'s reason: it is the
	// applier's DERIVED column rather than a field of the record, and the
	// document is the record. A screen showing a flagged task needs it to
	// hand anything onward — a link back to the board, a peek, a call to
	// copy — because the task's key opens the task that claimed it first.
	// Read in the same statement as `blocked`, so the two describe one
	// instant.
	KeyCollision bool `json:"key_collision,omitempty"`

	// DueStanding is where the due date stands against the company's
	// today — see [DueStanding]. Only with [DetailWants.Clock], and absent
	// for a task with no due date.
	DueStanding *DueStanding `json:"due_standing,omitempty"`

	// ReassignmentBudget is [ReassignmentBudget], served beside the
	// counter it bounds ([Task.Reassignments]) so a screen saying "hand-off
	// 7 of 8" reads the limit from the engine that enforces it. The
	// dashboard used to hold its own figure — a warning at six against a
	// budget of eight — and a number stated twice is two numbers the day
	// either moves.
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

// TaskUnits is a task's two unit references as a reader renders them.
//
// BOTH HALVES, ALWAYS, because they answer different questions and a screen
// draws them side by side: Filed is the team the work belongs to and never
// moves, Routing is whose lead hears about it now. Each carries what the ROW
// holds beside what the chart calls it today — see [UnitRef], and [Units] for
// what an unresolved one means.
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

	// Reassignments is the task's hand-off counter as this commit left it
	// — the "3" of "hand-off 3 of 8" on a row that moved the assignee, and
	// the zero a person's touch reset it to on a row that did anything
	// else (migration 0028). ABSENT on a row this node holds no count for,
	// never a zero it did not derive.
	Reassignments *int `json:"reassignments,omitempty"`

	// Quiet marks a commit that ANNOUNCED NOTHING — one that carried no
	// notification at all. It is a fact about the change rather than
	// about its importance — every subtask a merge re-parents is quiet by
	// construction — and it is what an activity feed renders differently.
	//
	// A LOUD COMMIT MAY STILL HAVE REACHED NOBODY: who was told is
	// resolved against the live roster at wake time, and every candidate
	// may be the actor or may have left. This says what the change
	// claimed, not what landed.
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
// ONE READ TRANSACTION for every part and the coverage probe, which is what
// stops a comment appearing under a task the same answer says does not have
// it yet, and stops a completeness claim being made against state the rows
// were not read from.
func (r *Reader) Task(ctx context.Context, idOrKey string, want DetailWants,
	fresh statelog.Freshness) (TaskDetail, error) {

	idOrKey = strings.TrimSpace(idOrKey)
	if idOrKey == "" {
		return TaskDetail{}, fmt.Errorf("worklet: name a task by id or by key")
	}
	var out TaskDetail
	// A READ THAT REPORTS ITS COVERAGE rather than refusing: a record this
	// node cannot decode covering the task is served beside the rows with
	// [TaskDetail.Incomplete] saying so, because a detail read has callers
	// who can act on a caveat — a model is told to say it could not check,
	// a person sees the task with a banner — and a refusal would hand both
	// nothing at all during exactly the rolling upgrade that puts such a
	// record on the wire. A write to the task still refuses; it is the one
	// that could make the rows wronger.
	//
	// THE SCOPE IS THE TASK ITSELF and cannot be formed until the
	// reference is resolved — a key names no path, and an id names no
	// project — so the framework resolves it in the answer's own
	// transaction ([taskReadScope]) and probes where the task is filed.
	//
	// THE WHOLE FRESHNESS, not the level alone: a staleness bound is
	// about this node's lag rather than about a set, so one row's read
	// is exactly as far behind as a listing's, and a floor the caller
	// named is the position its own write landed at.
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
			// READ WHETHER OR NOT `comments` WAS ASKED FOR: naming one
			// comment IS asking for comments, and a caller that had to
			// pass both would meet a silently empty thread when it
			// forgot.
			if out.Comments, err = readComment(ctx, tx, id, want.Comment); err != nil {
				return err
			}
		case want.Comments:
			out.Comments, out.CommentsCursor, err = readComments(ctx, tx, id,
				want.CommentThread, want.CommentCursor, commentPage(want.CommentLimit))
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
			// IN THE SAME TRANSACTION as the rows it labels, for
			// the reason the feed's own read gives: a map read
			// separately could name a key from a later instant
			// than the history it is about.
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
		// THE SAME EXISTS THE BOARD ROW USES, in this same transaction,
		// so the badge on the item and the badge on its row cannot
		// disagree about one task at one instant — and the collision
		// flag beside it, from the same column the row reads, for the
		// same reason.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := tx.QueryRowContext(ctx, detailFlagsStatement,
			id, id).Scan(&out.Blocked, &out.KeyCollision); err != nil {
			return fmt.Errorf("worklet: read %s's open blockers: %w", id, err)
		}

		// THE COVERAGE, IN THE SAME TRANSACTION as the rows. A detail
		// read that reported it from a second one would be answering
		// "is this complete" about a state the rows were not read from
		// — and the whole value of the number is that it describes THIS
		// answer.
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied

		// SCOPED TO THE TASK, not to the company. A deferred record
		// somewhere else in the tracker makes a BOARD incomplete and
		// says nothing about this task — reporting it here would put a
		// permanent warning on every task in a company holding one
		// undecodable record about one other task.
		//
		// THE SAME RESOLUTION THE FRAMEWORK PROBED, in the same
		// transaction, so the count reported here is about exactly the
		// paths that decided whether the answer is complete — the key's
		// own address included.
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
	// THE UNITS ARE RESOLVED OUTSIDE THE TRANSACTION, because the seam is
	// the CALLER's code — a chart walk today, whatever a future surface
	// hands over tomorrow — and holding a read transaction open across it
	// would make one caller's resolver a lock on this node's estate. It
	// reads no row, so there is nothing to keep consistent with the ones
	// above.
	out.Units = taskUnits(want.Units, out.Task)
	out.DueStanding = dueStanding(want.Clock, out.Task)
	out.ReassignmentBudget = ReassignmentBudget
	// THE LEVEL SERVED, never the level asked for. Assigning the argument
	// here — which is the only thing this function used to do with it —
	// is what made the level a label: a read that refused and one that
	// went through a quorum-committed barrier reported the same word.
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
// reference the caller named.
//
// THE TASK'S OWN PATH, under the project it is filed in — the only place a
// record about it is filed. A key adds its own address term as well, because
// which task a key opens is decided by the records filed under the KEY — an
// alias claim, and every create, re-key and purge that claims, frees or hands
// it on ([keyTerm]) — and a deferred one of those is not filed under the task.
//
// RESOLVED IN [resolveTaskID]'s ORDER — the id, then the key directory, then a
// task row's own key — because the scope probed has to be the scope of the
// task the read then returns: a key two tasks hold opens the one that claimed
// it, and resolved the other way round the probe asked about one task's
// project while the answer was the other's.
//
// A REFERENCE THAT RESOLVES TO NOTHING STILL BOUNDS A SCOPE, because "no such
// task" is a claim about rows a deferred create has not written yet: a key
// names its project, so a create or an alias there could be the answer, and an
// id names nothing narrower than the domain.
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

// resolveTaskID turns an id or a key — current or former — into an id.
//
// THE FORMER KEY RESOLVES TOO, and that is not a nicety: a task moved between
// projects keeps its old key as an alias precisely so every link, chat message
// and comment that named it still opens the right record. A lookup that only
// knew the current key would break every reference the move was meant to keep.
//
// A KEY RESOLVES THROUGH THE DIRECTORY FIRST, so a key two tasks hold opens the
// one that claimed it — the task every earlier reference was written against —
// on every data node, rather than whichever row an index happened to return
// first.
// The task row is the fallback for a key whose claim has not been written yet.
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
// than this id" — [resolveTaskID]'s answer to a key, asked of a row that
// carries a key and a task's id side by side.
//
// THE DIRECTORY AND NOTHING ELSE, because the directory is what the resolver
// asks first, a create or a re-key writes its claim in the same transaction
// as the task row ([Applier.maintainKeys]) and a purge hands a claimant's key
// on to a task still holding it ([handOnKey]), so a task's current key always
// has its row. An
// empty key opens nothing and is never a collision — and neither is a key
// beside an EMPTY id, which names no task the key could fail to open: asked
// anyway, every directory row would count as "another" and the row's only
// address would be swapped for an id it does not have.
//
// It exists for a key that is NOT the task's current one — a notice stores
// the key its task held when it was written — where the task row's own
// `key_collision` describes a different key. A row reading the task's current
// key reads that column instead, which the applier derives from this same
// directory.
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

// readTaskDocument decodes the task's own record.
//
// FROM THE DOCUMENT rather than from the columns, and that is the same rule
// the applier writes under: the columns beside it are extracted for filtering
// and sorting and are a CACHE of what the document says. A rolling upgrade
// puts a newer node's fields on the wire, and a reader that rebuilt a task
// from its columns would hand a caller a task with the new field stripped.
//
// # `status_entered_at` and the spend ARE THE EXCEPTIONS, joined here
//
// The spend because a turn adds to the task's columns and never its document
// (see below). `rank` is read from its column too, though the two agree — the
// project's ORDER writes a task's place into both ([Applier.applyRankOrder]) —
// because the column is what every board orders by, so a detail read and a
// board can never name two places for one card. The rest of this section is
// about `status_entered_at`.
//
// Every other column is extracted from what the WRITER wrote, so the document
// is the authority and the column is the copy. This one is DERIVED by the
// applier from the history — no writer authors it and none could, since it is
// a closed form over every row about the task — and it is stamped AFTER the
// document is marshalled, because it reads the history row the same apply
// writes after the upsert. So the document always carries a zero here.
//
// Reading it from the document is what made the published field silently
// empty while the column behind `sort=status_entered` was maintained
// correctly: `omitzero` dropped it, and `GET /worklet/item`, the operator MCP's
// `get_work_item` and the dashboard's "In status since" row all showed
// nothing. Writing it INTO the document instead would mean a second write of
// the whole document per status change, to store a value that is a function
// of rows already committed.
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
	// AND THE SPEND, because a turn adds to the task's COLUMNS in the
	// transaction that inserts its turn row ([Applier.applyTurn]) and
	// never touches the document, so the document's copy is whatever the
	// create wrote — zero. Read from the document, every task page said no agent had ever worked on it, beside
	// a turn list and a board card counting the turns the columns hold.
	task.Spend = spend
	return task, nil
}

// DetailComments is how many comments one detail read returns.
//
// TWENTY, which is the design's own figure and is what a thread's recent shape
// takes: past that a reader is scrolling rather than catching up, and the
// cursor is how they do it. A task's thread is the ONE collection on a detail
// read with no bound of its own — a body is capped, a history feed is capped,
// a relation set is capped — so it is the shape that decides whether this
// answer fits [ToolAnswerBytes] at all.
const DetailComments = 20

// MaxCommentPage is the most comments one page may hold.
//
// FIFTY, the ceiling every other paged read in this tracker holds a caller to
// ([inbox.MaxRows], [MaxFeedPage], [MaxSearchLimit]), where a caller walking a
// long thread wants as few round trips as that allows. What a page WEIGHS is
// [CommentPageBytes]'s to bound, not this.
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

// CommentPageBytes is how much comment text one THREAD PAGE carries, in
// WHOLE comments — never a comment cut to fit.
//
// The page used to carry every body cut to two kibibytes, and a comment cut
// there reads as a comment that ENDED there: a reviewer's "looks good, but —"
// with the "but" two kilobytes in. Every comment on a page is now whole, and
// a page holds as many as fit this budget — at least one, which [MaxCommentBody]
// already bounds — and its cursor continues exactly where it stopped, so a
// long thread is more pages rather than shorter comments.
//
// 16 KiB is a quarter of the 64 KiB one tool answer may weigh: beside a whole
// description ([MaxBody]) it leaves the history, links and fields their room,
// and the answer's own ceiling refuses — naming `include` — rather than cuts
// on the rare item where all of them are at their largest at once.
const CommentPageBytes = 16 << 10

// readComments is a page of the thread, NEWEST FIRST.
//
// NEWEST FIRST, unlike the whole thread a person scrolls, because a PAGE has
// to start somewhere and the end is what a reader catching up needs: the
// oldest twenty of a two-hundred-comment thread is the conversation's
// beginning, which is the part they have already read. The page is reversed
// before it is returned, so what a caller holds is still in the order it was
// written — a reply resolves against what came before it.
//
// A REMOVED COMMENT KEEPS ITS ROW with a blank body, which is what lets a
// reply still resolve against something — so it is returned rather than
// filtered, and its `removed` flag is what a renderer reads.
//
// A [CommentThread] with a root narrows the page to that conversation; see
// the type for why that happens here rather than in the caller.
func readComments(ctx context.Context, tx *sql.Tx, taskID string, thread CommentThread,
	cursor string, limit int) ([]Comment, string, error) {
	return readCommentPage(ctx, tx, taskID, thread, cursor, limit, CommentPageBytes)
}

// readCommentPage is [readComments] with the byte budget a parameter, so the
// bound is exercised directly rather than only at its default.
//
// THE BUDGET ENDS A PAGE EARLY, never a comment: the comment that would take
// the page past it is the first of the next page, and the cursor names the
// last comment this page RETURNED — the same rule the count follows, for the
// same reason.
func readCommentPage(ctx context.Context, tx *sql.Tx, taskID string, thread CommentThread,
	cursor string, limit, maxBytes int) ([]Comment, string, error) {

	// ONE MORE THAN THE PAGE, which is how the cursor knows whether there
	// IS a next page without a second count over a table that grows for
	// the life of the task.
	rows, err := tx.QueryContext(ctx, `
		SELECT document, created_at, id FROM tracker_comments
		WHERE task_id = ? AND (? = '' OR (created_at, id) < (?, ?))
		  AND (? = '' OR reply_to = ? OR answers = ? OR (? AND id = ?))
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, taskID, cursor, cursorAt(cursor), cursorID(cursor),
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
			// THE EXTRA ROW SAYS THERE IS A NEXT PAGE, and the cursor
			// is the last row RETURNED, not this one. The query reads
			// strictly below its cursor, so a cursor naming the extra
			// row skipped it: every page boundary lost one comment —
			// the one a reader walking back through the thread would
			// have met next — and no read anywhere returned it.
			next = formatCommentCursor(lastAt, lastID)
			break
		}
		var comment Comment
		if err := json.Unmarshal(body, &comment); err != nil {
			return nil, "", fmt.Errorf("worklet: decode a comment on %s: %w",
				taskID, err)
		}
		// WHOLE, and the page ends before a comment that would take it
		// past its budget — HERE and not at the caller, because a caller
		// that forgot would send the whole thread. The first comment is
		// always taken, so a page is never empty while the thread is not.
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

// readComment is ONE comment, WHOLE — the read that answers one comment by id.
//
// A single body is bounded by [MaxCommentBody], which the write already
// refuses above, so the one value this returns fits inside a tool answer with
// room to spare.
//
// SCOPED TO THE TASK, so a comment id from another item answers
// [ErrNoComment] rather than quietly returning a thread the caller was not
// reading. The id is the table's primary key; the task_id term is the check,
// not the lookup.
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

func cursorAt(cursor string) int64 {
	at, _, _ := strings.Cut(cursor, ":")
	n, _ := strconv.ParseInt(at, 10, 64)
	return n
}

func cursorID(cursor string) string {
	_, id, _ := strings.Cut(cursor, ":")
	return id
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
	// SIZED TO THE PAGE, which is what [readActivity] does over the same
	// table for the same reason: `limit` is known before the first scan,
	// and growing from nil reallocates six times on the way to a default
	// page of [DetailHistoryDefault]. It also holds the rule
	// [readProjectRows] states at length — an answer's list is `[]` and
	// never nil — although a task's history has no reachable empty case to
	// exercise it: a create writes a row, so every task this reader can
	// find has at least one.
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

// readLinks is both directions of every relation.
//
// TWO QUERIES RATHER THAN A UNION, because the two halves mean different
// things: the rows keyed on this task are the ones it owns, and the rows
// pointing AT it are the mirror — which is what `derived` marks. Merging them
// in SQL would make a renderer re-derive which end authored the edge from a
// column that no longer says.
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
			// THE MIRROR IS DERIVED WHATEVER ITS COLUMN SAYS. The
			// column records how the edge was written; this side of
			// the read is by definition the end that did not author
			// it, and a renderer asking "may I edit this" needs the
			// second fact rather than the first.
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
