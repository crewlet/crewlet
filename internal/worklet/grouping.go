package worklet

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

// Grouping: the board's columns. A grouped answer is GROUPS, each with its own
// count over the whole set and a bounded slice of rows: rows with a label on
// each would make a column's count a fact about the page. [Answer.Rows] is
// EMPTY when a query groups, so no caller draws the rows twice, once without
// columns.
//
// A grouped answer mints no cursor: across groups there is no single order to
// be after, so it says how many rows each group holds back, and a board loads
// one column further with `group=<value>`, an ordinary flat query with an
// ordinary cursor. Why counts and rows are separate statements is at
// [readGroups]; which empty columns a closed axis draws, at [admittedColumns];
// how a unit axis resolves a caller's value, at [unitAxis].

// MaxGroups bounds how many columns one answer draws. Sixty-four is above
// every closed set grouped on (six statuses, four status groups, five
// priorities), so it bounds only the open ones; what it leaves out is counted
// on the answer.
const MaxGroups = 64

// MaxGroupsWithSubgroups and MaxSubgroups bound a SWIMLANE board, whose cost
// multiplies: one axis is 1 + G statements, two are 1 + G × (2 + S), so 4 225
// at [MaxGroups] on both, in one read transaction per poll. [checkGroupBreadth]
// cannot see that (it measures one pass's rows and is exempt at project scope,
// where swimlanes are opened), so the bound is on CELLS: 16 × 16 is already
// more than a person reads, and 1 + 16 × 18 = 289 statements. What did not fit
// is reported ([grouped.Dropped], [Group.SubgroupsDropped]), never cut silently.
const (
	MaxGroupsWithSubgroups = 16
	MaxSubgroups           = 16
)

// GroupRowsDefault and GroupRowsMax bound how many rows one column carries:
// twenty is what a column shows before scrolling, per COLUMN since a board
// draws several, and a hundred is the ceiling for rendering a whole column.
const (
	GroupRowsDefault = 20
	GroupRowsMax     = 100
)

// Group is one column of a grouped answer.
type Group struct {
	// Key is the stored value the rows share (a status slug, a handle, an
	// option id) and Label what a person reads, where the two differ.
	Key   string `json:"key"`
	Label string `json:"label,omitempty" untrusted:"name"`

	// Count is over the WHOLE set, never over Rows.
	Count int `json:"count"`

	Rows []TaskRow `json:"rows"`

	// Subgroups is the second axis, when one was asked for.
	Subgroups []Group `json:"subgroups,omitempty"`

	// SubgroupsDropped is how many lanes this column has beyond
	// [MaxSubgroups], said rather than cut, as a column overflow is.
	SubgroupsDropped int `json:"subgroups_dropped,omitempty"`
}

// groupAxis is one grouping axis compiled into SQL.
type groupAxis struct {
	// Expr is what the value is, and Join what it needs to reach it.
	Expr string
	Join string

	// ExprArgs and JoinArgs are what each binds, SEPARATE because a
	// placeholder binds in textual order and the two are written apart:
	// the expression in the count's SELECT and every narrowed WHERE, the
	// join always between them.
	ExprArgs []any
	JoinArgs []any

	// Multi marks an axis on which one task is in SEVERAL groups (a tag, a
	// multi-valued field). Not an error, but the counts do not sum to the
	// answer's total, and the answer says so.
	Multi bool

	// Unset labels the column of rows with no value: "nobody is assigned"
	// is a column a board draws, not a row it hides.
	Unset string

	// Order is a closed set's DECLARED order (todo before in_progress
	// before done): ordering by size would reshuffle the board whenever
	// work moved. An open set has none and puts the largest column first.
	Order []string

	// Labels is what a person reads where the stored value is not the word
	// (a field's option ids, the due bands' slugs, a unit key); nil where
	// it is, so one value has one name. A FUNCTION because a unit's name
	// comes from the chart, which this package does not hold, for keys
	// known only after the read; a static table renders through [labelsOf].
	Labels func(key string) string

	// ParentLabels marks the axis whose value is another TASK's id: its
	// columns are headed from that task's row ([labelParents]), which only
	// the read's transaction reaches, so it cannot be a [groupAxis.Labels].
	ParentLabels bool

	// Canonical spells a caller's value the way [groupAxis.Expr] does. Only
	// the unit axes have one: the column holds the unit's KEY, so a
	// `group=` or `subgroup=` written with the name would narrow to
	// nothing. It is applied to a key from an answer too, where it is a
	// no-op, so one rule covers both.
	Canonical func(key string) string

	// Exists is the JOIN-FREE form of this axis as a predicate, a probe of
	// each task's own rows by their key ([groupAxis.filter]). `group=`
	// narrows the whole query with it because the count hint and the
	// totals share that predicate and carry no join: a join-only axis would
	// leave a header adding up the whole board while the rows showed one
	// column. It also narrows every statement reading one column or lane
	// ([columnSet]).
	Exists func(key string) (string, []any)
}

// filter renders this axis as a predicate on one value.
func (a groupAxis) filter(key string) (string, []any) {
	key = a.canonical(key)
	if a.Exists != nil {
		return a.Exists(key)
	}
	if key == "" {
		// THE EXPRESSION IS WRITTEN TWICE, so its bindings are bound
		// twice; borrowing the predicate's would answer another question.
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')",
			append(append([]any{}, a.ExprArgs...), a.ExprArgs...)
	}
	return a.Expr + " = ?", append(append([]any{}, a.ExprArgs...), key)
}

// canonical is [groupAxis.Canonical] where the axis has one, else the key.
// The EMPTY key is never resolved: it is the column of rows with no value, a
// state rather than a name.
func (a groupAxis) canonical(key string) string {
	if key == "" || a.Canonical == nil {
		return key
	}
	return a.Canonical(key)
}

// compileGroup turns a grouping key into its axis. `window` is the query's own
// calendar, read only by the due bands ([dayWindow]); `units` is the chart the
// unit axes label from, and nil leaves each column reading as its key.
//
// A field's join has ONE ALIAS on both axes of a lane board, because no
// statement carries two joins: a lane is counted with its own join inside a
// column narrowed by the join-free predicate ([groupAxis.filter]), and a run's
// rows by both axes' join-free predicates. [TestTwoFieldAxesDrawASwimlaneBoard]
// holds it.
func compileGroup(key string, fields map[string]resolvedField,
	window dayWindow, units Units) (groupAxis, error) {
	const alias = "gv"
	if ref, ok := strings.CutPrefix(key, FieldKeyPrefix); ok && ref != "" {
		field, held := fields[ref]
		if !held {
			return groupAxis{}, fmt.Errorf("worklet: group_by names f.%s and "+
				"no field resolved to it", ref)
		}
		column := FieldValueColumn(field.Type)
		// A single-valued field pins `seq = 0`, as a field SORT does. A
		// MULTI-VALUED one is a label board like `tag`: unpinned, a task
		// with three values is on three columns and the axis declares the
		// overlap; pinned, every task would sit quietly under its first.
		pin := " AND " + alias + ".seq = 0"
		if field.Multi {
			pin = ""
		}
		return groupAxis{
			Expr: alias + "." + column,
			Join: " LEFT JOIN tracker_field_values " + alias + " INDEXED BY " +
				fieldValuesKey + " ON " + alias +
				".task_id = t.id AND " + alias + ".field_id = ? AND " +
				strings.ReplaceAll(liveFieldValue, "v.", alias+".") + pin,
			JoinArgs: []any{field.ID},
			Multi:    field.Multi,
			Unset:    "(not set)",
			// FROM THE LABEL MAP, never by inverting
			// [resolvedField.Options]: it holds two keys per option,
			// so an inversion would pick by map order and a heading
			// would differ between two requests.
			Labels: labelsOf(field.Labels),
			Exists: func(key string) (string, []any) {
				inner := "SELECT 1 FROM tracker_field_values v " +
					"INDEXED BY " + fieldValuesKey + " " +
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
		// THE KEY IS AN ID: each column is headed from the parent's own row
		// ([labelParents]), since the parent need not be in the answer.
		return groupAxis{
			Expr: "COALESCE(t.parent_id, '')", Unset: "(no parent)", ParentLabels: true,
		}, nil
	case "tag":
		// MULTI-VALUED by its join: a task with three labels is on three
		// columns.
		return groupAxis{
			Expr:  "gt.slug",
			Join:  " LEFT JOIN tracker_task_tags gt ON gt.task_id = t.id",
			Multi: true, Unset: "(untagged)",
			// THE PROBE NAMES THE TAG TABLE'S KEY, as [anchorProbe] does
			// the task table's: with no statistics the planner seeks on
			// `slug = ?` and reads every task carrying the tag once per
			// row it narrows.
			Exists: func(key string) (string, []any) {
				inner := "SELECT 1 FROM tracker_task_tags g INDEXED BY " +
					tagsKey + " WHERE g.task_id = t.id"
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
	return groupAxis{}, fmt.Errorf("worklet: %q is not a grouping", key)
}

// groupByDueBucket is the grouping key for the relative due bands, a constant
// because it is named in more than one file: a key in [groupKeys] that
// [compileGroup] does not compile parses cleanly and then fails its read.
const groupByDueBucket = "due:bucket"

// dayWindow is the calendar a relative grouping cuts on: the query's day
// start, that day's end and its week's end. Carried from the query rather
// than derived in SQL ([Query.DayEnd] says why): arithmetic on the day start
// lands where the `due=` filters do not, twice a year.
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
// The bands are an axis rather than the reader's computation so that they,
// [TaskRow.Overdue] and the `due=` filters are cut once, on the COMPANY's
// [Query.DayStart] and the boundaries beside it; cut on the reader's
// midnight, a task could land under "Earlier" while flagged due today.
//
// There are SIX bands: `overdue` means OPEN and past its date, so work
// finished late is [dueEarlier], neither Overdue (a false claim about
// delivered work) nor Today (a date nobody set). Earlier is empty on an
// open-work answer, which is every one without `show_closed`.
type dueBucket string

const (
	dueOverdue  dueBucket = "overdue"
	dueEarlier  dueBucket = "earlier"
	dueToday    dueBucket = "today"
	dueThisWeek dueBucket = "this_week"
	dueLater    dueBucket = "later"

	// dueNone is a task with no due date, the EMPTY key like every axis's
	// absent value: [groupCounts] labels it from [groupAxis.Unset] and
	// `group=` narrows to it by the same spelling.
	dueNone dueBucket = ""
)

// dueBands is the axis's declared order and each band's heading in ONE table,
// from which the axis's two forms below are derived.
var dueBands = []struct {
	Key   dueBucket
	Label string
}{
	{dueOverdue, "Overdue"},
	{dueEarlier, "Earlier"},
	{dueToday, "Today"},
	{dueThisWeek, "This week"},
	{dueLater, "Later"},
	// NO PARENTHESES, unlike `(no due date)` on `due:day`: that marks a
	// value missing from an open set, this is a closed set's sixth heading.
	{dueNone, "No due date"},
}

// dueBandOrder and dueBandLabels are [dueBands] read the two ways the axis
// needs, derived once.
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
	// A ZERO CALENDAR IS REFUSED: the zero instant is year one, so an
	// unresolved window would silently band every dated task `later`.
	// [ParseQuery] resolves all three; a [Query] built otherwise must too.
	if window.Start.IsZero() || window.DayEnd.IsZero() || window.WeekEnd.IsZero() {
		return groupAxis{}, fmt.Errorf("worklet: group_by=%s needs the day, "+
			"day-end and week-end boundaries ParseQuery resolves, and this "+
			"query carries none", groupByDueBucket)
	}
	expr, args := dueBucketCase(window)
	return groupAxis{
		Expr: expr, ExprArgs: args,
		Order: dueBandOrder,
		// From the same table as the other five headings.
		Unset:  dueBandLabels[string(dueNone)],
		Labels: labelsOf(dueBandLabels),
	}, nil
}

// dueBucketCase renders the bands as ONE self-contained, join-free CASE,
// because `group=today` narrows the whole query through [groupAxis.filter]
// ([groupAxis.Exists] says why that carries no join).
//
// The arms are ORDERED, each narrowing what the last left: past the first two
// every dated row is at or after the day start. The first carries the open
// condition because that is what `overdue` means (as [openGroupsSQL] does for
// `due=overdue`); the second is the rest of "past its date", work finished
// late. On a Sunday `this_week` matches nothing, right for a Monday-anchored
// week: a rolling seven days would band a task the `due=eow` bound excludes.
// The bounds are MICROSECONDS, as [store.EncodeTime] writes.
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

// dayBucket renders an instant column as its calendar day in UTC: the grammar
// carries no zone on a grouping, and each node's local midnight would put one
// task on two days on two machines. The column holds MICROSECONDS
// (store.EncodeTime) and SQLite's date functions take seconds.
func dayBucket(column string) string {
	return `COALESCE(STRFTIME('%Y-%m-%d', ` + column + ` / 1000000, 'unixepoch'), '')`
}

// weekBucket is the same at week granularity, Monday-anchored. `%W` numbers
// from the year's first Monday, so earlier days are week 00 rather than the
// last ISO week, which is fine for a heading; `%V` and `%G` are not in every
// Turso build, and an unsupported specifier renders as itself, not an error.
func weekBucket(column string) string {
	return `COALESCE(STRFTIME('%Y-W%W', ` + column + ` / 1000000, 'unixepoch'), '')`
}

// groupCounts reads one axis's distinct values and their counts.
func groupCounts(ctx context.Context, tx *sql.Tx, axis groupAxis,
	set taskSet, limit int) ([]Group, int, error) {

	query, bound := groupCountsStatement(axis, set)
	rows, err := tx.QueryContext(ctx, query, append(bound, limit+1)...)
	if err != nil {
		return nil, 0, fmt.Errorf("worklet: count the groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Group
	for rows.Next() {
		var key sql.NullString
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, 0, fmt.Errorf("worklet: scan a group: %w", err)
		}
		group := Group{Key: key.String, Count: count}
		if group.Key == "" {
			// AN ABSENT VALUE IS ITS OWN COLUMN ([groupAxis.Unset]).
			group.Label = axis.Unset
		}
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("worklet: walk the groups: %w", err)
	}
	dropped := 0
	if len(out) > limit {
		// THE OVERFLOW IS COUNTED AND SAID: 64 of 200 columns drawn
		// silently would read as a company with 64 assignees.
		dropped = len(out) - limit
		out = out[:limit]
	}
	return out, dropped, nil
}

// groupCountsStatement is the statement [groupCounts] counts an axis's
// columns with, and its arguments up to its final LIMIT; named for the plan
// test.
func groupCountsStatement(axis groupAxis, set taskSet) (string, []any) {
	// [groupAxis.Order]'s CASE is built from the closed set itself, so no
	// caller's value is composed into SQL.
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
	          FROM ` + set.from + axis.Join + `
	          WHERE ` + set.where + `
	          GROUP BY g
	          ORDER BY ` + order + `
	          LIMIT ?`
	// IN STATEMENT ORDER (expression, join, predicate): a placeholder
	// binds where it is written.
	bound := append(append([]any{}, axis.ExprArgs...), axis.JoinArgs...)
	return query, append(bound, set.args...)
}

// groupRows reads one group's own page: the ordinary flat statement narrowed
// to one value ([columnSet]), so one sort and one row shape serve both.
func groupRows(ctx context.Context, tx *sql.Tx, axis groupAxis, key string,
	set taskSet, terms []sortTerm, limit int,
	dayStart time.Time) ([]TaskRow, error) {

	rows, _, err := readTasks(ctx, tx, columnSet(axis, key, set), terms,
		limit, dayStart)
	return rows, err
}

// columnSet is a set narrowed to one column of an axis — what a column's or a
// lane's rows are read from, and what a run of the drawing order counts and
// steps through ([groupAxis.run]).
//
// It narrows by the JOIN-FREE probe ([groupAxis.filter]), not the axis's join:
// for a field or tag axis a narrowed join is an inner join onto a child table
// keyed by the task, which a planner with no statistics may drive from either
// side, walking every value of a field, or the whole task table once per task
// carrying a tag. A probe is one seek per task read, whichever way the rest of
// the predicate leans. Only a statement that COUNTS the axis's values keeps
// the join ([groupCountsStatement]).
func columnSet(axis groupAxis, key string, set taskSet) taskSet {
	clause, values := axis.filter(key)
	return set.and(clause, values)
}

// joinedFilter is the axis as a predicate on the column its own join reached,
// narrowing a statement that carries the join to count the axis's values
// ([readSubgroups]). Its bindings ride as in [groupAxis.filter].
func (a groupAxis) joinedFilter(key string) (string, []any) {
	key = a.canonical(key)
	if key == "" {
		return "(" + a.Expr + " IS NULL OR " + a.Expr + " = '')",
			append(append([]any{}, a.ExprArgs...), a.ExprArgs...)
	}
	return a.Expr + " = ?", append(append([]any{}, a.ExprArgs...), key)
}

// groupLabels fills in what a person reads where the stored value is not the
// word, from [groupAxis.Labels]. It fills only empty labels, so the unset
// column keeps the [groupAxis.Unset] heading [groupCounts] gave it.
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

// labelGroups heads every column of one answer: [groupLabels], and the
// parent axis's own lookup where the axis is that one.
func labelGroups(ctx context.Context, tx *sql.Tx, groups []Group, axis groupAxis) error {
	groupLabels(groups, axis)
	if axis.ParentLabels {
		return labelParents(ctx, tx, groups)
	}
	return nil
}

// labelParents heads each column of the parent axis with its parent's key and
// title (`ENG-12 · Rotate the build keys`) rather than its uuid. Each is one
// [readTaskRef] seek, at most [MaxGroups] per page, made whether or not the
// answer holds the parent, since a filter admitting a subtask need not admit
// its parent. A parent this node has no row for keeps the id.
func labelParents(ctx context.Context, tx *sql.Tx, groups []Group) error {
	for i := range groups {
		if groups[i].Key == "" || groups[i].Label != "" {
			continue
		}
		ref, err := readTaskRef(ctx, tx, groups[i].Key)
		if err != nil {
			return err
		}
		if ref != nil {
			groups[i].Label = ref.Key + " · " + ref.Title
		}
	}
	return nil
}

// labelsOf renders a static table as an axis lookup, and nil for an empty
// one — which is what [groupLabels] reads as "this axis stores the word".
func labelsOf(table map[string]string) func(string) string {
	if len(table) == 0 {
		return nil
	}
	return func(key string) string { return table[key] }
}

// unitAxis is a unit column, headed with each team's name. Rows hold the
// unit's KEY ([ChartUnit.Key]), immutable for a team's life (internal/org), so
// the column groups on the team; a stored unit the chart no longer has keeps
// its own column under that key. A caller may name a unit by key or name, in
// any case, so its value is resolved to the key ([groupAxis.Canonical]).
func unitAxis(column string, units Units) groupAxis {
	axis := groupAxis{Expr: column, Unset: "(no unit)", Labels: unitLabels(units)}
	if units == nil {
		return axis
	}
	// `group=Engineering` and `group=eng` narrow to the same column.
	axis.Canonical = func(key string) string {
		if unit, found := units.ResolveUnit(key); found {
			return unit.Key
		}
		return key
	}
	return axis
}

// unitLabels is what a person reads on a unit column, whose stored KEY is an
// id nobody reads. The chart is asked per key because a column may name a
// team the chart no longer has. An unresolved key answers "", never a stand-in
// like "(unknown)": a client then falls back to the key, which is still in the
// rows and is what a filter on that column takes.
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
// built from. Its own sentinel because a caller repairs it by NARROWING, not
// by correcting a malformed key: a surface maps it to "add a filter".
var ErrTooBroad = errors.New("worklet: this query selects more rows than the answer can be built from")

// checkGroupBreadth refuses a grouping whose input is wider than the working
// set that draws it.
//
// It is a bounded COUNT, not a test for a filter key, which a filter like
// `status_group=not_started,active` passes while narrowing nothing.
// [readGroups] sorts this predicate's rows once per column, and past
// [GroupByRowCeiling] that working set spills the page cache. Project scope is
// exempt by PLAN: `t.project_key = ?` drives `tracker_tasks_board_idx`, a
// range one project wide, so the test is an absent project, not the literal
// `container=workspace` (an omitted container adds no predicate either, see
// [compile]). The count is capped at the ceiling + 1 to stay cheap, so the
// refusal names the ceiling crossed rather than a total.
func checkGroupBreadth(ctx context.Context, tx *sql.Tx, q Query,
	set taskSet) error {

	if q.Scope.Project != "" {
		return nil
	}
	query := cappedCountStatement(set.from, set.where)
	var n int
	bound := append(append([]any{}, set.args...), GroupByRowCeiling+1)
	if err := tx.QueryRowContext(ctx, query, bound...).Scan(&n); err != nil {
		return fmt.Errorf("worklet: count what group_by=%s would sort: %w",
			q.GroupBy, err)
	}
	if n <= GroupByRowCeiling {
		return nil
	}
	return fmt.Errorf("worklet: group_by=%s over the whole company matches more "+
		"than %d tasks, and a board is drawn by sorting every one of them — "+
		"scope it with container=project:<key>, or add a filter that actually "+
		"excludes rows (assignee=, updated=, a date bound) rather "+
		"than one every open task already satisfies: %w",
		q.GroupBy, GroupByRowCeiling, ErrTooBroad)
}

// readGroups assembles the columns one query asked for: a GROUP BY for the
// counts and each column's ordinary paged statement for its rows, rather than
// one window function ranking every row in the corpus on every board poll.
func readGroups(ctx context.Context, tx *sql.Tx, q Query,
	fields map[string]resolvedField, set taskSet,
	terms []sortTerm) (grouped, error) {

	// THE GATE BEFORE ANY WORK, even compiling the axis: it exists to
	// refuse that cost.
	if err := checkGroupBreadth(ctx, tx, q, set); err != nil {
		return grouped{}, err
	}
	axis, err := compileGroup(q.GroupBy, fields, q.dayWindow(), q.Units)
	if err != nil {
		return grouped{}, err
	}
	// THE `group=` FILTER IS ALREADY IN THE SET: [compile] adds its
	// join-free form, so the count hint and totals are narrowed too.
	rowsPer := q.GroupLimit
	if rowsPer <= 0 {
		rowsPer = GroupRowsDefault
	}
	if rowsPer > GroupRowsMax {
		rowsPer = GroupRowsMax
	}

	// Lanes lower the column cap ([MaxGroupsWithSubgroups]).
	columns := MaxGroups
	if q.GroupBy2 != "" {
		columns = MaxGroupsWithSubgroups
	}
	groups, dropped, err := groupCounts(ctx, tx, axis, set, columns)
	if err != nil {
		return grouped{}, err
	}
	// COUNTS, THEN FILL, THEN LABELS: the fill mints columns from bare
	// keys (a closed axis's unset column too, which [groupCounts] never
	// saw), so labelling first would head a zero-count band by its slug.
	// That unset heading comes from [groupAxis.Labels]' `""` key, from the
	// same list as [groupAxis.Unset] ([dueBucketAxis]).
	groups = fillColumns(groups, admittedColumns(q, axis))
	if err := labelGroups(ctx, tx, groups, axis); err != nil {
		return grouped{}, err
	}

	for i := range groups {
		if groups[i].Count == 0 {
			// A FILLED COLUMN HOLDS NOTHING by construction
			// ([admittedColumns]), so its rows and lanes are not read.
			continue
		}
		rows, err := groupRows(ctx, tx, axis, groups[i].Key, set,
			terms, rowsPer, q.DayStart)
		if err != nil {
			return grouped{}, err
		}
		groups[i].Rows = rows
		if q.GroupBy2 == "" {
			continue
		}
		// THE SECOND AXIS IS THE FIRST AGAIN, inside this column; one
		// level only, since a board draws swimlanes, not a tree.
		inner, innerDropped, err := readSubgroups(ctx, tx, q, fields, axis,
			groups[i].Key, set, terms, rowsPer)
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
	set taskSet, terms []sortTerm, rowsPer int) ([]Group, int, error) {

	inner, err := compileGroup(q.GroupBy2, fields, q.dayWindow(), q.Units)
	if err != nil {
		return nil, 0, err
	}
	// THE OUTER COLUMN NARROWS THE INNER QUERY by its join-free predicate
	// ([columnSet]), so lanes are counted over the inner axis alone, in its
	// declared order.
	scoped := columnSet(outer, key, set)
	// A NAMED SUBGROUP NARROWS THE INNER AXIS as `group` does the outer.
	// On the counting statement it is the JOINED form, since that groups by
	// the joined value: the probe would make a label lane count every
	// other label of its tasks.
	counted := scoped
	if q.Subgroup != nil {
		innerClause, innerArgs := inner.joinedFilter(*q.Subgroup)
		counted = scoped.and(innerClause, innerArgs)
	}
	counts, dropped, err := groupCounts(ctx, tx, inner, counted, MaxSubgroups)
	if err != nil {
		return nil, 0, err
	}
	if err := labelGroups(ctx, tx, counts, inner); err != nil {
		return nil, 0, err
	}
	for i := range counts {
		rows, err := groupRows(ctx, tx, inner, counts[i].Key, scoped,
			terms, rowsPer, q.DayStart)
		if err != nil {
			return nil, 0, err
		}
		counts[i].Rows = rows
	}
	return counts, dropped, nil
}

// declaredOrder renders a closed set's own sequence as strings, from the set
// itself: `Statuses`, `StatusGroups` and `Priorities` are declared in the
// order they mean, and a second copy here would drift.
func declaredOrder[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

// admittedColumns is the closed axis's values THIS query could hold, in
// declared order: the columns a board draws even when empty, since a board
// with one lane reads as one that did not load, and an empty column is honest
// only where a row could have landed. Nil for an open axis (assignee, tag,
// type, a field), where every value would be a roster, not a board;
// [TestEveryClosedAxisAdmitsItsWholeSetWhenNothingNarrowsIt] catches a
// [groupAxis.Order] without an arm here. [compilePredicate]'s top-level
// filters narrow it, and a disjunction's arms only narrow further. `group=` is
// read by PRESENCE ([Query.Group]) and narrows it to that column, so a
// present-empty one admits nothing where no value is empty. The second axis is
// never filled: an empty lane in every column multiplies the cells
// [MaxGroupsWithSubgroups] bounds.
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

// admittedDueBands is the due axis's arm of [admittedColumns], narrowed in two
// halves. STATUS: [dueOverdue] and [dueEarlier] share the interval before the
// day start and differ by status, so each is admitted only where
// [admittedStatuses] leaves a status of its kind. CALENDAR: only `due=`
// narrows the interval (other date keys bound other columns), admitting a
// band whose [bandRange] meets the filter's, on [dueBucketCase]'s boundaries.
// The overdue alias is both halves and admits [dueOverdue] alone. [dueNone]
// is never admitted under `due=`: every comparison is `due_at IS NOT NULL AND
// …` and `due=none` is refused, so no `due=` value holds an undated task.
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
		// THE ALIAS CARRIES THE OPEN CONDITION, so it admits only
		// [dueOverdue], not [dueEarlier], which shares its interval.
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
// beginning of time and a zero Hi for ever, never a real boundary since
// [dueBucketAxis] refuses a zero one.
type dueRange struct{ Lo, Hi time.Time }

// overlaps reports whether two ranges share an instant. A DEGENERATE interval
// is the instant it sits at: on a Sunday `this_week`, [DayEnd, WeekEnd), is
// empty, and read as empty it would drop from every filtered board one day in
// seven while an unfiltered board still drew it.
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

// dueFilterRange is one `due=` filter read as the same kind of interval. An
// inclusive bound is bumped by a microsecond, the stored unit
// ([store.EncodeTime]), so `lte:X` and `< X+1µs` admit the same rows and the
// comparison needs no inclusivity flag.
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
	// An unknown comparison bounds nothing: a filter nobody could read
	// must not remove a column somebody's rows are in.
	return dueRange{}
}

// admittedStatuses is every status the query's own predicate could match,
// under [compilePredicate]'s finished-work rule: excluded unless `show_closed`
// asks, with the overdue alias ANDing the open condition back on.
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

// fillColumns lays the present columns over the admitted set in admitted
// order, minting an empty column for each admitted value nothing was counted
// under. An empty column carries an empty list, never nil: `rows` is not
// `omitempty`, and `null` would break a renderer on exactly that column. A
// present value the set does not name (a status a newer peer wrote) keeps its
// place at the end: the predicate admitted it and it has rows.
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
