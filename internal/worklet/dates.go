package worklet

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// Relative dates resolve HERE, once, at parse time, against the company's ONE
// timezone, never in SQL: a WHERE clause would compute the calendar per row, and
// two nodes in two datacentres would compute it differently. The store sees two
// instants, and every token is a pure function of an instant and a location.
//
// These are predicates on a task's OWN dates, never its storage age: the log's
// retention is not a filter anybody types.

// DateAlias expands one of the vendor's own dynamic tokens into this grammar,
// as an alias rather than a second implementation that would drift from the
// token it means.
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

// The five comparisons. Each constant IS the token a filter value carries
// ([ParseDateFilter] casts the `lt:` prefix to a [DateOp]), so the spellings are
// a wire format: renaming one refuses every saved view already spelling it.
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

	// From and To are the resolved bounds; only a range uses To.
	From period.Anchor
	To   period.Anchor

	// Overdue marks the one alias that is not purely a date: it carries an
	// open-status condition, and the preset meaning the same compiles to
	// exactly this, so the two cannot disagree.
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
