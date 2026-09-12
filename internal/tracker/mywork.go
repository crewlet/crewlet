package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// "Everything about me" — the one call a seat makes at the start of a turn.
//
// # ONE CALL, SEVEN LISTS, ONE TRANSACTION
//
// A turn opens by asking what it is expected to do. Assembled from seven
// separate reads that would be seven round trips, seven coverage probes and
// seven different instants — so a seat could see a task in `assigned` that had
// already moved out of it by the time `priorities` was read, and spend its
// turn on work somebody else had taken.
//
// It is its OWN reader method rather than a preset of the task query for the
// reason §6 gives: `list_tasks` always answers one shape, and a preset that
// returned seven lists would make every caller of that grammar handle a second
// one.
//
// # Why these seven, and not a longer list
//
// Each is a different CLAIM on the reader's attention, and a seat that saw
// only its assignments would miss all but the first:
//
//   - `priorities` — what a lead put at the top of this person's list, in the
//     order they put it. It is first because it is the only block somebody
//     else arranged deliberately.
//   - `assigned` — the open work this seat owns.
//   - `asked_of_me` — questions waiting on an answer, with the comment to
//     answer and the form to answer it with.
//   - `checklist_items` — the sub-item claims, which live on somebody else's
//     task and are invisible to every assignee filter.
//   - `collaborating` — work this seat was brought onto without owning.
//   - `watching_recent` — what moved on the tasks it follows.
//   - `unblocked_recent` — what became workable, which is the one block that
//     is about a change rather than a state.
//
// # Each is bounded, and the bound is the same
//
// Twenty rows each. A turn-start block is read by a model beside its prompt,
// and seven unbounded lists is an answer that crowds out the work.

// MyWorkRows is how many rows each block carries.
//
// TWENTY, and the same for all seven so no block can crowd out another: the
// whole answer is read as one prompt block, and a caller with two hundred
// assignments would otherwise push its asks off the end of the context. A seat
// with more than twenty of anything here has a queue problem the list cannot
// fix by being longer.
const MyWorkRows = 20

// AskRow is one question waiting on this person, with what to do about it.
type AskRow struct {
	TaskRow

	// Comment is the ask itself — the id an answer replies to, and the
	// body, so a model can decide without a second read.
	Comment string    `json:"comment"`
	AskedBy string    `json:"asked_by"`
	AskedAt time.Time `json:"asked_at"`
	Body    string    `json:"body"`

	// Answer is the literal call that answers it. A model handed the
	// comment id still has to compose the call, and every one it composes
	// differently is a round spent being refused.
	Answer string `json:"answer_with"`
}

// ChecklistRow is one sub-item claimed by this person.
//
// IT LIVES ON SOMEBODY ELSE'S TASK, which is the whole reason it is its own
// block: no assignee filter over tasks reaches it, so a seat holding six
// checklist items and no assignment reads its queue as empty.
type ChecklistRow struct {
	Task      string `json:"task"`
	TaskKey   string `json:"task_key"`
	TaskTitle string `json:"task_title"`
	Checklist string `json:"checklist"`
	Item      string `json:"item"`
	Name      string `json:"name"`
	Done      bool   `json:"done"`
}

// MyWork is the compound answer.
type MyWork struct {
	Handle string `json:"handle"`

	// Priorities is in the STORED ORDER, not re-sorted: the order is the
	// content — it is what somebody decided — and sorting it by anything
	// else would discard the decision.
	Priorities []TaskRow `json:"priorities"`

	Assigned        []TaskRow      `json:"assigned"`
	AskedOfMe       []AskRow       `json:"asked_of_me"`
	ChecklistItems  []ChecklistRow `json:"checklist_items"`
	Collaborating   []TaskRow      `json:"collaborating"`
	WatchingRecent  []TaskRow      `json:"watching_recent"`
	UnblockedRecent []TaskRow      `json:"unblocked_recent"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MyWorkQuery asks for one person's own day.
type MyWorkQuery struct {
	// Handle is whose. Required — "mine" is resolved by the surface from
	// its own credential, never by this reader, because a reader that
	// defaulted it would answer about whoever it happened to pick.
	Handle string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
}

// MyWork answers everything one person is expected to look at.
func (r *Reader) MyWork(ctx context.Context, q MyWorkQuery, now time.Time) (
	MyWork, error) {

	if q.Level == "" {
		return MyWork{}, fmt.Errorf("tracker: this my_work read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if q.Handle == "" {
		return MyWork{}, fmt.Errorf("tracker: my_work names nobody — a " +
			"surface resolves the viewer from its own credential before it " +
			"reads, because a day with nobody's name on it is everybody's")
	}

	out := MyWork{Handle: q.Handle}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, honestly: this person's work is in every container
		// and a narrower closure would certify the answer complete while
		// a deferred record in some project held the task that belongs
		// in it.
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readMyWork(ctx, tx, q.Handle, now, &out)
	})
	if err != nil {
		return MyWork{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

func readMyWork(ctx context.Context, tx *sql.Tx, handle string, now time.Time,
	out *MyWork) error {

	// THE DAY BOUNDARY the `overdue` column on every row is computed
	// against. UTC, because a compound answer has no caller-supplied zone
	// and the alternative — a boundary read from the process's own clock
	// — would put one node's rows a day out from another's.
	anchor, err := ResolveDate("today", now, time.UTC)
	if err != nil {
		return err
	}
	dayStart := anchor.At
	open := openGroups()

	priorities, err := readPriorityRows(ctx, tx, handle, dayStart)
	if err != nil {
		return err
	}
	out.Priorities = priorities

	// ASSIGNED, in the queue's own order — priority then due, which is
	// exactly what `tracker_tasks_queue_idx` is built in, so the block a
	// turn opens on is the query the index is named for.
	assigned, _, err := readTasks(ctx, tx,
		"t.removed_at IS NULL AND t.assignee = ? AND t.status_group IN ("+
			placeholders(len(open))+")",
		append([]any{handle}, open...),
		[]sortTerm{
			{Column: "t.prio_rank", Descending: true},
			{Column: "t.due_at"}, {Column: "t.id"},
		}, MyWorkRows, dayStart)
	if err != nil {
		return err
	}
	out.Assigned = assigned

	asks, err := readAsks(ctx, tx, handle, dayStart)
	if err != nil {
		return err
	}
	out.AskedOfMe = asks

	items, err := readChecklistClaims(ctx, tx, handle)
	if err != nil {
		return err
	}
	out.ChecklistItems = items

	// COLLABORATING EXCLUDES WHAT THIS SEAT OWNS, because a task already
	// in `assigned` listed again here is one row spending two of the seven
	// blocks — and the distinction the block exists for is precisely
	// "brought on without owning".
	collaborating, _, err := readTasks(ctx, tx,
		"t.removed_at IS NULL AND t.assignee <> ? AND t.status_group IN ("+
			placeholders(len(open))+") AND EXISTS (SELECT 1 FROM "+
			"tracker_collaborators c WHERE c.task_id = t.id AND c.handle = ?)",
		append(append([]any{handle}, open...), handle),
		[]sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
		MyWorkRows, dayStart)
	if err != nil {
		return err
	}
	out.Collaborating = collaborating

	// WATCHING, MINUS THE MUTED. A mute is how somebody says "keep me on
	// this but stop telling me", and a block that ignored it would put
	// every muted task back in front of them once a turn.
	watching, _, err := readTasks(ctx, tx,
		"t.removed_at IS NULL AND t.assignee <> ? AND EXISTS (SELECT 1 FROM "+
			"tracker_watchers w WHERE w.task_id = t.id AND w.handle = ? "+
			"AND w.muted = 0)",
		[]any{handle, handle},
		[]sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
		MyWorkRows, dayStart)
	if err != nil {
		return err
	}
	out.WatchingRecent = watching

	// UNBLOCKED: this seat's open work that HAD a blocker and no longer
	// has an open one. The only block about a CHANGE rather than a state,
	// and the reason it is here at all — a task that became workable while
	// nobody was looking has nothing else to announce it at turn start.
	//
	// THE EXISTS/NOT-EXISTS PAIR IS THE WHOLE PREDICATE. Without the first
	// leg every task that never had a dependency qualifies, which is most
	// of the board; without the second, a task with one blocker cleared
	// and another still open reads as workable and is not.
	unblocked, _, err := readTasks(ctx, tx,
		"t.removed_at IS NULL AND t.assignee = ? AND t.status_group IN ("+
			placeholders(len(open))+") AND EXISTS (SELECT 1 FROM "+
			"tracker_task_deps d WHERE d.task_id = t.id "+
			"AND d.cleared_at IS NOT NULL) AND NOT EXISTS (SELECT 1 FROM "+
			"tracker_task_deps d WHERE d.task_id = t.id AND d.blocker_open = 1)",
		append([]any{handle}, open...),
		[]sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
		MyWorkRows, dayStart)
	if err != nil {
		return err
	}
	out.UnblockedRecent = unblocked
	return nil
}

// openGroups is the two status groups where work has not stopped.
//
// DERIVED from [StatusGroups] rather than typed, for the reason
// [deliveredClause] derives its own: a group added to the set has to reach
// every reader, and a literal pair is the reader it would not reach.
func openGroups() []any {
	out := make([]any, 0, len(StatusGroups))
	for _, group := range StatusGroups {
		if !group.Finished() {
			out = append(out, string(group))
		}
	}
	return out
}

// readPriorityRows reads the stored list, IN ITS STORED ORDER.
//
// The order is the content — somebody decided it — so the rows are re-ordered
// to match the list rather than sorted by anything. A finished or removed task
// is filtered out HERE rather than rewritten out of the list, because the list
// is a person's own object and a read must not write to it: the next write to
// the list drops it for good.
func readPriorityRows(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time) ([]TaskRow, error) {

	person, held, err := readPerson(ctx, tx, handle)
	if err != nil {
		return nil, err
	}
	if !held || len(person.Priorities) == 0 {
		return nil, nil
	}
	ids := person.Priorities
	if len(ids) > MyWorkRows {
		ids = ids[:MyWorkRows]
	}
	open := openGroups()
	args := make([]any, 0, len(ids)+len(open))
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, open...)
	rows, _, err := readTasks(ctx, tx,
		"t.removed_at IS NULL AND t.id IN ("+placeholders(len(ids))+
			") AND t.status_group IN ("+placeholders(len(open))+")",
		args, []sortTerm{{Column: "t.id"}}, MyWorkRows, dayStart)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]TaskRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]TaskRow, 0, len(ids))
	for _, id := range ids {
		if row, live := byID[id]; live {
			out = append(out, row)
		}
	}
	return out, nil
}

// readAsks reads the questions waiting on this person.
//
// THE OPEN ONES ONLY, which is the shipped partial index's own predicate: an
// ask is open until somebody answers it or resolves it, and a removed comment
// is not an ask at all.
func readAsks(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time) ([]AskRow, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.task_id, c.author, c.body, c.created_at
		FROM tracker_comments c
		JOIN tracker_tasks t ON t.id = c.task_id
		WHERE c.ask = ? AND c.resolved = 0 AND c.answered_by IS NULL
		  AND c.removed = 0 AND t.removed_at IS NULL
		ORDER BY c.created_at DESC
		LIMIT ?`, handle, MyWorkRows)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the asks waiting on %s: %w",
			handle, err)
	}
	defer func() { _ = rows.Close() }()

	type ask struct {
		comment, task, author, body string
		at                          int64
	}
	var pending []ask
	for rows.Next() {
		var a ask
		if err := rows.Scan(&a.comment, &a.task, &a.author, &a.body, &a.at); err != nil {
			return nil, fmt.Errorf("tracker: scan an ask: %w", err)
		}
		pending = append(pending, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the asks waiting on %s: %w",
			handle, err)
	}
	if len(pending) == 0 {
		return nil, nil
	}

	ids := make([]any, 0, len(pending))
	for _, a := range pending {
		ids = append(ids, a.task)
	}
	tasks, _, err := readTasks(ctx, tx,
		"t.id IN ("+placeholders(len(ids))+")", ids,
		[]sortTerm{{Column: "t.id"}}, len(ids), dayStart)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]TaskRow, len(tasks))
	for _, row := range tasks {
		byID[row.ID] = row
	}
	out := make([]AskRow, 0, len(pending))
	for _, a := range pending {
		row, held := byID[a.task]
		if !held {
			continue
		}
		out = append(out, AskRow{
			TaskRow: row, Comment: a.comment, AskedBy: a.author,
			AskedAt: store.DecodeTime(a.at),
			Body:    textcut.Within(a.body, MaxExcerpt),
			// THE LITERAL CALL, composed here rather than described.
			// A model handed a comment id still has to compose the
			// answer, and every one it composes differently is a
			// round spent being refused.
			Answer: fmt.Sprintf("%s(item: %q, body: \"…\", answers: %q)",
				CommentOnWorkTool, row.Key, a.comment),
		})
	}
	return out, nil
}

// readChecklistClaims reads the sub-items this person owns.
func readChecklistClaims(ctx context.Context, tx *sql.Tx, handle string) (
	[]ChecklistRow, error) {

	// THE OPEN ONES, on live tasks. A done item is not a claim, and an
	// item on a removed task is an item nobody can act on.
	rows, err := tx.QueryContext(ctx, `
		SELECT i.task_id, t.key, t.title, i.checklist_id, i.item_id, i.name,
		       i.done
		FROM tracker_checklist_items i
		JOIN tracker_tasks t ON t.id = i.task_id
		WHERE i.assignee = ? AND i.done = 0 AND t.removed_at IS NULL
		ORDER BY t.key, i.ord
		LIMIT ?`, handle, MyWorkRows)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the checklist items of %s: %w",
			handle, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ChecklistRow
	for rows.Next() {
		var row ChecklistRow
		var done int
		if err := rows.Scan(&row.Task, &row.TaskKey, &row.TaskTitle,
			&row.Checklist, &row.Item, &row.Name, &done); err != nil {
			return nil, fmt.Errorf("tracker: scan a checklist item: %w", err)
		}
		row.Done = done != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the checklist items of %s: %w",
			handle, err)
	}
	return out, nil
}
