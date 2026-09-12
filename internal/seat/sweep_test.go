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
// balancer a refresh tells a different story each time.
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

// --- the unserviceable shed ------------------------------------------------

// A NODE WHOSE ROWS ARE WRONG GIVES THE SEATS BACK, which is the half of D122
// that had no implementation at all.
//
// [Config.Ready] withholds claims and says so in its own log line — "it keeps
// what it holds and claims nothing until it is" — which is right for a copy
// that is merely behind. It is wrong for a copy that is WRONG, and the engine
// was telling operators otherwise: the `deferred_old` alarm reads "its seats
// move at 30m0s" with the remedy "its seats have already moved", against a
// mechanism where nothing moved them.
func TestAnUnserviceableNodeGivesBackEverySeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	fit := true
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo", "eng"), Hooks: hooks,
		Serviceable: func() (bool, string) {
			if fit {
				return true, ""
			}
			return false, "tracker"
		},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantInt(t, len(h.Held()), 2, "seats held while serviceable")

	fit = false
	h.Sweep(f.ctx)
	wantStrings(t, h.Held(), nil, "seats held while unserviceable")
	wantStrings(t, hooks.released(),
		[]string{"ceo:unserviceable", "eng:unserviceable"}, "releases")

	// AND IT CLAIMS NOTHING WHILE IT HOLDS. A shed that let the very next
	// pass re-claim would be a node cycling its whole company every sweep
	// interval, which is worse than either holding or shedding.
	h.Sweep(f.ctx)
	wantStrings(t, h.Held(), nil, "seats re-claimed while still unserviceable")

	// THE RECOVERY IS THE SAME GATE, from the other side: nothing has to
	// be reset by hand, and the node takes its share back on the first
	// pass after its rows are right again.
	fit = true
	h.Sweep(f.ctx)
	wantInt(t, len(h.Held()), 2, "seats reclaimed once serviceable again")
}

// THE RELEASE IS VOLUNTARY, not fenced. The lease is still held and still
// renewed — what is wrong is this node's ROWS — so the turn already running
// against them finishes rather than being abandoned mid-flight.
func TestTheUnserviceableShedIsVoluntary(t *testing.T) {
	t.Parallel()
	if ReasonUnserviceable.Fenced() {
		t.Error("ReasonUnserviceable reports itself fenced, so an in-flight " +
			"turn is abandoned rather than finished — the lease is intact " +
			"and only the rows are wrong")
	}
}

// A GATE THAT CANNOT ANSWER MUST NOT SHED A FLEET'S WORK, which is the
// opposite default from every other check in this file and deliberate: a
// panicking readiness gate costs a node its new claims, and a panicking
// serviceability gate would cost the company every seat on the node.
func TestAPanickingServiceabilityGateKeepsTheSeats(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks,
		Serviceable: func() (bool, string) { panic("status source is down") },
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantInt(t, len(h.Held()), 1, "seats held through a panicking gate")
	wantStrings(t, hooks.released(), nil, "releases from a panicking gate")
}

// A NIL GATE KEEPS EVERY SEAT, which is the single-node case and every
// deployment with no native backend at all.
func TestNoServiceabilityGateKeepsTheSeats(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo", "eng")})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantInt(t, len(h.Held()), 2, "seats held with no gate wired")
}
