package config

import (
	"errors"
	"testing"
	"time"
)

// THE ATLAS'S HISTORY IS KEPT BETWEEN A QUARTER OF A YEAR AND TEN YEARS, a year
// and a month when the company says nothing, as the access history's is.
//
// Mutation: drop the bound check in Atlas.validate and 89 days is accepted.
func TestTheAtlasHistoryIsKeptBetweenAQuarterAndTenYears(t *testing.T) {
	t.Parallel()
	if got := (Atlas{}).Retention(); got != 400*24*time.Hour {
		t.Errorf("an unset retention keeps %s, want 400 days", got)
	}
	for _, tc := range []struct {
		days int
		ok   bool
	}{{0, true}, {89, false}, {90, true}, {400, true}, {3650, true}, {3651, false}} {
		err := Atlas{RetentionDays: tc.days}.validate(field("atlas"))
		if (err == nil) != tc.ok {
			t.Errorf("retention_days %d: %v, want accepted %v", tc.days, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrOutOfRange) {
			t.Errorf("retention_days %d is refused as %v, want ErrOutOfRange", tc.days, err)
		}
		if tc.ok && tc.days != 0 && (Atlas{RetentionDays: tc.days}).Days() != tc.days {
			t.Errorf("retention_days %d keeps %d days", tc.days, (Atlas{RetentionDays: tc.days}).Days())
		}
	}
}
