package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// heardLive is what one node's sockets were handed, per domain: every inbox
// movement and every chat effect, and how many times each domain was told to
// read again. Its working-indicator half raises nothing ([quietPoster]): the
// relay never touches it.
type heardLive struct {
	quietPoster
	mu        sync.Mutex
	moved     []worklet.InboxMovement
	chat      []chatlet.Applied
	inboxGaps int
	chatGaps  int
	ping      chan struct{}
	// socketless is a hub attached for the working indicator alone, on a
	// node that serves no API ([ChatLive.ServesSockets]).
	socketless bool
}

func newHeardLive() *heardLive { return &heardLive{ping: make(chan struct{}, 256)} }

func (h *heardLive) deliver(moved []worklet.InboxMovement) {
	h.mu.Lock()
	h.moved = append(h.moved, moved...)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardLive) gap() {
	h.mu.Lock()
	h.inboxGaps++
	h.mu.Unlock()
	h.ping <- struct{}{}
}

// Applied and Reread make heardLive the chat live half a hub would be.
func (h *heardLive) Applied(batch []chatlet.Applied) {
	h.mu.Lock()
	h.chat = append(h.chat, batch...)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardLive) Reread() {
	h.mu.Lock()
	h.chatGaps++
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardLive) ServesSockets() bool { return !h.socketless }

// quietPoster is a [notify.StatusPoster] that shows nothing.
type quietPoster struct{}

func (quietPoster) StatusBackend() string                                          { return chatlet.Source }
func (quietPoster) SupportsStatusText() bool                                       { return true }
func (quietPoster) StatusRefresh() time.Duration                                   { return time.Minute }
func (quietPoster) AddressRule() notify.AddressRule                                { return chatlet.AddressRule() }
func (quietPoster) SetStatus(context.Context, string, string, string, string) bool { return true }
func (quietPoster) ClearStatus(context.Context, string, string, string) bool       { return true }

// heard is one snapshot of what a node's sockets were handed.
type heard struct {
	moved               []worklet.InboxMovement
	chat                []chatlet.Applied
	inboxGaps, chatGaps int
}

func (h *heardLive) snapshot() heard {
	h.mu.Lock()
	defer h.mu.Unlock()
	return heard{moved: append([]worklet.InboxMovement(nil), h.moved...),
		chat:      append([]chatlet.Applied(nil), h.chat...),
		inboxGaps: h.inboxGaps, chatGaps: h.chatGaps}
}

// waitFor waits until cond holds of what was heard, or fails naming what.
func (h *heardLive) waitFor(t *testing.T, what string, cond func(heard) bool) {
	t.Helper()
	deadline := time.After(2 * liveRelayWait)
	for {
		if got := h.snapshot(); cond(got) {
			return
		}
		select {
		case <-h.ping:
		case <-deadline:
			got := h.snapshot()
			t.Fatalf("%s never happened: heard %v, chat %v, %d inbox and %d chat gaps",
				what, handles(got.moved), messages(got.chat), got.inboxGaps, got.chatGaps)
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

// messages is every chat effect's message, in the order heard.
func messages(applied []chatlet.Applied) []string {
	out := make([]string, 0, len(applied))
	for _, a := range applied {
		out = append(out, a.MessageID)
	}
	return out
}

// posted is one committed chat post, as an applier announces it.
func posted(message string) []chatlet.Applied {
	return []chatlet.Applied{{
		Position: statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: 9},
		Subject:  chatlet.MessageSubject("c-1"), Op: chatlet.OpPost, OpID: "op-" + message,
		ChannelID: "c-1", MessageID: message, ChannelSeq: 3,
		Actor: "ada", ActorKind: chatlet.AuthorHuman,
		At: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC),
	}}
}

// relayNode is one data node's relay over a shared in-memory broker, with its
// copy's verdict a switch the case holds.
type relayNode struct {
	relay   *liveRelay
	engine  *Engine
	heard   *heardLive
	faulted atomic.Bool
}

func newRelayNode(t *testing.T, broker *memory.Broker, self string,
	fleet []string) *relayNode {

	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start %s's queue: %v", self, err)
	}
	n := &relayNode{engine: &Engine{}, heard: newHeardLive()}
	n.engine.SetOnInboxMoved(n.heard.deliver)
	n.engine.SetOnInboxGap(n.heard.gap)
	n.engine.SetChatLive(t.Context(), n.heard)
	n.relay = newLiveRelay(self, q,
		func(context.Context) ([]string, error) { return fleet, nil },
		func(context.Context) bool { return n.faulted.Load() },
		n.engine.liveListening, n.engine.deliverLive, n.engine.liveGap)
	n.engine.liveRelay.Store(n.relay)
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

// A NODE WHOSE OWN COPY IS OUT OF SERVICE STILL TELLS ITS SOCKETS WHAT ITS
// APPLIERS WOULD HAVE — WHOSE INBOX MOVED AND WHAT WAS SAID IN A ROOM.
//
// A copy that is wrong goes out of service and its node stays in rotation, its
// dashboards reading through a sound peer; its appliers commit nothing anybody
// vouches for, and they were the only thing those dashboards heard from — so
// every socket on it went silent until it reconnected somewhere else, which
// nothing told it to. Here node b's copy is out of service: what a's appliers
// announce — an inbox movement and a chat post — reaches b's sockets, what b's
// own announce is withheld (one source at a time), each switch of source tells
// BOTH domains' screens to read again, and once b's copy is back its own
// appliers are heard again.
//
// Mutation: make the follower never follow (or let announce ignore whether it
// is being relayed), and the relayed batches never arrive (or b's own arrive
// beside them); drop a domain from deliverLive and its batch never reaches b;
// drop a domain from liveGap and its re-reads never come.
func TestAnOutOfServiceNodeHearsAPeersAppliers(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b"}
	a := newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	b.faulted.Store(true)
	b.follow(t)

	// THE SWITCH TO A PEER IS A RE-READ IN EVERY DOMAIN, once the follower
	// knows where the peer stands.
	b.heard.waitFor(t, "b switching to a's appliers", func(h heard) bool {
		return h.inboxGaps == 1 && h.chatGaps == 1
	})

	// b's own appliers — halted, wrong, or simply not the ones being heard
	// — are withheld; a's are relayed.
	b.engine.inboxMoved([]worklet.InboxMovement{{Handle: "from-b-own-applier", UnreadDelta: 1}})
	b.engine.chatApplied(posted("m-from-b-own-applier"))
	a.engine.inboxMoved([]worklet.InboxMovement{{Handle: "ana", UnreadDelta: 1,
		Subject: "t-1", Reason: worklet.ReasonAssignee}})
	a.engine.chatApplied(posted("m-1"))
	b.heard.waitFor(t, "a's batches reaching b's sockets", func(h heard) bool {
		return len(h.moved) > 0 && len(h.chat) > 0
	})
	got := b.heard.snapshot()
	if h := handles(got.moved); len(h) != 1 || h[0] != "ana" {
		t.Fatalf("b's sockets heard %v, want a's movement for ana alone — b's own "+
			"applier is not the source while its copy is out of service", h)
	}
	if got.moved[0].Subject != "t-1" || got.moved[0].Reason != worklet.ReasonAssignee {
		t.Errorf("the relayed movement lost its fields: %+v", got.moved[0])
	}
	if m := messages(got.chat); len(m) != 1 || m[0] != "m-1" {
		t.Fatalf("b's chat heard %v, want a's post m-1 alone", m)
	}
	if want := posted("m-1")[0]; got.chat[0] != want {
		t.Errorf("the relayed chat effect lost its fields:\n got %+v\nwant %+v",
			got.chat[0], want)
	}
	// a's own sockets heard a's appliers, as ever.
	if own := a.heard.snapshot(); len(own.moved) != 1 || len(own.chat) != 1 {
		t.Errorf("a's own sockets heard %v and %v, want its own appliers' batches",
			handles(own.moved), messages(own.chat))
	}

	// BACK IN SERVICE: b's sockets hear b's own appliers again, after a
	// re-read in both domains covering whatever the switch skipped.
	b.faulted.Store(false)
	a.engine.inboxMoved([]worklet.InboxMovement{{Handle: "bo", UnreadDelta: 1}})
	b.heard.waitFor(t, "b switching back to its own appliers", func(h heard) bool {
		return h.inboxGaps == 2 && h.chatGaps == 2
	})
	if b.relay.relaying.Load() {
		t.Fatal("b is still relaying a peer's batches with its own copy back in service")
	}
	b.engine.chatApplied(posted("m-from-b-own-applier"))
	b.heard.waitFor(t, "b's own chat applier being heard again", func(h heard) bool {
		m := messages(h.chat)
		return len(m) > 0 && m[len(m)-1] == "m-from-b-own-applier"
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
		func(h heard) bool { return h.inboxGaps == 1 })

	// a — the first in order — goes out of service; whatever it would say
	// now is not to be trusted, and c's appliers are.
	a.faulted.Store(true)
	a.engine.chatApplied(posted("m-from-a"))
	b.heard.waitFor(t, "b moving to c", func(h heard) bool { return h.inboxGaps == 2 })
	c.engine.chatApplied(posted("m-from-c"))
	b.heard.waitFor(t, "c's post reaching b", func(h heard) bool {
		for _, m := range messages(h.chat) {
			if m == "m-from-c" {
				return true
			}
		}
		return false
	})
	if m := messages(b.heard.snapshot().chat); m[0] != "m-from-c" {
		t.Errorf("b heard %v, want nothing of a's once a's copy was out of service", m)
	}
}

// A SERVER SAYS WHAT A FOLLOWER CANNOT PLACE ITSELF IN, rather than answering
// as though nothing were missing: a ring that no longer reaches back to the
// follower's last batch is a GAP — whether newer batches pushed it out or one
// was too large to keep at all — and a server back in service after its copy
// was out is another EPOCH, because what its appliers announced meanwhile is
// no sequence anybody can trust to be whole. Each is a re-read on the
// follower's side; answered as an ordinary "nothing new", either is
// announcements lost without a trace.
//
// Mutation: drop the eviction check and the first case answers no gap; keep
// an oversized batch's predecessors and the follower behind it is answered the
// batches around the hole; keep the epoch across a return to service and the
// last case answers the old one.
func TestAServerSaysWhatAFollowerCannotPlaceItselfIn(t *testing.T) {
	t.Parallel()
	var faulted atomic.Bool
	r := newLiveRelay("a", nil, nil,
		func(context.Context) bool { return faulted.Load() }, nil, nil, nil)
	ctx := t.Context()

	first, _ := r.answerNow(ctx, relayAsk{})
	// A RING BOUNDED BY WHAT IT HOLDS: enough of the largest batches the
	// chat domain makes to fill it more than twice over.
	big := make([]chatlet.Applied, 0, 64)
	for range 64 {
		a := posted("m")[0]
		a.Notify = &chatlet.Notify{Excerpt: strings.Repeat("x", chatlet.MaxExcerpt)}
		big = append(big, a)
	}
	for range 2 * liveRelayRingBytes / sizeOf(t, liveBatch{Chat: big}) {
		r.record(liveBatch{Chat: big})
	}
	got, _ := r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: 1})
	if !got.Gap || len(got.Batches) != 0 {
		t.Errorf("a follower behind the ring was answered %d batches (gap %v), want a gap",
			len(got.Batches), got.Gap)
	}
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head - 2})
	if got.Gap || len(got.Batches) != 2 {
		t.Errorf("a follower the ring still reaches was answered %d batches (gap %v), "+
			"want the two after it", len(got.Batches), got.Gap)
	}
	if held := r.held; held > liveRelayRingBytes {
		t.Errorf("the ring holds %d bytes, past its %d", held, liveRelayRingBytes)
	}

	// ONE BATCH NO ANSWER COULD CARRY is a gap for every follower behind
	// it, never a hole between the batches either side.
	head := got.Head
	r.record(liveBatch{Inbox: []worklet.InboxMovement{{Handle: "ana"}}})
	huge := make([]chatlet.Applied, 0, 8)
	for len(huge) == 0 || sizeOf(t, liveBatch{Chat: huge}) <= liveRelayRingBytes {
		a := posted("m")[0]
		a.Notify = &chatlet.Notify{Excerpt: strings.Repeat("y", 1<<20)}
		huge = append(huge, a)
	}
	r.record(liveBatch{Chat: huge})
	r.record(liveBatch{Inbox: []worklet.InboxMovement{{Handle: "bo"}}})
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: head + 1})
	if !got.Gap || len(got.Batches) != 0 {
		t.Errorf("a follower behind a batch too large to keep was answered %d batches "+
			"(gap %v), want a gap", len(got.Batches), got.Gap)
	}
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head - 1})
	if got.Gap || len(got.Batches) != 1 {
		t.Errorf("a follower past the oversized batch was answered %d batches (gap %v), "+
			"want the one after it", len(got.Batches), got.Gap)
	}

	faulted.Store(true)
	if got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head}); !got.Unserved {
		t.Fatalf("a server whose copy is out of service answered %+v, want unserved", got)
	}
	faulted.Store(false)
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head})
	if got.Epoch == first.Epoch {
		t.Errorf("a server back in service kept its epoch %q, so a follower would "+
			"read on as though its own appliers had announced everything meanwhile",
			got.Epoch)
	}
}

// sizeOf is what one batch weighs in the ring.
func sizeOf(t *testing.T, b liveBatch) int {
	t.Helper()
	raw, err := json.Marshal(relayedBatch{Seq: 1, liveBatch: b})
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// A NODE NOBODY LISTENS ON RELAYS NOTHING, AND ONE LISTENER IS ENOUGH. A data
// node with nothing attached has no sockets to tell, so a follower there would
// be a loop asking a peer every few seconds for batches it hands to nobody —
// and logging that it relays. So is a node whose only attachment is a chat hub
// that serves no socket: the process attaches its hub on EVERY node, for the
// working indicator, and a data node without `ingress` is one of them. But a
// node whose only listener is a chat hub serving its sockets is one whose
// rooms would otherwise go quiet.
//
// Mutation: drop the listening check from follow, and b starts relaying; drop
// chat from liveListening, and c never does; let chatListening take any
// attached hub, and d starts relaying.
func TestANodeNobodyListensOnRelaysNothing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b", "c", "d"}
	newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	b.engine.onInbox.Store(nil)
	b.engine.SetChatLive(t.Context(), nil)
	b.faulted.Store(true)
	b.follow(t)
	d := newRelayNode(t, broker, "d", fleet)
	d.engine.onInbox.Store(nil)
	d.heard.socketless = true
	d.faulted.Store(true)
	d.follow(t)
	c := newRelayNode(t, broker, "c", fleet)
	c.engine.onInbox.Store(nil)
	c.faulted.Store(true)
	c.follow(t)
	c.heard.waitFor(t, "c, whose only listener is chat, relaying",
		func(h heard) bool { return h.chatGaps == 1 })
	time.Sleep(3 * servingRecheck)
	if b.relay.relaying.Load() {
		t.Fatal("a node with nothing listening for what its appliers announce is " +
			"relaying a peer's")
	}
	if d.relay.relaying.Load() {
		t.Fatal("a node whose only attachment is a chat hub serving no socket is " +
			"relaying a peer's batches to nobody")
	}
}
