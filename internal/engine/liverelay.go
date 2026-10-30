package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE LIVE RELAY: how a node whose own copy is OUT OF SERVICE still tells its
// open dashboards what its appliers would have — whose inbox moved, what was
// said in a room, and what moved on an artifact and in its datastore.
//
// # Why one is needed at all
//
// A node's sockets learn what changed from its OWN appliers, after every
// committed batch: the tracker's says whose inbox moved
// ([Engine.SetOnInboxMoved]), chat's what landed in a room
// ([Engine.SetChatLive]), the artifacts' and the datastore's what moved on an
// artifact and which documents changed ([Engine.OnArtifactsMoved],
// [Engine.OnArtifactDataMoved]). That is complete only while those appliers
// apply every record. A copy that is WRONG — an applier halted at a record it cannot
// decode, a stalled prefix, an estate file shut, rows below the log — goes out
// of service and the node STAYS in rotation ([localEstate.For]): its seats'
// calls and its dashboards' reads go to a sound data node through the estate's
// router, and /ready follows whether it can route rather than whether its copy
// is sound. So its dashboards keep reading correct data — and an applier that
// commits nothing announces nothing, so every socket on that node stopped
// receiving `inbox_changed`, and every room open on it, a chat room's or an
// artifact's, went quiet, until it reconnected somewhere else, which a node
// still in rotation gives it no reason to do. The artifact origin's verdict
// cache on that node hears the same appliers, and a share withdrawn on a
// sound peer was forgotten by nothing there until the verdict aged out.
//
// # ONE relay for every domain that announces, not one per domain
//
// The verdict that takes a copy out of service is the WHOLE COPY's — one
// [localEstate.For] for every log the node holds — so whether this node's
// sockets hear its own appliers or a peer's is one fact for every domain at
// once, and a relay per domain would be several followers reading that one
// fact on their own clocks: one switched while its sibling had not, a screen's
// inbox already a peer's and its room still this node's halted applier's. So
// there is one ring, one sequence, one follower and one switch, and a batch in
// the ring is EXACTLY ONE domain's — one applier's commit ([liveBatch]) —
// handed on to that domain's listeners by [Engine.deliverLive]. The artifact
// copies once had a follower of their own, which withheld this node's artifact
// pushes while its copy was out of service and told every room to read again
// at each switch: a second reader of the one verdict, on its own clock, that
// relayed nothing, so an artifact room on an out-of-service node heard nobody's
// appliers at all.
//
// # One source at a time, and a switch is a re-read
//
// While this node's copy serves, its sockets hear its own appliers and nothing
// else. While it does not, they hear a SOUND PEER's appliers, relayed, and the
// node's own — whatever an out-of-service applier still commits — are
// withheld: two sources would push one movement or one message twice, and the
// one being withheld is the one nobody vouches for. Every SWITCH, either way,
// tells every screen to read again ([Engine.liveGap]: every watched inbox,
// every chat screen and every artifact room, which is a reset of every artifact
// and every datastore to their listeners), because what was committed between
// the last batch one source delivered and the first the other does is not
// replayed: the rows are the truth, and a screen re-reads them.
//
// # A follower reads ONE server, by that server's own sequence
//
// Every data node keeps the batches its appliers announced, up to
// [liveRelayRingBytes] of them, numbered by a sequence of its own under an
// EPOCH it draws at boot and draws again whenever its copy comes back into
// service — and serves them on its own subject. A follower asks one server for
// everything after the last batch it took, and the server answers as soon as
// it holds one (or at the end of [liveRelayWait]). Positions on the logs would
// have let a follower switch servers without a re-read, and are not what a
// batch has: a batch is whatever one node's applier committed together,
// retained records reprocessed out of order included, so two nodes' batches
// never line up — and an inbox movement carries no position at all. A
// sequence of the server's own is exact for the one thing it is used for —
// "after the last one I took" — and anything it cannot answer is DETECTED
// rather than lost: another epoch, a sequence the ring no longer reaches, or
// another server, each of which is a re-read.
//
// # Signed, because what it answers is shown as somebody's work
//
// The fleet's ephemeral channels that are INSTRUCTIONS are signed
// (internal/signedask), and the scatters that only READ are not (ADR-0031):
// a forged history page or search slice is a wrong answer to one question,
// read once and gone with the next. This scatter's answer is not that. What a
// follower takes from it is PUSHED, unasked, to every socket on its node as
// what that server's appliers committed — a chat batch carries each message's
// excerpt and who it mentions ([chatlet.Applied]), and a datastore batch every
// document it changed, whole ([artifactdata.DocDelta]) — and a screen shows it
// as what a colleague just said or wrote. Unsigned, anybody who reached the
// cluster or the leaf port could answer a follower before its server did and
// put words in a room under anybody's name, or a value in a shared table, on
// every screen of an out-of-service node until each read the rows again.
//
// So the relay is a CHANNEL of internal/signedask's, in key domains of its own
// ([liveRelayChannel], `live.relay.request` and `live.relay.reply`), so no
// state-log record, presence frame or other channel's envelope verifies as one
// of its asks or answers. A channel and not a one-way frame, because the relay
// is an ask and ONE answer to it, and what makes an answer worth believing is
// that it answers THIS ask: sealed one way, with an expiry and no binding, a
// captured answer would be believed by any follower asking inside its validity
// — one server's "unserved" moving every follower off it, or an old answer's
// batches handed to a follower that had already moved on. Bound to an id drawn
// for each ask ([signedask.NewID]), an answer is believed by the one follower
// that asked, once. The server admits the ask too ([signedask.Channel.Admit]):
// it is no instruction, but a server that sealed an answer to any ask would
// seal its ring for anybody, bound to whatever id they named, and one under a
// key it does not hold is answered "unserved" at once, so its follower moves to
// a server that holds the key rather than waiting out its ask.
//
// AN ANSWER THE FOLLOWER CANNOT BELIEVE IS NO ANSWER — never "unserved", never
// a gap, never batches — and the follower goes on waiting for its server's own
// ([liveRelay.ask]): the transport hands back whichever reply arrives first and
// carries no sender, so a forged reply that arrived first and ended the ask
// would let anybody on the broker move every follower off every server. Its
// server's real answer, arriving behind it, is the one taken; a server that
// never sends one is a silent peer, which the follower already handles by
// moving on.
//
// What a follower does with a believed answer is what its own appliers' batch
// would have had done with it: hand it to its sockets' own filters, which
// decide what each is shown — an inbox movement through the `work_inbox`
// question and its own authority, a chat batch through the room's own
// visibility for each viewer ([chatlet.Visible]), and an artifact's batch through
// the room's own join and each reader's role and the collection's read rule
// ([artifactdata.Readable]). Signing authenticates and does not encrypt: what
// the relay carries is readable by anything on the broker, as every record on
// the logs it derives from is, and keeping both ports private is the
// operator's half (SECURITY.md).

// liveRelaySubject is the one subject a node serves its relay on —
// [topics.LiveRelayPrefix] and the node, one node per subject, because a
// follower reads ONE server's sequence. THE NODE ID IS ESCAPED as a subject
// token, for the estate subject's reason: an id may carry a dot, which would
// split it in two.
func liveRelaySubject(node string) string {
	return topics.LiveRelayPrefix + coord.DocumentKey(node)
}

// liveRelayChannel is the relay's channel, and so the stem of the two key
// domains its asks and answers are signed under — its own, so no estate
// envelope, broker request, steer note, presence frame or state-log record
// verifies as one.
const liveRelayChannel = "live.relay"

// relayWire is the relay's envelopes: one per node, both halves, since a data
// node both asks (as a follower) and answers (as a server).
type relayWire = signedask.Channel[relayAsk, relayAnswer]

// newRelayWire builds this node's relay envelopes over the fleet keyring
// every state-log record is signed under. The keyring is required on every
// node, so this fails only on a bootstrap [config.Bootstrap.Validate] would
// already have refused.
func newRelayWire(ring statelog.Keyring) (*relayWire, error) {
	wire, err := signedask.New[relayAsk, relayAnswer](liveRelayChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the live relay's envelopes: %w", err)
	}
	return wire, nil
}

// errNoRelayWire is a relay built without its envelopes: it can neither ask a
// server nor answer a follower.
var errNoRelayWire = errors.New("engine: no envelopes for the live relay — every " +
	"ask and answer on its subjects is signed under the fleet keyring (Tier A " +
	"secrets.keys)")

// liveRelayRingBytes is how much of what its appliers announced a server keeps
// for its followers, ENCODED.
//
// BOUNDED BY BYTES AND NOT BY BATCHES, because the domains' batches differ by
// orders of magnitude: an inbox movement is identifiers and a count
// (worklet.InboxMovement), and so is an artifact's movement, while a chat batch
// carries each record's routing snapshot — an excerpt of up to
// [chatlet.MaxExcerpt] bytes and up to [chatlet.MaxRecipients] recipients — and one
// applier commit can hold a thousand messages ([statelog.ApplyTxRowBudget]
// rows at about four a message), and a datastore batch carries every document
// it changed, whole. The thousand-batch ring the inbox alone had held tens of
// kilobytes of movements and would hold gigabytes of a catching-up room.
//
// HALF OF [queue.MaxPayloadBytes], the rule a chat transcript page is bounded
// by and for the same reason: an answer is a SUFFIX of the ring, so a ring
// that fits in half a payload answers every follower in one reply with room
// for the envelope. What it has to hold is what a server's appliers commit
// between a follower's two asks: a follower asks again the moment an answer
// lands, so that is one round trip and the follower's own hand-off to its
// sockets — well under a second, in which the declared census of either
// domain commits a few kilobytes. A ring that did overflow — a catch-up
// committing a thousand-message batch every few milliseconds — is a DETECTED
// gap, answered with a re-read, never a lost movement or message; and a
// single batch larger than the whole ring is relayed as exactly that gap
// ([liveRelay.record]).
const liveRelayRingBytes = queue.MaxPayloadBytes / 2

// liveRelayWait is how long a server holds a follower's ask open when it has
// nothing newer to answer with.
//
// FIVE SECONDS, and it bounds both edges of the relay: a follower notices its
// own copy back in service, or its server's copy out of it, within one wait.
// The relay exists to beat the 30-second poll the Inbox screen falls back on,
// so a wait anywhere near that would buy nothing at either edge — and an idle
// follower costs one ask per wait, and a follower exists only while its node's
// copy is out of service.
const liveRelayWait = 5 * time.Second

// liveRelayRetry is how long a follower pauses when no data node could serve
// it — every peer silent, out of service itself, or none live — before it asks
// again: [servingRecheck], the cadence the copy's own verdict is read at, so a
// follower is never more eager than the judgement it follows.
const liveRelayRetry = servingRecheck

// liveBatch is one committed batch's announcement to this node's sockets, as
// one of its appliers made it.
//
// EXACTLY ONE FIELD IS SET, because a batch is one applier's commit and each
// applier is one domain's. The fields are the domains whose appliers announce
// at all — the tracker's inbox movements, chat's committed effects, and what
// the artifacts' and the datastore's commits moved — and a domain that starts
// announcing is one more field here and one more arm in [Engine.deliverLive],
// [Engine.liveGap] and [Engine.liveListening].
//
// ADDITIVE ON THE WIRE: a newer server's batch of a domain this build does not
// know decodes as a batch with none of these fields set, and is delivered to
// nobody — which is all this build's sockets could do with it anyway.
type liveBatch struct {
	Inbox        []worklet.InboxMovement `json:"inbox,omitempty"`
	Chat         []chatlet.Applied       `json:"chat,omitempty"`
	Artifacts    []artifactsdomain.Moved `json:"artifacts,omitempty"`
	ArtifactData []artifactdata.Moved    `json:"artifact_data,omitempty"`
}

// relayedBatch is one batch a server's appliers announced, numbered by the
// server's own sequence — the form it crosses the wire in.
type relayedBatch struct {
	Seq uint64 `json:"seq"`
	liveBatch
}

// heldBatch is one batch as the server holds it: its sequence and its
// encoding, made once when its applier announced it, so the ring is bounded by
// what it would cost to send ([liveRelayRingBytes]) and every follower is
// answered without encoding it again.
type heldBatch struct {
	seq uint64
	raw json.RawMessage
}

// relayAsk is what a follower sends: the epoch and the sequence of the last
// batch it took from this server — none, for a follower that has taken
// nothing from it yet and asks only where it stands — signed whole, with the
// id and the deadline that bind it ([signedask.Request]).
type relayAsk struct {
	// ID and Deadline bind the ask: drawn per ask, so the answer a follower
	// believes is the one to the ask it just sent.
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`

	Epoch string `json:"epoch,omitempty"`
	After uint64 `json:"after"`

	// From is the asking node, for the server's log.
	From string `json:"from,omitempty"`
}

// Binding is the ask's id and deadline ([signedask.Request]).
func (a relayAsk) Binding() (string, time.Time) { return a.ID, a.Deadline }

// relayAnswer is what a server answers: where it stands, and every batch after
// the follower's that it holds, each a [relayedBatch] — or that it cannot
// answer from its own copy at all (Unserved), or no longer reaches back as far
// as the follower (Gap). Signed whole, and naming the ask it answers
// ([signedask.Reply]).
type relayAnswer struct {
	// Request is the id of the ask this answers, empty only where the ask
	// could not be read at all.
	Request  string            `json:"request,omitempty"`
	Epoch    string            `json:"epoch"`
	Head     uint64            `json:"head"`
	Batches  []json.RawMessage `json:"batches,omitempty"`
	Gap      bool              `json:"gap,omitempty"`
	Unserved bool              `json:"unserved,omitempty"`
	// Detail says why an answer is unserved where the server did not take
	// the ask at all — not signed by the fleet, under a key it does not
	// hold, past its deadline — for the follower's log.
	Detail string `json:"detail,omitempty"`
}

// Answers is the id of the ask this answers ([signedask.Reply]).
func (a relayAnswer) Answers() string { return a.Request }

// relayQueue is the two ephemeral verbs the relay rides. AskEach rather than
// Ask, so a follower judges each reply as it arrives and goes on waiting past
// one it cannot believe ([liveRelay.ask]).
type relayQueue interface {
	AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// liveRelay is one data node's half of the relay: the server's ring, and the
// follower's state.
type liveRelay struct {
	self  string
	queue relayQueue

	// wire signs this node's asks and answers and verifies its peers'
	// ([newRelayWire]); taken is the ask ids this node has admitted as a
	// server, each until its deadline ([signedask.Guard]).
	wire  *relayWire
	taken *signedask.Guard

	// roster is every live data node, from memory ([Engine.dataRoster]).
	roster func(context.Context) ([]string, error)

	// faulted reports whether this node's own copy is out of service, as
	// far as it can tell without waiting ([localEstate.faultedUnasked]).
	faulted func(context.Context) bool

	// listening reports whether anything on this node hears what its
	// appliers announce at all — a data node with nothing attached, or only
	// a chat hub that serves no socket here, has nobody to relay to, and a
	// follower that relayed to nobody would be a loop reported as work.
	// deliver hands one batch to its domain's
	// listener, and gap tells every screen to read again — the engine's
	// registered listeners, read per call ([Engine.liveListening],
	// [Engine.deliverLive], [Engine.liveGap]).
	listening func() bool
	deliver   func(liveBatch)
	gap       func()

	// relaying is whether this node's sockets hear a peer now rather than
	// its own appliers.
	relaying atomic.Bool

	mu sync.Mutex
	// epoch names the sequence below; drawn at boot and again whenever this
	// node's copy comes back into service.
	epoch string
	seq   uint64
	// ring is the newest batches, oldest first, and held is what their
	// encodings weigh together — never more than [liveRelayRingBytes].
	ring []heldBatch
	held int
	// moved is closed and replaced by every append, so an ask waiting for a
	// batch wakes the instant one lands.
	moved chan struct{}
	// outOfService is whether this node last answered a follower that its
	// copy is out of service — what tells the next answer it serves to draw
	// a new epoch.
	outOfService bool
}

// newLiveRelay builds a relay with a fresh epoch.
func newLiveRelay(self string, q relayQueue, wire *relayWire,
	roster func(context.Context) ([]string, error),
	faulted func(context.Context) bool, listening func() bool,
	deliver func(liveBatch), gap func()) *liveRelay {

	return &liveRelay{self: self, queue: q, wire: wire, taken: signedask.NewGuard(),
		roster: roster, faulted: faulted, listening: listening, deliver: deliver,
		gap: gap, epoch: rand.Text(), moved: make(chan struct{})}
}

// record keeps one batch this node's appliers announced, for its followers.
//
// ENCODED HERE, ONCE, on the applier's own goroutine: what the ring is bounded
// by is what a follower would be sent, and a batch encoded per follower would
// be the same bytes made again for each. It is a JSON encoding of what the
// applier already built, a small fraction of the commit it follows.
//
// A BATCH NO ANSWER COULD CARRY — larger than the whole ring, or one that will
// not encode — still takes its sequence number, and EMPTIES the ring: every
// follower behind it then finds the ring starting past its last batch, which
// is a gap and a re-read. Skipped with the older batches kept, a follower
// before it would be handed the batches on either side and never learn one
// was missing.
func (r *liveRelay) record(b liveBatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	raw, err := json.Marshal(relayedBatch{Seq: r.seq, liveBatch: b})
	switch {
	case err != nil:
		log.Error("live_relay_batch_unencodable", "seq", r.seq, "error", err.Error())
		r.ring, r.held = nil, 0
	case len(raw) > liveRelayRingBytes:
		r.ring, r.held = nil, 0
	default:
		r.ring = append(r.ring, heldBatch{seq: r.seq, raw: raw})
		r.held += len(raw)
		evict := 0
		for r.held > liveRelayRingBytes {
			r.held -= len(r.ring[evict].raw)
			evict++
		}
		r.ring = slices.Delete(r.ring, 0, evict)
	}
	close(r.moved)
	r.moved = make(chan struct{})
}

// answer is what this node answers one follower's ask, holding it open up to
// wait for a batch it does not hold yet.
func (r *liveRelay) answer(ctx context.Context, ask relayAsk, wait time.Duration) relayAnswer {
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
func (r *liveRelay) answerNow(ctx context.Context, ask relayAsk) (relayAnswer, <-chan struct{}) {
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
		// BACK IN SERVICE: what this node's appliers announced while its
		// copy was out of service, or did not announce at all, is no
		// sequence a follower can trust to be whole — so it is another
		// epoch, which every follower meets as a re-read.
		r.outOfService = false
		r.epoch = rand.Text()
	}
	out := relayAnswer{Epoch: r.epoch, Head: r.seq}
	// THE FIRST SEQUENCE THE RING STILL HOLDS, which for an empty ring is
	// the next it will number: a ring emptied by a batch it could not keep
	// holds nothing up to its head, and a follower anywhere behind that
	// head is behind the ring.
	first := r.seq + 1
	if len(r.ring) > 0 {
		first = r.ring[0].seq
	}
	switch {
	case ask.Epoch != r.epoch:
		// A FOLLOWER NEW TO THIS SERVER, or one whose epoch this node no
		// longer serves: where it stands, and nothing it could not place.
		return out, nil
	case ask.After > r.seq || ask.After+1 < first:
		// A FOLLOWER THIS RING NO LONGER REACHES BACK TO — the batches
		// after its last were evicted, or one was too large to keep — or
		// one claiming a batch this epoch never numbered: it re-reads and
		// starts again from here.
		out.Gap = true
		return out, nil
	}
	for i, b := range r.ring {
		if b.seq > ask.After {
			for _, held := range r.ring[i:] {
				out.Batches = append(out.Batches, held.raw)
			}
			return out, nil
		}
	}
	return out, r.moved
}

// serve makes this node the answerer of its own relay subject.
//
// A RELAY WITHOUT ITS ENVELOPES IS REFUSED rather than serving unsigned: what
// it answers is pushed to a peer's sockets as this node's appliers' work, and
// a server that answered unsigned is the hole the envelopes close.
func (r *liveRelay) serve(ctx context.Context) (queue.Unsubscribe, error) {
	if r.wire == nil {
		return nil, errNoRelayWire
	}
	return r.queue.Serve(ctx, liveRelaySubject(r.self),
		func(ctx context.Context, raw []byte) ([]byte, error) {
			return r.wire.SealReply(r.answerSealed(ctx, raw, time.Now()))
		})
}

// answerSealed answers one sealed ask, and ALWAYS answers, for the estate
// server's reason: a server that stayed silent would be indistinguishable from
// one that is down, and its follower would wait out its whole ask before it
// asked another.
//
// IT ANSWERS FROM ITS RING ONLY WHAT IT ADMITS ([signedask.Channel.Admit]);
// every other ask is answered unserved, with what it was, so its follower
// moves on — bound to the ask's id where the id is the fleet's to name, and to
// nothing where it is not, which no follower believes.
func (r *liveRelay) answerSealed(ctx context.Context, raw []byte, now time.Time) relayAnswer {
	admitted := r.wire.Admit(raw, r.taken, now)
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S: nothing in it is believed, not even the id an
		// answer would name. At ERROR — no server will ever take it, so
		// whoever sent it reached the broker and is not the fleet.
		log.ErrorContext(ctx, "live_relay_ask_refused", "node", r.self,
			"disposition", string(admitted.Disposition), "detail", admitted.Why)
		return relayAnswer{Unserved: true, Detail: fmt.Sprintf(
			"%s refused an ask that is not signed by this fleet", r.self)}
	case signedask.KeyUnknown:
		// A FACT ABOUT THIS SERVER, which an operator changes by adding
		// the key: unserved, bound to the id the follower sent, so it asks
		// a server that holds the key.
		log.WarnContext(ctx, "live_relay_ask_refused", "node", r.self,
			"disposition", string(admitted.Disposition), "claimed_from", admitted.Request.From,
			"detail", admitted.Why)
		return relayAnswer{Request: admitted.ID, Unserved: true, Detail: fmt.Sprintf(
			"%s does not hold the key the ask is signed under (it holds %s)",
			r.self, words.And(r.wire.HeldKeys()))}
	case signedask.Undecodable:
		log.WarnContext(ctx, "live_relay_ask_refused", "node", r.self,
			"disposition", string(admitted.Disposition), "detail", admitted.Why)
		return relayAnswer{Unserved: true, Detail: fmt.Sprintf(
			"%s could not decode an ask the fleet signed: %s", r.self, admitted.Why)}
	case signedask.Stale:
		log.WarnContext(ctx, "live_relay_ask_refused", "node", r.self,
			"disposition", string(admitted.Disposition), "from", admitted.Request.From,
			"detail", admitted.Why)
		return relayAnswer{Request: admitted.ID, Unserved: true,
			Detail: fmt.Sprintf("%s: %s", r.self, admitted.Why)}
	}
	ask := admitted.Request
	out := r.answer(ctx, ask, liveRelayWait)
	out.Request = ask.ID
	return out
}

// follow runs until ctx ends: idle while this node's own copy serves, and
// following a sound peer's appliers while it does not — see the file head.
func (r *liveRelay) follow(ctx context.Context) {
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
				// BACK TO ITS OWN APPLIERS, which committed nothing for
				// the batches the relay carried or announce them a
				// second time: either way, read again.
				log.InfoContext(ctx, "live_relay_stopped", "server", source,
					"detail", "this node's copy is back in service, or nothing "+
						"on it listens for what its appliers announce any more, "+
						"so its dashboards hear its own appliers again")
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
			log.WarnContext(ctx, "live_relay_started",
				"detail", "this node's copy is out of service, so its appliers "+
					"announce nothing its dashboards can trust; their inbox "+
					"movements, chat and artifact rooms are relayed from a data "+
					"node whose copy serves")
		}
		if source == "" {
			source = r.pick(ctx, tried)
			if source == "" {
				clear(tried)
				if !sleepCtx(ctx, liveRelayRetry) {
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
		batches, ok := decodeRelayed(got.Batches)
		if !ok {
			// AN ANSWER THIS BUILD CANNOT READ: none of it is delivered,
			// so everything up to the server's head is read again, and
			// the follower goes on from there.
			log.WarnContext(ctx, "live_relay_unreadable", "server", source,
				"detail", "a relayed batch this build cannot decode; every "+
					"screen reads again")
			after = got.Head
			r.gap()
			continue
		}
		for _, b := range batches {
			if b.Seq <= after {
				continue
			}
			after = b.Seq
			r.deliver(b.liveBatch)
		}
	}
}

// decodeRelayed reads every batch of one answer, or reports false for an
// answer holding one this build cannot decode — which is never delivered in
// part, since a follower that took the batches around it would read on as
// though it had heard everything.
func decodeRelayed(raws []json.RawMessage) ([]relayedBatch, bool) {
	out := make([]relayedBatch, 0, len(raws))
	for _, raw := range raws {
		var b relayedBatch
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, false
		}
		out = append(out, b)
	}
	return out, true
}

// pick is the next live data node to follow — never this one, and never one
// already tried since the last that served — or "" for none.
func (r *liveRelay) pick(ctx context.Context, tried map[string]bool) string {
	nodes, err := r.roster(ctx)
	if err != nil {
		log.DebugContext(ctx, "live_relay_roster_unknown", "error", err.Error())
		return ""
	}
	for _, n := range slices.Sorted(slices.Values(nodes)) {
		if n != r.self && n != "" && !tried[n] {
			return n
		}
	}
	return ""
}

// ask asks one server, reporting false for a server that gave no answer this
// follower believes.
//
// EACH REPLY IS JUDGED AS IT ARRIVES, and one this follower cannot believe —
// not signed by the fleet, signed under a key it does not hold, bound to
// another ask, or one this build cannot decode — is logged and passed over
// while the ask goes on: the transport carries no sender, so a forged reply
// that ended the ask would be anybody on the broker deciding that this server
// did not answer. A server whose own answer never comes is silent, and the
// follower moves on as it does from any silent server.
func (r *liveRelay) ask(ctx context.Context, node string, a relayAsk) (relayAnswer, bool) {
	if r.wire == nil {
		log.ErrorContext(ctx, "live_relay_unanswered", "server", node,
			"error", errNoRelayWire.Error())
		return relayAnswer{}, false
	}
	// THE WAIT AND A ROUND TRIP: the server answers at the end of its wait
	// at the latest, and statelog.ReadBudget is the budget the estate's
	// own reads give a peer to answer in. The ask's deadline is the same
	// instant, which is how long its server remembers its id.
	askCtx, cancel := context.WithTimeout(ctx, liveRelayWait+statelog.ReadBudget)
	defer cancel()
	a.ID, a.From = signedask.NewID(), r.self
	a.Deadline, _ = askCtx.Deadline()
	sealed, err := r.wire.SealRequest(a)
	if err != nil {
		log.ErrorContext(ctx, "live_relay_unanswered", "server", node, "error", err.Error())
		return relayAnswer{}, false
	}
	var (
		got      relayAnswer
		believed bool
	)
	err = r.queue.AskEach(askCtx, liveRelaySubject(node), sealed, func(reply []byte) bool {
		rep, why, openErr := r.wire.OpenReply(reply, a.ID)
		switch {
		case openErr != nil:
			// THE FLEET SIGNED IT FOR THIS ASK and this build cannot read
			// it — a server on a newer build. Nothing else will come.
			log.WarnContext(ctx, "live_relay_unreadable", "server", node,
				"error", openErr.Error())
			return false
		case why != "":
			log.WarnContext(ctx, "live_relay_reply_refused", "server", node,
				"detail", why+"; still waiting for the server's own answer")
			return true
		}
		got, believed = rep, true
		return false
	})
	if !believed {
		if ctx.Err() == nil {
			log.DebugContext(ctx, "live_relay_unanswered", "server", node,
				"error", fmt.Sprint(err))
		}
		return relayAnswer{}, false
	}
	if got.Unserved && got.Detail != "" {
		log.DebugContext(ctx, "live_relay_unserved", "server", node, "detail", got.Detail)
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

// faultedUnasked reports whether this node's copy is OUT OF SERVICE as far as
// it can tell without waiting: a verdict read and found wrong. A copy nobody
// has judged yet and a node whose core is not up are not — [localEstate.For]
// serves both — and a stale verdict answers while the read that replaces it
// runs, exactly as a request is answered.
func (l *localEstate) faultedUnasked(ctx context.Context) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return false
	}
	v, known, _ := l.peek(ctx, c)
	return known && v.fault != ""
}

// announce is what an applier's post-commit hook does with one batch: KEPT FOR
// THIS NODE'S RELAY FOLLOWERS whatever its copy's state — a server answers a
// follower only while its copy serves ([liveRelay.answerNow]) — and handed to
// this node's own sockets only while they are not being relayed a peer's: one
// source at a time.
func (e *Engine) announce(b liveBatch) {
	relay := e.liveRelay.Load()
	if relay != nil {
		relay.record(b)
		if relay.relaying.Load() {
			return
		}
	}
	e.deliverLive(b)
}

// deliverLive hands one batch to whatever this node registered for its domain
// — the arm per field of [liveBatch].
func (e *Engine) deliverLive(b liveBatch) {
	if len(b.Inbox) > 0 {
		e.deliverInbox(b.Inbox)
	}
	if len(b.Chat) > 0 {
		e.deliverChat(b.Chat)
	}
	if len(b.Artifacts) > 0 {
		e.artifactsListeners.tell(b.Artifacts)
	}
	if len(b.ArtifactData) > 0 {
		e.artifactDataListeners.tell(b.ArtifactData)
	}
}

// liveGap tells whatever this node registered, for EVERY domain, that what it
// hears has a gap: a switch of source is one fact about the whole copy, so it
// is a gap in every domain at once. An artifact's and a datastore's listeners
// are told it as their domains' own reset — a movement naming nothing, which
// they already read as "everything decided from these rows may have moved":
// every room re-read and decided again, and every remembered verdict on the
// artifact origin forgotten.
func (e *Engine) liveGap() {
	e.inboxGap()
	e.chatGap()
	e.artifactsListeners.tell([]artifactsdomain.Moved{artifactsdomain.Reset()})
	e.artifactDataListeners.tell([]artifactdata.Moved{artifactdata.ResetMove()})
}

// liveListening reports whether anything on this node hears what any of its
// appliers announce: the API's inbox listener, registered wherever it serves,
// a chat hub that serves sockets here — never a hub attached only for the
// working indicator ([ChatLive.ServesSockets]) — or an artifact or datastore
// listener: the socket's rooms where the API serves, and the artifact origin's
// verdict cache where it does.
func (e *Engine) liveListening() bool {
	return e.inboxListening() || e.chatListening() ||
		e.artifactsListeners.listening() || e.artifactDataListeners.listening()
}

// armLiveRelay makes this data node a server of its live relay and starts the
// follower that takes over what its sockets hear whenever its own copy is out
// of service. A node without `data` holds no applier, so it has nothing to
// relay, and no socket to relay to: it serves no API (ingress requires
// `data`).
func (e *Engine) armLiveRelay(ctx context.Context) error {
	if e.local == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	relay := newLiveRelay(e.id, e.backends.Queue, e.relayWire, e.dataRoster,
		e.local.faultedUnasked, e.liveListening, e.deliverLive, e.liveGap)
	stop, err := relay.serve(ctx)
	if err != nil {
		return fmt.Errorf("engine: serve the live relay: %w", err)
	}
	// THE FOLLOWER'S LIFETIME IS THE ENGINE'S, ended by [Engine.stopLiveRelay]:
	// detached from the context it was armed under, which is the boot's.
	followCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.follow(followCtx)
	}()
	e.liveRelay.Store(relay)
	e.stopLiveRelayFn = func(ctx context.Context) {
		cancel()
		<-done
		if err := stop(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "live_relay_not_withdrawn", "error", err)
		}
	}
	return nil
}

// stopLiveRelay ends the follower and withdraws this node as a server,
// before the broker closes.
func (e *Engine) stopLiveRelay(ctx context.Context) {
	if e.stopLiveRelayFn == nil {
		return
	}
	e.stopLiveRelayFn(ctx)
	e.stopLiveRelayFn = nil
}
