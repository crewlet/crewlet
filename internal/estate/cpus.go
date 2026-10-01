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
// in-process ([Router]). A node's [LocalBackends] hand both the same one
// ([LocalBackends.CPUs]).
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
// # First come, first served
//
// A query waits behind every query that asked before it, and a place given
// back goes to the longest waiter rather than to whichever wakes first — so a
// batch of a hundred and fifty partitions cannot keep a later batch's few
// waiting until all of its own have run.
//
// The zero value is a node's CPUs. A CPUs must not be copied after first use.
type CPUs struct {
	// limit is how many places there are, nil for [runtime.GOMAXPROCS] —
	// set by a test that needs a number of its own.
	limit func() int

	mu      sync.Mutex
	held    int
	waiting []chan struct{}
}

// take takes a place for one query, waiting no longer than ctx, and answers
// how to give it back. A nil CPUs bounds nothing: a single-partition read's
// one query takes no place.
func (c *CPUs) take(ctx context.Context) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	c.mu.Lock()
	c.admit()
	if len(c.waiting) == 0 && c.held < c.places() {
		c.held++
		c.mu.Unlock()
		return c.give, nil
	}
	in := make(chan struct{})
	c.waiting = append(c.waiting, in)
	c.mu.Unlock()
	select {
	case <-in:
		return c.give, nil
	case <-ctx.Done():
	}
	c.mu.Lock()
	if i := slices.Index(c.waiting, in); i >= 0 {
		c.waiting = slices.Delete(c.waiting, i, i+1)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
	c.mu.Unlock()
	// LET IN AS ITS CONTEXT ENDED: the place is this query's now, and it
	// is given back to the next waiter rather than lost — a place nobody
	// returns is one CPU fewer for the life of the node.
	c.give()
	return nil, ctx.Err()
}

// give gives a place back, to the longest waiter while there is one.
func (c *CPUs) give() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held--
	c.admit()
}

// admit lets the longest waiters in while there are places, under c.mu.
func (c *CPUs) admit() {
	for len(c.waiting) > 0 && c.held < c.places() {
		close(c.waiting[0])
		c.waiting = slices.Delete(c.waiting, 0, 1)
		c.held++
	}
}

// places is how many queries may run at once now.
func (c *CPUs) places() int {
	if c.limit != nil {
		return c.limit()
	}
	return runtime.GOMAXPROCS(0)
}
