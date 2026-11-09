package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A seat's READY work: the assigned items it could start now, in the order
// the work pickup offers them (ADR-0112).
//
// # Ready
//
// Assigned to the seat, `in_progress` or `todo`, not archived, not mid-merge,
// not in the trash, past its start date if it has one, waiting on no open
// blocker, and not held on a question the seat itself asked on it and nobody
// has answered yet. `in_review` is not ready: the work is done and a
// colleague holds the next move.
//
// # The order
//
// What the seat already started comes first, because an `in_progress` item
// with no run behind it is work somebody believes is moving. Then the seat's
// priority list, in the order its lead set. Then the queue: priority, then
// due date, the order `my_work` lists `assigned` in.
//
// # What a pickup keys on
//
// [ReadyItem.Moved] is the last change SOMEBODY ELSE made to the item, so a
// seat that answers a pickup with a comment of its own is not handed the same
// item again for it, while a reviewer's send-back, a person's comment or the
// unblocked repair hands it over again.

// ReadyRows is how many items each status reads: the page `my_work` reads its
// blocks in, for the reason [MyWorkRows] gives.
const ReadyRows = MyWorkRows

// ReadyWorkQuery asks for one seat's ready work.
type ReadyWorkQuery struct {
	// Handle is whose. Required.
	Handle string

	// Level is the read level; a pickup reads at the node's own copy.
	Level statelog.ReadLevel
}

// ReadyItem is one item a seat could start now, and why it sits where it does.
type ReadyItem struct {
	TaskRow

	// Why is what put it at its place ([types.PickupWhy]).
	Why types.PickupWhy `json:"why"`

	// Moved is the log position of the last change somebody other than the
	// seat made to the item: zero where nobody else ever has.
	Moved uint64 `json:"moved"`
}

// ReadyWork is a seat's ready items in pickup order.
type ReadyWork struct {
	Handle string      `json:"handle"`
	Items  []ReadyItem `json:"items"`

	Level      statelog.ReadLevel `json:"read_level"`
	Complete   bool               `json:"complete"`
	Incomplete *Incomplete        `json:"incomplete,omitempty"`
}

// ReadyWork answers what one seat could start now, at `now`.
func (r *Reader) ReadyWork(ctx context.Context, q ReadyWorkQuery, now time.Time) (ReadyWork, error) {
	if q.Handle == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no seat; " +
			"the pickup names the seat it is deciding for")
	}
	if q.Level == "" {
		return ReadyWork{}, fmt.Errorf("worklet: a ready-work read names no level; " +
			"the pickup states the level it reads at")
	}
	out := ReadyWork{Handle: q.Handle, Items: []ReadyItem{}}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for my_work's reason: a seat's work is in every
		// project.
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		items, err := readReady(ctx, tx, q.Handle, now)
		out.Items = items
		return err
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

// readyBlock is one status's ready items for a seat, in queue order: named for
// the plan test, which holds it to a seek of the queue index.
func readyBlock(handle string, status Status, now time.Time) blockQuery {
	return blockQuery{
		where: "t.removed_at IS NULL AND t.assignee = ? AND t.status_group = ? " +
			"AND t.status = ? AND t.archived = 0 AND t.merging = 0 " +
			"AND (t.start_at IS NULL OR t.start_at <= ?) " +
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

// readyMovedStatement is the last change somebody other than the seat made to
// each of a page of items: one seek of the history's subject index per item.
func readyMovedStatement(n int) string {
	return `SELECT h.subject_id, MAX(h.log_seq) FROM tracker_history h
		WHERE h.subject_id IN (` + placeholders(n) + `) AND h.actor <> ?
		GROUP BY h.subject_id`
}

func readReady(ctx context.Context, tx *sql.Tx, handle string, now time.Time) ([]ReadyItem, error) {
	started, _, err := readTasks(ctx, tx, readyBlockSet(handle, StatusInProgress, now),
		readyBlock(handle, StatusInProgress, now).terms, ReadyRows, now)
	if err != nil {
		return nil, fmt.Errorf("worklet: read %s's started work: %w", handle, err)
	}
	waiting, _, err := readTasks(ctx, tx, readyBlockSet(handle, StatusTodo, now),
		readyBlock(handle, StatusTodo, now).terms, ReadyRows, now)
	if err != nil {
		return nil, fmt.Errorf("worklet: read %s's waiting work: %w", handle, err)
	}
	person, _, err := readPerson(ctx, tx, handle)
	if err != nil {
		return nil, err
	}
	items := orderReady(started, waiting, person.Priorities)
	if err := loadMoved(ctx, tx, handle, items); err != nil {
		return nil, err
	}
	return items, nil
}

func readyBlockSet(handle string, status Status, now time.Time) taskSet {
	block := readyBlock(handle, status, now)
	return tasksWhere(block.where, block.args)
}

// orderReady is the pickup order over the two blocks and the priority list:
// started work in queue order, then the waiting items the list names in its
// order, then the rest of the waiting items in queue order.
func orderReady(started, waiting []TaskRow, priorities []string) []ReadyItem {
	out := make([]ReadyItem, 0, len(started)+len(waiting))
	for _, row := range started {
		out = append(out, ReadyItem{TaskRow: row, Why: types.PickupInProgress})
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

// loadMoved stamps each item with the last change somebody other than handle
// made to it.
func loadMoved(ctx context.Context, tx *sql.Tx, handle string, items []ReadyItem) error {
	if len(items) == 0 {
		return nil
	}
	args := make([]any, 0, len(items)+1)
	for _, item := range items {
		args = append(args, item.ID)
	}
	args = append(args, handle)
	rows, err := tx.QueryContext(ctx, readyMovedStatement(len(items)), args...)
	if err != nil {
		return fmt.Errorf("worklet: read who last changed %s's ready work: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	moved := make(map[string]uint64, len(items))
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			return err
		}
		moved[id] = uint64(seq)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		items[i].Moved = moved[items[i].ID]
	}
	return nil
}
