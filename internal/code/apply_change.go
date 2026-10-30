package code

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LIFECYCLE'S APPLIES: an open's ([Applier.applyChangeOpen]) and every
// other write of a change's ([Applier.applyChangeWrite]), each in the one
// transaction the framework holds a record's apply to, and each PURE IN THE
// RECORD, ITS POSITION AND ITS INSTANT, like every apply here: it reads no
// clock and no configuration and decides nothing again. The decide held the
// repository active, an open's branches live and heading no live change, and
// every other write's change in the state its rule takes; the broker's
// arbitration on the repository's subject guarantees the record applies
// against the rows that decide read. So every guard here is a SKIP, never a
// refusal, which would stop the log on every node: a record that finds its
// change not in the state its decide held — an open whose change id a change
// already holds, a write whose change is not the repository's, or one in a
// state its op does not take — applies as NOTHING, whole, logged
// (code_change_not_applied): no correct log carries one but a record the
// ledger would have collapsed, met again.
//
// AN OPEN NUMBERS ITS CHANGE HERE, from the repository's next_change, which
// the same apply advances: numbers follow the log's order, and no decide —
// which two committers may each take on their own snapshot — gives one.
//
// EVERY WRITE COUNTS ONE VERSION and moves the repository's own, so the
// version guard every record on its subject meets skips it when it is met
// again: the change's version counts the records that changed its row — 1 at
// its open, which an edit names as its expectation — and its position is the
// last one's.

// applyChangeOpen opens the change o names: its row, numbered next_change and
// advancing it; its body, where it has one; its owner as its first author; and
// its first revision, the head its decide read off the source branch.
func (a *Applier) applyChangeOpen(ctx context.Context, tx *sql.Tx, at applyContext, o ChangeOpen) (int, error) {
	owner, kind := o.Owner.party()
	if owner == "" {
		return changeNotApplied(ctx, at, o.RepoID, o.ChangeID, "its owner names no seat, and only a seat or a "+
			"person opens a change")
	}
	var next int64
	if err := tx.QueryRowContext(ctx, nextChangeSQL, o.RepoID).Scan(&next); err != nil {
		return 0, fmt.Errorf("code: read %s's next change at %s: %w", o.RepoID, at.position, err)
	}
	state := ChangeStateOpen
	if o.Draft {
		state = ChangeStateDraft
	}
	var work types.WorkItem
	if o.WorkItem != nil {
		work = *o.WorkItem
	}
	// THE CREDENTIAL A PERSON OPENED IT WITH, the change's own attribution
	// as every record a write leaves names it (iam.ActorFor); a seat acts as
	// itself with none.
	var operator string
	if kind == PartyPerson {
		operator = o.Owner.OperatorID
	}
	res, err := tx.ExecContext(ctx, openChangeSQL, o.ChangeID, o.RepoID, next, o.Title, len(o.Body),
		string(state), o.SourceRef, o.TargetRef, o.Head, owner, string(kind), operator, string(work.Backend),
		work.ID, work.Key, work.Project, o.TurnID, o.RunID, at.at(), at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("code: open change %s at %s: %w", o.ChangeID, at.position, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return changeNotApplied(ctx, at, o.RepoID, o.ChangeID, "a change already holds its id")
	}
	rows := 1
	if o.Body != "" {
		if _, err := tx.ExecContext(ctx, setBodySQL, o.ChangeID, o.Body); err != nil {
			return 0, fmt.Errorf("code: write change %s's body at %s: %w", o.ChangeID, at.position, err)
		}
		rows++
	}
	if _, err := tx.ExecContext(ctx, addAuthorSQL, o.ChangeID, owner, string(kind), 1, at.packed); err != nil {
		return 0, fmt.Errorf("code: write change %s's owner as its author at %s: %w", o.ChangeID, at.position, err)
	}
	if _, err := tx.ExecContext(ctx, addRevisionSQL, o.ChangeID, 1, o.Head, owner, string(kind), "", at.packed,
		at.at()); err != nil {
		return 0, fmt.Errorf("code: write change %s's first revision at %s: %w", o.ChangeID, at.position, err)
	}
	if _, err := tx.ExecContext(ctx, openedSQL, at.packed, o.RepoID); err != nil {
		return 0, fmt.Errorf("code: advance %s's next change at %s: %w", o.RepoID, at.position, err)
	}
	return rows + 3, nil
}

// applyChangeWrite writes one write of a change beyond its open — an edit, a
// draft's state, a retarget, a close, a reopen or a landing — into its
// change's row: the row read by its key, moved as the record says, and written
// back with one more version at the record's position by the [changeWrite]
// its op makes ([lifecycleWrite]), which sets what the op moves and nothing
// else; an edit's body where it sets one, a reopen's revision where its head
// moved while the change was closed, and the repository's version. A close, a
// draft, a retarget and a landing END THE CHANGE'S LIVE QUEUE ENTRY in the
// same apply, dequeued, naming which ([endLiveEntryOf]): its queue would
// otherwise land a change that is no longer what was enqueued. A landing
// merges the change at the tip its home found its head in — `push`, as one
// whose push set the target to its head is — and applies as nothing where the
// change is not at the head the home found, which only a record the ledger
// would have collapsed meets.
func (a *Applier) applyChangeWrite(ctx context.Context, tx *sql.Tx, at applyContext, payload any) (int, error) {
	repoID, changeID, write, err := lifecycleWrite(payload)
	if err != nil {
		return 0, fmt.Errorf("code: the change record at %s: %w", at.position, err)
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
			// Measured through body, the declared variable, never p: a type
			// switch's variable is a value internal/sourcetree/decltype
			// cannot type, and the cache's writers gate
			// (TestNothingButSyncWritesACache) refuses one handed to a call
			// it cannot type either, len among them, since it cannot tell
			// it from a gitproc.Cmd.
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
	case ChangeReopen:
		if row.state != ChangeStateClosed {
			return changeNotApplied(ctx, at, repoID, changeID, "a reopen takes a closed change, and it is "+
				string(row.state))
		}
		if by, _ := p.By.party(); by == "" {
			return changeNotApplied(ctx, at, repoID, changeID, "who reopened it names no seat")
		}
		next.state, next.closeReason, next.closedAt = ChangeStateOpen, "", sql.NullInt64{}
		// THE SOURCE MOVED WHILE THE CHANGE WAS CLOSED: what it proposes is
		// the branch's head now, a revision of its own, made by the record
		// that brought it back — as a change record's first is its open's.
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
	next.version, next.position = row.version+1, at.packed
	if err := writeChanges(ctx, tx, at.maxVariables, write, []*changeRow{&next}); err != nil {
		return 0, fmt.Errorf("code: write change %s at %s: %w", changeID, at.position, err)
	}
	rows := 1
	if body != nil {
		stmt, args := setBodySQL, []any{changeID, *body}
		if *body == "" {
			stmt, args = deleteBodySQL, []any{changeID}
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return 0, fmt.Errorf("code: write change %s's body at %s: %w", changeID, at.position, err)
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
	if revision != nil {
		by, kind := revision.By.party()
		if _, err := tx.ExecContext(ctx, addRevisionSQL, changeID, next.revision, next.head, by, string(kind), "",
			at.packed, at.at()); err != nil {
			return 0, fmt.Errorf("code: write change %s's revision %d at %s: %w", changeID, next.revision,
				at.position, err)
		}
		rows++
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, repoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", repoID, at.position, err)
	}
	return rows + 1, nil
}

// leavesQueue is how a write of a change's lifecycle ends its live queue
// entry, if its queue holds one: a close, a draft — made one, never one made
// ready — a retarget and a landing each dequeue it, naming which; "" for a
// write that leaves the change what was enqueued.
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

// lifecycleWrite is what a write of a change's lifecycle names — its
// repository and its change — and the [changeWrite] it makes of the change's
// row, which sets the columns its op moves and no other: an edit its text,
// and its work item only where it sets or clears one; a retarget its target;
// a draft's state, a close and a reopen the change's state with a close's
// columns and the head a reopen moves; a landing its state with a merge's.
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

// pushWrite is the [changeWrite] a push record makes of a change its entries
// reached: an insert of one it opened, whatever its later entries did to it,
// since the row is new; and of every other what its moves set — a merge's
// state, a close's, or else its head alone, the one move that leaves a change
// live.
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

// writeChanges writes rows by w: each row's VALUES are the whole row as the
// record read and moved it — every NOT NULL column among them, which even a
// row an upsert finds by its key must bind — in as few statements as the
// engine's parameters allow ([store.InsertRows]), and what lands of each is
// what w sets.
func writeChanges(ctx context.Context, tx store.Execer, maxVariables int, w changeWrite,
	rows []*changeRow) error {

	_, err := store.InsertRows(ctx, tx, maxVariables, insertChangesSQL, changeValuesSQL, string(w), len(rows),
		func(i int) []any {
			r := rows[i]
			return []any{r.id, r.repoID, r.number, r.title, r.bodyBytes, string(r.state), r.source, r.target,
				r.head, r.revision, r.owner, string(r.ownerKind), r.operator, string(r.work.Backend), r.work.ID,
				r.work.Key, r.work.Project, r.turn, r.run, r.openedAt, r.opened, r.closedAt, r.closeReason,
				r.mergedAt, string(r.mergedVia), r.mergeOID, r.version, r.position}
		})
	return err
}

// write writes what a push record's [changeBook] made of the changes its
// entries reached — each change's row, inserted for a change the record
// opened and written back over every other by what the record did to it
// ([pushWrite]), its revisions, its new authors, the descriptions of the
// changes it opened and, where it opened any, the repository's next number —
// and answers the rows it wrote, [changeBook.rowCount]: the push apply's half
// of the book, here in the applier's own files, while the model it writes
// from (revisions.go) is the decide's as much as the apply's. An insert's
// conflict on a key is a skip, never an abort, which would stall the log on
// every node: no record this build applies meets one, since the book adds
// only what the rows it read do not hold, and a change it opens takes an id
// derived from a push no row holds yet ([PushChangeID]). A moved change's
// conflict is its write.
//
// # Every table in as few statements as the engine's parameters allow
//
// This is the push apply, on every data node, holding the replicated estate's
// one write lane, and one record can move some 1 300 changes — a bulk
// rebase's fast-forwards by their author, three rows each — so a statement a
// change is the cost that grows with a record. The change rows go through
// [store.InsertRows], a statement a chunk of each kind of write, as the
// revisions and authors beside them and the refs the same apply writes do: a
// change the record moved is written back as an upsert's conflict, found by
// the very key it conflicts on. Written one UPDATE a change, a record of a
// bulk rebase held the lane past statelog.ApplyTxTimeBudget (doc.go's
// measured section).
//
// # A head move rewrites one index entry
//
// Each change is written by what the record did to it, and its write sets
// nothing else ([changeWrite]), since the driver rewrites a row's entry in
// every index whose key or predicate holds a column a write names, whatever
// the value. So the changes whose heads the record moved and nothing more —
// nearly every change a record reaches, a push to a change's source being the
// write a change meets most — are written by [changeHeadMoved], which
// rewrites one entry each, in code_changes_target_idx, the one index the head
// is in; and the few it merged or closed by the write that moves their state,
// which rewrites every index the state is in.
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
			return 0, fmt.Errorf("code: %s: %w", part.what, err)
		}
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, n) DO NOTHING`, len(w.revisions),
		func(i int) []any {
			r := w.revisions[i]
			return []any{r.change, r.n, r.head, r.pusher, string(r.kind), r.pushID, b.packed, b.at}
		}); err != nil {
		return 0, fmt.Errorf("code: write the revisions: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position) VALUES `,
		`(?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, author) DO NOTHING`, len(w.authors),
		func(i int) []any {
			a := w.authors[i]
			return []any{a.change, a.author, string(a.kind), a.first, b.packed}
		}); err != nil {
		return 0, fmt.Errorf("code: write the authors: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables, `INSERT INTO code_change_bodies (change_id, body) VALUES `,
		`(?, ?)`, `ON CONFLICT (change_id) DO NOTHING`, len(w.bodies), func(i int) []any {
			return []any{w.bodies[i].change, w.bodies[i].body}
		}); err != nil {
		return 0, fmt.Errorf("code: write the descriptions of the changes opened: %w", err)
	}
	// THE MOVED CHANGES LEAVE THEIR QUEUES: a head moved ejects a live
	// entry, a merge or a close dequeues it — each list one statement a
	// chunk, a seek of code_queue_change_idx a change.
	var ejected, merged, closed []string
	for _, id := range w.touched {
		if w.opened[id] || !b.queued[id] {
			continue
		}
		switch pushWrite(b.rows[id], false) {
		case changeMerged:
			merged = append(merged, id)
		case changeStateMoved:
			closed = append(closed, id)
		default:
			ejected = append(ejected, id)
		}
	}
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
	// THE REPOSITORY'S NEXT NUMBER, moved on past every change the record
	// opened, in the repository's own row — which the record writes anyway,
	// its counters, so this costs the record no row of its budget.
	if len(byWrite[changeOpened]) > 0 {
		if _, err := tx.ExecContext(ctx, nextChangeAfterSQL, b.next, b.repo); err != nil {
			return 0, fmt.Errorf("code: move the repository's next change on: %w", err)
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
// the statement [writeChanges] issues, whose VALUES are the whole row — the
// insert of a change the record opens, which skips a conflict on its key, or
// an upsert found by that key, written back over the row the record read,
// which SETS WHAT ONE KIND OF WRITE MOVES AND NO OTHER COLUMN.
//
// # Why a write sets only what it moves
//
// The pinned driver keeps an index by the columns a write NAMES, never by the
// values it writes: for every index of code_changes whose key or partial
// predicate holds a column the write sets, it deletes the row's entry and
// inserts it again — a leaf of that index more in the record's commit, on
// every data node — where no value of the key moved as much as where one did,
// a partial index's predicate deciding only whether the row has an entry to
// delete and one to insert. One write that set every column a change's
// lifecycle may move kept seven of the table's eight secondary indexes on
// every write of a change, and the push's upsert that set every column a push
// may move kept six on every head move, rewriting five entries of a live
// change's: the head move is the write a change meets most — every push to
// its source branch, up to some 1 300 changes in one record — and the head is
// in the key of one index alone, code_changes_target_idx. So each kind of
// write sets only the columns it moves, and two kinds share one write only
// where they rewrite the same indexes:
//
//   - changeHeadMoved, a push's move of a live change's source: its head and
//     revision — code_changes_target_idx alone.
//   - changeStateMoved, a draft's state, a close, a reopen, and a push's close
//     of a change whose source it deleted: the state with a close's columns,
//     and the head and revision that a reopen, or the record's own earlier
//     move of the source, moved — every index whose key or predicate holds
//     the state (the open and target indexes, partial on the live states, and
//     the state, owner and repository-state listings), among them the one the
//     head is in.
//   - changeMerged, a landing and a push's merge: the state with a merge's
//     columns, and the head and revision — those, and code_changes_merge_idx.
//   - changeEdited, an edit of the title or the description: no index.
//   - changeItemEdited, an edit that sets or clears the work item, beside the
//     text: code_changes_work_idx alone.
//   - changeRetargeted, a retarget: code_changes_target_idx alone.
//
// Every write moves the change's version and position, which no index holds.
// TestAChangeWriteRewritesOnlyTheIndexesItMoves holds each kind to the
// indexes its plan rewrites, and the push apply's head moves to the one: an
// index added on a column a write sets — the head's above all — is a decision
// that test makes somebody take, never a cost nobody saw.
type changeWrite string

// The kinds of write of a change's row, each the conflict clause of its
// statement ([changeWrite] says what each rewrites).
const (
	changeOpened    changeWrite = `ON CONFLICT (id) DO NOTHING`
	changeHeadMoved changeWrite = `ON CONFLICT (id) DO UPDATE SET head_oid = excluded.head_oid,
		revision = excluded.revision, version = excluded.version, position = excluded.position`
	changeStateMoved changeWrite = `ON CONFLICT (id) DO UPDATE SET state = excluded.state,
		closed_at = excluded.closed_at, close_reason = excluded.close_reason, head_oid = excluded.head_oid,
		revision = excluded.revision, version = excluded.version, position = excluded.position`
	changeMerged changeWrite = `ON CONFLICT (id) DO UPDATE SET state = excluded.state,
		merged_at = excluded.merged_at, merged_via = excluded.merged_via, merge_oid = excluded.merge_oid,
		head_oid = excluded.head_oid, revision = excluded.revision, version = excluded.version,
		position = excluded.position`
	changeEdited changeWrite = `ON CONFLICT (id) DO UPDATE SET title = excluded.title,
		body_bytes = excluded.body_bytes, version = excluded.version, position = excluded.position`
	changeItemEdited changeWrite = `ON CONFLICT (id) DO UPDATE SET title = excluded.title,
		body_bytes = excluded.body_bytes, work_backend = excluded.work_backend, work_id = excluded.work_id,
		work_key = excluded.work_key, work_project = excluded.work_project, version = excluded.version,
		position = excluded.position`
	changeRetargeted changeWrite = `ON CONFLICT (id) DO UPDATE SET target_ref = excluded.target_ref,
		version = excluded.version, position = excluded.position`
)

// The lifecycle's writes beside a change's row. A statement that reads a row
// to find the ones it writes is registered in plan_test.go — the repository's
// next change and its counters by its key, and a change's body by its own —
// and the inserts locate no row, nor an upsert, whose conflict is found by
// the key its VALUES carry: a conflict on a key is a skip or the write, never
// an abort, which would stall the log on every node.
const (
	nextChangeSQL = `SELECT next_change FROM code_repos WHERE id = ?`
	openChangeSQL = `INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, owner_operator_id, work_backend, work_id, work_key, work_project,
			created_turn, created_run, opened_at, opened_position, version, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
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
	addRevisionSQL = `INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (change_id, n) DO NOTHING`

	// insertChangesSQL and changeValuesSQL are change rows as [writeChanges]
	// writes them, a chunk a statement: every column [changeColumns] names,
	// in its order — whole, since a NOT NULL column without a default is one
	// even a row an upsert finds by its key must bind.
	insertChangesSQL = `INSERT INTO code_changes (` + changeColumns + `) VALUES `
	changeValuesSQL  = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)
