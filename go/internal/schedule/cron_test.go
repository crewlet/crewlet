package schedule_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
)

// The cases below are the Python suite (tests/test_schedule/test_cron.py)
// ported case for case, plus the ones Go's own shape makes reachable. Where a
// Python case pinned an evaluator behaviour the two DST directions depend on,
// the doc comment travels with it: those are the reason the evaluator iterates
// UTC and matches the LOCAL projection rather than the other way round.

func mustParse(t *testing.T, expr string) schedule.Expr {
	t.Helper()
	e, err := schedule.Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return e
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Not a skip. A missing zoneinfo would make every DST case below
		// pass against UTC, which is exactly the reading they exist to
		// disprove.
		t.Fatalf("LoadLocation(%q): %v — this suite's DST cases are meaningless without it", name, err)
	}
	return loc
}

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// --- parsing --------------------------------------------------------------

func TestParseAcceptsTheGrammar(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{
		"0 9 * * 1-5",
		"*/15 * * * *",
		"0 9,17 * * *",
		"0 0 1 * *",
		"0 9 * * MON",
		"30 9 * * 5",
		"0 0 13 * 5",
		"0 0 * jan-mar *",
		// Go-side additions: the forms the Python parser accepts but no
		// Python case ever sent, so nothing pinned them.
		"0-30/10 * * * *",   // range with a step
		"5/15 * * * *",      // a/step, meaning a..max by step
		"0 0 * * SUN",       // a day NAME, where the Python only sent MON
		"0 0 * DEC *",       // a month name on its own
		"  0   9  *  *  * ", // arbitrary inter-field whitespace
		"0 9 * * 7",         // 7 is Sunday
	} {
		if err := schedule.Validate(expr); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", expr, err)
		}
	}
}

func TestParseRejectsWhatItCannotEvaluate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ expr, why string }{
		{"", "empty"},
		{"* * * *", "4 fields"},
		{"* * * * * *", "6 fields"},
		{"60 * * * *", "minute out of range"},
		{"* 24 * * *", "hour out of range"},
		{"0 9 * * 8", "dow out of range (max 7)"},
		{"0 9 32 * *", "dom out of range"},
		{"0 9 0 * *", "dom below range — day 0 does not exist"},
		{"5-1 * * * *", "descending range"},
		{"*/0 * * * *", "zero step"},
		{"*/-1 * * * *", "negative step"},
		{"abc * * * *", "not a number and not a name"},
		{"0 9 * * sat-sun", "descending day range: Sunday is 0, so sat-sun descends"},
		{"0,,5 * * * *", "empty term"},
		{"0 9 * mon *", "a day name in the month field"},
		{"0 9 * * jan", "a month name in the day field"},
		{"*/ * * * *", "a step with nothing after the slash"},
	} {
		err := schedule.Validate(tc.expr)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want an error (%s)", tc.expr, tc.why)
			continue
		}
		if !errors.Is(err, schedule.ErrCron) {
			t.Errorf("Validate(%q) = %v, want it to wrap ErrCron so callers can branch", tc.expr, err)
		}
		// The message has to name the expression: a config with twenty
		// schedules reports one line, and "invalid step" alone does not
		// say which schedule to go and fix.
		if tc.expr != "" && !strings.Contains(err.Error(), tc.expr) {
			t.Errorf("Validate(%q) = %v, want the message to quote the expression", tc.expr, err)
		}
	}
}

func TestTheZeroExprMatchesNothing(t *testing.T) {
	t.Parallel()
	// An Expr nobody parsed into fires nothing. That is the safe zero: a
	// struct field left unset must not become "* * * * *".
	var zero schedule.Expr
	for h := 0; h < 24; h++ {
		if zero.Matches(at(2026, time.June, 8, h, 0)) {
			t.Fatalf("the zero Expr matched %02d:00", h)
		}
	}
}

// --- matching -------------------------------------------------------------

func TestWeekdayNineAM(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * 1-5")
	// 2026-06-08 is a Monday.
	requireMatch(t, e, at(2026, time.June, 8, 9, 0), true)
	requireMatch(t, e, at(2026, time.June, 8, 9, 1), false) // wrong minute
	requireMatch(t, e, at(2026, time.June, 8, 8, 0), false) // wrong hour
	// 2026-06-13 is a Saturday, 2026-06-14 a Sunday.
	requireMatch(t, e, at(2026, time.June, 13, 9, 0), false)
	requireMatch(t, e, at(2026, time.June, 14, 9, 0), false)
}

func TestStepAndList(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "*/15 * * * *")
	requireMatch(t, e, at(2026, time.June, 8, 12, 0), true)
	requireMatch(t, e, at(2026, time.June, 8, 12, 15), true)
	requireMatch(t, e, at(2026, time.June, 8, 12, 45), true)
	requireMatch(t, e, at(2026, time.June, 8, 12, 7), false)

	e2 := mustParse(t, "0 9,17 * * *")
	requireMatch(t, e2, at(2026, time.June, 8, 9, 0), true)
	requireMatch(t, e2, at(2026, time.June, 8, 17, 0), true)
	requireMatch(t, e2, at(2026, time.June, 8, 13, 0), false)
}

func TestRangeWithAStepStopsAtTheRangeEnd(t *testing.T) {
	t.Parallel()
	// 0-30/10 is 0,10,20,30 — NOT every tenth minute of the hour. The
	// Python parser has this behaviour and no Python case sent the form.
	e := mustParse(t, "0-30/10 * * * *")
	for _, mi := range []int{0, 10, 20, 30} {
		requireMatch(t, e, at(2026, time.June, 8, 12, mi), true)
	}
	for _, mi := range []int{5, 40, 50} {
		requireMatch(t, e, at(2026, time.June, 8, 12, mi), false)
	}
}

func TestBareValueWithAStepRunsToTheFieldMaximum(t *testing.T) {
	t.Parallel()
	// a/step means a, a+step, ... up to the field's maximum — a bare `a`
	// is the single value. Both halves of that rule in one case.
	e := mustParse(t, "5/15 * * * *")
	for _, mi := range []int{5, 20, 35, 50} {
		requireMatch(t, e, at(2026, time.June, 8, 12, mi), true)
	}
	requireMatch(t, e, at(2026, time.June, 8, 12, 0), false)

	single := mustParse(t, "5 * * * *")
	requireMatch(t, single, at(2026, time.June, 8, 12, 5), true)
	requireMatch(t, single, at(2026, time.June, 8, 12, 20), false)
}

func TestDayNamesAndSundayAliases(t *testing.T) {
	t.Parallel()
	// Expr is a comparable struct precisely so this reads as one equality
	// rather than a field-by-field walk a new field could escape.
	if got, want := mustParse(t, "0 9 * * MON"), mustParse(t, "0 9 * * 1"); got != want {
		t.Fatalf("MON and 1 parsed differently: %+v vs %+v", got, want)
	}
	// 7 and 0 both mean Sunday, so the two expressions must be identical
	// after normalisation — not merely agree on the Sundays we test.
	if got, want := mustParse(t, "0 9 * * 7"), mustParse(t, "0 9 * * 0"); got != want {
		t.Fatalf("7 and 0 parsed differently: %+v vs %+v", got, want)
	}
	requireMatch(t, mustParse(t, "0 9 * * 0"), at(2026, time.June, 14, 9, 0), true) // a Sunday
	// Case folding, which the Python does with .lower() and no case sends.
	if got, want := mustParse(t, "0 9 * * Sun"), mustParse(t, "0 9 * * SUN"); got != want {
		t.Fatalf("day names are case-sensitive: %+v vs %+v", got, want)
	}
	if got, want := mustParse(t, "0 0 * Jan *"), mustParse(t, "0 0 * 1 *"); got != want {
		t.Fatalf("JAN and 1 parsed differently: %+v vs %+v", got, want)
	}
}

func TestDayOfMonthAndDayOfWeekOR(t *testing.T) {
	t.Parallel()
	// Vixie semantics: with BOTH day fields restricted a day matches if
	// either does. "the 13th of the month OR any Friday".
	e := mustParse(t, "0 0 13 * 5")
	requireMatch(t, e, at(2026, time.June, 13, 0, 0), true)  // the 13th, a Saturday
	requireMatch(t, e, at(2026, time.June, 12, 0, 0), true)  // a Friday, not the 13th
	requireMatch(t, e, at(2026, time.June, 10, 0, 0), false) // Wednesday, not the 13th
}

func TestOnlyOneDayFieldRestrictedDecidesAlone(t *testing.T) {
	t.Parallel()
	dom := mustParse(t, "0 0 1 * *")
	requireMatch(t, dom, at(2026, time.June, 1, 0, 0), true)
	requireMatch(t, dom, at(2026, time.June, 2, 0, 0), false)

	// The other half of the same rule, which the Python suite never sent:
	// dow restricted alone must NOT be widened by the unrestricted dom.
	dow := mustParse(t, "0 0 * * 1")
	requireMatch(t, dow, at(2026, time.June, 8, 0, 0), true)  // Monday
	requireMatch(t, dow, at(2026, time.June, 9, 0, 0), false) // Tuesday
}

func TestAStarStepIsRestrictedForTheDayRule(t *testing.T) {
	t.Parallel()
	// `*/2` is not a bare `*`, so it RESTRICTS — which decides whether the
	// day rule ORs or ANDs. "*/2 in dom, 1 in dow" must match every Monday
	// as well as every odd day, not just odd Mondays. (Odd, not even: the
	// day-of-month field starts at 1, so */2 walks 1,3,5,… — the same
	// footgun in every cron.)
	e := mustParse(t, "0 0 */2 * 1")
	requireMatch(t, e, at(2026, time.June, 3, 0, 0), true)  // odd day, a Wednesday
	requireMatch(t, e, at(2026, time.June, 8, 0, 0), true)  // even day, a Monday
	requireMatch(t, e, at(2026, time.June, 2, 0, 0), false) // even day, a Tuesday
}

// --- windowed iteration + next/prev ---------------------------------------

func TestFireTimesWindow(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * *")
	got := e.FireTimes(at(2026, time.June, 8, 8, 59), at(2026, time.June, 8, 9, 0), time.UTC)
	requireTimes(t, got, at(2026, time.June, 8, 9, 0))
}

func TestFireTimesLowerBoundIsExclusive(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * *")
	// A fire exactly at the lower bound belongs to the previous window; it
	// is what stops a tick boundary landing on a fire minute from firing
	// it twice.
	got := e.FireTimes(at(2026, time.June, 8, 9, 0), at(2026, time.June, 8, 9, 5), time.UTC)
	requireTimes(t, got)
}

func TestFireTimesUpperBoundIsInclusive(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * *")
	got := e.FireTimes(at(2026, time.June, 8, 8, 0), at(2026, time.June, 8, 9, 0), time.UTC)
	requireTimes(t, got, at(2026, time.June, 8, 9, 0))
}

func TestFireTimesEmptyOrInvertedWindow(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "* * * * *")
	if got := e.FireTimes(at(2026, time.June, 8, 9, 0), at(2026, time.June, 8, 9, 0), time.UTC); len(got) != 0 {
		t.Errorf("a zero-width window returned %v, want nothing", got)
	}
	if got := e.FireTimes(at(2026, time.June, 8, 9, 5), at(2026, time.June, 8, 9, 0), time.UTC); len(got) != 0 {
		t.Errorf("an inverted window returned %v, want nothing", got)
	}
}

func TestFireTimesIgnoresSubMinutePrecision(t *testing.T) {
	t.Parallel()
	// The tick clock is a wall clock, so both bounds arrive with seconds on
	// them. Matching is minute-granular: a bound mid-minute must behave
	// like the same bound on the minute for the UPPER edge, and must still
	// exclude its own minute on the LOWER edge.
	e := mustParse(t, "0 9 * * *")
	after := at(2026, time.June, 8, 8, 59).Add(30 * time.Second)
	until := at(2026, time.June, 8, 9, 0).Add(30 * time.Second)
	requireTimes(t, e.FireTimes(after, until, time.UTC), at(2026, time.June, 8, 9, 0))

	// The lower bound lands ON the fire minute but 30s past it. The fire
	// instant (09:00:00) is not strictly after 09:00:30, so it is excluded
	// — the same rule as the exact-boundary case, and the reason a tick
	// running late does not re-fire the minute it already covered.
	requireTimes(t, e.FireTimes(
		at(2026, time.June, 8, 9, 0).Add(30*time.Second),
		at(2026, time.June, 8, 9, 5),
		time.UTC))
}

func TestFireTimesReturnsInstantsInOrder(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "*/15 * * * *")
	got := e.FireTimes(at(2026, time.June, 8, 11, 59), at(2026, time.June, 8, 13, 0), time.UTC)
	requireTimes(t, got,
		at(2026, time.June, 8, 12, 0),
		at(2026, time.June, 8, 12, 15),
		at(2026, time.June, 8, 12, 30),
		at(2026, time.June, 8, 12, 45),
		at(2026, time.June, 8, 13, 0),
	)
}

func TestTimezoneConversion(t *testing.T) {
	t.Parallel()
	// 09:30 in Amsterdam (CEST = UTC+2 in June) is 07:30 UTC.
	e := mustParse(t, "30 9 * * *")
	loc := mustLoad(t, "Europe/Amsterdam")
	got, ok := e.Next(at(2026, time.June, 8, 0, 0), loc)
	if !ok || !got.Equal(at(2026, time.June, 8, 7, 30)) {
		t.Fatalf("Next = %v, %v; want 2026-06-08T07:30Z", got, ok)
	}
}

func TestNextAndPrev(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * *")
	after := at(2026, time.June, 8, 10, 0)
	if got, ok := e.Next(after, time.UTC); !ok || !got.Equal(at(2026, time.June, 9, 9, 0)) {
		t.Errorf("Next = %v, %v; want 2026-06-09T09:00Z", got, ok)
	}
	if got, ok := e.Prev(after, time.UTC); !ok || !got.Equal(at(2026, time.June, 8, 9, 0)) {
		t.Errorf("Prev = %v, %v; want 2026-06-08T09:00Z", got, ok)
	}
	// Prev is inclusive of its own minute; Next is exclusive of it. The
	// asymmetry is deliberate: Prev answers "what was the last fire, and
	// this instant counts", Next answers "what is the next one, and this
	// instant is already spent".
	boundary := at(2026, time.June, 8, 9, 0)
	if got, ok := e.Prev(boundary, time.UTC); !ok || !got.Equal(boundary) {
		t.Errorf("Prev(a fire minute) = %v, %v; want the same instant back", got, ok)
	}
	if got, ok := e.Next(boundary, time.UTC); !ok || !got.Equal(at(2026, time.June, 9, 9, 0)) {
		t.Errorf("Next(a fire minute) = %v, %v; want tomorrow's fire", got, ok)
	}
}

func TestNextAndPrevIgnoreSubMinutePrecision(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 9 * * *")
	// Prev floors to the minute, so 09:00:30 still finds 09:00.
	if got, ok := e.Prev(at(2026, time.June, 8, 9, 0).Add(30*time.Second), time.UTC); !ok ||
		!got.Equal(at(2026, time.June, 8, 9, 0)) {
		t.Errorf("Prev mid-minute = %v, %v; want 09:00Z", got, ok)
	}
	// Next is strictly after, so 08:59:30 finds this morning's 09:00.
	if got, ok := e.Next(at(2026, time.June, 8, 8, 59).Add(30*time.Second), time.UTC); !ok ||
		!got.Equal(at(2026, time.June, 8, 9, 0)) {
		t.Errorf("Next mid-minute = %v, %v; want 09:00Z", got, ok)
	}
}

func TestAnExpressionWithNoReachableDayReportsNotFound(t *testing.T) {
	t.Parallel()
	// February 30th. A valid expression by the grammar and an impossible
	// date, so the scan runs the whole horizon and comes back empty rather
	// than looping forever. `false`, not an error: the expression parsed.
	e := mustParse(t, "0 0 30 2 *")
	if got, ok := e.Next(at(2026, time.June, 8, 0, 0), time.UTC); ok {
		t.Fatalf("Next = %v, want not found", got)
	}
	if got, ok := e.Prev(at(2026, time.June, 8, 0, 0), time.UTC); ok {
		t.Fatalf("Prev = %v, want not found", got)
	}
}

func TestTheHorizonReachesTheRarestLegalFire(t *testing.T) {
	t.Parallel()
	// The largest gap any 5-field expression can have is February 29th
	// across a century that is not a leap year: 2096-02-29 to 2104-02-29,
	// because 2100 is divisible by 100 and not by 400.
	//
	// The Python evaluator scanned 400 days and so reported "never" for a
	// quadrennial schedule in three years out of four — and for this one in
	// seven years out of eight. The horizon here is sized to the grammar
	// rather than to a round number; this case is what holds it there.
	e := mustParse(t, "0 0 29 2 *")
	got, ok := e.Next(at(2096, time.March, 1, 0, 0), time.UTC)
	if !ok {
		t.Fatalf("Next from 2096-03-01 = not found; the horizon is shorter than the "+
			"longest gap the grammar allows (%v)", schedule.Horizon)
	}
	if want := at(2104, time.February, 29, 0, 0); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

// --- DST, both directions -------------------------------------------------
//
// The evaluator iterates UTC and matches the LOCAL projection, which is what
// makes DST unambiguous: every UTC instant has exactly one local time. The
// two consequences are opposite, and the evaluator owns only one half of each
// — the ledger's fire label owns the other.

func TestARepeatedLocalHourYieldsTwoInstants(t *testing.T) {
	t.Parallel()
	// Fall-back: the local minute happens twice, so the evaluator reports
	// both UTC instants.
	//
	// Collapsing them is the LEDGER's job, not the evaluator's — the fire
	// label is the local wall-clock stamp, so both share one identity and
	// the schedule fires once. That split is deliberate, and this pins the
	// evaluator's half of it: a change here that silently dropped one
	// instant would move DST correctness into a place no test covers.
	loc := mustLoad(t, "Europe/London") // 2026-10-25: 02:00 BST -> 01:00 GMT
	e := mustParse(t, "30 1 * * *")
	got := e.FireTimes(at(2026, time.October, 25, 0, 0), at(2026, time.October, 25, 12, 0), loc)
	requireTimes(t, got, at(2026, time.October, 25, 0, 30), at(2026, time.October, 25, 1, 30))
	for _, ft := range got {
		if local := ft.In(loc).Format("1504"); local != "0130" {
			t.Fatalf("%v is %s local, want both instants to be 01:30 — one wall-clock "+
				"minute, two instants", ft, local)
		}
	}
}

func TestASkippedLocalHourDoesNotFireThatDay(t *testing.T) {
	t.Parallel()
	// Spring-forward: the local minute does not exist, so nothing matches
	// and the schedule silently misses that day.
	//
	// Documented, and the reason the docs tell you to check WHICH hour
	// vanishes in your zone rather than assuming 02:00 — in Europe/London
	// it is 01:00-01:59, so this very ordinary-looking daily schedule is
	// the one that skips.
	loc := mustLoad(t, "Europe/London") // 2027-03-28: 01:00 GMT -> 02:00 BST
	e := mustParse(t, "30 1 * * *")
	got := e.FireTimes(at(2027, time.March, 26, 12, 0), at(2027, time.March, 30, 12, 0), loc)
	requireLocalDays(t, got, loc, "2027-03-27", "2027-03-29", "2027-03-30")
}

func TestAnHourOutsideTheGapStillFiresOnTheTransitionDay(t *testing.T) {
	t.Parallel()
	loc := mustLoad(t, "Europe/London")
	e := mustParse(t, "30 5 * * *")
	got := e.FireTimes(at(2027, time.March, 26, 12, 0), at(2027, time.March, 30, 12, 0), loc)
	requireLocalDays(t, got, loc, "2027-03-27", "2027-03-28", "2027-03-29", "2027-03-30")
}

func TestNextSkipsAVanishedLocalHour(t *testing.T) {
	t.Parallel()
	// The other reader of the same fact. Next must land on the following
	// day's fire rather than on a nonexistent local instant — a scanner
	// that constructed local times instead of projecting UTC ones would
	// synthesise 01:30 out of the gap and return an instant no clock in
	// that zone ever showed.
	loc := mustLoad(t, "Europe/London")
	e := mustParse(t, "30 1 * * *")
	got, ok := e.Next(at(2027, time.March, 27, 12, 0), loc)
	if !ok {
		t.Fatal("Next = not found across a spring-forward")
	}
	if day := got.In(loc).Format("2006-01-02"); day != "2027-03-29" {
		t.Fatalf("Next = %v (%s local), want the 29th — the 28th's 01:30 does not exist", got, day)
	}
}

func TestAZoneWithANonHourOffsetProjectsCorrectly(t *testing.T) {
	t.Parallel()
	// Asia/Kathmandu is UTC+05:45. A scan that assumed whole-hour offsets
	// — the obvious way to make the minute walk faster — reports the wrong
	// minute here and nowhere else, so this is the case that would catch it.
	loc := mustLoad(t, "Asia/Kathmandu")
	e := mustParse(t, "30 9 * * *")
	got, ok := e.Next(at(2026, time.June, 8, 0, 0), loc)
	if !ok {
		t.Fatal("Next = not found")
	}
	if want := at(2026, time.June, 8, 3, 45); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (09:30 at +05:45)", got, want)
	}
}

// --- helpers --------------------------------------------------------------

func requireMatch(t *testing.T, e schedule.Expr, local time.Time, want bool) {
	t.Helper()
	if got := e.Matches(local); got != want {
		t.Errorf("Matches(%s) = %v, want %v", local.Format(time.RFC3339), got, want)
	}
}

func requireTimes(t *testing.T, got []time.Time, want ...time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d fire times %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("fire time %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func requireLocalDays(t *testing.T, got []time.Time, loc *time.Location, want ...string) {
	t.Helper()
	seen := map[string]struct{}{}
	var days []string
	for _, ft := range got {
		d := ft.In(loc).Format("2006-01-02")
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		days = append(days, d)
	}
	if len(days) != len(want) {
		t.Fatalf("local days = %v, want %v", days, want)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Fatalf("local days = %v, want %v", days, want)
		}
	}
}
