package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The subtask closure is walked with a VISITED SET, not a depth counter.
//
// Two concurrent re-parents (A under B, B under A) are writes to different
// subjects, so the broker accepts both and the parent chain is a cycle. A
// depth-limited walk would yield a silently wrong closure; a visited set
// terminates, the record applies completely, and an attention flag lets the
// repair duty write the fix. A structural impossibility raises a flag rather
// than stalling the log, which would stop every node over one task's shape.

// maintainClosure rebuilds the ancestry rows for a task and everything under
// it.
//
// THE WHOLE SUBTREE, since a re-parent moves every descendant's ancestry;
// the descendant cap keeps rebuilding it all affordable.
func (a *Applier) maintainClosure(ctx context.Context, tx *sql.Tx, task Task) (int, error) {
	subtree, err := descendantsOf(ctx, tx, task.ID)
	if err != nil {
		return 0, err
	}
	// The task first: everything below derives from its ancestry.
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

// ancestryStampStatement is [Applier.rebuildAncestry]'s write of the columns
// its walk derived, named for the plan test.
const ancestryStampStatement = `
		UPDATE tracker_tasks SET root_id = ?, depth = ?, cycle = ?, too_deep = ?,
			inconsistent_project = ?
		WHERE id = ?`

// rebuildAncestry writes one task's closure rows and its derived columns: the
// root, the depth, and the three flags the walk decides — a cycle, a subtree
// deeper than [MaxDepth], and a task in another project than its root's.
func (a *Applier) rebuildAncestry(ctx context.Context, tx *sql.Tx, id string,
	parent *string) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_task_closure WHERE descendant_id = ?`, id); err != nil {
		return 0, fmt.Errorf("worklet: clear the ancestry of %s: %w", id, err)
	}
	// Its own ancestor at distance zero, so a subtree query is one join
	// rather than a union with the root.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_task_closure (ancestor_id, descendant_id, distance)
		VALUES (?,?,0)
		ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`, id, id); err != nil {
		return 0, fmt.Errorf("worklet: write the self-ancestry of %s: %w", id, err)
	}
	written := 1

	visited := map[string]bool{id: true}
	root, depth, cycle := id, 0, false
	for at, distance := parent, 1; at != nil && *at != ""; distance++ {
		if visited[*at] {
			// THE CYCLE: flag it and apply the record completely.
			cycle = true
			break
		}
		visited[*at] = true
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tracker_task_closure (ancestor_id, descendant_id, distance)
			VALUES (?,?,?)
			ON CONFLICT (ancestor_id, descendant_id) DO UPDATE SET
				distance = excluded.distance`, *at, id, distance); err != nil {
			return 0, fmt.Errorf("worklet: write the ancestry of %s: %w", id, err)
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
	if _, err := tx.ExecContext(ctx, ancestryStampStatement,
		root, depth, boolInt(cycle), boolInt(depth > MaxDepth),
		boolInt(inconsistent), id); err != nil {
		return 0, fmt.Errorf("worklet: stamp the ancestry of %s: %w", id, err)
	}
	return written + 1, nil
}

// rootsProjectStatement is [outsideRootsProject]'s comparison, named for the
// plan test.
const rootsProjectStatement = `
		SELECT t.project_key <> r.project_key
		FROM tracker_tasks t JOIN tracker_tasks r ON r.id = ?
		WHERE t.id = ?`

// outsideRootsProject reports a task filed in another project than its root —
// the `inconsistent_project` attention flag.
//
// A subtree lives in one project (a board draws it so); a subtask elsewhere is
// a cross-project move stopped part-way. The writer refuses every other way in
// ([refuseParent], [ErrReparentAcrossProjects]), and a record carrying the
// shape anyway gets a flag, not a refusal that stalls every node.
//
// DERIVED HERE, where the root is known: a root's apply rebuilds every
// descendant ([Applier.maintainClosure]), flagging each until its own move
// clears it. A root this node lacks answers false, for [parentOf]'s reason.
func outsideRootsProject(ctx context.Context, tx *sql.Tx, id, root string) (bool, error) {
	if root == id {
		return false, nil
	}
	var differ int
	err := tx.QueryRowContext(ctx, rootsProjectStatement, root, id).Scan(&differ)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("worklet: compare the project of %s with its "+
			"root %s's: %w", id, root, err)
	}
	return differ == 1, nil
}

// descendantsOf reads a task's existing subtree, deepest last.
//
// FROM THE CLOSURE, what the previous apply left: walking parent pointers
// would find the NEW shape and miss whatever the move detached.
func descendantsOf(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT descendant_id FROM tracker_task_closure
		WHERE ancestor_id = ? AND descendant_id <> ?
		ORDER BY distance
		LIMIT ?`, id, id, MaxDescendants)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the subtree of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("worklet: read a descendant of %s: %w", id, err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: walk the subtree of %s: %w", id, err)
	}
	return out, nil
}

// parentStatement is [parentOf]'s read, named for the plan test.
const parentStatement = `SELECT parent_id FROM tracker_tasks WHERE id = ?`

// parentOf reads one task's parent, answering nil for a root and for a task
// this node does not have.
//
// A MISSING TASK IS A ROOT, not an error: under a strict replay it can only be
// one a gate dropped, and refusing would stall the log over a task the fleet
// removed on purpose.
func parentOf(ctx context.Context, tx *sql.Tx, id string) (*string, error) {
	var parent sql.NullString
	err := tx.QueryRowContext(ctx, parentStatement, id).Scan(&parent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("worklet: read the parent of %s: %w", id, err)
	case !parent.Valid || parent.String == "":
		return nil, nil
	}
	value := parent.String
	return &value, nil
}
