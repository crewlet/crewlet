package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The subtask closure, and why it is rebuilt with a VISITED SET rather than a
// depth counter.
//
// Two concurrent re-parents on two nodes can form a shape no single write
// could see: A under B on one node and B under A on the other, both accepted
// by the broker because they are writes to two different subjects. The log
// then carries both, every node applies both, and the parent chain is a cycle.
//
// A depth-limited walk would loop until the limit and then produce a closure
// that is silently wrong. A visited set TERMINATES and reports what it found —
// so the applier applies the record completely, raises the flag, and the repair
// duty writes a real record to fix it. That is the rule the whole applier is
// written to: a structural impossibility raises an attention flag rather than
// stalling the log, because a stalled log is every node stopping over one
// task's shape.

// maintainClosure rebuilds the ancestry rows for a task and everything under
// it.
//
// THE WHOLE SUBTREE, because a re-parent moves every descendant's ancestry —
// and it is bounded by the descendant cap, which is what makes "rebuild it
// all" affordable rather than clever.
func (a *Applier) maintainClosure(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	subtree, err := descendantsOf(ctx, tx, task.ID)
	if err != nil {
		return 0, err
	}
	// The task itself first, so its own ancestry is right before anything
	// below it is derived from it.
	written, err := a.rebuildAncestry(ctx, tx, task.ID, task.Parent)
	if err != nil {
		return 0, err
	}
	for _, id := range subtree {
		parent, err := parentOf(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		n, err := a.rebuildAncestry(ctx, tx, id, parent)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// rebuildAncestry writes one task's closure rows and its derived columns: the
// root, the depth, and the three flags the walk decides — a cycle, a subtree
// deeper than [MaxDepth], and a task in another project than its root's.
func (a *Applier) rebuildAncestry(ctx context.Context, tx *sql.Tx, id string,
	parent *string) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_task_closure WHERE descendant_id = ?`, id); err != nil {
		return 0, fmt.Errorf("tracker: clear the ancestry of %s: %w", id, err)
	}
	// A task is its own ancestor at distance zero, which is what lets a
	// subtree query be one join rather than a union with the root.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_task_closure (ancestor_id, descendant_id, distance)
		VALUES (?,?,0)
		ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`, id, id); err != nil {
		return 0, fmt.Errorf("tracker: write the self-ancestry of %s: %w", id, err)
	}
	written := 1

	visited := map[string]bool{id: true}
	root, depth, cycle := id, 0, false
	for at, distance := parent, 1; at != nil && *at != ""; distance++ {
		if visited[*at] {
			// THE CYCLE. The walk stops, the flag is raised, and the
			// record still applies completely — a task in a cycle is
			// visible and repairable, where a stalled log is not.
			cycle = true
			break
		}
		visited[*at] = true
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_task_closure (ancestor_id, descendant_id, distance)
			VALUES (?,?,?)
			ON CONFLICT (ancestor_id, descendant_id) DO UPDATE SET
				distance = excluded.distance`, *at, id, distance); err != nil {
			return 0, fmt.Errorf("tracker: write the ancestry of %s: %w", id, err)
		}
		written++
		root, depth = *at, distance
		next, err := parentOf(ctx, tx, *at)
		if err != nil {
			return 0, err
		}
		at = next
	}

	inconsistent, err := outsideRootsProject(ctx, tx, id, root)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tracker_tasks SET root_id = ?, depth = ?, cycle = ?, too_deep = ?,
			inconsistent_project = ?
		WHERE id = ?`,
		root, depth, boolInt(cycle), boolInt(depth > MaxDepth),
		boolInt(inconsistent), id); err != nil {
		return 0, fmt.Errorf("tracker: stamp the ancestry of %s: %w", id, err)
	}
	return written + 1, nil
}

// outsideRootsProject reports a task filed in another project than its root —
// the `inconsistent_project` attention flag.
//
// A subtree lives in one project: a cross-project move carries every task
// beneath the root, and a board keeps a subtree's rows to the project it is
// drawing on the assumption that it does. So a subtask elsewhere is not drawn
// under its root — which is what a move that stopped part-way leaves until it
// is finished, and what a merge re-parenting a subtask onto a task in another
// project leaves until somebody moves it. The column, its filter and the
// attention index all shipped with the schema and nothing ever set it, so the
// attention queue answered `flag=inconsistent_project` with nothing on every
// company.
//
// DERIVED HERE, where the root is already known, and so on every apply that
// can move either end: a task's own apply rebuilds its ancestry, and a root's
// apply rebuilds every descendant's ([Applier.maintainClosure]) — so a root
// moved to another project flags each descendant until its own move clears it.
// A root this node does not have answers false, for the reason [parentOf]
// treats it as absent.
func outsideRootsProject(ctx context.Context, tx *sql.Tx, id, root string) (bool, error) {
	if root == id {
		return false, nil
	}
	var differ int
	err := tx.QueryRowContext(ctx, `
		SELECT t.project_key <> r.project_key
		FROM tracker_tasks t JOIN tracker_tasks r ON r.id = ?
		WHERE t.id = ?`, root, id).Scan(&differ)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("tracker: compare the project of %s with its "+
			"root %s's: %w", id, root, err)
	}
	return differ == 1, nil
}

// descendantsOf reads a task's existing subtree, deepest last.
//
// FROM THE CLOSURE rather than by walking children, because the closure is
// what the previous apply left and is therefore the set whose ancestry this
// one has to redo — walking the parent pointers would find the NEW shape and
// miss whatever the move detached.
func descendantsOf(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT descendant_id FROM tracker_task_closure
		WHERE ancestor_id = ? AND descendant_id <> ?
		ORDER BY distance
		LIMIT ?`, id, id, MaxDescendants)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the subtree of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("tracker: read a descendant of %s: %w", id, err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: walk the subtree of %s: %w", id, err)
	}
	return out, nil
}

// parentOf reads one task's parent, answering nil for a root and for a task
// this node does not have.
//
// A MISSING TASK IS A ROOT rather than an error: under a strict replay the
// parent's create is below this position, so an absent one can only be a
// record a gate dropped — and refusing here would stall the log over a task
// the fleet deliberately removed.
func parentOf(ctx context.Context, tx *sql.Tx, id string) (*string, error) {
	var parent sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT parent_id FROM tracker_tasks WHERE id = ?`, id).Scan(&parent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("tracker: read the parent of %s: %w", id, err)
	case !parent.Valid || parent.String == "":
		return nil, nil
	}
	value := parent.String
	return &value, nil
}
