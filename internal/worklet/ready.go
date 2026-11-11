package worklet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A seat's READY work, in the order the work pickup offers it (ADR-0112).
//
// # Ready
//
// Assigned to the seat, `in_progress` or `todo`, not archived, not mid-merge,
// not in the trash, past its start date if it has one, waiting on no open
// blocker, and not held on a question the seat itself asked on it and nobody
// has answered yet. `in_review` is ready on the same terms for the seat that
// holds it: as its review when somebody else moved it into review, and, when
// the seat moved it there itself, as a review nobody holds (a seat never
// reviews what it submitted itself, so it acts on the review it was given or
// names a reviewer).
//
// AND ONE WAIT IS READY BEFORE ITS DATE: an agent's wait whose reason names a
// person its writer has not asked on the item ([PersonsNamed], ADR-0122). A
// wait on a person is an ask, so such a wait comes back to be asked rather
// than sit out a date nobody was told of, whenever and however it was written.
//
// # The order
//
// Started work first, then the reviews the seat holds, then its own items in
// review that nobody reviews, then its priority list in its lead's order, then
// the queue: priority, then due date, the order `my_work` lists `assigned` in.
//
// # What a pickup keys on, and whom its turn works for
//
// [ReadyItem.Moved] is the last change to the item that would have woken the
// seat, so neither its own comment nor a change that only tells it (another
// agent's words that ask nothing, a watcher added) hands it the same item
// again. [ReadyItem.Quiet] is when the item went quiet for the seat, which the
// pickup rechecks once a day (ADR-0122).
// [ReadyItem.For] is whom the item's work is for, so the turn is held to
// their apps (ADR-0109): whom its assignment, else its creation, was written
// for, as the history row keeps it (`working_for`), else that row's writer.

// ReadyRows is one page of ready work: the pickup reads the next page only
// when every item on this one was already offered.
const ReadyRows = MyWorkRows

// MaxReady is how many items of each status the order is taken over: a seat
// holding more is offered its first MaxReady of each, and told so.
const MaxReady = 200

// ReadyWorkQuery asks for one page of a seat's ready work.
type ReadyWorkQuery struct {
	// Handle is whose. Required.
	Handle string

	// Level is the read level; a pickup reads at the node's own copy.
	Level statelog.ReadLevel

	// Page is which page of [ReadyRows] items, from 0.
	Page int

	// People is the chart's persons a wait's reason can name: an agent's
	// wait naming one it has not asked is ready before its date.
	People []PersonSeat
}

// ReadyItem is one item a seat could start now, and why it sits where it does.
type ReadyItem struct {
	TaskRow

	// Why is what put it at its place ([types.PickupWhy]).
	Why types.PickupWhy `json:"why"`

	// Moved is the log position of the last change to the item that would
	// have woken the seat, zero where none has, and MovedAt that change's
	// instant (the item's creation where Moved is zero).
	Moved   uint64    `json:"moved"`
	MovedAt time.Time `json:"moved_at"`

	// Progressed is the log position of the seat's own last change to a
	// started item other than a comment: its progress, which hands the
	// item over again. Zero on an item not started.
	Progressed uint64 `json:"progressed,omitempty"`

	// Quiet is when the item went quiet for the seat: the latest of MovedAt,
	// the start it waited until (once passed) and the seat's own last write
	// on it since, whatever that write was (ADR-0122). AN ITEM STILL READY
	// is one no answer took out of the seat's ready set: an answer that moved
	// its pickup key has it handed again anyway, and one that did not, an
	// edit of its content, leaves it as quiet as words do.
	Quiet time.Time `json:"quiet,omitzero"`

	// For is whom the item's work is for: whom its last assignment was
	// written for, a change that moved its assignee whatever kind its
	// record ended with ([readyAssignedArm]), else whom its creation was
	// written for ([readyCreatedArm]) — whom the writing turn worked for
	// where an agent wrote it for somebody, and otherwise the writer itself;
	// nil for the seat's own work.
	For *types.WorkingFor `json:"for,omitempty"`

	// SubmittedBy is who moved an item in review into review (or filed it
	// there): the review's asker, the seat itself on one nobody reviews.
	SubmittedBy string `json:"submitted_by,omitempty"`

	// WaitsOn is the persons an agent's wait on the item names without its
	// writer's open ask, by handle: set on an item ready before its date
	// ([types.PickupWaitsOnPerson]).
	WaitsOn []string `json:"waits_on,omitempty"`
}

// ReadyWork is one page of a seat's ready items in pickup order.
type ReadyWork struct {
	Handle string      `json:"handle"`
	Items  []ReadyItem `json:"items"`

	// Total is how many ready items the order holds, More whether a page
	// follows this one, and Capped whether a status held more than
	// [MaxReady], so Total is a floor.
	Total  int  `json:"total"`
	More   bool `json:"more,omitempty"`
	Capped bool `json:"capped,omitempty"`

	// Fingerprint names the whole ordered set, every item's id, status and
	// version, so a pickup that found every item offered asks again only
	// once something in it moved.
	Fingerprint string `json:"fingerprint"`

	Level      statelog.ReadLevel `json:"read_level"`
	Complete   bool               `json:"complete"`
	Incomplete *Incomplete        `json:"incomplete,omitempty"`
}

// ReadyWork answers one page of what a seat could start now, at `now`.
func (r *Reader) ReadyWork(ctx context.Context, q ReadyWorkQuery, now time.Time) (ReadyWork, error) {
	if q.Handle == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no seat; " +
			"the pickup names the seat it is deciding for")
	}
	if q.Level == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no level; " +
			"the pickup states the level it reads at")
	}
	if q.Page < 0 {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work page is %d; pages count from 0", q.Page)
	}
	out := ReadyWork{Handle: q.Handle, Items: []ReadyItem{}}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for my_work's reason: a seat's work is in every project.
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		return readReady(ctx, tx, q, now, &out)
	})
	if err != nil {
		return ReadyWork{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// readyStatuses is the statuses ready work is in, started work first.
var readyStatuses = []Status{StatusInProgress, StatusInReview, StatusTodo}

// readyBlock is one status's ready items for a seat, in queue order: named for
// the plan test, which holds it to a seek of the queue index.
func readyBlock(handle string, status Status, now time.Time) blockQuery {
	return heldBlock(handle, status, "(t.start_at IS NULL OR t.start_at <= ?)", now)
}

// waitBlock is one status's items a seat holds that wait until a later date
// with a reason: the candidates for a wait on a person ([readyWaits]).
func waitBlock(handle string, status Status, now time.Time) blockQuery {
	return heldBlock(handle, status, "t.start_at > ? "+
		"AND COALESCE(json_extract(t.document, '$.wait_reason'), '') <> ''", now)
}

// heldBlock is one status's items a seat holds that wait on no open blocker
// and no question of its own, whose start meets started, in queue order.
func heldBlock(handle string, status Status, started string, now time.Time) blockQuery {
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group = ? " +
			"AND t.status = ? AND t.archived = 0 AND t.merging = 0 " +
			"AND " + started + " " +
			"AND NOT EXISTS (SELECT 1 FROM tracker_task_deps d " +
			"WHERE d.task_id = t.id AND d.blocker_open = 1) " +
			"AND NOT EXISTS (SELECT 1 FROM tracker_comments c " +
			"WHERE c.task_id = t.id AND c.author = ? AND c.ask <> '' " +
			"AND c.resolved = 0 AND c.answered_by IS NULL AND c.removed = 0)",
		args: []any{handle, string(status.Group()), string(status),
			store.EncodeTime(now), handle},
		terms: []sortTerm{
			{Column: "t.prio_rank", Descending: true},
			{Column: "t.due_at"}, {Column: "t.id"},
		},
	}
}

func readReady(ctx context.Context, tx *sql.Tx, q ReadyWorkQuery, now time.Time,
	out *ReadyWork) error {

	var blocks [3][]TaskRow
	for i, status := range readyStatuses {
		block := readyBlock(q.Handle, status, now)
		rows, more, err := readTasks(ctx, tx, tasksWhere(block.where, block.args),
			block.terms, MaxReady, now)
		if err != nil {
			return fmt.Errorf("worklet: read %s's %s work: %w", q.Handle, status, err)
		}
		blocks[i] = rows
		out.Capped = out.Capped || more != ""
	}
	reviews, own, err := readyReviews(ctx, tx, q.Handle, blocks[1])
	if err != nil {
		return err
	}
	waits, err := readyWaits(ctx, tx, q, now, &out.Capped)
	if err != nil {
		return err
	}
	person, _, err := readPerson(ctx, tx, q.Handle)
	if err != nil {
		return err
	}
	all := orderReady(blocks[0], reviews, own, waits, blocks[2], person.Priorities)
	out.Total = len(all)
	out.Fingerprint = fingerprintOf(all)
	from := min(q.Page*ReadyRows, len(all))
	to := min(from+ReadyRows, len(all))
	out.More = to < len(all)
	out.Items = all[from:to]
	if err := loadMoved(ctx, tx, q.Handle, out.Items); err != nil {
		return err
	}
	if err := loadProgressed(ctx, tx, q.Handle, out.Items); err != nil {
		return err
	}
	if err := loadQuiet(ctx, tx, q.Handle, out.Items, now); err != nil {
		return err
	}
	return loadFor(ctx, tx, q.Handle, out.Items)
}

// fingerprintOf names an ordered ready set by its items' ids, statuses and
// versions.
func fingerprintOf(items []ReadyItem) string {
	h := sha256.New()
	for _, item := range items {
		fmt.Fprintf(h, "%s:%s:%d;", item.ID, item.Status, item.Version)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readyProgressArm is one started item's last change by the seat itself that
// changed a field: a comment or a write that changed nothing is no progress.
const readyProgressArm = `SELECT h.subject_id, h.log_seq, h.created_at FROM tracker_history h
	WHERE h.subject_id = ? AND h.actor = ? AND h.fields_json <> '{}'
	  AND h.kind NOT IN ('comment', 'comment_edited', 'comment_resolved', 'comment_removed')
	ORDER BY h.log_seq DESC LIMIT 1`

// loadProgressed stamps each started item with the seat's own last change to
// it that is not a comment.
func loadProgressed(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	var args []any
	for _, item := range items {
		if item.Status == StatusInProgress {
			args = append(args, item.ID, handle)
		}
	}
	if len(args) == 0 {
		return nil
	}
	got, err := seqAt(ctx, tx, arms(len(args)/2, readyProgressArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read %s's progress on its started work: %w", handle, err)
	}
	for i := range items {
		items[i].Progressed = got[items[i].ID].seq
	}
	return nil
}

// readyLastWordArm is the seat's own last write to one item after the change
// that last woke it (its log position) and the start it waited until (its
// instant), whatever the write was. A creation, a change of who watches and
// nothing else, and a write that changed nothing are no word at all; a watch
// written beside a change is that change ([movedBeyondWatch]).
const readyLastWordArm = `SELECT h.subject_id, h.log_seq, h.created_at
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.actor = ? AND h.log_seq > ? AND h.created_at >= ?
	  AND h.kind <> '` + string(ChangeCreated) + `'
	  AND (` + movedBeyondWatch + ` OR h.kind IN ` + conversationKinds + `)
	ORDER BY h.log_seq DESC LIMIT 1`

// loadQuiet stamps each item with when it went quiet for the seat: the latest
// of the change that last woke it, the start it waited until where that has
// passed, and the seat's own last word on it since.
func loadQuiet(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 4*len(items))
	for _, item := range items {
		var start int64
		if waited, ok := passedStart(item, now); ok {
			start = store.EncodeTime(waited)
		}
		args = append(args, item.ID, handle, int64(item.Moved), start)
	}
	last, err := seqAt(ctx, tx, arms(len(items), readyLastWordArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read %s's last word on its ready work: %w", handle, err)
	}
	for i := range items {
		quiet := items[i].MovedAt
		if waited, ok := passedStart(items[i], now); ok && waited.After(quiet) {
			quiet = waited
		}
		if w, said := last[items[i].ID]; said && w.at.After(quiet) {
			quiet = w.at
		}
		items[i].Quiet = quiet
	}
	return nil
}

// passedStart is the start item waited until, where that start has passed: a
// wait on a person is ready before its date, which bounds nothing yet.
func passedStart(item ReadyItem, now time.Time) (time.Time, bool) {
	if item.Start == nil || item.Start.After(now) {
		return time.Time{}, false
	}
	return *item.Start, true
}

// readyWaitArm is who last set one item's wait and why it waits: the newest
// row that moved its start or its reason, since a start moved without a reason
// takes the old reason off.
const readyWaitArm = `SELECT h.subject_id, h.actor, h.actor_kind,
	COALESCE(json_extract(t.document, '$.wait_reason'), '')
	FROM tracker_history h JOIN tracker_tasks t ON t.id = h.subject_id
	WHERE h.subject_id = ? AND (json_extract(h.fields_json, '$.start') IS NOT NULL
	  OR json_extract(h.fields_json, '$.wait_reason') IS NOT NULL)
	ORDER BY h.log_seq DESC LIMIT 1`

// readyWaitAsksArm is whom one writer's open asks on one item are put to.
const readyWaitAsksArm = `SELECT c.task_id, c.ask FROM tracker_comments c
	WHERE c.task_id = ? AND c.author = ? AND c.ask <> '' AND c.resolved = 0
	  AND c.answered_by IS NULL AND c.removed = 0`

// waitRow is an item set aside until a later date, and the persons its wait's
// reason names that its writer has not asked.
type waitRow struct {
	TaskRow
	on []string
}

// readyWaits is the items q's seat holds that an agent set aside until a later
// date with a reason naming a person ([PersonsNamed]) the agent has not asked
// on the item, in queue order, each status's started work first.
func readyWaits(ctx context.Context, tx *sql.Tx, q ReadyWorkQuery, now time.Time,
	capped *bool) ([]waitRow, error) {

	if len(q.People) == 0 {
		return nil, nil
	}
	var candidates []TaskRow
	for _, status := range readyStatuses {
		block := waitBlock(q.Handle, status, now)
		rows, more, err := readTasks(ctx, tx, tasksWhere(block.where, block.args),
			block.terms, MaxReady, now)
		if err != nil {
			return nil, fmt.Errorf("worklet: read %s's %s work set aside: %w", q.Handle, status, err)
		}
		candidates = append(candidates, rows...)
		*capped = *capped || more != ""
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(candidates))
	for _, row := range candidates {
		args = append(args, row.ID)
	}
	rows, err := tx.QueryContext(ctx, arms(len(candidates), readyWaitArm), args...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read who set %s's work aside: %w", q.Handle, err)
	}
	type wait struct{ writer, reason string }
	waits := map[string]wait{}
	for rows.Next() {
		var id, actor, kind, reason string
		if serr := rows.Scan(&id, &actor, &kind, &reason); serr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("worklet: read who set %s's work aside: %w", q.Handle, serr)
		}
		// AN AGENT'S WAIT ALONE: a person's own wait is written as given.
		if AuthorKind(kind) == AuthorAgent {
			waits[id] = wait{writer: actor, reason: reason}
		}
	}
	if cerr := closeRows(rows); cerr != nil {
		return nil, fmt.Errorf("worklet: read who set %s's work aside: %w", q.Handle, cerr)
	}
	var named []waitRow
	var askArgs []any
	for _, row := range candidates {
		w, ok := waits[row.ID]
		if !ok {
			continue
		}
		if on := PersonsNamed(w.reason, q.People); len(on) > 0 {
			named = append(named, waitRow{TaskRow: row, on: on})
			askArgs = append(askArgs, row.ID, w.writer)
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	asked, err := tx.QueryContext(ctx, arms(len(named), readyWaitAsksArm), askArgs...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the asks behind %s's waits: %w", q.Handle, err)
	}
	open := map[[2]string]bool{}
	for asked.Next() {
		var id, ask string
		if serr := asked.Scan(&id, &ask); serr != nil {
			_ = asked.Close()
			return nil, fmt.Errorf("worklet: read the asks behind %s's waits: %w", q.Handle, serr)
		}
		open[[2]string{id, ask}] = true
	}
	if cerr := closeRows(asked); cerr != nil {
		return nil, fmt.Errorf("worklet: read the asks behind %s's waits: %w", q.Handle, cerr)
	}
	out := make([]waitRow, 0, len(named))
	for _, row := range named {
		var unasked []string
		for _, handle := range row.on {
			if !open[[2]string{row.ID, handle}] {
				unasked = append(unasked, handle)
			}
		}
		if len(unasked) > 0 {
			out = append(out, waitRow{TaskRow: row.TaskRow, on: unasked})
		}
	}
	return out, nil
}

// orderReady is the pickup order over the blocks and the priority list:
// started work in queue order, then the reviews the seat holds, then its own
// items in review nobody reviews, then the waits on a person nobody asked,
// then the waiting items the list names in its order, then the rest in queue
// order.
func orderReady(started []TaskRow, reviews, own []reviewRow, onPerson []waitRow,
	waiting []TaskRow, priorities []string) []ReadyItem {

	out := make([]ReadyItem, 0, len(started)+len(reviews)+len(own)+len(onPerson)+len(waiting))
	for _, row := range started {
		out = append(out, ReadyItem{TaskRow: row, Why: types.PickupInProgress})
	}
	for _, r := range reviews {
		out = append(out, ReadyItem{TaskRow: r.TaskRow, Why: types.PickupReview, SubmittedBy: r.by})
	}
	for _, r := range own {
		out = append(out, ReadyItem{TaskRow: r.TaskRow, Why: types.PickupUnreviewed, SubmittedBy: r.by})
	}
	for _, w := range onPerson {
		out = append(out, ReadyItem{TaskRow: w.TaskRow, Why: types.PickupWaitsOnPerson, WaitsOn: w.on})
	}
	listed := make(map[string]bool, len(priorities))
	byID := make(map[string]TaskRow, len(waiting))
	for _, row := range waiting {
		byID[row.ID] = row
	}
	for _, id := range priorities {
		if row, ok := byID[id]; ok && !listed[id] {
			listed[id] = true
			out = append(out, ReadyItem{TaskRow: row, Why: types.PickupPriorityList})
		}
	}
	for _, row := range waiting {
		if !listed[row.ID] {
			out = append(out, ReadyItem{TaskRow: row, Why: types.PickupQueue})
		}
	}
	return out
}

// arms is one statement of n arms joined by UNION ALL, each a seek of its own
// item: the newest row of each item that one predicate selects.
func arms(n int, arm string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "SELECT * FROM (" + arm + ")"
	}
	return strings.Join(parts, " UNION ALL ")
}

// readyMovedArm is the newest notice to the seat about one item that started
// its turn: a change that would have woken it, by the notifier's own decision.
const readyMovedArm = `SELECT n.task_id, n.log_seq, n.created_at
	FROM tracker_notifications n INDEXED BY tracker_notifications_woke_idx
	WHERE n.task_id = ? AND n.recipient = ? AND n.starts_turn = 1 ORDER BY n.log_seq DESC LIMIT 1`

// readyReviewArm is who moved one item into its status, or filed it where no
// status move came after: who asked for the review of an item in review.
const readyReviewArm = `SELECT h.subject_id, h.actor FROM tracker_history h
	WHERE h.subject_id = ?
	  AND (json_extract(h.fields_json, '$.status.to') IS NOT NULL OR h.kind = 'created')
	ORDER BY h.log_seq DESC LIMIT 1`

// reviewRow is an item in review and who moved it there.
type reviewRow struct {
	TaskRow
	by string
}

// readyReviews splits the items in review handle holds by who moved them into
// review: somebody else's are its reviews, and its own are reviews nobody
// holds, since a seat never reviews what it submitted itself. Either way none
// is held on a question the seat asked ([readyBlock]), which is how a review
// put to a person waits on them.
func readyReviews(ctx context.Context, tx *sql.Tx, handle string,
	inReview []TaskRow) (reviews, own []reviewRow, err error) {

	if len(inReview) == 0 {
		return nil, nil, nil
	}
	args := make([]any, 0, len(inReview))
	for _, row := range inReview {
		args = append(args, row.ID)
	}
	rows, err := tx.QueryContext(ctx, arms(len(inReview), readyReviewArm), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
	}
	defer rows.Close()
	askedBy := make(map[string]string, len(inReview))
	for rows.Next() {
		var id, actor string
		if err := rows.Scan(&id, &actor); err != nil {
			return nil, nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
		}
		askedBy[id] = actor
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("worklet: read who asked for %s's reviews: %w", handle, err)
	}
	for _, row := range inReview {
		by, known := askedBy[row.ID]
		switch {
		case !known:
		case by != handle:
			reviews = append(reviews, reviewRow{row, by})
		default:
			own = append(own, reviewRow{row, by})
		}
	}
	return reviews, own, nil
}

// readyFirstArm is one item's first change, the creation the seat made itself.
const readyFirstArm = `SELECT h.subject_id, h.log_seq, h.created_at FROM tracker_history h
	WHERE h.subject_id = ? ORDER BY h.log_seq LIMIT 1`

// readyAssignedArm is one item's last assignment by a person, a colleague or
// a credential other than the seat — anybody but the engine's own duties — or
// the seat's own take made in a turn for somebody.
//
// AN ASSIGNMENT IS A ROW THAT MOVED THE ASSIGNEE, read off its assignee delta
// whatever kind its record ended with, as readyReviewArm reads a review
// request off the status delta. A record carries one kind, and a write that
// moves the assignee and another field in one call, the usual hand-off
// "assign it to you and set it to todo", is filed under the other change's
// kind ([ChangeKind.MovedFields]). Read by its kind, that hand-off was
// invisible, and the pickup worked for the assigner before it (#930). An
// assignment to the seat that already holds the item moves nothing, records no
// delta, and is no assignment here: another colleague's reassertion changes
// nothing about whom the work is for.
//
// THE CREATION OUTRANKS NOTHING. An item filed already assigned records the
// assignee's delta on its creation row, its oldest, so any later assignment by
// somebody else wins; where the creation is the only one, the arm reads whom
// its filing was for, whom readyCreatedArm names anyway.
//
// THE SEAT'S OWN ROWS STAY OUT, BUT FOR ITS OWN TAKE MADE FOR SOMEBODY: an
// agent's row that assigned the item to its own writer and carries whom that
// turn worked for (ADR-0109's carrier). The arm reads the newest admitted row,
// so the seat's own uncarried moves, and its moves away from itself — a
// hand-away, an unassignment — are never admitted: a take-back made on the
// seat's own initiative reads through to the newest move by somebody else, or
// to the seat's own take made for somebody. Whom the seat works for is never
// itself.
const readyAssignedArm = `SELECT h.subject_id, h.actor, h.actor_kind, h.actor_apps, h.working_for
	FROM tracker_history h
	WHERE h.subject_id = ? AND json_extract(h.fields_json, '$.assignee.to') IS NOT NULL
	  AND (h.actor <> ? OR (h.actor_kind = 'agent' AND h.working_for IS NOT NULL
	    AND json_extract(h.fields_json, '$.assignee.to') = h.actor))
	  AND h.actor_kind <> 'system'
	ORDER BY h.log_seq DESC LIMIT 1`

// readyCreatedArm is one item's creation by a person, a colleague or a
// credential other than the seat, or by the seat itself in a turn for
// somebody: whom work filed already assigned is for.
//
// THE SEAT'S OWN CREATION COUNTS ONLY AS AN AGENT'S ROW THAT CARRIES WHOM ITS
// TURN WORKED FOR, the rule [whomFor] believes a carrier by: work a seat filed
// for itself is its own, and work it filed for somebody is theirs.
const readyCreatedArm = `SELECT h.subject_id, h.actor, h.actor_kind, h.actor_apps, h.working_for
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.kind = 'created'
	  AND (h.actor <> ? OR (h.actor_kind = 'agent' AND h.working_for IS NOT NULL))
	  AND h.actor_kind <> 'system'
	ORDER BY h.log_seq LIMIT 1`

// loadMoved stamps each item with the last change that would have woken handle,
// and that change's instant; an item no such change reached takes the instant
// it was created.
func loadMoved(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(items))
	for _, item := range items {
		args = append(args, item.ID, handle)
	}
	got, err := seqAt(ctx, tx, arms(len(items), readyMovedArm), args)
	if err != nil {
		return fmt.Errorf("worklet: read who last changed %s's ready work: %w", handle, err)
	}
	var lonely []any
	for i := range items {
		if at, ok := got[items[i].ID]; ok {
			items[i].Moved, items[i].MovedAt = at.seq, at.at
			continue
		}
		lonely = append(lonely, items[i].ID)
	}
	if len(lonely) == 0 {
		return nil
	}
	first, err := seqAt(ctx, tx, arms(len(lonely), readyFirstArm), lonely)
	if err != nil {
		return fmt.Errorf("worklet: read when %s's own work was filed: %w", handle, err)
	}
	for i := range items {
		if at, ok := first[items[i].ID]; ok && items[i].Moved == 0 {
			items[i].MovedAt = at.at
		}
	}
	return nil
}

type seqInstant struct {
	seq uint64
	at  time.Time
}

func seqAt(ctx context.Context, tx *sql.Tx, query string, args []any) (map[string]seqInstant, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]seqInstant{}
	for rows.Next() {
		var id string
		var seq, at int64
		if err := rows.Scan(&id, &seq, &at); err != nil {
			return nil, err
		}
		out[id] = seqInstant{seq: uint64(seq), at: store.DecodeTime(at)}
	}
	return out, rows.Err()
}

// loadFor stamps each item with whom its work is for: whom its last admitted
// assignment was written for, else whom its admitted creation was ([whomFor]);
// nil for the seat's own work.
func loadFor(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(items))
	for _, item := range items {
		args = append(args, item.ID, handle)
	}
	assigned, err := writersOf(ctx, tx, arms(len(items), readyAssignedArm), args, handle)
	if err != nil {
		return fmt.Errorf("worklet: read who assigned %s's ready work: %w", handle, err)
	}
	filed, err := writersOf(ctx, tx, arms(len(items), readyCreatedArm), args, handle)
	if err != nil {
		return fmt.Errorf("worklet: read who filed %s's ready work: %w", handle, err)
	}
	for i := range items {
		if w, ok := assigned[items[i].ID]; ok {
			items[i].For = w
		} else {
			items[i].For = filed[items[i].ID]
		}
	}
	return nil
}

// writersOf runs one statement of writer arms and reads whom each item's row
// says its work is for, as reader reads it ([whomFor]).
func writersOf(ctx context.Context, tx *sql.Tx, query string, args []any, reader string) (
	map[string]*types.WorkingFor, error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]*types.WorkingFor{}
	for rows.Next() {
		var id, actor, kind, apps string
		var workingFor sql.NullString
		if serr := rows.Scan(&id, &actor, &kind, &apps, &workingFor); serr != nil {
			_ = rows.Close()
			return nil, serr
		}
		out[id] = whomFor(AuthorKind(kind), actor, apps, workingFor, reader)
	}
	return out, closeRows(rows)
}

// whomFor is whom one admitted history row says the item's work is for, read
// by the seat reader: whom an agent's row was written for, as its record
// carried them (`working_for`, ADR-0109), and otherwise the row's writer
// ([workingForWriter]).
//
// A CARRIER IS BELIEVED ONLY FROM AN AGENT'S ROW, the rule a wake is read by:
// a person, a credential and the engine write for nobody but themselves, so a
// carrier on one of their rows would make the pickup work for somebody that
// writer never spoke for. A carrier this build cannot read — one that does not
// decode, or decodes to nothing — is a stranger's, which holds no app, the
// closed end. A seat carrier naming the reader is the seat's own work, nil,
// as the ceiling reads one; a credential's apps are the ones its writing turn
// already narrowed, which the pickup's turn narrows again on its own node.
func whomFor(kind AuthorKind, actor, apps string, workingFor sql.NullString,
	reader string) *types.WorkingFor {

	if !workingFor.Valid || kind != AuthorAgent {
		return workingForWriter(kind, actor, apps)
	}
	var carried *types.WorkingFor
	if err := json.Unmarshal([]byte(workingFor.String), &carried); err != nil || carried == nil {
		return &types.WorkingFor{Kind: types.WorkingForStranger}
	}
	if carried.Kind == types.WorkingForSeat && strings.TrimSpace(carried.Seat) == reader {
		return nil
	}
	return carried
}

// closeRows closes rows and answers the error its walk ended on.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// workingForWriter is whom work a writer assigned is for: the writer's seat,
// or the credential with the apps it wrote under.
func workingForWriter(kind AuthorKind, actor, apps string) *types.WorkingFor {
	if kind != AuthorOperator {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: actor}
	}
	w := &types.WorkingFor{Kind: types.WorkingForCredential, Login: actor}
	if apps != "" {
		// A LIST THE APPLIER WROTE; one this build cannot read grants
		// nothing, the closed end.
		var grants []iam.Grant
		if json.Unmarshal([]byte(apps), &grants) == nil {
			w.Apps = grants
		}
	}
	return w
}
