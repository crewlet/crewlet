package seat

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// A NODE HOLDS ITS PRESENCE FOR ONE TTL PAST ITS LAST RENEW, AND NO LONGER.
// What a readiness probe reads: a store that stops answering leaves the last
// lease in hand while the row behind it lapses, and past the TTL every peer
// reads this node as gone. Holding on to the lease object alone would report
// a node the fleet cannot see as one doing its work.
func TestPresenceIsHeldOnlyInsideTheTTLOfTheLastRenew(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	faulty := coordtest.NewFaulty(f.store)
	h := f.newHost("node-a", Config{Backend: faulty, Seats: seatsNamed("ceo"), TTL: 30 * time.Second})

	if h.HoldsPresence() {
		t.Fatal("a host that never claimed its presence reports holding it")
	}
	h.renewNodePresence(f.ctx)
	if !h.HoldsPresence() {
		t.Fatal("a host whose presence claim succeeded does not report holding it")
	}

	// THE STORE GOES AWAY. Inside the TTL the row is still this node's.
	faulty.Break(nil)
	f.clock.Advance(20 * time.Second)
	h.renewNodePresence(f.ctx)
	if !h.HoldsPresence() {
		t.Error("a renew that could not reach the store dropped presence " +
			"while the row it last claimed is still live")
	}
	// PAST IT, the row has lapsed whatever this node can see.
	f.clock.Advance(15 * time.Second)
	h.renewNodePresence(f.ctx)
	if h.HoldsPresence() {
		t.Error("a host whose last successful renew is older than the TTL " +
			"still reports holding its presence")
	}

	// AND A RENEW THAT LANDS AGAIN RESTORES IT.
	faulty.Heal()
	h.renewNodePresence(f.ctx)
	if !h.HoldsPresence() {
		t.Error("a successful renew after the outage did not restore presence")
	}
}

// A DRAINING NODE HOLDS NO PRESENCE, from the moment the drain begins: it gave
// the lease up so peers stop counting it, and it must not read as a member
// that is doing its work.
func TestADrainingHostHoldsNoPresence(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)
	if !h.HoldsPresence() {
		t.Fatal("a host whose presence claim succeeded does not report holding it")
	}
	h.BeginDrain(f.ctx)
	if h.HoldsPresence() {
		t.Error("a draining host still reports holding its presence")
	}
}
