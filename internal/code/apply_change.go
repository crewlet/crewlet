package code

import (
	"context"
	"database/sql"
	"fmt"

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

// write writes what a push record's [changeBook] made of the changes its
// entries reached — each change's row whole, inserted for a change the record
// opened and written back over every other, its revisions, its new authors,
// the descriptions of the changes it opened and, where it opened any, the
// repository's next number — and answers the rows it wrote,
// [changeBook.rowCount]: the push apply's half of the book, here in the
// applier's own files, while the model it writes from (revisions.go) is the
// decide's as much as the apply's. A conflict on a key is a skip, never an
// abort, which would stall the log on every node: no record this build
// applies meets one, since the book adds only what the rows it read do not
// hold, and a change it opens takes an id derived from a push no row holds
// yet ([PushChangeID]).
func (b *changeBook) write(ctx context.Context, tx *sql.Tx, maxVariables int) (int, error) {
	var opened []*changeRow
	for _, id := range b.touched {
		if b.opened[id] {
			opened = append(opened, b.rows[id])
			continue
		}
		if err := writeChange(ctx, tx, *b.rows[id]); err != nil {
			return 0, fmt.Errorf("code: write change %s: %w", id, err)
		}
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_changes (`+changeColumns+`) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (id) DO NOTHING`, len(opened), func(i int) []any {
			r := opened[i]
			return []any{r.id, r.repoID, r.number, r.title, r.bodyBytes, string(r.state), r.source, r.target, r.head,
				r.revision, r.owner, string(r.ownerKind), r.operator, string(r.work.Backend), r.work.ID, r.work.Key,
				r.work.Project, r.turn, r.run, r.openedAt, r.opened, r.closedAt, r.closeReason, r.mergedAt,
				string(r.mergedVia), r.mergeOID, r.version, r.position}
		}); err != nil {
		return 0, fmt.Errorf("code: open the changes: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position, at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, n) DO NOTHING`, len(b.revisions),
		func(i int) []any {
			r := b.revisions[i]
			return []any{r.change, r.n, r.head, r.pusher, string(r.kind), r.pushID, b.packed, b.at}
		}); err != nil {
		return 0, fmt.Errorf("code: write the revisions: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position) VALUES `,
		`(?, ?, ?, ?, ?)`, `ON CONFLICT (change_id, author) DO NOTHING`, len(b.authors),
		func(i int) []any {
			a := b.authors[i]
			return []any{a.change, a.author, string(a.kind), a.first, b.packed}
		}); err != nil {
		return 0, fmt.Errorf("code: write the authors: %w", err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables, `INSERT INTO code_change_bodies (change_id, body) VALUES `,
		`(?, ?)`, `ON CONFLICT (change_id) DO NOTHING`, len(b.bodies), func(i int) []any {
			return []any{b.bodies[i].change, b.bodies[i].body}
		}); err != nil {
		return 0, fmt.Errorf("code: write the descriptions of the changes opened: %w", err)
	}
	// THE REPOSITORY'S NEXT NUMBER, moved on past every change the record
	// opened, in the repository's own row — which the record writes anyway,
	// its counters, so this costs the record no row of its budget.
	if len(opened) > 0 {
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
	writeChangeSQL = `UPDATE code_changes SET title = ?, body_bytes = ?, state = ?, target_ref = ?, head_oid = ?,
		revision = ?, work_backend = ?, work_id = ?, work_key = ?, work_project = ?, closed_at = ?, close_reason = ?,
		merged_at = ?, merged_via = ?, merge_oid = ?, version = ?, position = ? WHERE id = ?`
)
