package estate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// placesOf is a CPUs of n places, n read whenever a place is taken or given
// back — so a case can raise or cut it, as the runtime moves GOMAXPROCS.
func placesOf(n *atomic.Int32) *CPUs {
	return &CPUs{limit: func() int { return int(n.Load()) }}
}

// places is n places.
func places(n int32) *atomic.Int32 {
	var out atomic.Int32
	out.Store(n)
	return &out
}

// queued is how many queries wait for one of c's places.
func queued(c *CPUs) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiting)
}

// heldBy is how many of c's places are taken.
func heldBy(c *CPUs) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

// waitFor waits up to five seconds for done.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// taking takes a place on c in its own goroutine, returning once the take has
// reached c — at a place or in the queue, each of which counts it once — and
// answers the channel its give-back (nil for an error) arrives on.
func taking(ctx context.Context, t *testing.T, c *CPUs) <-chan func() {
	t.Helper()
	before := queued(c) + heldBy(c)
	got := make(chan func(), 1)
	go func() {
		give, err := c.take(ctx)
		if err != nil {
			give = nil
		}
		got <- give
	}()
	waitFor(t, "the query to reach its CPUs", func() bool { return queued(c)+heldBy(c) > before })
	return got
}

// admitted reports whether the query got answered a place within a moment.
func admitted(got <-chan func()) (func(), bool) {
	select {
	case give := <-got:
		return give, give != nil
	case <-time.After(50 * time.Millisecond):
		return nil, false
	}
}

// A PLACE GIVEN BACK GOES TO THE LONGEST WAITER: a query waits behind every
// one that asked before it, so a batch of a hundred and fifty partitions
// cannot keep a later batch's few waiting until all of its own have run.
func TestAPlaceGivenBackGoesToTheLongestWaiter(t *testing.T) {
	t.Parallel()
	c := placesOf(places(1))
	give, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := taking(t.Context(), t, c)
	second := taking(t.Context(), t, c)
	give()
	giveFirst, ok := admitted(first)
	if !ok {
		t.Fatal("the place given back went to nobody, want the query that waited longest")
	}
	if _, ok := admitted(second); ok {
		t.Fatal("the later waiter was let in beside the first, on one place")
	}
	giveFirst()
	if giveSecond, ok := admitted(second); !ok {
		t.Fatal("the second waiter was never let in")
	} else {
		giveSecond()
	}
}

// A RAISED LIMIT LETS A WAITER IN AT THE NEXT TAKE: the runtime moves
// GOMAXPROCS as the container's limit moves, and a bound fixed when the node
// started would run it on the CPUs it booted with for ever.
func TestARaisedLimitLetsAWaiterInAtTheNextTake(t *testing.T) {
	t.Parallel()
	limit := places(1)
	c := placesOf(limit)
	give, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer give()
	waiter := taking(t.Context(), t, c)
	limit.Store(2)
	// THE NEXT TAKE sees the raise, lets the waiter in ahead of itself, and
	// waits behind it — two places, both taken.
	behind, cancel := context.WithCancel(t.Context())
	late := taking(behind, t, c)
	gave, ok := admitted(waiter)
	if !ok {
		t.Fatal("the waiter was not let in when the limit rose, want it in at the next take")
	}
	defer gave()
	if _, ok := admitted(late); ok {
		t.Error("the query that saw the raise took a third place of two")
	}
	cancel()
}

// A CUT LIMIT LETS NOBODY IN UNTIL THE PLACES HELD FALL UNDER IT, and stops
// no query already running.
func TestACutLimitLetsNobodyInUntilThePlacesHeldFallUnderIt(t *testing.T) {
	t.Parallel()
	limit := places(2)
	c := placesOf(limit)
	giveA, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	giveB, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	limit.Store(1)
	waiter := taking(t.Context(), t, c)
	giveA()
	if _, ok := admitted(waiter); ok {
		t.Fatal("a waiter was let in beside a running query on one place")
	}
	giveB()
	give, ok := admitted(waiter)
	if !ok {
		t.Fatal("the waiter was not let in once the places held fell under the limit")
	}
	give()
	if held := heldBy(c); held != 0 {
		t.Errorf("%d places held after every query gave its back", held)
	}
}

// A WAITER WHOSE CONTEXT ENDS LEAVES THE QUEUE: it takes no place, and the
// next place given back goes to whoever is still waiting, never to it.
func TestAWaiterWhoseContextEndsLeavesTheQueue(t *testing.T) {
	t.Parallel()
	c := placesOf(places(1))
	give, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	gone := taking(ctx, t, c)
	cancel()
	if give := <-gone; give != nil {
		t.Fatal("a query whose context ended was handed a place")
	}
	if n := queued(c); n != 0 {
		t.Fatalf("%d queries still queued after the only waiter gave up", n)
	}
	give()
	if held := heldBy(c); held != 0 {
		t.Fatalf("%d places held after the only query gave its back", held)
	}
	again, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	again()
}

// A PLACE LET IN AS ITS CONTEXT ENDED IS GIVEN BACK: the place was handed over
// while the waiter was already leaving, and kept by nobody it is one CPU fewer
// for the life of the node.
func TestAPlaceLetInAsItsContextEndedIsGivenBack(t *testing.T) {
	t.Parallel()
	c := placesOf(places(1))
	if _, err := c.take(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	got := taking(ctx, t, c)
	// PARKED in its wait before the race is staged.
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	// The context ends while the queue is held, so the waiter wakes to its
	// context alone and stops at the lock; the first query's place is then
	// given back, and handed to the waiter, before the waiter can see it.
	cancel()
	time.Sleep(20 * time.Millisecond)
	c.held--
	c.admit()
	c.mu.Unlock()
	if give := <-got; give != nil {
		// THE OTHER ORDER — admitted before it saw its context — is a
		// place it holds and gives back itself.
		give()
	}
	if held := heldBy(c); held != 0 {
		t.Fatalf("%d places held with no query running, want the place handed to a "+
			"waiter that was leaving given back", held)
	}
	give, err := c.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	give()
}

// TWO BATCHES TOGETHER RUN NO MORE QUERIES THAN THE HOLDER'S CPUS: the bound is
// the node's, not the batch's — under a cap per batch, a holder answering two
// seats' gathers ran two queries per CPU, and its answerer serves up to
// [queue.MaxConcurrentAnswers] requests at once.
func TestTwoBatchesTogetherRunNoMoreQueriesThanTheHoldersCPUs(t *testing.T) {
	t.Parallel()
	f, node, _ := ceilingFleet(t, queue.MaxPayloadBytes)
	node.placesFor(2)
	gate := make(chan struct{})
	node.set(func(n *partNode) { n.gate = gate })
	done := make(chan error, 2)
	for _, asker := range []string{"agent-1", "agent-2"} {
		r := f.router(t, asker, nil)
		r.readBudget = 10 * time.Second
		go func() {
			answer, cov, err := listAll(t, r, 0, "")
			if err == nil && (!cov.Complete() || len(answer.Rows) != 24) {
				err = errors.New("short answer")
			}
			done <- err
		}()
	}
	// EVERY QUERY OF BOTH BATCHES at a place or waiting for one.
	waitFor(t, "both batches' ten queries", func() bool {
		return int(node.inFlight.Load())+queued(node.CPUs()) == 10
	})
	time.Sleep(20 * time.Millisecond)
	close(gate)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("gather: %v", err)
		}
	}
	if asked := node.asked(); len(asked) != 2 {
		t.Fatalf("the holder was asked %d times, want one batch per asker", len(asked))
	}
	if peak := node.peak.Load(); peak != 2 {
		t.Errorf("%d of the two batches' reads ran at once on a holder of two CPUs, want 2",
			peak)
	}
}

// THIS NODE'S OWN GATHER SHARES ITS CPUS WITH THE BATCHES IT ANSWERS: both run
// on the same processors, and a cap of the router's own ran a query per CPU
// beside every batch the node was answering for another.
func TestThisNodesOwnGatherSharesItsCPUsWithTheBatchesItAnswers(t *testing.T) {
	t.Parallel()
	every := []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company}
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": every})
	node := f.nodes["data-a"]
	node.placesFor(2)
	gate := make(chan struct{})
	node.set(func(n *partNode) { n.gate = gate })
	own := f.router(t, "data-a", node)
	other := f.router(t, "agent-1", nil)
	done := make(chan error, 2)
	for _, r := range []*Router{own, other} {
		r.readBudget = 10 * time.Second
		go func() {
			answer, cov, err := listAll(t, r, 0, "")
			if err == nil && (!cov.Complete() || len(answer.Rows) != 24) {
				err = errors.New("short answer")
			}
			done <- err
		}()
	}
	waitFor(t, "both gathers' ten queries", func() bool {
		return int(node.inFlight.Load())+queued(node.CPUs()) == 10
	})
	time.Sleep(20 * time.Millisecond)
	close(gate)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("gather: %v", err)
		}
	}
	if asked := node.asked(); len(asked) != 1 {
		t.Fatalf("data-a was asked %d times over the broker, want once — for agent-1's "+
			"batch, its own partitions answered in-process", len(asked))
	}
	if peak := node.peak.Load(); peak != 2 {
		t.Errorf("%d reads ran at once on a node of two CPUs, its own gather's and the "+
			"batch it answered together, want 2", peak)
	}
}

// noCPUs is a node's copies whose CPUs are missing.
type noCPUs struct{ LocalBackends }

func (noCPUs) CPUs() *CPUs { return nil }

// A NODE'S COPIES WITHOUT CPUS ARE REFUSED, by the server and the router
// alike, naming what to return: answered with none, every gather query they
// ran would be unbounded.
func TestCopiesWithoutCPUsAreRefused(t *testing.T) {
	t.Parallel()
	local := noCPUs{&partNode{name: "data-a"}}
	q := memory.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	placement := &partPlacement{layout: dividedLayout}
	if stop, err := Serve(t.Context(), q, "data-a", local, placement, ServerSeams{}); err == nil {
		_ = stop(context.Background())
		t.Error("Serve took copies whose CPUs are nil")
	} else if !strings.Contains(err.Error(), "LocalBackends.CPUs") {
		t.Errorf("Serve's refusal %q does not name LocalBackends.CPUs", err)
	}
	_, err := NewRouter(RouterOptions{Queue: q, Self: "data-a", Placement: placement,
		Session: NewSession(), Local: local})
	if err == nil {
		t.Error("NewRouter took copies whose CPUs are nil")
	} else if !strings.Contains(err.Error(), "LocalBackends.CPUs") {
		t.Errorf("NewRouter's refusal %q does not name LocalBackends.CPUs", err)
	}
}
