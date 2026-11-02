package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE QUEUE'S APPLIES: an enqueue's ([Applier.applyEnqueue]), a dequeue's
// ([Applier.applyDequeue]), and the ends of a change's live entry every other
// record of the repository's that leaves the change no longer what was
// enqueued writes in its own apply ([endLiveEntryOf], [endEntries]) — each in
// the one transaction the framework holds a record's apply to, PURE IN THE
// RECORD, ITS POSITION AND ITS INSTANT. The decide held the change live at its
// head and target and its queue holding no live entry of it; the broker's
// arbitration on the repository's subject guarantees the record applies
// against the rows that decide read. So every guard here is a SKIP, never a
// refusal, which would stop the log on every node: an enqueue that finds its
// change not as its decide held, or its entry made already, and a dequeue that
// finds its entry ended, apply as NOTHING, whole, logged
// (code_queue_not_applied) — no correct log carries one but a record the ledger
// would have collapsed, met again.
//
// EVERY RECORD MOVES THE REPOSITORY'S VERSION, so the version guard every
// record on its subject meets skips it when it is met again.

// applyEnqueue puts the change e names in its target's queue: its entry's row,
// waiting, its place the record's position.
func (a *Applier) applyEnqueue(ctx context.Context, tx *sql.Tx, at applyContext, e Enqueue) (int, error) {
	row, found, err := changeRowIn(ctx, tx, e.ChangeID)
	if err != nil {
		return 0, err
	}
	by, kind := e.Principal.party()
	switch {
	case !found || row.repoID != e.RepoID:
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "the repository holds no such change")
	case row.state != ChangeStateOpen || row.head != e.HeadOID || row.target != e.TargetRef:
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "an enqueue takes the open change at the head and "+
			"on the target its decide read, and it is "+string(row.state)+" at "+short(row.head)+" into "+row.target)
	case by == "":
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "its principal names no seat, and only a seat or a "+
			"person merges")
	}
	live, err := liveEntryOf(ctx, tx, e.ChangeID)
	if err != nil {
		return 0, err
	}
	if live != nil {
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "the change holds a live entry already, "+
			live.EntryID)
	}
	// THE CREDENTIAL A PERSON ENQUEUED IT WITH, as every record a write
	// leaves names it (iam.ActorFor); a seat acts as itself with none.
	var operator string
	if kind == PartyPerson {
		operator = e.Principal.OperatorID
	}
	res, err := tx.ExecContext(ctx, enqueueSQL, e.RepoID, e.EntryID, e.TargetRef, e.ChangeID, at.packed, e.HeadOID,
		string(e.Method), e.DeleteSource, e.CloseTask, by, string(kind), operator,
		string(QueueWaiting), at.at())
	if err != nil {
		return 0, fmt.Errorf("code: enqueue change %s at %s: %w", e.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "an entry already holds its id")
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, e.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", e.RepoID, at.position, err)
	}
	return 2, nil
}

// applyDequeue ends the live entry d names, dequeued at its principal's ask.
func (a *Applier) applyDequeue(ctx context.Context, tx *sql.Tx, at applyContext, d Dequeue) (int, error) {
	entry, found, err := entryIn(ctx, tx, d.RepoID, d.EntryID)
	if err != nil {
		return 0, err
	}
	switch {
	case !found:
		return queueNotApplied(ctx, at, d.RepoID, d.EntryID, "the repository's queues hold no such entry")
	case !entry.State.Live():
		return queueNotApplied(ctx, at, d.RepoID, d.EntryID, "a dequeue takes a live entry, and it is "+
			string(entry.State))
	}
	if _, err := tx.ExecContext(ctx, endEntrySQL, string(QueueDequeued), at.at(), at.packed, string(EndAsked),
		d.RepoID, d.EntryID); err != nil {
		return 0, fmt.Errorf("code: dequeue entry %s at %s: %w", d.EntryID, at.position, err)
	}
	rows := 2
	// AN ENTRY A TRAIN TESTS TAKES THE TRAIN WITH IT: every candidate from
	// its own on merged it, so none of them is what would land now, and a
	// train is abandoned whole (apply_queuerecords.go).
	if entry.State == QueueTesting {
		abandoned, err := abandonTrainOn(ctx, tx, d.RepoID, entry.TargetRef)
		if err != nil {
			return 0, fmt.Errorf("code: abandon %s's train at %s: %w", entry.TargetRef, at.position, err)
		}
		rows += abandoned
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, d.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", d.RepoID, at.position, err)
	}
	return rows, nil
}

// endLiveEntryOf ends changeID's live entry, if its queue holds one — state
// and reason what ended it — at the record at's position: what a close, a
// draft, a retarget and a landing of the change write in their own applies.
// It answers the rows it wrote: one, or none for a change no queue holds.
func endLiveEntryOf(ctx context.Context, tx *sql.Tx, at applyContext, changeID string, state QueueState,
	reason QueueEndReason) (int, error) {

	res, err := tx.ExecContext(ctx, endChangeEntrySQL, string(state), at.at(), at.packed, string(reason), changeID)
	if err != nil {
		return 0, fmt.Errorf("code: end change %s's queue entry at %s: %w", changeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// endEntries ends the live entry of every change of changes, each a change a
// push record moved, state and reason what moved it — one statement over the
// list, a seek of code_queue_change_idx a change, in as many statements as its
// JSON's chunks take ([inChunks]).
func endEntries(ctx context.Context, tx store.Execer, packed, instant int64, changes []string,
	state QueueState, reason QueueEndReason) error {

	return inChunks(changes, func(part []string) error {
		list, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, endChangesEntriesSQL, string(state), instant, packed, string(reason),
			string(list)); err != nil {
			return fmt.Errorf("code: end %d changes' queue entries (%s): %w", len(part), reason, err)
		}
		return nil
	})
}

// purgeQueues deletes up to budget of repoID's merge queues' rows — its
// entries, then its candidates, its landings and its green rows, each table's
// in its key's order, so every node deletes the same rows — for a purge part
// ([Applier.applyPurgePart]), and answers how many it deleted.
func purgeQueues(ctx context.Context, tx *sql.Tx, repoID string, budget int) (int, error) {
	deleted := 0
	for _, stmt := range []string{purgeQueueSQL, purgeCandidatesSQL, purgeLandingsSQL, purgeGreenSQL} {
		if budget <= 0 {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, repoID, budget)
		if err != nil {
			return 0, fmt.Errorf("code: purge %s's merge queues: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		budget -= int(n)
	}
	return deleted, nil
}

// queueNotApplied logs a queue record that applies as nothing because what it
// is about is not as its decide held, and answers that it wrote no row.
func queueNotApplied(ctx context.Context, at applyContext, repoID, entryID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_queue_not_applied", "repo", repoID, "entry", entryID,
		"op", string(at.record.Op), "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// The queue's writes, each that finds the rows it writes registered in
// plan_test.go: an entry inserted, its conflict on its key a skip; an entry
// ended by its key; a change's live entry ended, over code_queue_change_idx;
// and a list of changes' live entries ended, driven from the list, one seek of
// code_queue_change_idx a change. The live states are spelled as
// code_queue_live_idx is partial on them.
const (
	enqueueSQL = `INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
			delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, entry_id) DO NOTHING`
	endEntrySQL = `UPDATE code_queue SET state = ?, ended_at = ?, ended_position = ?, end_reason = ?
		WHERE repo_id = ? AND entry_id = ?`
	endChangeEntrySQL = `UPDATE code_queue SET state = ?, ended_at = ?, ended_position = ?, end_reason = ?
		WHERE rowid IN (SELECT rowid FROM code_queue INDEXED BY code_queue_change_idx
			WHERE change_id = ? AND state IN ('waiting', 'testing'))`
	endChangesEntriesSQL = `UPDATE code_queue SET state = ?, ended_at = ?, ended_position = ?, end_reason = ?
		WHERE rowid IN (SELECT q.rowid FROM json_each(?) AS j
			CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
			WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing'))`

	// A purge part's bounded deletes, each table's rows of the repository in
	// its key's order; and the final purge's, whole.
	purgeQueueSQL = `DELETE FROM code_queue WHERE rowid IN (SELECT rowid FROM code_queue
		WHERE repo_id = ? ORDER BY entry_id LIMIT ?)`
	purgeCandidatesSQL = `DELETE FROM code_candidates WHERE rowid IN (SELECT rowid FROM code_candidates
		WHERE repo_id = ? ORDER BY candidate_id LIMIT ?)`
	purgeLandingsSQL = `DELETE FROM code_landings WHERE rowid IN (SELECT rowid FROM code_landings
		WHERE repo_id = ? ORDER BY target_ref, position, ordinal LIMIT ?)`
	purgeGreenSQL = `DELETE FROM code_green WHERE rowid IN (SELECT rowid FROM code_green
		WHERE repo_id = ? ORDER BY target_ref LIMIT ?)`
	purgeAllQueueSQL      = `DELETE FROM code_queue WHERE repo_id = ?`
	purgeAllCandidatesSQL = `DELETE FROM code_candidates WHERE repo_id = ?`
	purgeAllLandingsSQL   = `DELETE FROM code_landings WHERE repo_id = ?`
	purgeAllGreenSQL      = `DELETE FROM code_green WHERE repo_id = ?`
)
