package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// A MEMBERSHIP LEASE IS RENEWED ON THE SEAT HEARTBEAT'S RATIO OF THE TTL IN
// FORCE, so it takes as many missed beats to lapse as a seat does whatever TTL
// the fleet adopted — never on the shipped interval against a shorter adopted
// TTL.
func TestAMembershipLeaseIsRenewedOnTheTTLInForce(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{45 * time.Second, 12 * time.Second} {
		if got := memberLeaseInterval(ttl); got*3 != ttl {
			t.Errorf("a %v lease renews every %v, want a third of it", ttl, got)
		}
	}
}

// A LEASE THAT CANNOT SAY WHAT ITS NODE HOLDS IS NOT CLAIMED. A membership is
// read by what it says, and one written without its account would be read as
// a claim the node never made — so a beat with nothing it can say claims
// nothing, and the first beat that can say it claims the lease.
func TestAMembershipLeaseThatCannotSayWhatItHoldsIsNotClaimed(t *testing.T) {
	t.Parallel()
	b := coordmemory.New()
	var said atomic.Bool
	var beats atomic.Int32
	l := startMemberLease(t.Context(), b, memberLeaseSpec{
		resource: coord.EstateResource("n1"), node: "n1", owner: "n1:inc",
		ttl: 300 * time.Millisecond, what: "estate membership", event: "estate",
		meta: func() (map[string]any, error) {
			beats.Add(1)
			if !said.Load() {
				return nil, errors.New("the store is not open")
			}
			return map[string]any{"weight": 1}, nil
		},
	})
	defer l.stop(context.Background())
	eventually(t, "three beats with nothing to say", func() bool { return beats.Load() >= 3 })
	if lease := held(t, b, coord.EstateResource("n1")); lease != nil {
		t.Fatalf("a lease with nothing to say was claimed: %+v", lease)
	}
	said.Store(true)
	eventually(t, "the lease, once it can be said", func() bool {
		return held(t, b, coord.EstateResource("n1")) != nil
	})
}
