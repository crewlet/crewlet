package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The derived columns: values the applier COMPUTES from the rows it already
// holds rather than copies out of a record.
//
// One today, `reopens`, and the rule it follows is [statelog.Deriver]'s. The
// column is maintained INCREMENTALLY as each task history row is written, off
// the status delta that row records, and REDERIVED from those history rows by
// [Applier.Rederive] the first time a build whose rules differ from the
// checkpoint's boots — in this Go, never a second copy in a migration's SQL.
// "Backfill equals replay" is therefore true by construction rather than by
// two implementations agreeing: both read one delta per applied status change
// through the one predicate, [reopened].

// DerivationVersion is the rule set this build derives its columns at.
//
// 1 is `reopens`. Bumped by ANY change to what a derived column holds, the
// first one a column adds included — the bump is what fills a new column on a
// node upgrading onto rows its predecessor wrote without it.
const DerivationVersion = 1

var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// reopened is THE rule: a status change that takes a task out of a finished
// group (done or closed) into an unfinished one. Done → closed is not a
// reopen — the work stayed finished — and neither is todo → in progress,
// which never finished at all.
//
// BY GROUP, from the fixed status table, for the reason [stampFinish] cuts on
// the group: a company that adds `shipped` or renames `done` must not change
// what a reopen is. An unknown slug is in no group and so is never finished,
// which reads a status this build does not know as unfinished on BOTH sides —
// the same answer the history row's delta gives it on every node.
func reopened(from, to Status) bool {
	return from.Group().Finished() && !to.Group().Finished()
}

// countReopen adds one to a task's reopen counter when the history row this
// apply has just written records a reopen.
//
// OFF THE ROW'S OWN `fields_json`, the exact bytes [Applier.Rederive] reads
// back — never off the two documents again. The history row is the one record
// of a status change both halves can see, and it is written in cases the
// object row is not (a record reprocessed below an applied successor writes
// its history row and skips the object), so a rule keyed anywhere else would
// count on one side what the other never sees.
func countReopen(ctx context.Context, tx *sql.Tx, subject Subject, fields string) (int, error) {
	if subject.Kind != KindTask {
		return 0, nil
	}
	var moved struct {
		Status *Delta `json:"status"`
	}
	if err := json.Unmarshal([]byte(fields), &moved); err != nil {
		return 0, fmt.Errorf("tracker: read the deltas of task %s for a reopen: %w",
			subject.ID, err)
	}
	if moved.Status == nil || !reopened(Status(moved.Status.From), Status(moved.Status.To)) {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tracker_tasks SET reopens = reopens + 1 WHERE id = ?`, subject.ID); err != nil {
		return 0, fmt.Errorf("tracker: count the reopen of task %s: %w", subject.ID, err)
	}
	return 1, nil
}

// Rederive implements [statelog.Deriver]: every task's `reopens` recomputed
// from the status deltas in its history rows.
//
// ORDER-FREE, because a count of transitions does not depend on the order it
// is taken in — each history row records one change with both of its ends, so
// the walk needs no sort and no per-task state. The rows are read whole and
// the counters written after, because a statement cannot be issued on a
// transaction while a query on it is still being read.
//
// A PURGED task has no row left to write, and its history rows are skipped
// with it: the UPDATE of an id with no row affects nothing.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx, _ statelog.ApplyOptions) (int, error) {
	counts, err := reopensFromHistory(ctx, tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tracker_tasks SET reopens = 0 WHERE reopens <> 0`)
	if err != nil {
		return 0, fmt.Errorf("tracker: clear the reopen counters to re-derive them: %w", err)
	}
	cleared, err := affected(res)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	// SORTED so the writes are issued in one order on every node; the
	// result does not depend on it, but a re-derivation that is
	// deterministic down to its statements is one a failure reproduces.
	slices.Sort(ids)
	written := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET reopens = ? WHERE id = ?`, counts[id], id)
		if err != nil {
			return 0, fmt.Errorf("tracker: write the re-derived reopens of task %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return max(cleared, written), nil
}

// reopensFromHistory counts each task's reopens over its history rows.
func reopensFromHistory(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT subject_id, json_extract(fields_json, '$.status')
		FROM tracker_history
		WHERE subject_kind = ? AND json_extract(fields_json, '$.status') IS NOT NULL`,
		string(KindTask))
	if err != nil {
		return nil, fmt.Errorf("tracker: read the status history to re-derive reopens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("tracker: scan a status history row: %w", err)
		}
		var status Delta
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, fmt.Errorf("tracker: decode the status delta of task %s: %w", id, err)
		}
		if reopened(Status(status.From), Status(status.To)) {
			counts[id]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the status history to re-derive reopens: %w", err)
	}
	return counts, nil
}
