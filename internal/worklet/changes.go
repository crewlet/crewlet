package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The changes (pull requests) live tasks wait on, read for the engine's settle
// of one: by webhook as its host tells it, and by the poll of the ones open.

// waitingOnChangeStatement is every task with a `waiting_on` edge to one
// change, a seek of the relations' reverse index.
const waitingOnChangeStatement = `SELECT r.task_id FROM tracker_relations r
	WHERE r.other_id = ? AND r.kind = 'waiting_on' ORDER BY r.task_id`

// openChangesStatement is the changes some task still waits on, in key order
// from after: a range of the dependency key, which a change's URL begins.
const openChangesStatement = `SELECT DISTINCT d.blocker_id FROM tracker_task_deps d
	WHERE d.blocker_id > ? AND d.blocker_id < 'httq' AND d.blocker_open = 1
	  AND d.blocker_id LIKE '%://%'
	ORDER BY d.blocker_id LIMIT ?`

// WaitingOnChange is every live task that waits on change ref and has not
// recorded it settled, read whole.
func (r *Reader) WaitingOnChange(ctx context.Context, ref string, level statelog.ReadLevel) ([]Task, error) {
	if !IsChangeRef(ref) {
		return nil, fmt.Errorf("worklet: %q names no change", ref)
	}
	var out []Task
	_, err := r.log.Read(ctx, statelog.Query{Level: level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(), Set: true,
	}, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, waitingOnChangeStatement, ref)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			task, held, err := readTask(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, settled := task.Changes[ref]; held && task.Removed == nil && !settled {
				out = append(out, task)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the tasks waiting on %s: %w", ref, err)
	}
	return out, nil
}

// OpenChanges is at most limit changes some task still waits on, in order
// after the one named, so a bounded pass resumes where the last one stopped.
func (r *Reader) OpenChanges(ctx context.Context, after string, limit int,
	level statelog.ReadLevel) ([]string, error) {

	if after == "" {
		after = "http"
	}
	var out []string
	_, err := r.log.Read(ctx, statelog.Query{Level: level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(), Set: true,
	}, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, openChangesStatement, after, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return err
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the open changes: %w", err)
	}
	return out, nil
}
