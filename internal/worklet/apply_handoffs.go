package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// `tracker_history.reassignments` is the task's hand-off count as that commit
// left it. THE RULE: a record whose payload carries `reassignments` (decided
// by [Writer.chargeHandOff], a reset to zero included) states it; one that
// does not keeps the previous row's, from zero. So a row's value is a function of the task's history in
// log order alone.
//
// FROM THE HISTORY, NOT THE TASK ROW: a record reprocessed below an applied
// successor skips the object row, whose count would be the successor's. The
// incremental rule and [rederiveHandOffs] both read history rows through
// [handOffsCarried], so backfill equals replay by construction.

// statedHandOffs is the counter a history row's record states, and false when
// it states none.
//
// A DOCUMENT THAT DOES NOT DECODE STATES NOTHING: its record already applied,
// and a rendering column must never be why a log stops.
func statedHandOffs(document []byte) (int, bool) {
	if len(document) == 0 {
		return 0, false
	}
	var stated struct {
		Reassignments *int `json:"reassignments"`
	}
	if err := json.Unmarshal(document, &stated); err != nil || stated.Reassignments == nil {
		return 0, false
	}
	return *stated.Reassignments, true
}

// handOffRow is one task history row as the derivation reads it.
type handOffRow struct {
	id       string
	document []byte
	stored   sql.NullInt64
}

// handOffsCarried walks one task's rows IN LOG ORDER from the value the row
// before them holds, and answers every row whose stored value differs from the
// one the rule gives it.
func handOffsCarried(carried int, rows []handOffRow) map[string]int {
	moved := map[string]int{}
	for _, row := range rows {
		if stated, ok := statedHandOffs(row.document); ok {
			carried = stated
		}
		if !row.stored.Valid || row.stored.Int64 != int64(carried) {
			moved[row.id] = carried
		}
	}
	return moved
}

// handOffsBeforeStatement is the count the row before a task's commit left,
// and handOffsFromStatement every row of that task from it on — the two reads
// of [deriveHandOffs], named for the plan test.
const (
	handOffsBeforeStatement = `
		SELECT reassignments FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND log_seq < ?
		ORDER BY log_seq DESC LIMIT 1`
	handOffsFromStatement = `
		SELECT id, document, reassignments FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND log_seq >= ?
		ORDER BY log_seq`
)

// deriveHandOffs brings the rows of one task at and after `from` to the rule.
//
// FROM THE WRITTEN ROW FORWARD: an ordinary apply appends at the head (one
// seek), but a record reprocessed below an applied successor lands behind rows
// derived without it, and each carries forward from it.
func deriveHandOffs(ctx context.Context, tx *sql.Tx, taskID string, from int64) (int, error) {
	carried := 0
	var before sql.NullInt64
	switch err := tx.QueryRowContext(ctx, handOffsBeforeStatement,
		taskID, string(KindTask), from).Scan(&before); {
	case err == nil:
		carried = int(before.Int64)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("worklet: read the hand-off count before %s's commit: %w", taskID, err)
	}
	rows, err := tx.QueryContext(ctx, handOffsFromStatement, taskID, string(KindTask), from)
	if err != nil {
		return 0, fmt.Errorf("worklet: read %s's history to count its hand-offs: %w", taskID, err)
	}
	walked, err := scanHandOffRows(rows)
	if err != nil {
		return 0, fmt.Errorf("worklet: read %s's history to count its hand-offs: %w", taskID, err)
	}
	return writeHandOffs(ctx, tx, handOffsCarried(carried, walked))
}

// rederiveHandOffsStatement is [rederiveHandOffs]' walk of the whole task
// history, named for the plan test.
//
// `subject_id DESC, log_seq` is `tracker_history_subject_idx (subject_id,
// log_seq DESC)` read backwards: each task's rows together in log order, the
// tasks in any order. `subject_id, log_seq`, which no index holds, would sort
// every task history row with its document before returning the first.
const rederiveHandOffsStatement = `
		SELECT subject_id, id, document, reassignments FROM tracker_history
		WHERE subject_kind = ?
		ORDER BY subject_id DESC, log_seq`

// rederiveHandOffs is every task history row's hand-off count recomputed from
// the whole history, for [Applier.Rederive].
//
// Task by task in log order, each from zero (its first row is its create). The
// reads finish before any write, for [rederiveReopens]' reason.
func rederiveHandOffs(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, rederiveHandOffsStatement, string(KindTask))
	if err != nil {
		return 0, fmt.Errorf("worklet: read the task history to re-derive hand-off counts: %w", err)
	}
	moved := map[string]int{}
	var (
		task  string
		batch []handOffRow
	)
	flush := func() {
		for id, n := range handOffsCarried(0, batch) {
			moved[id] = n
		}
		batch = batch[:0]
	}
	for rows.Next() {
		var subject string
		var row handOffRow
		if err := rows.Scan(&subject, &row.id, &row.document, &row.stored); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("worklet: scan a task history row: %w", err)
		}
		if subject != task {
			flush()
			task = subject
		}
		// A row stating nothing keeps its id and stored value, not its bytes.
		if _, ok := statedHandOffs(row.document); !ok {
			row.document = nil
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("worklet: read the task history to re-derive hand-off counts: %w", err)
	}
	_ = rows.Close()
	flush()
	return writeHandOffs(ctx, tx, moved)
}

// scanHandOffRows reads a walk's rows whole and closes them.
func scanHandOffRows(rows *sql.Rows) ([]handOffRow, error) {
	defer func() { _ = rows.Close() }()
	var out []handOffRow
	for rows.Next() {
		var row handOffRow
		if err := rows.Scan(&row.id, &row.document, &row.stored); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// writeHandOffStatement is one row's hand-off count written, by
// [writeHandOffs] and named for the plan test.
const writeHandOffStatement = `UPDATE tracker_history SET reassignments = ? WHERE id = ?`

// writeHandOffs writes the counts that moved, in id order so a re-derivation
// issues the same statements on every node.
func writeHandOffs(ctx context.Context, tx *sql.Tx, moved map[string]int) (int, error) {
	written := 0
	for _, id := range slices.Sorted(maps.Keys(moved)) {
		res, err := tx.ExecContext(ctx, writeHandOffStatement, moved[id], id)
		if err != nil {
			return 0, fmt.Errorf("worklet: write the hand-off count of history row %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}
