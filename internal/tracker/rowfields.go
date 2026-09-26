package tracker

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

// openAskSQL is "this comment is a question still waiting for its answer", over
// a `tracker_comments` row aliased `m`.
//
// ONE SPELLING for the `has_open_asks=` filter and the `open_asks` row count,
// so a card cannot say "1 open question" on a task the filter beside it says
// has none — the two were about to be written twice, one per reader.
const openAskSQL = "m.ask <> '' AND m.resolved = 0 AND m.answered_by IS NULL " +
	"AND m.removed = 0"

// rowFieldBatch bounds how many ids one row-fact statement binds. A grouped
// answer carries up to [MaxGroupsWithSubgroups] × [MaxSubgroups] ×
// [GroupRowsMax] rows, far past what one `IN (…)` should bind, and the page
// ceiling is the natural size of one batch: it is already the most rows one
// flat answer reads at once.
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

// fillRowFields answers the row facts a query asked for, over the rows in
// hand, in the read's own transaction — see [RowField].
//
// A TASK ON SEVERAL COLUMNS of a label board is one id and several rows, so
// the facts are read per distinct id and laid onto every row carrying it.
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
			if err := readRowCounts(ctx, tx, `
				SELECT d.blocker_id, COUNT(*) FROM tracker_task_deps d
				JOIN tracker_tasks w ON w.id = d.task_id AND w.removed_at IS NULL
				WHERE d.blocker_id IN (`+placeholders(len(batch))+`)
				GROUP BY d.blocker_id`, batch, dependents); err != nil {
				return fmt.Errorf("tracker: count what each row blocks: %w", err)
			}
		}
		if q.Wants(RowFieldOpenAsks) {
			if err := readRowCounts(ctx, tx, `
				SELECT m.task_id, COUNT(*) FROM tracker_comments m
				WHERE m.task_id IN (`+placeholders(len(batch))+`) AND `+
				openAskSQL+`
				GROUP BY m.task_id`, batch, asks); err != nil {
				return fmt.Errorf("tracker: count each row's open asks: %w", err)
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
			// NON-NIL WHEN ASKED, so "asked, and it has none" reaches
			// the wire as `[]` — see [TaskRow.Tags].
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

// readRowTags reads the labels of one batch, sorted per task.
func readRowTags(ctx context.Context, tx *sql.Tx, ids []string,
	into map[string][]string) error {

	rows, err := tx.QueryContext(ctx, `
		SELECT task_id, slug FROM tracker_task_tags
		WHERE task_id IN (`+placeholders(len(ids))+`)
		ORDER BY task_id, slug`, anyOf(ids)...)
	if err != nil {
		return fmt.Errorf("tracker: read each row's labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return fmt.Errorf("tracker: scan a row's label: %w", err)
		}
		into[id] = append(into[id], slug)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tracker: walk the rows' labels: %w", err)
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

// spendOf is what each of these tasks has cost, keyed by id — exactly the ids
// asked for that exist, and nothing else.
//
// INSIDE THE CALLER'S TRANSACTION, never a read of its own: it decorates rows
// somebody already read, and a cost from a later snapshot beside a title from
// an earlier one is a card describing two different tasks. The totals are the
// applier's own running columns ([Spend]), which are a function of the turn
// records it applied, so nothing here re-adds a turn.
func spendOf(ctx context.Context, tx *sql.Tx, ids []string) (map[string]RowSpend, error) {
	out := make(map[string]RowSpend, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, spend_tokens, spend_turns, spend_workers, spend_sent_back,
		       reopens
		FROM tracker_tasks WHERE id IN (`+placeholders(len(ids))+`)`,
		anyOf(ids)...)
	if err != nil {
		return nil, fmt.Errorf("tracker: read what each row cost: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var s RowSpend
		if err := rows.Scan(&id, &s.Tokens, &s.Turns, &s.Workers, &s.SentBack,
			&s.Reopens); err != nil {
			return nil, fmt.Errorf("tracker: scan what a row cost: %w", err)
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: walk what the rows cost: %w", err)
	}
	return out, nil
}

// listOrdered reports whether a priority list decides this answer's order.
//
// Then SQL cannot page it: the order is the sequence of ids on another
// object, and [orderByList] restores it after the rows are read. So the whole
// set is read at once — the `t.id IN (…)` the list compiles to bounds it at
// [MaxPriorities] — and [pageListed] pages it in the list's own order.
func listOrdered(q Query) bool {
	return q.PriorityListOf.Named() && len(q.PriorityList) > 0
}

// listCursorOrder is the order a list-ordered cursor is minted under, and it
// is not one [renderOrder] can produce — so a list cursor handed to a keyset
// query, or a keyset cursor handed to a list, is refused by [cursorClause]'s
// own order check rather than resumed against the wrong sequence.
const listCursorOrder = "priority-list"

// pageListed cuts one page out of the whole list-ordered set.
//
// # The bug this replaced
//
// The list's order used to be restored per PAGE: SQL read a page in its own
// order, minted a keyset cursor on that page's last row, and then the page was
// re-sorted by the list. With a page smaller than the list — `limit=5` over a
// ten-item list — the first page was whichever five SQL read first, in list
// order among themselves, and the second page resumed in SQL order: the top
// of somebody's priorities could land on page two. The dashboard never saw it
// (its page is fifty, above the list's cap); a seat asking for five did.
//
// The cursor is the list OFFSET the next page starts at. That shifts when the
// list is re-ordered between two pages, exactly as a rank cursor does when a
// card is dragged between them — the set is re-read whole on every page, so
// nothing is lost to it but the position.
func pageListed(rows []TaskRow, cursor string, limit int) ([]TaskRow, string, error) {
	from := 0
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("tracker: the cursor is not one this "+
				"surface minted: %w", err)
		}
		var resume page
		if json.Unmarshal(raw, &resume) != nil || len(resume.Keys) != 1 {
			return nil, "", fmt.Errorf("tracker: the cursor names no row to " +
				"resume after")
		}
		if resume.Order != listCursorOrder {
			return nil, "", fmt.Errorf("tracker: this cursor was minted for "+
				"the order %q and this query is ordered by a priority list — "+
				"a cursor belongs to the order that minted it, and re-sorting "+
				"starts a new page", resume.Order)
		}
		offset, err := strconv.Atoi(fmt.Sprint(resume.Keys[0]))
		if err != nil || offset < 0 {
			return nil, "", fmt.Errorf("tracker: the cursor names no row to " +
				"resume after")
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
