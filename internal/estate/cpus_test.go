package estate

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	n := 0
	for _, s := range c.waiting {
		n += len(s.queue)
	}
	return n
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

// taking takes a place through s in its own goroutine, returning once the take
// has reached s's CPUs — at a place or in the queue, each of which counts it
// once — and answers the channel its give-back (nil for an error) arrives on.
func taking(ctx context.Context, t *testing.T, s *cpuShare) <-chan func() {
	t.Helper()
	c := s.cpus
	before := queued(c) + heldBy(c)
	got := make(chan func(), 1)
	go func() {
		give, err := s.take(ctx)
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

// taken takes a place through s at once, failing the case where it waits.
func taken(t *testing.T, s *cpuShare) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	give, err := s.take(ctx)
	if err != nil {
		t.Fatalf("a free place was not taken at once: %v", err)
	}
	return give
}

// letInNext is which of the waiting queries was let in — exactly one, the
// moment a place was given back — and its give-back.
func letInNext(t *testing.T, waiting map[string]<-chan func()) (string, func()) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for name, got := range waiting {
			select {
			case give := <-got:
				delete(waiting, name)
				if give == nil {
					t.Fatalf("%s's take failed, want it let in", name)
				}
				return name, give
			default:
			}
		}
		select {
		case <-deadline:
			t.Fatal("a place was given back and no waiting query was let in")
		case <-time.After(time.Millisecond):
		}
	}
}

// A PLACE GIVEN BACK GOES TO THE REQUEST AT THE FRONT OF THE LINE, and a
// request goes to the back of it each time one of its queries is let in — so
// requests take turns at the node's CPUs, and one that arrives behind another
// waits for at most a turn of each request ahead of it, never for every query
// those requests queued before it. First come, first served across requests
// answered a later batch's queries only once an earlier batch's had all run,
// past the moment the later batch had to be answered.
func TestAPlaceGivenBackGoesToTheRequestAtTheFrontOfTheLine(t *testing.T) {
	t.Parallel()
	c := placesOf(places(1))
	a := c.share()
	give := taken(t, a)
	waiting := map[string]<-chan func(){}
	waiting["a2"] = taking(t.Context(), t, a)
	waiting["a3"] = taking(t.Context(), t, a)
	b := c.share()
	waiting["b1"] = taking(t.Context(), t, b)
	waiting["b2"] = taking(t.Context(), t, b)
	waiting["c1"] = taking(t.Context(), t, c.share())
	var order []string
	for len(waiting) > 0 {
		give()
		var name string
		name, give = letInNext(t, waiting)
		order = append(order, name)
		if held := heldBy(c); held != 1 {
			t.Fatalf("%d places held of one", held)
		}
	}
	give()
	if want := []string{"a2", "b1", "c1", "a3", "b2"}; !slices.Equal(order, want) {
		t.Errorf("the places given back went to %v, want %v — a turn for each request in "+
			"the line, each going to the back of it once let in", order, want)
	}
}

// A PLACE GIVEN BACK GOES TO THE REQUEST HOLDING FEWEST, ahead of one before it
// in the line: requests share the CPUs equally, and turns alone handed a
// request already holding most of them one more, ahead of a request holding
// fewer.
func TestAPlaceGivenBackGoesToTheRequestHoldingFewest(t *testing.T) {
	t.Parallel()
	c := placesOf(places(4))
	a := c.share()
	defer taken(t, a)()
	defer taken(t, a)()
	b := c.share()
	defer taken(t, b)()
	giveC := taken(t, c.share())
	// a, two places and at the front of the line, the place it took last
	// let in before b's; b, one place.
	waiting := map[string]<-chan func(){
		"a": taking(t.Context(), t, a),
	}
	waiting["b"] = taking(t.Context(), t, b)
	giveC()
	name, give := letInNext(t, waiting)
	defer give()
	if name != "b" {
		t.Errorf("the place given back went to %s, holding two, want b, holding one", name)
	}
	for other, got := range waiting {
		if give, ok := admitted(got); ok {
			give()
			t.Errorf("%s was let in beside %s on one place given back", other, name)
		}
	}
}

// A RAISED LIMIT LETS A WAITER IN AT THE NEXT TAKE: the runtime moves
// GOMAXPROCS as the container's limit moves, and a bound fixed when the node
// started would run it on the CPUs it booted with for ever.
func TestARaisedLimitLetsAWaiterInAtTheNextTake(t *testing.T) {
	t.Parallel()
	limit := places(1)
	c := placesOf(limit)
	s := c.share()
	defer taken(t, s)()
	waiter := taking(t.Context(), t, s)
	limit.Store(2)
	// THE NEXT TAKE sees the raise, lets the waiter in ahead of itself, and
	// waits behind it — two places, both taken.
	behind, cancel := context.WithCancel(t.Context())
	late := taking(behind, t, s)
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
	s := c.share()
	giveA, giveB := taken(t, s), taken(t, s)
	limit.Store(1)
	waiter := taking(t.Context(), t, c.share())
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
	give := taken(t, c.share())
	ctx, cancel := context.WithCancel(t.Context())
	gone := taking(ctx, t, c.share())
	cancel()
	if give := <-gone; give != nil {
		t.Fatal("a query whose context ended was handed a place")
	}
	if n := queued(c); n != 0 {
		t.Fatalf("%d queries still queued after the only waiter gave up", n)
	}
	c.mu.Lock()
	shares := len(c.waiting)
	c.mu.Unlock()
	if shares != 0 {
		t.Fatalf("%d requests still in the line with nothing waiting", shares)
	}
	give()
	if held := heldBy(c); held != 0 {
		t.Fatalf("%d places held after the only query gave its back", held)
	}
	taken(t, c.share())()
}

// A PLACE LET IN AS ITS CONTEXT ENDED IS GIVEN BACK: the place was handed over
// while the waiter was already leaving, and kept by nobody it is one CPU fewer
// for the life of the node.
func TestAPlaceLetInAsItsContextEndedIsGivenBack(t *testing.T) {
	t.Parallel()
	c := placesOf(places(1))
	first := c.share()
	taken(t, first)
	ctx, cancel := context.WithCancel(t.Context())
	got := taking(ctx, t, c.share())
	// PARKED in its wait before the race is staged.
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	// The context ends while the queue is held, so the waiter wakes to its
	// context alone and stops at the lock; the first query's place is then
	// given back, and handed to the waiter, before the waiter can see it.
	cancel()
	time.Sleep(20 * time.Millisecond)
	c.held--
	first.held--
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
	taken(t, c.share())()
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

// A BATCH THAT ARRIVES BEHIND ANOTHER ON A BUSY HOLDER STILL GETS ITS SHARE of
// the holder's CPUs within its own attempt. Each batch is answered a margin
// before its asker stops waiting, and a reply that decided nothing moves every
// partition in it on — so a batch whose queries waited behind every query an
// earlier batch had queued lost every partition, from a holder that was busy
// and nothing worse, and with no other holder the whole read was refused.
func TestABatchBehindAnotherOnABusyHolderGetsItsShare(t *testing.T) {
	t.Parallel()
	f, node, _ := ceilingFleet(t, queue.MaxPayloadBytes)
	node.placesFor(1)
	// FIVE READS TO A BATCH, each a fifth of the attempt and then some, so
	// no batch's reads fit in one attempt on the holder's one CPU and the
	// first batch is still running its own when the second's ends.
	const read, attempt = 300 * time.Millisecond, 1500 * time.Millisecond
	node.set(func(n *partNode) { n.delay = read })
	done := make(chan error, 2)
	gatherOn := func(r *Router) {
		r.readBudget = attempt
		answer, cov, err := listAll(t, r, 0, "")
		if err == nil && (!cov.Complete() || len(answer.Rows) != 24) {
			err = fmt.Errorf("%d rows, missing %+v", len(answer.Rows), cov.Missing)
		}
		done <- err
	}
	go gatherOn(f.router(t, "agent-1", nil))
	// EVERY QUERY OF THE FIRST BATCH at the place or waiting for it before
	// the second batch asks.
	waitFor(t, "the first batch's five queries", func() bool {
		return int(node.inFlight.Load())+queued(node.CPUs()) == 5
	})
	go gatherOn(f.router(t, "agent-2", nil))
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("gather: %v, want every partition of both reads from a holder "+
				"that was busy and nothing worse", err)
		}
	}
}

// THIS NODE'S OWN GATHER IS ONE REQUEST, as a batch it answers for another is
// one: each of its partitions taking its place as a request of its own, a
// batch arriving behind them waited for every one — first come, first served
// again, by the back door — and lost every partition to its stop.
func TestThisNodesOwnGatherTakesItsCPUsAsOneRequest(t *testing.T) {
	t.Parallel()
	every := []statelog.PartitionID{tp(0), tp(1), tp(2), tp(3), company}
	f := newPartFleet(t, map[string][]statelog.PartitionID{"data-a": every})
	node := f.nodes["data-a"]
	node.placesFor(1)
	const read, attempt = 300 * time.Millisecond, 1500 * time.Millisecond
	node.set(func(n *partNode) { n.delay = read })
	done := make(chan error, 2)
	gatherOn := func(r *Router) {
		r.readBudget = attempt
		answer, cov, err := listAll(t, r, 0, "")
		if err == nil && (!cov.Complete() || len(answer.Rows) != 24) {
			err = fmt.Errorf("%d rows, missing %+v", len(answer.Rows), cov.Missing)
		}
		done <- err
	}
	go gatherOn(f.router(t, "data-a", node))
	waitFor(t, "data-a's own five queries", func() bool {
		return int(node.inFlight.Load())+queued(node.CPUs()) == 5
	})
	go gatherOn(f.router(t, "agent-1", nil))
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("gather: %v, want every partition of both reads from a holder "+
				"that was busy and nothing worse", err)
		}
	}
}
