package queue

import (
	"context"
	"sync"
	"time"
)

// Inflight counts running handlers and lets a drain wait for them to finish.
// The zero value is ready to use.
//
// Not a sync.WaitGroup: its Add at zero must happen before Wait, but a
// dispatch loop adds whenever a message arrives, including while a drain
// waits, so Wait could return on a momentary zero and shut down through a
// starting handler (the race detector flags it). A count under a mutex with a
// channel closed at zero has no such ordering. One implementation serves every
// backend: the conformance suite could not tell a wrong copy from a right one.
type Inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// Begin records a handler starting. Safe at any time, including while a
// drain is waiting.
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
		// A stray End cannot go negative: an operator sees the count, and
		// a negative one would blame the queue for a double finish.
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
// and reports how many were still running when it stopped waiting. Zero is a
// clean drain; non-zero is no error (the caller owns that policy). A
// non-positive timeout waits until the handlers finish or ctx ends.
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
