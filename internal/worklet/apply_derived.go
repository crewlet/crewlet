package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The derived columns, COMPUTED from rows the applier holds, are maintained
// INCREMENTALLY by the apply and REDERIVED by [Applier.Rederive] in this Go
// (never a migration's SQL) when the checkpoint's rule set differs
// ([statelog.Deriver]). Each pair shares one rule, so backfill equals replay:
//
//   - a task's `reopens`: [countReopen] / [rederiveReopens], via [reopened];
//   - a project's `active_count`: [Applier.maintainProjectCounts] / [rederiveActiveCounts];
//   - a history row's `reassignments`: [deriveHandOffs] / [rederiveHandOffs];
//   - a task's `updated_at`, its newest history `effective_at`:
//     [Applier.applyTask] / [restampUpdated];
//   - a question's `answered_by`: [writeComment] / [rederiveAnswers];
//   - an open question's `overtaken`: [stampOvertaken] for one task / all.

// DerivationVersion is the rule set this build derives its columns at.
//
// Bumped by ANY change to what a derived column holds, a new column included:
// the bump is what re-derives every data node's rows to this rule set.
const DerivationVersion = 2

var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// reopened is THE rule: a status change that takes a task out of a finished
// group (done or closed) into an unfinished one. Done → closed is not a
// reopen — the work stayed finished — and neither is todo → in progress,
// which never finished at all.
//
// BY GROUP, for [stampFinish]'s reason: adding `shipped` or renaming `done`
// must not change what a reopen is. An unknown slug is in no group, so it
// reads as unfinished on both sides, on every data node alike.
func reopened(from, to Status) bool {
	return from.Group().Finished() && !to.Group().Finished()
}

// countReopenStatement is [countReopen]'s write, named for the plan test.
const countReopenStatement = `UPDATE tracker_tasks SET reopens = reopens + 1 WHERE id = ?`

// countReopen adds one to a task's reopen counter when the history row this
// apply has just written records a reopen.
//
// OFF THE ROW'S `fields_json`, the bytes [Applier.Rederive] reads back, never
// the documents: a record reprocessed below an applied successor writes its
// history row and skips the object, so only the history row is seen by both.
func countReopen(ctx context.Context, tx *sql.Tx, subject Subject, fields string) (int, error) {
	if subject.Kind != KindTask {
		return 0, nil
	}
	var moved struct {
		Status *Delta `json:"status"`
	}
	if err := json.Unmarshal([]byte(fields), &moved); err != nil {
		return 0, fmt.Errorf("worklet: read the deltas of task %s for a reopen: %w",
			subject.ID, err)
	}
	if moved.Status == nil || !reopened(Status(moved.Status.From), Status(moved.Status.To)) {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, countReopenStatement, subject.ID); err != nil {
		return 0, fmt.Errorf("worklet: count the reopen of task %s: %w", subject.ID, err)
	}
	return 1, nil
}

// Rederive implements [statelog.Deriver]: every derived column recomputed from
// the rows this transaction holds.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx, _ statelog.ApplyOptions) (int, error) {
	reopens, err := rederiveReopens(ctx, tx)
	if err != nil {
		return 0, err
	}
	active, err := rederiveActiveCounts(ctx, tx)
	if err != nil {
		return 0, err
	}
	handOffs, err := rederiveHandOffs(ctx, tx)
	if err != nil {
		return 0, err
	}
	updated, err := restampUpdated(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	answers, err := rederiveAnswers(ctx, tx)
	if err != nil {
		return 0, err
	}
	overtaken, err := stampOvertaken(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	return reopens + active + handOffs + updated + answers + overtaken, nil
}

// restampStatement is [restampUpdated]'s read for one task, or every task when
// id is empty, named for the plan test. The one task's filter sits INSIDE the
// aggregate, so it seeks the subject index rather than grouping all history.
func restampStatement(id string) (string, []any) {
	scope, args := "", []any{string(KindTask)}
	if id != "" {
		scope, args = " AND subject_id = ?", append(args, id)
	}
	return `
		SELECT t.id, t.document, h.newest
		FROM tracker_tasks t
		JOIN (SELECT subject_id, MAX(effective_at) AS newest
		        FROM tracker_history WHERE subject_kind = ?` + scope + `
		       GROUP BY subject_id) h ON h.subject_id = t.id
		WHERE h.newest > 0 AND t.updated_at <> h.newest`, args
}

// restampUpdatedStatement is [restampUpdated]'s write of one task's last
// change, named for the plan test.
const restampUpdatedStatement = `UPDATE tracker_tasks SET updated_at = ?, document = ? WHERE id = ?`

// restampUpdated recomputes `updated_at` as the newest `effective_at` among a
// task's history rows: every task's when id is empty, else the one task's.
//
// THE SAME INSTANT [Applier.applyTask] stamps (its history row carries it), so
// a maintained row re-derives to itself; only rows that differ are written.
// ONE TASK serves a late record: it changes no document, yet its history row
// raises every successor's effective instant ([Applier.raiseSuccessors]), so
// the task is restamped to match a node that applied in log order.
//
// THE DOCUMENT MOVES WITH THE COLUMN (a page reads one, a list the other), in
// Go, since the document holds RFC 3339 text and the column an integer.
func restampUpdated(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	query, args := restampStatement(id)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("worklet: read the tasks whose last change moved: %w", err)
	}
	type repair struct {
		id       string
		document []byte
		newest   int64
	}
	var repairs []repair
	for rows.Next() {
		var r repair
		if err := rows.Scan(&r.id, &r.document, &r.newest); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("worklet: read a task's last change: %w", err)
		}
		repairs = append(repairs, r)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("worklet: read the tasks whose last change moved: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("worklet: read the tasks whose last change moved: %w", err)
	}
	for _, r := range repairs {
		var task Task
		if err := json.Unmarshal(r.document, &task); err != nil {
			return 0, fmt.Errorf("worklet: decode task %s to stamp its last change: %w", r.id, err)
		}
		task.UpdatedAt = store.DecodeTime(r.newest).UTC()
		document, err := json.Marshal(task)
		if err != nil {
			return 0, fmt.Errorf("worklet: encode task %s: %w", r.id, err)
		}
		if _, err := tx.ExecContext(ctx, restampUpdatedStatement,
			r.newest, document, r.id); err != nil {
			return 0, fmt.Errorf("worklet: stamp task %s's last change: %w", r.id, err)
		}
	}
	return len(repairs), nil
}

// activeCountsStatement is [rederiveActiveCounts]' recount of every project,
// named for the plan test.
const activeCountsStatement = `UPDATE tracker_projects SET active_count = ` +
	activeRecount + ` WHERE active_count <> ` + activeRecount

// activeRecount is one project's active work, counted from its task rows.
const activeRecount = `(SELECT COUNT(*) FROM tracker_tasks t
		WHERE t.project_key = tracker_projects.key
		  AND t.status_group = ? AND t.removed_at IS NULL)`

// rederiveActiveCounts is every project's `active_count` recounted from its
// task rows.
//
// A COUNT OVER THE ROWS, not a replay: `status_group` and `removed_at` are
// written from the same two facts [activeOf] reads, so the two cannot
// disagree, and a purged task has no row.
//
// ONE STATEMENT WRITING ONLY THE PROJECTS THAT DIFFER, so maintained rows
// rewrite nothing. Projects are tens, each seeking the tasks' project index.
func rederiveActiveCounts(ctx context.Context, tx *sql.Tx) (int, error) {
	res, err := tx.ExecContext(ctx, activeCountsStatement,
		string(GroupActive), string(GroupActive))
	if err != nil {
		return 0, fmt.Errorf("worklet: recount the active work of every project: %w", err)
	}
	return affected(res)
}

// clearReopensStatement and setReopensStatement are [rederiveReopens]' two
// writes, named for the plan test: every counter cleared (a whole-table pass
// by definition), then each task's written by its key.
const (
	clearReopensStatement = `UPDATE tracker_tasks SET reopens = 0 WHERE reopens <> 0`
	setReopensStatement   = `UPDATE tracker_tasks SET reopens = ? WHERE id = ?`
)

// rederiveReopens is every task's `reopens` recomputed from the status deltas
// in its history rows.
//
// ORDER-FREE: each history row holds both ends of one change, so the walk
// needs no sort. The reads finish before any write: a statement cannot be
// issued on a transaction while a query on it is open. A purged task's UPDATE
// affects nothing.
func rederiveReopens(ctx context.Context, tx *sql.Tx) (int, error) {
	counts, err := reopensFromHistory(ctx, tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, clearReopensStatement)
	if err != nil {
		return 0, fmt.Errorf("worklet: clear the reopen counters to re-derive them: %w", err)
	}
	cleared, err := affected(res)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	// SORTED, not for the result: statements deterministic on every node
	// make a failure reproduce.
	slices.Sort(ids)
	written := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, setReopensStatement, counts[id], id)
		if err != nil {
			return 0, fmt.Errorf("worklet: write the re-derived reopens of task %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return max(cleared, written), nil
}

// reopensStatement is [reopensFromHistory]' walk of every task status row,
// named for the plan test.
const reopensStatement = `
		SELECT subject_id, json_extract(fields_json, '$.status')
		FROM tracker_history
		WHERE subject_kind = ? AND json_extract(fields_json, '$.status') IS NOT NULL`

// reopensFromHistory counts each task's reopens over its history rows.
func reopensFromHistory(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, reopensStatement, string(KindTask))
	if err != nil {
		return nil, fmt.Errorf("worklet: read the status history to re-derive reopens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("worklet: scan a status history row: %w", err)
		}
		var status Delta
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, fmt.Errorf("worklet: decode the status delta of task %s: %w", id, err)
		}
		if reopened(Status(status.From), Status(status.To)) {
			counts[id]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read the status history to re-derive reopens: %w", err)
	}
	return counts, nil
}
