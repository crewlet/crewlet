package node

import (
	"context"
	"sync"
	"testing"
	"time"
)

// WHAT THE GATE IS FOR.
//
// A seat's mailbox is already serial, so what is unbounded is how many SEATS
// run at once — and that number is the placement policy's arithmetic over the
// company's seat count, not a statement about the host. Five docs pages
// described `max_concurrent` as this node's ceiling and the knob capped only
// one LLM provider's subprocesses. These cases are the gate that makes the
// documented meaning true.

// gateWait is how long a case waits for something that should already have
// happened. Generous, because it only ever elapses on a real failure — the
// success path is a channel receive that lands in microseconds.
const gateWait = 2 * time.Second

// THE CEILING HOLDS. Turns past it wait rather than running.
func TestTheGateAdmitsExactlyItsSize(t *testing.T) {
	t.Parallel()
	g := newGate(2)

	first, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("the first turn was refused by an open gate")
	}
	second, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("the second turn was refused under a cap of 2")
	}
	if got := g.inFlight(); got != 2 {
		t.Fatalf("in flight = %d, want 2", got)
	}

	// The third has to WAIT, which is what a cap means. Proven by the
	// absence of an answer while both slots are held.
	admitted := make(chan struct{})
	go func() {
		release, ok := g.acquire(context.Background())
		if ok {
			close(admitted)
			release()
		}
	}()
	select {
	case <-admitted:
		t.Fatal("a third turn ran under a cap of 2")
	case <-time.After(50 * time.Millisecond):
	}

	// And is admitted the instant a slot frees, in this process, rather
	// than being sent back for the broker to redeliver on its own clock.
	first()
	select {
	case <-admitted:
	case <-time.After(gateWait):
		t.Fatal("a waiting turn was not admitted when a slot freed")
	}
	second()
}

// A RELEASE IS IDEMPOTENT. A handler that returns twice down two paths — a
// defer plus an explicit call — would otherwise free a slot it never took,
// and the cap would drift upward for the life of the process.
func TestReleasingTwiceFreesOneSlot(t *testing.T) {
	t.Parallel()
	g := newGate(1)
	release, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("refused by an open gate")
	}
	release()
	// The second release runs off the main goroutine: without the guard it
	// blocks on an empty channel forever, and a case that deadlocks fails
	// only when the whole binary's timeout expires.
	done := make(chan struct{})
	go func() { release(); close(done) }()
	select {
	case <-done:
	case <-time.After(gateWait):
		t.Fatal("a second release blocked — it is waiting to free a slot " +
			"this handler never took")
	}
	if got := g.inFlight(); got != 0 {
		t.Fatalf("in flight = %d, want 0", got)
	}
	// The cap still holds afterwards: a second slot must not have appeared.
	if _, ok := g.acquire(t.Context()); !ok {
		t.Fatal("the freed slot was not reusable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.acquire(ctx); ok {
		t.Fatal("a double release widened the gate")
	}
}

// A DRAIN RELEASES EVERY WAITER AT ONCE. They have not called a model or
// fired a side effect, so they are the one kind of in-flight work that costs
// nothing to abandon — and running them would put full Plan → Execute →
// Review turns inside a shutdown that waits indefinitely.
func TestDrainingReleasesEveryWaiter(t *testing.T) {
	t.Parallel()
	g := newGate(1)
	held, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("refused by an open gate")
	}

	const waiters = 8
	refused := make(chan bool, waiters)
	var ready sync.WaitGroup
	ready.Add(waiters)
	for range waiters {
		go func() {
			ready.Done()
			_, ok := g.acquire(context.Background())
			refused <- ok
		}()
	}
	ready.Wait()

	g.close()
	for range waiters {
		select {
		case ok := <-refused:
			if ok {
				t.Fatal("a waiter was admitted by a draining gate")
			}
		case <-time.After(gateWait):
			t.Fatal("a waiter was left parked through a drain")
		}
	}
	// The turn already holding a slot is untouched: it has done work.
	if got := g.inFlight(); got != 1 {
		t.Fatalf("in flight = %d — the drain took a running turn's slot", got)
	}
	held()
}

// A DRAINING GATE REFUSES EVEN WITH A SLOT FREE. Selecting over a ready
// barrier and a ready slot at once picks uniformly at random, so without the
// barrier being checked first a draining node would admit about half the
// turns arriving at it.
func TestADrainingGateRefusesWhileIdle(t *testing.T) {
	t.Parallel()
	g := newGate(4)
	g.close()
	for range 20 {
		if _, ok := g.acquire(t.Context()); ok {
			t.Fatal("an idle draining gate admitted a turn")
		}
	}
}

// CLOSING IS IDEMPOTENT. A drain that is interrupted and re-entered calls it
// again, and closing a closed channel panics.
func TestClosingTwiceDoesNotPanic(t *testing.T) {
	t.Parallel()
	g := newGate(1)
	g.close()
	g.close()
	if _, ok := g.acquire(t.Context()); ok {
		t.Fatal("a re-closed gate admitted a turn")
	}
}

// A DRAIN IS REVERSIBLE. The posture path sheds a node on config divergence
// and converges it back; a gate that latched shut would leave that node
// holding its seats, attached to their mailboxes, and refusing every turn.
func TestAResumedGateServesAgain(t *testing.T) {
	t.Parallel()
	g := newGate(2)
	g.close()
	if _, ok := g.acquire(t.Context()); ok {
		t.Fatal("a draining gate admitted a turn")
	}
	g.open()
	release, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("a resumed gate still refuses every turn")
	}
	release()
	// And the cap is the one it was built with, not a fresh unbounded one.
	a, _ := g.acquire(t.Context())
	b, _ := g.acquire(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.acquire(ctx); ok {
		t.Fatal("resuming widened the gate")
	}
	a()
	b()
}

// OPENING AN OPEN GATE CHANGES NOTHING. Swapping the barrier unconditionally
// would strand a concurrent waiter on a channel nothing will ever close.
func TestOpeningAnOpenGateStrandsNobody(t *testing.T) {
	t.Parallel()
	g := newGate(1)
	held, _ := g.acquire(t.Context())

	admitted := make(chan struct{})
	go func() {
		release, ok := g.acquire(context.Background())
		if ok {
			release()
		}
		close(admitted)
	}()
	// Let the waiter park on the current barrier, then re-open.
	time.Sleep(20 * time.Millisecond)
	g.open()
	g.close()
	select {
	case <-admitted:
	case <-time.After(gateWait):
		t.Fatal("a waiter was stranded on a barrier that was replaced under it")
	}
	held()
}

// A CANCELLED DELIVERY STOPS WAITING. Its events are going back to the broker
// either way, and a waiter that outlived its own context would hold a slot
// open for work nobody is going to run.
func TestACancelledDeliveryLeavesTheQueue(t *testing.T) {
	t.Parallel()
	g := newGate(1)
	held, _ := g.acquire(t.Context())
	defer held()

	ctx, cancel := context.WithCancel(t.Context())
	left := make(chan bool, 1)
	go func() {
		_, ok := g.acquire(ctx)
		left <- ok
	}()
	cancel()
	select {
	case ok := <-left:
		if ok {
			t.Fatal("a cancelled delivery was admitted")
		}
	case <-time.After(gateWait):
		t.Fatal("a cancelled delivery stayed parked")
	}
}

// AN ABSENT max_concurrent TAKES THE DEFAULT, NOT ONE SLOT AND NOT NO LIMIT.
//
// Zero is the shape an absent Tier A key has, and it reaches here from every
// caller that builds a node in code. Reading it as one would silently
// serialize a node that was never configured; reading it as unbounded would
// leave the knob's whole purpose off by default.
func TestAnUnsetSizeTakesTheDefault(t *testing.T) {
	t.Parallel()
	g := newGate(0)
	releases := make([]func(), 0, DefaultMaxConcurrent)
	for i := range DefaultMaxConcurrent {
		release, ok := g.acquire(t.Context())
		if !ok {
			t.Fatalf("turn %d of %d was refused", i+1, DefaultMaxConcurrent)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.acquire(ctx); ok {
		t.Fatalf("an unset size admitted more than %d turns — it is unbounded",
			DefaultMaxConcurrent)
	}
	for _, release := range releases {
		release()
	}
}

// A NEGATIVE SIZE STILL GATES AND STILL SERVES. Config refuses one, so this
// is only reached from a bootstrap built in code — where a gate that admitted
// nothing would wedge the node with no error to read.
func TestANegativeSizeIsOneSlotNotNone(t *testing.T) {
	t.Parallel()
	g := newGate(-1)
	release, ok := g.acquire(t.Context())
	if !ok {
		t.Fatal("a negative size admitted nothing, wedging the node")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.acquire(ctx); ok {
		t.Error("a negative size became an unbounded gate")
	}
	release()
}
