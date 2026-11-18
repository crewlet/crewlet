package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// The lifecycle's applies ([Applier.applyChangeOpen], [Applier.applyChangeWrite])
// are pure in the record, its position and its instant: no clock, no config,
// nothing decided again. Arbitration on the repository's subject means a record
// applies against the rows its decide read, so every guard is a SKIP, never a
// refusal that would stop the log on every node: a record finding its change
// not in the state its decide held applies as nothing, logged
// (code_change_not_applied); only a record the ledger would have collapsed,
// met again, does. An open numbers its change here, from next_change advanced
// in the same apply, in the log's order: two decides could take the same one.
// Every write counts one version (1 at the open, which an edit names as its
// expectation) and moves the repository's, so the version guard skips it when
// it is met again.

// applyChangeOpen opens the change o names: its row, numbered next_change and
// advancing it; its body, where it has one; its owner as its first author; and
// its first revision, the head its decide read off the source branch, keeping
// whom the open was made for ([keptCarrier]).
func (a *Applier) applyChangeOpen(ctx context.Context, tx *sql.Tx, at applyContext, o ChangeOpen) (int, error) {
	owner, kind := o.Owner.party()
	if owner == "" {
		return changeNotApplied(ctx, at, o.RepoID, o.ChangeID, "its owner names no seat, and only a seat or a "+
			"person opens a change")
	}
	var next int64
	if err := tx.QueryRowContext(ctx, nextChangeSQL, o.RepoID).Scan(&next); err != nil {
		return 0, fmt.Errorf("gitlet: read %s's next change at %s: %w", o.RepoID, at.position, err)
	}
	state := ChangeStateOpen
	if o.Draft {
		state = ChangeStateDraft
	}
	var work types.WorkItem
	if o.WorkItem != nil {
		work = *o.WorkItem
	}
	// A person's credential attributes the change, as every record names it
	// (iam.ActorFor); a seat acts as itself with none.
	var operator string
	if kind == PartyPerson {
		operator = o.Owner.OperatorID
	}
	res, err := tx.ExecContext(ctx, openChangeSQL, o.ChangeID, o.RepoID, next, o.Title, len(o.Body),
		string(state), o.SourceRef, o.TargetRef, o.Head, owner, string(kind), operator, string(work.Backend),
		work.ID, work.Key, work.Project, o.TurnID, o.RunID, at.at(), at.packed, at.packed, at.at())
	if err != nil {
		return 0, fmt.Errorf("gitlet: open change %s at %s: %w", o.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return changeNotApplied(ctx, at, o.RepoID, o.ChangeID, "a change already holds its id")
	}
	rows := 1
	if o.Body != "" {
		if _, err := tx.ExecContext(ctx, setBodySQL, o.ChangeID, o.Body); err != nil {
			return 0, fmt.Errorf("gitlet: write change %s's body at %s: %w", o.ChangeID, at.position, err)
		}
		rows++
	}
	if _, err := tx.ExecContext(ctx, addAuthorSQL, o.ChangeID, owner, string(kind), 1, at.packed); err != nil {
		return 0, fmt.Errorf("gitlet: write change %s's owner as its author at %s: %w", o.ChangeID, at.position, err)
	}
	madeFor, columnErr := carrierColumn(keptCarrier(at, o.WorkingFor))
	if columnErr != nil {
		return 0, columnErr
	}
	if _, err := tx.ExecContext(ctx, addRevisionSQL, o.ChangeID, 1, o.Head, owner, string(kind), "", at.packed,
		at.at(), madeFor); err != nil {
		return 0, fmt.Errorf("gitlet: write change %s's first revision at %s: %w", o.ChangeID, at.position, err)
	}
	if _, err := tx.ExecContext(ctx, openedSQL, at.packed, o.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: advance %s's next change at %s: %w", o.RepoID, at.position, err)
	}
	var opened ChangeCounts
	opened.add(state, 1)
	tallied, countErr := countChanges(ctx, tx, o.RepoID, opened)
	if countErr != nil {
		return 0, fmt.Errorf("%w (the open at %s)", countErr, at.position)
	}
	return rows + 3 + tallied, nil
}

// applyChangeWrite applies a write of a change beyond its open (an edit, a
// draft's state, a retarget, a close, a reopen or a landing): the row read by
// its key, moved, and written back one version on by the [changeWrite] its op
// makes ([lifecycleWrite]); then an edit's body, a reopen's revision where the
// head moved while closed, and the repository's version. A close, a draft, a
// retarget and a landing end the live queue entry ([leavesQueue],
// [endLiveEntryOf]), or the queue would land a change no longer what was
// enqueued. A landing merges via `push`, and applies as nothing unless the
// change is at the head its home found. An ended change's stored readiness is
// cleared ([clearReadiness]): none of its home's records refreshes it again.
func (a *Applier) applyChangeWrite(ctx context.Context, tx *sql.Tx, at applyContext, payload any) (int, error) {
	repoID, changeID, write, err := lifecycleWrite(payload)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the change record at %s: %w", at.position, err)
	}
	row, found, err := changeRowIn(ctx, tx, changeID)
	if err != nil {
		return 0, err
	}
	if !found || row.repoID != repoID {
		return changeNotApplied(ctx, at, repoID, changeID, "the repository holds no such change")
	}
	next := row
	var (
		body     *string
		revision *ChangeReopen
	)
	switch p := payload.(type) {
	case ChangeEdit:
		if !row.state.Live() {
			return changeNotApplied(ctx, at, repoID, changeID, "an edit takes a live change, and it is "+string(row.state))
		}
		if p.Title != nil {
			next.title = *p.Title
		}
		if p.Body != nil {
			// Measured through body, never p: internal/sourcetree/decltype
			// cannot type a type switch's variable, and the cache's writers
			// gate (TestNothingButSyncWritesACache) refuses one handed to any
			// call, len included, since it cannot tell it from a gitproc.Cmd.
			body = p.Body
			next.bodyBytes = int64(len(*body))
		}
		switch {
		case p.WorkItem != nil:
			next.work = *p.WorkItem
		case p.ClearWorkItem:
			next.work = types.WorkItem{}
		}
	case ChangeDraft:
		if !row.state.Live() {
			return changeNotApplied(ctx, at, repoID, changeID, "a draft's state takes a live change, and it is "+
				string(row.state))
		}
		next.state = ChangeStateOpen
		if p.Draft {
			next.state = ChangeStateDraft
		}
	case ChangeRetarget:
		if !row.state.Live() {
			return changeNotApplied(ctx, at, repoID, changeID, "a retarget takes a live change, and it is "+
				string(row.state))
		}
		next.target = p.TargetRef
	case ChangeClose:
		if !row.state.Live() {
			return changeNotApplied(ctx, at, repoID, changeID, "a close takes a live change, and it is "+
				string(row.state))
		}
		next.state, next.closeReason = ChangeStateClosed, p.Reason
		next.closedAt = sql.NullInt64{Int64: at.at(), Valid: true}
		next.closedBy, next.closedByKind = p.By.party()
	case ChangeReopen:
		if row.state != ChangeStateClosed {
			return changeNotApplied(ctx, at, repoID, changeID, "a reopen takes a closed change, and it is "+
				string(row.state))
		}
		if by, _ := p.By.party(); by == "" {
			return changeNotApplied(ctx, at, repoID, changeID, "who reopened it names no seat")
		}
		next.state, next.closeReason, next.closedAt = ChangeStateOpen, "", sql.NullInt64{}
		next.closedBy, next.closedByKind = "", ""
		// The source moved while closed: its head now is a revision of its
		// own, made by the reopen.
		if p.Head != row.head {
			next.head, next.revision, revision = p.Head, row.revision+1, &p
		}
	case ChangeLanded:
		if !row.state.Live() || row.revision != p.Revision || row.head != p.HeadOID || row.target != p.TargetRef {
			return changeNotApplied(ctx, at, repoID, changeID, "a landing takes a live change at the head and the "+
				"target its home found, and it is "+string(row.state)+" at revision "+
				strconv.FormatInt(row.revision, 10)+" into "+row.target)
		}
		next.state, next.mergedVia, next.mergeOID = ChangeStateMerged, MergedViaPush, p.TargetOID
		next.mergedAt = sql.NullInt64{Int64: at.at(), Valid: true}
	}
	next.version, next.position, next.updatedAt = row.version+1, at.packed, at.at()
	if err := writeChanges(ctx, tx, at.maxVariables, write, []*changeRow{&next}); err != nil {
		return 0, fmt.Errorf("gitlet: write change %s at %s: %w", changeID, at.position, err)
	}
	rows := 1
	// WHAT THE RECORD DID, as the change's timeline reads it (changemoves.go).
	if moves := lifecycleMoves(payload, row, next); len(moves) > 0 {
		if err := writeMoves(ctx, tx, at.maxVariables, at.packed, at.at(), moves); err != nil {
			return 0, fmt.Errorf("%w (the record at %s)", err, at.position)
		}
		rows += len(moves)
	}
	if body != nil {
		stmt, args := setBodySQL, []any{changeID, *body}
		if *body == "" {
			stmt, args = deleteBodySQL, []any{changeID}
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return 0, fmt.Errorf("gitlet: write change %s's body at %s: %w", changeID, at.position, err)
		}
		rows++
	}
	if state, reason := leavesQueue(payload); reason != "" {
		ended, err := endLiveEntryOf(ctx, tx, at, changeID, state, reason)
		if err != nil {
			return 0, err
		}
		rows += ended
	}
	if !next.state.Live() {
		cleared, err := clearReadiness(ctx, tx, []string{changeID})
		if err != nil {
			return 0, fmt.Errorf("%w (the record at %s)", err, at.position)
		}
		rows += cleared
	}
	if revision != nil {
		by, kind := revision.By.party()
		madeFor, columnErr := carrierColumn(keptCarrier(at, revision.WorkingFor))
		if columnErr != nil {
			return 0, columnErr
		}
		if _, err := tx.ExecContext(ctx, addRevisionSQL, changeID, next.revision, next.head, by, string(kind), "",
			at.packed, at.at(), madeFor); err != nil {
			return 0, fmt.Errorf("gitlet: write change %s's revision %d at %s: %w", changeID, next.revision,
				at.position, err)
		}
		rows++
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, repoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", repoID, at.position, err)
	}
	var moved ChangeCounts
	moved.move(row.state, next.state)
	tallied, countErr := countChanges(ctx, tx, repoID, moved)
	if countErr != nil {
		return 0, fmt.Errorf("%w (the record at %s)", countErr, at.position)
	}
	return rows + 1 + tallied, nil
}

// leavesQueue is how a lifecycle write ends the change's live queue entry: a
// close, a move to draft (never out of one), a retarget and a landing dequeue
// it; "" for a write that leaves the change what was enqueued.
func leavesQueue(payload any) (QueueState, QueueEndReason) {
	switch p := payload.(type) {
	case ChangeClose:
		return QueueDequeued, EndClosed
	case ChangeDraft:
		if p.Draft {
			return QueueDequeued, EndDrafted
		}
	case ChangeRetarget:
		return QueueDequeued, EndRetargeted
	case ChangeLanded:
		return QueueDequeued, EndMerged
	}
	return "", ""
}

// lifecycleWrite is the repository and change a lifecycle write names, and the
// [changeWrite] it makes of the row: an edit's touches the work item only where
// it sets or clears one.
func lifecycleWrite(payload any) (repoID, changeID string, write changeWrite, err error) {
	switch p := payload.(type) {
	case ChangeEdit:
		write = changeEdited
		if p.WorkItem != nil || p.ClearWorkItem {
			write = changeItemEdited
		}
		return p.RepoID, p.ChangeID, write, nil
	case ChangeDraft:
		return p.RepoID, p.ChangeID, changeStateMoved, nil
	case ChangeRetarget:
		return p.RepoID, p.ChangeID, changeRetargeted, nil
	case ChangeClose:
		return p.RepoID, p.ChangeID, changeStateMoved, nil
	case ChangeReopen:
		return p.RepoID, p.ChangeID, changeStateMoved, nil
	case ChangeLanded:
		return p.RepoID, p.ChangeID, changeMerged, nil
	}
	return "", "", "", fmt.Errorf("it carries a %T, which is no write of a change's lifecycle", payload)
}

// pushWrite is the [changeWrite] a push record makes of a change it reached:
// an insert of one it opened, whatever later entries did, since the row is
// new; else a merge's, a close's, or the head alone, the one move that leaves
// a change live.
func pushWrite(row *changeRow, opened bool) changeWrite {
	switch {
	case opened:
		return changeOpened
	case row.state == ChangeStateMerged:
		return changeMerged
	case row.state == ChangeStateClosed:
		return changeStateMoved
	}
	return changeHeadMoved
}

// writeChanges writes rows by w in as few statements as the parameters allow
// ([store.InsertRows]). VALUES carry the whole row, since even a row an upsert
// finds by its key must bind every NOT NULL column; what lands is what w sets.
func writeChanges(ctx context.Context, tx store.Execer, maxVariables int, w changeWrite,
	rows []*changeRow) error {

	_, err := store.InsertRows(ctx, tx, maxVariables, insertChangesSQL, changeValuesSQL, string(w), len(rows),
		func(i int) []any {
			r := rows[i]
			return []any{r.id, r.repoID, r.number, r.title, r.bodyBytes, string(r.state), r.source, r.target,
				r.head, r.revision, r.owner, string(r.ownerKind), r.operator, string(r.work.Backend), r.work.ID,
				r.work.Key, r.work.Project, r.turn, r.run, r.openedAt, r.opened, r.closedAt, r.closeReason,
				r.closedBy, string(r.closedByKind), r.mergedAt, string(r.mergedVia), r.mergeOID, r.mergedBy,
				string(r.mergedByKind), r.mergedOperator, r.overrideReason, r.version, r.position, r.updatedAt}
		})
	return err
}

// writeMoves writes moves at the record's position and instant, in as few statements as the
// parameters allow; a key already held is a skip, never an abort that would stall the log.
func writeMoves(ctx context.Context, tx store.Execer, maxVariables int, packed, at int64, moves []moveRow) error {
	_, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_change_moves (change_id, position, kind, by_handle, by_kind, reason, was, became, at)
			VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, position, kind) DO NOTHING`, len(moves),
		func(i int) []any {
			m := moves[i]
			return []any{m.change, packed, string(m.kind), m.by, string(m.byKind), m.reason, m.was, m.became, at}
		})
	if err != nil {
		return fmt.Errorf("gitlet: write the changes' moves: %w", err)
	}
	return nil
}

// write writes what a push record's [changeBook] made of the changes it
// reached (rows by [pushWrite], revisions, new authors, opened changes' bodies,
// ended changes' queue entries and readiness, the next number) and answers
// [changeBook.rowCount]. An insert's key conflict is a skip, never an abort
// that would stall the log on every node; none occurs, since the book adds only
// what its rows lack and an opened change's id is new ([PushChangeID]). Every
// table goes in as few statements as the parameters allow ([store.InsertRows]),
// a moved change as an upsert's conflict: one record can move ~1 300 changes
// on the estate's one write lane, and an UPDATE a change held it past
// statelog.ApplyTxTimeBudget (doc.go's measured section). Each write sets only
// what the record moved ([changeWrite]), so a head move, nearly every change
// reached, rewrites only code_changes_target_idx beside what every write does.
func (b *changeBook) write(ctx context.Context, tx store.Execer, maxVariables int) (int, error) {
	w := &b.writes
	byWrite := map[changeWrite][]*changeRow{}
	for _, id := range w.touched {
		kind := pushWrite(b.rows[id], w.opened[id])
		byWrite[kind] = append(byWrite[kind], b.rows[id])
	}
	for _, part := range []struct {
		write changeWrite
		what  string
	}{
		{changeOpened, "open the changes"},
		{changeHeadMoved, "move the changes' heads"},
		{changeMerged, "merge the changes"},
		{changeStateMoved, "close the changes"},
	} {
		if err := writeChanges(ctx, tx, maxVariables, part.write, byWrite[part.write]); err != nil {
			return 0, fmt.Errorf("gitlet: %s: %w", part.what, err)
		}
	}
	// WHOM EACH REVISION'S PUSH WAS MADE FOR, as its entry carries it.
	madeFor := make([]sql.NullString, len(w.revisions))
	for i, r := range w.revisions {
		var err error
		if madeFor[i], err = carrierColumn(r.workingFor); err != nil {
			return 0, err
		}
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at,
			working_for, via, forced) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, n) DO NOTHING`, len(w.revisions),
		func(i int) []any {
			r := w.revisions[i]
			return []any{r.change, r.n, r.head, r.pusher, string(r.kind), r.pushID, b.packed, b.at, madeFor[i],
				string(r.via), r.forced}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the revisions: %w", err)
	}
	if err := writeMoves(ctx, tx, maxVariables, b.packed, b.at, w.moves); err != nil {
		return 0, err
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position) VALUES `,
		`(?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, author) DO NOTHING`, len(w.authors),
		func(i int) []any {
			a := w.authors[i]
			return []any{a.change, a.author, string(a.kind), a.first, b.packed}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the authors: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables, `INSERT INTO code_change_bodies (change_id, body) VALUES `,
		`(?, ?)`, `ON CONFLICT (change_id) DO NOTHING`, len(w.bodies), func(i int) []any {
			return []any{w.bodies[i].change, w.bodies[i].body}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write the descriptions of the changes opened: %w", err)
	}
	// A head move ejects a live entry, a merge or a close dequeues it: one
	// statement a chunk, a seek of code_queue_change_idx a change.
	ejected, merged, closed := b.queueEnds()
	for _, end := range []struct {
		ids    []string
		state  QueueState
		reason QueueEndReason
	}{
		{ejected, QueueEjected, EjectHeadMoved}, {merged, QueueDequeued, EndMerged},
		{closed, QueueDequeued, EndClosed},
	} {
		if len(end.ids) == 0 {
			continue
		}
		if err := endEntries(ctx, tx, b.packed, b.at, end.ids, end.state, end.reason); err != nil {
			return 0, err
		}
	}
	// One statement for every ended change; the book counts a row each.
	if _, err := clearReadiness(ctx, tx, b.ended()); err != nil {
		return 0, err
	}
	// The repository's row is written anyway for its counters, so the next
	// number costs the record no row of its budget.
	if len(byWrite[changeOpened]) > 0 {
		if _, err := tx.ExecContext(ctx, nextChangeAfterSQL, b.next, b.repo); err != nil {
			return 0, fmt.Errorf("gitlet: move the repository's next change on: %w", err)
		}
	}
	return b.rowCount(), nil
}

// changeNotApplied logs a change record that applies as nothing because its
// change is not in the state its decide held, and answers that it wrote no
// row.
func changeNotApplied(ctx context.Context, at applyContext, repoID, changeID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_change_not_applied", "repo", repoID, "change", changeID,
		"op", string(at.record.Op), "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// changeWrite is how a record writes a change's row: the conflict clause of
// [writeChanges]'s statement, whose VALUES are the whole row — an insert that
// skips a conflict on its key, or an upsert that SETS WHAT ONE KIND OF WRITE
// MOVES AND NO OTHER COLUMN. The pinned driver rewrites a row's entry in every
// index whose key or partial predicate holds a column the write NAMES, moved
// or not, and the head move (every push to a source, ~1 300 changes a record)
// is in code_changes_target_idx alone; so two kinds share a write only where
// they rewrite the same indexes. Every write also moves version, position and
// updated_at (code_changes_updated_idx, and a live change's two last-move
// entries: replicated 0147, 0173), so a live head move rewrites five.
// TestAChangeWriteRewritesOnlyTheIndexesItMoves holds each kind to its
// indexes: an index on a column a write sets is a decision it forces.
type changeWrite string

// The kinds of write of a change's row, each the conflict clause of its
// statement ([changeWrite]).
const (
	changeOpened    changeWrite = `ON CONFLICT (id) DO NOTHING`
	changeHeadMoved changeWrite = `ON CONFLICT (id) DO UPDATE SET head_oid = excluded.head_oid,
		revision = excluded.revision, version = excluded.version,
		position = excluded.position, updated_at = excluded.updated_at`
	changeStateMoved changeWrite = `ON CONFLICT (id) DO UPDATE SET state = excluded.state,
		closed_at = excluded.closed_at, close_reason = excluded.close_reason, closed_by = excluded.closed_by,
		closed_by_kind = excluded.closed_by_kind, head_oid = excluded.head_oid,
		revision = excluded.revision, version = excluded.version,
		position = excluded.position, updated_at = excluded.updated_at`
	changeMerged changeWrite = `ON CONFLICT (id) DO UPDATE SET state = excluded.state,
		merged_at = excluded.merged_at, merged_via = excluded.merged_via, merge_oid = excluded.merge_oid,
		merged_by = excluded.merged_by, merged_by_kind = excluded.merged_by_kind,
		merged_operator_id = excluded.merged_operator_id, override_reason = excluded.override_reason,
		head_oid = excluded.head_oid, revision = excluded.revision, version = excluded.version,
		position = excluded.position, updated_at = excluded.updated_at`
	changeEdited changeWrite = `ON CONFLICT (id) DO UPDATE SET title = excluded.title,
		body_bytes = excluded.body_bytes, version = excluded.version,
		position = excluded.position, updated_at = excluded.updated_at`
	changeItemEdited changeWrite = `ON CONFLICT (id) DO UPDATE SET title = excluded.title,
		body_bytes = excluded.body_bytes, work_backend = excluded.work_backend, work_id = excluded.work_id,
		work_key = excluded.work_key, work_project = excluded.work_project, version = excluded.version,
		position = excluded.position, updated_at = excluded.updated_at`
	changeRetargeted changeWrite = `ON CONFLICT (id) DO UPDATE SET target_ref = excluded.target_ref,
		version = excluded.version, position = excluded.position, updated_at = excluded.updated_at`
)

// The lifecycle's writes beside a change's row. A statement that reads a row
// to find the ones it writes is registered in plan_test.go; an insert or an
// upsert locates none, its conflict found by the key its VALUES carry — a skip
// or the write, never an abort that would stall the log on every node.
const (
	nextChangeSQL = `SELECT next_change FROM code_repos WHERE id = ?`
	openChangeSQL = `INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, owner_operator_id, work_backend, work_id, work_key, work_project,
			created_turn, created_run, opened_at, opened_position, version, position, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (id) DO NOTHING`
	openedSQL  = `UPDATE code_repos SET next_change = next_change + 1, version = ? WHERE id = ?`
	touchedSQL = `UPDATE code_repos SET version = ? WHERE id = ?`

	// nextChangeAfterSQL sets the repository's next number past the changes a
	// push record opened: its version is the record's counters' to move.
	nextChangeAfterSQL = `UPDATE code_repos SET next_change = ? WHERE id = ?`

	setBodySQL = `INSERT INTO code_change_bodies (change_id, body) VALUES (?, ?)
		ON CONFLICT (change_id) DO UPDATE SET body = excluded.body`
	deleteBodySQL = `DELETE FROM code_change_bodies WHERE change_id = ?`
	addAuthorSQL  = `INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (change_id, author) DO NOTHING`
	addRevisionSQL = `INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at,
			working_for)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (change_id, n) DO NOTHING`

	// insertChangesSQL and changeValuesSQL are [writeChanges]' statement, a
	// chunk each: every column [changeColumns] names, in its order.
	insertChangesSQL = `INSERT INTO code_changes (` + changeColumns + `) VALUES `
	changeValuesSQL  = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?)`
)
