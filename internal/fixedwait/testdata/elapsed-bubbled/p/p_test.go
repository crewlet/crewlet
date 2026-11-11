package p

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestInABubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		time.Sleep(time.Second)
		if time.Since(start) != time.Second || time.Now().Sub(start) != time.Second {
			t.Fatal("the bubble's clock is exact")
		}
		_ = time.Until(start)
		_ = sinceIn(start)
		_ = subIn(start)
		deadline := start.Add(2 * time.Second)
		if !time.Now().Before(deadline) || time.Now().After(deadline) || time.Now().Compare(deadline) >= 0 {
			t.Fatal("the bubble's clock is exact")
		}
		_ = beforeIn(deadline)
	})
}

// sinceIn reads the clock, and every caller runs it in a bubble.
//
//fixedwait:bubble
func sinceIn(start time.Time) time.Duration {
	return time.Since(start)
}

// subIn reads the clock through a Sub alone, and every caller runs it in a
// bubble: the declaration holds that reading, so it is not stale.
//
//fixedwait:bubble
func subIn(start time.Time) time.Duration {
	return start.Add(time.Second).Sub(start)
}

// beforeIn compares the clock with an instant through a Before alone, and
// every caller runs it in a bubble: the declaration holds that reading, so
// it is not stale.
//
//fixedwait:bubble
func beforeIn(deadline time.Time) bool {
	return time.Now().Before(deadline)
}
