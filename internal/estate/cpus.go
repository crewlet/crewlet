package estate

import (
	"context"
	"runtime"
	"slices"
	"sync"
)

// CPUs is the places a node's gather queries take: at most one query per CPU
// at a time, across EVERYTHING the node runs them for — every batch it answers
// for another node ([Serve]) and every gather its own router answers
// in-process ([Router]) — SHARED FAIRLY between the requests they run for. A
// node's [LocalBackends] hand both the same one ([LocalBackends.CPUs]).
//
// # One per node
//
// The bound is on what the node's processors are doing, so it is the node's
// and nobody else's. Held per batch, it was multiplied by however many batches
// ran at once: a holder answering two seats' gathers ran two queries per CPU,
// and its answerer serves up to [queue.MaxConcurrentAnswers] requests at a
// time — while this node's own gathers, answered in-process, ran a cap of
// their own beside them on the same processors. And a query that takes no
// notice of its context keeps its place after its batch is answered, which is
// what its processor is doing: under a place per batch, the batch that asked
// for that partition again started its query a second time beside the first,
// on a cap of its own.
//
// # Shared fairly between REQUESTS, never first come, first served
//
// Each request's queries take their places through a share of their own
// ([CPUs.share]) — a batch this node answers, or one round of a gather it
// answers in-process. A place given back goes to the waiting request HOLDING
// THE FEWEST, and among those to the one at the FRONT OF THE LINE: a request
// joins the back of the line when it arrives, and goes to the back again each
// time one of its queries is let in. So the requests running on the node hold
// equal shares of its CPUs and take turns at them, and one that arrives behind
// a batch of a hundred and fifty partitions waits for at most a turn of each
// request ahead of it, never for every query that batch queued before it.
//
// First come, first served across requests is what that rules out, and it
// lost reads whole: a holder answers a batch a margin before its asker stops
// waiting ([batchMargin]) and a reply that decided nothing moves every
// partition in it on ([Router.askBatch]) — so a batch whose queries waited
// behind every query an earlier batch had queued was answered with none of
// them run, and a holder that was busy, and nothing worse, read to its asker
// as one that could not answer; with no other holder the whole read was
// refused. Turns alone — the next place to the front of the line, whatever it
// holds — would hand a request already holding every place but one that last
// one too, ahead of a request holding none.
//
// # Only a gather's queries
//
// A slice takes a place for its QUERY alone: its floor and barrier waits come
// first and are not CPU work, and a place held through them would queue every
// partition of a batch behind the slowest log's applier — a hundred and fifty
// partitions each waiting out a two-second floor, a CPU's worth at a time. And
// only a gather's slices take one. A single-partition read is one query per
// request, bounded by the requests a node answers at once; a gather is the one
// shape that turns one request into a query per partition, and that is what
// needs a bound. Queuing single reads behind slices would put a wait in front
// of every tool call under layout 0, where every read is single.
//
// # The process's CPUs, at each take
//
// [runtime.GOMAXPROCS], read whenever a place is taken or given back rather
// than once when the node starts, because the runtime moves it as the
// container's CPU limit moves. A raise lets waiters in at the next take or
// return; a cut lets nobody in until the places held fall under it, and a
// query already running is never stopped for it.
//
// The zero value is a node's CPUs. A CPUs must not be copied after first use.
type CPUs struct {
	// limit is how many places there are, nil for [runtime.GOMAXPROCS] —
	// set by a test that needs a number of its own.
	limit func() int

	mu   sync.Mutex
	held int
	// clock ticks at every arrival and at every place let in, so the
	// shares' [cpuShare.since] are their order in the line.
	clock uint64
	// waiting is the shares with a query waiting, in no order: [CPUs.admit]
	// ranks them at each place it lets in.
	waiting []*cpuShare
}

// cpuShare is one request's claim on a node's [CPUs]: what it holds, where it
// stands in the line, and its queries waiting for one.
type cpuShare struct {
	cpus *CPUs

	// Under cpus.mu: held is the places its queries hold; since is where
	// it stands in the line — the tick it arrived at, or the one its last
	// query was let in at, whichever is later, the lowest at the front;
	// queue is its queries waiting, in the order they asked.
	held  int
	since uint64
	queue []chan struct{}
}

// share is the share one request's queries take their places through: a batch
// this node answers for another, or one round of a gather it answers
// in-process. A nil CPUs is a nil share, which bounds nothing.
func (c *CPUs) share() *cpuShare {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	return &cpuShare{cpus: c, since: c.clock}
}

// take takes a place for one of the request's queries, waiting no longer than
// ctx, and answers how to give it back. A nil share bounds nothing: a
// single-partition read's one query takes no place.
func (s *cpuShare) take(ctx context.Context) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	c := s.cpus
	c.mu.Lock()
	// THE WAITERS FIRST, so a raise the runtime made since the last take
	// or return lets them in ahead of this query — and a place still free
	// after them is one nobody is waiting for.
	c.admit()
	if c.held < c.places() {
		c.letIn(s)
		c.mu.Unlock()
		return s.give, nil
	}
	in := make(chan struct{})
	if len(s.queue) == 0 {
		c.waiting = append(c.waiting, s)
	}
	s.queue = append(s.queue, in)
	c.mu.Unlock()
	select {
	case <-in:
		return s.give, nil
	case <-ctx.Done():
	}
	c.mu.Lock()
	if i := slices.Index(s.queue, in); i >= 0 {
		s.queue = slices.Delete(s.queue, i, i+1)
		if len(s.queue) == 0 {
			c.waiting = slices.DeleteFunc(c.waiting, func(w *cpuShare) bool { return w == s })
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
	c.mu.Unlock()
	// LET IN AS ITS CONTEXT ENDED: the place is this query's now, and it
	// is given back to the next waiter rather than lost — a place nobody
	// returns is one CPU fewer for the life of the node.
	s.give()
	return nil, ctx.Err()
}

// give gives one of the request's places back, to the next waiter by
// [CPUs.admit]'s ranking while there is one.
func (s *cpuShare) give() {
	c := s.cpus
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held--
	s.held--
	c.admit()
}

// admit lets waiters in while there are places, under c.mu: each place to the
// waiting share holding the fewest, the one at the front of the line first
// among equals — and that share then goes to the back of it ([CPUs.letIn]).
func (c *CPUs) admit() {
	for len(c.waiting) > 0 && c.held < c.places() {
		i := 0
		for j, w := range c.waiting {
			if best := c.waiting[i]; w.held < best.held ||
				(w.held == best.held && w.since < best.since) {
				i = j
			}
		}
		s := c.waiting[i]
		close(s.queue[0])
		s.queue = slices.Delete(s.queue, 0, 1)
		if len(s.queue) == 0 {
			c.waiting = slices.Delete(c.waiting, i, i+1)
		}
		c.letIn(s)
	}
}

// letIn counts a place as s's and sends s to the back of the line, under c.mu.
func (c *CPUs) letIn(s *cpuShare) {
	c.held++
	s.held++
	c.clock++
	s.since = c.clock
}

// places is how many queries may run at once now.
func (c *CPUs) places() int {
	if c.limit != nil {
		return c.limit()
	}
	return runtime.GOMAXPROCS(0)
}
