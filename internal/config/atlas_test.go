package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// AGENTS RECORD IN THE ATLAS UNLESS THE COMPANY TURNS IT OFF: unset is on,
// and only an explicit false stops it.
//
// Mutation: read the pointer's zero as off and a company that says nothing
// stops its agents recording.
func TestAgentsRecordInTheAtlasUnlessTurnedOff(t *testing.T) {
	t.Parallel()
	on, off := true, false
	for _, tc := range []struct {
		name string
		a    Atlas
		want bool
	}{{"unset", Atlas{}, true}, {"true", Atlas{AgentRecords: &on}, true}, {"false", Atlas{AgentRecords: &off}, false}} {
		if got := tc.a.AgentsRecord(); got != tc.want {
			t.Errorf("agent_records %s: agents record = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// AN ENTRY'S OWNING AGENT IS WOKEN WHEN IT DRIFTS UNLESS THE COMPANY TURNS IT
// OFF: unset is on, and only an explicit false stops it.
//
// Mutation: read the pointer's zero as off and a company that says nothing
// wakes nobody.
func TestOwnersAreWokenUnlessTurnedOff(t *testing.T) {
	t.Parallel()
	on, off := true, false
	for _, tc := range []struct {
		name string
		a    Atlas
		want bool
	}{{"unset", Atlas{}, true}, {"true", Atlas{OwnerWakes: &on}, true}, {"false", Atlas{OwnerWakes: &off}, false}} {
		if got := tc.a.WakesOwners(); got != tc.want {
			t.Errorf("owner_wakes %s: owners are woken = %v, want %v", tc.name, got, tc.want)
		}
	}
}

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

// THE ATLAS IS REFRESHED HOURLY UNLESS THE COMPANY SAYS OTHERWISE, AND NEVER
// MORE OFTEN THAN EVERY FIVE MINUTES: a value below the floor is refused
// naming the field, never clamped.
//
// Mutation: drop the floor check in Atlas.validate and 299 seconds is accepted.
func TestTheAtlasIsRefreshedHourlyAndNeverUnderFiveMinutes(t *testing.T) {
	t.Parallel()
	if got := (Atlas{}).RefreshInterval(); got != time.Hour {
		t.Errorf("an unset interval refreshes every %s, want an hour", got)
	}
	for _, tc := range []struct {
		secs int
		ok   bool
	}{{0, true}, {299, false}, {300, true}, {3600, true}, {86400, true}} {
		err := Atlas{RefreshIntervalSeconds: tc.secs}.validate(field("atlas"))
		if (err == nil) != tc.ok {
			t.Errorf("refresh_interval_seconds %d: %v, want accepted %v", tc.secs, err, tc.ok)
		}
		if err != nil && (!errors.Is(err, ErrOutOfRange) || !strings.Contains(err.Error(), "refresh_interval_seconds")) {
			t.Errorf("refresh_interval_seconds %d is refused as %v, want ErrOutOfRange naming the field", tc.secs, err)
		}
		if tc.ok && tc.secs != 0 && (Atlas{RefreshIntervalSeconds: tc.secs}).RefreshInterval() != time.Duration(tc.secs)*time.Second {
			t.Errorf("refresh_interval_seconds %d refreshes every %s", tc.secs, (Atlas{RefreshIntervalSeconds: tc.secs}).RefreshInterval())
		}
	}
}
