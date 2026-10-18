package artifactdata

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE COST-WEIGHTED WRITE LIMITS (D28): per node, per principal on a store and
// per store, token buckets weighed by what a write costs the applier rather
// than by how many records it is. A record's cost is the rows its apply
// writes, and every data node applies every record through one writer, so a
// write is weighed by those rows.
//
// THE ROWS ARE THE DOCUMENT'S, NOT THE OP'S. An apply rewrites a changed
// document's whole field index (deleted and inserted in one statement each,
// writeDoc), so a merge naming one cell of a twelve-column row writes the
// document row and every one of its index entries twice; spike DS-1 measured
// 21 rows for such a cell edit and 2,804 for a pasted block of 100 rows. The
// writer therefore reads, from this node's copy, how many index entries each
// document the batch names holds now ([Writer.cost]), and [Cost] weighs each
// op by them. A count read from a copy a little behind the log is still the
// right order of magnitude, which is all a guard needs.
//
// PER NODE, AND SAID SO: a bucket protects this node's applier queue and the
// fleet's one log from one client. It is not a fleet quota (that is the
// stamped footprint), it is not shared across nodes, and it never locks
// anybody out: a refusal names when the bucket holds the cost again.

// rowsPerUnit is how many rows one unit of cost covers: a cell edit on a
// twelve-column row (about thirty rows: the document, its index deleted and
// written again, a touch and the history row) is two units, and a pasted
// block of a hundred such rows is about two hundred.
const rowsPerUnit = 16

// maxBuckets bounds the buckets one node keeps. Past it, every bucket that has
// refilled (one nobody has written through for a while) is forgotten, which
// changes no answer: a full bucket and an absent one admit alike.
const maxBuckets = 16_384

// Cost is what a batch weighs, given held, how many field-index entries a
// named document holds now (zero for one that does not exist): the rows its
// apply writes, one unit per [rowsPerUnit] of them, at least one.
//
// Per op: the document row, its index entries deleted, the entries it holds
// after the op (an upper bound: what it held plus what the op writes, each
// array element an entry) and its touch row; once per batch, the history row.
func Cost(ops []artifacts.DataOp, held func(collection, doc string) int) int {
	rows := 1
	for _, op := range ops {
		before := held(op.Collection, op.Doc)
		rows += 2 // the document row and its touch
		switch op.Op {
		case artifacts.DataDelete:
			rows += before
		case artifacts.DataSet:
			rows += before + fieldEntries(op.Fields)
		case artifacts.DataMerge:
			rows += 2*before + fieldEntries(op.Fields)
		default:
			// increment, append, add, remove: one field changes, and the
			// whole index is written again.
			rows += 2*before + 1
		}
	}
	return (rows + rowsPerUnit - 1) / rowsPerUnit
}

// fieldEntries is how many index entries the fields an op writes take at most:
// one per value, and one per element of a list.
func fieldEntries(fields map[string]json.RawMessage) int {
	n := 0
	for _, raw := range fields {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			n += len(list)
			continue
		}
		n++
	}
	return n
}

// limiter is the node's buckets.
type limiter struct {
	mu         sync.Mutex
	now        func() time.Time
	principals map[[2]string]*bucket
	stores     map[string]*bucket
}

// bucket is one token bucket: tokens held at last, earned at rate up to
// burst. Each bucket carries its own rate and burst, so the principal's and
// the store's buckets are told apart by what they hold, never by the caller.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

// newBucket is a full bucket at now.
func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{tokens: burst, rate: rate, burst: burst, last: now}
}

// newLimiter builds the node's buckets on a clock.
func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, principals: map[[2]string]*bucket{}, stores: map[string]*bucket{}}
}

// take charges cost to the principal's bucket on the store and the store's own,
// both or neither, and answers the refusal naming the bucket that could not
// pay and when it can.
func (l *limiter) take(store, principal string, cost int) *RateLimited {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.principals)+len(l.stores) > maxBuckets {
		l.forgetFull(now)
	}
	pb := l.principals[[2]string{store, principal}]
	if pb == nil {
		pb = newBucket(artifacts.DataWritesPerPrincipalPerSecond, artifacts.DataWritesBurst, now)
		l.principals[[2]string{store, principal}] = pb
	}
	sb := l.stores[store]
	if sb == nil {
		sb = newBucket(artifacts.DataWritesPerStorePerSecond, artifacts.DataWritesStoreBurst, now)
		l.stores[store] = sb
	}
	pb.refill(now)
	sb.refill(now)
	// A WRITE HEAVIER THAN A WHOLE BUCKET weighs a whole bucket: it is
	// admitted when the bucket is full and empties it, rather than being a
	// write no wait could ever admit.
	pc, sc := pb.weigh(cost), sb.weigh(cost)
	if pb.tokens < pc {
		return &RateLimited{Scope: "principal", Cost: cost, RetryAfter: pb.wait(pc)}
	}
	if sb.tokens < sc {
		return &RateLimited{Scope: "store", Cost: cost, RetryAfter: sb.wait(sc)}
	}
	pb.tokens -= pc
	sb.tokens -= sc
	return nil
}

// refill adds what the time since last earned, up to the burst.
func (b *bucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
}

// weigh is what cost takes from the bucket: the cost, at most a whole bucket.
func (b *bucket) weigh(cost int) float64 { return math.Min(float64(cost), b.burst) }

// full reports a bucket that has refilled, which admits exactly as an absent
// one does.
func (b *bucket) full() bool { return b.tokens >= b.burst }

// wait is how long the bucket takes to hold need again, rounded up to a whole
// millisecond so a Retry-After is never zero.
func (b *bucket) wait(need float64) time.Duration {
	d := time.Duration(math.Ceil((need - b.tokens) / b.rate * float64(time.Second)))
	return max(d.Round(time.Millisecond), time.Millisecond)
}

// forgetFull drops every bucket that has refilled.
func (l *limiter) forgetFull(now time.Time) {
	for k, b := range l.principals {
		b.refill(now)
		if b.full() {
			delete(l.principals, k)
		}
	}
	for k, b := range l.stores {
		b.refill(now)
		if b.full() {
			delete(l.stores, k)
		}
	}
}
