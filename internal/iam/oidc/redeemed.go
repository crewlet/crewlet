package oidc

import (
	"container/list"
	"crypto/sha256"
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
// this one. So the node that finishes a flight remembers it, and a flight it
// has seen is refused before anything reaches the provider.
//
// # Asked twice: before the turn, and spent inside it
//
// A flight is SPENT ([Redemptions.Redeem]) only once it holds its turn at the
// token endpoint ([Provider.Admit]), because a flight spent before the wait was
// spent on a request that may never reach the provider — a browser that left
// mid-wave, whose reload was then refused as a replay. And it is ASKED
// ([Redemptions.Seen]) before the wait as well, because a flight this node has
// already finished has nothing to wait for: waiting, it took one of the
// provider's [ExchangeSlots] to learn it would be refused, and whoever
// stockpiled a spent cookie could keep every slot queued with presentations
// that ask the provider nothing, shutting out every sign-in and every
// deactivation probe on the node. The first question keeps a known replay out
// of the queue; the second is what settles two first presentations racing for
// the turn to one exchange, since both pass the first.
//
// # Named by its verifier, which every flight has always carried
//
// A flight is remembered by a digest of its PKCE [Flight.Verifier]: thirty-two
// random bytes, sealed, and never in a URL — the authorization request carries
// only its S256 challenge, and the digest here is taken under a label of its
// own so it is not that challenge either. A field minted for the purpose would
// be a field a flight sealed by the build before it does not carry, and during
// a rolling upgrade every sign-in whose two halves reached different builds
// would have been refused for lacking it; the verifier is in every flight any
// build has sealed. A DIGEST and not the verifier itself, so the set never
// holds a secret past the request that presented it.
//
// # Per node, bounded, and forgetting nothing that still matters
//
// PER NODE, because the flight is presented to whichever node the balancer
// picks and a fleet-wide record would be a coordination write on a path an
// unauthenticated caller drives: a stockpiled cookie is then good for one
// exchange per node rather than one per request, which is what the defence is
// for. An entry is kept until its flight EXPIRES, after which [Open] refuses the
// flight anyway, and the set is bounded at [MaxRedemptions] — so what a caller
// can do by flooding it is evict an entry early, which buys one more exchange
// for every [MaxRedemptions] flights it started and called back first. A fresh
// flight costs less than that, so nothing is lost.

// MaxRedemptions is how many redeemed flights one node remembers.
//
// 8192, sized so a real sign-in wave never evicts a flight that is still
// live: at the eight exchanges a provider admits at once and a hundred
// milliseconds each, a node finishes at most 80 flights a second, and the
// design's 3,000-person wave is over in under a minute — well inside one
// [FlightTTL], and under half the set. An entry is a 32-byte digest, an expiry
// and a list element, so the whole set is about a megabyte at its fullest.
const MaxRedemptions = 8192

// Redemptions remembers the flights this node has redeemed.
//
// SAFE FOR CONCURRENT USE. The zero value is not usable; build one with
// [NewRedemptions].
type Redemptions struct {
	mu      sync.Mutex
	max     int
	flights map[flightName]*list.Element
	order   *list.List // of redemption, oldest first
}

// flightName is what a flight is remembered by — a digest of its verifier, for
// the reasons at the top of this file.
type flightName [sha256.Size]byte

// nameOf is a flight's [flightName], the SHA-256 of its verifier under this
// purpose's own label — which is what keeps it from being the S256 challenge,
// the SHA-256 of the verifier alone.
func nameOf(f Flight) flightName {
	return sha256.Sum256([]byte(redemptionLabel + f.Verifier))
}

// redemptionLabel separates a flight's name from every other digest of its
// verifier — the S256 challenge above all, which is the digest of the verifier
// alone and has travelled in the authorization request's URL.
const redemptionLabel = "crewlet/oidc/redemption\x00"

// redemption is one remembered flight.
type redemption struct {
	name    flightName
	expires time.Time
}

// NewRedemptions builds an empty set holding at most [MaxRedemptions].
func NewRedemptions() *Redemptions {
	return newRedemptions(MaxRedemptions)
}

func newRedemptions(max int) *Redemptions {
	return &Redemptions{max: max, flights: map[flightName]*list.Element{},
		order: list.New()}
}

// Seen reports whether a flight is remembered as redeemed and has not yet
// expired — the replay the caller refuses — and records nothing. Ask it after
// [Open] has accepted the flight and before waiting for a turn at the token
// endpoint, so a known replay never queues for one; a flight it answers false
// for is still spent only by [Redemptions.Redeem], inside the turn.
func (r *Redemptions) Seen(f Flight, now time.Time) bool {
	name := nameOf(f)
	r.mu.Lock()
	defer r.mu.Unlock()
	held, seen := r.flights[name]
	// EXPIRED, it is a different flight of the same name — [Redemptions.Redeem]
	// makes way for it — so it is not a replay.
	return seen && now.Before(held.Value.(*redemption).expires)
}

// Redeem records a flight as redeemed, reporting false when it already was —
// the replay the caller refuses. Call it after [Open] has accepted the flight,
// once its turn at the token endpoint is held ([Provider.Admit]) and before
// the provider is asked anything — and give the turn back before refusing,
// since a refusal waits out its pad and the turn is one of the provider's
// [ExchangeSlots].
func (r *Redemptions) Redeem(f Flight, now time.Time) bool {
	name := nameOf(f)
	r.mu.Lock()
	defer r.mu.Unlock()
	if held, seen := r.flights[name]; seen {
		if now.Before(held.Value.(*redemption).expires) {
			return false
		}
		// EXPIRED, so [Open] would have refused this flight already; a
		// flight of that name reaching here is a different flight.
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
	r.flights[name] = r.order.PushBack(&redemption{name: name, expires: f.ExpiresAt})
	return true
}

// drop forgets one entry. The caller holds the lock.
func (r *Redemptions) drop(e *list.Element) {
	delete(r.flights, e.Value.(*redemption).name)
	r.order.Remove(e)
}
