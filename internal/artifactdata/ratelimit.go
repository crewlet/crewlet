package artifactdata

import (
	"math"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE COST-WEIGHTED WRITE LIMITS (D28): per node, per principal on a store and
// per store, token buckets weighed by what a write costs the applier rather
// than by how many records it is. A record's cost is the rows it writes, and a
// batch of a hundred ops touching a dozen fields each costs the estate's one
// writer a hundred times a checkbox's click, so it is weighed so.
//
// PER NODE, AND SAID SO: a bucket protects this node's applier queue and the
// fleet's one log from one client. It is not a fleet quota (that is the
// stamped footprint), it is not shared across nodes, and it never locks
// anybody out: a refusal names when the bucket holds the cost again.

// rowsPerUnit is how many field-index rows one unit of cost covers: a click
// that sets one or two fields of a row is one unit, and an op rewriting a
// sixty-field row is four.
const rowsPerUnit = 16

// storeBurst is a store's bucket size: one second of its sustained rate, so a
// room of people clicking at once is absorbed and a script looping on one
// sheet is not.
const storeBurst = artifacts.DataWritesPerStorePerSecond

// maxBuckets bounds the buckets one node keeps. Past it, every bucket that has
// refilled (one nobody has written through for a while) is forgotten, which
// changes no answer: a full bucket and an absent one admit alike.
const maxBuckets = 16_384

// Cost is what a batch weighs: per op, one unit per [rowsPerUnit] field rows
// it touches, the op itself counted as a row.
func Cost(ops []artifacts.DataOp) int {
	cost := 0
	for _, op := range ops {
		rows := 1
		switch op.Op {
		case artifacts.DataSet, artifacts.DataMerge:
			rows += len(op.Fields)
		case artifacts.DataDelete:
		default:
			rows++
		}
		cost += (rows + rowsPerUnit - 1) / rowsPerUnit
	}
	return cost
}

// limiter is the node's buckets.
type limiter struct {
	mu         sync.Mutex
	now        func() time.Time
	principals map[[2]string]*bucket
	stores     map[string]*bucket
}

// bucket is one token bucket: tokens held at last.
type bucket struct {
	tokens float64
	last   time.Time
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
		pb = &bucket{tokens: artifacts.DataWritesBurst, last: now}
		l.principals[[2]string{store, principal}] = pb
	}
	sb := l.stores[store]
	if sb == nil {
		sb = &bucket{tokens: storeBurst, last: now}
		l.stores[store] = sb
	}
	pb.refill(now, artifacts.DataWritesPerPrincipalPerSecond, artifacts.DataWritesBurst)
	sb.refill(now, artifacts.DataWritesPerStorePerSecond, storeBurst)
	// A WRITE HEAVIER THAN A WHOLE BUCKET weighs a whole bucket: it is
	// admitted when the bucket is full and empties it, rather than being a
	// write no wait could ever admit.
	pc := math.Min(float64(cost), artifacts.DataWritesBurst)
	sc := math.Min(float64(cost), storeBurst)
	if pb.tokens < pc {
		return &RateLimited{Scope: "principal", Cost: cost,
			RetryAfter: wait(pc-pb.tokens, artifacts.DataWritesPerPrincipalPerSecond)}
	}
	if sb.tokens < sc {
		return &RateLimited{Scope: "store", Cost: cost,
			RetryAfter: wait(sc-sb.tokens, artifacts.DataWritesPerStorePerSecond)}
	}
	pb.tokens -= pc
	sb.tokens -= sc
	return nil
}

// refill adds what the time since last earned, up to the burst.
func (b *bucket) refill(now time.Time, rate, burst float64) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*rate)
	}
	b.last = now
}

// wait is how long a deficit takes to refill at rate, rounded up to a whole
// millisecond so a Retry-After is never zero.
func wait(deficit, rate float64) time.Duration {
	d := time.Duration(math.Ceil(deficit / rate * float64(time.Second)))
	return max(d.Round(time.Millisecond), time.Millisecond)
}

// forgetFull drops every bucket that has refilled.
func (l *limiter) forgetFull(now time.Time) {
	for k, b := range l.principals {
		b.refill(now, artifacts.DataWritesPerPrincipalPerSecond, artifacts.DataWritesBurst)
		if b.tokens >= artifacts.DataWritesBurst {
			delete(l.principals, k)
		}
	}
	for k, b := range l.stores {
		b.refill(now, artifacts.DataWritesPerStorePerSecond, storeBurst)
		if b.tokens >= storeBurst {
			delete(l.stores, k)
		}
	}
}
