package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/worklet"
)

// heardInbox is what one node's sockets were handed: every movement, and how
// many times they were told to read every inbox again.
type heardInbox struct {
	mu    sync.Mutex
	moved []worklet.InboxMovement
	gaps  int
	ping  chan struct{}
}

func newHeardInbox() *heardInbox { return &heardInbox{ping: make(chan struct{}, 64)} }

func (h *heardInbox) deliver(moved []worklet.InboxMovement) {
	h.mu.Lock()
	h.moved = append(h.moved, moved...)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardInbox) gap() {
	h.mu.Lock()
	h.gaps++
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardInbox) snapshot() ([]worklet.InboxMovement, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]worklet.InboxMovement(nil), h.moved...), h.gaps
}

// waitFor waits until cond holds of what was heard, or fails naming what.
func (h *heardInbox) waitFor(t *testing.T, what string,
	cond func([]worklet.InboxMovement, int) bool) {

	t.Helper()
	deadline := time.After(2 * inboxRelayWait)
	for {
		if moved, gaps := h.snapshot(); cond(moved, gaps) {
			return
		}
		select {
		case <-h.ping:
		case <-deadline:
			moved, gaps := h.snapshot()
			t.Fatalf("%s never happened: heard %v and %d gaps", what, moved, gaps)
		}
	}
}

// handles is every movement's handle, in the order heard.
func handles(moved []worklet.InboxMovement) []string {
	out := make([]string, 0, len(moved))
	for _, m := range moved {
		out = append(out, m.Handle)
	}
	return out
}

// relayNode is one data node's relay over a shared in-memory broker, with its
// copy's verdict a switch the case holds.
type relayNode struct {
	relay   *inboxRelay
	engine  *Engine
	heard   *heardInbox
	faulted atomic.Bool
}

func newRelayNode(t *testing.T, broker *memory.Broker, self string,
	fleet []string) *relayNode {

	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start %s's queue: %v", self, err)
	}
	n := &relayNode{engine: &Engine{}, heard: newHeardInbox()}
	n.engine.SetOnInboxMoved(n.heard.deliver)
	n.engine.SetOnInboxGap(n.heard.gap)
	n.relay = newInboxRelay(self, q,
		func(context.Context) ([]string, error) { return fleet, nil },
		func(context.Context) bool { return n.faulted.Load() },
		n.engine.inboxListening, n.engine.deliverInbox, n.engine.inboxGap)
	n.engine.inboxRelay.Store(n.relay)
	stop, err := n.relay.serve(t.Context())
	if err != nil {
		t.Fatalf("serve %s's relay: %v", self, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return n
}

// follow runs n's follower until the case ends.
func (n *relayNode) follow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.relay.follow(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// A NODE WHOSE OWN COPY IS OUT OF SERVICE STILL TELLS ITS SOCKETS WHOSE INBOX
// MOVED.
//
// A copy that is wrong goes out of service and its node stays in rotation, its
// dashboards reading through a sound peer; its applier commits nothing anybody
// vouches for, and it was the only thing those dashboards heard movements
// from — so every socket on it went silent until it reconnected somewhere
// else, which nothing told it to. Here node b's copy is out of service: what
// a's applier announces reaches b's sockets, what b's own applier announces is
// withheld (one source at a time), each switch of source tells every watched
// inbox to read again, and once b's copy is back its own applier is heard
// again.
//
// Mutation: make the follower never follow (or let inboxMoved ignore whether
// it is being relayed), and the relayed movement never arrives (or b's own
// arrives beside it); drop either gap and the re-reads never come.
func TestAnOutOfServiceNodeHearsAPeersInboxMovements(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b"}
	a := newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	b.faulted.Store(true)
	b.follow(t)

	// THE SWITCH TO A PEER IS A RE-READ, once the follower knows where the
	// peer stands.
	b.heard.waitFor(t, "b switching to a's movements",
		func(_ []worklet.InboxMovement, gaps int) bool { return gaps == 1 })

	// b's own applier — halted, wrong, or simply not the one being heard —
	// is withheld; a's is relayed.
	b.engine.inboxMoved([]worklet.InboxMovement{{Handle: "from-b-own-applier", UnreadDelta: 1}})
	a.engine.inboxMoved([]worklet.InboxMovement{{Handle: "ana", UnreadDelta: 1,
		Subject: "t-1", Reason: worklet.ReasonAssignee}})
	b.heard.waitFor(t, "a's movement reaching b's sockets",
		func(moved []worklet.InboxMovement, _ int) bool { return len(moved) > 0 })
	moved, _ := b.heard.snapshot()
	if got := handles(moved); len(got) != 1 || got[0] != "ana" {
		t.Fatalf("b's sockets heard %v, want a's movement for ana alone — b's own "+
			"applier is not the source while its copy is out of service", got)
	}
	if moved[0].Subject != "t-1" || moved[0].Reason != worklet.ReasonAssignee {
		t.Errorf("the relayed movement lost its fields: %+v", moved[0])
	}
	// a's own sockets heard a's applier, as ever.
	if own, _ := a.heard.snapshot(); len(own) != 1 || own[0].Handle != "ana" {
		t.Errorf("a's own sockets heard %v, want its own applier's movement", handles(own))
	}

	// BACK IN SERVICE: b's sockets hear b's own applier again, after a
	// re-read covering whatever the switch skipped.
	b.faulted.Store(false)
	a.engine.inboxMoved([]worklet.InboxMovement{{Handle: "bo", UnreadDelta: 1}})
	b.heard.waitFor(t, "b switching back to its own applier",
		func(_ []worklet.InboxMovement, gaps int) bool { return gaps == 2 })
	if b.relay.relaying.Load() {
		t.Fatal("b is still relaying a peer's movements with its own copy back in service")
	}
	b.engine.inboxMoved([]worklet.InboxMovement{{Handle: "from-b-own-applier", UnreadDelta: 1}})
	b.heard.waitFor(t, "b's own applier being heard again",
		func(moved []worklet.InboxMovement, _ int) bool {
			got := handles(moved)
			return len(got) > 0 && got[len(got)-1] == "from-b-own-applier"
		})
}

// A FOLLOWER WHOSE SERVER STOPS SERVING MOVES TO ANOTHER, AND RE-READS.
//
// A server answers only while its own copy serves, and a follower that loses
// its server — out of service itself, or gone — must not go silent while
// another data node could serve it. The sequence it read is the lost server's
// own, so the move is a re-read rather than a guess at where the next one
// stands.
//
// Mutation: let a server answer whatever its copy's state, and b goes on
// hearing nothing from c; keep the follower on its first server, likewise.
func TestAFollowerMovesToAnotherServerWhenItsServerStopsServing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b", "c"}
	a := newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	c := newRelayNode(t, broker, "c", fleet)
	b.faulted.Store(true)
	b.follow(t)
	b.heard.waitFor(t, "b following its first server",
		func(_ []worklet.InboxMovement, gaps int) bool { return gaps == 1 })

	// a — the first in order — goes out of service; whatever it would say
	// now is not to be trusted, and c's applier is.
	a.faulted.Store(true)
	a.engine.inboxMoved([]worklet.InboxMovement{{Handle: "from-a", UnreadDelta: 1}})
	b.heard.waitFor(t, "b moving to c",
		func(_ []worklet.InboxMovement, gaps int) bool { return gaps == 2 })
	c.engine.inboxMoved([]worklet.InboxMovement{{Handle: "from-c", UnreadDelta: 1}})
	b.heard.waitFor(t, "c's movement reaching b",
		func(moved []worklet.InboxMovement, _ int) bool {
			for _, h := range handles(moved) {
				if h == "from-c" {
					return true
				}
			}
			return false
		})
}

// A SERVER SAYS WHAT A FOLLOWER CANNOT PLACE ITSELF IN, rather than answering
// as though nothing were missing: a ring that no longer reaches back to the
// follower's last batch is a GAP, and a server back in service after its copy
// was out is another EPOCH, because what its applier announced meanwhile is no
// sequence anybody can trust to be whole. Each is a re-read on the follower's
// side; answered as an ordinary "nothing new", either is movements lost
// without a trace.
//
// Mutation: drop the eviction check and the first case answers no gap; keep
// the epoch across a return to service and the second answers the old one.
func TestAServerSaysWhatAFollowerCannotPlaceItselfIn(t *testing.T) {
	t.Parallel()
	var faulted atomic.Bool
	r := newInboxRelay("a", nil, nil,
		func(context.Context) bool { return faulted.Load() }, nil, nil, nil)
	ctx := t.Context()

	first, _ := r.answerNow(ctx, relayAsk{})
	for range inboxRelayRing + 5 {
		r.record([]worklet.InboxMovement{{Handle: "ana", UnreadDelta: 1}})
	}
	got, _ := r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: 1})
	if !got.Gap || len(got.Batches) != 0 {
		t.Errorf("a follower behind the ring was answered %+v, want a gap", got)
	}
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head - 2})
	if got.Gap || len(got.Batches) != 2 {
		t.Errorf("a follower the ring still reaches was answered %d batches (gap %v), "+
			"want the two after it", len(got.Batches), got.Gap)
	}

	faulted.Store(true)
	if got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head}); !got.Unserved {
		t.Fatalf("a server whose copy is out of service answered %+v, want unserved", got)
	}
	faulted.Store(false)
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head})
	if got.Epoch == first.Epoch {
		t.Errorf("a server back in service kept its epoch %q, so a follower would "+
			"read on as though its own applier had announced everything meanwhile",
			got.Epoch)
	}
}

// A NODE NOBODY LISTENS ON RELAYS NOTHING. A data node that serves no API has
// no sockets to tell, so a follower there would be a loop asking a peer every
// few seconds for movements it hands to nobody — and logging that it relays.
//
// Mutation: drop the listening check from follow, and b starts relaying.
func TestANodeNobodyListensOnRelaysNothing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b"}
	newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	b.engine.onInbox.Store(nil)
	b.faulted.Store(true)
	b.follow(t)
	time.Sleep(3 * servingRecheck)
	if b.relay.relaying.Load() {
		t.Fatal("a node with nothing listening for its inbox movements is " +
			"relaying a peer's")
	}
}
