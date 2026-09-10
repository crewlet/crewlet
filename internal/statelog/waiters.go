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

	// arrived is closed when a waiter registers on an EMPTY heap, and
	// replaced when the heap empties again.
	//
	// # Why an idle applier has to be interruptible at all
	//
	// The fetch an idle applier parks in asks the broker for a whole batch
	// and waits [FetchWait] for it to fill. One record arriving does not
	// end that wait — the batch closes when it is FULL or the wait
	// expires — so a barrier appended onto an idle log sits in a batch
	// nobody has finished collecting for five seconds, against a two
	// second read budget. Measured on the three-node e2e the moment the
	// read path was first wired: every linearizable read on an idle
	// company refused `behind`.
	//
	// [Runner.nextRun] already yields a PARTIAL RUN the instant somebody
	// is waiting, for exactly this reason. This is the same yield for the
	// case that has no run in hand yet, and it has to be a signal rather
	// than a check because the applier is blocked inside the broker call
	// by the time the waiter appears.
	arrived chan struct{}
}

// waking is the channel closed when a waiter arrives on an idle applier.
//
// A NEW CHANNEL IS MINTED LAZILY and closed exactly once per arrival, so a
// caller that takes it and then sees a waiter register is woken rather than
// left holding a channel nothing will ever close.
//
// IT FIRES ON ARRIVAL, NEVER WHILE ONE IS OUTSTANDING. Reporting "somebody is
// waiting" for as long as a waiter exists cancels every fetch the instant it
// starts, so the applier spins on empty batches and never collects the record
// the waiter is waiting for — which is slower than the wait it replaced.
// [Runner.nextRun] shortens its wait instead while one is outstanding.
func (w *waiters) waking() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.arrived == nil {
		w.arrived = make(chan struct{})
	}
	return w.arrived
}

// signalArrival wakes an idle fetch. Callers hold the lock.
func (w *waiters) signalArrival() {
	if w.arrived != nil {
		close(w.arrived)
		w.arrived = nil
	}
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
	// AND THE IDLE FETCH IS TOLD, so a barrier appended onto a quiet log
	// is collected now rather than when the batch it landed in finishes
	// filling. See [waiters.arrived].
	w.signalArrival()
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
