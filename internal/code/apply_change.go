package code

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/events/types"
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
// draft's state, a retarget, a close or a reopen — into its change's row: the
// row read by its key, moved as the record says, and written back whole with
// one more version at the record's position; an edit's body where it sets one,
// a reopen's revision where its head moved while the change was closed, and
// the repository's version.
func (a *Applier) applyChangeWrite(ctx context.Context, tx *sql.Tx, at applyContext, payload any) (int, error) {
	var repoID, changeID string
	switch p := payload.(type) {
	case ChangeEdit:
		repoID, changeID = p.RepoID, p.ChangeID
	case ChangeDraft:
		repoID, changeID = p.RepoID, p.ChangeID
	case ChangeRetarget:
		repoID, changeID = p.RepoID, p.ChangeID
	case ChangeClose:
		repoID, changeID = p.RepoID, p.ChangeID
	case ChangeReopen:
		repoID, changeID = p.RepoID, p.ChangeID
	default:
		return 0, fmt.Errorf("code: the change record at %s carries a %T", at.position, payload)
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
			body, next.bodyBytes = p.Body, int64(len(*p.Body))
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
	}
	next.version, next.position = row.version+1, at.packed
	if err := writeChange(ctx, tx, next); err != nil {
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

// writeChange writes r over its change's row: every column a write of the
// change's lifecycle or a push may move, by its key.
func writeChange(ctx context.Context, tx *sql.Tx, r changeRow) error {
	_, err := tx.ExecContext(ctx, writeChangeSQL, r.title, r.bodyBytes, string(r.state), r.target, r.head,
		r.revision, string(r.work.Backend), r.work.ID, r.work.Key, r.work.Project, r.closedAt, r.closeReason,
		r.mergedAt, string(r.mergedVia), r.mergeOID, r.version, r.position, r.id)
	return err
}

// changeNotApplied logs a change record that applies as nothing because its
// change is not in the state its decide held, and answers that it wrote no
// row.
func changeNotApplied(ctx context.Context, at applyContext, repoID, changeID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_change_not_applied", "repo", repoID, "change", changeID,
		"op", string(at.record.Op), "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// The lifecycle's writes. A statement that reads a row to find the ones it
// writes is registered in plan_test.go — the repository's next change and
// its counters by its key, a change's row and its body by theirs — and the
// inserts locate no row: a conflict on a key is a skip, never an abort, which
// would stall the log on every node.
const (
	nextChangeSQL = `SELECT next_change FROM code_repos WHERE id = ?`
	openChangeSQL = `INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, owner_operator_id, work_backend, work_id, work_key, work_project,
			created_turn, created_run, opened_at, opened_position, version, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT (id) DO NOTHING`
	openedSQL  = `UPDATE code_repos SET next_change = next_change + 1, version = ? WHERE id = ?`
	touchedSQL = `UPDATE code_repos SET version = ? WHERE id = ?`
	setBodySQL = `INSERT INTO code_change_bodies (change_id, body) VALUES (?, ?)
		ON CONFLICT (change_id) DO UPDATE SET body = excluded.body`
	deleteBodySQL = `DELETE FROM code_change_bodies WHERE change_id = ?`
	addAuthorSQL  = `INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (change_id, author) DO NOTHING`
	addRevisionSQL = `INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (change_id, n) DO NOTHING`
	writeChangeSQL = `UPDATE code_changes SET title = ?, body_bytes = ?, state = ?, target_ref = ?, head_oid = ?,
		revision = ?, work_backend = ?, work_id = ?, work_key = ?, work_project = ?, closed_at = ?, close_reason = ?,
		merged_at = ?, merged_via = ?, merge_oid = ?, version = ?, position = ? WHERE id = ?`
)

// purgeChanges deletes, for a purge part, at most budget rows of a deleted
// repository's changes, and only while what the part's wide rows weigh —
// weighed, the push rows it already took, and then each body it takes — is
// under PurgePartBytes; it answers how many rows it deleted.
//
// A CHANGE GOES WHOLE, AND ITS ROW LAST. A change's revisions, authors and
// body carry no repo_id: the only way to them from the repository is the
// change's own row, so a row deleted before them would leave rows no later
// part — and no final purge — could find. The changes are taken in number
// order, and every node takes the same ones, because the walk reads them in
// the order of [changesNumberKey] and a repository's numbers never repeat.
//
// SET-BASED OVER THE CHANGES THAT FIT, ONE CUT AT MOST. The walk counts each
// change's revisions and authors, each to the part's bound, and its body
// row, and takes changes while the whole of one fits what is left of the
// budget; their revisions, authors, bodies and rows go in four statements over
// their ids. The change the walk stops at for its rows loses only revisions
// and then authors — its oldest revisions first, its authors in key order —
// up to what is left, and its body and row wait for a part that finds the
// rest gone. Measured on a part of a thousand changes of one revision and one
// author each: deleting one change at a time, four statements a change, held
// the transaction 850 ms, over three times the 250 ms after which the
// framework ends one at the next record (statelog.ApplyTxTimeBudget); this
// held it 62 ms, and a part of 3 000 refs 118 ms (one run each, under the
// race detector on four cores). BenchmarkApplyAPurgePart's arms over changes
// measure it without the detector (doc.go's measured section).
//
// AND WHILE THE BYTES LAST. A change's body is the one wide value it keeps,
// up to [MaxChangeBodyBytes], and deleting it frees its overflow pages, which
// a count does not see: a part that took a thousand changes with bodies at
// the bound, by their rows alone, on top of push rows just short of
// [PurgePartBytes], held the write lane 112 ms in the run of the review that
// weighed them, where the bound sizes the most a part takes to a third of
// the budget. So the bodies share the pushes' budget: a change is taken only
// while what the part weighs is under PurgePartBytes — the first whenever the
// pushes left room, whatever its body, and so at most one change past the
// bound — and a walk stopped by the bytes cuts nothing, leaving the rest to a
// part of their own. A part whose push rows reached the bound takes no
// change; it advanced by those. Each body is weighed by its change's
// body_bytes, a column of the narrow row the walk reads anyway, never by the
// body's row, which would walk the body's overflow pages to learn what
// deleting them costs. The cut never takes a body, so it weighs nothing.
func purgeChanges(ctx context.Context, tx *sql.Tx, repoID string, budget int, weighed int64) (int, error) {
	if weighed >= PurgePartBytes {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, purgeChangeWalkSQL, repoID, budget)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	var whole []string
	left, cut := budget, ""
	for rows.Next() {
		var id string
		var body int64
		var revisions, authors, bodies int
		if err := rows.Scan(&id, &body, &revisions, &authors, &bodies); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
		}
		if weighed >= PurgePartBytes {
			break
		}
		need := revisions + authors + bodies + 1
		if need > left {
			cut = id
			break
		}
		whole = append(whole, id)
		left -= need
		weighed += body
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("code: read %s's changes to purge: %w", repoID, err)
	}
	_ = rows.Close()

	deleted := 0
	if len(whole) > 0 {
		list, err := jsonList(whole)
		if err != nil {
			return 0, err
		}
		for _, stmt := range []string{purgeChangeRevisionsSQL, purgeChangeAuthorsSQL, purgeChangeBodiesSQL,
			purgeChangeRowsSQL} {
			res, err := tx.ExecContext(ctx, stmt, list)
			if err != nil {
				return 0, fmt.Errorf("code: purge %s's changes: %w", repoID, err)
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
		}
	}
	// THE CHANGE THE WALK STOPPED AT FOR ITS ROWS needs more than is left —
	// its revisions and authors alone fill it — so its body and row are
	// never reached here.
	for _, stmt := range []string{purgeCutRevisionsSQL, purgeCutAuthorsSQL} {
		if cut == "" || left == 0 {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, cut, left)
		if err != nil {
			return 0, fmt.Errorf("code: purge change %s of %s: %w", cut, repoID, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		left -= int(n)
	}
	return deleted, nil
}

// A purge's statements over a repository's changes: every one with a WHERE is
// registered in plan_test.go, so the pinned driver's planner is held to the
// seek each is written for.
const (
	// purgeChangeWalkSQL is a repository's changes in number order, each with
	// what its body weighs, its revisions and its authors counted at most ?2
	// deep, and its body row: a count is asked only of whether the whole of
	// one change fits, and the walk stops at the first that does not, so it
	// reads a part's bound of dependents and one change's more, whatever the
	// repository holds. The body is COUNTED from its key's index, never read,
	// so the walk never touches a body's overflow pages.
	purgeChangeWalkSQL = `SELECT c.id, c.body_bytes,
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_revisions WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_change_authors WHERE change_id = c.id LIMIT ?2)),
		(SELECT COUNT(*) FROM code_change_bodies WHERE change_id = c.id)
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` WHERE c.repo_id = ?1
		ORDER BY c.number LIMIT ?2`
	// The changes the walk took whole, by the JSON list of their ids — each
	// driven from the list, one seek of a key an element, as planner.go says
	// a batch is.
	purgeChangeRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM json_each(?) AS j CROSS JOIN code_revisions AS r
		WHERE r.change_id = CAST(j.value AS TEXT))`
	purgeChangeAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_authors AS a
		WHERE a.change_id = CAST(j.value AS TEXT))`
	purgeChangeBodiesSQL = `DELETE FROM code_change_bodies WHERE rowid IN (SELECT b.rowid
		FROM json_each(?) AS j CROSS JOIN code_change_bodies AS b
		WHERE b.change_id = CAST(j.value AS TEXT))`
	purgeChangeRowsSQL = `DELETE FROM code_changes WHERE rowid IN (SELECT c.rowid
		FROM json_each(?) AS j CROSS JOIN code_changes AS c
		WHERE c.id = CAST(j.value AS TEXT))`
	// purgeCutRevisionsSQL and purgeCutAuthorsSQL are the cut change's
	// oldest revisions and its authors in key order, no more than the part
	// has left.
	purgeCutRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT rowid
		FROM code_revisions WHERE change_id = ? ORDER BY n LIMIT ?)`
	purgeCutAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT rowid
		FROM code_change_authors WHERE change_id = ? ORDER BY author LIMIT ?)`
	// The final purge's, unbounded because the decide forms it only once its
	// bounded count found at most PurgePartRows rows left, and what the
	// bodies among them weigh within PurgePartBytes — the dependents first,
	// reached through their changes, then the changes.
	purgeAllRevisionsSQL = `DELETE FROM code_revisions WHERE rowid IN (SELECT r.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_revisions AS r
		WHERE c.repo_id = ? AND r.change_id = c.id)`
	purgeAllAuthorsSQL = `DELETE FROM code_change_authors WHERE rowid IN (SELECT a.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_authors AS a
		WHERE c.repo_id = ? AND a.change_id = c.id)`
	purgeAllBodiesSQL = `DELETE FROM code_change_bodies WHERE rowid IN (SELECT b.rowid
		FROM code_changes AS c INDEXED BY ` + changesNumberKey + ` CROSS JOIN code_change_bodies AS b
		WHERE c.repo_id = ? AND b.change_id = c.id)`
	purgeAllChangesSQL = `DELETE FROM code_changes INDEXED BY ` + changesNumberKey + ` WHERE repo_id = ?`
)
