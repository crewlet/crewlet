package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The missed-unblocked repair.
//
// # Why it is bounded by a POSITION and never by a clock
//
// Closing a blocker wakes everything it was blocking, and that wake rides the
// closing record's own notification. When it does not — a record deferred and
// applied later, a notification the valve dropped, an edge written after the
// close — somebody is waiting on work that is ready and nobody has told them.
//
// The obvious repair is a query over the last five minutes. It has a hole
// exactly the size of the outage it exists to survive: a duty that did not run
// for six minutes — a lease flap, a singleton moving, a node restart — leaves
// every dependent unblocked in that window untold FOR EVER, because nothing
// ever looks further back.
//
// So the repair holds its OWN POSITION and each tick reads the records since
// it. A gap of any length is caught up on the next tick, and a repair that has
// never run reads from zero. That is what a durable log makes available and a
// wall clock does not.
//
// # And it reads the EDGES, not a column
//
// "When was this task last unblocked" is not a column on the task: it is
// derived from the dependency edges and the blockers' own rows, which is why
// there is no `blocked_cleared_at` here to query. The comparison is between
// two FLEET-AGREED instants — the blocker's effective clearing time and what
// the dependent was last told — because on authored clocks a sixty-second skew
// re-issues a late wake for every task cleared in that window, on every tick.

// Unblock is one dependent that is ready and has not been told.
type Unblock struct {
	// Task is the dependent, Key its addressable name, Project its
	// container and Assignee whoever is waiting on it.
	//
	// BOTH THE ID AND THE KEY, because they are read by different things
	// and neither substitutes for the other: the id is what the record is
	// written against, and the KEY is what a card shows, what
	// `tracker_notifications.subject_key` stores and what the prompt tells
	// the seat to fetch. Carrying only the id put a uuid in all three —
	// the one row in that column that was not a key, and a prompt asking a
	// seat to read `3f2a…` by name.
	Task     string
	Key      string
	Project  string
	Assignee string

	// ClearedAt is the effective instant its last blocker finished,
	// which is what the dependent's own `unblocked_told_at` is compared
	// against and what the repair stamps once it has told them.
	ClearedAt int64
}

// UnblockScan is one tick's worth of repair work, and the position it read to.
type UnblockScan struct {
	Pending []Unblock

	// Through is the position this scan covered. IT IS ADVANCED ONLY
	// AFTER THE COMMITS LAND, so a crash mid-tick re-reads the same
	// records rather than skipping them — a repeated wake is a duplicate
	// the inbox collapses, and a skipped one is somebody never told.
	Through uint64
}

// ScanUnblocked finds every dependent that became workable since a position.
//
// # The query, and why each clause is there
//
// It reads status changes at `log_seq > since` — an index range on
// (kind, log_seq), which is the sibling of the one every report's window
// predicate uses — restricted to subjects that are BLOCKERS, collects their
// dependents, and keeps a dependent only when it has no open blocker left and
// the newest clearing among its blockers is later than what it was last told.
//
// A dependent with any open edge is not ready. A dependent already told about
// a later clearing is not owed anything. Both are the difference between a
// repair and a source of duplicate wakes.
func ScanUnblocked(ctx context.Context, db *store.DB, since uint64,
	limit int) (UnblockScan, error) {

	scan := UnblockScan{Through: since}
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE POSITION FIRST, and from the same transaction as the rows:
		// a `through` read afterwards would cover records this scan did
		// not see, and advancing past them is how a wake is lost.
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(log_seq), ?) FROM tracker_history
			WHERE kind = ? AND log_seq > ?`,
			since, string(ChangeStatus), since).Scan(&scan.Through); err != nil {
			return fmt.Errorf("tracker: read the repair's own horizon: %w", err)
		}
		// THE TWO PREDICATES ABOUT THE DEPENDENT ARE SUBQUERIES OVER
		// ALL ITS EDGES, never aggregates over the joined ones.
		//
		// The join reaches only the blockers whose status changed in
		// this window, so an aggregate over it answers "has every
		// blocker THAT MOVED cleared" — which is true the moment one of
		// two closes, and wakes somebody for work they still cannot
		// start. The same is true of the clearing instant: the newest
		// clearing among the blockers that moved is not the newest among
		// the blockers.
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT t.id, t.key, t.project_key, t.assignee,
			       (SELECT MAX(o.cleared_at) FROM tracker_task_deps o
			        WHERE o.task_id = t.id AND o.cleared_at IS NOT NULL)
			FROM tracker_history h
			JOIN tracker_task_deps d ON d.blocker_id = h.subject_id
			JOIN tracker_tasks t ON t.id = d.task_id
			WHERE h.kind = ? AND h.log_seq > ? AND h.log_seq <= ?
			  AND t.removed_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM tracker_task_deps o
			                  WHERE o.task_id = t.id AND o.blocker_open = 1)
			  AND (SELECT MAX(o.cleared_at) FROM tracker_task_deps o
			       WHERE o.task_id = t.id AND o.cleared_at IS NOT NULL)
			      > COALESCE(t.unblocked_told_at, 0)
			ORDER BY t.id LIMIT ?`,
			string(ChangeStatus), since, scan.Through, limit)
		if err != nil {
			return fmt.Errorf("tracker: read the unblocked dependents since "+
				"%d: %w", since, err)
		}
		defer rows.Close()
		for rows.Next() {
			var u Unblock
			if err := rows.Scan(&u.Task, &u.Key, &u.Project, &u.Assignee,
				&u.ClearedAt); err != nil {
				return fmt.Errorf("tracker: read an unblocked dependent: %w", err)
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
// # Why it is a record and not a direct wake
//
// The wake is derived from a durable record by something that outlives the
// writer, everywhere else in this design, and a repair is no exception: a
// notification this duty sent directly would be lost by exactly the failure it
// exists to repair. The commit carries `Unblocked` filled with the dependent
// and its assignee, and every node's own change feed turns it into a wake.
//
// It stamps the dependent's `unblocked_told_at`, which is a MAX over every
// applied commit that named it — so it is order-independent and identical on
// every node, and a redelivery cannot re-issue the wake.
func (w *Writer) TellUnblocked(ctx context.Context, opID string, u Unblock) (WriteResult, error) {
	if u.Task == "" {
		return WriteResult{}, fmt.Errorf("tracker: an unblocked notice names no task")
	}
	// AN EMPTY PATCH, DELIBERATELY. The repair changes no field of the
	// task — it tells people the task is ready, which was already true —
	// and the one durable trace it leaves, `unblocked_told_at`, is
	// stamped by the APPLIER from this notification rather than carried
	// as a field: a writer-supplied instant would be one node's clock
	// where the column has to be a MAX every node computes alike.
	return w.UpdateTask(ctx, opID, u.Task, u.Project, NoIfMatch, TaskPatch{}, &Notify{
		Kind: ChangeRelations,
		// LATE, and the flag is what tells a reader this wake is a
		// repair rather than the change itself — a person who receives
		// it hours after the close should see why.
		Late: true,
		Snapshot: Snapshot{
			Key: u.Key, Project: u.Project,
			Unblocked: []TaskParty{{
				Task: u.Task, Key: u.Key, Assignee: u.Assignee,
			}},
		},
	})
}
