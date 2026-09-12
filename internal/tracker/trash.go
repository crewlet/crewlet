package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The trash: how a task leaves the company's view without leaving its history,
// and how it comes back.
//
// # Why a removal is a TOMBSTONE and not a delete
//
// Every other object here is a document and a removal could have been one more
// field on it. It is not, because the two questions a person asks about a
// removed task — "who removed it and when" and "bring it back exactly as it
// was" — are both answered by the SAME record if the removal is a stamp, and by
// neither if it is a delete. A delete also makes the ONE irreversible operation
// (`purge`) indistinguishable from the ordinary one, which is how somebody
// discovers the difference at the wrong moment.
//
// So `removed_at` is a column, `removed=true` is the whole of the trash's
// query surface, and a restore clears the stamp AT ANY AGE with one commit —
// no window, no rebuild, and no grace period during which a restore stops
// being possible. That is the property the tombstone buys and the reason
// nothing here has a retention horizon.
//
// # A SUBTREE LEAVES TOGETHER OR NOT AT ALL
//
// Removing a parent and leaving its children reachable is the shape that
// produces orphans a person then has to find. `RemovedWith` is what makes the
// inverse true as well: every descendant's tombstone names the root of the
// gesture that removed it, so a restore brings back exactly what one removal
// took and never a task that was already in the trash for its own reasons.
//
// # It is NOT a write on a frozen task
//
// [Writer.UpdateTask] refuses every patch on a removed task — "restore it
// first" — which is what makes a tombstone a freeze rather than a flag. The
// restore therefore cannot go through it, and these are its own verbs for
// exactly that reason rather than for want of a field.

// RemoveTask puts a task, and optionally its whole subtree, in the trash.
//
// ONE COMMIT PER TASK, published in DEPTH ORDER so a reader that sees a child
// removed has already seen its parent go — the same order the move sequence
// uses and for the same reason: a partially applied gesture must never leave a
// tree in a shape no single write could have made.
//
// The ROOT's own tombstone carries no `RemovedWith`, and every descendant's
// names the root. A restore reads that to decide what comes back with what.
func (w *Writer) RemoveTask(ctx context.Context, opID, id, project string,
	subtree bool, notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("tracker: a removal names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a removal on task %s names "+
			"no project — the caller resolved a key to reach this task and "+
			"therefore holds one", id)
	}
	at := w.Now()
	stamp := Tombstone{By: w.Actor, Kind: w.ActorKind, At: at}

	var descendants []Task
	if subtree {
		var err error
		descendants, err = w.subtreeOf(ctx, id)
		if err != nil {
			return WriteResult{}, err
		}
		if len(descendants) > MaxDescendants {
			return WriteResult{}, fmt.Errorf("tracker: task %s has %d "+
				"descendants and a removal carries at most %d — remove them "+
				"in smaller subtrees, or purge the root if it is really going",
				id, len(descendants), MaxDescendants)
		}
	}

	// THE ROOT FIRST. A descendant removed while its parent is still live
	// is an ordinary state somebody can undo; a live child under a removed
	// parent is the orphan this order exists to avoid.
	result, err := w.tombstone(ctx, opID, id, project, stamp, notify)
	if err != nil {
		return result, err
	}
	for i, descendant := range descendants {
		with := id
		child := stamp
		child.RemovedWith = &with
		// THE DESCENDANTS WAKE NOBODY. One removal is one thing that
		// happened, and a subtree of forty would otherwise send forty
		// notifications for it — the root's is the one that says what
		// was done.
		if _, err := w.tombstone(ctx, stepID(opID, fmt.Sprintf("d%d", i)),
			descendant.ID, descendant.Project, child, nil); err != nil {
			return result, fmt.Errorf("tracker: the root of %s is in the trash "+
				"and %d of %d descendants followed it; re-run the removal to "+
				"finish, which is idempotent: %w",
				id, i, len(descendants), err)
		}
	}
	return result, nil
}

// RestoreTask clears a tombstone, at any age.
//
// ONE COMMIT AND NO WINDOW — see this file's head. The subtree comes back with
// it where the subtree left with it: a descendant whose tombstone names THIS
// root was removed by that gesture and is restored by its inverse, while one
// that was already in the trash for its own reasons stays there. That is what
// `RemovedWith` is for, and it is why a restore can be a single argument.
func (w *Writer) RestoreTask(ctx context.Context, opID, id, project string,
	notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("tracker: a restore names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a restore on task %s names "+
			"no project", id)
	}

	removedWith, err := w.removedWith(ctx, id)
	if err != nil {
		return WriteResult{}, err
	}
	// THE ROOT FIRST AGAIN, and for the mirror of the removal's reason: a
	// child restored under a still-removed parent is reachable from
	// nothing until the parent follows.
	result, err := w.clearTombstone(ctx, opID, id, project, notify)
	if err != nil {
		return result, err
	}
	for i, descendant := range removedWith {
		if _, err := w.clearTombstone(ctx, stepID(opID, fmt.Sprintf("d%d", i)),
			descendant.ID, descendant.Project, nil); err != nil {
			return result, fmt.Errorf("tracker: %s is out of the trash and %d "+
				"of %d tasks removed with it followed; re-run the restore to "+
				"finish, which is idempotent: %w",
				id, i, len(removedWith), err)
		}
	}
	return result, nil
}

// tombstone publishes one task's removal.
func (w *Writer) tombstone(ctx context.Context, opID, id, project string,
	stamp Tombstone, notify *Notify) (WriteResult, error) {

	subject := TaskSubject(id)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is not "+
					"on this node: %w", id, statelog.ErrUnavailable)
			case current.Removed != nil:
				// ALREADY IN THE TRASH IS NOTHING TO DO, and it is a
				// SUCCESS rather than a conflict: a re-run of a removal
				// that half-finished must be able to complete, and the
				// task is in the state the caller asked for.
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(subject, OpTombstone, ChangeRemoved, scope,
				opID, TaskPatch{Removed: &stamp}, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// clearTombstone publishes one task's restore.
//
// A ZERO TOMBSTONE is how the patch spells "clear it" — see [applyPatch]: an
// absent field means "leave it alone", so the clear has to be a value.
func (w *Writer) clearTombstone(ctx context.Context, opID, id, project string,
	notify *Notify) (WriteResult, error) {

	subject := TaskSubject(id)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is not "+
					"on this node: %w", id, statelog.ErrUnavailable)
			case current.Removed == nil:
				// NOT IN THE TRASH IS NOTHING TO DO, on the removal's
				// own rule: a re-run must be able to finish.
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(subject, OpRestore, ChangeRestored, scope,
				opID, TaskPatch{Removed: &Tombstone{}}, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// subtreeOf reads a task's descendants before the first append.
//
// OUTSIDE THE SNAPSHOT, like every other walking sequence's own read (see
// [Writer.db]): nothing decided from it is paired with a broker expectation —
// each tombstone below forms its own inside its own snapshot — and a
// descendant that arrives after this read is left live rather than removed,
// which is the safe direction and which a re-run fixes.
func (w *Writer) subtreeOf(ctx context.Context, id string) ([]Task, error) {
	if w.db == nil {
		return nil, fmt.Errorf("tracker: this writer has no store, so it "+
			"cannot read task %s's subtree; a subtree removal needs one", id)
	}
	var out []Task
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = readSubtree(ctx, tx, id)
		return err
	}); err != nil {
		return nil, fmt.Errorf("tracker: read %s's subtree: %w", id, err)
	}
	return out, nil
}

// removedWith is every task whose tombstone names this one as the gesture that
// removed it.
//
// A TASK ALREADY IN THE TRASH FOR ITS OWN REASONS IS NOT IN THIS LIST, which
// is the whole point of the column: a restore brings back exactly what one
// removal took.
func (w *Writer) removedWith(ctx context.Context, id string) ([]Task, error) {
	if w.db == nil {
		return nil, nil
	}
	var out []Task
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, project_key FROM tracker_tasks
			 WHERE removed_with = ? AND removed_at IS NOT NULL
			 ORDER BY depth, id`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var task Task
			if err := rows.Scan(&task.ID, &task.Project); err != nil {
				return err
			}
			out = append(out, task)
		}
		return rows.Err()
	}); err != nil {
		return nil, fmt.Errorf("tracker: read what was removed with %s: %w", id, err)
	}
	return out, nil
}
