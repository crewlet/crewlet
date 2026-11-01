package engine

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// relayInboxSource is the inbox source the cases' movements name: the
// tracker's, whose applier announces them.
var relayInboxSource = worklet.Domain{}.Name()

// heardLive is what one node's sockets were handed, per domain: every inbox
// movement, every chat effect and every artifact and datastore movement, and
// how many times each domain was told to read again — per inbox source, and
// for the artifact domains how many resets they were handed, which is how a
// gap reaches them.
// Its working-indicator half raises nothing ([quietPoster]): the relay never
// touches it.
type heardLive struct {
	quietPoster
	mu         sync.Mutex
	moved      []inbox.Movement
	chat       []chatlet.Applied
	artifacts  []artifactsdomain.Moved
	data       []artifactdata.Moved
	inboxGaps  map[string]int
	chatGaps   int
	artifactRs int
	dataResets int
	ping       chan struct{}
	// socketless is a hub attached for the working indicator alone, on a
	// node that serves no API ([ChatLive.ServesSockets]).
	socketless bool
}

func newHeardLive() *heardLive {
	return &heardLive{inboxGaps: map[string]int{}, ping: make(chan struct{}, 256)}
}

func (h *heardLive) deliver(moved []inbox.Movement) {
	h.mu.Lock()
	h.moved = append(h.moved, moved...)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardLive) gap(source string) {
	h.mu.Lock()
	h.inboxGaps[source]++
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

// artifactsMoved and dataMoved make heardLive the socket's artifact rooms, as
// [Engine.OnArtifactsMoved] and [Engine.OnArtifactDataMoved] hand them.
func (h *heardLive) artifactsMoved(moved []artifactsdomain.Moved) {
	h.mu.Lock()
	for _, m := range moved {
		if m.What == artifactsdomain.MoveReset {
			h.artifactRs++
			continue
		}
		h.artifacts = append(h.artifacts, m)
	}
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardLive) dataMoved(moved []artifactdata.Moved) {
	h.mu.Lock()
	for _, m := range moved {
		if m.Reset {
			h.dataResets++
			continue
		}
		h.data = append(h.data, m)
	}
	h.mu.Unlock()
	h.ping <- struct{}{}
}

// quietPoster is a [notify.StatusPoster] that shows nothing.
type quietPoster struct{}

func (quietPoster) StatusBackend() string                                    { return chatlet.Source }
func (quietPoster) SupportsStatusText() bool                                 { return true }
func (quietPoster) StatusRefresh() time.Duration                             { return time.Minute }
func (quietPoster) AddressRule() notify.AddressRule                          { return chatlet.AddressRule() }
func (quietPoster) SetStatus(context.Context, notify.StatusPost) bool        { return true }
func (quietPoster) ClearStatus(context.Context, string, string, string) bool { return true }

// heard is one snapshot of what a node's sockets were handed.
type heard struct {
	moved                  []inbox.Movement
	chat                   []chatlet.Applied
	artifacts              []artifactsdomain.Moved
	data                   []artifactdata.Moved
	inboxGaps              map[string]int
	chatGaps               int
	artifactRs, dataResets int
}

// rereadEverywhere reports whether every domain — each inbox source among
// them — was told to read again n times.
func (h heard) rereadEverywhere(n int) bool {
	for _, source := range inboxSources() {
		if h.inboxGaps[source] != n {
			return false
		}
	}
	return h.chatGaps == n && h.artifactRs == n && h.dataResets == n
}

func (h *heardLive) snapshot() heard {
	h.mu.Lock()
	defer h.mu.Unlock()
	return heard{moved: append([]inbox.Movement(nil), h.moved...),
		chat:      append([]chatlet.Applied(nil), h.chat...),
		artifacts: append([]artifactsdomain.Moved(nil), h.artifacts...),
		data:      append([]artifactdata.Moved(nil), h.data...),
		inboxGaps: maps.Clone(h.inboxGaps), chatGaps: h.chatGaps,
		artifactRs: h.artifactRs, dataResets: h.dataResets}
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
			t.Fatalf("%s never happened: heard %v, chat %v, %d artifact and %d "+
				"datastore movements, inbox gaps %v and %d chat gaps, %d artifact and %d "+
				"datastore resets", what, handles(got.moved), messages(got.chat),
				len(got.artifacts), len(got.data), got.inboxGaps, got.chatGaps,
				got.artifactRs, got.dataResets)
		}
	}
}

// handles is every movement's handle, in the order heard.
func handles(moved []inbox.Movement) []string {
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

// versioned is one committed artifact version, as its applier announces it.
func versioned(id string) []artifactsdomain.Moved {
	return []artifactsdomain.Moved{{Artifact: artifacts.IDFor(id),
		What: artifactsdomain.MoveVersion, AccessEpoch: 2, Head: 3,
		Position: statelog.Position{Stream: topics.ArtifactsLogStream, Generation: 1, Seq: 7}}}
}

// written is one committed datastore write, as its applier announces it: a
// document whole, which is what a room's readers are shown.
func written(doc string) []artifactdata.Moved {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	return []artifactdata.Moved{{Artifact: artifacts.IDFor("sheet"),
		Docs: []artifactdata.DocDelta{{Collection: "rows", Doc: doc, Version: 4,
			Body: json.RawMessage(`{"status":"done"}`), Read: artifacts.ReadAll,
			CreatedBy: "ada", UpdatedBy: iam.Actor{Name: "ada", Kind: iam.ActorHuman},
			UpdatedAt: at}},
		Writes: []artifactdata.WriteSummary{{Seq: 4, OpID: "op-" + doc,
			Op: artifactdata.OpWrite, Verdict: artifacts.DataApplied,
			Actor: iam.Actor{Name: "ada", Kind: iam.ActorHuman}, At: at}},
		Through: statelog.Position{Stream: topics.ArtifactDataLogStream, Generation: 1, Seq: 4}}}
}

// relayRing is the fleet keyring every relay node of a case signs under.
var relayRing = statelog.OneKey("k1", "the live relay cases' fleet key material")

// relayWireFor is one node's relay envelopes over ring.
func relayWireFor(t *testing.T, ring statelog.Keyring) *relayWire {
	t.Helper()
	wire, err := newRelayWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// relayNode is one data node's relay over a shared in-memory broker, with its
// copies' verdict two switches the case holds — every domain's copy out of
// service (faulted), or only the domains named (wrong) — and the stops of its
// artifact listeners for a case about a node that has none.
type relayNode struct {
	relay         *liveRelay
	engine        *Engine
	heard         *heardLive
	faulted       atomic.Bool
	wrong         atomic.Pointer[[]string]
	stopArtifacts func()
}

// outOfService is n's copies' verdict as the case holds it, in the order the
// domains are asked about, as [localEstate.outOfService] answers.
func (n *relayNode) outOfService(_ context.Context, domains ...string) []string {
	if n.faulted.Load() {
		return slices.Clone(domains)
	}
	wrong := n.wrong.Load()
	if wrong == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(domains), func(d string) bool {
		return !slices.Contains(*wrong, d)
	})
}

// relaying reports whether n's sockets hear any domain from a peer now.
func (n *relayNode) relaying() bool {
	relayed := n.relay.relayed.Load()
	return relayed != nil && len(*relayed) > 0
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
	stopMoved := n.engine.OnArtifactsMoved(n.heard.artifactsMoved)
	stopData := n.engine.OnArtifactDataMoved(n.heard.dataMoved)
	n.stopArtifacts = func() { stopMoved(); stopData() }
	n.relay = newLiveRelay(self, q, relayWireFor(t, relayRing),
		func(context.Context) ([]string, error) { return fleet, nil },
		liveDomains(), n.outOfService,
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
// APPLIERS WOULD HAVE — WHOSE INBOX MOVED, WHAT WAS SAID IN A ROOM, AND WHAT
// MOVED ON AN ARTIFACT AND IN ITS DATASTORE.
//
// A copy that is wrong goes out of service and its node stays in rotation, its
// dashboards reading through a sound peer; its appliers commit nothing anybody
// vouches for, and they were the only thing those dashboards heard from — so
// every socket on it went silent until it reconnected somewhere else, which
// nothing told it to. Here node b's copy is out of service: what a's appliers
// announce — an inbox movement, a chat post, an artifact's new version and a
// datastore write — reaches b's sockets, what b's own announce is withheld
// (one source at a time), each switch of source tells EVERY domain's screens
// to read again (the artifact domains as their own reset), and once b's copy
// is back its own appliers are heard again.
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
		return h.rereadEverywhere(1)
	})

	// b's own appliers — halted, wrong, or simply not the ones being heard
	// — are withheld; a's are relayed.
	b.engine.inboxMoved([]inbox.Movement{{Source: relayInboxSource,
		Handle: "from-b-own-applier", UnreadDelta: 1}})
	b.engine.chatApplied(posted("m-from-b-own-applier"))
	b.engine.artifactsMoved(versioned("from-b-own-applier"))
	b.engine.artifactDataMoved(written("from-b-own-applier"))
	stored := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	a.engine.inboxMoved([]inbox.Movement{{Source: relayInboxSource, Handle: "ana",
		UnreadDelta: 1, Subject: "t-1", Reason: string(worklet.ReasonAssignee), At: stored}})
	a.engine.chatApplied(posted("m-1"))
	a.engine.artifactsMoved(versioned("plan"))
	a.engine.artifactDataMoved(written("row-1"))
	b.heard.waitFor(t, "a's batches reaching b's sockets", func(h heard) bool {
		return len(h.moved) > 0 && len(h.chat) > 0 && len(h.artifacts) > 0 && len(h.data) > 0
	})
	got := b.heard.snapshot()
	if h := handles(got.moved); len(h) != 1 || h[0] != "ana" {
		t.Fatalf("b's sockets heard %v, want a's movement for ana alone — b's own "+
			"applier is not the source while its copy is out of service", h)
	}
	// EVERY FIELD CROSSES, the broker's instant among them: b times its push
	// from the instant a's record was stored, as a's own push is.
	if got.moved[0].Subject != "t-1" || got.moved[0].Reason != string(worklet.ReasonAssignee) ||
		!got.moved[0].At.Equal(stored) {
		t.Errorf("the relayed movement lost its fields: %+v", got.moved[0])
	}
	if m := messages(got.chat); len(m) != 1 || m[0] != "m-1" {
		t.Fatalf("b's chat heard %v, want a's post m-1 alone", m)
	}
	if want := posted("m-1")[0]; !reflect.DeepEqual(got.chat[0], want) {
		t.Errorf("the relayed chat effect lost its fields:\n got %+v\nwant %+v",
			got.chat[0], want)
	}
	// AN ARTIFACT ROOM ON b HEARS a's APPLIERS TOO, every field a room's
	// push and a verdict cache read arriving as the applier announced it.
	if want := versioned("plan"); !reflect.DeepEqual(got.artifacts, want) {
		t.Errorf("b's artifact rooms heard %+v, want a's movement alone:\n%+v",
			got.artifacts, want)
	}
	if want := written("row-1"); !reflect.DeepEqual(got.data, want) {
		t.Errorf("b's artifact rooms heard the datastore move %+v, want a's write "+
			"alone:\n%+v", got.data, want)
	}
	// a's own sockets heard a's appliers, as ever.
	if own := a.heard.snapshot(); len(own.moved) != 1 || len(own.chat) != 1 ||
		len(own.artifacts) != 1 || len(own.data) != 1 {
		t.Errorf("a's own sockets heard %v, %v, %d artifact and %d datastore "+
			"movements, want its own appliers' batches", handles(own.moved),
			messages(own.chat), len(own.artifacts), len(own.data))
	}

	// BACK IN SERVICE: b's sockets hear b's own appliers again, after a
	// re-read in every domain covering whatever the switch skipped.
	b.faulted.Store(false)
	a.engine.inboxMoved([]inbox.Movement{{Source: relayInboxSource, Handle: "bo",
		UnreadDelta: 1}})
	b.heard.waitFor(t, "b switching back to its own appliers", func(h heard) bool {
		return h.rereadEverywhere(2)
	})
	if b.relaying() {
		t.Fatal("b is still relaying a peer's batches with its own copy back in service")
	}
	b.engine.chatApplied(posted("m-from-b-own-applier"))
	b.heard.waitFor(t, "b's own chat applier being heard again", func(h heard) bool {
		m := messages(h.chat)
		return len(m) > 0 && m[len(m)-1] == "m-from-b-own-applier"
	})
	b.engine.artifactsMoved(versioned("from-b-own-applier"))
	b.heard.waitFor(t, "b's own artifacts applier being heard again", func(h heard) bool {
		return len(h.artifacts) == 2 && h.artifacts[1].Artifact == artifacts.IDFor("from-b-own-applier")
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
		func(h heard) bool { return h.inboxGaps[relayInboxSource] == 1 })

	// a — the first in order — goes out of service; whatever it would say
	// now is not to be trusted, and c's appliers are.
	a.faulted.Store(true)
	a.engine.chatApplied(posted("m-from-a"))
	b.heard.waitFor(t, "b moving to c", func(h heard) bool { return h.inboxGaps[relayInboxSource] == 2 })
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
	r := newLiveRelay("a", nil, nil, nil, liveDomains(),
		func(_ context.Context, domains ...string) []string {
			if faulted.Load() {
				return domains
			}
			return nil
		}, nil, nil, nil)
	ctx := t.Context()

	first, _ := r.answerNow(ctx, relayAsk{})
	// AN ASK HELD OPEN throughout, so every batch is one a follower may
	// still ask for ([liveRelay.record]) however long the encoding takes.
	release := holdOpen(r)
	defer release()
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
	r.record(liveBatch{Inbox: []inbox.Movement{{Source: relayInboxSource, Handle: "ana"}}})
	huge := make([]chatlet.Applied, 0, 8)
	for len(huge) == 0 || sizeOf(t, liveBatch{Chat: huge}) <= liveRelayRingBytes {
		a := posted("m")[0]
		a.Notify = &chatlet.Notify{Excerpt: strings.Repeat("y", 1<<20)}
		huge = append(huge, a)
	}
	r.record(liveBatch{Chat: huge})
	r.record(liveBatch{Inbox: []inbox.Movement{{Source: relayInboxSource, Handle: "bo"}}})
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
	if got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head,
		Domains: []string{relayInboxSource}}); !got.Unserved {
		t.Fatalf("a server whose copy is out of service answered %+v, want unserved", got)
	}
	faulted.Store(false)
	got, _ = r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head,
		Domains: []string{relayInboxSource}})
	if got.Epoch == first.Epoch {
		t.Errorf("a server back in service kept its epoch %q, so a follower would "+
			"read on as though its own appliers had announced everything meanwhile",
			got.Epoch)
	}
}

// holdOpen counts one ask as held open on r until release — what a follower's
// ask waiting for a batch is ([liveRelay.answer]).
func holdOpen(r *liveRelay) (release func()) {
	r.mu.Lock()
	r.waiting++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.waiting--
		r.mu.Unlock()
	}
}

// A SERVER KEEPS ONLY WHAT A FOLLOWER CAN STILL ASK FOR.
//
// A follower new to a server is answered where it stands and nothing before
// it, so no follower is ever served a batch recorded before the last answer it
// took: a server that kept its ring whatever anybody asked would hold its whole
// bound on every data node, for followers that do not exist while every copy
// serves. So a batch recorded with nobody answered within [liveRelayHold] and
// no ask held open is numbered and not kept, and a follower that comes back
// after the hold ran out is answered a gap — never the batches either side of
// the ones it missed; a batch recorded within the hold, or while an ask is
// held open, is kept and answered.
//
// Mutation: keep every batch whatever anybody asked, and the first ring is not
// empty; drop the held-open count, and the batch recorded past the hold is not
// kept for the waiting ask; stamp no hold on an answer, and the follower that
// asked within it is answered a gap.
func TestARelayServerKeepsOnlyWhatAFollowerCanStillAskFor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newLiveRelay("a", nil, nil, nil, liveDomains(),
		func(context.Context, ...string) []string { return nil }, nil, nil, nil)

	r.record(liveBatch{Chat: posted("m-nobody-asked-for")})
	if len(r.ring) != 0 || r.held != 0 {
		t.Fatalf("a server nobody has asked kept %d batches (%d bytes)", len(r.ring), r.held)
	}

	first, _ := r.answerNow(ctx, relayAsk{})
	r.record(liveBatch{Chat: posted("m-1")})
	got, _ := r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: first.Head})
	if got.Gap || len(got.Batches) != 1 {
		t.Fatalf("a follower asking within the hold was answered %d batches (gap %v), "+
			"want the one recorded since its last answer", len(got.Batches), got.Gap)
	}

	// THE HOLD RUNS OUT: nothing recorded now is kept, unless an ask is
	// held open for it.
	r.mu.Lock()
	r.heldUntil = time.Time{}
	r.mu.Unlock()
	r.record(liveBatch{Chat: posted("m-past-the-hold")})
	if late, _ := r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: got.Head}); !late.Gap ||
		len(late.Batches) != 0 {
		t.Errorf("a follower back after the hold ran out was answered %d batches (gap %v), "+
			"want a gap", len(late.Batches), late.Gap)
	}
	r.mu.Lock()
	r.heldUntil = time.Time{}
	head := r.seq
	r.mu.Unlock()
	release := holdOpen(r)
	r.record(liveBatch{Chat: posted("m-while-held-open")})
	release()
	if held, _ := r.answerNow(ctx, relayAsk{Epoch: first.Epoch, After: head}); held.Gap ||
		len(held.Batches) != 1 {
		t.Errorf("an ask held open was answered %d batches (gap %v), want the one that "+
			"woke it", len(held.Batches), held.Gap)
	}
}

// A BATCH NOBODY CAN ASK FOR IS NEVER COPIED.
//
// Every data node records every committed batch of every announcing domain,
// and in the normal state — no copy out of service anywhere — nobody follows
// any of them, so what a batch costs there is what the relay costs the whole
// fleet: it is numbered, and nothing is encoded or allocated.
//
// NOT PARALLEL, because an allocation count is the whole process's, and over
// a hundred runs, which testing.AllocsPerRun divides and rounds down.
//
// Mutation: encode a batch whether or not anybody may ask for it, or replace
// the wake-up channel with nobody waiting on it, and a run allocates.
func TestABatchNobodyCanAskForIsNeverCopied(t *testing.T) {
	r := newLiveRelay("a", nil, nil, nil, liveDomains(),
		func(context.Context, ...string) []string { return nil }, nil, nil, nil)
	batch := liveBatch{Inbox: []inbox.Movement{{Source: relayInboxSource, Handle: "ana",
		UnreadDelta: 1, Subject: "t-1", Reason: string(worklet.ReasonAssignee)}}}
	if allocs := testing.AllocsPerRun(100, func() { r.record(batch) }); allocs != 0 {
		t.Errorf("recording a batch nobody follows allocated %v times, want none", allocs)
	}
	if len(r.ring) != 0 || r.held != 0 {
		t.Errorf("a server nobody follows holds %d batches (%d bytes)", len(r.ring), r.held)
	}
}

// ONE DOMAIN'S WRONG COPY IS RELAYED ALONE.
//
// A fault is one log's (ADR-0072): a node whose chat copy is out of service
// still has a sound tracker copy, whose applier's movements are complete. So
// its sockets hear chat from a peer — and its own chat applier is withheld —
// while its inbox movements are its own tracker applier's, unrelayed; only
// chat's screens re-read at the switch; and a server whose own chat copy is
// out of service answers a follower relaying chat unserved, while it still
// answers one that relays only the tracker.
//
// Mutation: switch every domain on any one's fault, and b's own inbox
// movement is withheld and a's relayed; gap every domain at a switch, and the
// inbox is re-read; answer a follower whatever its domains, and the server
// with a wrong chat copy serves it.
func TestOneDomainsWrongCopyIsRelayedAlone(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b"}
	a := newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	chat := []string{chatlet.Domain{}.Name()}
	b.wrong.Store(&chat)
	b.follow(t)
	b.heard.waitFor(t, "b switching chat to a's applier", func(h heard) bool {
		return h.chatGaps == 1
	})

	b.engine.chatApplied(posted("m-from-b-own-applier"))
	b.engine.inboxMoved([]inbox.Movement{{Source: relayInboxSource, Handle: "from-b-own-applier",
		UnreadDelta: 1}})
	a.engine.inboxMoved([]inbox.Movement{{Source: relayInboxSource, Handle: "from-a",
		UnreadDelta: 1}})
	a.engine.chatApplied(posted("m-from-a"))
	b.heard.waitFor(t, "a's post reaching b", func(h heard) bool {
		return slices.Contains(messages(h.chat), "m-from-a")
	})
	got := b.heard.snapshot()
	if m := messages(got.chat); !slices.Equal(m, []string{"m-from-a"}) {
		t.Errorf("b's chat heard %v, want a's post alone", m)
	}
	if h := handles(got.moved); !slices.Equal(h, []string{"from-b-own-applier"}) {
		t.Errorf("b's inbox heard %v, want its own sound tracker applier's movement "+
			"alone", h)
	}
	if got.inboxGaps[relayInboxSource] != 0 || got.artifactRs != 0 || got.dataResets != 0 {
		t.Errorf("a switch of chat re-read the inbox %v times and reset %d artifact and "+
			"%d datastore rooms, want chat's screens alone", got.inboxGaps,
			got.artifactRs, got.dataResets)
	}

	a.wrong.Store(&chat)
	if ans, _ := a.relay.answerNow(t.Context(), relayAsk{Domains: chat}); !ans.Unserved {
		t.Errorf("a server whose chat copy is out of service answered a follower "+
			"relaying chat %+v, want unserved", ans)
	}
	if ans, _ := a.relay.answerNow(t.Context(),
		relayAsk{Domains: []string{relayInboxSource}}); ans.Unserved {
		t.Errorf("a server whose chat copy alone is out of service refused a follower "+
			"relaying the tracker: %+v", ans)
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
// rooms would otherwise go quiet, and so is one whose only listener hears the
// artifacts — the artifact origin's verdict cache, on a data node serving that
// origin and no API.
//
// Mutation: drop the listening check from follow, and b starts relaying; drop
// chat from liveListening, and c never does; drop the artifact domains from
// it, and e never does; let chatListening take any attached hub, and d starts
// relaying.
func TestANodeNobodyListensOnRelaysNothing(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b", "c", "d", "e"}
	newRelayNode(t, broker, "a", fleet)
	b := newRelayNode(t, broker, "b", fleet)
	b.engine.onInbox.Store(nil)
	b.engine.SetChatLive(t.Context(), nil)
	b.stopArtifacts()
	b.faulted.Store(true)
	b.follow(t)
	d := newRelayNode(t, broker, "d", fleet)
	d.engine.onInbox.Store(nil)
	d.heard.socketless = true
	d.stopArtifacts()
	d.faulted.Store(true)
	d.follow(t)
	c := newRelayNode(t, broker, "c", fleet)
	c.engine.onInbox.Store(nil)
	c.stopArtifacts()
	c.faulted.Store(true)
	c.follow(t)
	c.heard.waitFor(t, "c, whose only listener is chat, relaying",
		func(h heard) bool { return h.chatGaps == 1 })
	e := newRelayNode(t, broker, "e", fleet)
	e.engine.onInbox.Store(nil)
	e.engine.SetChatLive(t.Context(), nil)
	stop := e.engine.OnArtifactsMoved(e.heard.artifactsMoved)
	e.stopArtifacts()
	t.Cleanup(stop)
	e.faulted.Store(true)
	e.follow(t)
	e.heard.waitFor(t, "e, whose only listener hears the artifacts, relaying",
		func(h heard) bool { return h.artifactRs == 1 })
	time.Sleep(3 * servingRecheck)
	if b.relaying() {
		t.Fatal("a node with nothing listening for what its appliers announce is " +
			"relaying a peer's")
	}
	if d.relaying() {
		t.Fatal("a node whose only attachment is a chat hub serving no socket is " +
			"relaying a peer's batches to nobody")
	}
}

// A RELAYED ANSWER THE FLEET DID NOT SIGN NEVER REACHES A SOCKET, AND ONE IT
// SIGNED DOES.
//
// What a follower takes from the relay is pushed to every socket on its node
// as a peer's appliers' work — a chat batch's excerpts and mentions, a
// datastore's documents whole — so an answer anybody on the broker could write
// is words in a room under anybody's name. Here a forger serves a's subject
// beside a and answers every ask at once, before a does, in exactly the epoch
// and the sequence the follower asked after — the forgery the unsigned relay
// believed — unsealed, sealed under a key the fleet does not hold, and sealed
// under the fleet's own key id with other material. Each is passed over while
// the follower goes on waiting, and a's own sealed answer, arriving behind it,
// is the one b's sockets hear.
//
// Mutation: let the follower believe a reply its OpenReply refuses (or decode
// the body without opening it), and the forged post reaches b's chat; end the
// ask on the first reply whatever it is, and b stops hearing a's posts behind
// the forger; break a's seal (another channel name), and a's real post never
// arrives.
func TestARelayedAnswerTheFleetDidNotSignNeverReachesASocket(t *testing.T) {
	t.Parallel()
	for name, seal := range map[string]func(t *testing.T, a relayAnswer) []byte{
		"unsealed": func(t *testing.T, a relayAnswer) []byte {
			raw, err := json.Marshal(a)
			if err != nil {
				t.Error(err)
			}
			return raw
		},
		"sealed under a key the fleet does not hold": forgedSeal(
			statelog.OneKey("k-forger", "material nobody in the fleet holds")),
		"sealed under the fleet's key id with other material": forgedSeal(
			statelog.OneKey("k1", "material that is not the fleet's")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			broker := memory.NewBroker()
			fleet := []string{"a", "b"}
			a := newRelayNode(t, broker, "a", fleet)
			b := newRelayNode(t, broker, "b", fleet)
			b.faulted.Store(true)
			b.follow(t)
			b.heard.waitFor(t, "b following a", func(h heard) bool { return h.chatGaps == 1 })

			// THE FORGER: on a's subject, answering first, in the follower's
			// own epoch, with the batch after its last — and bound to the
			// id it read off the ask, since an ask's body is in the clear.
			forger := broker.Client()
			if err := forger.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			reader := relayWireFor(t, statelog.OneKey("k-forger", "material nobody in the fleet holds"))
			var forged atomic.Int32
			stop, err := forger.Serve(t.Context(), liveRelaySubject("a"), queue.ConcurrentAnswers,
				func(_ context.Context, raw []byte) ([]byte, error) {
					claimed := reader.Admit(raw, signedask.NewGuard(), time.Now()).Request
					batch, err := json.Marshal(relayedBatch{Seq: claimed.After + 1,
						liveBatch: liveBatch{Chat: posted("m-forged")}})
					if err != nil {
						return nil, err
					}
					forged.Add(1)
					return seal(t, relayAnswer{Request: claimed.ID, Epoch: claimed.Epoch,
						Head: claimed.After + 1, Batches: []json.RawMessage{batch}}), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

			// a's first post releases the ask in flight, which the forger
			// never saw; the follower's next ask reaches both, and the
			// forger answers it first.
			a.engine.chatApplied(posted("m-1"))
			b.heard.waitFor(t, "a's first post reaching b", func(h heard) bool {
				return len(h.chat) == 1
			})
			deadline := time.Now().Add(2 * liveRelayWait)
			for forged.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the forger was never asked, so nothing here was forged")
				}
				time.Sleep(10 * time.Millisecond)
			}
			a.engine.chatApplied(posted("m-2"))
			b.heard.waitFor(t, "a's sealed post reaching b behind the forger",
				func(h heard) bool { return slices.Contains(messages(h.chat), "m-2") })
			got := b.heard.snapshot()
			if m := messages(got.chat); m[0] != "m-1" || m[1] != "m-2" {
				t.Errorf("b's chat heard %v, want a's two posts alone — a forged "+
					"answer reached its sockets", m)
			}
			if got.chatGaps != 1 {
				t.Errorf("b's chat was told to read again %d times, want once — a "+
					"forged answer moved the follower", got.chatGaps)
			}
		})
	}
}

// forgedSeal seals an answer the way a party holding ring would — one that is
// not the fleet's.
func forgedSeal(ring statelog.Keyring) func(t *testing.T, a relayAnswer) []byte {
	return func(t *testing.T, a relayAnswer) []byte {
		wire, err := newRelayWire(ring)
		if err != nil {
			t.Error(err)
			return nil
		}
		sealed, err := wire.SealReply(a)
		if err != nil {
			t.Error(err)
		}
		return sealed
	}
}

// A SERVER ANSWERS FROM ITS RING ONLY AN ASK THE FLEET SIGNED, BOUND AND NOT
// TAKEN BEFORE.
//
// A server that sealed an answer to any ask would seal its ring — excerpts,
// documents — for anybody, bound to whatever id they named. Here a server
// holding a batch is asked unsealed, under a key it does not hold, with a
// fleet-signed ask twice, and with one past its deadline: only the first
// fleet-signed ask is answered with the batch, the one under an unheld key is
// unserved and bound to its id (so its follower moves on at once), and the
// rest are unserved and carry nothing. And a relay with no envelopes refuses
// to serve rather than answering unsigned.
//
// Mutation: decode the ask without Admit, and the unsealed ask is answered
// the batch; drop the guard, and the replay is; drop the deadline check in the
// guard, and the late ask is.
func TestARelayServerAnswersOnlyAnAskTheFleetSigned(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	wire := relayWireFor(t, relayRing)
	r := newLiveRelay("a", nil, wire, nil, liveDomains(),
		func(context.Context, ...string) []string { return nil }, nil, nil, nil)
	first, _ := r.answerNow(ctx, relayAsk{})
	r.record(liveBatch{Chat: posted("m-1")})
	now := time.Now()
	ask := relayAsk{Epoch: first.Epoch, After: first.Head, From: "b",
		ID: signedask.NewID(), Deadline: now.Add(time.Minute)}

	unsealed, err := json.Marshal(ask)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.answerSealed(ctx, unsealed, now); !got.Unserved || len(got.Batches) != 0 ||
		got.Request != "" {
		t.Errorf("an ask the fleet did not sign was answered %+v, want unserved, "+
			"bound to nothing and carrying nothing", got)
	}
	unheld, err := relayWireFor(t, statelog.OneKey("k2", "a key this server does not hold")).
		SealRequest(ask)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.answerSealed(ctx, unheld, now); !got.Unserved || len(got.Batches) != 0 ||
		got.Request != ask.ID {
		t.Errorf("an ask under a key the server does not hold was answered %+v, "+
			"want unserved, bound to its id and carrying nothing", got)
	}
	sealed, err := wire.SealRequest(ask)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.answerSealed(ctx, sealed, now); got.Unserved || len(got.Batches) != 1 ||
		got.Request != ask.ID {
		t.Fatalf("an ask the fleet signed was answered %+v, want the batch after "+
			"it, bound to its id", got)
	}
	if got := r.answerSealed(ctx, sealed, now); !got.Unserved || len(got.Batches) != 0 {
		t.Errorf("a replay of an ask already answered was answered %+v, want "+
			"unserved and nothing", got)
	}
	late := ask
	late.ID, late.Deadline = signedask.NewID(), now.Add(-time.Second)
	sealedLate, err := wire.SealRequest(late)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.answerSealed(ctx, sealedLate, now); !got.Unserved || len(got.Batches) != 0 {
		t.Errorf("an ask past its deadline was answered %+v, want unserved and nothing", got)
	}

	if _, err := newLiveRelay("a", nil, nil, nil, nil, nil, nil, nil, nil).serve(ctx); err == nil {
		t.Error("a relay with no envelopes served its subject, answering unsigned")
	}
}
