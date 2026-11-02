package node_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// wakeBroker is a started twin whose ensures are journalled.
func wakeBroker(t *testing.T) (*recordingQueue, *callLog) {
	t.Helper()
	log := &callLog{}
	q := &recordingQueue{Queue: qmem.New(), log: log}
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q, log
}

// wakesNode is a node over q serving a company of two seats, never started,
// so no mailbox pass has run on it: the state a node is in while its boot
// starts the producers.
func wakesNode(t *testing.T, q queue.EventQueue) *node.Node {
	t.Helper()
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{},
		NodeID: "node-a", Owner: "node-a-1",
		Seats: seatsOf("ceo", "swe"),
		Turn:  func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return n
}

// publishingQueue journals each publish beside the ensures recordingQueue
// already journals, so a case can assert which came first.
type publishingQueue struct {
	*recordingQueue
}

func (q *publishingQueue) Publish(ctx context.Context, topic string, ev *events.Event) error {
	q.log.add("publish " + topic)
	return q.recordingQueue.Publish(ctx, topic, ev)
}

// partialQueue lists one seat's mailbox as missing its priority lane, which is
// how a broker answers for a mailbox made before the lane existed or one whose
// lane somebody deleted.
type partialQueue struct {
	*publishingQueue
	partial string
}

func (q *partialQueue) ListSubscriptions(ctx context.Context, pattern string) ([]queue.Subscription, error) {
	subs, err := q.publishingQueue.ListSubscriptions(ctx, pattern)
	for i := range subs {
		if subs[i].Topic == q.partial {
			subs[i].Partial = true
		}
	}
	return subs, err
}

func wake(label string) *events.Event {
	return &events.Event{Type: label}
}

// A WAKE PUBLISHED BEFORE THE FIRST PASS REACHES ITS SEAT.
//
// The boot order this guards: a node starts its scheduler, its change feeds
// and its inbound router before the mailbox pass has run, and the stream drops
// what is published to a subject no consumer covers. A wake sent through the
// node's wake queue makes the mailbox first; one sent straight to the queue,
// which is what every producer did, is gone.
func TestAWakeBeforeTheFirstPassReachesItsSeat(t *testing.T) {
	q, _ := wakeBroker(t)
	n := wakesNode(t, q)

	if err := q.Queue.Publish(t.Context(), inbox("swe"), wake("straight")); err != nil {
		t.Fatalf("publish straight to the queue: %v", err)
	}
	if err := n.Wakes().Publish(t.Context(), inbox("ceo"), wake("through-the-node")); err != nil {
		t.Fatalf("publish through the node's wake queue: %v", err)
	}

	if got := labelsOf(q.Queue.Backlog(inbox("ceo"), inboxGroup("ceo"))); !slices.Equal(got,
		[]string{"through-the-node"}) {
		t.Fatalf("the seat's mailbox holds %v, want the wake published through the node: "+
			"a wake sent before the first mailbox pass was dropped by the stream", got)
	}
	// The failure mode itself, so the case cannot pass on a twin that kept
	// mail nobody subscribed to.
	if got := q.Queue.Backlog(inbox("swe"), inboxGroup("swe")); len(got) != 0 {
		t.Fatalf("a wake published to a seat with no mailbox was kept (%v); this twin no "+
			"longer drops what no consumer covers, so it certifies nothing here", labelsOf(got))
	}
}

// A MAILBOX MISSING ITS LANE IS COMPLETED BEFORE A WAKE IS SENT TO IT. A
// person's message rides the lane, and a lane with no consumer drops it while
// the inbox beside it looks healthy.
func TestAWakeToAMailboxMissingItsLaneCompletesItFirst(t *testing.T) {
	q, log := wakeBroker(t)
	pq := &partialQueue{publishingQueue: &publishingQueue{recordingQueue: q}, partial: inbox("ceo")}
	if _, err := q.Queue.EnsureSubscription(t.Context(), inbox("ceo"), inboxGroup("ceo")); err != nil {
		t.Fatalf("the mailbox from before the lane: %v", err)
	}
	if _, err := q.Queue.EnsureSubscription(t.Context(), inbox("swe"), inboxGroup("swe")); err != nil {
		t.Fatalf("swe's mailbox: %v", err)
	}
	n := wakesNode(t, pq)

	person := wake("person")
	queue.Prioritize(person)
	if err := n.Wakes().Publish(t.Context(), inbox("ceo"), person); err != nil {
		t.Fatalf("publish through the node's wake queue: %v", err)
	}

	want := []string{"ensure " + inbox("ceo"), "publish " + inbox("ceo")}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("the node made %v, want %v: the partial mailbox must be ensured, and only it, "+
			"before the wake is sent", got, want)
	}
}

// A WAKE THE NODE CANNOT GIVE A MAILBOX IS REFUSED, NOT DROPPED. The refusal is
// the broker-unavailable condition every producer already retries; a publish
// that went ahead would be discarded by the stream and reported as sent.
func TestAWakeToASeatWithNoMailboxIsRefused(t *testing.T) {
	q, _ := wakeBroker(t)
	n := wakesNode(t, &failingQueue{recordingQueue: q, refuse: map[string]int{inbox("ceo"): 1000}})

	err := n.Wakes().Publish(t.Context(), inbox("ceo"), wake("refused"))
	if !errors.Is(err, queue.ErrUnavailable) {
		t.Fatalf("publish to a seat whose mailbox cannot be made returned %v, want an error "+
			"that is queue.ErrUnavailable, which the producers retry", err)
	}
	if _, err := q.Queue.EnsureSubscription(t.Context(), inbox("ceo"), inboxGroup("ceo")); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := q.Queue.Backlog(inbox("ceo"), inboxGroup("ceo")); len(got) != 0 {
		t.Fatalf("a refused wake reached the broker anyway: %v", labelsOf(got))
	}
}

// A CONFIRMED MAILBOX COSTS NOTHING, and a topic that is not a seat's inbox is
// published as it is: the check is a lookup in the node's own set, and it
// makes no mailbox for a subject that is not a seat.
func TestAWakeToAConfirmedSeatMakesNoBrokerCall(t *testing.T) {
	q, log := wakeBroker(t)
	n := wakesNode(t, &publishingQueue{recordingQueue: q})
	n.EnsureMailboxes(t.Context())
	log.reset()

	if err := n.Wakes().Publish(t.Context(), inbox("ceo"), wake("steady")); err != nil {
		t.Fatalf("publish to a confirmed seat: %v", err)
	}
	other := topics.Event("agent_phase_started")
	if err := n.Wakes().Publish(t.Context(), other, wake("not-a-seat")); err != nil {
		t.Fatalf("publish to a topic that is not a seat: %v", err)
	}

	want := []string{"publish " + inbox("ceo"), "publish " + other}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("the node made %v, want only the two publishes %v", got, want)
	}
}

// listingQueue journals each subscription listing beside the publishes
// publishingQueue journals.
type listingQueue struct {
	*publishingQueue
}

func (q *listingQueue) ListSubscriptions(ctx context.Context, pattern string) ([]queue.Subscription, error) {
	q.log.add("list " + pattern)
	return q.publishingQueue.ListSubscriptions(ctx, pattern)
}

// A WAKE FOR A SEAT OUTSIDE THE COMPANY RUNS NO PASS. Its mailbox is the
// retirement sweep's, so the node has nothing to confirm, and a pass on its
// behalf would list the broker's subscriptions once per wake, under the lock
// every other wake's confirmation waits on: a feed's backlog for a removed seat
// is one wake per record. The company's own seats have no mailbox here, so a
// pass that did run would have to list.
func TestAWakeForASeatOutsideTheCompanyRunsNoPass(t *testing.T) {
	q, log := wakeBroker(t)
	n := wakesNode(t, &listingQueue{publishingQueue: &publishingQueue{recordingQueue: q}})

	if err := n.Wakes().Publish(t.Context(), inbox("retired"), wake("late")); err != nil {
		t.Fatalf("publish to a seat outside the company: %v", err)
	}
	if err := n.ConfirmMailbox(t.Context(), seatID("retired")); err != nil {
		t.Fatalf("confirm a seat outside the company: %v", err)
	}

	want := []string{"publish " + inbox("retired")}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("the node made %v, want only the publish %v: a seat outside the company "+
			"ran a mailbox pass", got, want)
	}
}

func labelsOf(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}
