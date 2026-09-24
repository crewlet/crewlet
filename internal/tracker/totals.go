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

// Aggregates over the answer's WHOLE set, not over its page.
//
// A total is what the question adds up to — "how much work is on this board",
// "what has this epic cost" — and a page is fifty rows of it. Computing one
// over the page would make it change as somebody scrolled, which is the one
// thing a number on a header must not do.
//
// # ONE STATEMENT, N AGGREGATES
//
// Every total shares the answer's own predicate, so they are columns of one
// pass rather than one query each. [MaxTotals] bounds the list at eight
// because each entry is another expression over the same rows — cheap — and
// eight is already more than a header renders.
//
// # Why a total names its COLUMN rather than a metric
//
// `spend_tokens:sum` is the DDL's own spelling, written into the index comment
// beside the index that serves it. A metric vocabulary on top ("cost",
// "effort") would be a second name for one column and the two would drift: the
// index comment names the column, so the grammar does too.
//
// # Why a total reads EVERY value and a sort reads one
//
// A multi-valued field is several rows per task, and the two operations want
// opposite things from that. A SUM of it is the sum of the values — all of
// them, or it is not a sum — while a SORT has to pick exactly one or the join
// multiplies every task by its own value count and the page repeats rows. So
// the aggregate here is unpinned and [sortTerms] pins `seq = 0`, and the
// difference is the operations' rather than an oversight. `count` is the third
// answer again: it counts TASKS, distinct, because "how many have this set" is
// what a header means by it.
//
// # And why a custom field's total reads a different table
//
// A declared field's values live in `tracker_field_values`, keyed by field id
// and typed per column, so `f.<slug>:sum` is a correlated aggregate over that
// table rather than a column of the task. Its two indexes say so — the numeric
// one names "their totals" and the date one "their earliest/latest totals".

// The aggregate operations.
//
// A CLOSED SET, because an op reaches the statement as a NAME. `count` is
// there and is not the same as the answer's own [Answer.TotalHint]: the hint
// stops at a ceiling because an exact count over an unbounded set turns a poll
// into a scan, and a count ASKED FOR is a caller who wants the number and has
// said so.
//
// `median` and `p90` are ORDER STATISTICS, answered by nearest rank: the value
// at 1-based position ⌈p·n⌉ of the set's non-null values in ascending order —
// so the median of four values is the second, never an average of two. A
// value the set does not hold would be a number no task has, and a header
// saying "median 3.5 points" over a board of 3s and 4s describes nobody. The
// same rule answers both, which is why they are two names for one expression.
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

// totalColumns is what a total may be taken over, beside a custom field.
//
// THE SUMMABLE COLUMNS AND NOTHING ELSE: a sum over a status is a number with
// no meaning, and a grammar that accepted it would answer one rather than
// refuse.
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

	// Value is the number, and ABSENT when no row contributed one — which
	// is not zero: "nothing is estimated" and "everything is estimated at
	// nothing" are different facts, and a header rendering the second for
	// the first is how an empty board reads as free.
	Value *float64 `json:"value,omitempty"`

	// At is the instant, for an order statistic — min, max, median, p90 —
	// over a date column.
	At *time.Time `json:"at,omitempty"`

	// instant is decided at COMPILE time, from the column's declared
	// type, and carried here rather than re-derived at scan time: a
	// custom field's min is an instant or a number depending on its
	// DECLARATION, which the scan cannot see and the compiler already
	// resolved.
	instant bool

	// rank is an order statistic's p over [rankDenominator], zero for
	// every other op, and ranked the FROM … WHERE clause over the values it
	// is taken from, with rankArgs its arguments. The statement's own
	// column for such a total is the COUNT of those values, and the value
	// itself is read by a second statement once the count says which
	// position to read — see [readRanked].
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
			return nil, nil, nil, fmt.Errorf("tracker: a total is "+
				"<column>:<op> and %q carries no operation", entry)
		}
		column, op = strings.TrimSpace(column), strings.ToLower(strings.TrimSpace(op))
		if !validTotalOp(op) {
			return nil, nil, nil, fmt.Errorf("tracker: %q is not an aggregate "+
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
			// ONLY AN ORDER STATISTIC ANSWERS AN INSTANT: it is one of
			// the set's own values. A sum of dates is a number of
			// microseconds since 1970, which is a value nothing renders
			// and nobody meant.
			if !orderStatistic(op) {
				return nil, nil, nil, fmt.Errorf("tracker: %s is a date, and "+
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
			return "", nil, false, fmt.Errorf("tracker: totals name f.%s and "+
				"no field resolved to it", ref)
		}
		value := FieldValueColumn(field.Type)
		if value != "num" && value != "at" {
			return "", nil, false, fmt.Errorf("tracker: f.%s is a %s, and an "+
				"aggregate over it would be a number with no meaning — a total "+
				"is taken over a number or a date", field.Slug, field.Type)
		}
		if op == TotalCount {
			// A COUNT OF A FIELD IS "HOW MANY TASKS SET IT", which is
			// the honest reading and the one a header wants — never
			// how many VALUE ROWS a multi-valued field produced.
			return `(SELECT COUNT(DISTINCT v.task_id) FROM tracker_field_values v
			         WHERE v.field_id = ? AND ` + liveFieldValue + `
			           AND v.task_id IN (SELECT id FROM matched))`,
				[]any{field.ID}, false, nil
		}
		if _, ranked := rankOf[op]; ranked {
			// THE COUNT OF THE VALUES, which is this statement's column
			// for an order statistic — see [readRanked].
			return `(SELECT COUNT(v.` + value + `)
			         FROM tracker_field_values v
			         WHERE v.field_id = ? AND ` + liveFieldValue + `
			           AND v.task_id IN (SELECT id FROM matched))`,
				[]any{field.ID}, value == "at", nil
		}
		return `(SELECT ` + strings.ToUpper(op) + `(v.` + value + `)
		         FROM tracker_field_values v
		         WHERE v.field_id = ? AND ` + liveFieldValue + `
		           AND v.task_id IN (SELECT id FROM matched))`,
			[]any{field.ID}, value == "at", nil
	}
	if op == TotalCount && column == "tasks" {
		// THE ONE COUNT THAT NAMES NO COLUMN, because "how many" is the
		// commonest total there is and `tasks:count` is what a caller
		// writes for it.
		return "COUNT(*)", nil, false, nil
	}
	expr, known := totalColumns[column]
	if !known {
		return "", nil, false, fmt.Errorf("tracker: %q is not a column a total "+
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

// rankedValues is the value expression and the FROM … WHERE clause an order
// statistic reads its values from, with the clause's arguments. Called only
// for a column [totalExpr] has already accepted.
func rankedValues(column string, fields map[string]resolvedField) (string, string, []any) {
	if ref, ok := strings.CutPrefix(column, FieldKeyPrefix); ok && ref != "" {
		field := fields[ref]
		value := "v." + FieldValueColumn(field.Type)
		return value, `FROM tracker_field_values v
		         WHERE v.field_id = ? AND ` + liveFieldValue + `
		           AND v.task_id IN (SELECT id FROM matched)
		           AND ` + value + ` IS NOT NULL`, []any{field.ID}
	}
	expr := totalColumns[column]
	return expr, `FROM matched t WHERE ` + expr + ` IS NOT NULL`, nil
}

// readTotals runs the one statement every total is a column of.
//
// A COMMON TABLE EXPRESSION HOLDS THE MATCHED SET, so a custom field's
// correlated aggregate and a task column's plain one read the same rows — and
// the predicate is evaluated once rather than once per total.
func readTotals(ctx context.Context, tx *sql.Tx, where string, whereArgs []any,
	totals []Total, exprs []string, exprArgs []any) ([]Total, error) {

	if len(totals) == 0 {
		return nil, nil
	}
	query := matchedSet(where) + `
	          SELECT ` + strings.Join(exprs, ", ") + ` FROM matched t`
	// THE EXPRESSION ARGUMENTS COME AFTER THE PREDICATE'S, because the
	// common table expression is written first and its placeholders are
	// bound in statement order.
	args := append(append([]any{}, whereArgs...), exprArgs...)

	cells := make([]any, len(totals))
	targets := make([]any, len(totals))
	for i := range cells {
		targets[i] = &cells[i]
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(targets...); err != nil {
		return nil, fmt.Errorf("tracker: read the answer's totals: %w", err)
	}
	out := make([]Total, 0, len(totals))
	for i, total := range totals {
		number, held := asFloat(cells[i])
		if held && total.rank > 0 {
			// THE CELL IS A COUNT, and the value is the one at its
			// nearest-rank position.
			var err error
			if number, held, err = readRanked(ctx, tx, where, whereArgs, total, int64(number)); err != nil {
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
// A SECOND STATEMENT over the same predicate, in the same read transaction,
// so it sees exactly the set the count was taken over. It is not folded into
// the first as a subquery because the engine does not resolve the common
// table expression inside a LIMIT or OFFSET expression ("no such table:
// matched", measured), and a window function would number every row of the
// set to read one. An EMPTY set has no position to read and answers absent,
// like every other aggregate over nothing.
func readRanked(ctx context.Context, tx *sql.Tx, where string, whereArgs []any,
	total Total, n int64) (float64, bool, error) {

	if n <= 0 {
		return 0, false, nil
	}
	offset := (n*int64(total.rank)+rankDenominator-1)/rankDenominator - 1
	query := matchedSet(where) + `
	          SELECT ` + total.value + ` ` + total.ranked + `
	          ORDER BY ` + total.value + ` LIMIT 1 OFFSET ?`
	args := append(append(append([]any{}, whereArgs...), total.rankArgs...), offset)
	var cell any
	switch err := tx.QueryRowContext(ctx, query, args...).Scan(&cell); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("tracker: read the %s of %s: %w",
			total.Op, total.Column, err)
	}
	number, held := asFloat(cell)
	return number, held, nil
}

// matchedSet is the common table expression every total is read over: the
// answer's own predicate, and every column a total may name.
func matchedSet(where string) string {
	return `WITH matched AS (SELECT t.id, t.spend_tokens, t.spend_turns,
	                                  t.spend_rounds, t.spend_input,
	                                  t.spend_output, t.spend_cache_read,
	                                  t.spend_cache_write, t.spend_wall_ms,
	                                  t.spend_workers, t.spend_sent_back,
	                                  t.estimate_min, t.points,
	                                  t.reassignments, t.reopens, t.depth, t.due_at,
	                                  t.start_at, t.created_at, t.updated_at,
	                                  t.finished_at
	                             FROM tracker_tasks t WHERE ` + where + `)`
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
