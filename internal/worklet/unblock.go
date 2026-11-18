package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The unblocked repair is the one path by which a dependent hears it became
// workable: closing a blocker clears its edges and tells nobody
// ([Snapshot.Unblocked] is filled by [Writer.TellUnblocked] alone).
//
// It is bounded by its OWN LOG POSITION, never a clock window: a "last five
// minutes" query leaves every dependent cleared during a longer outage of the
// duty untold for ever, while a position catches up a gap of any length.
//
// "Last unblocked" is derived from the dependency edges, not a column, and is
// compared between two FLEET-AGREED instants (the blocker's effective clearing
// time and the dependent's `unblocked_told_at`): on authored clocks a skew
// re-issues a late wake for every task cleared in it, on every tick.

// Unblock is one dependent that is ready and has not been told.
type Unblock struct {
	// Task is the dependent, Key its addressable name, Project its
	// container and Assignee whoever is waiting on it.
	//
	// Both id and key: the record is written against the id, while a card,
	// `tracker_notifications.subject_key` and the prompt all need the key.
	Task     string
	Key      string
	Project  string
	Assignee string

	// ClearedAt is the effective instant its last blocker finished: what
	// `unblocked_told_at` is compared against and stamped with once told.
	ClearedAt int64
}

// UnblockScan is one tick's worth of repair work, and the position it read to.
//
// AN UNASSIGNED DEPENDENT IS SKIPPED: the notice's only recipient is the
// assignee (recipients.go routes off the Unblocked party list), so its record
// would cost the log and every node a delivery and reach no inbox. A skipped
// dependent is not found again, since the window advances by log position
// whether or not a row produced a notice; whoever is assigned later reads the
// task's state.
type UnblockScan struct {
	Pending []Unblock

	// Through is the position this scan covered. IT IS ADVANCED ONLY
	// AFTER THE COMMITS LAND: a crash mid-tick re-reads, and a repeated
	// wake is a duplicate the inbox collapses where a skipped one is lost.
	Through uint64
}

// unblockHorizonStatement is the repair's own horizon: the newest status row
// past the last one it read. Named so the plan test checks what
// [ScanUnblocked] issues.
const unblockHorizonStatement = `
		SELECT COALESCE(MAX(log_seq), ?) FROM tracker_history
		WHERE log_seq > ?
		  AND json_extract(fields_json, '$.status.to') IS NOT NULL`

// unblockedDependentsStatement is every ready dependent of a blocker whose
// status moved between two positions, read by [ScanUnblocked].
//
// THE WINDOW DRIVES IT, so the dependents are an IN over the window's rows
// rather than a join: a range on the repair's partial index, a seek per
// blocker's edges, a primary-key read per dependent. As a three-way join the
// planner (no statistics on any node) drives from the tasks instead, walking
// every live task each tick. An IN also needs no DISTINCT.
const unblockedDependentsStatement = `
		SELECT t.id, t.key, t.project_key, t.assignee,
		       (SELECT MAX(o.cleared_at) FROM tracker_task_deps o
		        WHERE o.task_id = t.id AND o.cleared_at IS NOT NULL)
		FROM tracker_tasks t
		WHERE t.id IN (SELECT d.task_id FROM tracker_history h
		               JOIN tracker_task_deps d ON d.blocker_id = h.subject_id
		               WHERE json_extract(h.fields_json, '$.status.to') IS NOT NULL
		                 AND h.log_seq > ? AND h.log_seq <= ?)
		  AND t.removed_at IS NULL
		  -- AN UNASSIGNED DEPENDENT HAS NOBODY TO TELL: see the
		  -- comment on ScanUnblocked.
		  AND t.assignee <> ''
		  AND NOT EXISTS (SELECT 1 FROM tracker_task_deps o
		                  WHERE o.task_id = t.id AND o.blocker_open = 1)
		  AND (SELECT MAX(o.cleared_at) FROM tracker_task_deps o
		       WHERE o.task_id = t.id AND o.cleared_at IS NOT NULL)
		      > COALESCE(t.unblocked_told_at, 0)
		ORDER BY t.id LIMIT ?`

// ScanUnblocked finds every dependent that became workable since a position.
//
// It reads status changes at `log_seq > since` (a range on
// `tracker_history_status_seq_idx`) on BLOCKERS, and keeps a dependent only
// when it has no open blocker left and its newest clearing is later than what
// it was last told; without both, it wakes for work not ready, or twice.
//
// A status change is a row CARRYING a status delta, never one filed under a
// status kind, as for [Applier.stampStatusEntered]: the kind is one word for a
// patch that may move several fields, and a quiet cancel filed under another
// word would otherwise never be reached, by the rows or by the horizon, so its
// dependents would never hear.
func ScanUnblocked(ctx context.Context, db store.ReplicatedReader, since uint64,
	limit int) (UnblockScan, error) {

	scan := UnblockScan{Through: since}
	err := db.Read(ctx, func(tx *sql.Tx) error {
		// THE POSITION FIRST, in the rows' transaction and over the
		// rows' predicate: a horizon read later, or over a wider set,
		// steps past records the scan never saw, and loses their wakes.
		if err := tx.QueryRowContext(ctx, unblockHorizonStatement,
			since, since).Scan(&scan.Through); err != nil {
			return fmt.Errorf("worklet: read the repair's own horizon: %w", err)
		}
		// The dependent's two predicates are subqueries over ALL its
		// edges, never aggregates over the window's: the window holds only
		// the blockers that moved, so "all cleared" would hold when one of
		// two closes, and its newest clearing is not the newest overall.
		rows, err := tx.QueryContext(ctx, unblockedDependentsStatement,
			since, scan.Through, limit)
		if err != nil {
			return fmt.Errorf("worklet: read the unblocked dependents since "+
				"%d: %w", since, err)
		}
		defer rows.Close()
		for rows.Next() {
			var u Unblock
			if err := rows.Scan(&u.Task, &u.Key, &u.Project, &u.Assignee,
				&u.ClearedAt); err != nil {
				return fmt.Errorf("worklet: read an unblocked dependent: %w", err)
			}
			scan.Pending = append(scan.Pending, u)
		}
		return rows.Err()
	})
	if err != nil {
		return UnblockScan{Through: since}, err
	}
	return scan, nil
}

// TellUnblocked publishes the LATE relations commit for one dependent.
//
// It is a record, not a direct wake: a wake the duty sent itself would be lost
// by the very failure it repairs, while every node's change feed turns the
// record's `Unblocked` party into a wake. The apply stamps `unblocked_told_at`
// as a MAX over every commit naming the task: order-independent, identical on
// every node, and a redelivery cannot re-issue the wake.
func (w *Writer) TellUnblocked(ctx context.Context, opID string, u Unblock) (WriteResult, error) {
	switch {
	case u.Task == "":
		return WriteResult{}, fmt.Errorf("worklet: an unblocked notice names no task")
	case u.Assignee == "":
		// THE ASSIGNEE IS THE WHOLE RECIPIENT LIST: a notice nobody
		// received would still stamp `unblocked_told_at`, and [ScanUnblocked]
		// would never look at the task again. The scan's own filter
		// is one query's predicate; this is the verb's rule, for any caller.
		return WriteResult{}, fmt.Errorf("worklet: the unblocked notice for "+
			"task %s names no assignee, and the assignee is the only person "+
			"it tells — publishing it would stamp the task told and stop the "+
			"repair ever looking at it again", u.Task)
	}
	// AN EMPTY PATCH: the repair changes no field, and the APPLIER stamps
	// `unblocked_told_at` from this notification, since a writer-supplied
	// instant would be one node's clock where every node must agree.
	return w.UpdateTask(ctx, opID, u.Task, u.Project, NoIfMatch, TaskPatch{},
		ChangeRelations, &Notify{
			Kind: ChangeRelations,
			// LATE marks the wake a repair, so whoever gets it hours after
			// the close sees why.
			Late: true,
			Snapshot: Snapshot{
				Key: u.Key, Project: u.Project,
				Unblocked: []TaskParty{{
					Task: u.Task, Key: u.Key, Assignee: u.Assignee,
				}},
			},
		})
}
