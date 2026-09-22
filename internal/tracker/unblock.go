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
//
// AN UNASSIGNED DEPENDENT IS SKIPPED, and that is not a narrowing of the
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
// position rather than by the told-stamp: an unassigned dependent is filtered
// BEFORE the bound, so it never takes a place in [UnblockScan.Pending] and
// never holds the window open, and the position advances past it on the first
// tick that drains that window. A skipped dependent is simply never found
// again rather than found repeatedly. Somebody assigned the task afterwards
// reads its state when they pick it up.
type UnblockScan struct {
	// Pending holds at most the `limit` the caller passed. THE BOUND IS
	// ON RECORDS, not on a screenful: every entry becomes one durable
	// commit every node in the company applies, so a tick that carried a
	// whole backlog would publish an unbounded number of them.
	//
	// NOTHING IS LOST TO THAT BOUND. What it leaves behind is still owed,
	// still inside this window, and found by the next tick from the SAME
	// position, because [UnblockScan.Through] does not move while
	// [UnblockScan.Truncated] is set.
	Pending []Unblock

	// Truncated says the window holds more owed dependents than this scan
	// carried — ASKED, NOT INFERRED: the query reads ONE ROW PAST the
	// bound and that row is dropped rather than carried, so its presence
	// is the evidence. `len(Pending) == limit` is a different fact and not
	// this one: a window holding exactly the limit holds everything it
	// has, and reading a full page as a cut would pin the repair's
	// position on a range it has already drained and re-read it for ever.
	//
	// It is the idiom `internal/pages`' listings use, for the same reason,
	// and the flag is the whole of what makes the bound above sound rather
	// than a silent cut.
	Truncated bool

	// Through is the position this scan covered, and the rule it obeys is
	// that IT NEVER OVERTAKES [UnblockScan.Pending]: a truncated scan
	// reports the position it was GIVEN, so the records it did not account
	// for stay inside the next tick's window.
	//
	// IT IS ADVANCED ONLY AFTER THE COMMITS LAND, so a crash mid-tick
	// re-reads the same records rather than skipping them — a repeated
	// wake is a duplicate the inbox collapses, and a skipped one is
	// somebody never told.
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
// [Applier.stampStatusEntered] is gated on the same predicate. The kind is ONE
// word a writer chose for a patch that may have moved several things, so a
// change that moved the status and something else is filed under the something
// else and was invisible here.
//
// The reachable case is a BULK CANCEL — `StatusCancelled` is in the `done`
// group, whose own description names this path — so the apply stamps each task
// finished and clears every edge naming it as a blocker, and its dependents
// become workable. Such a record is quiet by design, and nothing outside
// [Writer.TellUnblocked] ever fills `Snapshot.Unblocked`, so this scan is the
// ONLY path by which those people hear. Filed under whatever else the patch
// moved, the join never reached them — and the horizon below, computed from
// the same predicate, then advanced past the record on the next status row
// anywhere in the log, so it was never reconsidered. "A gap of any length is
// caught up on the next tick" does not hold for a row the predicate cannot
// name.
//
// A dependent with any open edge is not ready. A dependent already told about
// a later clearing is not owed anything. Both are the difference between a
// repair and a source of duplicate wakes.
//
// # THE BOUND AND THE HORIZON ARE ONE DECISION
//
// The rows are cut at `limit` because each one becomes a durable commit every
// node applies, and the horizon is where the NEXT tick starts — so a horizon
// computed without reference to that cut does not make a wake late, it loses
// it. ONE record can clear more dependents than a tick may carry: a bulk
// cancel finishes up to [MaxBulkTasks] tasks and the apply clears every edge
// naming each of them, so a single row in this window can make hundreds of
// people workable. A MAX over the window taken with no reference to the bound
// then advanced the position past that very record, and everybody past the
// limit stayed owed FOR EVER — below the new horizon, so the join never
// reaches them again, and nothing outside [Writer.TellUnblocked] ever fills
// `Snapshot.Unblocked`, so this scan is the only path by which they hear.
//
// So the cut is EVIDENCE-BACKED rather than blind, in the idiom
// `internal/pages` reads its listings with: the query asks for one row past
// the bound, the extra row is dropped rather than carried, and its presence is
// [UnblockScan.Truncated]. A truncated scan returns the position it was GIVEN,
// so the next tick re-reads the same window — where the dependents already
// told have left the predicate (their `unblocked_told_at` now stands at or
// past their newest clearing) and the ones left behind come up in their place.
//
// WHAT MAKES THAT TERMINATE IS THE TOLD-STAMP, NOT THE POSITION, which is the
// sentence the duty already writes at its own checkpoint: the position is an
// optimisation over how much history one tick reads, never the thing that
// makes the repair sound. The window drains at `limit` a tick — the duty
// passes [WalkBatch], and what that number means as a fleet-wide throughput,
// how long it takes to drain a backlog, and what else moves with it are stated
// ONCE, at its definition, rather than spelled a second time here where the
// two copies would drift. That is the pacing this design already chose for
// every other walk, and it is the right way round here: a late notice is what
// this whole file is, and a lost one is what it exists to prevent.
//
// The alternative is PAGING UNTIL DRAINED inside one tick, and it is wrong for
// the reason the bound exists at all: the number of records the tick published
// would be set by the size of the backlog rather than by anything this design
// picked, landing on every node's applier at once, for a repair that is by
// definition already late.
func ScanUnblocked(ctx context.Context, db *store.DB, since uint64,
	limit int) (UnblockScan, error) {

	if limit < 1 {
		// REFUSED, because there is no honest fallback. The query keeps
		// at most `limit` rows and reads one past it as evidence, so
		// below one it carries nobody and reports itself truncated on
		// every tick: a repair that tells nobody, advances nowhere, and
		// returns success while doing it. A default chosen here would be
		// this file guessing at its caller's pacing.
		return UnblockScan{}, fmt.Errorf("tracker: the unblocked repair was "+
			"given limit %d, and a scan that may carry no dependent tells "+
			"nobody and never advances its position: pass a positive limit "+
			"(the duty passes WalkBatch, %d)", limit, WalkBatch)
	}

	scan := UnblockScan{Through: since}
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE POSITION FIRST, and from the same transaction as the rows:
		// a `through` read afterwards would cover records this scan did
		// not see, and advancing past them is how a wake is lost.
		// THE HORIZON IS OVER THE SAME PREDICATE AS THE ROWS, which is
		// what makes advancing it safe: a horizon computed over a WIDER
		// set steps past records the scan below never looked at.
		//
		// INTO A LOCAL, never straight onto the answer. This is the
		// window's CEILING, which the rows below are read against, and
		// it becomes the REPORTED position only if they accounted for
		// all of it. The two were ONE VARIABLE, and that is exactly how
		// a cut set of rows came to advance a whole window's horizon.
		var horizon uint64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(log_seq), ?) FROM tracker_history
			WHERE log_seq > ?
			  AND json_extract(fields_json, '$.status.to') IS NOT NULL`,
			since, since).Scan(&horizon); err != nil {
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
			-- ONE ROW PAST THE BOUND: the extra row is evidence
			-- that the window holds more, never an answer. See
			-- [UnblockScan.Truncated].
			ORDER BY t.id LIMIT ?`,
			since, horizon, limit+1)
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
		if err := rows.Err(); err != nil {
			return err
		}
		// THE PROBE ROW IS EVIDENCE, never an answer: the tick stays at
		// its bound and the horizon stays where it started, so the
		// dependents it did not carry are still inside the next tick's
		// window rather than below it.
		scan.Truncated = len(scan.Pending) > limit
		if scan.Truncated {
			scan.Pending = scan.Pending[:limit]
			return nil
		}
		// AND ONLY A DRAINED WINDOW MOVES THE POSITION: every row the
		// horizon covers has been accounted for — carried, or filtered
		// as not owed — so there is nothing left below it to find.
		scan.Through = horizon
		return nil
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
