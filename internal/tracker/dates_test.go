package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A Wednesday, deliberately mid-week and mid-month, so every boundary token
// has somewhere to move to in both directions.
var (
	berlin, _ = time.LoadLocation("Europe/Berlin")
	wednesday = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
)

func at(t *testing.T, token string) time.Time {
	t.Helper()
	got, err := tracker.ResolveDate(token, wednesday, berlin)
	if err != nil {
		t.Fatalf("ResolveDate(%q): %v", token, err)
	}
	return got.At
}

// EVERY CALENDAR TOKEN RESOLVES IN THE COMPANY'S OWN TIMEZONE.
//
// Midnight in Berlin is 22:00 UTC the day before. A resolver that worked in
// UTC would put "today" two hours late for a European company and a whole day
// late for a Pacific one — and the symptom is a board that shows yesterday's
// overdue tasks as due today.
func TestADayBoundaryIsTheCompanysMidnight(t *testing.T) {
	t.Parallel()
	today := at(t, "today")
	if got := today.In(berlin); got.Hour() != 0 || got.Minute() != 0 {
		t.Fatalf("today resolves to %v in the company's own timezone", got)
	}
	if !today.Equal(time.Date(2031, 4, 15, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("today is %v; Berlin midnight on 2031-04-16 is 22:00 UTC on "+
			"the 15th", today)
	}
}

// EVERY END TOKEN IS THE START OF THE NEXT PERIOD.
//
// A range is half-open, so "this week" contains every instant of Sunday. An
// end at 23:59:59 silently drops the last second of the last day — and drops
// it differently depending on whether the column stores seconds or
// milliseconds.
func TestEveryEndTokenIsTheNextPeriodsStart(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{"eow", "sonw"},
		{"eopw", "sow"},
		{"eom", "sonm"},
		{"eopm", "som"},
	} {
		if got, want := at(t, pair[0]), at(t, pair[1]); !got.Equal(want) {
			t.Errorf("%s is %v and %s is %v; a half-open range needs them equal",
				pair[0], got, pair[1], want)
		}
	}
}

// THE WEEK STARTS ON MONDAY, AND THE PERIODS ARE THE RIGHT LENGTH.
func TestTheCalendarPeriodsAreWholeAndAdjacent(t *testing.T) {
	t.Parallel()
	sow := at(t, "sow")
	if got := sow.In(berlin).Weekday(); got != time.Monday {
		t.Fatalf("the week starts on %v", got)
	}
	if got := at(t, "eow").Sub(sow); got != 7*24*time.Hour {
		t.Errorf("this week is %v long", got)
	}
	if got := sow.Sub(at(t, "sopw")); got != 7*24*time.Hour {
		t.Errorf("last week ends %v before this one starts", got)
	}
	som := at(t, "som")
	if got := som.In(berlin).Day(); got != 1 {
		t.Fatalf("the month starts on day %d", got)
	}
	if got := at(t, "eom").In(berlin); got.Day() != 1 || got.Month() != time.May {
		t.Errorf("this month ends at %v, not the first of the next", got)
	}
}

// AN OFFSET IS IN WHOLE DAYS FROM THE COMPANY'S MIDNIGHT.
func TestAnOffsetMovesWholeDaysFromMidnight(t *testing.T) {
	t.Parallel()
	today := at(t, "today")
	for token, want := range map[string]int{
		"+7d": 7, "7d": 7, "-7d": -7, "+0d": 0,
	} {
		if got := at(t, token); !got.Equal(today.AddDate(0, 0, want)) {
			t.Errorf("%s resolves to %v, want %d day(s) from %v",
				token, got, want, today)
		}
	}
}

// AN UNKNOWN TOKEN IS AN ERROR, NOT A ZERO INSTANT.
//
// The zero time is 1 January year one, so a filter that silently resolved to
// it would match every task ever written — which reads as a filter that did
// not apply rather than as one that failed.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "soon", "next-week", "d", "+d", "2031-13-40"} {
		if got, err := tracker.ResolveDate(token, wednesday, berlin); err == nil {
			t.Errorf("%q resolved to %v", token, got.At)
		}
	}
}

// THE VENDOR'S ALIASES EXPAND INTO THIS GRAMMAR RATHER THAN BESIDE IT.
//
// An alias implemented separately drifts from the token it means the first
// time either is tuned. `overdue` is the sharpest case: it is also a preset,
// and the two compile to the same predicate precisely because one expands into
// the other.
func TestTheVendorAliasesExpandRatherThanDuplicate(t *testing.T) {
	t.Parallel()
	for alias, want := range map[string]string{
		"next7":     "range:today..+7d",
		"last7":     "range:-7d..today",
		"thisweek":  "range:sow..eow",
		"thismonth": "range:som..eom",
		"lastmonth": "range:sopm..eopm",
		"earlier":   "lt:today",
	} {
		got, ok := tracker.DateAlias(alias)
		if !ok || got != want {
			t.Errorf("%s expands to %q (%v), want %q", alias, got, ok, want)
		}
		expanded, err := tracker.ParseDateFilter(alias, wednesday, berlin)
		if err != nil {
			t.Fatalf("ParseDateFilter(%q): %v", alias, err)
		}
		direct, err := tracker.ParseDateFilter(want, wednesday, berlin)
		if err != nil {
			t.Fatalf("ParseDateFilter(%q): %v", want, err)
		}
		if expanded != direct {
			t.Errorf("%s and %s resolve differently:\n  %+v\n  %+v",
				alias, want, expanded, direct)
		}
	}
}

// `overdue` IS NOT PURELY A DATE, AND THE FILTER SAYS SO.
//
// It carries an open-status condition, and the preset that means the same
// thing compiles to exactly this — so a caller cannot get one answer from the
// alias and another from the preset.
func TestOverdueCarriesItsOpenCondition(t *testing.T) {
	t.Parallel()
	got, err := tracker.ParseDateFilter("overdue", wednesday, berlin)
	if err != nil {
		t.Fatalf("ParseDateFilter: %v", err)
	}
	if !got.Overdue {
		t.Fatal("overdue resolved to a plain date filter, so a finished task " +
			"whose due date has passed would be in the answer")
	}
	if got.Op != tracker.DateLT || !got.From.At.Equal(at(t, "today")) {
		t.Fatalf("overdue is %+v, want everything before today", got)
	}
}

// A RANGE THAT ENDS BEFORE IT STARTS IS REFUSED.
//
// It matches nothing, and a filter that matches nothing is indistinguishable
// from a company with no work in it.
func TestABackwardsRangeIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := tracker.ParseDateFilter("range:eow..sow", wednesday, berlin); err == nil {
		t.Fatal("a backwards range was accepted")
	}
	if _, err := tracker.ParseDateFilter("range:sow..sow", wednesday, berlin); err != nil {
		t.Fatalf("an empty-but-forward range was refused: %v", err)
	}
}

// A COMPARISON WITHOUT AN OPERATOR IS REFUSED NAMING THE FIVE.
func TestADateFilterNeedsAComparison(t *testing.T) {
	t.Parallel()
	_, err := tracker.ParseDateFilter("today", wednesday, berlin)
	if err == nil {
		t.Fatal("a bare date was accepted as a filter")
	}
	if got := err.Error(); !contains5(got) {
		t.Fatalf("the refusal is %q and does not name the comparisons", got)
	}
}

func contains5(msg string) bool {
	for _, op := range []string{"lt:", "lte:", "gt:", "gte:", "range:"} {
		if !strings.Contains(msg, op) {
			return false
		}
	}
	return true
}
