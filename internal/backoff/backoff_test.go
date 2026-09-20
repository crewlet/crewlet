package backoff_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
)

// THE FIRST ATTEMPT WAITS THE BASE, and each after it twice the last, until
// the ceiling holds it.
func TestADoublingDelayReachesItsCeilingAndStays(t *testing.T) {
	t.Parallel()
	for attempt, want := range map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 8 * time.Second,
		9: 8 * time.Second,
	} {
		if got := backoff.Doubling(attempt, time.Second, 8*time.Second); got != want {
			t.Errorf("attempt %d waits %v, want %v", attempt, got, want)
		}
	}
}

// A dependency that stays down accumulates attempts, and a count read off the
// wire is whatever a peer sent. The closed form overflows well before either
// becomes unreasonable, and an overflowed duration is NEGATIVE, which every
// caller reads as already due.
func TestADoublingDelayNeverGoesNegative(t *testing.T) {
	t.Parallel()
	for _, attempt := range []int{1, 32, 62, 63, 64, 1000, 1 << 20, 1 << 40} {
		got := backoff.Doubling(attempt, 30*time.Second, 5*time.Minute)
		if got <= 0 || got > 5*time.Minute {
			t.Fatalf("attempt %d produced %v, want a wait in (0, 5m]", attempt, got)
		}
	}
}

// Zero and negative counts are the first attempt, not an instant retry.
func TestNoAttemptsYetIsTheFirstAttempt(t *testing.T) {
	t.Parallel()
	for _, attempt := range []int{-1, 0, 1} {
		if got := backoff.Doubling(attempt, time.Second, time.Hour); got != time.Second {
			t.Fatalf("attempt %d waits %v, want the base", attempt, got)
		}
	}
}

// A NONSENSE BASE IS THE LONG WAIT. A ceiling below the base is the ceiling
// rather than a delay that ignores it, and a zero base doubles to zero for ever,
// which would spin any loop that trusted it.
func TestANonsenseBaseIsTheCeiling(t *testing.T) {
	t.Parallel()
	for _, base := range []time.Duration{-time.Second, 0, time.Minute} {
		if got := backoff.Doubling(3, base, time.Second); got != time.Second {
			t.Errorf("base %v waits %v, want the ceiling", base, got)
		}
	}
}

// A JITTERED INTERVAL STAYS INSIDE ITS BAND, and it actually moves: a spread
// that always returned the interval would pass the band check and leave every
// node of a fleet waking on the same second.
func TestAJitteredIntervalStaysInItsBandAndMoves(t *testing.T) {
	t.Parallel()
	const interval = 10 * time.Minute
	lo, hi := interval-2*time.Minute, interval+2*time.Minute
	seen := map[time.Duration]bool{}
	for range 200 {
		got := backoff.Jitter(interval, 0.2)
		if got < lo || got > hi {
			t.Fatalf("Jitter = %v, want within [%v, %v]", got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("200 jittered intervals were all identical")
	}
}

// A spread of the whole interval or more could return a wait of zero or less,
// which reads as already due. The widest spread still leaves a positive wait.
func TestAWideSpreadNeverReachesZero(t *testing.T) {
	t.Parallel()
	for _, fraction := range []float64{1, 5} {
		for range 500 {
			if got := backoff.Jitter(time.Second, fraction); got <= 0 {
				t.Fatalf("Jitter(1s, %v) = %v", fraction, got)
			}
		}
	}
	if got := backoff.Jitter(time.Second, -1); got != time.Second {
		t.Errorf("a negative spread moved the interval to %v", got)
	}
	if got := backoff.Jitter(0, 0.2); got != 0 {
		t.Errorf("a zero interval jittered to %v", got)
	}
}
