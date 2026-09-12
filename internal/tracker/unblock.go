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

// An unassigned dependent is skipped, and that is not a narrowing of the
// repair — it is the repair declining to manufacture a record with no
// recipient.
//
// The notice's only recipient is the dependent's own assignee (the
// Unblocked party list is what recipients.go routes off), so a repair record
// for an unassigned task names NOBODY: a full routing snapshot on the durable
// log, a change-feed delivery and an ack for every node in the company, a
// parse, a debug line — and no inbox row at the end of it. The duty
// manufactures one on a timer for every unassigned dependent that becomes
// workable, for ever.
//
// Skipping costs nothing, because the scan's window is bounded by the log
// position rather than by the told-stamp: "since" advances on every tick
// whether or not a row produced a notice, so a skipped dependent is simply
// never found again rather than found repeatedly. Somebody assigned the task
// afterwards reads its state when they pick it up.
//
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
// It reads status changes at `log_seq > since` — a keyset range on the
// (log_seq) index — restricted to subjects that are BLOCKERS, collects their
// dependents, and keeps a dependent only when it has no open blocker left and
// the newest clearing among its blockers is later than what it was last told.
//
// # THE ROW'S OWN STATUS DELTA DECIDES, NOT ITS KIND
//
// A row CARRYING a status delta is a status change, whatever it was filed
// under — and the kind cannot be trusted for this, for exactly the reason
// [Applier.recomputeSpans] states beside the same predicate. The kind is ONE
// word a writer chose for a patch that may have moved several things, so a
// change that moved the status and something else is filed under the something
// else and was invisible here.
//
// The reachable case was the sprint ROLLOVER CLOSE. It cancels every straggler
// — `StatusCancelled` is in the `done` group, whose own description names this
// path — so the apply stamps the task finished and clears every edge naming it
// as a blocker, and its dependents become workable. The record is quiet by
// design, and nothing outside [Writer.TellUnblocked] ever fills
// `Snapshot.Unblocked`, so this scan is the ONLY path by which those people
// hear. Filed under the sprint move, the join never reached them — and the
// horizon below, computed from the same predicate, then advanced past the
// record on the next status row anywhere in the log, so it was never
// reconsidered. "A gap of any length is caught up on the next tick" does not
// hold for a row the predicate cannot name.
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
		// THE HORIZON IS OVER THE SAME PREDICATE AS THE ROWS, which is
		// what makes advancing it safe: a horizon computed over a WIDER
		// set steps past records the scan below never looked at.
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(log_seq), ?) FROM tracker_history
			WHERE log_seq > ?
			  AND json_extract(fields_json, '$.status.to') IS NOT NULL`,
			since, since).Scan(&scan.Through); err != nil {
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
			WHERE json_extract(h.fields_json, '$.status.to') IS NOT NULL
			  AND h.log_seq > ? AND h.log_seq <= ?
			  AND t.removed_at IS NULL
			  -- AN UNASSIGNED DEPENDENT HAS NOBODY TO TELL: see the
			  -- comment on ScanUnblocked.
			  AND t.assignee <> ''
			  AND NOT EXISTS (SELECT 1 FROM tracker_task_deps o
			                  WHERE o.task_id = t.id AND o.blocker_open = 1)
			  AND (SELECT MAX(o.cleared_at) FROM tracker_task_deps o
			       WHERE o.task_id = t.id AND o.cleared_at IS NOT NULL)
			      > COALESCE(t.unblocked_told_at, 0)
			ORDER BY t.id LIMIT ?`,
			since, scan.Through, limit)
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
	switch {
	case u.Task == "":
		return WriteResult{}, fmt.Errorf("tracker: an unblocked notice names no task")
	case u.Assignee == "":
		// THE ASSIGNEE IS THE WHOLE RECIPIENT LIST, so a notice without
		// one tells nobody — and it does not merely waste a record. The
		// apply stamps `unblocked_told_at` from this notification, and
		// [ScanUnblocked] selects on that column being older than the
		// clearing instant: a notice nobody received would mark the task
		// TOLD and the repair would never look at it again.
		//
		// The scan already filters `t.assignee <> ''`, so nothing in the
		// duty reaches this. That is what makes the guard worth having
		// rather than redundant: the filter is one query's predicate and
		// this is the verb's own rule, and a second caller would inherit
		// the rule rather than have to rediscover the predicate.
		return WriteResult{}, fmt.Errorf("tracker: the unblocked notice for "+
			"task %s names no assignee, and the assignee is the only person "+
			"it tells — publishing it would stamp the task told and stop the "+
			"repair ever looking at it again", u.Task)
	}
	// AN EMPTY PATCH, DELIBERATELY. The repair changes no field of the
	// task — it tells people the task is ready, which was already true —
	// and the one durable trace it leaves, `unblocked_told_at`, is
	// stamped by the APPLIER from this notification rather than carried
	// as a field: a writer-supplied instant would be one node's clock
	// where the column has to be a MAX every node computes alike.
	return w.UpdateTask(ctx, opID, u.Task, u.Project, NoIfMatch, TaskPatch{},
		ChangeRelations, &Notify{
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
