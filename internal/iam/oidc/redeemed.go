package oidc

import (
	"container/list"
	"sync"
	"time"
)

// A FLIGHT IS EXCHANGED AT THE PROVIDER ONCE.
//
// The flight cookie is valid for [FlightTTL] and the browser is told to drop it
// at the callback — but a caller who STARTED a flight holds its cookie, its
// state and a made-up code, and could present the three as often as it liked
// for ten minutes, each time making this node exchange a code at the provider.
// The provider refuses every one; what it costs is an outbound request per
// inbound request, aimed at somebody else's token endpoint by whoever can reach
// this one. So the node that finishes a flight remembers its [Flight.ID], and
// a flight whose id it has seen is refused before anything reaches the
// provider.
//
// # Per node, bounded, and forgetting nothing that still matters
//
// PER NODE, because the flight is presented to whichever node the balancer
// picks and a fleet-wide record would be a coordination write on a path an
// unauthenticated caller drives: a stockpiled cookie is then good for one
// exchange per node rather than one per request, which is what the defence is
// for. An entry is kept until its flight EXPIRES, after which [Open] refuses the
// flight anyway, and the set is bounded at [MaxRedemptions] — so what a caller
// can do by flooding it is evict an id early, which buys one more exchange for
// every [MaxRedemptions] flights it started and called back first. A fresh
// flight costs less than that, so nothing is lost.

// MaxRedemptions is how many redeemed flights one node remembers.
//
// 8192, sized so a real sign-in wave never evicts a flight that is still
// live: at the eight exchanges a provider admits at once and a hundred
// milliseconds each, a node finishes at most 80 flights a second, and the
// design's 3,000-person wave is over in under a minute — well inside one
// [FlightTTL], and under half the set. An entry is an id, an expiry and a
// list element, so the whole set is about a megabyte at its fullest.
const MaxRedemptions = 8192

// Redemptions remembers the flights this node has redeemed.
//
// SAFE FOR CONCURRENT USE. The zero value is not usable; build one with
// [NewRedemptions].
type Redemptions struct {
	mu    sync.Mutex
	max   int
	ids   map[string]*list.Element
	order *list.List // of redemption, oldest first
}

// redemption is one remembered flight.
type redemption struct {
	id      string
	expires time.Time
}

// NewRedemptions builds an empty set holding at most [MaxRedemptions].
func NewRedemptions() *Redemptions {
	return newRedemptions(MaxRedemptions)
}

func newRedemptions(max int) *Redemptions {
	return &Redemptions{max: max, ids: map[string]*list.Element{},
		order: list.New()}
}

// Redeem records a flight as redeemed, reporting false when it already was —
// the replay the caller refuses. Call it after [Open] has accepted the flight
// and before the provider is asked anything.
func (r *Redemptions) Redeem(f Flight, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if held, seen := r.ids[f.ID]; seen {
		if now.Before(held.Value.(*redemption).expires) {
			return false
		}
		// EXPIRED, so [Open] would have refused this flight already; a
		// flight with the id reaching here is a different flight.
		r.drop(held)
	}
	// EXPIRED ENTRIES GO FIRST, from the oldest: nothing depends on them.
	for front := r.order.Front(); front != nil &&
		!now.Before(front.Value.(*redemption).expires); front = r.order.Front() {
		r.drop(front)
	}
	// AND AT THE BOUND, THE OLDEST LIVE ONE — see the type's doc for why an
	// early eviction costs nothing a fresh flight would not.
	for r.order.Len() >= r.max {
		r.drop(r.order.Front())
	}
	r.ids[f.ID] = r.order.PushBack(&redemption{id: f.ID, expires: f.ExpiresAt})
	return true
}

// drop forgets one entry. The caller holds the lock.
func (r *Redemptions) drop(e *list.Element) {
	delete(r.ids, e.Value.(*redemption).id)
	r.order.Remove(e)
}
