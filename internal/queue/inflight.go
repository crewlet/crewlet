package queue

import (
	"context"
	"sync"
	"time"
)

// Inflight counts running handlers and lets a drain wait for them to finish.
//
// # Why this is not a sync.WaitGroup
//
// Two of the three backends used one, and it was wrong in a way that only
// shows up under load. A WaitGroup's contract is explicit: "calls with a
// positive delta that start when the counter is zero must happen before a
// Wait". A dispatch loop calls Add the moment a message arrives — which is
// any moment at all, including one where a drain is already waiting on an
// empty queue. There is no way for a consumer to honour that ordering
// without the coordination the WaitGroup was supposed to provide.
//
// It is not only a rule on paper. Wait may return on a momentary zero while
// a handler is starting, so a drain reports clean and the process shuts down
// through a running handler — the exact failure WaitForHandlers exists to
// prevent. The race detector reports it as a data race on the WaitGroup
// itself, and that is how it was found: a JetStream dispatch against a
// concurrent drain, in a suite that starts and stops nodes in parallel.
//
// The memory twin never had it — it kept a count under a mutex with a
// channel closed on the transition to zero, which is what this is. That the
// twin was right and both real backends were wrong is the case for one
// implementation rather than three: the certified suite exercised the
// CONTRACT identically for all of them and could not see the difference.
//
// The zero value is ready to use.
type Inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// Begin records a handler starting. Safe at any time, including while a
// drain is waiting, which is the whole point.
func (f *Inflight) Begin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

// End records one finishing, waking every waiter when the last one goes.
func (f *Inflight) End() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		// A stray End cannot take the count negative: the count is also
		// what an operator sees, and a negative one reads as a bug in the
		// queue rather than in whoever double-finished a handler.
		return
	}
	f.n--
	if f.n == 0 && f.idle != nil {
		close(f.idle)
		f.idle = nil
	}
}

// Count reports how many handlers are running.
func (f *Inflight) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// Wait blocks until no handler is running, the timeout expires, or ctx ends,
// and reports how many were still running when it stopped waiting.
//
// Zero is a clean drain. A non-zero count is not an error — the caller owns
// any "this took too long" policy — and a non-positive timeout waits until
// the handlers finish or ctx ends.
func (f *Inflight) Wait(ctx context.Context, timeout time.Duration) (int, error) {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return 0, nil
	}
	idle := f.idle
	f.mu.Unlock()

	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-idle:
		return f.Count(), nil
	case <-expired:
		return f.Count(), nil
	case <-ctx.Done():
		return f.Count(), ctx.Err()
	}
}
