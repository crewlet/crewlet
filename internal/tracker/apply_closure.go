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
// so the applier applies the record completely and raises the flag, which puts
// the task in the attention queue (`flag=cycle`) for somebody to break. That
// is the rule the whole applier is written to: a structural impossibility
// raises an attention flag rather than stalling the log, because a stalled log
// is every node stopping over one task's shape.
//
// # And the project a subtree lives in is derived here too
//
// A subtree lives in its root's project — a cross-project move carries every
// descendant with it — but the move is one append per task, so a walk that
// stops part-way, or a subtask filed under the subtree while it runs, leaves a
// task in a project its root is not in. From [ProjectFlagRecordVersion] the
// closure pass derives that as `inconsistent_project`, because this is the one
// place that walks a task's ancestry and its whole subtree together: a root
// that changes project re-derives every task under it, and a descendant that
// arrives re-derives itself. The tracker duty moves what the flag marks
// ([duty.finishSplits]).

// maintainClosure rebuilds the ancestry rows for a task and everything under
// it.
//
// THE WHOLE SUBTREE, because a re-parent moves every descendant's ancestry,
// and whole means whole. [MaxDescendants] does not bound this walk, because
// nothing enforces that cap on the path a subtree actually GROWS: only a move
// and a removal check it — a create under a parent never does, and [MaxDepth]
// beside it is likewise a derived FLAG rather than a refusal, which is the
// design.
//
// So a walk cut at the cap would leave the tail it cut with its old
// `root_id`, `depth`, `cycle` and `too_deep` for ever, since nothing revisits
// a task whose own record did not change — and every node would compute the
// same wrong answer identically, so nothing could notice: a `too_deep` flag
// derived from a depth nobody updated, and a re-parent that left half a
// subtree filed under the root it came from.
//
// A SHORT READ IN AN APPLIER IS NOT A SHORT ANSWER, it is durable wrong state
// replicated to the fleet — which is the one place this package's own rule
// against a silent cut has to hold hardest. The walk is over what the closure
// actually holds, so its cost is the subtree's real size and its output is
// correct at any size; the caps that bound what a single gesture carries stay
// where they are, on the move and the removal.
//
// derive is whether the record being applied derives `inconsistent_project`
// ([ProjectFlagRecordVersion]); every record rebuilds the ancestry.
func (a *Applier) maintainClosure(ctx context.Context, tx *sql.Tx, task Task,
	derive bool) (int, error) {

	subtree, err := descendantsOf(ctx, tx, task.ID)
	if err != nil {
		return 0, err
	}
	// The task itself first, so its own ancestry is right before anything
	// below it is derived from it.
	written, err := a.rebuildAncestry(ctx, tx, task.ID, task.Parent, derive)
	if err != nil {
		return 0, err
	}
	for _, id := range subtree {
		parent, err := parentOf(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		n, err := a.rebuildAncestry(ctx, tx, id, parent, derive)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}

// rebuildAncestry writes one task's closure rows and its derived columns —
// `inconsistent_project` among them when derive is set.
func (a *Applier) rebuildAncestry(ctx context.Context, tx *sql.Tx, id string,
	parent *string, derive bool) (int, error) {

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

	if !derive {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tracker_tasks SET root_id = ?, depth = ?, cycle = ?, too_deep = ?
			WHERE id = ?`,
			root, depth, boolInt(cycle), boolInt(depth > MaxDepth), id); err != nil {
			return 0, fmt.Errorf("tracker: stamp the ancestry of %s: %w", id, err)
		}
		return written + 1, nil
	}
	// THE ROOT'S PROJECT, READ IN THIS STATEMENT, so the comparison is
	// against the row as this position leaves it — the root's own record may
	// be the one being applied. A root compares with nothing, a task in a
	// cycle has no root to compare with, and a root this node does not hold
	// ([parentOf]'s gated-away parent) leaves nothing to compare: each is
	// clear.
	if _, err := tx.ExecContext(ctx, `
		UPDATE tracker_tasks SET root_id = ?, depth = ?, cycle = ?, too_deep = ?,
			inconsistent_project = COALESCE((
				SELECT r.project_key <> tracker_tasks.project_key
				FROM tracker_tasks r
				WHERE r.id = ? AND r.id <> tracker_tasks.id AND ? = 0), 0)
		WHERE id = ?`,
		root, depth, boolInt(cycle), boolInt(depth > MaxDepth), root,
		boolInt(cycle), id); err != nil {
		return 0, fmt.Errorf("tracker: stamp the ancestry of %s: %w", id, err)
	}
	return written + 1, nil
}

// descendantsOf reads a task's existing subtree, deepest last.
//
// FROM THE CLOSURE rather than by walking children, because the closure is
// what the previous apply left and is therefore the set whose ancestry this
// one has to redo — walking the parent pointers would find the NEW shape and
// miss whatever the move detached.
//
// UNBOUNDED, deliberately: see [Applier.maintainClosure] for what a bound here
// would cost. The rows are one indexed range over a table the applier is
// already writing a row per member of, so the read is not what decides this
// walk's cost.
func descendantsOf(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT descendant_id FROM tracker_task_closure
		WHERE ancestor_id = ? AND descendant_id <> ?
		ORDER BY distance`, id, id)
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
