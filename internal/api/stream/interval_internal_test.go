package stream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// AN UNSET INTERVAL TICKS AT THE PRODUCTION CADENCE.
//
// Every production caller leaves [Options.HealthInterval] unset — it exists so
// a suite need not wait out five seconds per tick — so the branch that turns
// zero into [HealthInterval] is what decides how often every open tab hears the
// engine's health, and the dashboard's staleness rule is sized against exactly
// that cadence. The suite's tick cases all inject an interval, so this is the
// one case that reads the default.
func TestAnUnsetIntervalTicksAtTheProductionCadence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset", 0, HealthInterval},
		{"negative", -time.Second, HealthInterval},
		{"injected", 20 * time.Millisecond, 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewService(livestate.New(), Options{
				Health:         func() any { return nil },
				Handles:        func() map[string]string { return nil },
				Roster:         func() []map[string]any { return nil },
				Org:            func() any { return nil },
				Tools:          func() []map[string]any { return nil },
				Schedules:      func() any { return nil },
				Placement:      func() (map[string]bool, error) { return nil, nil },
				HealthInterval: tc.set,
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if s.interval != tc.want {
				t.Errorf("an interval of %s ticks every %s, want %s", tc.set, s.interval, tc.want)
			}
		})
	}
}
