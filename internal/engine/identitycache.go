package engine

import (
	"context"
	"maps"
	"sync"

	"github.com/crewlet/crewlet/internal/provision"
)

// identityCache is what each integration surface remembers about which account
// a seat credential authenticates as, and it is ONE implementation because the
// three that had it privately had also drifted into carrying one bug each.
//
// # Keyed on the credential, which is what makes an apply free
//
// Identity is a function of the credential, credentials change rarely, and a
// config revision that touched something else must not spend one request per
// seat to re-learn what it already knows. A rotated credential is a cache miss
// and costs exactly one request, which is correct: it may well be a different
// account.
//
// # ONE REQUEST, and that is a promise about CALLERS and not just about time
//
// Every surface resolves from two places and both run at boot: a start path
// ([Engine.startGitLab] and its siblings, reached from
// [Engine.startNotifications]) and a rewire path ([Engine.rewireGitLab] and
// its siblings, reached from the integration reconcile pass, which
// [Engine.startIntegrations] arms twenty-six lines earlier). The two were
// measured about 11 ms apart on one boot.
//
// Written as "read the map under the lock, release it around the network
// lookup, re-lock to store", that is two callers both missing on one
// credential and both buying it — the promise above broken by the ordinary
// case rather than by an exotic one. So a MISS IS CLAIMED under the same lock
// that observed it, and a caller that finds another's claim outstanding waits
// for it instead of issuing a second request.
//
// # And a caller that waited has the answer before it returns
//
// Not a courtesy: every one of the six call sites registers the seat from THIS
// CALL'S OWN snapshot, a few lines on — immediately on the three start paths,
// and past [Engine.registryOf] on the three rewire paths — with nothing
// re-resolving in between. A caller that returned while a peer's lookup was
// still in flight would bind that seat to nobody, and nothing later would
// report it, because by then the cache is populated and the next resolve is a
// hit. So the wait is part of the contract, bounded by the WAITER's own
// context so a cancelled caller is not held by a peer's slow instance.
type identityCache[K comparable] struct {
	mu sync.Mutex

	// known is the answer, and the absence of a key is "not resolved"
	// rather than "resolved to nobody" — a failed lookup stores nothing,
	// so the next pass retries it.
	known map[K]string

	// inflight holds one channel per credential a caller has claimed,
	// closed when that caller has stored what it found. It is the claim
	// AND the barrier: a second caller takes the channel under the lock
	// and waits on it after its own lookups, so the two facts can never
	// disagree.
	inflight map[K]chan struct{}
}

// unresolvedSeatDetail is what an operator is told when one seat's credential
// could not be resolved, in the one place that decides it.
//
// The surfaces keep their own log KEY — gitlab_seat_identity_unresolved and
// its GitHub and Jira twins, which is how an operator greps for the one that
// is failing — but the sentence behind it is a statement about this cache's
// own contract (the seat is skipped, the next pass retries, nothing has to be
// applied), and it was the same sentence written three times, differing only
// in the word for what the seat stops receiving. That is the shape
// [internal/api/httpjson] records the cost of: byte-identical copies that had
// already drifted into three spellings of one message.
func unresolvedSeatDetail(events string) string {
	return "this seat receives no " + events + " until a lookup succeeds; the " +
		"reconcile loop retries it on this surface's own pass, so nothing has " +
		"to be applied"
}

// resolve fills in the accounts behind any credentials not already known,
// buying each exactly once however many callers want it.
//
// CONCURRENTLY, bounded by [provision.ResolveConcurrently]. Sequentially this
// is one round trip per seat on the boot path, which on a company of thirty
// seats against a slow instance is thirty timeouts end to end; unbounded it is
// thirty simultaneous connections to one third-party app, which is the shape
// an abuse detector is built to notice.
//
// lookup returns the account, or "" for a credential it could not resolve.
// A seat whose lookup FAILS is left unresolved rather than failing the boot:
// the instance may be briefly down, and the next pass retries. What that costs
// is that seat's inbound routing until then, which is the honest consequence
// and is reported per seat by the caller.
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
			// THE WAITER'S OWN DEADLINE, not the holder's. A
			// cancelled caller registers nothing for that seat,
			// exactly as a failed lookup does, rather than being
			// held by a peer against an instance that is not
			// answering.
			return
		}
	}
}

// snapshot is what this cache knows, as a value the caller can read without
// the lock.
//
// A COPY, because register and unresolved walk the whole org against it and
// holding the mutex across that walk would put a resolve behind every seat in
// the company.
func (c *identityCache[K]) snapshot() map[K]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.known)
}
