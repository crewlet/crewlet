// Package costlimit is the engine's one cost-weighted limiter: two token
// buckets per charge, one for a principal on a scope and one for the scope
// itself, both paid or neither.
//
// # Why one limiter for two surfaces
//
// The artifact datastore weighs a write by the rows its apply writes (D28),
// and a dashboard weighs a read of a live engine source by the domain read it
// costs. Both protect THIS NODE from one client, and both need the
// same two answers: one principal must not be able to take the whole scope,
// and everybody together must not be able to take more than the node gives
// the scope. A second token bucket written beside the first would be the
// place the two drift (a refill rule, a forgetting bound, a rounded wait), so
// the buckets live here and each surface names its own rates.
//
// The code host's re-syncs at the code log's head are one more surface of a
// git node (internal/code's NewResyncs): ONE limiter a node, which its coding
// boxes (a run on its seat), its checks' boxes (an attempt on its
// repository's checks) and its seats' code tools (a turn on its seat) all
// draw on — so a seat's boxes and its tools pay one seat bucket — each
// weighing a read through the log's barrier at one, at the census's seat term.
//
// # Per node, and said so
//
// A bucket protects this node's own work. It is not a fleet quota, it is not
// shared across nodes, and it never locks anybody out: a refusal names when
// the bucket holds the cost again.
package costlimit

import (
	"math"
	"sync"
	"time"
)

// MaxBuckets bounds the buckets one limiter keeps. Past it, every bucket that
// has refilled (one nobody has charged for a while) is forgotten, which
// changes no answer: a full bucket and an absent one admit alike.
const MaxBuckets = 16_384

// Rates are a limiter's two buckets: what each earns a second and how much it
// holds.
type Rates struct {
	// PrincipalRate and PrincipalBurst are one principal's bucket on one
	// scope.
	PrincipalRate, PrincipalBurst float64

	// ScopeRate and ScopeBurst are the scope's own bucket, which every
	// principal on it pays into.
	ScopeRate, ScopeBurst float64
}

// valid reports whether every rate and burst is positive: a zero rate never
// refills, which is a lockout rather than a limit.
func (r Rates) valid() bool {
	return r.PrincipalRate > 0 && r.PrincipalBurst > 0 && r.ScopeRate > 0 && r.ScopeBurst > 0
}

// Scope says which bucket refused a charge.
type Scope string

const (
	// ScopePrincipal is the caller's own bucket on the scope.
	ScopePrincipal Scope = "principal"

	// ScopeShared is the scope's bucket, which everybody on it pays into.
	ScopeShared Scope = "scope"
)

// Refusal is a charge neither bucket paid, and when the refusing one holds it
// again.
type Refusal struct {
	// Scope says which bucket could not pay.
	Scope Scope

	// RetryAfter is when that bucket holds the cost again, never zero.
	RetryAfter time.Duration
}

// Limiter is one node's buckets for one surface.
type Limiter struct {
	rates Rates
	now   func() time.Time

	mu         sync.Mutex
	principals map[[2]string]*bucket
	scopes     map[string]*bucket
}

// New builds a limiter charging at rates on a clock (nil is the wall clock).
// It panics on a rate or a burst that is not positive, at wiring time, since
// such a bucket refuses every charge for ever.
func New(rates Rates, now func() time.Time) *Limiter {
	if !rates.valid() {
		panic("costlimit: every rate and burst must be positive; a zero bucket never refills")
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{rates: rates, now: now, principals: map[[2]string]*bucket{},
		scopes: map[string]*bucket{}}
}

// Take charges cost to principal's bucket on scope and to scope's own, both or
// neither, and answers the refusal naming the bucket that could not pay. A
// charge heavier than a whole bucket weighs a whole bucket: it is admitted
// when the bucket is full and empties it, rather than being a charge no wait
// could ever admit.
func (l *Limiter) Take(scope, principal string, cost int) *Refusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.principals)+len(l.scopes) > MaxBuckets {
		l.forgetFull(now)
	}
	key := [2]string{scope, principal}
	pb := l.principals[key]
	if pb == nil {
		pb = newBucket(l.rates.PrincipalRate, l.rates.PrincipalBurst, now)
		l.principals[key] = pb
	}
	sb := l.scopes[scope]
	if sb == nil {
		sb = newBucket(l.rates.ScopeRate, l.rates.ScopeBurst, now)
		l.scopes[scope] = sb
	}
	pb.refill(now)
	sb.refill(now)
	pc, sc := pb.weigh(cost), sb.weigh(cost)
	if pb.tokens < pc {
		return &Refusal{Scope: ScopePrincipal, RetryAfter: pb.wait(pc)}
	}
	if sb.tokens < sc {
		return &Refusal{Scope: ScopeShared, RetryAfter: sb.wait(sc)}
	}
	pb.tokens -= pc
	sb.tokens -= sc
	return nil
}

// bucket is one token bucket: tokens held at last, earned at rate up to
// burst.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

// newBucket is a full bucket at now.
func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{tokens: burst, rate: rate, burst: burst, last: now}
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
func (l *Limiter) forgetFull(now time.Time) {
	for k, b := range l.principals {
		b.refill(now)
		if b.full() {
			delete(l.principals, k)
		}
	}
	for k, b := range l.scopes {
		b.refill(now)
		if b.full() {
			delete(l.scopes, k)
		}
	}
}
