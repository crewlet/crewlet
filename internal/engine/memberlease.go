package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat"
)

// A MEMBERSHIP LEASE: a data node's claim to be a member of one of the fleet's
// placement maps — the object store's `objects:{node}` (objectslease.go) and
// the estate map's `estate:{node}` (estatelease.go).
//
// # One loop for both maps
//
// Both maps read who their members are off a lease of their own class rather
// than off presence, for one reason: presence is the seat host's, and a drain
// gives it back at its very first step while the node still serves what it
// holds (ADR-0020). So each subsystem claims its own lease, renews it on a
// loop of its own, re-sends what it says on every beat, and gives it back only
// once it has stopped serving. That is ONE rule — claimed ungated, a claim
// rather than a renew on every beat, an unanswered beat read as unknown rather
// than lost, a peer's hold left alone, released at the epoch this process was
// granted — and written twice it would drift (ADR-0008): the object store's
// copy was already the only place the estate's could be learned from.
//
// What differs between the maps is what the lease SAYS, which each builds
// fresh for every beat, and the words its log lines use.

// memberLease keeps one of this node's membership leases claimed.
type memberLease struct {
	leases   coord.Backend
	resource string
	node     string
	owner    string
	ttl      time.Duration
	interval time.Duration

	// what names the membership in this lease's log lines — "object-store
	// membership", "estate membership" — and event is their prefix.
	what, event string

	// meta is what the lease says this beat, built fresh each time, or why
	// it cannot be said.
	meta func() (map[string]any, error)

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// held is the lease as last granted, nil until a claim lands.
	held *coord.Lease
	// refused, failing and unsaid are whether the last beat was refused by
	// another owner, went unanswered, or had nothing it could say — kept so
	// each is logged once when it starts and once when it ends, rather than
	// every beat.
	refused, failing, unsaid bool
}

// memberLeaseSpec is what distinguishes one membership lease from another.
type memberLeaseSpec struct {
	// resource is the lease's name, node the node it names, owner this
	// process's incarnation and ttl the lease TTL in force.
	resource, node, owner string
	ttl                   time.Duration

	// what and event name the membership in its log lines.
	what, event string

	// meta builds what the lease says, fresh for every beat.
	meta func() (map[string]any, error)
}

// startMemberLease claims the lease at once and renews it every interval until
// stopped. The interval is the seat heartbeat's ratio of the TTL IN FORCE — a
// lease renewed on the shipped interval against an adopted, shorter TTL would
// lapse between beats.
func startMemberLease(ctx context.Context, leases coord.Backend, spec memberLeaseSpec) *memberLease {
	l := &memberLease{
		leases: leases, resource: spec.resource, node: spec.node, owner: spec.owner,
		ttl: spec.ttl, interval: memberLeaseInterval(spec.ttl), what: spec.what,
		event: spec.event, meta: spec.meta, done: make(chan struct{}),
	}
	// DETACHED, like every loop the engine runs: the lease must outlive the
	// boot's context and stop only when what it describes has stopped.
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

// beat claims or renews the lease with this beat's account.
//
// A CLAIM RATHER THAN A RENEW, for presence's reason: a claim by the holder
// doubles as a renew and keeps the epoch, it re-establishes the lease after a
// lapse, and it is the only call that carries Meta — a renew would leave what
// the lease says a beat old for ever.
//
// UNGATED: membership is not work, and a newer-protocol data node refused its
// lease during a rolling upgrade reads to its map as absent — see
// coord.AcquireOptions.Ungated.
//
// A BEAT WITH NOTHING IT CAN SAY CLAIMS NOTHING. A lease is read by what it
// says, and one written without its account would be read as a claim the node
// never made; left unrenewed it lapses, and the map counts the node absent,
// which is the honest reading of a member that cannot say what it is.
func (l *memberLease) beat(ctx context.Context) {
	// THE ACCOUNT FIRST, outside the claim's deadline: building it may
	// probe a disk or read a runtime, and a slow one must not spend the
	// budget of the write that says so.
	meta, metaErr := l.meta()
	l.mu.Lock()
	if metaErr != nil {
		if !l.unsaid {
			log.ErrorContext(ctx, l.event+"_lease_unsaid", "node", l.node,
				"error", metaErr.Error(),
				"detail", "this node's "+l.what+" cannot say what it holds, so "+
					"its lease is not renewed: it lapses after the lease TTL and the "+
					"map then counts this node absent")
		}
		l.unsaid = true
		l.mu.Unlock()
		return
	}
	if l.unsaid {
		log.InfoContext(ctx, l.event+"_lease_said", "node", l.node)
	}
	l.unsaid = false
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, l.interval)
	defer cancel()
	lease, _, err := l.leases.TryAcquire(ctx, l.resource, coord.AcquireOptions{
		Owner: l.owner, TTL: l.ttl, Preferred: l.node, Ungated: true, Meta: meta,
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err != nil:
		// UNKNOWN, NOT LOST: the store did not answer, and the lease is
		// probably still held. The next beat asks again.
		if !l.failing {
			log.WarnContext(ctx, l.event+"_lease_unavailable", "node", l.node,
				"error", err.Error(),
				"detail", "this node's "+l.what+" could not be renewed; it lapses "+
					"after the lease TTL if the store stays unreachable, and the "+
					"map then counts this node absent")
		}
		l.failing = true
	case lease == nil:
		if !l.refused {
			log.WarnContext(ctx, l.event+"_lease_refused", "node", l.node,
				"hint", "another process holds this node id's "+l.what+" — a "+
					"previous incarnation whose lease has not lapsed yet, which "+
					"clears within the lease TTL, or a second process running "+
					"under the same node id, which does not")
		}
		l.refused, l.failing = true, false
	default:
		if l.refused || l.failing {
			log.InfoContext(ctx, l.event+"_lease_claimed", "node", l.node,
				"epoch", lease.Epoch)
		}
		l.held, l.refused, l.failing = lease, false, false
	}
}

// stop ends the loop, waiting for a beat in flight, and gives the lease back
// — so the map sees this node gone at once rather than a TTL later. The caller
// has already stopped serving what the membership describes: a member that
// has stopped answering must not be counted present.
func (l *memberLease) stop(ctx context.Context) {
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
		log.WarnContext(ctx, l.event+"_lease_not_released", "node", l.node,
			"error", err.Error(),
			"detail", "the map counts this node present until the lease lapses")
	}
}

// memberLeaseInterval is how often a membership lease is renewed: the seat
// heartbeat's ratio of the TTL, so it takes the same number of missed beats to
// lapse as a seat does.
func memberLeaseInterval(ttl time.Duration) time.Duration {
	return ttl / seat.HeartbeatRatio
}
