package seat

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// What a node tells its peers about itself, on the presence heartbeat.

// A NODE ADVERTISES WHAT IT IS DOING, not only what it is. Only the node
// running a seat knows its in-flight count and its drain state, and /health
// answers about whichever node served the request — so behind a load
// balancer a refresh tells a different story each time. See
// decisions/501-node-runtime.md.
func TestPresenceCarriesThisNodesLiveStatus(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(context.Context) coord.NodeStatus {
			return coord.NodeStatus{InFlight: 2, Posture: "serve"}
		},
	})
	h.renewNodePresence(f.ctx)

	lease := presenceOf(t, f, "node-a")
	live, ok := coord.StatusFromMeta(lease.Meta)
	if !ok {
		t.Fatalf("no status on the presence lease: %v", lease.Meta)
	}
	if live.InFlight != 2 || live.Posture != "serve" {
		t.Errorf("status = %+v", live)
	}
	// THE PLACEMENT HALF SURVIVES BESIDE IT: two concerns sharing one map
	// is how one of them eventually shadows the other.
	if profile, ok := placement.FromLease(lease); !ok || profile.ID != "node-a" {
		t.Errorf("the placement half was lost: %v", lease.Meta)
	}
}

// A NODE WITH NO STATUS HOOK PUBLISHES NONE, which reads as "did not say"
// rather than as an idle node.
func TestPresenceOmitsStatusWhenTheNodeHasNone(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo")})
	h.renewNodePresence(f.ctx)

	if _, ok := coord.StatusFromMeta(presenceOf(t, f, "node-a").Meta); ok {
		t.Error("a node with no status hook published one")
	}
}

// presenceOf reads a node's presence lease.
func presenceOf(t *testing.T, f *fleet, node string) coord.Lease {
	t.Helper()
	leases, err := f.store.ListLive(f.ctx, coord.NodePrefix)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, lease := range leases {
		if id, ok := coord.NodeID(lease.Resource); ok && id == node {
			return lease
		}
	}
	t.Fatalf("no presence lease for %s", node)
	return coord.Lease{}
}

// THE BEAT IS NOT HOSTAGE TO A DISPLAY COLUMN. Answering may mean reading the
// control plane, and this runs on the path that renews presence — so a hook
// that overruns its share of the interval publishes nothing rather than
// holding the renewal until the watchdog shoots the process.
func TestASlowStatusHookDoesNotHoldThePresenceRenewal(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	released := make(chan struct{})
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(ctx context.Context) coord.NodeStatus {
			<-ctx.Done()
			close(released)
			return coord.NodeStatus{InFlight: 7}
		},
	})
	h.heartbeat = 5 * time.Millisecond

	h.renewNodePresence(f.ctx)

	select {
	case <-released:
	default:
		t.Fatal("the hook was never cancelled")
	}
	lease := presenceOf(t, f, "node-a")
	if _, ok := coord.StatusFromMeta(lease.Meta); ok {
		t.Error("an unanswered status hook published a status anyway")
	}
	// THE RENEWAL STILL HAPPENED: the column is what is sacrificed, never
	// the node's place in the fleet.
	if profile, ok := placement.FromLease(lease); !ok || profile.ID != "node-a" {
		t.Errorf("presence was not renewed: %v", lease.Meta)
	}
}

// A HOST WITH A HEARTBEAT UNDER THE DIVISOR still gives the hook a turn: a
// budget that rounds to zero expires before the call starts, and the column
// would never be published at all.
func TestATinyHeartbeatStillPublishesStatus(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(context.Context) coord.NodeStatus {
			return coord.NodeStatus{InFlight: 3}
		},
	})
	h.heartbeat = time.Nanosecond

	h.renewNodePresence(f.ctx)

	live, ok := coord.StatusFromMeta(presenceOf(t, f, "node-a").Meta)
	if !ok || live.InFlight != 3 {
		t.Errorf("status = %+v, published = %v", live, ok)
	}
}
