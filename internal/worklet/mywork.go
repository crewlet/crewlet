package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// my_work, "everything about me": the one call a seat makes at a turn's start.
//
// ONE TRANSACTION for all eight blocks: eight reads would be eight instants,
// and a seat could spend its turn on an `assigned` task that had already moved.
// Its own reader rather than a task-query preset, because `list_tasks` answers
// one shape. Each block is a different claim on attention ([MyWork]'s fields),
// each is a page of [MyWorkRows], and [MyWork.Totals] counts each in full by
// the same predicate in the same transaction: a badge drawn from the page reads
// "20" for the person with two hundred, the one person the number is for.

// MyWorkAskBytes is how much ask text the my_work block carries WHOLE: 16 KiB,
// a quarter of one tool answer's 64 KiB for [CommentPageBytes]'s reason, since
// twenty asks at their 32 KiB cap would be ten answers' worth. Past it an ask
// keeps its decision, author and answering call and says how large its omitted
// body is, one get_work_item away. Never a body cut to fit.
const MyWorkAskBytes = 16 << 10

// MyWorkRows is how many rows each block carries: the same for all eight, so
// no block pushes another off the end of the one prompt block they are read
// as. A seat with more than twenty of anything has a queue problem a longer
// list cannot fix.
const MyWorkRows = 20

// AskRow is one question waiting on this person, with what to do about it.
type AskRow struct {
	TaskRow

	// Comment is the id an answer replies to; Body is carried WHOLE, so a
	// model decides without a second read.
	Comment string    `json:"comment"`
	AskedBy string    `json:"asked_by"`
	AskedAt time.Time `json:"asked_at"`
	Body    string    `json:"body" untrusted:"fence"`

	// BodyNotIncluded says the body was left out, how large it is and the
	// call that reads it, set only once earlier asks spent [MyWorkAskBytes].
	// An ask is whole or by reference, never cut: a cut question loses the
	// condition past the cut.
	BodyNotIncluded string `json:"body_not_included,omitempty"`

	// Decision is the ask's structured choice (options, recommendation,
	// evidence), so a person and a model decide from the same read.
	Decision *decision.Decision `json:"decision,omitempty"`

	// Open says the ask still waits on an answer. Every my_work row does,
	// but the row shape is shared with lists that also hold answered asks.
	Open bool `json:"open"`

	// Answer is the literal call that answers it (see [answerCall]).
	// Absent on an answered ask, which nothing answers again.
	Answer string `json:"answer_with,omitempty"`
}

// ChecklistRow is one sub-item claimed by this person.
//
// It lives on somebody else's task, so no assignee filter reaches it: without
// this block a seat holding only checklist items reads its queue as empty.
type ChecklistRow struct {
	Task    string `json:"task"`
	TaskKey string `json:"task_key"`

	// TaskKeyCollision is the task's [TaskRow.KeyCollision]: a link built
	// from TaskKey opens another task, so reach this one by Task
	// ([ItemAddress]).
	TaskKeyCollision bool `json:"task_key_collision,omitempty"`

	TaskTitle string `json:"task_title" untrusted:"name"`
	Checklist string `json:"checklist" untrusted:"name"`
	Item      string `json:"item"`
	Name      string `json:"name" untrusted:"name"`
	Done      bool   `json:"done"`
}

// ClaimTotal is how many rows one block holds in full, beside the page of
// [MyWorkRows] it carries.
//
// Capped at [TotalHintCeiling], and saying so, for [Answer.TotalCapped]'s
// reason.
type ClaimTotal struct {
	Total  int  `json:"total"`
	Capped bool `json:"capped,omitempty"`
}

// MyWorkTotals is one [ClaimTotal] per block, keyed by the block's own name.
//
// A named field per block rather than a map, so TestEveryMyWorkBlockHasATotal
// can catch a block added without its total.
type MyWorkTotals struct {
	Priorities      ClaimTotal `json:"priorities"`
	Assigned        ClaimTotal `json:"assigned"`
	AskedOfMe       ClaimTotal `json:"asked_of_me"`
	ChecklistItems  ClaimTotal `json:"checklist_items"`
	Collaborating   ClaimTotal `json:"collaborating"`
	WatchingRecent  ClaimTotal `json:"watching_recent"`
	UnblockedRecent ClaimTotal `json:"unblocked_recent"`
	HoldingUp       ClaimTotal `json:"holding_up"`
}

// MyWork is the compound answer.
type MyWork struct {
	Handle string `json:"handle"`

	// Priorities keeps the STORED ORDER, never re-sorted: the order is what
	// a lead decided, and it comes first as the one block arranged on purpose.
	Priorities []TaskRow `json:"priorities"`

	Assigned        []TaskRow      `json:"assigned"`
	AskedOfMe       []AskRow       `json:"asked_of_me"`
	ChecklistItems  []ChecklistRow `json:"checklist_items"`
	Collaborating   []TaskRow      `json:"collaborating"`
	WatchingRecent  []TaskRow      `json:"watching_recent"`
	UnblockedRecent []TaskRow      `json:"unblocked_recent"`
	HoldingUp       []HoldingRow   `json:"holding_up"`

	// HoldingAgents sums HoldingUp over agent work, across every held item
	// walked: what a person's Inbox leads with ([HoldingAgents]).
	HoldingAgents HoldingAgents `json:"holding_agents"`

	// Totals is every block counted in full; a block's slice is a PAGE of
	// at most [MyWorkRows].
	Totals MyWorkTotals `json:"totals"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MyWorkQuery asks for one person's own day.
type MyWorkQuery struct {
	// Handle is whose: the person's record name (iam.RecordOwner), for a
	// person bound to a seat that seat whatever credential they hold.
	// Required: the surface resolves "mine" from its own credential, since
	// a reader that defaulted it would answer about whoever it picked.
	Handle string

	Level   statelog.ReadLevel
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound in RECORDS, what the broker answers;
	// MaxLag is derived from it through this node's drain rate. The read
	// refuses past whichever is reached first.
	MaxLagSeq uint64

	// Agents are the company's agent seats, by handle: `holding_up` counts
	// the items they hold among what waits on the person. Nil counts none.
	Agents []string
}

// MyWork answers everything one person is expected to look at.
//
// `now` and `loc` are the instant and the company's clock the day is cut on,
// the pair [ParseQuery] takes for a `due=` filter: every row's `overdue` mark
// here is derived from where the company's day began, so it agrees with the
// mark the same task carries on a board.
func (r *Reader) MyWork(ctx context.Context, q MyWorkQuery, now time.Time,
	loc *time.Location) (MyWork, error) {

	if q.Level == "" {
		return MyWork{}, fmt.Errorf("worklet: this " + MyWorkTool + " read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if q.Handle == "" {
		return MyWork{}, fmt.Errorf("worklet: " + MyWorkTool + " names nobody; a " +
			"surface resolves the viewer from its own credential before it " +
			"reads, because a day with nobody's name on it is everybody's")
	}

	out := MyWork{Handle: q.Handle}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE WHOLE DOMAIN: this person's work is in every container, and
		// a narrower closure would certify complete an answer a deferred
		// record elsewhere should change.
		Scope:     statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		return readMyWork(ctx, tx, q.Handle, q.Agents, now, loc, &out)
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

func readMyWork(ctx context.Context, tx *sql.Tx, handle string, agents []string, now time.Time,
	loc *time.Location, out *MyWork) error {

	// `overdue` is cut at the company's own midnight on the surface's
	// clock, so it agrees with the board; neither UTC nor the process's
	// clock, which would put rows a day out from the board or another node.
	anchor, err := period.Resolve("today", now, loc)
	if err != nil {
		return err
	}
	dayStart := anchor.At

	priorities, total, err := readPriorityRows(ctx, tx, handle, dayStart)
	if err != nil {
		return err
	}
	out.Priorities = priorities
	out.Totals.Priorities = ClaimTotal{Total: total}

	if out.Assigned, out.Totals.Assigned, err = taskBlock(ctx, tx,
		assignedBlock(handle), dayStart); err != nil {
		return err
	}

	if out.AskedOfMe, out.Totals.AskedOfMe, err = readAsks(ctx, tx, handle,
		dayStart, MyWorkRows, MyWorkAskBytes); err != nil {
		return err
	}

	if out.ChecklistItems, out.Totals.ChecklistItems, err = readChecklistClaims(
		ctx, tx, handle); err != nil {
		return err
	}

	if out.Collaborating, out.Totals.Collaborating, err = taskBlock(ctx, tx,
		collaboratingBlock(handle), dayStart); err != nil {
		return err
	}
	if out.WatchingRecent, out.Totals.WatchingRecent, err = taskBlock(ctx, tx,
		watchingBlock(handle), dayStart); err != nil {
		return err
	}
	if out.UnblockedRecent, out.Totals.UnblockedRecent, err = taskBlock(ctx, tx,
		unblockedBlock(handle), dayStart); err != nil {
		return err
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}
	if out.HoldingUp, out.Totals.HoldingUp, out.HoldingAgents, err = readHolding(ctx, tx, handle,
		agentSet, dayStart); err != nil {
		return err
	}
	return nil
}

// blockQuery is one task block of [readMyWork]: the predicate, its arguments
// and its page order, from which [taskBlock] reads the page and the total.
type blockQuery struct {
	where string
	args  []any
	terms []sortTerm
}

// The task blocks [readMyWork] reads, each named for the plan test.
//
// ASSIGNED is in the queue's order, priority then due: exactly
// `tracker_tasks_queue_idx`'s.
func assignedBlock(handle string) blockQuery {
	open := openGroups()
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group IN (" +
			placeholders(len(open)) + ")",
		args: append([]any{handle}, open...),
		terms: []sortTerm{
			{Column: "t.prio_rank", Descending: true},
			{Column: "t.due_at"}, {Column: "t.id"},
		},
	}
}

// COLLABORATING EXCLUDES WHAT THIS PERSON OWNS: the block is "brought on
// without owning", and an owned task is already in `assigned`.
func collaboratingBlock(handle string) blockQuery {
	open := openGroups()
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee <> ? AND t.status_group IN (" +
			placeholders(len(open)) + ") AND " + peopleClause(rowAlias, "SELECT c.task_id FROM "+
			"tracker_collaborators c WHERE c.handle = ?"),
		args:  append(append([]any{handle}, open...), handle),
		terms: []sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
	}
}

// WATCHING, MINUS THE MUTED: a mute says "keep me on this but stop telling
// me", and ignoring it would show every muted task once a turn.
func watchingBlock(handle string) blockQuery {
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee <> ? AND " +
			peopleClause(rowAlias, "SELECT w.task_id FROM tracker_watchers w "+
				"WHERE w.handle = ? AND w.muted = 0"),
		args:  []any{handle, handle},
		terms: []sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
	}
}

// UNBLOCKED: this person's open work that HAD a blocker and has no open one
// left; the one block about a change rather than a state. Both legs are
// needed: without EXISTS every task that never had a dependency qualifies,
// and without NOT EXISTS one with a blocker still open reads as workable.
func unblockedBlock(handle string) blockQuery {
	open := openGroups()
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group IN (" +
			placeholders(len(open)) + ") AND EXISTS (SELECT 1 FROM " +
			"tracker_task_deps d WHERE d.task_id = t.id " +
			"AND d.cleared_at IS NOT NULL) AND NOT EXISTS (SELECT 1 FROM " +
			"tracker_task_deps d WHERE d.task_id = t.id AND d.blocker_open = 1)",
		args:  append([]any{handle}, open...),
		terms: []sortTerm{{Column: "t.updated_at", Descending: true}, {Column: "t.id"}},
	}
}

// taskBlock reads one task block's page AND its total, from ONE predicate.
//
// A total in a second statement is a second copy of the predicate, and the
// first edit to one makes a heading count rows its list would never show.
func taskBlock(ctx context.Context, tx *sql.Tx, block blockQuery,
	dayStart time.Time) ([]TaskRow, ClaimTotal, error) {

	set := tasksWhere(block.where, block.args)
	rows, _, err := readTasks(ctx, tx, set, block.terms, MyWorkRows, dayStart)
	if err != nil {
		return nil, ClaimTotal{}, err
	}
	total, capped, err := countHint(ctx, tx, set)
	if err != nil {
		return nil, ClaimTotal{}, err
	}
	return rows, ClaimTotal{Total: total, Capped: capped}, nil
}

// openGroups is the two status groups where work has not stopped.
//
// Derived from [StatusGroups] rather than a literal pair, so a group added to
// the set reaches this reader too ([deliveredClause] does the same).
func openGroups() []any {
	out := make([]any, 0, len(StatusGroups))
	for _, group := range StatusGroups {
		if !group.Finished() {
			out = append(out, string(group))
		}
	}
	return out
}

// readPriorityRows reads the stored list, IN ITS STORED ORDER, and how many
// of its entries are still open work.
//
// Rows follow the list, never a sort. A finished or removed task is filtered
// here rather than rewritten out: a read must not write a person's own list,
// and the list's next write drops it for good.
//
// The WHOLE list is read and the page cut AFTER the filter, or finished
// entries would hide open ones past the page; [MaxPriorities] keeps it one
// small read with an exact total.
func readPriorityRows(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time) ([]TaskRow, int, error) {

	person, held, err := readPerson(ctx, tx, handle)
	if err != nil {
		return nil, 0, err
	}
	if !held || len(person.Priorities) == 0 {
		// Empty, not nil: see [readTasks].
		return []TaskRow{}, 0, nil
	}
	ids := person.Priorities
	open := openGroups()
	args := make([]any, 0, len(ids)+len(open))
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, open...)
	rows, _, err := readTasks(ctx, tx, tasksWhere(
		"t.removed_at IS NULL AND t.id IN ("+placeholders(len(ids))+
			") AND t.status_group IN ("+placeholders(len(open))+")", args),
		[]sortTerm{{Column: "t.id"}}, len(ids), dayStart)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]TaskRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]TaskRow, 0, min(len(rows), MyWorkRows))
	live := 0
	for _, id := range ids {
		row, listed := byID[id]
		if !listed {
			continue
		}
		// A task listed twice counts once: [Writer.WriteDocument] writes
		// a person record whole, past the priorities write's dedupe.
		delete(byID, id)
		live++
		if len(out) < MyWorkRows {
			out = append(out, row)
		}
	}
	return out, live, nil
}

// openAsksFrom and openAsksWhere are the open asks put to one person, bound
// to their record name as the one argument: every reader of "what is waiting
// on this person" states them through here.
const (
	openAsksFrom = `tracker_comments c
		JOIN tracker_tasks t ON t.id = c.task_id`
	openAsksWhere = `c.ask = ?
		  AND c.resolved = 0 AND c.answered_by IS NULL
		  AND c.removed = 0 AND c.overtaken = '' AND t.removed_at IS NULL`

	// asksPageStatement is [readAsks]' page of them, newest first.
	asksPageStatement = `
		SELECT c.id, c.task_id, c.author, c.body, c.created_at, c.document
		FROM ` + openAsksFrom + `
		WHERE ` + openAsksWhere + `
		ORDER BY c.created_at DESC
		LIMIT ?`
)

// readAsks reads the questions waiting on this person.
//
// The open ones only, the partial index's own predicate. maxBytes is the ask
// text carried whole before the rest go by reference
// ([AskRow.BodyNotIncluded]); 0, for a screen with no ceiling, carries all.
func readAsks(ctx context.Context, tx *sql.Tx, handle string,
	dayStart time.Time, limit, maxBytes int) ([]AskRow, ClaimTotal, error) {

	// One FROM-and-WHERE for the page, the count and [Reader.Decisions]'
	// oldest-ask instant, for [taskBlock]'s reason.
	total, capped, err := countCapped(ctx, tx, openAsksFrom, openAsksWhere,
		[]any{handle})
	if err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: count the asks "+
			"waiting on %s: %w", handle, err)
	}
	claim := ClaimTotal{Total: total, Capped: capped}

	rows, err := tx.QueryContext(ctx, asksPageStatement, handle, limit)
	if err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: read the asks waiting on %s: %w",
			handle, err)
	}
	defer func() { _ = rows.Close() }()

	var pending []askRecord
	for rows.Next() {
		var a askRecord
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := rows.Scan(&a.comment, &a.task, &a.author, &a.body, &a.at,
			&a.document); err != nil {
			return nil, ClaimTotal{}, fmt.Errorf("worklet: scan an ask: %w", err)
		}
		pending = append(pending, a)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := rows.Err(); err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: read the asks waiting on %s: %w",
			handle, err)
	}
	out, err := askRowsOf(ctx, tx, pending, dayStart, maxBytes, true)
	if err != nil {
		return nil, ClaimTotal{}, err
	}
	return out, claim, nil
}

// askRecord is one ask as its comment row holds it, before the task it is on
// has been read.
type askRecord struct {
	comment, task, author, body string
	at                          int64
	document                    []byte
}

// askRowsOf is the asks' rows beside their tasks, in the order given: the one
// shape every list of asks shares ([readAsks], [readAnswered]). An ask whose
// task the read does not return is left out. open says they still wait, so
// each carries its answering call (an ask is answered once, ADR-0023).
// maxBytes is [readAsks]'.
func askRowsOf(ctx context.Context, tx *sql.Tx, pending []askRecord,
	dayStart time.Time, maxBytes int, open bool) ([]AskRow, error) {

	if len(pending) == 0 {
		return []AskRow{}, nil
	}
	ids := make([]any, 0, len(pending))
	for _, a := range pending {
		ids = append(ids, a.task)
	}
	tasks, _, err := readTasks(ctx, tx,
		tasksWhere("t.id IN ("+placeholders(len(ids))+")", ids),
		[]sortTerm{{Column: "t.id"}}, len(ids), dayStart)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]TaskRow, len(tasks))
	for _, row := range tasks {
		byID[row.ID] = row
	}
	out := make([]AskRow, 0, len(pending))
	weight := 0
	for _, a := range pending {
		row, held := byID[a.task]
		if !held {
			continue
		}
		var stored Comment
		if len(a.document) > 0 {
			if err := json.Unmarshal(a.document, &stored); err != nil {
				return nil, fmt.Errorf("worklet: decode the ask %s: %w", a.comment, err)
			}
		}
		ask := AskRow{
			TaskRow: row, Comment: a.comment, AskedBy: a.author,
			AskedAt:  store.DecodeTime(a.at),
			Body:     a.body,
			Decision: stored.Decision,
			Open:     open,
		}
		if open {
			// On the task's ADDRESS, not its key: through a key
			// another task claimed first, the answer would land on
			// the claimant, where the comment it `answers` is not.
			ask.Answer = answerCall(row.Address(), a.comment, stored.Decision)
		}
		// Whole or by reference: the first ask is always whole; one
		// past the budget names its size and its read, on the answer's
		// address.
		if maxBytes > 0 && len(out) > 0 && weight+len(a.body) > maxBytes {
			ask.Body = ""
			ask.BodyNotIncluded = fmt.Sprintf("%d bytes, left out to keep this answer "+
				"readable — read it with "+GetWorkItemTool+"(item=%q, comment=%q)",
				len(a.body), row.Address(), a.comment)
		} else {
			weight += len(a.body)
		}
		out = append(out, ask)
	}
	return out, nil
}

// answerCall is the literal call that answers an ask.
//
// Composed here, not described: every call a model composes differently is a
// round spent being refused. A decision's call carries its recommendation as
// the choice, the answer most give; a dissenter changes one value. The item is
// the task's ADDRESS ([ItemAddress]), never its bare key.
func answerCall(address, comment string, decision *decision.Decision) string {
	if decision != nil && decision.Recommended != "" {
		return fmt.Sprintf("%s(item: %q, body: \"…\", answers: %q, choice: %q)",
			CommentOnWorkTool, address, comment, decision.Recommended)
	}
	return fmt.Sprintf("%s(item: %q, body: \"…\", answers: %q)",
		CommentOnWorkTool, address, comment)
}

// The checklist items one person owns, bound to their record name as the one
// argument, and [readChecklistClaims]' page of them, named for the plan test.
//
// The non-empty owner test is `tracker_checklist_items_assignee_idx`'s partial
// predicate, stated because the planner does not infer it from the equality:
// without it every turn's claims scan the company's checklist items from the
// heap instead of one seek.
const (
	claimsFrom = `tracker_checklist_items i
		JOIN tracker_tasks t ON t.id = i.task_id`
	claimsWhere = `i.assignee = ? AND i.assignee <> '' AND i.done = 0
		AND t.removed_at IS NULL`
	claimsPageStatement = `
		SELECT i.task_id, t.key, t.key_collision, t.title, i.checklist_id,
		       i.item_id, i.name, i.done
		FROM ` + claimsFrom + `
		WHERE ` + claimsWhere + `
		ORDER BY t.key, i.ord
		LIMIT ?`
)

// readChecklistClaims reads the sub-items this person owns, and how many.
func readChecklistClaims(ctx context.Context, tx *sql.Tx, handle string) (
	[]ChecklistRow, ClaimTotal, error) {

	// One FROM-and-WHERE for the page and the count: see [taskBlock].
	total, capped, err := countCapped(ctx, tx, claimsFrom, claimsWhere,
		[]any{handle})
	if err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: count the checklist "+
			"items of %s: %w", handle, err)
	}
	claim := ClaimTotal{Total: total, Capped: capped}

	rows, err := tx.QueryContext(ctx, claimsPageStatement, handle, MyWorkRows)
	if err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: read the checklist items of %s: %w",
			handle, err)
	}
	defer func() { _ = rows.Close() }()

	out := []ChecklistRow{}
	for rows.Next() {
		var row ChecklistRow
		var done, collision int
		if err := rows.Scan(&row.Task, &row.TaskKey, &collision, &row.TaskTitle,
			&row.Checklist, &row.Item, &row.Name, &done); err != nil {
			return nil, ClaimTotal{}, fmt.Errorf("worklet: scan a checklist item: %w", err)
		}
		row.Done = done != 0
		row.TaskKeyCollision = collision == 1
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, ClaimTotal{}, fmt.Errorf("worklet: read the checklist items of %s: %w",
			handle, err)
	}
	return out, claim, nil
}
