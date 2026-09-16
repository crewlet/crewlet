package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Aggregates over the answer's WHOLE set, not over its page.
//
// A total is what the question adds up to — "how much work is in this sprint",
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
const (
	TotalSum   = "sum"
	TotalAvg   = "avg"
	TotalMin   = "min"
	TotalMax   = "max"
	TotalCount = "count"
)

// TotalOps are the five.
var TotalOps = []string{TotalSum, TotalAvg, TotalMin, TotalMax, TotalCount}

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
	"estimate_min":      "t.estimate_min",
	"points":            "t.points",
	"reassignments":     "t.reassignments",
	"depth":             "t.depth",
	"due_at":            "t.due_at",
	"start_at":          "t.start_at",
	"created_at":        "t.created_at",
	"updated_at":        "t.updated_at",
	"finished_at":       "t.finished_at",
}

// instantColumns are the totals whose value is an INSTANT rather than a
// number, so `min` and `max` on them answer a time.
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
	// the first is how a sprint reads as free.
	Value *float64 `json:"value,omitempty"`

	// At is the instant, for a min or a max over a date column.
	At *time.Time `json:"at,omitempty"`

	// instant is decided at COMPILE time, from the column's declared
	// type, and carried here rather than re-derived at scan time: a
	// custom field's min is an instant or a number depending on its
	// DECLARATION, which the scan cannot see and the compiler already
	// resolved.
	instant bool
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
				"— the five are %s", op, strings.Join(TotalOps, ", "))
		}
		total := Total{Key: entry, Column: column, Op: op}
		expr, values, instant, err := totalExpr(column, op, fields)
		if err != nil {
			return nil, nil, nil, err
		}
		total.instant = instant
		if instant {
			// ONLY min AND max ANSWER AN INSTANT. A sum of dates is a
			// number of microseconds since 1970, which is a value
			// nothing renders and nobody meant.
			if op != TotalMin && op != TotalMax {
				return nil, nil, nil, fmt.Errorf("tracker: %s is a date, and "+
					"%s over dates is a number of microseconds rather than a "+
					"time — the two that answer an instant are %s and %s",
					column, op, TotalMin, TotalMax)
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
	return strings.ToUpper(op) + "(" + expr + ")", nil, instantColumns[column], nil
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
	query := `WITH matched AS (SELECT t.id, t.spend_tokens, t.spend_turns,
	                                  t.spend_rounds, t.spend_input,
	                                  t.spend_output, t.spend_cache_read,
	                                  t.spend_cache_write, t.spend_wall_ms,
	                                  t.estimate_min, t.points,
	                                  t.reassignments, t.depth, t.due_at,
	                                  t.start_at, t.created_at, t.updated_at,
	                                  t.finished_at
	                             FROM tracker_tasks t WHERE ` + where + `)
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
		if !held {
			// ABSENT RATHER THAN ZERO — see [Total.Value].
			out = append(out, total)
			continue
		}
		if total.instant {
			at := store.DecodeTime(int64(number))
			total.At = &at
			out = append(out, total)
			continue
		}
		total.Value = &number
		out = append(out, total)
	}
	return out, nil
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
