package statelog

import (
	"container/heap"
	"context"
	"errors"
	"sync"
)

// ErrWaitAbandoned reports a wait the applier ended without reaching the
// position: the loop stopped, or it is being restarted over a replaced file.
// An error rather than a release, which would send the caller to read rows
// below its position at the level it asked for; every caller maps the error
// to `behind`.
var ErrWaitAbandoned = errors.New("statelog: the applier stopped while this " +
	"caller was waiting on it")

// waiters is the set of callers blocked on this node's applier reaching a
// position, ordered BY POSITION so a move of the reached position wakes
// exactly the ones it satisfied.
//
// A min-heap with a channel per waiter, not a sync.Cond: the reached position
// moves many times a second (every decoded barrier and every commit,
// [Runner.Reached]) under many concurrent waiters, a Broadcast would wake all
// of them to re-test unreached targets, and turning a context deadline into a
// wake would need a watcher goroutine per wait. The heap wakes each waiter
// once, by the move that satisfied it, and its length is the waiting gauge.
type waiters struct {
	mu    sync.Mutex
	items waiterHeap
	next  uint64
}

// waiter is one blocked caller.
type waiter struct {
	// target is the position this caller is waiting for.
	target Position

	// done is closed by the move that reaches target; a channel so the
	// caller selects on it against its own context, with no goroutine.
	done chan struct{}

	// abandoned is closed by an applier that ends without reaching
	// target: separate from done, which is an answer where this is the
	// absence of one.
	abandoned chan struct{}

	// seq breaks ties in arrival order: not needed for correctness, but a
	// total order keeps the tests deterministic.
	seq uint64

	// index is the heap's own bookkeeping, -1 once out of the heap.
	index int
}

// wait registers a waiter for target and returns its channels plus a cancel
// that removes it, so a caller whose context expires does not stay in the
// heap, and the gauge, until the applier passes its target.
func (w *waiters) wait(target Position) (done, abandoned <-chan struct{}, cancel func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item := &waiter{
		target: target, done: make(chan struct{}),
		abandoned: make(chan struct{}), seq: w.next,
	}
	w.next++
	heap.Push(&w.items, item)
	return item.done, item.abandoned, func() { w.drop(item) }
}

// drop removes a waiter that gave up, if the applier has not already released
// it.
func (w *waiters) drop(item *waiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if item.index < 0 {
		return
	}
	heap.Remove(&w.items, item.index)
	item.index = -1
}

// release wakes every waiter at or below reached ([Runner.Reached]).
//
// For a record that writes a row it runs AFTER the transaction commits, never
// inside it: the store may re-run a body, so a wake from inside can announce a
// position that was rolled back. A barrier is released when decoded
// ([Runner.reach]): it writes no row, and its later commit changes nothing its
// waiters read.
func (w *waiters) release(reached Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.items.Len() > 0 {
		top := w.items[0]
		if reached.Packed() < top.target.Packed() {
			return
		}
		heap.Pop(&w.items)
		top.index = -1
		close(top.done)
	}
}

// abandonAll tells every waiter the applier will not reach it, for a loop
// that is ending: left blocked, it would wait out its whole budget; released,
// it would read rows below its position.
func (w *waiters) abandonAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.items.Len() > 0 {
		top := heap.Pop(&w.items).(*waiter)
		top.index = -1
		close(top.abandoned)
	}
}

// len is how many callers are waiting: the gauge an operator reads.
func (w *waiters) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.items.Len()
}

// minimum is the lowest position anybody is waiting for, and false when
// nobody is. The applier's batch linger reads it to end at once a batch that
// holds a record somebody waits for.
func (w *waiters) minimum() (Position, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.items.Len() == 0 {
		return Position{}, false
	}
	return w.items[0].target, true
}

// waiterHeap orders waiters by target, earliest first.
type waiterHeap []*waiter

func (h waiterHeap) Len() int { return len(h) }

func (h waiterHeap) Less(i, j int) bool {
	if a, b := h[i].target.Packed(), h[j].target.Packed(); a != b {
		return a < b
	}
	return h[i].seq < h[j].seq
}

func (h waiterHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *waiterHeap) Push(x any) {
	item := x.(*waiter)
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *waiterHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// awaitPosition blocks until the applier reaches target, the context ends, or
// the applier stops.
func (w *waiters) awaitPosition(ctx context.Context, target Position, reached func() Position) error {
	if reached().Packed() >= target.Packed() {
		return nil
	}
	done, abandoned, cancel := w.wait(target)
	defer cancel()
	// RE-CHECK AFTER REGISTERING: a move between the check above and the
	// registration released a heap without this waiter, and no later move
	// is guaranteed.
	if reached().Packed() >= target.Packed() {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-abandoned:
		return ErrWaitAbandoned
	case <-ctx.Done():
		return ctx.Err()
	}
}
