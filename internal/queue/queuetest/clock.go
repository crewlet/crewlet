package queuetest

import (
	"slices"
	"sync"
	"time"
)

// Clock is a clock a case moves by hand: the one a queue built by
// [Capabilities.LingerClock] ends its batch linger windows on. The zero value
// is a clock at its start.
//
// IT STANDS STILL UNTIL IT IS MOVED. A window opened on it ends once the case
// has moved it past the window's end ([Clock.Advance]) and never before,
// however long the case takes to get there: so a case lands a verb inside the
// window and only then ends it, an order the wall clock never promised, since a
// loaded runner could reach the verb after the window had ended by itself.
//
// Its instants are its own, counted from a fixed start, and never a reading of
// the wall clock: two of them are exactly as far apart as the moves between
// them.
type Clock struct {
	mu sync.Mutex
	// moved is how far the clock has been moved in all.
	moved time.Duration
	// timers are the timers set on it that have neither fired nor been
	// stopped.
	timers []*clockTimer
}

// clockTimer is a call set on a [Clock], due once the clock has been moved to
// at.
type clockTimer struct {
	at time.Duration
	f  func()
}

// clockStart is the instant a [Clock] reads before it is moved. Any fixed
// instant would do; one that is plainly not now keeps a reading of it from
// passing for the wall clock's.
var clockStart = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// Now is the clock's instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clockStart.Add(c.moved)
}

// AfterFunc calls f on a goroutine of its own, as time.AfterFunc does, once the
// clock has been moved d past where it stands now, unless the stop it returns
// is called first; stop reports whether it stopped the call. A d at or below
// zero is due at once.
func (c *Clock) AfterFunc(d time.Duration, f func()) (stop func() bool) {
	if d <= 0 {
		go f()
		return func() bool { return false }
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &clockTimer{at: c.moved + d, f: f}
	c.timers = append(c.timers, timer)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		i := slices.Index(c.timers, timer)
		if i < 0 {
			return false
		}
		c.timers = slices.Delete(c.timers, i, i+1)
		return true
	}
}

// Advance moves the clock on by d and starts every call that comes due, each on
// a goroutine of its own. It returns once they have been started, not once they
// have run: what a timer did is read off the queue, as a real timer's is.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.moved += d
	var due []*clockTimer
	c.timers = slices.DeleteFunc(c.timers, func(timer *clockTimer) bool {
		if timer.at > c.moved {
			return false
		}
		due = append(due, timer)
		return true
	})
	c.mu.Unlock()
	for _, timer := range due {
		go timer.f()
	}
}
