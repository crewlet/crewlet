package llm_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE SET IS CLOSED, and empty means no level was named. A backend maps these
// onto two vendors' parameters and takes the lower of two of them, so a value
// outside the set is one it can neither send nor compare.
func TestTheEffortSetIsClosedAndEmptyMeansUnnamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		effort llm.Effort
		valid  bool
	}{
		{"", true},
		{llm.EffortLow, true},
		{llm.EffortMedium, true},
		{llm.EffortHigh, true},
		{llm.EffortXHigh, true},
		{llm.EffortMax, true},
		{"x-high", false},  // The spelling a person reaches for.
		{"HIGH", false},    // Not case-folded: neither vendor's parameter is.
		{"minimal", false}, // OpenAI-only, and below what the contract orders.
		{"none", false},
	} {
		if got := tc.effort.Valid(); got != tc.valid {
			t.Errorf("Effort(%q).Valid() = %v, want %v", tc.effort, got, tc.valid)
		}
	}
}

// A CEILING ONLY EVER LOWERS. The caller knows what a call is for, the
// operator what the entry costs, and neither may raise the other: a `low`
// classifier on a `high` entry runs low, a `max` request on a `medium` entry
// stays medium, and an entry with no level is left alone, since lowering a
// level nobody named could raise it above the vendor's own default.
func TestAtMostTakesTheLowerAndNeverRaises(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		entry, ceiling, want llm.Effort
	}{
		{llm.EffortHigh, llm.EffortLow, llm.EffortLow},
		{llm.EffortMedium, llm.EffortMax, llm.EffortMedium},
		{llm.EffortXHigh, llm.EffortXHigh, llm.EffortXHigh},
		{llm.EffortMax, llm.EffortXHigh, llm.EffortXHigh},
		{llm.EffortXHigh, llm.EffortHigh, llm.EffortHigh},
		{llm.EffortLow, llm.EffortMedium, llm.EffortLow},
		{llm.EffortHigh, "", llm.EffortHigh},
		{"", llm.EffortLow, ""},
		{"", "", ""},
		{llm.EffortHigh, "bogus", llm.EffortHigh},
	} {
		if got := tc.entry.AtMost(tc.ceiling); got != tc.want {
			t.Errorf("Effort(%q).AtMost(%q) = %q, want %q", tc.entry, tc.ceiling, got, tc.want)
		}
	}
}
