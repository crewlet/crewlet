package worklet_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A Wednesday, deliberately mid-week and mid-month, so every boundary token
// has somewhere to move to in both directions.
var (
	berlin, _ = time.LoadLocation("Europe/Berlin")
	wednesday = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
)

func at(t *testing.T, token string) time.Time {
	t.Helper()
	got, err := period.Resolve(token, wednesday, berlin)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", token, err)
	}
	return got.At
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
		got, ok := worklet.DateAlias(alias)
		if !ok || got != want {
			t.Errorf("%s expands to %q (%v), want %q", alias, got, ok, want)
		}
		expanded, err := worklet.ParseDateFilter(alias, wednesday, berlin)
		if err != nil {
			t.Fatalf("ParseDateFilter(%q): %v", alias, err)
		}
		direct, err := worklet.ParseDateFilter(want, wednesday, berlin)
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
	got, err := worklet.ParseDateFilter("overdue", wednesday, berlin)
	if err != nil {
		t.Fatalf("ParseDateFilter: %v", err)
	}
	if !got.Overdue {
		t.Fatal("overdue resolved to a plain date filter, so a finished task " +
			"whose due date has passed would be in the answer")
	}
	if got.Op != worklet.DateLT || !got.From.At.Equal(at(t, "today")) {
		t.Fatalf("overdue is %+v, want everything before today", got)
	}
}

// A RANGE THAT ENDS BEFORE IT STARTS IS REFUSED.
//
// It matches nothing, and a filter that matches nothing is indistinguishable
// from a company with no work in it.
func TestABackwardsRangeIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := worklet.ParseDateFilter("range:eow..sow", wednesday, berlin); err == nil {
		t.Fatal("a backwards range was accepted")
	}
	if _, err := worklet.ParseDateFilter("range:sow..sow", wednesday, berlin); err != nil {
		t.Fatalf("an empty-but-forward range was refused: %v", err)
	}
}

// A COMPARISON WITHOUT AN OPERATOR IS REFUSED NAMING THE FIVE.
func TestADateFilterNeedsAComparison(t *testing.T) {
	t.Parallel()
	_, err := worklet.ParseDateFilter("today", wednesday, berlin)
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
