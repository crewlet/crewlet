package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE APPLY LAG IS THE BACKLOG OVER THE DRAIN AS MEASURED, fraction and all.
//
// The drain is a float — records a second, smoothed across runs — and a rate
// truncated to a whole number before the division overstates the lag by up to
// twice for a drain between one and two. Both the gauge an operator reads and
// the reading the `apply_lag` alarm fires on are this function's answer.
//
// Mutation: truncate the drain to a whole number before dividing and the
// fractional cases read 3 s, 7 s and 2.5 s; drop the floor and the unmeasured
// drain divides by zero while the one below the floor reads 6 s.
func TestTheApplyLagIsTheBacklogOverTheMeasuredDrain(t *testing.T) {
	t.Parallel()
	lag := func(records uint64) *uint64 { return &records }
	for _, c := range []struct {
		name  string
		lag   *uint64
		drain float64
		want  time.Duration
	}{
		{name: "a fractional drain between one and two", lag: lag(3), drain: 1.5,
			want: 2 * time.Second},
		{name: "another, closer to two", lag: lag(7), drain: 1.75,
			want: 4 * time.Second},
		{name: "a fractional drain above two", lag: lag(5), drain: 2.5,
			want: 2 * time.Second},
		{name: "a whole drain", lag: lag(1000), drain: 250,
			want: 4 * time.Second},
		// ZERO IS UNMEASURED, and it reads at the floor rather than
		// dividing by zero.
		{name: "an unmeasured drain", lag: lag(3), drain: 0,
			want: 3 * time.Second},
		// THE FLOOR IS A FLOOR FOR A MEASURED RATE TOO — see
		// [statelog.DrainFloor] for what that costs.
		{name: "a measured drain below the floor", lag: lag(3), drain: 0.5,
			want: 3 * time.Second},
		{name: "caught up", lag: lag(0), drain: 1.5, want: 0},
		// NO LAG READING IS NOT A LAG OF ZERO to the gauge, which skips
		// it; here it is simply nothing to convert.
		{name: "no lag reading", lag: nil, drain: 1.5, want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := applyLagOf(statelog.Health{Lag: c.lag}, c.drain)
			if got != c.want {
				t.Errorf("a backlog of %v records at %v records a second reads "+
					"as %v, want %v", lagOrNone(c.lag), c.drain, got, c.want)
			}
		})
	}
}

// lagOrNone renders an optional lag for a failure message.
func lagOrNone(lag *uint64) any {
	if lag == nil {
		return "no"
	}
	return *lag
}
