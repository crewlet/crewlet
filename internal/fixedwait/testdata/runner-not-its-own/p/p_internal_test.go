package p

import (
	"testing"
	"testing/synctest"
	"time"
)

// inBubble hands its function whole to synctest, so a literal handed to it
// runs in a bubble.
func inBubble(t *testing.T, f func(*testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}

func TestThroughTheRunner(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}

func TestThroughALocalOfTheRunnersName(t *testing.T) {
	inBubble := func(t *testing.T, f func(*testing.T)) { f(t) }
	inBubble(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}

type fakeSync struct{}

func (fakeSync) Test(t *testing.T, f func(*testing.T)) { f(t) }

func TestThroughALocalOfSynctestsName(t *testing.T) {
	synctest := fakeSync{}
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Second)
	})
}
