package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Grouping: the board's columns, and why a grouped answer is a different shape
// from a flat one.
//
// # A grouped answer is GROUPS, not rows with a label on each
//
// A board is columns, and what it needs per column is the column's own count —
// which is a fact about the whole set — and enough of its rows to draw. Rows
// with a group label on each would make the count a property of the PAGE: a
// column with four hundred tasks and a fifty-row page would render "12", and
// nothing in the answer would say the number was of what happened to fit.
//
// So [Answer.Groups] carries one entry per distinct value, each with its own
// `count` over the whole set and its own bounded slice of rows. The flat
// [Answer.Rows] is EMPTY when a query groups: returning both would be the same
// rows twice, and a caller that rendered the flat half would draw a board with
// no columns and no way to tell.
//
// # And there is no cursor across groups
//
// A keyset cursor is "after this row in this order", and across a set of
// groups there is no single order to be after. What a board actually does is
// load ONE column further, and that has a spelling already: `group=<value>`
// narrows the query to that group, and it is an ordinary flat query with an
// ordinary cursor. So a grouped answer mints no cursor and says how many rows
// each group is holding back rather than pretending to page.
//
// # Why the counts come from their own statement
//
// One statement per shape rather than a window function doing both: the counts
// are a `GROUP BY` over the predicate, and the rows are the ordinary paged
// statement run once per group. A single windowed query would rank every row
// in the corpus to return a handful per column, which is the cost a board
// pays on every poll.

// MaxGroups bounds how many columns one answer draws.
//
// SIXTY-FOUR, which is above every closed set this grammar groups on — six
// statuses, four status groups, five priorities — and is a real bound only for
// the open ones: assignee, tag, type and a custom field's options. A board
// with more columns than that is not a board, and the count of what was left
// out is on the answer rather than silently dropped.
const MaxGroups = 64

// GroupRowsDefault and GroupRowsMax bound how many rows one column carries.
//
// TWENTY is what a column shows before somebody scrolls it, and a board draws
// several at once — so the default is per COLUMN and the product is what the
// answer costs. A hundred is the ceiling for a caller that means to render a
// whole column at once.
const (
	GroupRowsDefault = 20
	GroupRowsMax     = 100
)

// Group is one column of a grouped answer.
type Group struct {
	// Key is the stored value the rows share — a status slug, a handle, an
	// option id — and Label what a person reads, which differ exactly
	// where the store keeps an id and a person knows a word.
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`

	// Count is over the WHOLE set, never over Rows: a column with four
	// hundred tasks says four hundred and carries twenty.
	Count int `json:"count"`

	Rows []TaskRow `json:"rows"`

	// Subgroups is the second axis, when one was asked for.
	Subgroups []Group `json:"subgroups,omitempty"`
}

// groupAxis is one grouping axis compiled into SQL.
type groupAxis struct {
	// Expr is what the value is, and Join what it needs to reach it.
	Expr string
	Join string
	Args []any

	// Multi marks an axis on which one task appears in SEVERAL groups —
	// only a tag today. It is not an error: a task with three labels is on
	// three columns, which is what a label board IS. What it changes is
	// that the counts do not sum to the answer's own total, and the
	// answer says so rather than leaving a reader to wonder.
	Multi bool

	// Unset is the label for a row whose value is absent, because "nobody
	// is assigned" is a column a board draws rather than a row it hides.
	Unset string

	// Order is the axis's own DECLARED order, when it has one. A closed
	// set has a meaning in its sequence — todo before in_progress before
	// done — and ordering those columns by size would re-shuffle a board
	// every time work moved between them, which is a board nobody can
	// learn the shape of. An open set (an assignee, a tag, an option) has
	// no such order and falls back to the largest column first.
	Order []string

	// Exists is the JOIN-FREE form of this axis as a predicate, used when
	// `group=<value>` narrows the whole query. It has to be join-free
	// because the narrowed predicate is shared with the count hint and
	// the totals, and neither of those carries a join — so an axis
	// expressed only as one would leave a header adding up the whole
	// board while the rows showed one column of it.
	Exists func(key string) (string, []any)
}

// filter renders this axis as a predicate on one value.
func (a groupAxis) filter(key string) (string, []any) {
	if a.Exists != nil {
		return a.Exists(key)
	}
	if key == "" {
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')", nil
	}
	return a.Expr + " = ?", []any{key}
}

// compileGroup turns a grouping key into its axis.
func compileGroup(key string, fields map[string]resolvedField) (groupAxis, error) {
	if ref, ok := strings.CutPrefix(key, FieldKeyPrefix); ok && ref != "" {
		field, held := fields[ref]
		if !held {
			return groupAxis{}, fmt.Errorf("tracker: group_by names f.%s and "+
				"no field resolved to it", ref)
		}
		// ONE ALIAS, and `seq = 0` for the reason a field SORT pins it:
		// a multi-valued field joined whole would put one task on every
		// column it holds a value for, which is a labels board — and
		// this axis is the field's own first value.
		column := FieldValueColumn(field.Type)
		// A MULTI-VALUED FIELD IS A LABEL BOARD, exactly as `tag` is: the
		// join is unpinned so a task with three values is on three
		// columns, and the axis declares the overlap so the counts do
		// not read as an answer that fails to add up. Pinning `seq = 0`
		// on one of these would show every task under its FIRST value
		// and say nothing — which is a board that is quietly wrong
		// rather than one that is differently shaped.
		pin := " AND gv.seq = 0"
		if field.Multi {
			pin = ""
		}
		return groupAxis{
			Expr: "gv." + column,
			Join: " LEFT JOIN tracker_field_values gv ON gv.task_id = t.id" +
				" AND gv.field_id = ? AND " +
				strings.ReplaceAll(liveFieldValue, "v.", "gv.") + pin,
			Args:  []any{field.ID},
			Multi: field.Multi,
			Unset: "(not set)",
			Exists: func(key string) (string, []any) {
				inner := "SELECT 1 FROM tracker_field_values v " +
					"WHERE v.task_id = t.id AND v.field_id = ? AND " +
					liveFieldValue
				if !field.Multi {
					inner += " AND v.seq = 0"
				}
				if key == "" {
					return "NOT EXISTS (" + inner + " AND v." + column +
						" IS NOT NULL)", []any{field.ID}
				}
				return "EXISTS (" + inner + " AND v." + column + " = ?)",
					[]any{field.ID, key}
			},
		}, nil
	}
	switch key {
	case "status":
		return groupAxis{Expr: "t.status", Order: declaredOrder(Statuses)}, nil
	case "status_group":
		return groupAxis{
			Expr: "t.status_group", Order: declaredOrder(StatusGroups),
		}, nil
	case "assignee":
		return groupAxis{Expr: "t.assignee", Unset: "(unassigned)"}, nil
	case "priority":
		return groupAxis{Expr: "t.priority", Order: declaredOrder(Priorities)}, nil
	case "type":
		return groupAxis{Expr: "t.type"}, nil
	case "project":
		return groupAxis{Expr: "t.project_key"}, nil
	case "sprint":
		return groupAxis{Expr: "COALESCE(CAST(t.sprint_number AS TEXT), '')",
			Unset: "(no sprint)"}, nil
	case "unit":
		return groupAxis{Expr: "t.filed_unit", Unset: "(no unit)"}, nil
	case "routing_unit":
		return groupAxis{Expr: "t.routing_unit", Unset: "(no unit)"}, nil
	case "parent":
		return groupAxis{Expr: "COALESCE(t.parent_id, '')", Unset: "(no parent)"}, nil
	case "tag":
		// THE ONE MULTI-VALUED AXIS, and the join is what makes it one: a
		// task with three labels is on three columns, which is what a
		// label board is for.
		return groupAxis{
			Expr:  "gt.slug",
			Join:  " LEFT JOIN tracker_task_tags gt ON gt.task_id = t.id",
			Multi: true, Unset: "(untagged)",
			Exists: func(key string) (string, []any) {
				inner := "SELECT 1 FROM tracker_task_tags g " +
					"WHERE g.task_id = t.id"
				if key == "" {
					return "NOT EXISTS (" + inner + ")", nil
				}
				return "EXISTS (" + inner + " AND g.slug = ?)", []any{key}
			},
		}, nil
	case "due:day":
		return groupAxis{Expr: dayBucket("t.due_at"), Unset: "(no due date)"}, nil
	case "due:week":
		return groupAxis{Expr: weekBucket("t.due_at"), Unset: "(no due date)"}, nil
	case "start:week":
		return groupAxis{Expr: weekBucket("t.start_at"), Unset: "(no start date)"}, nil
	}
	return groupAxis{}, fmt.Errorf("tracker: %q is not a grouping", key)
}

// dayBucket renders an instant column as its own calendar day, in UTC.
//
// UTC, AND THE ANSWER SAYS SO. A day boundary is a company's own and this
// grammar carries no zone on a grouping — so the bucket is the one every node
// computes identically, rather than each node's local midnight, which would
// put one task on two different days on two machines.
//
// The division by a million is the unit change: an instant column holds
// MICROSECONDS, which is what store.EncodeTime writes, and SQLite's own date
// functions take seconds.
func dayBucket(column string) string {
	return `COALESCE(STRFTIME('%Y-%m-%d', ` + column + ` / 1000000, 'unixepoch'), '')`
}

// weekBucket is the same at week granularity, Monday-anchored.
//
// `%W` NUMBERS FROM THE YEAR'S FIRST MONDAY, so the days before it fall in
// week 00 rather than in the previous year's last ISO week. That is a heading
// on a column rather than a date anybody computes with, and it is the
// numbering every Turso build has — `%V` and `%G` are recent additions this
// package cannot assume, and an unsupported format specifier renders as
// itself rather than refusing.
func weekBucket(column string) string {
	return `COALESCE(STRFTIME('%Y-W%W', ` + column + ` / 1000000, 'unixepoch'), '')`
}

// groupCounts reads one axis's distinct values and their counts.
func groupCounts(ctx context.Context, tx *sql.Tx, axis groupAxis,
	where string, args []any, limit int) ([]Group, int, error) {

	// THE DECLARED ORDER WHERE THERE IS ONE, and the largest column first
	// where there is not — see [groupAxis.Order]. The CASE is built from
	// the closed set's own sequence, so nothing here composes a caller's
	// value into SQL.
	order := "n DESC, g"
	if len(axis.Order) > 0 {
		var arms strings.Builder
		arms.WriteString("CASE g")
		for i, value := range axis.Order {
			fmt.Fprintf(&arms, " WHEN '%s' THEN %d", value, i)
		}
		fmt.Fprintf(&arms, " ELSE %d END, g", len(axis.Order))
		order = arms.String()
	}
	query := `SELECT ` + axis.Expr + ` AS g, COUNT(*) AS n
	          FROM tracker_tasks t` + axis.Join + `
	          WHERE ` + where + `
	          GROUP BY g
	          ORDER BY ` + order + `
	          LIMIT ?`
	bound := append(append([]any{}, axis.Args...), args...)
	rows, err := tx.QueryContext(ctx, query, append(bound, limit+1)...)
	if err != nil {
		return nil, 0, fmt.Errorf("tracker: count the groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Group
	for rows.Next() {
		var key sql.NullString
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, 0, fmt.Errorf("tracker: scan a group: %w", err)
		}
		group := Group{Key: key.String, Count: count}
		if group.Key == "" {
			// AN ABSENT VALUE IS ITS OWN COLUMN. "Nobody is assigned"
			// is a question a board answers rather than a row it
			// hides, and the label is what a person reads on it.
			group.Label = axis.Unset
		}
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("tracker: walk the groups: %w", err)
	}
	dropped := 0
	if len(out) > limit {
		// THE OVERFLOW IS COUNTED AND SAID, never silently cut: a board
		// that drew sixty-four of two hundred columns and reported
		// nothing would look like a company with sixty-four assignees.
		dropped = len(out) - limit
		out = out[:limit]
	}
	return out, dropped, nil
}

// groupRows reads one group's own page.
//
// THE AXIS BECOMES A PREDICATE, which is what makes this the ordinary flat
// statement: a column's rows are the answer's rows narrowed to one value, so
// the same sort, the same joins and the same row shape serve both.
func groupRows(ctx context.Context, tx *sql.Tx, axis groupAxis, key string,
	where string, args []any, terms []sortTerm, limit int) ([]TaskRow, error) {

	// THE JOINED FORM HERE, not the join-free one: this statement already
	// carries the axis's join for the GROUP BY's sake, so comparing the
	// joined column is one predicate rather than a second subquery.
	clause, values := axis.joinedFilter(key)
	rows, _, err := readTasksJoined(ctx, tx, axis.Join, axis.Args,
		"("+where+") AND "+clause,
		append(append([]any{}, args...), values...), terms, limit)
	return rows, err
}

// joinedFilter is the axis as a predicate on the column its own join reached.
func (a groupAxis) joinedFilter(key string) (string, []any) {
	if key == "" {
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')", nil
	}
	return a.Expr + " = ?", []any{key}
}

// groupLabels fills in what a person reads where the store keeps an id.
//
// ONLY A CUSTOM FIELD'S OPTIONS NEED IT TODAY: every other axis stores the
// word already, and a label derived where none is needed would be a second
// name for one value.
func groupLabels(groups []Group, key string, fields map[string]resolvedField) {
	ref, ok := strings.CutPrefix(key, FieldKeyPrefix)
	if !ok || ref == "" {
		return
	}
	field, held := fields[ref]
	if !held || len(field.Labels) == 0 {
		return
	}
	// FROM THE LABEL MAP, never by inverting [resolvedField.Options]:
	// that one holds two keys per option, so an inversion picks the slug
	// or the name by Go's randomised map iteration and a column heading
	// would differ between two requests to one node.
	for i := range groups {
		if groups[i].Label == "" {
			groups[i].Label = field.Labels[groups[i].Key]
		}
	}
}

// grouped is a grouped answer's own half, before it reaches [Answer].
type grouped struct {
	Groups  []Group
	Dropped int
	Overlap bool
}

// readGroups assembles the columns one query asked for.
//
// TWO STATEMENTS PER AXIS rather than one windowed query: the counts are a
// GROUP BY over the predicate, and each column's rows are the ordinary paged
// statement narrowed to that column. A single window function would rank every
// row in the corpus to return twenty per column, which is what a board would
// pay on every poll.
func readGroups(ctx context.Context, tx *sql.Tx, q Query,
	fields map[string]resolvedField, where string, args []any,
	terms []sortTerm) (grouped, error) {

	axis, err := compileGroup(q.GroupBy, fields)
	if err != nil {
		return grouped{}, err
	}
	// THE GROUP FILTER IS ALREADY IN `where` — [compile] adds it, in its
	// join-free form, so the count hint and the totals are narrowed by it
	// too. Narrowing again here would double the predicate and, worse,
	// would leave those two describing the whole board.
	rowsPer := q.GroupLimit
	if rowsPer <= 0 {
		rowsPer = GroupRowsDefault
	}
	if rowsPer > GroupRowsMax {
		rowsPer = GroupRowsMax
	}

	groups, dropped, err := groupCounts(ctx, tx, axis, where, args, MaxGroups)
	if err != nil {
		return grouped{}, err
	}
	groupLabels(groups, q.GroupBy, fields)

	for i := range groups {
		rows, err := groupRows(ctx, tx, axis, groups[i].Key, where, args,
			terms, rowsPer)
		if err != nil {
			return grouped{}, err
		}
		groups[i].Rows = rows
		if q.GroupBy2 == "" {
			continue
		}
		// THE SECOND AXIS IS THE FIRST ONE AGAIN, inside this column.
		// One level and no more: a third would be a tree, and a board
		// draws columns and swimlanes rather than a hierarchy.
		inner, err := readSubgroups(ctx, tx, q, fields, axis,
			groups[i].Key, where, args, terms, rowsPer)
		if err != nil {
			return grouped{}, err
		}
		groups[i].Subgroups = inner
	}
	return grouped{Groups: groups, Dropped: dropped, Overlap: axis.Multi}, nil
}

// readSubgroups is the second axis within one column.
func readSubgroups(ctx context.Context, tx *sql.Tx, q Query,
	fields map[string]resolvedField, outer groupAxis, key string,
	where string, args []any, terms []sortTerm, rowsPer int) ([]Group, error) {

	inner, err := compileGroup(q.GroupBy2, fields)
	if err != nil {
		return nil, err
	}
	// THE OUTER COLUMN NARROWS THE INNER QUERY, and the outer axis's own
	// join rides with it — a subgroup of a tag column is still inside that
	// tag.
	outerClause, outerArgs := outer.joinedFilter(key)
	scoped := "(" + where + ") AND " + outerClause
	bound := append(append([]any{}, args...), outerArgs...)
	joined := groupAxis{
		Expr: inner.Expr, Join: outer.Join + inner.Join,
		Args:  append(append([]any{}, outer.Args...), inner.Args...),
		Multi: inner.Multi, Unset: inner.Unset,
	}
	// BOTH JOINS' ARGUMENTS RIDE ON THE AXIS, in statement order: the
	// count statement writes its joins before its WHERE, so the outer
	// join's binding comes first, then the inner's, then the predicate's.
	// A NAMED SUBGROUP NARROWS THE INNER AXIS, exactly as `group` narrows
	// the outer one — which is how a board loads one swimlane further.
	if q.Subgroup != "" {
		innerClause, innerArgs := inner.joinedFilter(q.Subgroup)
		scoped += " AND " + innerClause
		bound = append(append([]any{}, bound...), innerArgs...)
	}
	counts, _, err := groupCounts(ctx, tx, joined, scoped, bound, MaxGroups)
	if err != nil {
		return nil, err
	}
	groupLabels(counts, q.GroupBy2, fields)
	for i := range counts {
		rows, err := groupRows(ctx, tx, joined, counts[i].Key, scoped, bound,
			terms, rowsPer)
		if err != nil {
			return nil, err
		}
		counts[i].Rows = rows
	}
	return counts, nil
}

// declaredOrder renders a closed set's own sequence as plain strings.
//
// FROM THE SET ITSELF, never a list written again here: `Statuses`,
// `StatusGroups` and `Priorities` are each declared in the order they MEAN —
// todo before in_progress before done — and a second copy of that order is one
// that stops matching.
func declaredOrder[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}
