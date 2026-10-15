package node_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// callLog records, in order, the registrations and subscription creations one
// walk made, so a test can assert which came first.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

type recordingRegistry struct {
	log  *callLog
	fail error
}

func (r *recordingRegistry) Register(_ context.Context, s placement.Seat) error {
	r.log.add("register " + s.Handle)
	return r.fail
}

type recordingQueue struct {
	*qmem.Queue
	log *callLog
}

func (q *recordingQueue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	q.log.add("ensure " + topic)
	return q.Queue.EnsureSubscription(ctx, topic, group)
}

func mailboxNode(t *testing.T, registry node.MailboxRegistry, log *callLog) (*node.Node, *recordingQueue) {
	t.Helper()
	q := &recordingQueue{Queue: qmem.New(), log: log}
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{}, Mailboxes: registry,
		NodeID: "node-a", Owner: "node-a-1",
		Seats: func() []placement.Seat {
			return seats("ceo", "swe")
		},
		Turn: func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return n, q
}

// A MAILBOX IS REGISTERED BEFORE IT EXISTS. Once a seat leaves the company the
// fleet's record is the only thing that remembers its subscription, so a
// subscription created first and registered second is one a crash in between
// leaves unretirable for ever.
func TestEveryMailboxIsRegisteredBeforeItIsCreated(t *testing.T) {
	log := &callLog{}
	n, _ := mailboxNode(t, &recordingRegistry{log: log}, log)

	n.EnsureMailboxes(t.Context())

	want := []string{
		"register ceo", "ensure " + inbox("ceo"),
		"register swe", "ensure " + inbox("swe"),
	}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("walk made\n  %v\nwant\n  %v", got, want)
	}
}

// A registration that fails still creates the mailbox. A seat in the company
// losing its mail is strictly worse than a mailbox the sweep registers on its
// next tick.
func TestAFailedRegistrationStillCreatesTheMailbox(t *testing.T) {
	log := &callLog{}
	n, q := mailboxNode(t, &recordingRegistry{log: log, fail: errors.New("coordination store unreachable")}, log)

	n.EnsureMailboxes(t.Context())

	for _, handle := range []string{"ceo", "swe"} {
		made, err := q.Queue.EnsureSubscription(t.Context(), inbox(handle), inboxGroup(handle))
		if err != nil {
			t.Fatalf("probe %s: %v", handle, err)
		}
		if made {
			t.Errorf("seat %s has no mailbox because its registration failed, so its mail is dropped", handle)
		}
	}
}

// A node with no fleet store registers nothing and still creates every
// mailbox, rather than refusing to start.
func TestANodeWithoutARegistryStillCreatesEveryMailbox(t *testing.T) {
	log := &callLog{}
	n, _ := mailboxNode(t, nil, log)

	n.EnsureMailboxes(t.Context())

	want := []string{"ensure " + inbox("ceo"), "ensure " + inbox("swe")}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("walk made %v, want %v", got, want)
	}
}

// --- the convergence -------------------------------------------------------

// seatsOf is a company's seat list, as the node reads it fresh each pass.
func seatsOf(handles ...string) func() []placement.Seat {
	return func() []placement.Seat {
		out := make([]placement.Seat, 0, len(handles))
		for _, h := range handles {
			out = append(out, seat(h))
		}
		return out
	}
}

// convergingNode builds a node over the given broker client with a company of
// its own, and a lease TTL short enough that a case can outlive it.
//
// The TTL is what decides when the set is re-seeded from the broker, so it is
// the one timing a case here has to be able to move. Twenty milliseconds
// rather than a clock seam on the node: the value is read straight off the
// seat host, and a second idea of the time would be a second thing to keep
// true.
func convergingNode(t *testing.T, q queue.EventQueue, id string, ttl time.Duration,
	seats func() []placement.Seat) *node.Node {

	t.Helper()
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{},
		NodeID: id, Owner: id + ":1",
		LeaseTTL: ttl,
		Seats:    seats,
		Turn:     func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
	})
	if err != nil {
		t.Fatalf("node.New(%s): %v", id, err)
	}
	return n
}

// ensured is the inbox subjects a pass asked the queue to ensure, in order.
func ensured(log *callLog) []string {
	var out []string
	for _, call := range log.snapshot() {
		if handle, ok := strings.CutPrefix(call, "ensure "); ok {
			out = append(out, handle)
		}
	}
	return out
}

// THE SET IS THE BROKER'S ANSWER, NEVER THIS NODE'S MEMORY.
//
// Both halves are the same property seen from each side. A mailbox a PEER made
// is one this node must not propose again — that is the cost the convergence
// exists to remove. A mailbox that has GONE is one this node must propose
// again however sure it was — and it can only be sure from memory, which a
// recreated stream makes a lie: the stream comes back empty, every consumer
// with it, and a node reading its own set would leave every seat in the
// company with no mailbox and nothing anywhere to say so.
func TestTheEnsuredSetIsSeededFromTheBroker(t *testing.T) {
	const ttl = 20 * time.Millisecond
	log := &callLog{}
	q := &recordingQueue{Queue: qmem.New(), log: log}
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })

	// A peer got to this one first. Made through the embedded queue so the
	// call log records only what the NODE asked for.
	if _, err := q.Queue.EnsureSubscription(t.Context(),
		inbox("ceo"), inboxGroup("ceo")); err != nil {
		t.Fatalf("the peer's EnsureSubscription: %v", err)
	}

	n := convergingNode(t, q, "node-a", ttl, seatsOf("ceo", "swe"))
	n.EnsureMailboxes(t.Context())

	if got, want := ensured(log), []string{inbox("swe")}; !slices.Equal(got, want) {
		t.Fatalf("the first pass ensured %v, want %v: the seat a peer had already given a "+
			"mailbox was proposed again, which is one replicated write per seat per apply "+
			"for a company whose mailboxes all exist", got, want)
	}

	// THE STREAM IS RECREATED. Nothing tells the node; the consumers are
	// simply not there any more.
	for _, handle := range []string{"ceo", "swe"} {
		if _, err := q.Queue.DeleteSubscription(t.Context(),
			inbox(handle), inboxGroup(handle)); err != nil {
			t.Fatalf("recreate the stream (delete %s): %v", handle, err)
		}
	}

	// Inside the TTL the node is entitled to its set, and says nothing.
	log.reset()
	n.EnsureMailboxes(t.Context())
	if got := ensured(log); len(got) != 0 {
		t.Fatalf("a pass inside the lease TTL ensured %v, want nothing: the set is younger "+
			"than a lease, so this pass should not have reached the broker at all", got)
	}

	// Past it, the set is a claim about somebody else's state that has to
	// be re-asked — and the answer is that every mailbox is gone.
	time.Sleep(5 * ttl)
	log.reset()
	n.EnsureMailboxes(t.Context())
	want := []string{inbox("ceo"), inbox("swe")}
	if got := ensured(log); !slices.Equal(got, want) {
		t.Fatalf("after the stream was recreated the node ensured %v, want %v: it trusted "+
			"its own memory, so every seat in the company is silently dropping its mail", got, want)
	}
}

// A FAILED ENSURE IS LEFT OUT OF THE SET, so the next tick tries it again.
//
// The alternative is worse than it looks: a set that recorded the attempt
// rather than the answer would report a mailbox this node never made, and the
// seat would drop its mail for the life of the process — with the failure
// already logged and long since scrolled away.
func TestAFailedEnsureIsRetriedOnTheNextTick(t *testing.T) {
	log := &callLog{}
	q := &failingQueue{
		recordingQueue: &recordingQueue{Queue: qmem.New(), log: log},
		refuse:         map[string]int{inbox("swe"): 1},
	}
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })

	n := convergingNode(t, q, "node-a", time.Minute, seatsOf("ceo", "swe"))

	n.EnsureMailboxes(t.Context())
	want := []string{inbox("ceo"), inbox("swe")}
	if got := ensured(log); !slices.Equal(got, want) {
		t.Fatalf("the first pass ensured %v, want %v", got, want)
	}

	// ONLY THE ONE THAT FAILED. The seat that succeeded is in the set, and
	// re-proposing it is the cost this whole change removes.
	log.reset()
	n.EnsureMailboxes(t.Context())
	if got, want := ensured(log), []string{inbox("swe")}; !slices.Equal(got, want) {
		t.Fatalf("the retry pass ensured %v, want %v: a seat whose mailbox could not be "+
			"made is the one thing a later tick has to come back to", got, want)
	}

	// And now that it is there, a pass with nothing missing sends nothing.
	log.reset()
	n.EnsureMailboxes(t.Context())
	if got := ensured(log); len(got) != 0 {
		t.Fatalf("a converged pass ensured %v, want nothing", got)
	}
}

// EVERY SEAT IN THE COMPANY, FROM A NODE THAT WILL RUN A FEW OF THEM — which
// is the normal fleet case rather than an edge one.
//
// A mailbox is a fact about the company: the node that ends up serving a seat
// may not be the node that made its mailbox, and a seat no live node's
// placement matches has to hold its mail all the same. What the convergence
// adds is that the second node pays NOTHING for saying so, where the walk it
// replaces paid one replicated create per seat on every boot of every member.
func TestAPeerEnsuresEverySeatAndProposesNothingForTheOnesThatExist(t *testing.T) {
	broker := qmem.NewBroker()
	company := func() []placement.Seat {
		return []placement.Seat{
			{ID: seatID("ceo"), Handle: "ceo",
				Placement: placement.SeatPlacement{Node: "node-a"}},
			{ID: seatID("swe"), Handle: "swe",
				Placement: placement.SeatPlacement{Node: "node-a"}},
			{ID: seatID("pm"), Handle: "pm",
				Placement: placement.SeatPlacement{Node: "node-b"}},
		}
	}

	first := broker.Client()
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("first queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = first.Stop(context.WithoutCancel(t.Context())) })
	convergingNode(t, first, "node-a", time.Minute, company).EnsureMailboxes(t.Context())

	if got, want := first.ConsumerProposals(), 3; got != want {
		t.Fatalf("the first node proposed %d mailboxes, want %d: every seat in the company "+
			"gets one, not this node's share", got, want)
	}

	second := broker.Client()
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("second queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop(context.WithoutCancel(t.Context())) })
	convergingNode(t, second, "node-b", time.Minute, company).EnsureMailboxes(t.Context())

	if got := second.ConsumerProposals(); got != 0 {
		t.Fatalf("the peer proposed %d mailboxes, want 0: they all exist, and on the broker "+
			"this engine ships each proposal is a replicated write through the metadata group", got)
	}

	// And they are all still there, with whatever they were holding.
	subs, err := second.ListSubscriptions(t.Context(), topics.AgentInboxPrefix+">")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	var held []string
	for _, sub := range subs {
		if id, ok := topics.MailboxSeat(sub.Topic, sub.Group); ok {
			held = append(held, id.String())
		}
	}
	slices.Sort(held)
	want := []string{seatID("ceo").String(), seatID("pm").String(), seatID("swe").String()}
	slices.Sort(want)
	if !slices.Equal(held, want) {
		t.Fatalf("the broker holds mailboxes for %v, want %v", held, want)
	}
}

// failingQueue refuses an ensure a fixed number of times per subject, which is
// the shape a broker blip has: the call fails, the next one works.
type failingQueue struct {
	*recordingQueue
	mu     sync.Mutex
	refuse map[string]int
}

func (q *failingQueue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	q.mu.Lock()
	left := q.refuse[topic]
	if left > 0 {
		q.refuse[topic] = left - 1
	}
	q.mu.Unlock()
	if left > 0 {
		q.log.add("ensure " + topic)
		return false, errors.New("the broker did not answer")
	}
	return q.recordingQueue.EnsureSubscription(ctx, topic, group)
}
