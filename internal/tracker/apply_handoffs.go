package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// The hand-off count a task history row carries: `tracker_history.reassignments`,
// the task's counter as that commit left it (migration 0022).
//
// # The rule, stated once
//
// A record STATES the counter when its payload carries `reassignments` — the
// value [Writer.chargeHandOff] decided inside the writer's snapshot, which a
// create carries as part of the whole task and a patch carries whenever it
// moves the counter, a person's reset to zero included. A record that states
// nothing leaves it where the row before it left it, and a task with no row
// before starts at zero. So a row's value is a function of the task's history
// IN LOG ORDER and of nothing else, which is what makes it identical on every
// node however it got there.
//
// # Why from the history rather than the task row
//
// The task row holds the counter NOW, and it is written on a different
// condition: a record reprocessed below an applied successor writes its
// history row and skips the object row, so copying the object's value there
// would stamp the successor's count onto the earlier commit. The history rows
// are the one record of the sequence both halves see, so the incremental rule
// and [rederiveHandOffs] read the same rows through the same function,
// [handOffsCarried], and "backfill equals replay" holds by construction.

// statedHandOffs is the counter a history row's record states, and false when
// it states none.
//
// A DOCUMENT THAT DOES NOT DECODE STATES NOTHING rather than failing the
// apply: the row's own record has already been applied by the time this runs,
// so its payload decoded once, and the column is a rendering fact that must
// never be the reason a log stops.
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

// deriveHandOffs brings the rows of one task at and after `from` to the rule.
//
// FROM THE WRITTEN ROW FORWARD rather than for that row alone, because an
// ordinary apply appends at the head — one row, one seek — while a record
// reprocessed below an applied successor lands BEHIND rows that were derived
// without it, and every one of them carries forward from it.
func deriveHandOffs(ctx context.Context, tx *sql.Tx, taskID string, from int64) (int, error) {
	carried := 0
	var before sql.NullInt64
	switch err := tx.QueryRowContext(ctx, `
		SELECT reassignments FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND log_seq < ?
		ORDER BY log_seq DESC LIMIT 1`, taskID, string(KindTask), from).Scan(&before); {
	case err == nil:
		carried = int(before.Int64)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("tracker: read the hand-off count before %s's commit: %w", taskID, err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, document, reassignments FROM tracker_history
		WHERE subject_id = ? AND subject_kind = ? AND log_seq >= ?
		ORDER BY log_seq`, taskID, string(KindTask), from)
	if err != nil {
		return 0, fmt.Errorf("tracker: read %s's history to count its hand-offs: %w", taskID, err)
	}
	walked, err := scanHandOffRows(rows)
	if err != nil {
		return 0, fmt.Errorf("tracker: read %s's history to count its hand-offs: %w", taskID, err)
	}
	return writeHandOffs(ctx, tx, handOffsCarried(carried, walked))
}

// rederiveHandOffs is every task history row's hand-off count recomputed from
// the whole history, for [Applier.Rederive].
//
// Task by task in log order, each starting from zero — a task's first row is
// its create, which states the whole task. The reads finish before any write,
// for the reason [rederiveReopens] gives.
func rederiveHandOffs(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT subject_id, id, document, reassignments FROM tracker_history
		WHERE subject_kind = ?
		ORDER BY subject_id, log_seq`, string(KindTask))
	if err != nil {
		return 0, fmt.Errorf("tracker: read the task history to re-derive hand-off counts: %w", err)
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
			return 0, fmt.Errorf("tracker: scan a task history row: %w", err)
		}
		if subject != task {
			flush()
			task = subject
		}
		// ONLY WHAT THE WALK NEEDS is kept: a row whose record states
		// nothing contributes its id and stored value, never its bytes.
		if _, ok := statedHandOffs(row.document); !ok {
			row.document = nil
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("tracker: read the task history to re-derive hand-off counts: %w", err)
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

// writeHandOffs writes the counts that moved, in id order so a re-derivation
// issues the same statements on every node.
func writeHandOffs(ctx context.Context, tx *sql.Tx, moved map[string]int) (int, error) {
	written := 0
	for _, id := range slices.Sorted(maps.Keys(moved)) {
		res, err := tx.ExecContext(ctx,
			`UPDATE tracker_history SET reassignments = ? WHERE id = ?`, moved[id], id)
		if err != nil {
			return 0, fmt.Errorf("tracker: write the hand-off count of history row %s: %w", id, err)
		}
		n, err := affected(res)
		if err != nil {
			return 0, err
		}
		written += n
	}
	return written, nil
}
