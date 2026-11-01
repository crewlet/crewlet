package clocktest_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clocktest"
)

// A FORGED INSTANT COMPARES AS A HOST WHOSE CLOCK PARTED WOULD READ IT: on the
// monotonic reading against an instant that carries one, and on the wall
// reading against one that does not. That pair is what the code under test
// must tell apart, so it is what the forgery is held to.
func TestAForgedInstantComparesAsAPartedClockReadsIt(t *testing.T) {
	t.Parallel()
	at := time.Now()
	for name, tc := range map[string]struct{ monotonic, wall time.Duration }{
		"a host that slept an hour":          {0, time.Hour},
		"a wall clock stepped back a minute": {10 * time.Second, -50 * time.Second},
		"a clock that did not part":          {3 * time.Second, 3 * time.Second},
	} {
		forged := clocktest.Parted(t, at, tc.monotonic, tc.wall)
		if got := forged.Sub(at); got != tc.monotonic {
			t.Errorf("%s: %s on the monotonic clock, want %s", name, got, tc.monotonic)
		}
		if got := forged.Sub(at.Round(0)); got != tc.wall {
			t.Errorf("%s: %s against a wall-only instant, want %s", name, got, tc.wall)
		}
	}
	slept := clocktest.AfterSleep(t, at, 2*time.Hour)
	if slept.After(at) || !slept.Round(0).After(at.Round(0).Add(time.Hour)) {
		t.Errorf("a host that slept two hours reads %s after %s: want no time passed on "+
			"the monotonic clock and two hours on the wall", slept, at)
	}
}
