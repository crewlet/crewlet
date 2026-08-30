package node_test

// The fleet suite. This is the architecture's proof, and it is deliberately
// not a unit test of anything: it runs TWO real nodes against ONE broker and
// ONE coordination store, and asserts the five properties seat ownership
// exists to provide.
//
// Every property here failed at least once in the Python engine's history,
// and each failure was invisible from a single node — which is why this suite
// runs two.

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordkv "github.com/crewlet/crewlet/internal/coord/kv"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// trigger is the event a seat's turn runs for.
type trigger struct {
	Work string `json:"work"`
}

func (trigger) EventType() string { return "test.trigger" }

func init() { events.Register[trigger]() }

// fleetTTL is short so a suite run does not wait out production timings. The
// behaviours are the same at any scale; the numbers themselves are pinned by
// the measurement harnesses in the backend packages.
const fleetTTL = 2 * time.Second

// --- the two substrates ---------------------------------------------------
//
// The suite runs against BOTH the in-memory twins and the real embedded
// stack. "The same suite passes on the twin" is itself an exit criterion: a
// twin that models the broker wrongly must fail here rather than certify a
// bug in CI.

type substrate struct {
	name  string
	build func(t *testing.T) (mkQueue func(t *testing.T) queue.EventQueue, backend coord.Backend)
}

func substrates() []substrate {
	return []substrate{
		{
			name: "twin",
			build: func(t *testing.T) (func(*testing.T) queue.EventQueue, coord.Backend) {
				broker := qmem.NewBroker()
				backend := &coordmem.Backend{}
				return func(t *testing.T) queue.EventQueue {
					q := broker.Client()
					if err := q.Start(t.Context()); err != nil {
						t.Fatalf("queue.Start: %v", err)
					}
					t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
					return q
				}, backend
			},
		},
		{
			name: "embedded",
			build: func(t *testing.T) (func(*testing.T) queue.EventQueue, coord.Backend) {
				srv, err := jetstream.StartServer(t.Context(), jetstream.Config{
					FetchWait: 25 * time.Millisecond,
					NakDelay:  25 * time.Millisecond,
					AckWait:   2 * time.Second,
				})
				if err != nil {
					t.Fatalf("StartServer: %v", err)
				}
				t.Cleanup(srv.Shutdown)

				admin, err := srv.Client(t.Context())
				if err != nil {
					t.Fatalf("admin client: %v", err)
				}
				t.Cleanup(func() { _ = admin.Stop(context.WithoutCancel(t.Context())) })

				backend, err := coordkv.Open(t.Context(), admin.Conn(), coordkv.Config{TTL: fleetTTL})
				if err != nil {
					t.Fatalf("coord kv: %v", err)
				}
				return func(t *testing.T) queue.EventQueue {
					q, err := srv.Client(t.Context())
					if err != nil {
						t.Fatalf("client: %v", err)
					}
					t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
					return q
				}, backend
			},
		},
		{
			// The production fleet topology, and the only substrate that
			// exercises the mechanisms a fleet actually depends on:
			// replication, quorum, leader election, placement. Its
			// clients connect to DIFFERENT members, as separate
			// processes would — a suite where every node talks to one
			// server proves nothing about the cluster.
			//
			// Slow to stand up (cluster formation, then quorum writes),
			// so it is last: a failure on the twin or the single server
			// is the same failure, found sooner.
			name: "cluster",
			build: func(t *testing.T) (func(*testing.T) queue.EventQueue, coord.Backend) {
				c := jetstreamtest.StartCluster(t, 3, jetstream.Config{
					FetchWait: 25 * time.Millisecond,
					NakDelay:  25 * time.Millisecond,
					AckWait:   2 * time.Second,
				})
				admin := c.Client(t, 0)
				backend, err := coordkv.Open(t.Context(), admin.Conn(), coordkv.Config{
					TTL: fleetTTL, Replicas: len(c.Servers),
				})
				if err != nil {
					t.Fatalf("coord kv: %v", err)
				}
				// Round-robin over the members, so consecutive nodes in a
				// test land on different servers.
				var next atomic.Int64
				return func(t *testing.T) queue.EventQueue {
					i := int(next.Add(1)-1) % len(c.Servers)
					return c.Client(t, i)
				}, backend
			},
		},
	}
}

// --- harness --------------------------------------------------------------

// fleet is a company plus the nodes running it.
type fleet struct {
	t       *testing.T
	seats   []string
	mkQueue func(*testing.T) queue.EventQueue
	backend coord.Backend

	// pinned constrains a seat's placement, for the cases about a seat
	// nobody can claim. Written before any node starts, read by the
	// hosts' sweeps afterwards, so it needs no lock of its own.
	pinned map[string]placement.SeatPlacement

	mu    sync.Mutex
	turns []turnRecord
}

// turnRecord is one turn a node ran, so the suite can assert WHICH node did
// what — the fact a single-node test can never see.
type turnRecord struct {
	node   string
	handle string
	work   []string
}

func newFleet(t *testing.T, sub substrate, seats ...string) *fleet {
	mk, backend := sub.build(t)
	return &fleet{t: t, seats: seats, mkQueue: mk, backend: backend}
}

func (f *fleet) seatList() []placement.Seat {
	out := make([]placement.Seat, len(f.seats))
	for i, h := range f.seats {
		out[i] = placement.Seat{Handle: h, Placement: f.pinned[h]}
	}
	return out
}

// pin sends a seat somewhere no node in this fleet is, so it stays unclaimed
// for the life of the test.
func (f *fleet) pin(handle, node string) {
	if f.pinned == nil {
		f.pinned = map[string]placement.SeatPlacement{}
	}
	f.pinned[handle] = placement.SeatPlacement{Node: node}
}

// start brings up a node and returns it.
func (f *fleet) start(id string) *node.Node {
	f.t.Helper()
	q := f.mkQueue(f.t)

	n, err := node.New(node.Config{
		Queue:             q,
		Coord:             f.backend,
		NodeID:            id,
		Owner:             id + ":1",
		Seats:             f.seatList,
		LeaseTTL:          fleetTTL,
		HeartbeatInterval: fleetTTL / 4,
		SweepInterval:     fleetTTL / 8,
		Turn: func(_ context.Context, handle string, evs []*events.Event) queue.Result {
			work := make([]string, len(evs))
			for i, e := range evs {
				if p, ok := events.DataAs[*trigger](e); ok {
					work[i] = p.Work
				}
			}
			f.mu.Lock()
			f.turns = append(f.turns, turnRecord{node: id, handle: handle, work: work})
			f.mu.Unlock()
			return queue.Ack()
		},
	})
	if err != nil {
		f.t.Fatalf("node.New(%s): %v", id, err)
	}
	if err := n.Start(f.t.Context()); err != nil {
		f.t.Fatalf("node.Start(%s): %v", id, err)
	}
	f.t.Cleanup(func() { n.Stop(context.WithoutCancel(f.t.Context())) })
	return n
}

// ensureMailboxes creates every seat's subscription with nothing attached.
//
// This is what makes an unclaimed seat's mail survivable at all, and it is
// deliberately explicit in the suite: a test that let attachment create the
// subscription would never exercise the case the property is about.
//
// It used to say the engine did this behind a singleton duty. Nothing did —
// EnsureSubscription had no caller anywhere outside this file — and the suite
// doing it by hand is exactly why nobody noticed. [node.Node.Start] does it
// now, for every seat in the company, which is what
// an_unplaceable_seat_still_retains_its_mail below certifies.
func (f *fleet) ensureMailboxes() {
	f.t.Helper()
	q := f.mkQueue(f.t)
	for _, h := range f.seats {
		if _, err := q.EnsureSubscription(f.t.Context(), topics.AgentInbox(h), topics.AgentInboxGroup(h)); err != nil {
			f.t.Fatalf("EnsureSubscription(%s): %v", h, err)
		}
	}
}

func (f *fleet) send(handle string, work ...string) {
	f.t.Helper()
	q := f.mkQueue(f.t)
	for _, w := range work {
		ev := events.New(trigger{Work: w}, events.TraceContext{})
		ev.Payload = map[string]any{"conversation_key": "c/" + handle}
		if err := q.Publish(f.t.Context(), topics.AgentInbox(handle), ev); err != nil {
			f.t.Fatalf("Publish(%s, %s): %v", handle, w, err)
		}
	}
}

func (f *fleet) records() []turnRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.turns)
}

// workSeen returns the work items handled for a seat, in the order handled.
func (f *fleet) workSeen(handle string) []string {
	var out []string
	for _, r := range f.records() {
		if r.handle == handle {
			out = append(out, r.work...)
		}
	}
	return out
}

// nodesThatRan returns which nodes ran a turn for a seat.
func (f *fleet) nodesThatRan(handle string) []string {
	seen := map[string]struct{}{}
	for _, r := range f.records() {
		if r.handle == handle {
			seen[r.node] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// eventually polls until cond holds or the deadline passes.
//
// The budget is a MULTIPLE OF THE LEASE TTL, not a wall-clock number: every
// convergence this test waits on is counted in sweeps (TTL/8) and heartbeats
// (TTL/4), so a fixed number silently stops meaning the same thing the moment
// fleetTTL changes. 30x is deliberately generous — the suite runs 77 packages
// in parallel, and a goroutine on a loaded CI box can lose the scheduler for
// whole seconds at a time. The old fixed 20 s (10x) passed every run in
// isolation and failed under full-suite load.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * fleetTTL)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the exit criteria ----------------------------------------------------

func TestFleet(t *testing.T) {
	for _, sub := range substrates() {
		t.Run(sub.name, func(t *testing.T) {
			t.Run("every_seat_is_owned_by_exactly_one_node", func(t *testing.T) {
				f := newFleet(t, sub, "ceo", "cto", "pm", "eng")
				f.ensureMailboxes()
				a, b := f.start("node-a"), f.start("node-b")

				eventually(t, "both nodes to hold seats", func() bool {
					return len(a.Attached()) > 0 && len(b.Attached()) > 0
				})
				eventually(t, "every seat to be claimed", func() bool {
					return len(a.Attached())+len(b.Attached()) == len(f.seats)
				})

				// The property that matters is not the split, it is that
				// nothing is held twice: two nodes consuming one inbox is
				// the split brain every other guarantee rests on.
				held := append(append([]string{}, a.Attached()...), b.Attached()...)
				slices.Sort(held)
				if len(slices.Compact(slices.Clone(held))) != len(held) {
					t.Errorf("a seat is held by both nodes: %v", held)
				}
			})

			// A SEAT NOBODY CAN CLAIM STILL KEEPS ITS MAIL.
			//
			// The quiet-loss case, and the one nothing covered because the
			// harness created every mailbox by hand. A durable subscription
			// IS the mailbox: publish to a topic no subscription covers and
			// the broker DROPS the event, silently, with nothing anywhere
			// reporting a loss. So a seat's mail existed only from the
			// moment some node attached a consumer to it.
			//
			// A pinned seat makes that permanent and is a real
			// configuration — a role pinned to a node that is down, or
			// carrying a label nobody has. The sweep already reports it as
			// unplaceable; what it could not do was keep the work. Every
			// trigger aimed at such a seat vanished, and it vanished the
			// same way during the seconds of a boot or a rollout when a
			// live seat has not been claimed yet.
			//
			// Note the harness does NOT pre-create mailboxes here: the
			// node's own start is what has to do it.
			t.Run("an_unplaceable_seat_still_retains_its_mail", func(t *testing.T) {
				f := newFleet(t, sub, "ceo", "ghost")
				f.pin("ghost", "node-that-does-not-exist")
				a := f.start("node-a")

				eventually(t, "the claimable seat to be held", func() bool {
					return len(a.Attached()) == 1
				})
				if held := a.Attached(); held[0] != "ceo" {
					t.Fatalf("attached %v, want only the placeable seat", held)
				}

				f.send("ghost", "w1", "w2")

				// Nothing runs it — that is the point, and correct. What
				// must be true is that the work is still THERE, so the seat
				// does its backlog the moment a matching node appears.
				if got := f.workSeen("ghost"); len(got) != 0 {
					t.Fatalf("an unplaceable seat ran %v", got)
				}
				b := f.start("node-that-does-not-exist")
				eventually(t, "the pinned seat to be claimed", func() bool {
					return len(b.Attached()) == 1
				})
				eventually(t, "the retained mail to be delivered", func() bool {
					return len(f.workSeen("ghost")) == 2
				})
			})

			t.Run("a_trigger_reaches_only_the_owner", func(t *testing.T) {
				f := newFleet(t, sub, "ceo", "cto")
				f.ensureMailboxes()
				f.start("node-a")
				f.start("node-b")
				eventually(t, "seats to settle", func() bool {
					return len(f.ownersSettled()) == 2
				})

				f.send("ceo", "w1")
				eventually(t, "the turn to run", func() bool {
					return len(f.workSeen("ceo")) == 1
				})

				// Exactly one node ran it. A fleet-wide consumer group
				// would let either node pick it up, and the seat would
				// have no memory continuity at all.
				if ran := f.nodesThatRan("ceo"); len(ran) != 1 {
					t.Errorf("the trigger ran on %v, want exactly one node", ran)
				}
			})

			t.Run("unclaimed_mail_survives_and_is_delivered_on_claim", func(t *testing.T) {
				f := newFleet(t, sub, "ceo")
				// The subscription exists; nothing is attached to it. This
				// is an unowned seat, and its mail must wait.
				f.ensureMailboxes()
				f.send("ceo", "w1", "w2")

				// Nothing may have run: there was no owner.
				time.Sleep(200 * time.Millisecond)
				if got := f.workSeen("ceo"); len(got) != 0 {
					t.Fatalf("work ran with no owner: %v", got)
				}

				f.start("node-a")
				eventually(t, "the backlog to be delivered", func() bool {
					return len(f.workSeen("ceo")) == 2
				})
				if got := f.workSeen("ceo"); got[0] != "w1" || got[1] != "w2" {
					t.Errorf("backlog arrived as %v, want [w1 w2] — order was lost", got)
				}
			})

			t.Run("a_seats_mail_survives_the_whole_fleet_restarting", func(t *testing.T) {
				// The stronger form of the previous criterion, and the one
				// a deploy actually performs: every node stops, so for a
				// while NOTHING holds the seat or consumes its subject,
				// and then a fresh fleet arrives. Mail published in that
				// gap must be there when it does.
				//
				// This is where an in-memory mailbox and a durable one
				// look different for the first time — a subscription that
				// only exists while a consumer is attached loses
				// everything here, and every earlier criterion still
				// passes.
				f := newFleet(t, sub, "ceo", "cto")
				f.ensureMailboxes()
				a, b := f.start("node-a"), f.start("node-b")
				eventually(t, "the fleet to settle", func() bool {
					return len(a.Attached())+len(b.Attached()) == 2
				})
				f.send("ceo", "before")
				eventually(t, "the first turn", func() bool {
					return len(f.workSeen("ceo")) == 1
				})

				// The whole fleet goes away, cleanly.
				stop := context.WithoutCancel(t.Context())
				a.Stop(stop)
				b.Stop(stop)
				eventually(t, "every seat to be unowned", func() bool {
					return len(f.ownersSettled()) == 0
				})

				// Published into a company with no engine running at all.
				f.send("ceo", "during-the-gap")
				time.Sleep(200 * time.Millisecond)
				if got := f.workSeen("ceo"); len(got) != 1 {
					t.Fatalf("work ran with the whole fleet down: %v", got)
				}

				// A fresh fleet, with node identities a redeploy would
				// produce rather than the ones that just left.
				f.start("node-c")
				f.start("node-d")
				eventually(t, "the new fleet to work the gap's mail", func() bool {
					return len(f.workSeen("ceo")) == 2
				})
				if got := f.workSeen("ceo"); got[1] != "during-the-gap" {
					t.Errorf("the restarted fleet saw %v, want during-the-gap second", got)
				}
				last := f.records()[len(f.records())-1]
				if last.node != "node-c" && last.node != "node-d" {
					t.Errorf("the post-restart turn ran on %s, want one of the new nodes", last.node)
				}
			})

			t.Run("a_handoff_preserves_the_seats_mail", func(t *testing.T) {
				f := newFleet(t, sub, "ceo")
				f.ensureMailboxes()
				a := f.start("node-a")
				eventually(t, "node-a to hold the seat", func() bool {
					return len(a.Attached()) == 1
				})

				f.send("ceo", "w1")
				eventually(t, "the first turn", func() bool {
					return len(f.workSeen("ceo")) == 1
				})

				// node-a leaves the way a drain does: voluntarily, with
				// the mailbox handed back intact.
				a.Drain(context.WithoutCancel(t.Context()))
				eventually(t, "node-a to let the seat go", func() bool {
					return len(a.Attached()) == 0
				})

				// Work published while NOBODY owns the seat must still be
				// there when a successor arrives.
				f.send("ceo", "w2")
				b := f.start("node-b")
				eventually(t, "node-b to take the seat", func() bool {
					return len(b.Attached()) == 1
				})
				eventually(t, "the successor to run the pending work", func() bool {
					return len(f.workSeen("ceo")) == 2
				})

				if got := f.workSeen("ceo"); got[1] != "w2" {
					t.Errorf("after handoff the seat saw %v, want w2 second", got)
				}
				// And it ran on the successor, not on the node that left.
				last := f.records()[len(f.records())-1]
				if last.node != "node-b" {
					t.Errorf("the post-handoff turn ran on %s, want node-b", last.node)
				}
			})

			t.Run("a_node_that_lost_its_lease_starts_no_turn", func(t *testing.T) {
				// The situation this reproduces is a node PARTITIONED
				// FROM THE STORE, not a peer robbing a live holder — a
				// live holder cannot be robbed, because it renews. So
				// node-a's renewals start failing while everything else
				// keeps working: its lease lapses, node-b claims the
				// seat, and node-a is left ATTACHED to a mailbox it no
				// longer owns.
				//
				// That gap is the whole reason a turn is gated on
				// ownership rather than on having a consumer. A renew
				// ERROR deliberately keeps the seat (the store being
				// unreachable is not proof of anything), so node-a's
				// consumer stays up — and freshness, not the consumer,
				// is what has to stop it working.
				//
				// TWO defences answer this: OnAdmission quiesces the
				// mailbox so no delivery arrives, and MayStart refuses
				// the turn if one does anyway. This case is timed to sit
				// squarely in the window where the first is what has to
				// hold — the work is published once node-a has stopped
				// admitting but before its lease has lapsed — because
				// that window can be entered deterministically, while
				// the later one (attached, lease genuinely gone, drop
				// not yet noticed) is a race between node-a's heartbeat
				// and node-b's sweep that either can win. MayStart's own
				// behaviour is pinned in the seat package, on a fake
				// clock that can hold a window open exactly.
				f := newFleet(t, sub, "ceo")
				cut := &partitionable{Backend: f.backend}
				f.backend = cut

				a := f.start("node-a")
				eventually(t, "node-a to hold the seat", func() bool {
					return len(a.Attached()) == 1
				})
				f.ensureMailboxes()
				b := f.start("node-b")

				// From here node-a cannot prove it still owns the seat.
				// It KEEPS it — an unreachable store is not evidence of
				// anything — so what has to stop it working is
				// admission, not ownership.
				cut.isolate("node-a:1")
				eventually(t, "node-a to stop admitting work", func() bool {
					_, ok := a.Host().MayStart("ceo")
					return !ok
				})
				if len(a.Attached()) != 1 {
					t.Fatalf("node-a detached itself (attached=%v) — the case this "+
						"asserts about no longer arises", a.Attached())
				}

				// Published into a mailbox node-a is still consuming. If
				// attachment were what gated a turn, node-a would take
				// it right now.
				f.send("ceo", "w-after-loss")
				eventually(t, "node-b to take over the lapsed seat", func() bool {
					return len(b.Attached()) == 1
				})
				eventually(t, "the successor to run the work", func() bool {
					return len(f.workSeen("ceo")) >= 1
				})
				// Settle, so a late second delivery would be caught.
				time.Sleep(fleetTTL)

				if ran := f.nodesThatRan("ceo"); !slices.Equal(ran, []string{"node-b"}) {
					t.Errorf("the work ran on %v, want only node-b — node-a acted on a "+
						"seat whose lease it had lost", ran)
				}
				if got := f.workSeen("ceo"); len(got) != 1 {
					t.Errorf("the trigger was worked %d times (%v), want exactly once",
						len(got), got)
				}
			})
		})
	}
}

// ownersSettled reports the seats currently claimed by anyone.
func (f *fleet) ownersSettled() []string {
	var out []string
	for _, h := range f.seats {
		if l, err := f.backend.Get(f.t.Context(), coord.SeatResource(h)); err == nil && l != nil {
			out = append(out, h)
		}
	}
	return out
}

// partitionable wraps a coordination backend so ONE owner's renewals fail
// while every other call keeps working.
//
// That asymmetry is the point. A node cut off from the store is the only way
// a live seat actually changes hands under load, and it is not something the
// contract's own fault injector models: that one fails calls for everybody,
// which stalls the whole fleet rather than isolating a member of it.
//
// Renew is the single method it cuts. Failing reads too would be a different
// scenario — a node that cannot even find out what it holds — and failing
// TryAcquire would let the isolated node quietly re-claim the seat its peer
// had just taken, which is the outcome under test.
type partitionable struct {
	coord.Backend

	mu     sync.Mutex
	cutOff map[string]struct{}
}

// isolate makes every subsequent renewal by this owner report the store as
// unreachable, the way a network partition does.
func (p *partitionable) isolate(owner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cutOff == nil {
		p.cutOff = map[string]struct{}{}
	}
	p.cutOff[owner] = struct{}{}
}

func (p *partitionable) Renew(
	ctx context.Context, resource, owner string, epoch int64, ttl time.Duration,
) (bool, error) {
	p.mu.Lock()
	_, cut := p.cutOff[owner]
	p.mu.Unlock()
	if cut {
		// An ERROR, never (false, nil): the contract reads false as "this
		// lease is definitively no longer yours", which would have the
		// node drop the seat immediately and skip the very window —
		// attached, unowned — this test exists to look at.
		return false, fmt.Errorf("%w: partitioned", coord.ErrUnavailable)
	}
	return p.Backend.Renew(ctx, resource, owner, epoch, ttl)
}
