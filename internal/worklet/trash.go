package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The trash: a task leaves the company's view without leaving its history.
//
// A removal is a TOMBSTONE (`removed_at`), not a delete: the stamp answers both
// "who removed it and when" and "bring it back exactly as it was", and keeps the
// irreversible `purge` distinct from it. `removed=true` is the trash's whole
// query surface, and a restore clears the stamp at ANY AGE in one commit.
//
// A subtree removal takes it all, each descendant's tombstone naming the root in
// `RemovedWith`, so a restore brings back exactly what that removal took. A
// removal of an item ALONE (an author's: what hangs from it may be anybody's)
// leaves its live subtasks listed as roots of what is left; the trash lists
// each removal once, under the task it was made on ([anchorOf]).

// ErrNothingToRestore reports a restore with nothing to bring back: the task
// is not in the trash, nothing is in the trash with it, and no earlier copy of
// this operation put it back. A restore answered from its own first copy is
// NOT this — it is the success that copy was.
var ErrNothingToRestore = errors.New("worklet: nothing to restore")

// ErrRestoresOthers reports a restore asked to bring its task back ALONE (how
// an author restores) while the gesture that trashed it took other tasks, which
// are a project lead's or an admin's to bring back. Carried beside
// [ErrForbidden], the class every surface reads it as. The writer decides it
// over the same read of what went with the task that the restore walks, so the
// two cannot disagree; nothing is written when it refuses.
var ErrRestoresOthers = errors.New("worklet: other tasks went into the trash with this one")

// SubtreeStopped reports a subtree removal or restore whose ROOT landed and
// whose walk over the rest stopped part of the way: a type, because the caller
// must say both that the named task moved and that some of what goes with it
// did not, where a bare error reads as "nothing happened". The same gesture
// again, under the same operation id or a new one, finishes it: a task already
// where the gesture leaves it is nothing to do.
type SubtreeStopped struct {
	// Verb is the gesture, as a caller would name it: "removal" or
	// "restore".
	Verb string
	// Root is the task the gesture named, whose own step landed.
	Root string
	// Followed is how many of the rest landed before the stop, and Of how
	// many the walk set out to carry.
	Followed, Of int
	// OpID is the gesture's operation.
	OpID string
	// Err is why the walk stopped — an [ErrStepUnresolved] where a step's
	// outcome is unknown, or the refusal of the step it stopped at.
	Err error
}

func (e *SubtreeStopped) Error() string {
	return fmt.Sprintf("worklet: the %s of task %s landed and %d of the %d "+
		"tasks that go with it followed before the walk stopped; making the "+
		"gesture again finishes it — under operation %s or a new one, since a "+
		"task already where it leaves it is nothing to do: %v",
		e.Verb, e.Root, e.Followed, e.Of, e.OpID, e.Err)
}

func (e *SubtreeStopped) Unwrap() error { return e.Err }

// RemoveTask puts a task, and optionally its whole subtree, in the trash.
//
// ONE COMMIT PER TASK, in DEPTH ORDER, so a reader that sees a child removed
// has seen its parent go, as the move sequence does: a partly applied gesture
// never leaves a tree no single write could have made. The walk keeps
// [resolved]'s contract (an unknown outcome ENDS it), and each descendant's
// step is NAMED FOR THE DESCENDANT, never its place: a re-run reads the walk
// afresh, and a purge in between shifts every place after it, so the ledger
// would answer one task's step with another's row.
//
// The ROOT's tombstone carries no `RemovedWith`; every descendant's names the
// root, which a restore reads to decide what comes back.
func (w *Writer) RemoveTask(ctx context.Context, opID, id, project string,
	subtree bool, notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, invalid("a removal names no task")
	case project == "":
		return WriteResult{}, invalid("a removal on task %s names "+
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
			return WriteResult{}, invalid("task %s has %d "+
				"descendants and a removal carries at most %d — remove them "+
				"in smaller subtrees, or purge the root if it is really going",
				id, len(descendants), MaxDescendants)
		}
	}

	// THE ROOT FIRST: its step is what the answer reports, and an UNKNOWN
	// root ends the walk. A walk stopped part way leaves the rest live under
	// a removed root, the state an item-alone removal leaves by design
	// ([anchorOf]), and the same gesture again takes the rest.
	result, err := w.tombstone(ctx, opID, id, project, stamp, notify)
	if err = resolved(fmt.Sprintf("task %s's removal", id), result, err); err != nil {
		return result, err
	}
	for i, descendant := range descendants {
		with := id
		child := stamp
		child.RemovedWith = &with
		// THE DESCENDANTS WAKE NOBODY: one removal is one thing that
		// happened, and the root's notification says it.
		removal, err := w.tombstone(ctx, stepID(opID, "d/"+descendant.ID),
			descendant.ID, descendant.Project, child, nil)
		if err = resolved(fmt.Sprintf("task %s's removal with %s",
			descendant.ID, id), removal, err); err != nil {
			return result, &SubtreeStopped{Verb: "removal", Root: id,
				Followed: i, Of: len(descendants), OpID: opID, Err: err}
		}
	}
	return result, nil
}

// RestoreTask clears a tombstone, at any age: its own verb, because
// [Writer.UpdateTask] refuses every patch on a removed task (the freeze).
//
// A descendant whose tombstone names THIS root comes back with it; one already
// in the trash for its own reasons stays. Each descendant's step is NAMED FOR
// THE DESCENDANT, as in [Writer.RemoveTask] and more sharply: the list walked
// is what is STILL in the trash, so every task restored shifts the rest.
//
// ALONE asks for the task by itself, refused with [ErrRestoresOthers] before
// anything is written when the removal took others.
func (w *Writer) RestoreTask(ctx context.Context, opID, id, project string,
	alone bool, notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, invalid("a restore names no task")
	case project == "":
		return WriteResult{}, invalid("a restore on task %s names "+
			"no project", id)
	}

	removedWith, err := w.removedWith(ctx, id)
	if err != nil {
		return WriteResult{}, err
	}
	if alone && len(removedWith) > 0 {
		return WriteResult{}, forbiddenAs(ErrRestoresOthers, "%d other tasks "+
			"went into the trash with task %s, and this restore may bring back "+
			"that task alone — restoring them is the project lead's or an "+
			"admin's", len(removedWith), id)
	}
	// THE ROOT FIRST: a child restored under a still-removed parent is
	// reachable from nothing until the parent follows. A root already back
	// is not a refusal, being what a part-way restore leaves; only one with
	// nothing to bring back, and no earlier copy of this operation that
	// brought it back, is [ErrNothingToRestore].
	var live bool
	result, err := w.clearTombstone(ctx, opID, id, project, notify, &live)
	if err = resolved(fmt.Sprintf("task %s's restore", id), result, err); err != nil {
		return result, err
	}
	if live && len(removedWith) == 0 {
		// Said beside the task on a screen, so it carries no id.
		return result, fmt.Errorf("%w: this task isn't in the trash, and nothing "+
			"went in with it", ErrNothingToRestore)
	}
	for i, descendant := range removedWith {
		var already bool
		restore, err := w.clearTombstone(ctx, stepID(opID, "d/"+descendant.ID),
			descendant.ID, descendant.Project, nil, &already)
		if err = resolved(fmt.Sprintf("task %s's restore with %s",
			descendant.ID, id), restore, err); err != nil {
			return result, &SubtreeStopped{Verb: "restore", Root: id,
				Followed: i, Of: len(removedWith), OpID: opID, Err: err}
		}
	}
	return result, nil
}

// tombstone publishes one task's removal.
func (w *Writer) tombstone(ctx context.Context, opID, id, project string,
	removal Tombstone, notify *Notify) (WriteResult, error) {

	subject := TaskSubject(id)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, missingTask(ctx, tx, id,
					"there is nothing left to remove")
			}
			// THE PROJECT BEFORE THE NO-OP — see [stillIn].
			if moved := stillIn(current, project); moved != nil {
				return statelog.Decision{}, moved
			}
			if current.Removed != nil {
				// ALREADY IN THE TRASH IS A SUCCESS, not a conflict: a
				// half-finished removal's re-run must complete.
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(ctx, tx, stamp, subject, OpTombstone, ChangeRemoved, scope,
				opID, TaskPatch{Removed: &removal}, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// clearTombstone publishes one task's restore. A ZERO TOMBSTONE spells "clear
// it", since an absent patch field means "leave it alone" ([applyPatch]).
//
// live reports whether the LAST decision found the task already out of the
// trash: captured, not returned, because the decision may run several rounds
// and only the answering one counts. It stays false where the ledger answered
// the step (an earlier copy of this operation restored it).
func (w *Writer) clearTombstone(ctx context.Context, opID, id, project string,
	notify *Notify, live *bool) (WriteResult, error) {

	subject := TaskSubject(id)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readTask(ctx, tx, id)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, missingTask(ctx, tx, id,
					"nothing can restore it")
			}
			if moved := stillIn(current, project); moved != nil {
				return statelog.Decision{}, moved
			}
			if current.Removed == nil {
				// NOT IN THE TRASH IS NOTHING TO DO, on the removal's
				// own rule: a re-run must be able to finish.
				*live = true
				return statelog.Decision{}, nil
			}
			*live = false
			decision, err := w.decide(ctx, tx, stamp, subject, OpRestore, ChangeRestored, scope,
				opID, TaskPatch{Removed: &Tombstone{}}, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// stillIn refuses a write whose authority was decided on a project the task is
// no longer filed under: an edit, a removal, a restore or a purge. It is the ONE
// such refusal, checked inside the snapshot BEFORE the write's idempotent no-op.
//
// Every caller read the project off the row outside this transaction, decided
// on it who may write and formed the write's SCOPE from it, so for a task moved
// since ([Writer.MoveTaskToProject]) one lead's authority would land on another
// project's work, under deferrals of a container it is not in. A CONFLICT: the
// caller reads again, decides on the project it is in now, and re-sends; it also
// stops a second walk of one move re-keying a descendant. Never after the no-op,
// which answers success; a same-operation re-run is answered by its ledger row.
func stillIn(current Task, project string) error {
	if current.Project == project {
		return nil
	}
	return fmt.Errorf("worklet: task %s (%s) is in project %s, not %s where "+
		"it was read — what this write may do was decided on a project the "+
		"task is not in, so read it again and decide on %s: %w",
		current.Key, current.ID, current.Project, project, current.Project,
		statelog.ErrConflict)
}

// subtreeOf reads a task's descendants before the first append, OUTSIDE the
// snapshot like every walking sequence's own read ([Writer.db]): each tombstone
// decides inside its own, and a descendant arriving after this read is left
// live, the safe direction, which a re-run fixes.
func (w *Writer) subtreeOf(ctx context.Context, id string) ([]Task, error) {
	if w.db.IsZero() {
		return nil, fmt.Errorf("worklet: this writer has no store, so it "+
			"cannot read task %s's subtree; a subtree removal needs one", id)
	}
	var out []Task
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = readSubtree(ctx, tx, id)
		return err
	}); err != nil {
		return nil, fmt.Errorf("worklet: read %s's subtree: %w", id, err)
	}
	return out, nil
}

// removedWithStatement is [Writer.removedWith]'s one statement, named for the
// plan test. THE PARTIAL PREDICATE IS SPELLED OUT: the planner does not infer
// `tracker_tasks_removed_with_idx`'s `removed_with IS NOT NULL` from
// `removed_with = ?`, so without the term the restore cannot use the index and
// reads every task from the heap.
const removedWithStatement = `
		SELECT id, project_key FROM tracker_tasks
		WHERE removed_with IS NOT NULL AND removed_with = ?
		  AND removed_at IS NOT NULL
		ORDER BY depth, id`

// removedWith is every task whose tombstone names this one as the gesture that
// removed it, never one already in the trash for its own reasons.
func (w *Writer) removedWith(ctx context.Context, id string) ([]Task, error) {
	if w.db.IsZero() {
		return nil, nil
	}
	var out []Task
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, removedWithStatement, id)
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
		return nil, fmt.Errorf("worklet: read what was removed with %s: %w", id, err)
	}
	return out, nil
}
