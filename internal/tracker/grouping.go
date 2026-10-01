package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
//
// # A closed axis draws every column the query admits
//
// A `GROUP BY` emits one row per value PRESENT. On a company with one task
// that answered one column, and a board with one lane reads as a board that
// did not load: nothing on it says whether "In review" is empty or missing.
// So the FIRST axis, where it is a closed set — status, status group,
// priority and the relative due bands, the four with a declared
// [groupAxis.Order] — carries every value the query's own predicate admits,
// the absent ones at count 0 with no rows. The due bands are the one of the
// four whose values are not a stored column ([dueBucketAxis] computes them
// against the query's own day), which changes what ADMITS a band and nothing
// about the padding itself.
// The rule is the histogram's (every bucket is drawn, empty ones included,
// because a quiet hour is a fact about the company rather than a gap in the
// chart), and the admission is the predicate's own: an open-work board draws
// To do, In progress and In review and never Done, because the query excluded
// finished work, and a Done lane on it would claim "nothing is done" about a
// set that was never asked. [admittedColumns] names the filters that decide
// it, and why an open axis and the second axis are left as they are.
//
// # Two axes group on something other than their column
//
// Every axis above is its column's own value. The two UNIT axes are not: a
// unit answers to two spellings — its id and its name — and which one a row
// holds is whatever was true when it was filed, so grouping on the string
// drew one team as two columns. They group on the unit's KEY instead, folded
// in SQL, and a caller's `group=` is folded the same way so either spelling
// narrows to the one column. [unitAxis] is the expression and
// [groupAxis.Canonical] is the caller's half.

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
	// word: a custom field's option ids, the relative due bands' slugs,
	// and a unit key that is an id the chart chose to survive a rename.
	// Absent on every axis that stores the word already, because a label
	// derived where none is needed would be a second name for one value.
	//
	// A FUNCTION rather than a table, because the newest of the three is
	// not one: a unit's name comes from the CHART, which this package
	// deliberately does not hold, and the stored keys a board will group
	// by are not knowable before the read. A static table renders through
	// [labelsOf].
	Labels func(key string) string

	// Canonical spells a value the way [groupAxis.Expr] spells it.
	//
	// Only the unit axes have one, and it is what lets `group=` and
	// `subgroup=` take either of a team's spellings: that expression folds
	// every spelling onto the unit's KEY, so a narrowing written with the
	// name would compare against a value the column never emits — a board
	// with a column and a narrowing to it that answers nothing.
	//
	// It is applied to a key that came from an ANSWER too, where it is a
	// no-op: the key is already canonical and resolving one answers
	// itself. One rule over both, rather than two paths that have to agree
	// about which keys are which.
	//
	// Absent everywhere else, because every other axis's expression emits
	// the stored value and a caller's value is that value.
	Canonical func(key string) string

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
	key = a.canonical(key)
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

// canonical is [groupAxis.Canonical] where the axis has one, and the key
// itself where it does not.
//
// THE EMPTY KEY IS NEVER RESOLVED. It is the column of rows with NO value on
// this axis, on every axis — a state rather than a name — and handing it to a
// resolver would ask the chart for a unit called nothing.
func (a groupAxis) canonical(key string) string {
	if key == "" || a.Canonical == nil {
		return key
	}
	return a.Canonical(key)
}

// compileGroup turns a grouping key into its axis.
//
// `window` is the query's own calendar, which only the relative due bands
// read — see [dayWindow]; `units` is the chart the two unit axes label their
// columns from, and nil leaves each column reading as the key the rows hold.
func compileGroup(key string, fields map[string]resolvedField,
	window dayWindow, units Units) (groupAxis, error) {
	return compileAxis(key, "gv", fields, window, units)
}

// compileLane is [compileGroup] for the SECOND axis, the swimlanes.
//
// ITS OWN ALIAS, because a lane is read with the column's join in the same
// statement: two custom-field axes — `group_by=f.team&group_by2=f.risk` —
// each joined `tracker_field_values` as `gv`, and a statement carrying the
// alias twice is refused by the engine, so that board failed its read on
// every poll. Only the field axis names a join alias a second axis could
// repeat: `tag` is one axis and cannot be both, and every other axis reads a
// column of the task itself.
func compileLane(key string, fields map[string]resolvedField,
	window dayWindow, units Units) (groupAxis, error) {
	return compileAxis(key, "gl", fields, window, units)
}

// compileAxis is one axis with its field join under `alias`.
func compileAxis(key, alias string, fields map[string]resolvedField,
	window dayWindow, units Units) (groupAxis, error) {
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
		pin := " AND " + alias + ".seq = 0"
		if field.Multi {
			pin = ""
		}
		return groupAxis{
			Expr: alias + "." + column,
			Join: " LEFT JOIN tracker_field_values " + alias + " ON " + alias +
				".task_id = t.id AND " + alias + ".field_id = ? AND " +
				strings.ReplaceAll(liveFieldValue, "v.", alias+".") + pin,
			JoinArgs: []any{field.ID},
			Multi:    field.Multi,
			Unset:    "(not set)",
			// FROM THE LABEL MAP, never by inverting
			// [resolvedField.Options]: that one holds two keys per
			// option, so an inversion picks the slug or the name by
			// Go's randomised map iteration and a column heading
			// would differ between two requests to one node.
			Labels: labelsOf(field.Labels),
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
		return unitAxis("t.filed_unit", units), nil
	case "routing_unit":
		return unitAxis("t.routing_unit", units), nil
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
		Labels: labelsOf(dueBandLabels),
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
	key = a.canonical(key)
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
	if axis.Labels == nil {
		return
	}
	for i := range groups {
		if groups[i].Label == "" {
			groups[i].Label = axis.Labels(groups[i].Key)
		}
	}
}

// labelsOf renders a static table as an axis lookup, and nil for an empty
// one — which is what [groupLabels] reads as "this axis stores the word".
func labelsOf(table map[string]string) func(string) string {
	if len(table) == 0 {
		return nil
	}
	return func(key string) string { return table[key] }
}

// unitAxis is a unit column grouped on the TEAM rather than on the string.
//
// A unit answers to two spellings — its id and its name — and which one a row
// holds is decided by when it was written, because a filed unit is a record of
// what was true and nothing rewrites it. Grouped on the column, a company that
// gave a team an id therefore drew that ONE team as TWO columns, both headed
// with its name, splitting its counts down the middle: everything filed before
// the id under one, everything after it under the other. That is the defect
// the `unit=` filter was fixed for, one surface along.
//
// So the expression folds every spelling onto the unit's KEY, in SQL, with the
// spellings BOUND rather than written into the statement — a unit's name is
// prose a founder typed, and prose reaching a statement as text is how an
// injection gets in. One arm per unit that carries an id, two bound values
// each; the expression appears at most three times in one statement (the
// SELECT, the narrowing and a subgroup's), so the driver's own variable
// ceiling is reached at a few hundred id-carrying units — an order of
// magnitude past any org chart, and a loud refusal rather than a wrong answer
// if one ever arrives.
//
// A COMPANY THAT SET NO IDS GETS NO CASE AT ALL. Its every key IS its name, so
// every arm would be `WHEN x THEN x` — which the ELSE already answers — and
// the expression is the bare column it has always been. Same for a nil chart,
// which is the honest state of a process holding no org.
//
// A STORED UNIT THE CHART NO LONGER HAS KEEPS ITS OWN COLUMN, through the
// ELSE, under the literal the rows hold. It is a team that has left the chart:
// folding it into anything would be inventing a home for work whose team is
// gone, and [unitLabels] is what says so on the heading.
func unitAxis(column string, units Units) groupAxis {
	axis := groupAxis{Expr: column, Unset: "(no unit)", Labels: unitLabels(units)}
	if units == nil {
		return axis
	}
	var arms strings.Builder
	for _, unit := range units.AllUnits() {
		key, name := strings.TrimSpace(unit.Key), strings.TrimSpace(unit.Name)
		if key == "" || name == "" || key == name {
			continue
		}
		arms.WriteString(" WHEN ? THEN ?")
		axis.ExprArgs = append(axis.ExprArgs, name, key)
	}
	if arms.Len() > 0 {
		axis.Expr = "CASE " + column + arms.String() + " ELSE " + column + " END"
	}
	// AND A CALLER'S OWN VALUE IS SPELLED THE WAY THE EXPRESSION SPELLS
	// IT, so `group=Engineering` and `group=eng` narrow to the one column
	// the board draws — see [groupAxis.Canonical].
	axis.Canonical = func(key string) string {
		if unit, found := units.ResolveUnit(key); found {
			return unit.Key
		}
		return key
	}
	return axis
}

// unitLabels is what a person reads on a unit column.
//
// A COLUMN HEADING IS READ BY A PERSON and what these rows hold is the unit's
// KEY, which is an id on any company that gave its units one — a word chosen
// to survive a rename precisely because nobody reads it. Without this, giving
// a unit an id silently re-headed every board in the company with a slug.
//
// THE CHART IS ASKED PER KEY rather than enumerated, because the keys are
// whatever the rows hold: a column may name a team the chart no longer has,
// or a spelling it no longer uses, and both are legitimate on a record of
// what was true.
//
// AN EMPTY LABEL IS THE ANSWER FOR AN UNRESOLVED KEY, never a stand-in like
// "(unknown)": a client renders the label or falls back to the key, so a
// column whose unit has left the chart keeps reading as the value that is
// still in the rows — which is also what a filter on that column takes.
func unitLabels(units Units) func(string) string {
	if units == nil {
		return nil
	}
	return func(key string) string {
		unit, found := units.ResolveUnit(key)
		if !found {
			return ""
		}
		return unit.Name
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
	axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
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
	// COUNTS, THEN FILL, THEN LABELS. The fill mints a column from a bare
	// KEY, so it has to run before the labels or a band drawn at zero would
	// be headed by its slug where the one beside it carries a word — and
	// the unset column a closed axis declares is minted here rather than
	// read out of [groupCounts], which only ever saw the values something
	// was counted under. Its heading comes from the same table as its
	// neighbours' ([dueBucketAxis] sets [groupAxis.Unset] and the `""` key
	// of [groupAxis.Labels] from one list), so the two spellings of that
	// one label cannot disagree.
	groups = fillColumns(groups, admittedColumns(q, axis))
	groupLabels(groups, axis)

	for i := range groups {
		if groups[i].Count == 0 {
			// A COLUMN THE FILL ADDED HOLDS NOTHING BY CONSTRUCTION — see
			// [admittedColumns] — so neither its rows nor its lanes are
			// read: a statement per empty column would cost a board with
			// one task as much as one with six.
			continue
		}
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

	inner, err := compileLane(q.GroupBy2, fields, q.dayWindow(), q.Units)
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

// admittedColumns is the closed axis's values THIS query could hold, in the
// axis's declared order — the columns a board draws whether or not anything is
// in them. Nil for an open axis, which keeps only the values present.
//
// ONLY THE COLUMNS THE PREDICATE ADMITS. An empty column is honest exactly
// where a row could have landed in it, so the same filters that narrow the
// predicate narrow this set, in the terms [compileWhere] writes them: the
// `status` and `status!` keys, `status_group`, the finished-work default that
// `show_closed` lifts, and the overdue alias that carries an open-status
// condition of its own. The `group=` column filter narrows the whole query to
// one value, so it narrows this to one column — an empty one is still that
// column, which is what the reader who followed "N more →" into it is looking
// at. A disjunction's arms can only narrow further, so the top-level keys
// bound what any arm can produce.
//
// CLOSED AXES ONLY. An open axis — assignee, tag, type, a custom field — has no
// set to fill from: every seat in the company as an empty column is a roster
// rather than a board, and a field's option list is a catalogue read this
// statement does not make. [groupAxis.Order] is what marks an axis closed, and
// compileGroup sets it on exactly four — status, status group, priority and
// the relative due bands; an axis that gains an order without gaining an arm
// here is caught by the internal test that walks groupKeys.
//
// `group=` IS READ BY PRESENCE, not by emptiness — see [Query.Group]. A
// request that NAMES the key narrows this to the one column it names, and the
// empty string names the column holding the rows with no value: that is a
// declared band on the due axis ("No due date") and is not a declared value on
// the other three, where a status or a priority is never empty. So a
// present-empty `group=` on those admits nothing and pads nothing, which is
// the honest answer — the narrowed board has no declared column to draw.
//
// NOT THE SECOND AXIS. A swimlane is a split of one column's rows — the
// grammar refuses `group_by2` without `group_by` for that reason — and an
// empty lane inside every column would multiply exactly the cells
// [MaxGroupsWithSubgroups] exists to bound.
func admittedColumns(q Query, axis groupAxis) []string {
	if len(axis.Order) == 0 {
		return nil
	}
	var admitted []string
	switch q.GroupBy {
	case "status":
		admitted = declaredOrder(admittedStatuses(q))
	case "status_group":
		held := map[StatusGroup]bool{}
		for _, s := range admittedStatuses(q) {
			held[s.Group()] = true
		}
		for _, group := range StatusGroups {
			if held[group] {
				admitted = append(admitted, string(group))
			}
		}
	case "priority":
		for _, p := range Priorities {
			if len(q.Priorities) > 0 && !slices.Contains(q.Priorities, p) {
				continue
			}
			admitted = append(admitted, string(p))
		}
	case groupByDueBucket:
		admitted = admittedDueBands(q)
	default:
		return nil
	}
	if q.Group == nil {
		return admitted
	}
	if slices.Contains(admitted, *q.Group) {
		return []string{*q.Group}
	}
	return nil
}

// admittedDueBands is the relative due axis's own arm of [admittedColumns].
//
// THE BANDS ARE A CALENDAR AND A STATUS, so two different filters narrow them
// and each narrows a different half:
//
//   - [dueOverdue] and [dueEarlier] share one interval — everything before the
//     day start — and are told apart by the status: overdue is OPEN and past
//     its date, earlier is what is left, which is work somebody finished late.
//     So each is admitted only where [admittedStatuses] leaves a status of its
//     own kind, which is the same computation the other three arms rest on
//     rather than a second reading of `show_closed`. An open-work board draws
//     Overdue and never Earlier; a board narrowed to `status=done` draws
//     Earlier and never Overdue.
//   - The `due=` filter narrows the INTERVAL, and only that filter: the other
//     eight date keys bound a different column, so a `created=` bound says
//     nothing about which due band a row can be in. A band is admitted where
//     its own interval intersects the filter's, computed against the same
//     three boundaries [dueBucketCase] cuts on, so the arm that could match a
//     row and the column that draws it are the same arithmetic.
//
// THE OVERDUE ALIAS IS BOTH AT ONCE — `due=overdue` is "before the day start
// AND open" — so it admits exactly [dueOverdue] rather than the two bands its
// interval alone would give.
//
// AND `none` IS NEVER ADMITTED UNDER A `due=` FILTER. The band is the rows
// whose `due_at` is NULL, and every comparison in this grammar is written
// `due_at IS NOT NULL AND …` for the index's sake — so no `due=` value can
// hold an undated task, and the grammar has no spelling that asks for one
// (`due=none` is refused as a comparison). Drawing the column would claim
// nothing is undated about a set undated work was never in.
func admittedDueBands(q Query) []string {
	open, finished := false, false
	for _, s := range admittedStatuses(q) {
		if s.Group().Open() {
			open = true
			continue
		}
		finished = true
	}
	filter, dated := q.Dates["due"]
	window := q.dayWindow()
	var out []string
	for _, band := range dueBands {
		switch {
		// The STATUS half, which holds whether or not a date filter is on.
		case band.Key == dueOverdue && !open:
		case band.Key == dueEarlier && !finished:
		// The CALENDAR half, which only a `due=` filter narrows.
		case dated && band.Key == dueNone:
		// THE ALIAS CARRIES THE OPEN CONDITION, so it admits the one
		// band that is defined by it and no other — not even
		// [dueEarlier], which shares its interval exactly.
		case dated && filter.Overdue && band.Key != dueOverdue:
		case dated && !filter.Overdue &&
			!bandRange(band.Key, window).overlaps(dueFilterRange(filter)):
		default:
			out = append(out, string(band.Key))
		}
	}
	return out
}

// dueRange is a half-open interval of instants, [Lo, Hi). A zero Lo is the
// beginning of time and a zero Hi is for ever, which is the sentinel every
// boundary here already uses — [dueBucketAxis] refuses a window whose
// boundaries are zero, so a real one is never mistaken for an open end.
type dueRange struct{ Lo, Hi time.Time }

// overlaps reports whether two of these hold an instant in common.
//
// A DEGENERATE INTERVAL IS THE INSTANT IT SITS AT. `this_week` is [DayEnd,
// WeekEnd) and on a Sunday the week ends exactly where the day does, so the
// band is empty — and read as empty it would drop out of every filtered board
// on one day in seven while an unfiltered board still drew it. Read as the
// instant it collapsed to, a filter covering that boundary admits the lane and
// the board keeps its shape all week.
func (r dueRange) overlaps(o dueRange) bool {
	hi := func(x dueRange) time.Time {
		if !x.Lo.IsZero() && !x.Hi.IsZero() && !x.Hi.After(x.Lo) {
			return x.Lo.Add(time.Microsecond)
		}
		return x.Hi
	}
	rHi, oHi := hi(r), hi(o)
	if !r.Lo.IsZero() && !oHi.IsZero() && !r.Lo.Before(oHi) {
		return false
	}
	if !o.Lo.IsZero() && !rHi.IsZero() && !o.Lo.Before(rHi) {
		return false
	}
	return true
}

// bandRange is one band's own interval, which is [dueBucketCase]'s arms read
// as bounds rather than as SQL. [dueNone] has none — it is a NULL rather than
// an instant — and answers the empty interval nothing intersects.
func bandRange(band dueBucket, window dayWindow) dueRange {
	switch band {
	case dueOverdue, dueEarlier:
		return dueRange{Hi: window.Start}
	case dueToday:
		return dueRange{Lo: window.Start, Hi: window.DayEnd}
	case dueThisWeek:
		return dueRange{Lo: window.DayEnd, Hi: window.WeekEnd}
	case dueLater:
		return dueRange{Lo: window.WeekEnd}
	}
	return dueRange{}
}

// dueFilterRange is one `due=` filter read as the same kind of interval.
//
// AN INCLUSIVE BOUND IS BUMPED BY A MICROSECOND rather than carried as a flag,
// because that is the unit the column is stored in — [store.EncodeTime] writes
// microseconds — so `lte:X` and `< X+1µs` admit exactly the same rows. Written
// as a flag the comparison below would have four cases where it has one.
func dueFilterRange(filter DateFilter) dueRange {
	switch filter.Op {
	case DateLT:
		return dueRange{Hi: filter.From.At}
	case DateLTE:
		return dueRange{Hi: filter.From.At.Add(time.Microsecond)}
	case DateGT:
		return dueRange{Lo: filter.From.At.Add(time.Microsecond)}
	case DateGTE:
		return dueRange{Lo: filter.From.At}
	case DateRange:
		return dueRange{Lo: filter.From.At, Hi: filter.To.At}
	}
	// A comparison this build does not know bounds nothing, which admits
	// every band: a filter nobody could read must not silently remove a
	// column somebody's rows are in.
	return dueRange{}
}

// admittedStatuses is every status the query's own predicate could match.
//
// THE FINISHED-WORK RULE IS [compileWhere]'S, restated in the same three terms
// so the two cannot disagree: finished work is excluded unless `show_closed`
// asks for it, and the overdue alias ANDs the open condition back on whatever
// `show_closed` said.
func admittedStatuses(q Query) []Status {
	finished := q.ShowClosed.Finished()
	for _, filter := range q.Dates {
		if filter.Overdue {
			finished = false
		}
	}
	var out []Status
	for _, s := range Statuses {
		switch {
		case len(q.Status) > 0 && !slices.Contains(q.Status, s):
		case slices.Contains(q.StatusNot, s):
		case len(q.StatusGroups) > 0 && !slices.Contains(q.StatusGroups, s.Group()):
		case !finished && !s.Group().Open():
		default:
			out = append(out, s)
		}
	}
	return out
}

// fillColumns lays the present columns over the admitted set, in the admitted
// order, minting an empty column for every admitted value nothing was counted
// under.
//
// AN EMPTY COLUMN CARRIES AN EMPTY LIST, never nil: `rows` is not `omitempty`,
// so nil would reach the wire as `null`, and a renderer that maps a column's
// rows — every one of them — would fall over on exactly the column this
// exists to draw. A present value the admitted set does not name — a status a
// newer peer's record wrote — keeps its place at the end rather than
// vanishing: the predicate admitted it, and a column with rows in it is never
// the one to drop.
func fillColumns(present []Group, admitted []string) []Group {
	if len(admitted) == 0 {
		return present
	}
	held := make(map[string]int, len(present))
	for i, group := range present {
		held[group.Key] = i
	}
	out := make([]Group, 0, len(admitted)+len(present))
	used := make([]bool, len(present))
	for _, key := range admitted {
		if i, ok := held[key]; ok {
			out = append(out, present[i])
			used[i] = true
			continue
		}
		out = append(out, Group{Key: key, Rows: []TaskRow{}})
	}
	for i, group := range present {
		if !used[i] {
			out = append(out, group)
		}
	}
	return out
}
