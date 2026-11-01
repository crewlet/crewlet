package worklet

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// Relative dates, and why they are resolved HERE rather than in SQL.
//
// A filter a person types is "due this week", and a filter a database
// understands is a pair of instants. Resolving the first into the second needs
// a calendar — the company's ONE timezone, where a week starts, what the end
// of a month is — and none of that belongs in a WHERE clause: a query that
// computed it would compute it per row, and two nodes in two datacentres would
// compute it differently.
//
// So the resolution happens once, at parse time, against the company's clock,
// and what reaches the store is two numbers. That is also what makes it
// testable: every token below is a pure function of an instant and a location.
//
// # These are predicates on a task's OWN dates
//
// Never on its storage age. "Due before today" is a question about the work;
// "written more than a year ago" is a question about the log, and the log's
// retention is not a filter anybody types.

// DateAlias expands one of the vendor's own dynamic tokens into this grammar.
//
// ACCEPTED AS ALIASES rather than implemented separately, so the alias and the
// token it expands to can never disagree — which is the whole failure the
// second implementation would have: `overdue` as its own predicate would drift
// from the preset that means the same thing the first time either was tuned.
func DateAlias(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "next7":
		return "range:today..+7d", true
	case "last7":
		return "range:-7d..today", true
	case "thisweek":
		return "range:sow..eow", true
	case "thismonth":
		return "range:som..eom", true
	case "lastmonth":
		return "range:sopm..eopm", true
	case "earlier":
		return "lt:today", true
	}
	return "", false
}

// DateOp is a comparison against a date column.
type DateOp string

// The five comparisons, and each constant IS the token a filter value carries:
// [ParseDateFilter] cuts `lt:today` at the first colon, lowercases the prefix
// and casts it to a [DateOp]. So these spellings are a wire format rather than
// a naming choice — they are what a person or a model types, and what a saved
// view's params hold, so renaming one turns every filter already spelling it
// the old way into a refusal.
const (
	DateLT    DateOp = "lt"
	DateLTE   DateOp = "lte"
	DateGT    DateOp = "gt"
	DateGTE   DateOp = "gte"
	DateRange DateOp = "range"
)

// DateFilter is one resolved date predicate.
type DateFilter struct {
	Op DateOp

	// From and To are the resolved bounds. For every op but a range, only
	// From is meaningful.
	From period.Anchor
	To   period.Anchor

	// Overdue marks the one alias that is not purely a date: it carries an
	// open-status condition with it, and the preset that means the same
	// thing compiles to exactly this — so the two can never disagree.
	Overdue bool
}

// ParseDateFilter reads one date filter value.
func ParseDateFilter(value string, now time.Time, loc *time.Location) (DateFilter, error) {
	value = strings.TrimSpace(value)
	if expanded, aliased := DateAlias(value); aliased {
		value = expanded
	}
	if strings.EqualFold(value, "overdue") {
		at, err := period.Resolve("today", now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		return DateFilter{Op: DateLT, From: at, Overdue: true}, nil
	}
	op, rest, found := strings.Cut(value, ":")
	if !found {
		return DateFilter{}, fmt.Errorf("worklet: %q names no comparison; a "+
			"date filter is lt:, lte:, gt:, gte: or range:a..b", value)
	}
	switch DateOp(strings.ToLower(op)) {
	case DateLT, DateLTE, DateGT, DateGTE:
		at, err := period.Resolve(rest, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		return DateFilter{Op: DateOp(strings.ToLower(op)), From: at}, nil
	case DateRange:
		from, to, ok := strings.Cut(rest, "..")
		if !ok {
			return DateFilter{}, fmt.Errorf("worklet: a range is a..b and %q "+
				"carries no separator", rest)
		}
		lower, err := period.Resolve(from, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		upper, err := period.Resolve(to, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		if upper.At.Before(lower.At) {
			return DateFilter{}, fmt.Errorf("worklet: the range %q ends before "+
				"it starts, so it matches nothing", rest)
		}
		return DateFilter{Op: DateRange, From: lower, To: upper}, nil
	}
	return DateFilter{}, fmt.Errorf("worklet: %q is not a date comparison", op)
}
