package cliagent

import (
	"testing"
	"time"
)

func expired(s *stretch) bool {
	select {
	case <-s.expired():
		return true
	default:
		return false
	}
}

// THE CLOCK BOUNDS A STRETCH, NOT THE RUN. Paused, it never runs out, however
// long the pause; resumed, the stretch starts again with the whole limit — the
// model's next answer is a new one — and a stretch that runs its limit out
// with nothing paused is what ends the run.
//
// Mutation: resume a stretch with what was left of the last, or let a timer
// armed before a pause fire after it, and the first two checks are red.
func TestTheClockBoundsAStretchAndNotTheRun(t *testing.T) {
	t.Parallel()
	limit := 150 * time.Millisecond
	s := newStretch(limit)
	defer s.stop()
	s.start()
	time.Sleep(limit * 2 / 3)
	s.pause()
	time.Sleep(limit * 2)
	if expired(s) {
		t.Fatal("the clock ran out while paused")
	}
	s.resume()
	time.Sleep(limit * 2 / 3)
	if expired(s) {
		t.Fatal("a resumed stretch ran out early: it was charged the time before the pause")
	}
	time.Sleep(limit)
	if !expired(s) {
		t.Fatal("a stretch past its limit with nothing paused did not run out")
	}
}

// A retired clock never runs out: the run it bounded is over.
func TestARetiredClockNeverRunsOut(t *testing.T) {
	t.Parallel()
	limit := 50 * time.Millisecond
	s := newStretch(limit)
	s.start()
	s.stop()
	s.resume()
	time.Sleep(limit * 3)
	if expired(s) {
		t.Error("a clock stopped for good ran out")
	}
}
