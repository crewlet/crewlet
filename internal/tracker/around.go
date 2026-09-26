package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Where one task sits in an answer: `around=<key>`.
//
// A task page opened from a board says "3 of 18" and steps to the task before
// and after it — and a page holds ONE task, not the board it came from. What
// it holds is the board's QUESTION (the same parameters), so the answer to
// "where is this one in it" has to come from the engine, over the whole
// answer rather than the page the board happened to have loaded: a peek that
// stepped only through the rows a client held would stop dead at row fifty of
// a four-hundred-task column and call it the end.
//
// # The order is the DRAWING order
//
// Flat, it is the answer's own sort — the very order a cursor pages through,
// so the neighbours are the rows paging would have shown next to it. Grouped,
// it is the board read left to right: every column in the answer's own column
// order, and within a column its rows in the row order — then, with swimlanes,
// every lane of a column in lane order before the next column. A column or a
// lane the answer did not draw ([Answer.GroupsDropped], [Group.SubgroupsDropped])
// is not in it, because a person stepping through a board steps through what
// is on the screen. Under a priority list it is the list's own order, which is
// the order the rows are drawn in there.
//
// # A label board counts CELLS
//
// On an axis where one task is on several columns — `group_by=tag` — the
// drawing order holds it once per column, so [Around.TotalHint] is the number
// of cards on the board rather than the number of tasks, and a task is placed
// at the FIRST column it appears in. That is what "3 of 18" means on a board
// where 18 cards are drawn; the answer's own `total_hint` counts tasks, and
// `groups_overlap` already says the two differ.
//
// # Absent is null, never an error
//
// A task the question does not match — filtered out, finished under an
// open-work board, removed, or in a column the board did not draw — answers
// `around: null`. That is the ordinary case of somebody opening a task that
// has just moved off the board they came from, and a refusal would take the
// page down with it.

// Around is one task's place in an answer's drawing order.
type Around struct {
	// Position is 1-based, and NULL when it lies past [TotalHintCeiling]:
	// counting the rows ahead of a task is the same scan an exact total is,
	// and the answer bounds it at the same ceiling for the same reason — a
	// task page polls.
	Position *int `json:"position"`

	// Prev and Next are the KEYS of the neighbours, null at either end.
	// Keys rather than ids because a key is what a task page's address
	// carries.
	Prev *string `json:"prev"`
	Next *string `json:"next"`

	// TotalHint is how long the drawing order is, capped at
	// [TotalHintCeiling] with TotalCapped set exactly as the answer's own
	// is. Equal to the answer's `total_hint` everywhere except a label
	// board, where it counts cards — see the package doc above.
	TotalHint   int  `json:"total_hint"`
	TotalCapped bool `json:"total_capped,omitempty"`
}

// segment is one run of the drawing order: the whole answer when it is flat,
// one column or one lane when it is grouped. Its rows are in the answer's own
// row order.
type segment struct {
	// Join reaches the grouping axis the clause compares, and Clause
	// narrows the answer's predicate to this run. Both empty for a flat
	// answer, which is one run.
	Join     string
	JoinArgs []any
	Clause   string
	Args     []any

	// Count is how many rows the run holds, from the answer's own column
	// and lane counts — exact, because a GROUP BY is — and unused for a
	// flat answer, whose one run is the answer.
	Count int
}

// readAround answers [Query.Around] over the rows an answer describes.
//
// `where` and `args` are the answer's UNPAGED predicate — a cursor says where
// one page starts and says nothing about where a task sits in the whole — and
// `listed` is the whole list-ordered set when a priority list decided the
// order.
func readAround(ctx context.Context, tx *sql.Tx, q Query,
	fields map[string]resolvedField, where string, args []any,
	terms []sortTerm, answer *Answer, listed []TaskRow) (*Around, error) {

	id, err := resolveTaskID(ctx, tx, q.Around)
	switch {
	case errors.Is(err, ErrNoTask):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if q.GroupBy == "" && listOrdered(q) {
		return aroundListed(listed, id), nil
	}

	segments, err := drawingOrder(q, fields, answer)
	if err != nil {
		return nil, err
	}
	out := &Around{}
	if q.GroupBy == "" {
		out.TotalHint, out.TotalCapped = answer.TotalHint, answer.TotalCapped
	} else {
		cells := 0
		for _, seg := range segments {
			cells += seg.Count
		}
		out.TotalHint, out.TotalCapped = capHint(cells)
	}

	at, ahead := -1, 0
	for i, seg := range segments {
		held, holdErr := segmentHolds(ctx, tx, seg, where, args, id)
		if holdErr != nil {
			return nil, holdErr
		}
		if held {
			at = i
			break
		}
		ahead += seg.Count
	}
	if at < 0 {
		return nil, nil
	}
	seg := segments[at]

	// THE TASK'S OWN SORT VALUES, which the "before" and "after" predicates
	// compare against — the same keyset a cursor minted on this row would
	// carry, so a neighbour here is exactly the row a page boundary on it
	// would have resumed at.
	keys, err := sortValuesOf(ctx, tx, terms, id)
	if err != nil {
		return nil, err
	}
	after, afterArgs := keysetAfter(terms, keys)
	// BEFORE IS "NOT AFTER AND NOT THIS ROW". The order is TOTAL — every
	// one ends in the id — so a row that is neither after this one nor this
	// one is before it; and the after predicate is never NULL (see
	// [keysetAfter], whose every leg is spelled NULL-safely), so its
	// negation is exact rather than dropping the rows a NULL comparison
	// would.
	before := "NOT " + after + " AND t.id <> ?"
	beforeArgs := append(append([]any{}, afterArgs...), id)

	counted, err := segmentCount(ctx, tx, seg, terms, where, args, before,
		beforeArgs)
	if err != nil {
		return nil, err
	}
	if position := ahead + counted + 1; counted <= TotalHintCeiling &&
		position <= TotalHintCeiling {
		out.Position = &position
	}

	forward, reverse := renderOrder(terms), renderReverse(terms)
	if out.Next, err = segmentFirst(ctx, tx, seg, terms, where, args, after,
		afterArgs, forward); err != nil {
		return nil, err
	}
	for j := at + 1; out.Next == nil && j < len(segments); j++ {
		if out.Next, err = segmentFirst(ctx, tx, segments[j], terms, where,
			args, "", nil, forward); err != nil {
			return nil, err
		}
	}
	if out.Prev, err = segmentFirst(ctx, tx, seg, terms, where, args, before,
		beforeArgs, reverse); err != nil {
		return nil, err
	}
	for j := at - 1; out.Prev == nil && j >= 0; j-- {
		if out.Prev, err = segmentFirst(ctx, tx, segments[j], terms, where,
			args, "", nil, reverse); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// aroundListed places a task in a list-ordered answer, which is read whole —
// see [readListOrdered] — so its place is an index rather than a count.
func aroundListed(rows []TaskRow, id string) *Around {
	for i, row := range rows {
		if row.ID != id {
			continue
		}
		position := i + 1
		out := &Around{Position: &position, TotalHint: len(rows)}
		if i > 0 {
			out.Prev = &rows[i-1].Key
		}
		if i+1 < len(rows) {
			out.Next = &rows[i+1].Key
		}
		return out
	}
	return nil
}

// drawingOrder is the answer's runs, in the order a person reads them.
//
// FROM THE ANSWER'S OWN COLUMNS, never recounted: the columns and lanes were
// counted, capped and ordered by [readGroups] in this transaction, and a
// second reading of the same rules here would be the one place "which columns
// a board draws" could come out differently. A run that holds nothing is
// skipped, which is every column [fillColumns] minted empty.
func drawingOrder(q Query, fields map[string]resolvedField,
	answer *Answer) ([]segment, error) {

	if q.GroupBy == "" {
		return []segment{{}}, nil
	}
	outer, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
	if err != nil {
		return nil, err
	}
	var inner groupAxis
	if q.GroupBy2 != "" {
		if inner, err = compileLane(q.GroupBy2, fields, q.dayWindow(),
			q.Units); err != nil {
			return nil, err
		}
	}
	var out []segment
	for _, column := range answer.Groups {
		if column.Count == 0 {
			continue
		}
		clause, values := outer.joinedFilter(column.Key)
		if q.GroupBy2 == "" {
			out = append(out, segment{
				Join: outer.Join, JoinArgs: outer.JoinArgs,
				Clause: clause, Args: values, Count: column.Count,
			})
			continue
		}
		for _, lane := range column.Subgroups {
			if lane.Count == 0 {
				continue
			}
			laneClause, laneValues := inner.joinedFilter(lane.Key)
			out = append(out, segment{
				Join: outer.Join + inner.Join,
				JoinArgs: append(append([]any{}, outer.JoinArgs...),
					inner.JoinArgs...),
				Clause: clause + " AND " + laneClause,
				Args:   append(append([]any{}, values...), laneValues...),
				Count:  lane.Count,
			})
		}
	}
	return out, nil
}

// segmentFrom renders a statement's FROM and WHERE over one run, with the
// order's own joins so a keyset over a custom-field sort can name its alias,
// and the arguments in statement order: the joins' first, then the answer's
// predicate, then the run's narrowing, then the caller's own clause.
func segmentFrom(seg segment, terms []sortTerm, where string, args []any,
	extra string, extraArgs []any) (string, []any) {

	joins, joinArgs := sortJoins(terms)
	clause := "(" + where + ")"
	if seg.Clause != "" {
		clause += " AND " + seg.Clause
	}
	if extra != "" {
		clause += " AND " + extra
	}
	bound := append(append([]any{}, seg.JoinArgs...), joinArgs...)
	bound = append(bound, args...)
	bound = append(bound, seg.Args...)
	bound = append(bound, extraArgs...)
	return " FROM tracker_tasks t" + seg.Join + joins + " WHERE " + clause, bound
}

// segmentHolds reports whether one run holds the task.
func segmentHolds(ctx context.Context, tx *sql.Tx, seg segment, where string,
	args []any, id string) (bool, error) {

	from, bound := segmentFrom(seg, nil, where, args, "t.id = ?", []any{id})
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1"+from+" LIMIT 1", bound...).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("tracker: find the around task in its run: %w", err)
	}
	return true, nil
}

// segmentCount counts one run's rows matching a clause, stopping one past the
// ceiling exactly as [countHint] does.
func segmentCount(ctx context.Context, tx *sql.Tx, seg segment,
	terms []sortTerm, where string, args []any, extra string,
	extraArgs []any) (int, error) {

	from, bound := segmentFrom(seg, terms, where, args, extra, extraArgs)
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT 1"+from+" LIMIT ?)",
		append(bound, TotalHintCeiling+1)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("tracker: count the rows ahead of the around "+
			"task: %w", err)
	}
	return n, nil
}

// segmentFirst is the key of the first row of one run in an order, narrowed by
// a clause, or nil when there is none.
func segmentFirst(ctx context.Context, tx *sql.Tx, seg segment,
	terms []sortTerm, where string, args []any, extra string, extraArgs []any,
	order string) (*string, error) {

	from, bound := segmentFrom(seg, terms, where, args, extra, extraArgs)
	var key string
	err := tx.QueryRowContext(ctx,
		"SELECT t.key"+from+" ORDER BY "+order+" LIMIT 1", bound...).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("tracker: read a neighbour of the around "+
			"task: %w", err)
	}
	return &key, nil
}

// sortValuesOf reads one task's value for every column an order sorts by,
// scanned as opaque values exactly as [readTasksJoined] scans a cursor's.
func sortValuesOf(ctx context.Context, tx *sql.Tx, terms []sortTerm,
	id string) ([]any, error) {

	columns := make([]string, len(terms))
	for i, term := range terms {
		columns[i] = term.Column
	}
	joins, joinArgs := sortJoins(terms)
	values := make([]any, len(terms))
	targets := make([]any, len(terms))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := tx.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ", ")+
		" FROM tracker_tasks t"+joins+" WHERE t.id = ?",
		append(joinArgs, id)...).Scan(targets...); err != nil {
		return nil, fmt.Errorf("tracker: read the around task's sort values: %w",
			err)
	}
	return values, nil
}

// renderReverse is [renderOrder] read backwards — the order "the row before
// this one" is the first row of.
//
// THE NULLS MOVE TOO. The forward order puts an absent value LAST in both
// directions, so reversed they come FIRST in both, and a bare flip of ASC and
// DESC would not do that: SQLite's own default puts NULL first ascending and
// last descending, which is right for exactly one of the two. The leading
// `IS NULL` term states it rather than relying on either default, and it is
// written only on a column that can be NULL, for the planner reason
// [renderOrder] gives.
func renderReverse(terms []sortTerm) string {
	rendered := make([]string, 0, len(terms))
	for _, term := range terms {
		direction := " DESC"
		if term.Descending {
			direction = " ASC"
		}
		if !term.NeverNull {
			rendered = append(rendered, "("+term.Column+" IS NULL) DESC")
		}
		rendered = append(rendered, term.Column+direction)
	}
	return strings.Join(rendered, ", ")
}
