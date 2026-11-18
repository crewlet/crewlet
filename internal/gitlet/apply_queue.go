package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The queue's applies: an enqueue's, a dequeue's, and the end of a change's
// live entry that every other record leaving the change no longer what was
// enqueued writes in its own apply ([endLiveEntryOf], [endEntries]). Each is
// pure in the record, its position and its instant, and every guard is a skip
// (apply.go's head, rule 3): an enqueue finding its change not as its decide
// held or its entry made already, or a dequeue finding its entry ended,
// applies as nothing, logged (code_queue_not_applied); only a redelivered
// record the ledger would have collapsed does.
//
// Every record moves the repository's version, so the version guard on its
// subject skips it when it is met again.

// applyEnqueue puts the change e names in its target's queue: its entry's row,
// waiting, its place the record's position, keeping whom the enqueue was made
// for ([keptCarrier]), which its landing's push rows read off the row
// ([Land.pushes]).
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
	// A person's credential, as every write's record names it
	// (iam.ActorFor); a seat acts as itself with none.
	var operator string
	if kind == PartyPerson {
		operator = e.Principal.OperatorID
	}
	madeFor, err := carrierColumn(keptCarrier(at, e.WorkingFor))
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, enqueueSQL, e.RepoID, e.EntryID, e.TargetRef, e.ChangeID, at.packed, e.HeadOID,
		string(e.Method), e.DeleteSource, e.CloseTask, by, string(kind), operator,
		string(QueueWaiting), at.at(), madeFor)
	if err != nil {
		return 0, fmt.Errorf("gitlet: enqueue change %s at %s: %w", e.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return queueNotApplied(ctx, at, e.RepoID, e.EntryID, "an entry already holds its id")
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, e.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", e.RepoID, at.position, err)
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
		return 0, fmt.Errorf("gitlet: dequeue entry %s at %s: %w", d.EntryID, at.position, err)
	}
	rows := 2
	// An entry a train tests takes the train with it: every candidate from
	// its own on merged it, so none would land now, and a train is abandoned
	// whole (apply_queuerecords.go's head).
	if entry.State == QueueTesting {
		abandoned, err := abandonTrainOn(ctx, tx, d.RepoID, entry.TargetRef)
		if err != nil {
			return 0, fmt.Errorf("gitlet: abandon %s's train at %s: %w", entry.TargetRef, at.position, err)
		}
		rows += abandoned
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, d.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", d.RepoID, at.position, err)
	}
	return rows, nil
}

// endLiveEntryOf ends changeID's live entry, if any, with state and reason at
// the record at's position, as a close, a draft, a retarget and a landing of
// the change do. It answers the rows it wrote: one, or none.
func endLiveEntryOf(ctx context.Context, tx *sql.Tx, at applyContext, changeID string, state QueueState,
	reason QueueEndReason) (int, error) {

	res, err := tx.ExecContext(ctx, endChangeEntrySQL, string(state), at.at(), at.packed, string(reason), changeID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: end change %s's queue entry at %s: %w", changeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// endEntries ends the live entry of every change a push record moved, with
// state and reason: one statement a JSON chunk ([inChunks]), a seek of
// code_queue_change_idx a change.
func endEntries(ctx context.Context, tx store.Execer, packed, instant int64, changes []string,
	state QueueState, reason QueueEndReason) error {

	return inChunks(changes, func(part []string) error {
		list, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, endChangesEntriesSQL, string(state), instant, packed, string(reason),
			string(list)); err != nil {
			return fmt.Errorf("gitlet: end %d changes' queue entries (%s): %w", len(part), reason, err)
		}
		return nil
	})
}

// purgeQueues deletes up to budget of repoID's merge queues' rows for a purge
// part ([Applier.applyPurgePart]): its entries, candidates, landings, green
// rows and tip work, each table's in its key's order so every node deletes
// the same rows. It answers how many it deleted.
func purgeQueues(ctx context.Context, tx *sql.Tx, repoID string, budget int) (int, error) {
	deleted := 0
	for _, stmt := range []string{purgeQueueSQL, purgeCandidatesSQL, purgeLandingsSQL, purgeGreenSQL,
		purgeTipsDueSQL} {
		if budget <= 0 {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, repoID, budget)
		if err != nil {
			return 0, fmt.Errorf("gitlet: purge %s's merge queues: %w", repoID, err)
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

// The queue's writes, each that finds its rows registered in plan_test.go. The
// live states are spelled as code_queue_live_idx's partial predicate is.
const (
	enqueueSQL = `INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
			delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, enqueued_at,
			working_for)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, entry_id) DO NOTHING`
	endEntrySQL = `UPDATE code_queue INDEXED BY ` + queueKey + ` SET state = ?, ended_at = ?, ended_position = ?,
		end_reason = ? WHERE repo_id = ? AND entry_id = ?`
	endChangeEntrySQL = `UPDATE code_queue SET state = ?, ended_at = ?, ended_position = ?, end_reason = ?
		WHERE rowid IN (SELECT rowid FROM code_queue INDEXED BY code_queue_change_idx
			WHERE change_id = ? AND state IN ('waiting', 'testing'))`
	endChangesEntriesSQL = `UPDATE code_queue SET state = ?, ended_at = ?, ended_position = ?, end_reason = ?
		WHERE rowid IN (SELECT q.rowid FROM json_each(?) AS j
			CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
			WHERE q.change_id = CAST(j.value AS TEXT) AND q.state IN ('waiting', 'testing'))`

	// A purge part's bounded deletes, in key order; then the final purge's.
	purgeQueueSQL = `DELETE FROM code_queue WHERE rowid IN (SELECT rowid FROM code_queue INDEXED BY ` + queueKey + `
		WHERE repo_id = ? ORDER BY entry_id LIMIT ?)`
	purgeCandidatesSQL = `DELETE FROM code_candidates WHERE rowid IN (SELECT rowid FROM code_candidates
		WHERE repo_id = ? ORDER BY candidate_id LIMIT ?)`
	purgeLandingsSQL = `DELETE FROM code_landings WHERE rowid IN (SELECT rowid FROM code_landings
		WHERE repo_id = ? ORDER BY target_ref, position, ordinal LIMIT ?)`
	purgeGreenSQL = `DELETE FROM code_green WHERE rowid IN (SELECT rowid FROM code_green
		WHERE repo_id = ? ORDER BY target_ref LIMIT ?)`
	purgeTipsDueSQL = `DELETE FROM code_tips_due WHERE rowid IN (SELECT rowid FROM code_tips_due
		WHERE repo_id = ? ORDER BY target_ref LIMIT ?)`
	purgeAllQueueSQL      = `DELETE FROM code_queue WHERE repo_id = ?`
	purgeAllCandidatesSQL = `DELETE FROM code_candidates WHERE repo_id = ?`
	purgeAllLandingsSQL   = `DELETE FROM code_landings WHERE repo_id = ?`
	purgeAllGreenSQL      = `DELETE FROM code_green WHERE repo_id = ?`
	purgeAllTipsDueSQL    = `DELETE FROM code_tips_due WHERE repo_id = ?`
)
