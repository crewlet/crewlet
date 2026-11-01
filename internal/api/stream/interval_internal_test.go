package stream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tokens"
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
			opts := quietOptions()
			opts.HealthInterval = tc.set
			s, err := NewService(livestate.New(), opts)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if s.interval != tc.want {
				t.Errorf("an interval of %s ticks every %s, want %s", tc.set, s.interval, tc.want)
			}
		})
	}
}

// quietOptions are the surface functions every case here needs and none reads:
// a node answering nothing about its company, on no chart and no directory.
// Each case sets the one option its subject is.
func quietOptions() Options {
	return Options{
		Health:    func() Health { return nodeHealth{Status: "ok"} },
		Posture:   func(Health) FramePosture { return FrameLive },
		Seats:     func() tokens.Seats { return tokens.Seats{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return nil },
		Tools:     func() any { return nil },
		Schedules: func() any { return nil },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Chart:     authz.NoChart{},
		Holders:   blindHolders{},
		SeatOf:    func(string) (SeatState, bool) { return SeatState{}, false },
	}
}
