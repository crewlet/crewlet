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

// Totals are over the answer's WHOLE set, never its page: one over the page
// would change as somebody scrolled, which a header's number must not. All
// share the answer's predicate, so they are columns of one pass ([MaxTotals]
// is eight: each is a cheap expression over the same rows, and eight is more
// than a header renders). A total names its COLUMN, the index comment's own
// spelling (`spend_tokens:sum`), since a metric name would drift from it.
//
// A total reads EVERY value of a multi-valued field, where [sortTerms] pins
// `seq = 0` because a sort over them all repeats rows; `count` counts distinct
// TASKS. A custom field's total reads `tracker_field_values` by its key over
// the answer's own tasks ([matchedValues]), never by the field's value index,
// which reads every value the field holds in the company.

// The aggregate operations: a CLOSED SET, because an op reaches the statement
// as a NAME. An asked-for `count` is exact, unlike [Answer.TotalHint], which
// stops at a ceiling so that a poll is not a scan.
//
// `median` and `p90` are ORDER STATISTICS by nearest rank: the value at 1-based
// position ⌈p·n⌉ of the set's non-null values ascending, so the median of four
// is the second, never an average: a value no task has describes nobody.
const (
	TotalSum    = "sum"
	TotalAvg    = "avg"
	TotalMin    = "min"
	TotalMax    = "max"
	TotalCount  = "count"
	TotalMedian = "median"
	TotalP90    = "p90"
)

// TotalOps are the seven.
var TotalOps = []string{TotalSum, TotalAvg, TotalMin, TotalMax, TotalCount,
	TotalMedian, TotalP90}

// rankOf is each order statistic's p, as the numerator over [rankDenominator]
// — integer arithmetic, so ⌈p·n⌉ is exact in SQL rather than a float rounded
// on one engine and truncated on another.
var rankOf = map[string]int{TotalMedian: 50, TotalP90: 90}

const rankDenominator = 100

// orderStatistic reports whether an op answers with one of the set's own
// values — the four that may be taken over a date.
func orderStatistic(op string) bool {
	switch op {
	case TotalMin, TotalMax, TotalMedian, TotalP90:
		return true
	}
	return false
}

// totalColumns is what a total may be taken over beside a custom field: the
// summable columns only, since a sum over a status is a number with no meaning.
var totalColumns = map[string]string{
	"spend_tokens":      "t.spend_tokens",
	"spend_turns":       "t.spend_turns",
	"spend_rounds":      "t.spend_rounds",
	"spend_input":       "t.spend_input",
	"spend_output":      "t.spend_output",
	"spend_cache_read":  "t.spend_cache_read",
	"spend_cache_write": "t.spend_cache_write",
	"spend_wall_ms":     "t.spend_wall_ms",
	"spend_workers":     "t.spend_workers",
	"spend_sent_back":   "t.spend_sent_back",
	"estimate_min":      "t.estimate_min",
	"points":            "t.points",
	"reassignments":     "t.reassignments",
	"reopens":           "t.reopens",
	"depth":             "t.depth",
	"due_at":            "t.due_at",
	"start_at":          "t.start_at",
	"created_at":        "t.created_at",
	"updated_at":        "t.updated_at",
	"finished_at":       "t.finished_at",
}

// instantColumns are the totals whose value is an INSTANT rather than a
// number, so an order statistic on them — `min`, `max`, `median`, `p90` —
// answers a time.
var instantColumns = map[string]bool{
	"due_at": true, "start_at": true, "created_at": true,
	"updated_at": true, "finished_at": true,
}

// Total is one aggregate over the answer's whole set.
type Total struct {
	// Key is the entry as the caller wrote it, so a renderer can find the
	// total it asked for without re-deriving the spelling.
	Key    string `json:"key"`
	Column string `json:"column"`
	Op     string `json:"op"`

	// Value is the number, ABSENT when no row contributed one: "nothing is
	// estimated" is not "everything is estimated at nothing", and rendering
	// the second for the first makes an empty board read as free.
	Value *float64 `json:"value,omitempty"`

	// At is the instant, for an order statistic — min, max, median, p90 —
	// over a date column.
	At *time.Time `json:"at,omitempty"`

	// instant is decided at compile time and carried: a custom field's min
	// is an instant or a number by its DECLARATION, which the scan cannot
	// see.
	instant bool

	// rank is an order statistic's p over [rankDenominator] (zero for any
	// other op), ranked the FROM … WHERE clause over its values and
	// rankArgs its arguments. The statement's column for such a total is
	// the COUNT of those values; [readRanked] reads the value itself.
	rank     int
	ranked   string
	value    string
	rankArgs []any
}

// compileTotals turns the asked-for entries into aggregate expressions.
func compileTotals(entries []string, fields map[string]resolvedField) (
	[]Total, []string, []any, error) {

	totals := make([]Total, 0, len(entries))
	exprs := make([]string, 0, len(entries))
	var args []any
	for _, entry := range entries {
		column, op, found := strings.Cut(entry, ":")
		if !found {
			return nil, nil, nil, fmt.Errorf("worklet: a total is "+
				"<column>:<op> and %q carries no operation", entry)
		}
		column, op = strings.TrimSpace(column), strings.ToLower(strings.TrimSpace(op))
		if !validTotalOp(op) {
			return nil, nil, nil, fmt.Errorf("worklet: %q is not an aggregate "+
				"— the seven are %s", op, strings.Join(TotalOps, ", "))
		}
		total := Total{Key: entry, Column: column, Op: op}
		expr, values, instant, err := totalExpr(column, op, fields)
		if err != nil {
			return nil, nil, nil, err
		}
		total.instant = instant
		if rank, ranked := rankOf[op]; ranked {
			total.rank = rank
			total.value, total.ranked, total.rankArgs = rankedValues(column, fields)
		}
		if instant {
			// ONLY AN ORDER STATISTIC ANSWERS AN INSTANT, being one of the
			// set's own values; a sum of dates is microseconds since 1970.
			if !orderStatistic(op) {
				return nil, nil, nil, fmt.Errorf("worklet: %s is a date, and "+
					"%s over dates is a number of microseconds rather than a "+
					"time — the four that answer an instant are %s, %s, %s and %s",
					column, op, TotalMin, TotalMax, TotalMedian, TotalP90)
			}
		}
		totals = append(totals, total)
		exprs = append(exprs, expr)
		args = append(args, values...)
	}
	return totals, exprs, args, nil
}

func validTotalOp(op string) bool {
	for _, known := range TotalOps {
		if op == known {
			return true
		}
	}
	return false
}

// totalExpr is one aggregate, over a task column or over a field's values.
func totalExpr(column, op string, fields map[string]resolvedField) (
	string, []any, bool, error) {

	if ref, ok := strings.CutPrefix(column, FieldKeyPrefix); ok && ref != "" {
		field, held := fields[ref]
		if !held {
			return "", nil, false, fmt.Errorf("worklet: totals name f.%s and "+
				"no field resolved to it", ref)
		}
		value := FieldValueColumn(field.Type)
		if value != "num" && value != "at" {
			return "", nil, false, fmt.Errorf("worklet: f.%s is a %s, and an "+
				"aggregate over it would be a number with no meaning — a total "+
				"is taken over a number or a date", field.Slug, field.Type)
		}
		if op == TotalCount {
			// A COUNT OF A FIELD IS HOW MANY TASKS SET IT, never how many
			// VALUE ROWS a multi-valued field produced.
			return `(SELECT COUNT(DISTINCT v.task_id) ` + matchedValues + `
			         WHERE v.field_id = ? AND ` + liveFieldValue + `)`,
				[]any{field.ID}, false, nil
		}
		if _, ranked := rankOf[op]; ranked {
			// THE COUNT OF THE VALUES, which is this statement's column
			// for an order statistic — see [readRanked].
			return `(SELECT COUNT(v.` + value + `) ` + matchedValues + `
			         WHERE v.field_id = ? AND ` + liveFieldValue + `)`,
				[]any{field.ID}, value == "at", nil
		}
		return `(SELECT ` + strings.ToUpper(op) + `(v.` + value + `) ` +
				matchedValues + `
		         WHERE v.field_id = ? AND ` + liveFieldValue + `)`,
			[]any{field.ID}, value == "at", nil
	}
	if op == TotalCount && column == "tasks" {
		// THE ONE COUNT THAT NAMES NO COLUMN: `tasks:count`, the
		// commonest total.
		return "COUNT(*)", nil, false, nil
	}
	expr, known := totalColumns[column]
	if !known {
		return "", nil, false, fmt.Errorf("worklet: %q is not a column a total "+
			"may be taken over — a sum of a status is a number with no "+
			"meaning; the columns are %s, or f.<slug>, or tasks:count",
			column, strings.Join(sortedKeysOf(totalColumns), ", "))
	}
	if op == TotalCount {
		return "COUNT(" + expr + ")", nil, false, nil
	}
	if _, ranked := rankOf[op]; ranked {
		// THE COUNT OF THE VALUES, which is this statement's column for
		// an order statistic — see [readRanked].
		return "COUNT(" + expr + ")", nil, instantColumns[column], nil
	}
	return strings.ToUpper(op) + "(" + expr + ")", nil, instantColumns[column], nil
}

// matchedValues is where a custom field's total reads its values: each task of
// the answer's set, then that task's own rows by the table's key
// ([fieldValuesKey]), so a total costs the set it totals rather than every
// value the field holds in the company.
//
// A CROSS JOIN, which this engine, like SQLite, reads in the order written. A
// plain JOIN leaves the order to estimates no statistics anchor and any clause
// of the predicate moves, and the plan that reads the value table's key index
// first scans the matched set again per value: every value of every field for
// one header.
const matchedValues = `FROM matched m
		         CROSS JOIN tracker_field_values v INDEXED BY ` + fieldValuesKey + `
		           ON v.task_id = m.id`

// rankedValues is the value expression and the FROM … WHERE clause an order
// statistic reads its values from, with the clause's arguments. Called only
// for a column [totalExpr] has already accepted.
func rankedValues(column string, fields map[string]resolvedField) (string, string, []any) {
	if ref, ok := strings.CutPrefix(column, FieldKeyPrefix); ok && ref != "" {
		field := fields[ref]
		value := "v." + FieldValueColumn(field.Type)
		return value, matchedValues + `
		         WHERE v.field_id = ? AND ` + liveFieldValue + `
		           AND ` + value + ` IS NOT NULL`, []any{field.ID}
	}
	expr := totalColumns[column]
	return expr, `FROM matched t WHERE ` + expr + ` IS NOT NULL`, nil
}

// readTotals runs the one statement every total is a column of. A common table
// expression holds the matched set, so field and column aggregates read the
// same rows and the predicate is evaluated once rather than once per total.
func readTotals(ctx context.Context, tx *sql.Tx, set taskSet,
	totals []Total, exprs []string, exprArgs []any) ([]Total, error) {

	if len(totals) == 0 {
		return nil, nil
	}
	query := totalsStatement(set, exprs)
	// THE EXPRESSION ARGUMENTS FOLLOW THE PREDICATE'S: the common table
	// expression is written first, and placeholders bind in statement order.
	args := append(append([]any{}, set.args...), exprArgs...)

	cells := make([]any, len(totals))
	targets := make([]any, len(totals))
	for i := range cells {
		targets[i] = &cells[i]
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(targets...); err != nil {
		return nil, fmt.Errorf("worklet: read the answer's totals: %w", err)
	}
	out := make([]Total, 0, len(totals))
	for i, total := range totals {
		number, held := asFloat(cells[i])
		if held && total.rank > 0 {
			// THE CELL IS A COUNT, and the value is the one at its
			// nearest-rank position.
			var err error
			if number, held, err = readRanked(ctx, tx, set, total, int64(number)); err != nil {
				return nil, err
			}
		}
		out = append(out, total.answered(number, held))
	}
	return out, nil
}

// answered is the total with its value in the field its type reads.
func (t Total) answered(number float64, held bool) Total {
	if !held {
		// ABSENT RATHER THAN ZERO — see [Total.Value].
		return t
	}
	if t.instant {
		at := store.DecodeTime(int64(number))
		t.At = &at
		return t
	}
	t.Value = &number
	return t
}

// readRanked reads an order statistic's value: the one at 1-based position
// ⌈rank·n/100⌉ of the set's n values in ascending order.
//
// A SECOND STATEMENT in the same read transaction, so it sees the set the count
// was taken over. Not a subquery of the first: the engine does not resolve the
// common table expression inside LIMIT or OFFSET ("no such table: matched"),
// and a window function would number every row to read one. An EMPTY set
// answers absent, like every aggregate over nothing.
func readRanked(ctx context.Context, tx *sql.Tx, set taskSet,
	total Total, n int64) (float64, bool, error) {

	if n <= 0 {
		return 0, false, nil
	}
	offset := (n*int64(total.rank)+rankDenominator-1)/rankDenominator - 1
	query, args := rankedStatement(set, total, offset)
	var cell any
	switch err := tx.QueryRowContext(ctx, query, args...).Scan(&cell); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("worklet: read the %s of %s: %w",
			total.Op, total.Column, err)
	}
	number, held := asFloat(cell)
	return number, held, nil
}

// rankedStatement is [readRanked]'s read of the value at one position of the
// set's values in order, named for the plan test.
func rankedStatement(set taskSet, total Total, offset int64) (string, []any) {
	return matchedSet(set) + `
	          SELECT ` + total.value + ` ` + total.ranked + `
	          ORDER BY ` + total.value + ` LIMIT 1 OFFSET ?`,
		append(append(append([]any{}, set.args...), total.rankArgs...), offset)
}

// totalsStatement is the one statement [readTotals] reads every total with,
// one column per expression — named for the plan test.
func totalsStatement(set taskSet, exprs []string) string {
	return matchedSet(set) + `
	          SELECT ` + strings.Join(exprs, ", ") + ` FROM matched t`
}

// matchedSet is the common table expression every total is read over: the
// answer's own set, and every column a total may name.
func matchedSet(set taskSet) string {
	return `WITH matched AS (SELECT t.id, t.spend_tokens, t.spend_turns,
	                                  t.spend_rounds, t.spend_input,
	                                  t.spend_output, t.spend_cache_read,
	                                  t.spend_cache_write, t.spend_wall_ms,
	                                  t.spend_workers, t.spend_sent_back,
	                                  t.estimate_min, t.points,
	                                  t.reassignments, t.reopens, t.depth, t.due_at,
	                                  t.start_at, t.created_at, t.updated_at,
	                                  t.finished_at
	                             FROM ` + set.from + ` WHERE ` + set.where + `)`
}

// asFloat reads whatever the driver handed back for an aggregate.
func asFloat(cell any) (float64, bool) {
	switch v := cell.(type) {
	case nil:
		return 0, false
	case float64:
		return v, true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
