package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE INBOX RELAY: how a node whose own tracker copy is OUT OF SERVICE still
// tells its open dashboards whose inbox moved.
//
// # Why one is needed at all
//
// A node learns whose inbox moved from its OWN tracker applier, after every
// committed batch ([Engine.SetOnInboxMoved]), and that is complete only while
// that applier applies every record. A copy that is WRONG — an applier halted
// at a record it cannot decode, a stalled prefix, an estate file shut, rows
// below the log — goes out of service and the node STAYS in rotation
// ([localEstate.For]): its seats' calls and its dashboards' reads go to a
// sound data node through the estate's router, and /ready follows whether it
// can route rather than whether its copy is sound. So its dashboards keep
// reading correct data — and an applier that commits nothing announces
// nothing, so every socket on that node stopped receiving `inbox_changed`
// until it reconnected somewhere else, which a node still in rotation gives it
// no reason to do.
//
// # One source at a time, and a switch is a re-read
//
// While this node's copy serves, its sockets hear its own applier and nothing
// else. While it does not, they hear a SOUND PEER's applier, relayed, and the
// node's own — whatever an out-of-service applier still commits — is
// withheld: two sources would push one movement twice, and the one being
// withheld is the one nobody vouches for. Every SWITCH, either way, tells every
// watched inbox to read itself again ([Engine.SetOnInboxGap]), because what
// moved between the last movement one source delivered and the first the other
// does is not replayed: the rows are the truth, and a screen re-reads them.
//
// # A follower reads ONE server, by that server's own sequence
//
// Every data node keeps the last [inboxRelayRing] batches its applier
// announced, numbered by a sequence of its own under an EPOCH it draws at boot
// and draws again whenever its copy comes back into service — and serves them
// on its own subject. A follower asks one server for everything after the last
// batch it took, and the server answers as soon as it holds one (or at the end
// of [inboxRelayWait]). Positions on the tracker log would have let a follower
// switch servers without a re-read, and are not what a batch has: a batch is
// whatever one node's applier committed together, retained records reprocessed
// out of order included, so two nodes' batches never line up. A sequence of the
// server's own is exact for the one thing it is used for — "after the last one
// I took" — and anything it cannot answer is DETECTED rather than lost: another
// epoch, a sequence the ring no longer reaches, or another server, each of
// which is a re-read.
//
// # Not signed, deliberately
//
// The fleet's ephemeral channels that are INSTRUCTIONS are signed
// (internal/signedask): a node carries the request out as somebody. This is a
// question a node answers about itself, and what the follower does with the
// answer is tell a screen to read its inbox again — through the `work_inbox`
// question and its own authority, which decide what it sees. A forged answer
// can cause a re-read and nothing else.

// inboxRelaySubject is the one subject node serves its relay on —
// [topics.InboxRelayPrefix] and the node, one node per subject, because a
// follower reads ONE server's sequence. THE NODE ID IS ESCAPED as a subject
// token, for the estate subject's reason: an id may carry a dot, which would
// split it in two.
func inboxRelaySubject(node string) string {
	return topics.InboxRelayPrefix + coord.DocumentKey(node)
}

// inboxRelayRing is how many announced batches a server keeps for its
// followers.
//
// A THOUSAND AND TWENTY-FOUR, and what it has to hold is what a server's
// applier commits between a follower's two asks: a follower asks again the
// moment an answer lands, so that is one round trip and the follower's own
// hand-off to its sockets — well under a second, in which an applier that
// commits a batch a millisecond fills fewer than this. And a ring that did
// overflow is a DETECTED gap, answered with a re-read, never a lost movement.
// Each batch is identifiers and a count per seat (tracker.InboxMovement), so
// the ring is tens of kilobytes on a busy node.
const inboxRelayRing = 1024

// inboxRelayWait is how long a server holds a follower's ask open when it has
// nothing newer to answer with.
//
// FIVE SECONDS, and it bounds both edges of the relay: a follower notices its
// own copy back in service, or its server's copy out of it, within one wait.
// The relay exists to beat the 30-second poll the Inbox screen falls back on,
// so a wait anywhere near that would buy nothing at either edge — and an idle
// follower costs one ask per wait, and a follower exists only while its node's
// copy is out of service.
const inboxRelayWait = 5 * time.Second

// inboxRelayRetry is how long a follower pauses when no data node could serve
// it — every peer silent, out of service itself, or none live — before it asks
// again: [servingRecheck], the cadence the copy's own verdict is read at, so a
// follower is never more eager than the judgement it follows.
const inboxRelayRetry = servingRecheck

// relayedBatch is one batch a server's applier announced, numbered by the
// server's own sequence.
type relayedBatch struct {
	Seq   uint64                  `json:"seq"`
	Moved []tracker.InboxMovement `json:"moved"`
}

// relayAsk is what a follower sends: the epoch and the sequence of the last
// batch it took from this server — none, for a follower that has taken
// nothing from it yet and asks only where it stands.
type relayAsk struct {
	Epoch string `json:"epoch,omitempty"`
	After uint64 `json:"after"`
}

// relayAnswer is what a server answers: where it stands, and every batch after
// the follower's that it holds — or that it cannot answer from its own copy at
// all (Unserved), or no longer reaches back as far as the follower (Gap).
type relayAnswer struct {
	Epoch    string         `json:"epoch"`
	Head     uint64         `json:"head"`
	Batches  []relayedBatch `json:"batches,omitempty"`
	Gap      bool           `json:"gap,omitempty"`
	Unserved bool           `json:"unserved,omitempty"`
}

// relayQueue is the two ephemeral verbs the relay rides.
type relayQueue interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// inboxRelay is one data node's half of the relay: the server's ring, and the
// follower's state.
type inboxRelay struct {
	self  string
	queue relayQueue

	// roster is every live data node, from memory ([Engine.dataRoster]).
	roster func(context.Context) ([]string, error)

	// faulted reports whether this node's own copy is out of service, as
	// far as it can tell without waiting ([localEstate.faultedUnasked]).
	faulted func(context.Context) bool

	// listening reports whether anything on this node hears movements at
	// all — a data node that serves no API has nobody to relay to, and a
	// follower that relayed to nobody would be a loop reported as work.
	// deliver hands one batch to this node's sockets, and gap tells every
	// watched inbox to read itself again — the engine's registered
	// listeners, read per call.
	listening func() bool
	deliver   func([]tracker.InboxMovement)
	gap       func()

	// relaying is whether this node's sockets hear a peer now rather than
	// its own applier.
	relaying atomic.Bool

	mu sync.Mutex
	// epoch names the sequence below; drawn at boot and again whenever this
	// node's copy comes back into service.
	epoch string
	seq   uint64
	ring  []relayedBatch
	// moved is closed and replaced by every append, so an ask waiting for a
	// batch wakes the instant one lands.
	moved chan struct{}
	// outOfService is whether this node last answered a follower that its
	// copy is out of service — what tells the next answer it serves to draw
	// a new epoch.
	outOfService bool
}

// newInboxRelay builds a relay with a fresh epoch.
func newInboxRelay(self string, q relayQueue, roster func(context.Context) ([]string, error),
	faulted func(context.Context) bool, listening func() bool,
	deliver func([]tracker.InboxMovement), gap func()) *inboxRelay {

	return &inboxRelay{self: self, queue: q, roster: roster, faulted: faulted,
		listening: listening, deliver: deliver, gap: gap, epoch: rand.Text(),
		moved: make(chan struct{})}
}

// record keeps one batch this node's applier announced, for its followers.
func (r *inboxRelay) record(moved []tracker.InboxMovement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.ring = append(r.ring, relayedBatch{Seq: r.seq, Moved: slices.Clone(moved)})
	if over := len(r.ring) - inboxRelayRing; over > 0 {
		r.ring = slices.Delete(r.ring, 0, over)
	}
	close(r.moved)
	r.moved = make(chan struct{})
}

// answer is what this node answers one follower's ask, holding it open up to
// wait for a batch it does not hold yet.
func (r *inboxRelay) answer(ctx context.Context, ask relayAsk, wait time.Duration) relayAnswer {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		out, waitOn := r.answerNow(ctx, ask)
		if waitOn == nil {
			return out
		}
		select {
		case <-waitOn:
		case <-deadline.C:
			out, _ = r.answerNow(ctx, ask)
			return out
		case <-ctx.Done():
			return out
		}
	}
}

// answerNow is the answer as this node stands, and the channel to wait on
// where it holds nothing after the follower's last batch.
func (r *inboxRelay) answerNow(ctx context.Context, ask relayAsk) (relayAnswer, <-chan struct{}) {
	// ASKED BEFORE THE LOCK: the verdict read may start a read of the
	// broker, which must never sit under the ring's lock.
	faulted := r.faulted(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if faulted {
		r.outOfService = true
		return relayAnswer{Unserved: true}, nil
	}
	if r.outOfService {
		// BACK IN SERVICE: what this node's applier announced while its
		// copy was out of service, or did not announce at all, is no
		// sequence a follower can trust to be whole — so it is another
		// epoch, which every follower meets as a re-read.
		r.outOfService = false
		r.epoch = rand.Text()
	}
	out := relayAnswer{Epoch: r.epoch, Head: r.seq}
	switch {
	case ask.Epoch != r.epoch:
		// A FOLLOWER NEW TO THIS SERVER, or one whose epoch this node no
		// longer serves: where it stands, and nothing it could not place.
		return out, nil
	case ask.After > r.seq || (len(r.ring) > 0 && r.ring[0].Seq > ask.After+1):
		// A FOLLOWER THIS RING NO LONGER REACHES BACK TO — the batches
		// after its last were evicted — or one claiming a batch this
		// epoch never numbered: it re-reads and starts again from here.
		out.Gap = true
		return out, nil
	}
	for i, b := range r.ring {
		if b.Seq > ask.After {
			out.Batches = slices.Clone(r.ring[i:])
			return out, nil
		}
	}
	return out, r.moved
}

// serve makes this node the answerer of its own relay subject.
//
// ON A REGISTRATION OF ITS OWN, because its answers WAIT — up to
// [inboxRelayWait] for the applier's next announcement — and a slot held
// through a wait is one every other request of a shared registration would
// wait behind. Its bound is the queue those waits come from: each follower, a
// node whose own copy is out of service, keeps one ask in flight, so
// [queue.ConcurrentAnswers] is that many such nodes at once, and one past it
// waits for a slot inside its own wait rather than being refused.
func (r *inboxRelay) serve(ctx context.Context) (queue.Unsubscribe, error) {
	return r.queue.Serve(ctx, inboxRelaySubject(r.self), queue.ConcurrentAnswers,
		func(ctx context.Context, raw []byte) ([]byte, error) {
			var ask relayAsk
			if err := json.Unmarshal(raw, &ask); err != nil {
				return nil, fmt.Errorf("engine: an inbox relay ask this build "+
					"cannot read: %w", err)
			}
			return json.Marshal(r.answer(ctx, ask, inboxRelayWait))
		})
}

// follow runs until ctx ends: idle while this node's own copy serves, and
// following a sound peer's applier while it does not — see the file head.
func (r *inboxRelay) follow(ctx context.Context) {
	var (
		source string
		epoch  string
		after  uint64
		// tried is the servers asked since the last one that served, so a
		// follower that lost its server moves through the others before
		// it pauses.
		tried = map[string]bool{}
	)
	switchSource := func() {
		source, epoch, after = "", "", 0
	}
	for ctx.Err() == nil {
		if !r.listening() || !r.faulted(ctx) {
			if r.relaying.Swap(false) {
				// BACK TO ITS OWN APPLIER, which committed nothing for
				// the batches the relay carried or announces them a
				// second time: either way, read again.
				log.InfoContext(ctx, "inbox_relay_stopped", "server", source,
					"detail", "this node's copy is back in service, or nothing "+
						"on it listens for inbox movements any more, so its "+
						"dashboards hear its own applier again")
				r.gap()
			}
			switchSource()
			clear(tried)
			if !sleepCtx(ctx, servingRecheck) {
				return
			}
			continue
		}
		if !r.relaying.Swap(true) {
			log.WarnContext(ctx, "inbox_relay_started",
				"detail", "this node's copy is out of service, so its applier "+
					"announces nothing its dashboards can trust; their inbox "+
					"movements are relayed from a data node whose copy serves")
		}
		if source == "" {
			source = r.pick(ctx, tried)
			if source == "" {
				clear(tried)
				if !sleepCtx(ctx, inboxRelayRetry) {
					return
				}
				continue
			}
			tried[source] = true
		}
		got, ok := r.ask(ctx, source, relayAsk{Epoch: epoch, After: after})
		switch {
		case !ok || got.Unserved:
			switchSource()
			continue
		case got.Epoch != epoch || got.Gap:
			// A SERVER THIS FOLLOWER IS NEW TO, or one it can no longer
			// place itself in: everything from here is relayed, and
			// everything before it is read again.
			epoch, after = got.Epoch, got.Head
			clear(tried)
			r.gap()
			continue
		}
		clear(tried)
		for _, b := range got.Batches {
			if b.Seq <= after {
				continue
			}
			after = b.Seq
			if len(b.Moved) > 0 {
				r.deliver(b.Moved)
			}
		}
	}
}

// pick is the next live data node to follow — never this one, and never one
// already tried since the last that served — or "" for none.
func (r *inboxRelay) pick(ctx context.Context, tried map[string]bool) string {
	nodes, err := r.roster(ctx)
	if err != nil {
		log.DebugContext(ctx, "inbox_relay_roster_unknown", "error", err.Error())
		return ""
	}
	for _, n := range slices.Sorted(slices.Values(nodes)) {
		if n != r.self && n != "" && !tried[n] {
			return n
		}
	}
	return ""
}

// ask asks one server, reporting false for a server that did not answer.
func (r *inboxRelay) ask(ctx context.Context, node string, a relayAsk) (relayAnswer, bool) {
	raw, err := json.Marshal(a)
	if err != nil {
		return relayAnswer{}, false
	}
	// THE WAIT AND A ROUND TRIP: the server answers at the end of its wait
	// at the latest, and statelog.ReadBudget is the budget the estate's
	// own reads give a peer to answer in.
	askCtx, cancel := context.WithTimeout(ctx, inboxRelayWait+statelog.ReadBudget)
	defer cancel()
	replies, err := r.queue.Ask(askCtx, inboxRelaySubject(node), raw, 1)
	if err != nil || len(replies) == 0 {
		if ctx.Err() == nil {
			log.DebugContext(ctx, "inbox_relay_unanswered", "server", node,
				"error", fmt.Sprint(err))
		}
		return relayAnswer{}, false
	}
	var got relayAnswer
	if err := json.Unmarshal(replies[0], &got); err != nil {
		log.WarnContext(ctx, "inbox_relay_unreadable", "server", node,
			"error", err.Error())
		return relayAnswer{}, false
	}
	return got, true
}

// sleepCtx waits d, reporting false where ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// faultedUnasked reports whether this node's TRACKER copy is OUT OF SERVICE as
// far as it can tell without waiting: a verdict read and found that domain's
// copy wrong (ADR-0046). The tracker's alone, because the inbox movements come
// from the tracker's applier: a wrong copy of any other domain leaves this
// node's own announcements complete. A copy nobody has judged yet and a node
// whose core is not up are not — [localEstate.For] serves both — and a stale
// verdict answers while the read that replaces it runs, exactly as a request
// is answered.
func (l *localEstate) faultedUnasked(ctx context.Context) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return false
	}
	v, known, _ := l.peek(ctx, c)
	return known && !v.serves(tracker.Domain{}.Name())
}

// armInboxRelay makes this data node a server of its inbox relay and starts
// the follower that takes over its sockets' movements whenever its own copy is
// out of service. A node without `data` serves no API (ingress requires it)
// and holds no applier, so it has nothing to relay and nobody to relay to.
func (e *Engine) armInboxRelay(ctx context.Context) error {
	if e.local == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	relay := newInboxRelay(e.id, e.backends.Queue, e.dataRoster,
		e.local.faultedUnasked, e.inboxListening, e.deliverInbox, e.inboxGap)
	stop, err := relay.serve(ctx)
	if err != nil {
		return fmt.Errorf("engine: serve the inbox relay: %w", err)
	}
	// THE FOLLOWER'S LIFETIME IS THE ENGINE'S, ended by [Engine.stopInboxRelay]:
	// detached from the context it was armed under, which is the boot's.
	followCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.follow(followCtx)
	}()
	e.inboxRelay.Store(relay)
	e.stopInboxRelayFn = func(ctx context.Context) {
		cancel()
		<-done
		if err := stop(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "inbox_relay_not_withdrawn", "error", err)
		}
	}
	return nil
}

// stopInboxRelay ends the follower and withdraws this node as a server,
// before the broker closes.
func (e *Engine) stopInboxRelay(ctx context.Context) {
	if e.stopInboxRelayFn == nil {
		return
	}
	e.stopInboxRelayFn(ctx)
	e.stopInboxRelayFn = nil
}
