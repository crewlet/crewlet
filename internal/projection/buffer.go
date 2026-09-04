package projection

import (
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("projection")

// buffer is the drain's handoff to the writer: a FIFO of changes bounded by
// the bytes they hold.
//
// # Why the drain and the writer are separate at all
//
// The coordination watch delivers on the read loop of this node's single NATS
// connection. A consumer that blocks — and a store transaction blocks, for
// milliseconds at a time — stops that loop, and with it every seat's mailbox
// and every coordination read in the process. So one goroutine takes changes
// off the watch and touches nothing else, and another does the writing.
//
// # Why the bound is bytes
//
// Entries say nothing about memory here: a change carries a document, and a
// page body is capped at 512 KiB while a status flip is a few hundred. A
// thousand-entry buffer is 100 KB or half a gigabyte depending on what
// happens to be flowing through it, and only one of those is a bug.
//
// # Why overflow is a rebuild and not a wait
//
// Waiting is what the split above exists to prevent — back-pressure here
// reaches the NATS read loop, which is the outage this design is shaped
// around. So a full buffer DROPS, records that it dropped, and the projector
// treats a non-zero drop count as "this projection has missed writes it
// cannot name" and re-runs the reconcile. A dropped change with no rebuild
// would be an item silently missing from a board.
type buffer struct {
	mu       sync.Mutex
	notEmpty *sync.Cond

	items   []*coord.Change
	bytes   int
	limit   int
	dropped uint64
	closed  bool
}

func newBuffer(limit int) *buffer {
	b := &buffer{limit: limit}
	b.notEmpty = sync.NewCond(&b.mu)
	return b
}

// changeBytes is what one change costs, counting the value plus a fixed
// allowance for the key and the struct.
//
// The allowance is deliberate rather than precise: a family whose changes are
// all tiny would otherwise let the buffer hold millions of entries and spend
// its memory on slice headers the byte count never saw.
const changeOverhead = 256

func changeBytes(c *coord.Change) int {
	if c == nil {
		return changeOverhead
	}
	return len(c.Value) + len(c.Key) + changeOverhead
}

// Push adds a change, reporting whether it fitted.
//
// False is a DROP, and the caller does not retry: the buffer is full because
// the writer is behind, and a retry loop here is the back-pressure this whole
// arrangement exists to keep off the NATS read loop.
func (b *buffer) Push(c *coord.Change) bool {
	size := changeBytes(c)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	// A single change larger than the whole limit is admitted anyway, on an
	// empty buffer. Refusing it would drop every oversized record for ever
	// and rebuild in a loop that could never converge — and the writer will
	// take it straight back out.
	if b.bytes+size > b.limit && len(b.items) > 0 {
		b.dropped++
		return false
	}
	b.items = append(b.items, c)
	b.bytes += size
	b.notEmpty.Signal()
	return true
}

// Take removes up to max changes, blocking until at least one is there or the
// buffer closes. A nil slice means closed.
func (b *buffer) Take(max int) []*coord.Change {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.items) == 0 && !b.closed {
		b.notEmpty.Wait()
	}
	if len(b.items) == 0 {
		return nil
	}
	if max <= 0 || max > len(b.items) {
		max = len(b.items)
	}
	out := b.items[:max:max]
	rest := b.items[max:]
	b.items = append(make([]*coord.Change, 0, len(rest)), rest...)
	for _, c := range out {
		b.bytes -= changeBytes(c)
	}
	if b.bytes < 0 {
		b.bytes = 0
	}
	return out
}

// TryTake removes up to max changes without blocking.
func (b *buffer) TryTake(max int) []*coord.Change {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return nil
	}
	if max <= 0 || max > len(b.items) {
		max = len(b.items)
	}
	out := b.items[:max:max]
	rest := b.items[max:]
	b.items = append(make([]*coord.Change, 0, len(rest)), rest...)
	for _, c := range out {
		b.bytes -= changeBytes(c)
	}
	if b.bytes < 0 {
		b.bytes = 0
	}
	return out
}

// Close wakes every waiter and refuses further pushes.
func (b *buffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.notEmpty.Broadcast()
}

// Reset empties the buffer and clears the drop count, for a rebuild.
func (b *buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = nil
	b.bytes = 0
	b.dropped = 0
	b.closed = false
}

// Len is how many changes are waiting.
func (b *buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// Dropped is how many changes did not fit. NON-ZERO IS A REBUILD.
func (b *buffer) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}
