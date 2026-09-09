package statelog

import (
	"container/heap"
	"context"
	"sync"
)

// waiters is the set of callers blocked on this node's applier reaching a
// position, ordered BY POSITION so a commit wakes exactly the ones it
// satisfied.
//
// # Why a heap and a channel each, rather than a condition variable
//
// The obvious shape — and the one this tree's projection package uses — is a
// sync.Cond with a Broadcast on every commit and a watcher goroutine per wait
// to turn a context deadline into a wake. At a fraction of a read per second
// that costs nothing.
//
// It stops being free at the rate this framework creates: a linearizable read
// appends a barrier and the applier yields its batch to serve it, so commits
// become roughly one-to-one with reads, and every commit then wakes EVERY
// waiter to re-test a target most of them have not reached. The goroutine per
// wait is the other half — a goroutine whose only job is to watch somebody
// else's deadline is the shape this engine's concurrency rule exists to
// refuse.
//
// A min-heap keyed on the target inverts it: the applier pops while the
// minimum is at or below its checkpoint and closes those channels, so a waiter
// is woken EXACTLY ONCE, by the commit that satisfied it, and a commit that
// satisfies nobody touches nothing. The heap's length is also the one number
// an operator wants — how many callers are waiting on this applier — which a
// condition variable's registry could not report.
type waiters struct {
	mu    sync.Mutex
	items waiterHeap
	next  uint64
}

// waiter is one blocked caller.
type waiter struct {
	// target is the position this caller is waiting for.
	target Position

	// done is closed by the commit that reaches target. A channel rather
	// than a flag because the caller selects on it against its own
	// context, which is what makes a wait cancellable without a goroutine.
	done chan struct{}

	// seq breaks ties in arrival order, so two waiters on one position are
	// woken in the order they arrived. Not load-bearing for correctness;
	// it makes the heap a total order, which makes the tests
	// deterministic.
	seq uint64

	// index is the heap's own bookkeeping.
	index int
}

// wait registers a waiter for target and returns its channel plus a cancel
// that removes it.
//
// The cancel is what stops an abandoned wait leaking a heap entry: a caller
// whose context expires would otherwise stay in the heap until the applier
// reached a position nobody is waiting for any more, holding a channel and a
// position in the minimum that delays nothing but is reported as a waiter.
func (w *waiters) wait(target Position) (<-chan struct{}, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item := &waiter{target: target, done: make(chan struct{}), seq: w.next}
	w.next++
	heap.Push(&w.items, item)
	return item.done, func() { w.drop(item) }
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

// release wakes every waiter at or below checkpoint.
//
// Called AFTER the transaction commits, never inside it: the store re-runs a
// conflicted transaction's body, so a wake from inside one can announce a
// position that was then rolled back.
func (w *waiters) release(checkpoint Position) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.items.Len() > 0 {
		top := w.items[0]
		if checkpoint.Packed() < top.target.Packed() {
			return
		}
		heap.Pop(&w.items)
		top.index = -1
		close(top.done)
	}
}

// releaseAll wakes everybody, for a shutting-down applier. A waiter left
// blocked on a stopped applier waits out its whole budget for a position
// nothing will ever reach.
func (w *waiters) releaseAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.items.Len() > 0 {
		top := heap.Pop(&w.items).(*waiter)
		top.index = -1
		close(top.done)
	}
}

// len is how many callers are waiting, which is the gauge an operator reads.
func (w *waiters) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.items.Len()
}

// minimum is the lowest position anybody is waiting for, and false when
// nobody is.
//
// The applier's batch linger reads it: a batch being filled while a caller
// waits for a record already in it is a batch that should end now, and the
// minimum is what says whether there is anybody to end it for.
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
	done, cancel := w.wait(target)
	defer cancel()
	// RE-CHECK AFTER REGISTERING. A commit landing between the check above
	// and the registration would otherwise never wake this waiter: it
	// released a heap that did not yet contain it, and no later commit is
	// obliged to reach the same position.
	if reached().Packed() >= target.Packed() {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
