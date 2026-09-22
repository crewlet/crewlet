package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
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

// MaxGroupsWithSubgroups and MaxSubgroups bound a SWIMLANE board, which costs
// a different shape entirely.
//
// # The arithmetic, which the single-axis cap was never sized for
//
// One axis is `1 + G` statements: a count over the predicate and one paged
// read per column. A SECOND axis repeats that pattern inside every column, so
// it is `1 + G × (2 + S)` — at [MaxGroups] on both, 1 + 64 × 66 = 4 225
// ordered statements, each with two joins, inside one read transaction, for
// ONE board poll. Nothing else in this grammar multiplies like that, and
// [checkGroupBreadth] cannot see it: that gate measures the ROW count of a
// single pass and is exempt at project scope, where a swimlane board is
// exactly what somebody opens.
//
// # So the bound is on the CELLS, not on either axis alone
//
// Sixteen by sixteen is 256 cells, which is already more than a person reads
// at once — a board wide enough to need scrolling in both directions is a
// board nobody is using as a board. The first axis is therefore capped LOWER
// when a second one is asked for, and the count that did not fit is reported
// exactly as it is for a single axis ([grouped.Dropped] and
// [Group.SubgroupsDropped]) rather than silently cut.
//
// 1 + 16 × 18 = 289 statements, down from 4 225.
const (
	MaxGroupsWithSubgroups = 16
	MaxSubgroups           = 16
)

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

	// SubgroupsDropped is how many lanes this column has beyond
	// [MaxSubgroups]. Said rather than silently cut, on the same rule the
	// column overflow follows: a board that drew sixteen of two hundred
	// lanes and reported nothing looks like a company with sixteen.
	SubgroupsDropped int `json:"subgroups_dropped,omitempty"`
}

// groupAxis is one grouping axis compiled into SQL.
type groupAxis struct {
	// Expr is what the value is, and Join what it needs to reach it.
	Expr string
	Join string

	// ExprArgs and JoinArgs are what each of those two binds, and they
	// are SEPARATE because a placeholder binds in TEXTUAL order and the
	// two halves are written in different places: the expression appears
	// in the SELECT of the count statement and in the WHERE of every
	// narrowed one, the join always between them. Held as one list they
	// were correct only for an axis whose expression carried no values,
	// which every axis did until the relative due bands arrived with four.
	ExprArgs []any
	JoinArgs []any

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

	// Labels is what a person reads where the stored value is not the
	// word: a custom field's option ids, and the relative due bands'
	// slugs. Absent on every axis that stores the word already, because a
	// label derived where none is needed would be a second name for one
	// value.
	Labels map[string]string

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
		// THE EXPRESSION IS WRITTEN TWICE HERE, so whatever it binds is
		// bound twice: a second copy taking its arguments from the
		// predicate beside it is how an expression with values of its
		// own silently answers a different question.
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')",
			append(append([]any{}, a.ExprArgs...), a.ExprArgs...)
	}
	return a.Expr + " = ?", append(append([]any{}, a.ExprArgs...), key)
}

// compileGroup turns a grouping key into its axis.
//
// `window` is the query's own calendar, which only the relative due bands
// read — see [dayWindow].
func compileGroup(key string, fields map[string]resolvedField,
	window dayWindow) (groupAxis, error) {
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
			JoinArgs: []any{field.ID},
			Multi:    field.Multi,
			Unset:    "(not set)",
			// FROM THE LABEL MAP, never by inverting
			// [resolvedField.Options]: that one holds two keys per
			// option, so an inversion picks the slug or the name by
			// Go's randomised map iteration and a column heading
			// would differ between two requests to one node.
			Labels: field.Labels,
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
	case groupByDueBucket:
		return dueBucketAxis(window)
	}
	return groupAxis{}, fmt.Errorf("tracker: %q is not a grouping", key)
}

// groupByDueBucket is the grouping key for the relative due bands.
//
// A CONSTANT, unlike every axis key beside it, because the two places that
// name one are in different files here: a key in [groupKeys] the switch below
// does not compile parses cleanly and then fails the read it was accepted
// for.
const groupByDueBucket = "due:bucket"

// dayWindow is the calendar a relative grouping cuts on: the query's own day
// start, the instant that day ends, and the instant its week does.
//
// CARRIED FROM THE QUERY rather than derived in SQL from one instant, and
// [Query.DayEnd] says why: a day is not always 24 hours and a week is not
// always 168, so arithmetic on the day start lands somewhere the `due=`
// filters do not, twice a year.
type dayWindow struct {
	Start   time.Time
	DayEnd  time.Time
	WeekEnd time.Time
}

// dayWindow is the calendar this query resolved for itself.
func (q Query) dayWindow() dayWindow {
	return dayWindow{Start: q.DayStart, DayEnd: q.DayEnd, WeekEnd: q.WeekEnd}
}

// dueBucket is one band of [groupByDueBucket].
//
// # ONE BOUNDARY FOR ONE FACT
//
// "Overdue · Today · This week · Later" is the question somebody opens their
// own work to ask, and it is the one grouping a company cannot answer from a
// stored value: every other axis reads a column, and this one reads a column
// against a calendar. Computed by the reader it is cut on the READER's
// midnight and the reader's week, while the engine derives [TaskRow.Overdue]
// and compiles every `due=` filter against the COMPANY's day start — so for
// anybody whose local day differs from the company's, a task landed under
// "Earlier" on a row the same answer flagged as due today and not overdue.
// As an axis the bands, the flag and the filters are cut once, in one place,
// from [Query.DayStart] and the two boundaries beside it.
//
// # AND THERE ARE SIX BANDS, not the five a day is usually read in
//
// `overdue` means OPEN and past its date, so work that was finished late is
// past its date and not overdue: calling it Overdue would be a false claim
// about work somebody delivered, and calling it Today would invent a date
// nobody set. [dueEarlier] is where it belongs. It is empty on an answer that
// carries only open work — which is every answer that does not ask for
// `show_closed` — because nothing there can be in it.
type dueBucket string

const (
	dueOverdue  dueBucket = "overdue"
	dueEarlier  dueBucket = "earlier"
	dueToday    dueBucket = "today"
	dueThisWeek dueBucket = "this_week"
	dueLater    dueBucket = "later"

	// dueNone is a task with no due date, and it is the EMPTY key for the
	// reason every other axis's absent value is: [groupCounts] labels that
	// column from [groupAxis.Unset], and `group=` narrows to it by the
	// same spelling on every axis in the grammar.
	dueNone dueBucket = ""
)

// dueBands is the axis's DECLARED ORDER and the word each band is drawn
// under, in ONE table so the two can never disagree — and the two derived
// forms below are what the axis is actually built from, so neither is a
// second list to keep in step.
var dueBands = []struct {
	Key   dueBucket
	Label string
}{
	{dueOverdue, "Overdue"},
	{dueEarlier, "Earlier"},
	{dueToday, "Today"},
	{dueThisWeek, "This week"},
	{dueLater, "Later"},
	// NO PARENTHESES, unlike the `(no due date)` on `due:day`: there the
	// label is the parenthetical this grammar uses for a value simply
	// missing from an open set, and here it is the sixth HEADING of a
	// closed one, read in the same voice as the five above it.
	{dueNone, "No due date"},
}

// dueBandOrder and dueBandLabels are [dueBands] read the two ways the axis
// needs it, derived ONCE rather than per query.
var dueBandOrder, dueBandLabels = func() ([]string, map[string]string) {
	order := make([]string, 0, len(dueBands))
	labels := make(map[string]string, len(dueBands))
	for _, band := range dueBands {
		order = append(order, string(band.Key))
		labels[string(band.Key)] = band.Label
	}
	return order, labels
}()

// dueBucketAxis is [groupByDueBucket] compiled.
func dueBucketAxis(window dayWindow) (groupAxis, error) {
	// A ZERO CALENDAR IS REFUSED RATHER THAN CUT ON. Every boundary here
	// is a resolved instant and the zero one is 1 January year one, so a
	// window nobody resolved does not fail — it silently answers `later`
	// for every dated task in the company. [ParseQuery] resolves all
	// three; a [Query] built any other way has to as well.
	if window.Start.IsZero() || window.DayEnd.IsZero() || window.WeekEnd.IsZero() {
		return groupAxis{}, fmt.Errorf("tracker: group_by=%s needs the day, "+
			"day-end and week-end boundaries ParseQuery resolves, and this "+
			"query carries none", groupByDueBucket)
	}
	expr, args := dueBucketCase(window)
	return groupAxis{
		Expr: expr, ExprArgs: args,
		Order: dueBandOrder,
		// THE UNSET COLUMN'S LABEL COMES FROM THE SAME TABLE as the
		// other five, so the six headings are declared once.
		Unset:  dueBandLabels[string(dueNone)],
		Labels: dueBandLabels,
	}, nil
}

// dueBucketCase renders the bands as ONE self-contained CASE.
//
// SELF-CONTAINED AND JOIN-FREE, because `group=today` narrows the WHOLE query
// through [groupAxis.filter]: the count hint and the totals share that
// predicate and carry no join, so an axis that needed one would leave a header
// adding up the whole board while the rows showed a single band of it.
//
// THE ARMS ARE ORDERED AND EACH NARROWS WHAT THE ONE BEFORE IT LEFT. Past the
// first two every dated row is at or after the day start, so `< DayEnd` is
// today and `< WeekEnd` is the rest of this week. The first arm carries the
// open condition because that is what `overdue` MEANS — the same condition
// [openGroupsSQL] gives the `due=overdue` filter — and the second is what is
// left of "past its date": work that was finished late.
//
// On a Sunday the week ends where the day does, so the `this_week` arm matches
// nothing. That is the honest answer for a Monday-anchored week rather than a
// rolling seven days, which would band a task under "this week" that the
// `due=eow` bound beside it excludes.
//
// The bounds are MICROSECONDS, which is what [store.EncodeTime] writes.
func dueBucketCase(window dayWindow) (string, []any) {
	arm := func(bucket dueBucket) string { return " THEN '" + string(bucket) + "'" }
	return "CASE" +
			" WHEN t.due_at IS NULL" + arm(dueNone) +
			" WHEN t.due_at < ? AND t.status_group IN (" + openGroupsSQL + ")" +
			arm(dueOverdue) +
			" WHEN t.due_at < ?" + arm(dueEarlier) +
			" WHEN t.due_at < ?" + arm(dueToday) +
			" WHEN t.due_at < ?" + arm(dueThisWeek) +
			" ELSE '" + string(dueLater) + "' END",
		[]any{
			store.EncodeTime(window.Start), store.EncodeTime(window.Start),
			store.EncodeTime(window.DayEnd), store.EncodeTime(window.WeekEnd),
		}
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
	// IN STATEMENT ORDER: the expression is written in the SELECT, the
	// join after it, the predicate after that — and a placeholder binds
	// where it is written.
	bound := append(append([]any{}, axis.ExprArgs...), axis.JoinArgs...)
	bound = append(bound, args...)
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
	where string, args []any, terms []sortTerm, limit int,
	dayStart time.Time) ([]TaskRow, error) {

	// THE JOINED FORM HERE, not the join-free one: this statement already
	// carries the axis's join for the GROUP BY's sake, so comparing the
	// joined column is one predicate rather than a second subquery.
	clause, values := axis.joinedFilter(key)
	rows, _, err := readTasksJoined(ctx, tx, axis.Join, axis.JoinArgs,
		"("+where+") AND "+clause,
		append(append([]any{}, args...), values...), terms, limit, dayStart)
	return rows, err
}

// joinedFilter is the axis as a predicate on the column its own join reached.
//
// The expression's own bindings ride with it for the reason [groupAxis.filter]
// gives, and the empty key writes it twice there too.
func (a groupAxis) joinedFilter(key string) (string, []any) {
	if key == "" {
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')",
			append(append([]any{}, a.ExprArgs...), a.ExprArgs...)
	}
	return a.Expr + " = ?", append(append([]any{}, a.ExprArgs...), key)
}

// groupLabels fills in what a person reads where the stored value is not the
// word.
//
// FROM THE AXIS, which is the only frame that knows: a custom field's options
// are the company's own and a due band's slug is this package's, and a caller
// holding nothing but the grouping KEY would have to resolve the field again
// to find either. Every other axis leaves [groupAxis.Labels] empty, because a
// label derived where none is needed would be a second name for one value.
//
// THE UNSET COLUMN KEEPS WHAT [groupCounts] GAVE IT, which is why this only
// fills a label that is still empty: that one comes from [groupAxis.Unset] on
// every axis in the grammar.
func groupLabels(groups []Group, axis groupAxis) {
	if len(axis.Labels) == 0 {
		return
	}
	for i := range groups {
		if groups[i].Label == "" {
			groups[i].Label = axis.Labels[groups[i].Key]
		}
	}
}

// grouped is a grouped answer's own half, before it reaches [Answer].
type grouped struct {
	Groups  []Group
	Dropped int
	Overlap bool
}

// ErrTooBroad refuses a read whose input is wider than the answer can be
// built from.
//
// ITS OWN SENTINEL, because it is the one query refusal a caller repairs by
// NARROWING rather than by correcting what they typed: a surface maps it to
// "add a filter" and a retry with the same keys is pointless, where a
// malformed key is a mistake in the request itself.
var ErrTooBroad = errors.New("tracker: this query selects more rows than the answer can be built from")

// checkGroupBreadth refuses a grouping whose input is wider than the working
// set that draws it.
//
// # A BOUNDED COUNT, never a test for the presence of a filter key
//
// The gate this replaces refused a workspace grouping "without a narrowing
// filter — a status_group, an assignee, a unit or a date bound", and
// `status_group=not_started,active` satisfies that while narrowing nothing:
// every open task is already in it. A gate that tests a NAME is one a caller
// learns to satisfy in a single attempt without making the query any cheaper.
//
// So the gate is on CARDINALITY. [readGroups] runs one paged statement per
// column over this same predicate, and past [GroupByRowCeiling] that sorting
// working set crosses the page cache and spills — the count is what decides,
// so the count is what is measured.
//
// # Why it does not run at project scope
//
// `t.project_key = ?` drives `tracker_tasks_board_idx`, so the input is an
// index range whose width is one project's own size rather than the company's.
// The exemption is therefore about the PLAN rather than about the spelling of
// the container key — which is why what it tests is an absent project and not
// the literal `container=workspace`: an omitted container adds no predicate
// either (see [compile]) and scans exactly the same rows.
//
// # And why the refusal says "more than" rather than a number
//
// The count carries `LIMIT ceiling + 1`, so a company ten times past the bound
// pays for twenty thousand rows and not for its corpus. That bound is the
// whole point of the gate being cheap, and it means the honest thing to report
// is the ceiling that was crossed rather than a total nobody counted.
func checkGroupBreadth(ctx context.Context, tx *sql.Tx, q Query,
	where string, args []any) error {

	if q.Scope.Project != "" {
		return nil
	}
	query := `SELECT COUNT(*) FROM (SELECT 1 FROM tracker_tasks t WHERE ` +
		where + ` LIMIT ?)`
	var n int
	bound := append(append([]any{}, args...), GroupByRowCeiling+1)
	if err := tx.QueryRowContext(ctx, query, bound...).Scan(&n); err != nil {
		return fmt.Errorf("tracker: count what group_by=%s would sort: %w",
			q.GroupBy, err)
	}
	if n <= GroupByRowCeiling {
		return nil
	}
	return fmt.Errorf("tracker: group_by=%s over the whole company matches more "+
		"than %d tasks, and a board is drawn by sorting every one of them — "+
		"scope it with container=project:<key>, or add a filter that actually "+
		"excludes rows (assignee=, updated=, a date bound) rather "+
		"than one every open task already satisfies: %w",
		q.GroupBy, GroupByRowCeiling, ErrTooBroad)
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

	// THE GATE BEFORE THE WORK, and before the axis is even compiled: what
	// it refuses is the cost of the statements below, so paying any part of
	// that cost first would be paying exactly what it exists to avoid.
	if err := checkGroupBreadth(ctx, tx, q, where, args); err != nil {
		return grouped{}, err
	}
	axis, err := compileGroup(q.GroupBy, fields, q.dayWindow())
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

	// THE COLUMN CAP IS LOWER WHEN THERE ARE LANES — see
	// [MaxGroupsWithSubgroups]: the statement count is the PRODUCT of the
	// two axes, so bounding one alone bounds nothing.
	columns := MaxGroups
	if q.GroupBy2 != "" {
		columns = MaxGroupsWithSubgroups
	}
	groups, dropped, err := groupCounts(ctx, tx, axis, where, args, columns)
	if err != nil {
		return grouped{}, err
	}
	groupLabels(groups, axis)

	for i := range groups {
		rows, err := groupRows(ctx, tx, axis, groups[i].Key, where, args,
			terms, rowsPer, q.DayStart)
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
		inner, innerDropped, err := readSubgroups(ctx, tx, q, fields, axis,
			groups[i].Key, where, args, terms, rowsPer)
		if err != nil {
			return grouped{}, err
		}
		groups[i].Subgroups = inner
		groups[i].SubgroupsDropped = innerDropped
	}
	return grouped{Groups: groups, Dropped: dropped, Overlap: axis.Multi}, nil
}

// readSubgroups is the second axis within one column.
func readSubgroups(ctx context.Context, tx *sql.Tx, q Query,
	fields map[string]resolvedField, outer groupAxis, key string,
	where string, args []any, terms []sortTerm, rowsPer int) ([]Group, int, error) {

	inner, err := compileGroup(q.GroupBy2, fields, q.dayWindow())
	if err != nil {
		return nil, 0, err
	}
	// THE OUTER COLUMN NARROWS THE INNER QUERY, and the outer axis's own
	// join rides with it — a subgroup of a tag column is still inside that
	// tag.
	outerClause, outerArgs := outer.joinedFilter(key)
	scoped := "(" + where + ") AND " + outerClause
	bound := append(append([]any{}, args...), outerArgs...)
	joined := groupAxis{
		Expr: inner.Expr, ExprArgs: inner.ExprArgs,
		Join: outer.Join + inner.Join,
		JoinArgs: append(append([]any{}, outer.JoinArgs...),
			inner.JoinArgs...),
		Multi: inner.Multi, Unset: inner.Unset,
	}
	// BOTH JOINS' ARGUMENTS RIDE ON THE AXIS, in statement order: the
	// count statement writes its joins before its WHERE, so the outer
	// join's binding comes first, then the inner's, then the predicate's.
	// A NAMED SUBGROUP NARROWS THE INNER AXIS, exactly as `group` narrows
	// the outer one — which is how a board loads one swimlane further.
	if q.Subgroup != nil {
		innerClause, innerArgs := inner.joinedFilter(*q.Subgroup)
		scoped += " AND " + innerClause
		bound = append(append([]any{}, bound...), innerArgs...)
	}
	counts, dropped, err := groupCounts(ctx, tx, joined, scoped, bound, MaxSubgroups)
	if err != nil {
		return nil, 0, err
	}
	groupLabels(counts, inner)
	for i := range counts {
		rows, err := groupRows(ctx, tx, joined, counts[i].Key, scoped, bound,
			terms, rowsPer, q.DayStart)
		if err != nil {
			return nil, 0, err
		}
		counts[i].Rows = rows
	}
	return counts, dropped, nil
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
