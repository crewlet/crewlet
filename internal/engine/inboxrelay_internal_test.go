package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// heardInbox is what one node's sockets were handed: every movement, and the
// source of every time they were told to read every inbox again.
type heardInbox struct {
	mu    sync.Mutex
	moved []inbox.Movement
	gaps  []string
	ping  chan struct{}
}

func newHeardInbox() *heardInbox { return &heardInbox{ping: make(chan struct{}, 64)} }

func (h *heardInbox) deliver(moved []inbox.Movement) {
	h.mu.Lock()
	h.moved = append(h.moved, moved...)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardInbox) gap(source string) {
	h.mu.Lock()
	h.gaps = append(h.gaps, source)
	h.mu.Unlock()
	h.ping <- struct{}{}
}

func (h *heardInbox) snapshot() ([]inbox.Movement, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.moved), slices.Clone(h.gaps)
}

// waitFor waits until cond holds of what was heard, or fails naming what.
func (h *heardInbox) waitFor(t *testing.T, what string,
	cond func([]inbox.Movement, []string) bool) {

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
			t.Fatalf("%s never happened: heard %v and gaps of %v", what, moved, gaps)
		}
	}
}

// gapsOf is how many of gaps were about source.
func gapsOf(gaps []string, source string) int {
	n := 0
	for _, g := range gaps {
		if g == source {
			n++
		}
	}
	return n
}

// handles is every movement's handle, in the order heard.
func handles(moved []inbox.Movement) []string {
	out := make([]string, 0, len(moved))
	for _, m := range moved {
		out = append(out, m.Handle)
	}
	return out
}

// moving is one notice for handle from source's applier.
func moving(source, handle string) inbox.Movement {
	return inbox.Movement{Source: source, Handle: handle, UnreadDelta: 1}
}

// relayNode is one data node's relay over a shared in-memory broker, with its
// copy's verdict of each domain a switch the case holds.
type relayNode struct {
	relay  *inboxRelay
	engine *Engine
	heard  *heardInbox

	mu  sync.Mutex
	out map[string]bool
}

func newRelayNode(t *testing.T, broker *memory.Broker, self string, fleet []string,
	sources ...string) *relayNode {

	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start %s's queue: %v", self, err)
	}
	return newRelayNodeOn(t, q, self, fleet, sources...)
}

// newRelayNodeOn is newRelayNode over a queue the case holds.
func newRelayNodeOn(t *testing.T, q relayQueue, self string, fleet []string,
	sources ...string) *relayNode {

	t.Helper()
	if len(sources) == 0 {
		sources = []string{trackerName}
	}
	n := &relayNode{engine: &Engine{}, heard: newHeardInbox(), out: map[string]bool{}}
	n.engine.SetOnInboxMoved(n.heard.deliver)
	n.engine.SetOnInboxGap(n.heard.gap)
	n.relay = newInboxRelay(self, q,
		func(context.Context) ([]string, error) { return fleet, nil },
		sources, n.outOfService,
		n.engine.inboxListening, n.engine.deliverInbox, n.engine.inboxGap)
	n.engine.inboxRelay.Store(n.relay)
	stop, err := n.relay.serve(t.Context())
	if err != nil {
		t.Fatalf("serve %s's relay: %v", self, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return n
}

// setOut holds this node's copy of source out of service, or back in it.
func (n *relayNode) setOut(source string, out bool) {
	n.mu.Lock()
	n.out[source] = out
	n.mu.Unlock()
}

// outOfService is [localEstate.outOfService] over the switches the case holds.
func (n *relayNode) outOfService(_ context.Context, domains ...string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, d := range domains {
		if n.out[d] {
			out = append(out, d)
		}
	}
	return out
}

// relaying reports whether this node's sockets hear source from a peer now.
func (n *relayNode) relaying(source string) bool {
	return n.relay.streams[source].relaying.Load()
}

// follow runs n's followers until the case ends.
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
	b.setOut(trackerName, true)
	b.follow(t)

	// THE SWITCH TO A PEER IS A RE-READ, once the follower knows where the
	// peer stands — of the source that switched.
	b.heard.waitFor(t, "b switching to a's movements",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, trackerName) == 1 })

	// b's own applier — halted, wrong, or simply not the one being heard —
	// is withheld; a's is relayed.
	b.engine.inboxMoved([]inbox.Movement{moving(trackerName, "from-b-own-applier")})
	a.engine.inboxMoved([]inbox.Movement{{Source: trackerName, Handle: "ana",
		UnreadDelta: 1, Subject: "t-1", Reason: "assignee"}})
	b.heard.waitFor(t, "a's movement reaching b's sockets",
		func(moved []inbox.Movement, _ []string) bool { return len(moved) > 0 })
	moved, _ := b.heard.snapshot()
	if got := handles(moved); len(got) != 1 || got[0] != "ana" {
		t.Fatalf("b's sockets heard %v, want a's movement for ana alone — b's own "+
			"applier is not the source while its copy is out of service", got)
	}
	if moved[0].Subject != "t-1" || moved[0].Reason != "assignee" || moved[0].Source != trackerName {
		t.Errorf("the relayed movement lost its fields: %+v", moved[0])
	}
	// a's own sockets heard a's applier, as ever.
	if own, _ := a.heard.snapshot(); len(own) != 1 || own[0].Handle != "ana" {
		t.Errorf("a's own sockets heard %v, want its own applier's movement", handles(own))
	}

	// BACK IN SERVICE: b's sockets hear b's own applier again, after a
	// re-read covering whatever the switch skipped.
	b.setOut(trackerName, false)
	a.engine.inboxMoved([]inbox.Movement{moving(trackerName, "bo")})
	b.heard.waitFor(t, "b switching back to its own applier",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, trackerName) == 2 })
	if b.relaying(trackerName) {
		t.Fatal("b is still relaying a peer's movements with its own copy back in service")
	}
	b.engine.inboxMoved([]inbox.Movement{moving(trackerName, "from-b-own-applier")})
	b.heard.waitFor(t, "b's own applier being heard again",
		func(moved []inbox.Movement, _ []string) bool {
			got := handles(moved)
			return len(got) > 0 && got[len(got)-1] == "from-b-own-applier"
		})
}

// EACH DOMAIN IS RELAYED FROM ITS OWN SOUND COPY, and only the domain whose
// copy is out.
//
// A fault is one log's (ADR-0049). Node a's code copy is out and its tracker
// copy serves; node b's is the other way round. So a's sockets hear its own
// tracker applier and b's code applier, b's hear its own code applier and a's
// tracker applier, and neither hears the other's announcements of a domain its
// own copy serves — nor its own of the one it does not. A relay that judged a
// node's copy as one verdict would have both nodes relay both domains from
// servers that answer neither; one that filed every movement in one stream
// would hand b a's code movements beside its tracker ones. And a server's
// answer for one domain is decided by that domain's copy alone.
//
// Mutation: judge a follower's need, or a server's answer, by every source's
// verdict rather than its own; or file every source in one stream; and a node
// hears a movement it must not, or never hears one it must.
func TestEachDomainIsRelayedFromItsOwnSoundCopy(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	fleet := []string{"a", "b"}
	a := newRelayNode(t, broker, "a", fleet, trackerName, codeName)
	b := newRelayNode(t, broker, "b", fleet, trackerName, codeName)
	a.setOut(codeName, true)
	b.setOut(trackerName, true)
	a.follow(t)
	b.follow(t)

	// EACH SWITCHES THE ONE SOURCE WHOSE COPY IS OUT, and only that one.
	a.heard.waitFor(t, "a switching its code to b's",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, codeName) == 1 })
	b.heard.waitFor(t, "b switching its tracker to a's",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, trackerName) == 1 })
	if a.relaying(trackerName) || b.relaying(codeName) {
		t.Fatal("a node relays a domain its own copy of serves")
	}

	a.engine.inboxMoved([]inbox.Movement{moving(trackerName, "tracker-from-a"),
		moving(codeName, "code-from-a")})
	b.engine.inboxMoved([]inbox.Movement{moving(trackerName, "tracker-from-b"),
		moving(codeName, "code-from-b")})

	want := []string{"code-from-b", "tracker-from-a"}
	for _, n := range []struct {
		name string
		node *relayNode
	}{{"a", a}, {"b", b}} {
		n.node.heard.waitFor(t, n.name+" hearing each domain from its sound copy",
			func(moved []inbox.Movement, _ []string) bool { return len(moved) >= len(want) })
		moved, _ := n.node.heard.snapshot()
		if got := slices.Sorted(slices.Values(handles(moved))); !slices.Equal(got, want) {
			t.Errorf("%s's sockets heard %v, want %v — each domain from the "+
				"node whose copy of it serves, and nothing else", n.name, got, want)
		}
	}

	// AND A SERVER ANSWERS EACH DOMAIN BY ITS OWN COPY.
	if got, _ := a.relay.answerNow(t.Context(), relayAsk{Source: codeName}); !got.Unserved {
		t.Errorf("a, whose code copy is out, answered for the code host %+v", got)
	}
	if got, _ := a.relay.answerNow(t.Context(), relayAsk{Source: trackerName}); got.Unserved ||
		got.Source != trackerName {
		t.Errorf("a, whose tracker copy serves, answered for the tracker %+v", got)
	}
	if got, _ := a.relay.answerNow(t.Context(), relayAsk{Source: "vectors"}); !got.Unserved {
		t.Errorf("a answered for a domain it keeps no stream of: %+v", got)
	}
}

// recordingQueue is a relay queue that says what was served and asked of it.
type recordingQueue struct {
	relayQueue
	mu     sync.Mutex
	served []string
	asked  map[[2]string]bool
}

func (q *recordingQueue) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	q.mu.Lock()
	q.served = append(q.served, subject)
	q.mu.Unlock()
	return q.relayQueue.Serve(ctx, subject, answers, h)
}

func (q *recordingQueue) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {

	var ask relayAsk
	if err := json.Unmarshal(request, &ask); err == nil {
		q.mu.Lock()
		q.asked[[2]string{subject, ask.Source}] = true
		q.mu.Unlock()
	}
	return q.relayQueue.Ask(ctx, subject, request, want)
}

// THE RELAY SERVES ONE SUBJECT, WITH ONE REGISTRATION — one flush at boot —
// WHATEVER THE DOMAINS.
//
// A server holds a stream per domain, and an ask names the one it follows, so
// a node with two domains' copies out asks its server's one subject for each.
// A registration per domain would be a subscription and a flush per domain on
// every data node, at every boot, for an exchange that runs only while some
// copy is wrong.
//
// Mutation: serve each source on a subject of its own, and the server's
// registrations and the follower's subjects multiply.
func TestTheRelayServesOneSubjectWithOneFlush(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	start := func(self string) *recordingQueue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start %s's queue: %v", self, err)
		}
		return &recordingQueue{relayQueue: q, asked: map[[2]string]bool{}}
	}
	fleet := []string{"a", "b"}
	aq, bq := start("a"), start("b")
	a := newRelayNodeOn(t, aq, "a", fleet, trackerName, codeName)
	b := newRelayNodeOn(t, bq, "b", fleet, trackerName, codeName)
	b.setOut(trackerName, true)
	b.setOut(codeName, true)
	b.follow(t)
	b.heard.waitFor(t, "b following both domains from a",
		func(_ []inbox.Movement, gaps []string) bool {
			return gapsOf(gaps, trackerName) == 1 && gapsOf(gaps, codeName) == 1
		})
	a.engine.inboxMoved([]inbox.Movement{moving(codeName, "ana")})
	b.heard.waitFor(t, "a's code movement reaching b",
		func(moved []inbox.Movement, _ []string) bool { return len(moved) == 1 })

	for _, q := range []struct {
		name string
		rec  *recordingQueue
		self string
	}{{"a", aq, "a"}, {"b", bq, "b"}} {
		q.rec.mu.Lock()
		served := slices.Clone(q.rec.served)
		q.rec.mu.Unlock()
		if want := []string{inboxRelaySubject(q.self)}; !slices.Equal(served, want) {
			t.Errorf("%s registered its relay on %v, want once, on %v", q.name, served, want)
		}
	}
	bq.mu.Lock()
	defer bq.mu.Unlock()
	want := map[[2]string]bool{
		{inboxRelaySubject("a"), trackerName}: true,
		{inboxRelaySubject("a"), codeName}:    true,
	}
	if len(bq.asked) != len(want) {
		t.Errorf("b asked %v, want each domain of a's one subject", bq.asked)
	}
	for ask := range want {
		if !bq.asked[ask] {
			t.Errorf("b never asked %s for %s", ask[0], ask[1])
		}
	}
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
	b.setOut(trackerName, true)
	b.follow(t)
	b.heard.waitFor(t, "b following its first server",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, trackerName) == 1 })

	// a — the first in order — goes out of service; whatever it would say
	// now is not to be trusted, and c's applier is.
	a.setOut(trackerName, true)
	a.engine.inboxMoved([]inbox.Movement{moving(trackerName, "from-a")})
	b.heard.waitFor(t, "b moving to c",
		func(_ []inbox.Movement, gaps []string) bool { return gapsOf(gaps, trackerName) == 2 })
	c.engine.inboxMoved([]inbox.Movement{moving(trackerName, "from-c")})
	b.heard.waitFor(t, "c's movement reaching b",
		func(moved []inbox.Movement, _ []string) bool {
			return slices.Contains(handles(moved), "from-c")
		})
}

// heldBy is how many batches, and how many movements between them, r keeps of
// source now.
func heldBy(r *inboxRelay, source string) (batches, moves int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streams[source]
	return len(s.ring), s.moves
}

// switchedVerdict is a copy verdict per domain that a case switches, as
// [localEstate.outOfService] reads one.
type switchedVerdict struct {
	mu  sync.Mutex
	out map[string]bool
}

func (v *switchedVerdict) set(source string, out bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.out == nil {
		v.out = map[string]bool{}
	}
	v.out[source] = out
}

func (v *switchedVerdict) faulted(_ context.Context, domains ...string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var held []string
	for _, d := range domains {
		if v.out[d] {
			held = append(held, d)
		}
	}
	return held
}

// A SERVER SAYS WHAT A FOLLOWER CANNOT PLACE ITSELF IN, rather than answering
// as though nothing were missing: a ring that no longer reaches back to the
// follower's last batch is a GAP — the bound on the movements it keeps
// evicted what came after it, or a batch was wider than that bound and never
// kept at all — and a server back in service after its copy was out is
// another EPOCH, because what its applier announced meanwhile is no sequence
// anybody can trust to be whole, so it keeps none of it. Each is a re-read on
// the follower's side; answered as an ordinary "nothing new", either is
// movements lost without a trace. Both are one source's: another source's
// batches never fill its ring, and its return to service leaves the other's
// epoch alone. On a bubble's clock, so the followers' round trip never runs
// out under the case.
//
// Mutation: drop the bound on movements and the first case answers no gap;
// keep the newest batch however wide, and the wide case holds it; keep the
// epoch, or the ring, across a return to service and the epoch case fails;
// share one ring or one epoch across sources and the other source's cases
// fail.
func TestAServerSaysWhatAFollowerCannotPlaceItselfIn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var verdict switchedVerdict
		r := newInboxRelay("a", nil, nil, []string{trackerName, codeName}, verdict.faulted,
			nil, nil, nil)
		ctx := t.Context()

		first, _ := r.answerNow(ctx, relayAsk{Source: trackerName})
		code, _ := r.answerNow(ctx, relayAsk{Source: codeName})
		r.record([]inbox.Movement{moving(codeName, "ana")})
		for range inboxRelayMoves + 5 {
			r.record([]inbox.Movement{moving(trackerName, "ana")})
		}
		if batches, moves := heldBy(r, trackerName); batches != inboxRelayMoves ||
			moves != inboxRelayMoves {
			t.Errorf("a ring past its bound holds %d batches of %d movements, want %d of %d",
				batches, moves, inboxRelayMoves, inboxRelayMoves)
		}
		got, _ := r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch, After: 1})
		if !got.Gap || len(got.Batches) != 0 {
			t.Errorf("a follower behind the ring was answered %+v, want a gap", got)
		}
		got, _ = r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: got.Head - 2})
		if got.Gap || len(got.Batches) != 2 || got.Batches[0].Seq != got.Head-1 {
			t.Errorf("a follower the ring still reaches was answered %d batches (gap %v), "+
				"want the two after it", len(got.Batches), got.Gap)
		}
		// THE OTHER SOURCE'S RING STILL REACHES BACK to its one batch: the
		// tracker's thousands did not evict it.
		if got, _ := r.answerNow(ctx, relayAsk{Source: codeName, Epoch: code.Epoch}); got.Gap ||
			len(got.Batches) != 1 || got.Batches[0].Moved[0].Source != codeName {
			t.Errorf("the code host's follower was answered %+v, want its one batch", got)
		}

		// A BATCH WIDER THAN THE BOUND is never kept, and takes the ring with
		// it: a follower behind it re-reads, and nothing is held for it.
		wide := make([]inbox.Movement, inboxRelayMoves+1)
		for i := range wide {
			wide[i] = moving(trackerName, fmt.Sprintf("seat-%d", i))
		}
		r.record(wide)
		if batches, moves := heldBy(r, trackerName); batches != 0 || moves != 0 {
			t.Errorf("a batch wider than the bound left %d batches of %d movements held",
				batches, moves)
		}
		if wideGot, _ := r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: got.Head}); !wideGot.Gap || len(wideGot.Batches) != 0 {
			t.Errorf("a follower behind a batch too wide to keep was answered %+v, want a gap",
				wideGot)
		}

		r.record([]inbox.Movement{moving(trackerName, "bo")})
		verdict.set(trackerName, true)
		if got, _ = r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: got.Head}); !got.Unserved {
			t.Fatalf("a server whose copy is out of service answered %+v, want unserved", got)
		}
		verdict.set(trackerName, false)
		got, _ = r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: got.Head})
		if got.Epoch == first.Epoch {
			t.Errorf("a server back in service kept its epoch %q, so a follower would "+
				"read on as though its own applier had announced everything meanwhile",
				got.Epoch)
		}
		if batches, _ := heldBy(r, trackerName); batches != 0 {
			t.Errorf("a server back in service still holds %d batches its old epoch "+
				"numbered, which no follower is ever served again", batches)
		}
		if again, _ := r.answerNow(ctx, relayAsk{Source: codeName}); again.Epoch != code.Epoch {
			t.Errorf("the tracker's return to service moved the code host's epoch %q to "+
				"%q, a re-read for a source whose copy never left service",
				code.Epoch, again.Epoch)
		}
	})
}

// A SERVER KEEPS ONLY WHAT A FOLLOWER CAN STILL ASK FOR.
//
// No follower is served a batch recorded before the last answer it took: one
// new to a server is answered where the stream stands, and one that moves
// starts over. So a batch is worth keeping only while a follower answered
// before it may still ask — for a round trip after that answer, or after the
// batch itself while an ask waits for it — and a ring kept by count holds a
// thousand batches of every source on every data node, each a whole apply
// transaction wide, for followers that do not exist: with no copy out of
// service anywhere, nobody asks. Here, on a bubble's clock: a stream nobody
// asks keeps nothing; a follower just answered is kept what is recorded for a
// round trip and no longer, so one slower than that is answered a gap; the
// ring lets go of what nobody may ask for any more at the next answer or the
// next batch, whichever comes first; and an ask held open past a round trip
// still finds the batch that wakes it.
//
// Mutation: keep a batch whether or not anybody may ask for it, and the
// unasked stream holds one; never hold at an answer, and the follower just
// answered loses its batch to a gap; never expire at an answer, and the slow
// follower is served instead of re-reading; never expire at a batch, and the
// ring keeps what nobody asked for once its followers stop; drop the count of
// waiting asks, and the ask held open is answered a gap rather than its batch.
func TestAServerKeepsOnlyWhatAFollowerCanStillAskFor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var verdict switchedVerdict
		r := newInboxRelay("a", nil, nil, []string{trackerName}, verdict.faulted, nil, nil, nil)
		ctx := t.Context()
		nothingHeld := func(when string) {
			t.Helper()
			if batches, moves := heldBy(r, trackerName); batches != 0 || moves != 0 {
				t.Fatalf("%s, the stream holds %d batches of %d movements", when, batches, moves)
			}
		}

		// NOBODY ASKS: every batch is numbered and none is kept, and the
		// first follower is answered where the stream stands.
		for range 10 {
			r.record([]inbox.Movement{moving(trackerName, "ana")})
		}
		nothingHeld("with nobody asking")
		first, _ := r.answerNow(ctx, relayAsk{Source: trackerName})
		if first.Head != 10 || len(first.Batches) != 0 || first.Gap {
			t.Fatalf("a follower new to the server was answered %+v, want where the "+
				"stream stands, at 10, and nothing before it", first)
		}

		// A FOLLOWER JUST ANSWERED is kept what is recorded before it asks
		// again.
		time.Sleep(inboxRelayHold / 2)
		r.record([]inbox.Movement{moving(trackerName, "bo")})
		got, _ := r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: first.Head})
		if got.Gap || len(got.Batches) != 1 || got.Batches[0].Moved[0].Handle != "bo" {
			t.Fatalf("a follower asking within a round trip of its answer was answered "+
				"%+v, want bo's batch", got)
		}

		// AND FOR NO LONGER: one that asks more than a round trip after its
		// answer is answered a gap, the ring letting go of what it held for
		// it at that answer.
		r.record([]inbox.Movement{moving(trackerName, "cy")})
		time.Sleep(inboxRelayHold)
		late, _ := r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: got.Head})
		if !late.Gap || len(late.Batches) != 0 {
			t.Fatalf("a follower asking %s after its answer was answered %+v, want a gap",
				inboxRelayHold, late)
		}
		nothingHeld("answering a round trip past the last answer")

		// OR AT THE NEXT BATCH, where the followers stopped asking: one
		// recorded a round trip after the last answer finds nobody who may
		// ask for it or for what came before it.
		r.record([]inbox.Movement{moving(trackerName, "dee")})
		time.Sleep(inboxRelayHold)
		r.record([]inbox.Movement{moving(trackerName, "eve")})
		nothingHeld("recording a round trip past the last answer")

		// AN ASK HELD OPEN past a round trip still finds the batch that wakes
		// it: it counts as asking for as long as it waits. The follower is
		// placed where the stream stands first, as a gap places it.
		placed, _ := r.answerNow(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: late.Head})
		done := make(chan relayAnswer, 1)
		go func() {
			done <- r.answer(ctx, relayAsk{Source: trackerName, Epoch: first.Epoch,
				After: placed.Head}, inboxRelayWait)
		}()
		time.Sleep(inboxRelayHold + time.Second)
		synctest.Wait()
		r.record([]inbox.Movement{moving(trackerName, "fay")})
		woken := <-done
		if woken.Gap || len(woken.Batches) != 1 || woken.Batches[0].Moved[0].Handle != "fay" {
			t.Fatalf("an ask held open %s was answered %+v, want the batch that woke it",
				inboxRelayHold+time.Second, woken)
		}
	})
}

// A BATCH NOBODY CAN ASK FOR IS NEVER COPIED.
//
// Every data node records every committed batch of every source, and in the
// normal state — no copy out of service anywhere — nobody follows any of
// them, so what a batch costs there is what the relay costs the whole fleet:
// it is numbered and nothing is allocated. Nor is a batch too wide to keep
// copied, however recently a follower asked, only to be let go of at once.
//
// NOT PARALLEL, because an allocation count is the whole process's, and over
// a hundred runs, which testing.AllocsPerRun divides and rounds down.
//
// Mutation: copy a batch whether or not anybody may ask for it, or copy one
// wider than the bound before letting it go, and a run allocates.
func TestABatchNobodyCanAskForIsNeverCopied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var verdict switchedVerdict
		r := newInboxRelay("a", nil, nil, []string{trackerName}, verdict.faulted, nil, nil, nil)
		batch := benchMovements(100, 0)
		if allocs := testing.AllocsPerRun(100, func() { r.record(batch) }); allocs != 0 {
			t.Errorf("recording a batch nobody follows allocated %v times, want none", allocs)
		}
		r.answerNow(t.Context(), relayAsk{Source: trackerName})
		wide := benchMovements(inboxRelayMoves+1, 0)
		if allocs := testing.AllocsPerRun(100, func() { r.record(wide) }); allocs != 0 {
			t.Errorf("recording a batch too wide to keep allocated %v times, want none", allocs)
		}
		if batches, moves := heldBy(r, trackerName); batches != 0 || moves != 0 {
			t.Errorf("after a batch too wide to keep, the stream holds %d batches of %d "+
				"movements", batches, moves)
		}
	})
}

// THE WIDEST ANSWER A SERVER CAN GIVE FITS ONE REPLY.
//
// An answer carries every batch the follower has not taken, and a reply the
// broker refuses as too large is no answer at all: the follower hears nothing,
// moves to the next server and asks it the same, so a server holding more than
// one reply carries could never be followed. The bound on the movements a
// stream keeps is what holds an answer to one reply — here every movement at
// the longest handle a seat may have, with a subject and a reason as long,
// and as many notices as one transaction can write.
//
// Mutation: raise inboxRelayMoves tenfold, and the answer outgrows the reply.
func TestTheWidestRelayAnswerFitsOneReply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var verdict switchedVerdict
		r := newInboxRelay("a", nil, nil, []string{trackerName}, verdict.faulted, nil, nil, nil)
		first, _ := r.answerNow(t.Context(), relayAsk{Source: trackerName})
		long := strings.Repeat("x", iam.MaxLogin)
		for i := range inboxRelayMoves {
			r.record([]inbox.Movement{{Source: trackerName,
				Handle:      fmt.Sprintf("%0*d", iam.MaxLogin, i),
				UnreadDelta: statelog.ApplyTxRowBudget, Subject: long, Reason: long}})
		}
		got, _ := r.answerNow(t.Context(), relayAsk{Source: trackerName, Epoch: first.Epoch,
			After: first.Head})
		if len(got.Batches) != inboxRelayMoves {
			t.Fatalf("the full ring answered %d batches, want all %d", len(got.Batches),
				inboxRelayMoves)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > queue.MaxPayloadBytes {
			t.Fatalf("the widest answer is %d bytes, past the %d one reply carries",
				len(raw), queue.MaxPayloadBytes)
		}
		t.Logf("the widest answer is %d bytes of the %d one reply carries",
			len(raw), queue.MaxPayloadBytes)
	})
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
	b.setOut(trackerName, true)
	b.follow(t)
	time.Sleep(3 * servingRecheck)
	if b.relaying(trackerName) {
		t.Fatal("a node with nothing listening for its inbox movements is " +
			"relaying a peer's")
	}
}

// benchMovements is one batch of width movements as an applier's decode hands
// them over: a fresh handle and subject string per movement, the source and
// reason its constants.
func benchMovements(width, batch int) []inbox.Movement {
	out := make([]inbox.Movement, width)
	for i := range out {
		out[i] = inbox.Movement{Source: trackerName,
			Handle:      fmt.Sprintf("seat-%07d", batch*width+i),
			UnreadDelta: 1, Subject: uuid.NewString(), Reason: "assignee"}
	}
	return out
}

// BenchmarkRecordingAnInboxBatch is what the relay costs a data node per
// committed batch of one source, and what it holds of that source: UNFOLLOWED,
// with nobody asking — every data node of a fleet with no copy out of service
// — and FOLLOWED for the whole run, at a transaction that moved 1, 100 and
// 1 000 inboxes. ring-B is the heap a full ring holds, measured once before
// the timed loop over batches as an applier hands them over; ns/op, B/op and
// allocs/op are one batch recorded into that full ring, which keeps it and
// lets go of the oldest.
//
//	go test -run '^$' -bench BenchmarkRecordingAnInboxBatch -benchtime 200x -count 1 ./internal/engine/
func BenchmarkRecordingAnInboxBatch(b *testing.B) {
	for _, c := range []struct {
		name     string
		width    int
		followed bool
	}{
		{"unfollowed/width=1", 1, false},
		{"followed/width=1", 1, true},
		{"followed/width=100", 100, true},
		{"followed/width=1000", 1000, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			r := newInboxRelay("a", nil, nil, []string{trackerName}, nil, nil, nil, nil)
			if c.followed {
				// A FOLLOWER FOR THE WHOLE RUN, as though it had just been
				// answered: what a ring past its round trip would have
				// let go of is not what is measured here.
				r.streams[trackerName].heldUntil = time.Now().Add(time.Hour)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i := range inboxRelayMoves/c.width + 1 {
				r.record(benchMovements(c.width, i))
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			held := float64(after.HeapAlloc) - float64(before.HeapAlloc)
			batch := benchMovements(c.width, -1)
			b.ReportAllocs()
			for b.Loop() {
				r.record(batch)
			}
			// REPORTED AFTER THE LOOP, whose first turn resets the timer and
			// with it every metric a benchmark reported.
			b.ReportMetric(held, "ring-B")
		})
	}
}
