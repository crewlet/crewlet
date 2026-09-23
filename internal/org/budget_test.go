package org

import (
	"testing"

	"github.com/crewlet/crewlet/internal/period"
)

// THE TIGHTEST CEILING IS THE SMALLEST, AND A SCOPE THAT CAPS NOTHING SAYS SO.
//
// A counter that knows no calendar is held to this one number, and it is safe
// only because it is the smallest: held to a larger one, a scope capped at
// 100 000 a day could spend a month's allowance in an afternoon. The "none"
// answer has to be distinguishable from a ceiling, because a scope with no
// entries is uncapped rather than capped at zero.
func TestTheTightestCeilingIsTheSmallestAndNoneIsUncapped(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ceilings TokenCeilings
		limit    int
		capped   bool
	}{
		"no budget at all":          {nil, 0, false},
		"an empty budget":           {TokenCeilings{}, 0, false},
		"one window":                {TokenCeilings{period.Week: 700}, 700, true},
		"the day under the month":   {TokenCeilings{period.Day: 100, period.Month: 3000}, 100, true},
		"a week under a larger day": {TokenCeilings{period.Day: 900, period.Week: 500}, 500, true},
		"every window":              {TokenCeilings{period.Day: 50, period.Week: 40, period.Month: 60}, 40, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			limit, capped := tc.ceilings.Tightest()
			if limit != tc.limit || capped != tc.capped {
				t.Errorf("Tightest() = (%d, %v), want (%d, %v)", limit, capped, tc.limit, tc.capped)
			}
		})
	}
}
