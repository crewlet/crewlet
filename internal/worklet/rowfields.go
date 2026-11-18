package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// openAskSQL is "this comment is a question still waiting for its answer", over
// a `tracker_comments` row aliased `m`: the one spelling the `has_open_asks=`
// filter and the `open_asks` row count share, so a card's count cannot
// disagree with the filter beside it.
const openAskSQL = "m.ask <> '' AND m.resolved = 0 AND m.answered_by IS NULL " +
	"AND m.removed = 0 AND m.overtaken = ''"

// rowFieldBatch bounds how many ids one row-fact statement binds: a grouped
// answer carries up to [MaxGroupsWithSubgroups] × [MaxSubgroups] ×
// [GroupRowsMax] rows, too many for one `IN (…)`, and the page ceiling is
// already the most rows one flat answer reads at once.
const rowFieldBatch = PageMax

// answerRows is every row an answer carries — the flat page, or every
// column's and every lane's slice — as pointers the row facts are filled
// through.
func answerRows(answer *Answer) []*TaskRow {
	var out []*TaskRow
	for i := range answer.Rows {
		out = append(out, &answer.Rows[i])
	}
	var walk func(groups []Group)
	walk = func(groups []Group) {
		for i := range groups {
			for j := range groups[i].Rows {
				out = append(out, &groups[i].Rows[j])
			}
			walk(groups[i].Subgroups)
		}
	}
	walk(answer.Groups)
	return out
}

// dependentsCountStatement is [fillRowFields]' count of the live work each of
// n tasks blocks, named for the plan test.
func dependentsCountStatement(n int) string {
	return `
		SELECT d.blocker_id, COUNT(*) FROM tracker_task_deps d
		JOIN tracker_tasks w ON w.id = d.task_id AND w.removed_at IS NULL
		WHERE d.blocker_id IN (` + placeholders(n) + `)
		GROUP BY d.blocker_id`
}

// fillRowFields answers the row facts ([RowField]) a query asked for, over the
// rows in hand, in the read's own transaction. A task on several columns of a
// label board is one id and several rows, so facts are read per distinct id
// and laid onto every row carrying it.
func fillRowFields(ctx context.Context, tx *sql.Tx, q Query,
	rows []*TaskRow) error {

	if len(q.RowFields) == 0 || len(rows) == 0 {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, row := range rows {
		if !seen[row.ID] {
			seen[row.ID] = true
			ids = append(ids, row.ID)
		}
	}
	tags := map[string][]string{}
	dependents := map[string]int{}
	asks := map[string]int{}
	spend := map[string]RowSpend{}
	for start := 0; start < len(ids); start += rowFieldBatch {
		batch := ids[start:min(start+rowFieldBatch, len(ids))]
		if q.Wants(RowFieldTags) {
			if err := readRowTags(ctx, tx, batch, tags); err != nil {
				return err
			}
		}
		if q.Wants(RowFieldDependentsCount) {
			if err := readRowCounts(ctx, tx, dependentsCountStatement(len(batch)),
				batch, dependents); err != nil {
				return fmt.Errorf("worklet: count what each row blocks: %w", err)
			}
		}
		if q.Wants(RowFieldOpenAsks) {
			if err := readRowCounts(ctx, tx, `
				SELECT m.task_id, COUNT(*) FROM tracker_comments m
				WHERE m.task_id IN (`+placeholders(len(batch))+`) AND `+
				openAskSQL+`
				GROUP BY m.task_id`, batch, asks); err != nil {
				return fmt.Errorf("worklet: count each row's open asks: %w", err)
			}
		}
		if q.Wants(RowFieldSpend) {
			got, err := spendOf(ctx, tx, batch)
			if err != nil {
				return err
			}
			for id, s := range got {
				spend[id] = s
			}
		}
	}
	for _, row := range rows {
		if q.Wants(RowFieldTags) {
			// Non-nil when asked, so "asked, and it has none" reaches
			// the wire as `[]` ([TaskRow.Tags]).
			row.Tags = append([]string{}, tags[row.ID]...)
		}
		if q.Wants(RowFieldDependentsCount) {
			n := dependents[row.ID]
			row.DependentsCount = &n
		}
		if q.Wants(RowFieldOpenAsks) {
			n := asks[row.ID]
			row.OpenAsks = &n
		}
		if q.Wants(RowFieldSpend) {
			s := spend[row.ID]
			row.Spend = &s
		}
	}
	return nil
}

// rowTagsStatement is [readRowTags]' read of n tasks' labels, named for the
// plan test.
func rowTagsStatement(n int) string {
	return `
		SELECT task_id, slug FROM tracker_task_tags
		WHERE task_id IN (` + placeholders(n) + `)
		ORDER BY task_id, slug`
}

// readRowTags reads the labels of one batch, sorted per task.
func readRowTags(ctx context.Context, tx *sql.Tx, ids []string,
	into map[string][]string) error {

	rows, err := tx.QueryContext(ctx, rowTagsStatement(len(ids)), anyOf(ids)...)
	if err != nil {
		return fmt.Errorf("worklet: read each row's labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return fmt.Errorf("worklet: scan a row's label: %w", err)
		}
		into[id] = append(into[id], slug)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("worklet: walk the rows' labels: %w", err)
	}
	return nil
}

// readRowCounts reads an `(id, count)` statement into a map.
func readRowCounts(ctx context.Context, tx *sql.Tx, query string, ids []string,
	into map[string]int) error {

	rows, err := tx.QueryContext(ctx, query, anyOf(ids)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		into[id] = n
	}
	return rows.Err()
}

// rowSpendStatement is [spendOf]'s read of n tasks' running totals, named for
// the plan test.
func rowSpendStatement(n int) string {
	return `
		SELECT id, spend_tokens, spend_turns, spend_workers, spend_sent_back,
		       reopens
		FROM tracker_tasks WHERE id IN (` + placeholders(n) + `)`
}

// spendOf is what each of these tasks has cost, keyed by id: exactly the ids
// asked for that exist. It reads in the caller's transaction, never its own: a
// cost from a later snapshot beside a title from an earlier one describes two
// different tasks. The totals are the applier's running columns ([Spend]), so
// nothing here re-adds a turn.
func spendOf(ctx context.Context, tx *sql.Tx, ids []string) (map[string]RowSpend, error) {
	out := make(map[string]RowSpend, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, rowSpendStatement(len(ids)), anyOf(ids)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read what each row cost: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var s RowSpend
		if err := rows.Scan(&id, &s.Tokens, &s.Turns, &s.Workers, &s.SentBack,
			&s.Reopens); err != nil {
			return nil, fmt.Errorf("worklet: scan what a row cost: %w", err)
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: walk what the rows cost: %w", err)
	}
	return out, nil
}

// listOrdered reports whether a priority list decides this answer's order.
// SQL cannot page that order (it is the id sequence on another object, which
// [orderByList] restores after the read), so the whole set is read at once,
// bounded at [MaxPriorities] by the `t.id IN (…)` the list compiles to, and
// [pageListed] pages it in the list's order.
func listOrdered(q Query) bool {
	return q.PriorityListOf != "" && len(q.PriorityList) > 0
}

// listCursorOrder is the order a list-ordered cursor is minted under, and it
// is not one [renderOrder] can produce — so a list cursor handed to a keyset
// query, or a keyset cursor handed to a list, is refused by [cursorArms]'s
// own order check rather than resumed against the wrong sequence.
const listCursorOrder = "priority-list"

// pageListed cuts one page out of the whole list-ordered set; re-sorting each
// SQL-read page instead would put the top of a list longer than the page on
// page two. The cursor is the list OFFSET the next page starts at: a re-order
// between pages shifts it, as dragging a card shifts a rank cursor, but the
// set is re-read whole each page, so only the position is lost.
func pageListed(rows []TaskRow, cursor string, limit int) ([]TaskRow, string, error) {
	from := 0
	if cursor != "" {
		resume, err := decodePage(cursor)
		if err != nil {
			return nil, "", err
		}
		if resume.Order != listCursorOrder {
			return nil, "", fmt.Errorf("%w: it was minted for another order "+
				"and this query is ordered by a priority list. A cursor belongs "+
				"to the order that minted it, and re-sorting starts again from "+
				"the first page", ErrBadCursor)
		}
		offset := -1
		if len(resume.Keys) == 1 {
			offset, err = strconv.Atoi(fmt.Sprint(resume.Keys[0]))
		}
		if err != nil || offset < 0 {
			return nil, "", fmt.Errorf("%w: it names no row to resume after",
				ErrBadCursor)
		}
		from = min(offset, len(rows))
	}
	end := min(from+limit, len(rows))
	out := rows[from:end]
	if end < len(rows) {
		return out, mintCursor([]any{end}, listCursorOrder), nil
	}
	return out, "", nil
}
