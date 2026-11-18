package engine

import (
	"context"
	"maps"
	"sync"

	"github.com/crewlet/crewlet/internal/provision"
)

// identityCache is what each integration surface remembers about which account
// a seat credential authenticates as. Keyed on the credential: a revision that
// touched something else costs no request, and a rotated credential costs
// exactly one, since it may be a different account.
//
// ONE REQUEST PER CREDENTIAL, across callers: each surface resolves at boot from
// both a start path ([Engine.startGitLab] and siblings) and a rewire path
// ([Engine.rewireGitLab] and siblings) concurrently, so a miss is CLAIMED under
// the lock that observed it and a second caller waits on the claim. The wait is
// the contract: each call site registers seats from its own snapshot right
// after, so returning early binds the seat to nobody, and a later hit never
// reports it. It is bounded by the WAITER's context.
type identityCache[K comparable] struct {
	mu sync.Mutex

	// known is the answer, and the absence of a key is "not resolved"
	// rather than "resolved to nobody" — a failed lookup stores nothing,
	// so the next pass retries it.
	known map[K]string

	// inflight holds one channel per claimed credential, closed once its
	// claimant stored what it found: the claim AND the barrier, taken under
	// one lock, so the two can never disagree.
	inflight map[K]chan struct{}
}

// unresolvedSeatDetail is what an operator is told when one seat's credential
// could not be resolved, stated once because it describes this cache's contract
// (the seat is skipped, the next pass retries, nothing to apply). Each surface
// keeps its own log KEY (gitlab_seat_identity_unresolved and its GitHub and
// Jira twins), which is what an operator greps for.
func unresolvedSeatDetail(events string) string {
	return "this seat receives no " + events + " until a lookup succeeds; the " +
		"reconcile loop retries it on this surface's own pass, so nothing has " +
		"to be applied"
}

// resolve fills in the accounts behind any credentials not already known,
// buying each exactly once however many callers want it.
//
// CONCURRENTLY, bounded by [provision.ResolveConcurrently]: sequential is one
// timeout per seat end to end against a slow instance at boot; unbounded is a
// burst of connections to one third-party app that an abuse detector notices.
//
// lookup returns the account, or "" for a credential it could not resolve. A
// failed lookup leaves the seat unresolved rather than failing the boot (the
// instance may be briefly down; the next pass retries), costing that seat's
// inbound routing until then, which the caller reports per seat.
func (c *identityCache[K]) resolve(ctx context.Context, keys []K, lookup func(K) string) {
	c.mu.Lock()
	if c.known == nil {
		c.known = map[K]string{}
	}
	if c.inflight == nil {
		c.inflight = map[K]chan struct{}{}
	}
	var mine []K
	var waiting []chan struct{}
	for _, key := range keys {
		if _, resolved := c.known[key]; resolved {
			continue
		}
		if claimed, busy := c.inflight[key]; busy {
			// SOMEBODY ELSE'S, or this call's own if keys repeats a
			// credential: either way the answer arrives on that
			// channel and asking again would be the second request.
			waiting = append(waiting, claimed)
			continue
		}
		c.inflight[key] = make(chan struct{})
		mine = append(mine, key)
	}
	c.mu.Unlock()

	if len(mine) > 0 {
		found := make([]string, len(mine))
		provision.ResolveConcurrently(len(mine), func(i int) { found[i] = lookup(mine[i]) })

		c.mu.Lock()
		for i, key := range mine {
			if found[i] != "" {
				c.known[key] = found[i]
			}
			// RELEASED WHETHER OR NOT IT RESOLVED, and the claim
			// removed with it: a failure that kept the claim would
			// make this credential permanently unbuyable, which is
			// the one way this could be worse than the race.
			close(c.inflight[key])
			delete(c.inflight, key)
		}
		c.mu.Unlock()
	}

	for _, claimed := range waiting {
		select {
		case <-claimed:
		case <-ctx.Done():
			// THE WAITER'S OWN DEADLINE, not the holder's: a
			// cancelled caller registers nothing for that seat, as
			// a failed lookup does, rather than being held by a
			// peer against an instance that is not answering.
			return
		}
	}
}

// snapshot is a COPY of what this cache knows, readable without the lock:
// register and unresolved walk the whole org against it, and holding the mutex
// across that walk would put a resolve behind every seat in the company.
func (c *identityCache[K]) snapshot() map[K]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.known)
}
