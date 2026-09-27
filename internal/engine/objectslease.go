package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/seat"
)

// The object store's membership lease: `objects:{node}` (coord.ClassObjects).
//
// # Why the store claims it, and not the seat host
//
// Membership in the object store used to be read off the node's PRESENCE, and
// presence is the seat host's: a shutdown drain gives it back at its very first
// step, while this node is still serving every chunk it holds. A drain longer
// than the placement map's grace therefore moved the node's whole share of the
// company's files to the other members — and moved it back when the node
// returned — for a node that never stopped answering. So the store claims its
// own lease, renews it on its own loop, and gives it back only once its chunk
// server has been withdrawn ([Engine.stopObjects]).
//
// # What it carries, and why every beat
//
// Presence carried a weight the node was CONFIGURED with and nothing about
// whether the volume under it still worked, so a data node whose objects
// directory had failed stayed placed on for ever and every write sent it a
// copy it could not keep. The lease carries the store's own account of its
// health, measured on every beat (disk.Store.Probe), and what its passes last
// found — the repair the map's split waits on, the scrub, the strays an
// operator waits on before stopping it — so the maintainer can take a failed
// store out of the map and an operator can see why. Re-sent on EVERY renew,
// as presence's profile is, because it describes the live process.

// objectsLease keeps this node's object-store membership claimed.
type objectsLease struct {
	leases   coord.Backend
	resource string
	node     string
	owner    string
	ttl      time.Duration
	interval time.Duration

	// meta is what the lease says this beat, built fresh each time.
	meta func() objstore.ObjectsMeta

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// held is the lease as last granted, nil until a claim lands.
	held *coord.Lease
	// refused and failing are whether the last beat was refused by
	// another owner, or went unanswered — kept so each is logged once
	// when it starts and once when it ends, rather than every beat.
	refused, failing bool
}

// startObjectsLease claims the lease at once and renews it every interval
// until stopped. The interval is the seat heartbeat's ratio of the TTL IN
// FORCE — a lease renewed on the shipped interval against an adopted, shorter
// TTL would lapse between beats.
func startObjectsLease(ctx context.Context, leases coord.Backend, node, owner string,
	ttl time.Duration, meta func() objstore.ObjectsMeta) *objectsLease {

	l := &objectsLease{
		leases: leases, resource: coord.ObjectsResource(node), node: node,
		owner: owner, ttl: ttl, interval: objectsLeaseInterval(ttl), meta: meta,
		done: make(chan struct{}),
	}
	// DETACHED, like every loop the engine runs: the lease must outlive the
	// boot's context and stop only when the chunk server has.
	ctx, l.cancel = context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(l.done)
		for {
			l.beat(ctx)
			wait := time.NewTimer(l.interval)
			select {
			case <-ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
		}
	}()
	return l
}

// beat claims or renews the lease with this beat's account of the store.
//
// A CLAIM RATHER THAN A RENEW, for presence's reason: a claim by the holder
// doubles as a renew and keeps the epoch, it re-establishes the lease after a
// lapse, and it is the only call that carries Meta — a renew would leave the
// health a beat old for ever.
//
// UNGATED: membership is not work, and a newer-protocol data node refused its
// lease during a rolling upgrade reads to the map as absent — see
// coord.AcquireOptions.Ungated.
func (l *objectsLease) beat(ctx context.Context) {
	// THE ACCOUNT FIRST, outside the claim's deadline: it probes the disk,
	// and a slow volume must not spend the budget of the write that says so.
	meta := l.meta().Encode()
	ctx, cancel := context.WithTimeout(ctx, l.interval)
	defer cancel()
	lease, err := l.leases.TryAcquire(ctx, l.resource, coord.AcquireOptions{
		Owner: l.owner, TTL: l.ttl, Preferred: l.node, Ungated: true, Meta: meta,
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err != nil:
		// UNKNOWN, NOT LOST: the store did not answer, and the lease is
		// probably still held. The next beat asks again.
		if !l.failing {
			log.WarnContext(ctx, "objects_lease_unavailable", "node", l.node,
				"error", err.Error(),
				"detail", "this node's object-store membership could not be "+
					"renewed; it lapses after the lease TTL if the store stays "+
					"unreachable, and the map then counts this node absent")
		}
		l.failing = true
	case lease == nil:
		if !l.refused {
			log.WarnContext(ctx, "objects_lease_refused", "node", l.node,
				"hint", "another process holds this node id's object-store "+
					"membership — a previous incarnation whose lease has not "+
					"lapsed yet, which clears within the lease TTL, or a second "+
					"process running under the same node id, which does not")
		}
		l.refused, l.failing = true, false
	default:
		if l.refused || l.failing {
			log.InfoContext(ctx, "objects_lease_claimed", "node", l.node,
				"epoch", lease.Epoch)
		}
		l.held, l.refused, l.failing = lease, false, false
	}
}

// stop ends the loop, waiting for a beat in flight, and gives the lease back
// — so the map sees this node gone at once rather than a TTL later. The caller
// has already withdrawn the chunk server: a member that has stopped answering
// must not be counted present.
func (l *objectsLease) stop(ctx context.Context) {
	l.cancel()
	<-l.done
	l.mu.Lock()
	held := l.held
	l.held = nil
	l.mu.Unlock()
	if held == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.interval)
	defer cancel()
	if _, err := l.leases.Release(ctx, held.Resource, l.owner, held.Epoch); err != nil {
		log.WarnContext(ctx, "objects_lease_not_released", "node", l.node,
			"error", err.Error(),
			"detail", "the map counts this node present until the lease lapses")
	}
}

// objectsLeaseInterval is how often the lease is renewed: the seat
// heartbeat's ratio of the TTL, so it takes the same number of missed beats to
// lapse as a seat does.
func objectsLeaseInterval(ttl time.Duration) time.Duration {
	return ttl / seat.HeartbeatRatio
}
