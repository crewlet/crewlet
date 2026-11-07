package p_test

import (
	"testing"
	"time"
)

// inBubble is the external test package's own, and runs f on the machine's
// clock.
func inBubble(t *testing.T, f func(*testing.T)) { f(t) }

func TestThroughTheExternalPackagesOwn(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}
