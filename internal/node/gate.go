package node

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// The per-node turn concurrency gate.
//
// A seat is already serial (a durable mailbox, one batch at a time), but how
// many seats run at once is the placement policy's fair share, not a statement
// about the host. This knob bounds it. It is per node (Tier A): a fleet's
// ceiling is N x this value, sized against the machine an operator owns.
//
// A turn waits rather than being refused: a refusal sends the trigger back to
// the broker's redelivery schedule, a visible stall for a waiting person. A
// DRAIN is the exception: the gate closes and waiters leave at once, leaving
// their deliveries unacked for the successor. Turns past the gate run to
// completion, having called a model and possibly fired side effects.
type gate struct {
	// slots is the semaphore. A buffered channel rather than a
	// sync.Cond, because the wait has to select against two other things
	// — the delivery's context and the close below — and a Cond cannot
	// be waited on with a context at all.
	slots chan struct{}

	// closed is the barrier: an open channel while the node serves, closed
	// while it drains, and REPLACED with a fresh open one when it resumes.
	// A channel rather than a flag because a waiter is parked in a select.
	// Swapped because a closed channel cannot be reopened, which keeps
	// [Node.ResumeClaiming] possible. It has no production caller (every drain
	// is a shutdown); the reversibility is kept because it is free and a latch
	// would fail silently, not because anything reverses a drain.
	mu     sync.Mutex
	closed chan struct{}

	// running counts the slots held, and is what a drain waits on to see
	// every turn end ([gate.waitIdle]). The queue's own count of running
	// handlers is NOT that number: a turn this node starts outside any
	// delivery (a recorded answer's retried resume, on the sandbox
	// coordinator's own timer) holds a slot and no handler, so a drain that
	// waited on handlers alone handed the seat back under a running turn.
	// The same counter the queue keeps its handlers in, for the reason
	// [queue.Inflight] gives: a WaitGroup cannot be counted up while a
	// drain is waiting on it.
	running queue.Inflight

	// waiting counts the turns parked for a slot while the node is at its
	// ceiling ([Node.TurnsWaiting]): the one state a caller cannot see from
	// the slots, since a turn that waits holds none.
	waiting atomic.Int64
}

// DefaultMaxConcurrent is how many turns one process runs at once when
// `node.max_concurrent` says nothing.
//
// It lives in the layer that enforces it so the config field and the gate
// cannot disagree about what unset means. A turn costs one model round-trip
// and its tool loop (megabytes, not the 200-400 MB of a harness subprocess), so
// this sits far above that provider's own max_concurrent of 4. A node holding
// more than 32 seats wanted a second node; past it the excess turns wait rather
// than open a hundred completions a provider would rate-limit anyway.
const DefaultMaxConcurrent = 32

// newGate builds a gate of the given size. A size below 1 is NEVER a
// pass-through: zero takes [DefaultMaxConcurrent] (an absent config key), and
// a negative — which config refuses — is clamped to one slot rather than to
// none, because a gate that admitted nothing would wedge the node silently.
func newGate(size int) *gate {
	switch {
	case size == 0:
		size = DefaultMaxConcurrent
	case size < 0:
		size = 1
	}
	return &gate{slots: make(chan struct{}, size), closed: make(chan struct{})}
}

// acquire takes a slot, returning the release and whether one was taken.
//
// False means the node is draining or the delivery's context is done. Both
// are the caller's cue to hand the delivery back rather than run it, and
// neither is an error: the events are healthy and somebody else will take
// them.
//
// The release is nil when false, and calling it is never required in that
// case — returning a no-op instead would be a release the caller could
// deadlock on by holding, since it releases a slot it never took.
func (g *gate) acquire(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	barrier := g.closed
	g.mu.Unlock()

	// THE BARRIER FIRST, in its own select, because a select over several
	// ready channels picks uniformly at random: a draining node with a
	// free slot would admit roughly half the turns arriving at it.
	select {
	case <-barrier:
		return nil, false
	default:
	}
	g.waiting.Add(1)
	defer g.waiting.Add(-1)
	select {
	case g.slots <- struct{}{}:
		g.running.Begin()
		var once sync.Once
		return func() {
			once.Do(func() {
				g.running.End()
				<-g.slots
			})
		}, true
	case <-barrier:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// close releases every waiter and refuses every later acquire.
//
// Idempotent: a drain that is interrupted and re-entered calls it again, and
// closing a closed channel panics. It does NOT wait for the turns already
// holding slots — the drain's own wait on in-flight handlers does that, and
// it is the layer that can see them.
func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.closed:
	default:
		close(g.closed)
	}
}

// open undoes close, so a node that drained and converged serves again.
//
// A FRESH barrier, never a reopened one. Waiters released by the close are
// gone and are not owed a slot: their deliveries were deferred and the broker
// or a peer already has them.
func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.closed:
		g.closed = make(chan struct{})
	default:
		// Already open. Replacing the barrier here would strand a
		// concurrent acquire on a channel nothing will ever close.
	}
}

// inFlight is how many slots are currently held.
func (g *gate) inFlight() int { return g.running.Count() }

// waitIdle waits until no slot is held, timeout passes or ctx ends, and
// reports how many were still held when it stopped: [queue.Inflight.Wait]'s
// contract, which the drain already reads the queue's handlers through.
func (g *gate) waitIdle(ctx context.Context, timeout time.Duration) (int, error) {
	return g.running.Wait(ctx, timeout)
}
