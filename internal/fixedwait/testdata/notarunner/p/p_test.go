package p

import (
	"testing"
	"testing/synctest"
	"time"
)

// either runs f in a bubble or on the machine's clock, so it is no runner.
func either(t *testing.T, bubble bool, f func(*testing.T)) {
	if bubble {
		synctest.Test(t, f)
		return
	}
	f(t)
}

func TestThroughEither(t *testing.T) {
	either(t, false, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}
