package p

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestInABubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Second)
		<-time.After(time.Second)
	})
}

func TestInARun(t *testing.T) {
	synctest.Run(func() {
		time.Sleep(time.Second)
	})
}

// inBubble hands its function whole to synctest, so a literal handed to it
// runs in a bubble.
func inBubble(t *testing.T, f func(*testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}

func TestThroughARunner(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}
