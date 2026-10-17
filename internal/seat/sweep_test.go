package seat

import (
	"context"
	"errors"
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
		Status: func(context.Context) (coord.NodeStatus, error) {
			return coord.NodeStatus{InFlight: 2, Posture: "serve"}, nil
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
	leases, err := f.store.ListLive(f.ctx, coord.ClassNode)
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
// that does not answer within its share of the interval publishes nothing
// rather than holding the renewal until the watchdog shoots the process.
func TestASlowStatusHookDoesNotHoldThePresenceRenewal(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	released := make(chan struct{})
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(ctx context.Context) (coord.NodeStatus, error) {
			<-ctx.Done()
			close(released)
			return coord.NodeStatus{InFlight: 7}, ctx.Err()
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
//
// The budget is read rather than raced. This case used to publish through a
// one-millisecond deadline and pass or fail on whether the scheduler let the
// goroutine finish inside it — it failed under a loaded `make test` — so it
// asserts the deadline the hook was HANDED, which is the floor's whole
// promise and needs no clock to have behaved.
func TestATinyHeartbeatStillGivesTheHookATurn(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	var (
		deadline    time.Time
		hasDeadline bool
	)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(ctx context.Context) (coord.NodeStatus, error) {
			deadline, hasDeadline = ctx.Deadline()
			return coord.NodeStatus{InFlight: 3}, nil
		},
	})
	h.heartbeat = time.Nanosecond
	if got := h.statusBudget(); got <= 0 {
		t.Fatalf("a %v heartbeat gives the hook a budget of %v", h.heartbeat, got)
	}

	before := time.Now()
	h.renewNodePresence(f.ctx)

	if !hasDeadline || deadline.Sub(before) < h.statusBudget() {
		t.Errorf("the hook was handed a deadline %v after the beat began (set: %v), want at least %v",
			deadline.Sub(before), hasDeadline, h.statusBudget())
	}
	live, ok := coord.StatusFromMeta(presenceOf(t, f, "node-a").Meta)
	if !ok || live.InFlight != 3 {
		t.Errorf("status = %+v, published = %v", live, ok)
	}
}

// WHETHER THE HOOK ANSWERED IS THE HOOK'S FACT, never the beat's clock after
// it returned. A hook that hands back its whole answer is published even when
// the deadline has passed by the time the beat looks — which is exactly what
// a goroutine descheduled between the hook's return and that look used to
// lose.
func TestAnAnswerReturnedPastTheDeadlineIsStillPublished(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(ctx context.Context) (coord.NodeStatus, error) {
			// Finish the answer, then let the deadline pass before
			// returning it: the beat now reads a dead context beside a
			// complete answer.
			<-ctx.Done()
			return coord.NodeStatus{InFlight: 4}, nil
		},
	})
	h.heartbeat = 5 * time.Millisecond

	h.renewNodePresence(f.ctx)

	live, ok := coord.StatusFromMeta(presenceOf(t, f, "node-a").Meta)
	if !ok || live.InFlight != 4 {
		t.Errorf("status = %+v, published = %v; an answered hook was discarded on the clock", live, ok)
	}
}

// A HOOK THAT SAYS IT DID NOT ANSWER PUBLISHES NOTHING, however quickly it
// said so: a read that failed at once is no more an answer than one the
// deadline cut short.
func TestAHookThatDidNotAnswerPublishesNoStatus(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"),
		Status: func(context.Context) (coord.NodeStatus, error) {
			return coord.NodeStatus{InFlight: 9}, errors.New("the control plane refused the read")
		},
	})

	h.renewNodePresence(f.ctx)

	lease := presenceOf(t, f, "node-a")
	if live, ok := coord.StatusFromMeta(lease.Meta); ok {
		t.Errorf("a hook that did not answer published %+v", live)
	}
	if profile, ok := placement.FromLease(lease); !ok || profile.ID != "node-a" {
		t.Errorf("presence was not renewed: %v", lease.Meta)
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
// move at 30m" with the remedy "its seats have already moved", against a
// mechanism where nothing moved them.
func TestAnUnserviceableNodeGivesBackEverySeat(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	fit := true
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo", "eng"), Hooks: hooks,
		Serviceable: func(context.Context) (bool, string) {
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
		Serviceable: func(context.Context) (bool, string) { panic("status source is down") },
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

// countingHints is a backend that records how often the placement hints were
// read, and delegates everything else.
type countingHints struct {
	coord.Backend
	reads int
}

func (c *countingHints) PreferredResources(ctx context.Context, class coord.Class, nodeID string) (map[string]struct{}, error) {
	c.reads++
	return c.Backend.PreferredResources(ctx, class, nodeID)
}

// THE HINT READ IS PAID FOR AN ORDERING, so a pass with no ordering to make
// does not pay it.
//
// Placement hints live in the epochs bucket, which has NO TTL and is never
// pruned — it holds a record for every resource the deployment has ever
// leased, so it is the largest thing a node reads and it grows for the life
// of the deployment. The sweep runs every five seconds. Reading all of it to
// sort a list of fewer than two elements — which has exactly one order — is
// the whole of that cost for none of its benefit, and it lands hardest on a
// node whose remaining candidates are all in acquire backoff, which is the
// state the backoff exists to calm. The hint orders seats WITHIN a placement
// group, so it is the groups' candidates that have to number two or more, not
// the pass's.
func TestTheSweepReadsNoHintsWhenThereIsNothingToOrder(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	counting := &countingHints{Backend: f.store}

	h := f.newHost("node-a", Config{Backend: counting, Seats: seatsNamed("ceo", "eng")})

	group := func(handles ...string) []placement.Group {
		return []placement.Group{{Handles: handles, Nodes: 1, Share: len(handles)}}
	}
	// The company's seats by handle, as a sweep hands the claim path: a
	// group names its seats by handle, and the hint is on the lease, which
	// is named by the seat's id.
	byHandle := seatIndex([]string{"ceo", "eng"})
	order := func(groups []placement.Group) []candidate {
		room := make([]int, len(groups))
		for i, g := range groups {
			room[i] = g.Share
		}
		return h.claimOrder(f.ctx, byHandle, groups, room)
	}

	// Two candidates: an ordering exists, so the hints are worth reading.
	if got := order(group("ceo", "eng")); len(got) != 2 {
		t.Fatalf("claimOrder over two = %v", got)
	}
	if counting.reads != 1 {
		t.Fatalf("two candidates read the hints %d times, want 1", counting.reads)
	}

	// One, and none: each has exactly one ordering, so neither reads.
	if got := order(group("ceo")); len(got) != 1 {
		t.Fatalf("claimOrder over one = %v", got)
	}
	if got := order(nil); len(got) != 0 {
		t.Fatalf("claimOrder over none = %v", got)
	}
	// Two seats, but one in a group with no room left: one candidate, and
	// still one ordering.
	full := append(group("ceo"), group("eng")...)
	if got := h.claimOrder(f.ctx, byHandle, full, []int{1, 0}); len(got) != 1 || got[0].seat.Handle != "ceo" {
		t.Fatalf("claimOrder with one group full = %v, want only ceo", got)
	}
	// Two candidates in two groups of one: the groups' order is the plan's,
	// and each group has one ordering, so the pass has one too.
	if got := order(full); len(got) != 2 {
		t.Fatalf("claimOrder over two groups of one = %v", got)
	}
	if counting.reads != 1 {
		t.Errorf("a pass with nothing to order read the hints; reads = %d, want 1. "+
			"That walk is the never-pruned epochs bucket, on the five-second sweep, "+
			"for an ordering that has one possible answer", counting.reads)
	}
}

// EVERY ROLE NAMES WHAT A FLEET LOSES WITHOUT IT. The unmanned-role warning
// walks the placement vocabulary, and a role with no hint would be reported
// as missing with nothing telling an operator what stopped working — which
// is the one thing the warning exists to say, since every symptom is an
// absence.
func TestEveryRoleSaysWhatAFleetWithoutItLoses(t *testing.T) {
	t.Parallel()
	for _, role := range placement.Vocabulary() {
		if unmannedHints[role] == "" {
			t.Errorf("role %q has no unmanned hint", role)
		}
	}
}

// A CLAIM COUNTS ITS LEASE FROM BEFORE IT ASKED FOR IT, as a renew and the
// presence claim count theirs.
//
// The store counts a lease's TTL from the moment it grants it, inside the
// acquire, and until the first renew replaces it the host's own stamp is what
// turn admission and the unreachable-store grace measure from. Stamped once
// the call had returned, it was later than the store's by the round trip, so
// the host believed it held the seat that much longer than the store did —
// and admitted turns into time a peer may already own. Here the store's
// answer arrives three seconds after it granted the lease.
func TestAClaimCountsItsLeaseFromBeforeItAskedForIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), TTL: 30 * time.Second, HeartbeatInterval: 5 * time.Second,
		Backend: &lateAnswers{Backend: f.store, clock: f.clock, late: 3 * time.Second},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo")
	wantAdmits(t, h, "ceo", true)

	// One heartbeat and a second after the store granted the lease, and
	// only three seconds after this node heard that it had.
	f.clock.Advance(3 * time.Second)
	if _, ok := h.MayStart("ceo"); ok {
		t.Error("a turn is admitted more than a heartbeat after the store granted " +
			"the seat: the claim stamped its lease from when the store's answer " +
			"arrived, not from when it was asked for")
	}
	// And a whole TTL after the grant, when the store's lease has lapsed and
	// a peer may take the seat, no child may be started for it here.
	f.clock.Advance(24 * time.Second)
	if h.Provable("ceo") {
		t.Error("the seat is provable a TTL after the store granted it, so a " +
			"child could be started for an agent a peer may already run")
	}
}

// lateAnswers is the lease store with every seat acquire answered late: the
// store grants the lease at once, by its own clock, and the answer reaches the
// host once the shared clock has moved on by late.
type lateAnswers struct {
	coord.Backend
	clock *fakeClock
	late  time.Duration
}

func (s *lateAnswers) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	lease, refused, err := s.Backend.TryAcquire(ctx, resource, opts)
	if coord.IsSeatResource(resource) {
		s.clock.Advance(s.late)
	}
	return lease, refused, err
}
