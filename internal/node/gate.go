package node

import (
	"context"
	"sync"
)

// The per-node turn concurrency gate.
//
// # What it bounds, and what already bounds itself
//
// A seat's mailbox is a durable subscription and its handler runs one batch
// at a time, so a SEAT is already serial. What is not bounded is how many
// seats run at once: that number is the placement policy's answer to "what is
// this node's fair share", which is arithmetic over the company's seat count
// and the live node count — not a statement about the host this process is
// running on. A node handed forty seats will open forty simultaneous model
// round-trips and their tool loops, and nothing in the config could say
// otherwise. This is the knob that can.
//
// # Per node, which is what makes it Tier A
//
// A fleet's ceiling is N × this value. That is deliberate and it is the one
// knob a fleet changes the meaning of: it is sized against the machine, and
// the machine is what an operator owns. Putting it in the company config
// would ask a founder to reason about the hosts their engine happens to run
// on today.
//
// # Why a turn waits rather than being refused
//
// A refusal would send the trigger back to the broker to be redelivered on
// the broker's schedule, which for a chat message a person is waiting on
// turns a busy moment into a visible stall. Waiting keeps the delivery in
// this process, where it starts the instant a slot frees.
//
// The one exception is a DRAIN. There the wait is exactly wrong: the node is
// leaving, the slot it is waiting for is being handed to a peer, and a turn
// that started mid-drain runs a full multi-minute Plan → Execute → Review
// after the operator asked the process to stop. So a drain closes the gate
// and every waiter leaves immediately, deferring its delivery — which is
// unacked, so the successor gets it rather than the broker's redelivery
// timer. Turns already past the gate are untouched and run to completion:
// they have called a model and may have fired side effects, and abandoning
// that work buys a faster deploy by throwing away what was nearly done.
//
// # Why closing is reversible
//
// A drain is not always a shutdown. The posture path sheds a node's seats on
// config divergence and CONVERGES it back, and [seat.Host.ResumeClaiming]
// exists for exactly that. A gate that latched shut would leave such a node
// holding seats, attached to their mailboxes and refusing every turn — the
// same permanently-deaf seat OnAdmission exists to prevent, one layer up.
type gate struct {
	// slots is the semaphore. A buffered channel rather than a
	// sync.Cond, because the wait has to select against two other things
	// — the delivery's context and the close below — and a Cond cannot
	// be waited on with a context at all.
	slots chan struct{}

	// closed is the barrier: an open channel while the node serves, closed
	// while it drains, and REPLACED with a fresh open one when it resumes.
	// A channel rather than a flag because a waiter is parked in a select,
	// and a flag it would have to poll is a flag it never reads. Swapping
	// rather than reopening because a closed channel cannot be reopened.
	mu     sync.Mutex
	closed chan struct{}
}

// DefaultMaxConcurrent is how many turns one process runs at once when
// `node.max_concurrent` says nothing.
//
// It lives here, in the layer that enforces it, so the config field and the
// gate can never disagree about what unset means — the same arrangement
// coordination.lease_ttl_seconds has with seat.SeatLeaseTTL.
//
// Sized so it does not change the behaviour of any company that runs on one
// node today, while still bounding the pathological case. A turn's cost here
// is one model round-trip and its tool loop — a few megabytes, nothing like
// the 200-400 MB a cli-agent subprocess holds, which is why this is far above
// that provider's own max_concurrent of 4. The seat count of a single-node
// company is the number that matters: the example company runs a handful of
// seats, a large one runs tens, and a node holding more than 32 seats is one
// that wanted a second node. Past it the excess turns wait rather than opening
// a hundred simultaneous completions against a provider that will rate-limit
// them anyway.
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
	select {
	case g.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.slots }) }, true
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

// inFlight is how many slots are currently held. Diagnostics only.
func (g *gate) inFlight() int { return len(g.slots) }
