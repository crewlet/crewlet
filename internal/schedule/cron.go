package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The 5-field cron evaluator.
//
//	┌───────────── minute        (0-59)
//	│ ┌───────────── hour         (0-23)
//	│ │ ┌───────────── day-of-month (1-31)
//	│ │ │ ┌───────────── month       (1-12 or JAN-DEC)
//	│ │ │ │ ┌───────────── day-of-week (0-7 or SUN-SAT; 0 and 7 = Sunday)
//	│ │ │ │ │
//	* * * * *
//
// Each field accepts `*`, single values, `a-b` ranges, `*/step` and `a-b/step`
// steps, `a/step` (= `a-max/step`), comma-separated lists, and three-letter
// month / day names.
//
// Vixie day semantics: when BOTH day-of-month and day-of-week are restricted
// (neither is a bare `*`), a day matches if EITHER field matches. When only
// one is restricted, that field alone decides.
//
// Dependency-free on purpose. A cron library is a large surface for a grammar
// that fits on a screen, and the engine's runtime dependency set is the thing
// an operator installs.

// ErrCron is the sentinel every parse failure wraps. Callers branch on it
// with errors.Is; the wrapped message names the expression and the field, so
// a company with twenty schedules gets a line that says which one to fix.
var ErrCron = errors.New("invalid cron expression")

// Horizon bounds how far Next and Prev will scan before reporting that an
// expression has no fire in that direction.
//
// Sized to the GRAMMAR, not to a round number. The longest gap between two
// fires of any valid 5-field expression is `0 0 29 2 *` across a century that
// is not a leap year — 2096-02-29 to 2104-02-29 is 2921 days, because 2100 is
// divisible by 100 and not by 400. Anything shorter reports "never" for a
// legitimate quadrennial schedule, which is what the Python evaluator's
// 400-day scan did in three years out of four (and in seven out of eight
// across the century gap): the dashboard drew no next run, and the catchup
// window silently fell back to its minimum clamp.
//
// The cost of the larger horizon falls only on an expression that never
// matches at all (`0 0 30 2 *` — February 30th), because every reachable one
// terminates at its next fire. Measured, not assumed: BenchmarkNextUnreachable
// walks the whole horizon in 143 ms, BenchmarkNextDaily finds an ordinary
// weekday fire in 25 µs. The full walk is reachable at most twice per such
// schedule on the first tick (the catchup window asks for two fires) and once
// per schedule per dashboard projection, so a company that writes an
// impossible date pays milliseconds where it currently gets a wrong answer.
// See the note on scanning in [Expr.Next].
const Horizon = 2925 * 24 * time.Hour

// Expr is a parsed 5-field cron expression.
//
// Fields are bitmasks, which makes an Expr COMPARABLE: two expressions that
// mean the same thing are ==, so `MON` and `1` can be asserted equal in one
// line rather than field by field — and a field added later cannot escape
// that comparison the way a hand-written equality would.
//
// The zero Expr matches nothing. That is the safe zero for a struct field
// nobody parsed into: a schedule carrying an unset Expr fires never, rather
// than firing every minute the way an all-ones default would.
type Expr struct {
	minutes uint64 // bits 0..59
	hours   uint32 // bits 0..23
	doms    uint32 // bits 1..31
	months  uint16 // bits 1..12
	dows    uint8  // bits 0..6, with 7 normalised to 0

	// domRestricted and dowRestricted record whether each day field was
	// anything other than a bare `*`. They drive the Vixie OR rule, and
	// they cannot be recovered from the masks: `*` and `0-6` produce the
	// same day-of-week mask and mean opposite things for that rule.
	domRestricted bool
	dowRestricted bool
}

var months = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var daysOfWeek = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// cronFields is the number of whitespace-separated fields in the grammar.
const cronFields = 5

// Parse compiles a 5-field cron expression.
func Parse(expr string) (Expr, error) {
	fields := strings.Fields(expr)
	if len(fields) != cronFields {
		return Expr{}, fmt.Errorf("%w %q: expected %d fields (minute hour day-of-month month day-of-week), got %d",
			ErrCron, expr, cronFields, len(fields))
	}

	minutes, _, err := parseField(expr, "minute", fields[0], 0, 59, nil)
	if err != nil {
		return Expr{}, err
	}
	hours, _, err := parseField(expr, "hour", fields[1], 0, 23, nil)
	if err != nil {
		return Expr{}, err
	}
	doms, domRestricted, err := parseField(expr, "day-of-month", fields[2], 1, 31, nil)
	if err != nil {
		return Expr{}, err
	}
	mons, _, err := parseField(expr, "month", fields[3], 1, 12, months)
	if err != nil {
		return Expr{}, err
	}
	// Day-of-week accepts 0-7 and both ends are Sunday, so 7 folds onto 0
	// before the mask is stored. Folding at parse time is what makes
	// `* * * * 7` and `* * * * 0` compare equal.
	rawDows, dowRestricted, err := parseField(expr, "day-of-week", fields[4], 0, 7, daysOfWeek)
	if err != nil {
		return Expr{}, err
	}
	dows := rawDows
	if dows&(1<<7) != 0 {
		dows = (dows &^ (1 << 7)) | 1
	}

	return Expr{
		minutes:       minutes,
		hours:         uint32(hours),
		doms:          uint32(doms),
		months:        uint16(mons),
		dows:          uint8(dows),
		domRestricted: domRestricted,
		dowRestricted: dowRestricted,
	}, nil
}

// Validate reports whether an expression parses, discarding the result. It is
// what config validation calls, so a bad expression fails `crewlet validate`
// rather than silently at 9am.
func Validate(expr string) error {
	_, err := Parse(expr)
	return err
}

// parseField compiles one field into a bitmask plus whether it was restricted
// — anything other than a bare `*`.
func parseField(expr, name, spec string, lo, hi int, aliases map[string]int) (uint64, bool, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, false, fmt.Errorf("%w %q: empty %s field", ErrCron, expr, name)
	}
	restricted := spec != "*"

	var mask uint64
	for _, term := range strings.Split(spec, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return 0, false, fmt.Errorf("%w %q: empty term in %s field %q", ErrCron, expr, name, spec)
		}

		base, stepStr, hasStep := strings.Cut(term, "/")
		step := 1
		if hasStep {
			var err error
			if step, err = strconv.Atoi(strings.TrimSpace(stepStr)); err != nil {
				return 0, false, fmt.Errorf("%w %q: %s field: invalid step %q", ErrCron, expr, name, stepStr)
			}
			if step <= 0 {
				return 0, false, fmt.Errorf("%w %q: %s field: step must be positive, got %d", ErrCron, expr, name, step)
			}
		}

		base = strings.TrimSpace(base)
		var start, end int
		switch {
		case base == "*":
			start, end = lo, hi
		case strings.Contains(base[1:], "-"):
			// base[1:] because a leading '-' is not a range separator; it
			// is a negative value, which parseValue rejects with a message
			// about the value rather than about the range.
			startStr, endStr, _ := strings.Cut(base, "-")
			var err error
			if start, err = parseValue(expr, name, startStr, lo, hi, aliases); err != nil {
				return 0, false, err
			}
			if end, err = parseValue(expr, name, endStr, lo, hi, aliases); err != nil {
				return 0, false, err
			}
			if end < start {
				return 0, false, fmt.Errorf("%w %q: %s field: range %q is descending — ranges do not wrap, "+
					"so a weekend is 6-7, sat,sun or 0,6", ErrCron, expr, name, base)
			}
		default:
			var err error
			if start, err = parseValue(expr, name, base, lo, hi, aliases); err != nil {
				return 0, false, err
			}
			// `a/step` means a, a+step, ... up to the field maximum. A
			// bare `a` is the single value.
			end = start
			if hasStep {
				end = hi
			}
		}

		for v := start; v <= end; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, restricted, nil
}

// parseValue resolves one value token, honouring the field's name aliases.
func parseValue(expr, name, token string, lo, hi int, aliases map[string]int) (int, error) {
	key := strings.ToLower(strings.TrimSpace(token))
	if v, ok := aliases[key]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(key)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %s field: %q is not a number or a name for this field", ErrCron, expr, name, token)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%w %q: %s field: value %d is outside %d-%d", ErrCron, expr, name, v, lo, hi)
	}
	return v, nil
}

// Matches reports whether local — an instant already projected into the
// schedule's own timezone — is a fire time. Matching is minute-granular;
// seconds and below are ignored.
func (e Expr) Matches(local time.Time) bool {
	// Cheapest gate first: the minute rejects 59 candidates out of 60 for a
	// typical expression, and each of these accessors decomposes the
	// instant independently.
	if e.minutes&(1<<uint(local.Minute())) == 0 {
		return false
	}
	if e.hours&(1<<uint(local.Hour())) == 0 {
		return false
	}
	if e.months&(1<<uint(local.Month())) == 0 {
		return false
	}
	domOK := e.doms&(1<<uint(local.Day())) != 0
	// Go's time.Weekday is Sunday=0..Saturday=6, which is already cron's
	// numbering — the Python evaluator had to rotate its Monday-based
	// weekday() by hand.
	dowOK := e.dows&(1<<uint(local.Weekday())) != 0

	switch {
	case e.domRestricted && e.dowRestricted:
		return domOK || dowOK
	case e.domRestricted:
		return domOK
	case e.dowRestricted:
		return dowOK
	default:
		return true
	}
}

// FireTimes returns the UTC fire times in (after, until] whose projection
// into loc matches.
//
// The lower bound is EXCLUSIVE and the upper bound inclusive, which is what
// makes consecutive tick windows partition time: a fire landing exactly on a
// tick boundary belongs to one window, never to both. Both bounds are used at
// minute granularity, so a tick clock carrying seconds behaves the same as one
// that does not.
func (e Expr) FireTimes(after, until time.Time, loc *time.Location) []time.Time {
	if !until.After(after) {
		return nil
	}
	var out []time.Time
	for m := firstMinuteAfter(after); !m.After(until); m = m.Add(time.Minute) {
		if e.Matches(m.In(loc)) {
			out = append(out, m)
		}
	}
	return out
}

// Next returns the first UTC fire time strictly after `after`, reporting
// false when the expression has none within [Horizon].
//
// False is not an error: `0 0 30 2 *` parses and simply never happens. The
// callers treat it as "this schedule has no knowable period", which the
// catchup window resolves to its minimum clamp.
//
// # Why this walks minute by minute
//
// The walk iterates UTC instants and matches their LOCAL projection, and that
// direction is the whole DST design: every UTC instant has exactly one local
// time, so a repeated local hour yields two fire instants and a vanished one
// yields none, with no ambiguity to resolve. Constructing local times instead
// — the obvious way to skip whole days — has to invent an answer in both of
// those cases, and Go's time.Date explicitly does not promise which one it
// picks in a gap.
//
// Skipping ahead by a whole hour is also unavailable: zone offsets are not all
// whole hours (Asia/Kathmandu is +05:45, Pacific/Chatham +12:45), so an
// hour-granular jump lands on the wrong minute in exactly the zones nobody
// tests. The scan is therefore linear, and bounded: every expression with a
// reachable fire terminates at it, and only an impossible date pays the full
// horizon.
func (e Expr) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	m := firstMinuteAfter(after)
	limit := m.Add(Horizon)
	for ; !m.After(limit); m = m.Add(time.Minute) {
		if e.Matches(m.In(loc)) {
			return m, true
		}
	}
	return time.Time{}, false
}

// Prev returns the most recent UTC fire time at or before `at`, reporting
// false when there is none within [Horizon].
//
// Inclusive of its own minute where [Expr.Next] is exclusive of it. The
// asymmetry is what the two callers need: Prev answers "what was the last
// fire, counting this instant", Next answers "what is the next one, this
// instant being already spent".
func (e Expr) Prev(at time.Time, loc *time.Location) (time.Time, bool) {
	m := floorMinute(at)
	limit := m.Add(-Horizon)
	for ; !m.Before(limit); m = m.Add(-time.Minute) {
		if e.Matches(m.In(loc)) {
			return m, true
		}
	}
	return time.Time{}, false
}

// floorMinute truncates to the minute in UTC.
//
// Truncate also strips the monotonic reading a wall clock carries, which is
// what keeps the scan's arithmetic pure wall time — the only kind that can be
// compared against an instant reconstructed from a stored timestamp.
func floorMinute(t time.Time) time.Time { return t.UTC().Truncate(time.Minute) }

// firstMinuteAfter is the first minute-aligned instant strictly after t.
func firstMinuteAfter(t time.Time) time.Time {
	m := floorMinute(t)
	for !m.After(t) {
		m = m.Add(time.Minute)
	}
	return m
}
