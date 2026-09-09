package tracker

import (
	"fmt"
	"strconv"
	"strings"
	"time"
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

// DateAnchor is one end of a resolved date range.
type DateAnchor struct {
	// At is the resolved instant, in UTC.
	At time.Time

	// AllDay marks a token that named a DAY rather than an instant, so a
	// renderer can show "today" rather than midnight.
	AllDay bool
}

// weekStart is the day a week begins for the relative tokens.
//
// MONDAY, and it is a constant rather than a setting: the tokens exist so that
// "this week" means the same thing in a saved view, in a seat's tool call and
// on the dashboard, and a per-person or per-project week start would make one
// saved view answer two different questions depending on who opened it. A
// company whose week genuinely starts on Sunday writes the two dates.
const weekStart = time.Monday

// ResolveDate turns one relative token into an instant.
//
// The tokens are the ones a person types, and they come in three shapes: three
// day names, an offset in days, and eight calendar boundaries. Anything else is
// an RFC3339 timestamp, which is the caller's job rather than this function's —
// so an unknown token is an ERROR here rather than a silent zero, because a
// zero instant is 1 January year one and would match every task ever written.
func ResolveDate(token string, now time.Time, loc *time.Location) (DateAnchor, error) {
	if loc == nil {
		loc = time.UTC
	}
	token = strings.ToLower(strings.TrimSpace(token))
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	day := func(t time.Time) DateAnchor { return DateAnchor{At: t.UTC(), AllDay: true} }

	switch token {
	case "":
		return DateAnchor{}, fmt.Errorf("tracker: an empty date matches every " +
			"task ever written; name an instant or a relative token")
	case "now":
		return DateAnchor{At: now.UTC()}, nil
	case "today":
		return day(midnight), nil
	case "yesterday":
		return day(midnight.AddDate(0, 0, -1)), nil
	case "tomorrow":
		return day(midnight.AddDate(0, 0, 1)), nil
	case "sow":
		return day(startOfWeek(midnight)), nil
	case "eow":
		return day(startOfWeek(midnight).AddDate(0, 0, 7)), nil
	case "sopw":
		return day(startOfWeek(midnight).AddDate(0, 0, -7)), nil
	case "eopw":
		return day(startOfWeek(midnight)), nil
	case "sonw":
		return day(startOfWeek(midnight).AddDate(0, 0, 7)), nil
	case "eonw":
		return day(startOfWeek(midnight).AddDate(0, 0, 14)), nil
	case "som":
		return day(startOfMonth(midnight, loc)), nil
	case "eom":
		return day(startOfMonth(midnight, loc).AddDate(0, 1, 0)), nil
	case "sopm":
		return day(startOfMonth(midnight, loc).AddDate(0, -1, 0)), nil
	case "eopm":
		return day(startOfMonth(midnight, loc)), nil
	case "sonm":
		return day(startOfMonth(midnight, loc).AddDate(0, 1, 0)), nil
	case "eonm":
		return day(startOfMonth(midnight, loc).AddDate(0, 2, 0)), nil
	}

	// ±<n>d, the one token with an argument.
	if days, ok := offsetDays(token); ok {
		return day(midnight.AddDate(0, 0, days)), nil
	}

	// An absolute instant. Both spellings, because a person types a date
	// and a machine sends a timestamp.
	if at, err := time.Parse(time.RFC3339, token); err == nil {
		return DateAnchor{At: at.UTC()}, nil
	}
	if at, err := time.ParseInLocation("2006-01-02", token, loc); err == nil {
		return day(at), nil
	}
	return DateAnchor{}, fmt.Errorf("tracker: %q is not a date: name an instant, "+
		"a calendar date, an offset like +7d, or one of the relative tokens", token)
}

// EVERY END TOKEN IS THE START OF THE NEXT PERIOD, and that is deliberate.
//
// A range is half-open — `>= start AND < end` — so "this week" is Monday
// midnight to next Monday midnight and contains every instant of Sunday. The
// alternative, an end at 23:59:59, silently drops the last second of the last
// day and drops it differently depending on whether the column stores seconds
// or milliseconds.

// startOfWeek is midnight on the week's first day, in the given local time.
func startOfWeek(midnight time.Time) time.Time {
	back := (int(midnight.Weekday()) - int(weekStart) + 7) % 7
	return midnight.AddDate(0, 0, -back)
}

// startOfMonth is midnight on the first of the month.
func startOfMonth(midnight time.Time, loc *time.Location) time.Time {
	return time.Date(midnight.Year(), midnight.Month(), 1, 0, 0, 0, 0, loc)
}

// offsetDays reads a ±<n>d token.
func offsetDays(token string) (int, bool) {
	rest, ok := strings.CutSuffix(token, "d")
	if !ok || rest == "" {
		return 0, false
	}
	if rest[0] == '+' {
		rest = rest[1:]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

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
	From DateAnchor
	To   DateAnchor

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
		at, err := ResolveDate("today", now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		return DateFilter{Op: DateLT, From: at, Overdue: true}, nil
	}
	op, rest, found := strings.Cut(value, ":")
	if !found {
		return DateFilter{}, fmt.Errorf("tracker: %q names no comparison — a "+
			"date filter is lt:, lte:, gt:, gte: or range:a..b", value)
	}
	switch DateOp(strings.ToLower(op)) {
	case DateLT, DateLTE, DateGT, DateGTE:
		at, err := ResolveDate(rest, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		return DateFilter{Op: DateOp(strings.ToLower(op)), From: at}, nil
	case DateRange:
		from, to, ok := strings.Cut(rest, "..")
		if !ok {
			return DateFilter{}, fmt.Errorf("tracker: a range is a..b and %q "+
				"carries no separator", rest)
		}
		lower, err := ResolveDate(from, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		upper, err := ResolveDate(to, now, loc)
		if err != nil {
			return DateFilter{}, err
		}
		if upper.At.Before(lower.At) {
			return DateFilter{}, fmt.Errorf("tracker: the range %q ends before "+
				"it starts, so it matches nothing", rest)
		}
		return DateFilter{Op: DateRange, From: lower, To: upper}, nil
	}
	return DateFilter{}, fmt.Errorf("tracker: %q is not a date comparison", op)
}
