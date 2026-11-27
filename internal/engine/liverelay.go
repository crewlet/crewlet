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
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// The live relay: while this node's copy of a domain is out of service it
// stays in rotation ([localEstate.For]), but that domain's applier announces
// nothing its dashboards can trust, so its sockets hear a sound peer's applier
// for that domain instead and its own announcements of it are withheld. The
// switch is PER DOMAIN ([liveDomains], [localEstate.outOfService]), the
// machinery one ring, sequence, follower and subscription per node, and every
// switch re-reads that domain's screens ([Engine.liveGap]). A follower reads
// one server by that server's own epoch and sequence, so what it cannot place
// is a detected gap, never a lost batch. Every ask and answer is signed on a
// signedask channel of its own, and an answer the follower cannot believe is
// no answer (package doc, "The live relay").

// liveRelaySubject is the subject a node serves its relay on, one node per
// subject because a follower reads ONE server's sequence. The node id is
// escaped as a token: an id may carry a dot, which would split it in two.
func liveRelaySubject(node string) string {
	return topics.LiveRelayPrefix + coord.DocumentKey(node)
}

// liveRelayChannel is the relay's channel, the stem of the two key domains its
// asks and answers are signed under (`live.relay.request`,
// `live.relay.reply`): its own, so no other channel's envelope,
// presence frame or state-log record verifies as one.
const liveRelayChannel = "live.relay"

// relayWire is the relay's envelopes: one per node, both halves, since a data
// node both asks (as a follower) and answers (as a server).
type relayWire = signedask.Channel[relayAsk, relayAnswer]

// newRelayWire builds this node's relay envelopes over the fleet keyring. The
// keyring is required on every node, so this fails only on a bootstrap
// [config.Bootstrap.Validate] already refuses.
func newRelayWire(ring recordseal.Keyring) (*relayWire, error) {
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

// liveRelayRingBytes bounds what a server keeps for its followers, ENCODED.
//
// BYTES, NOT BATCHES: an inbox or artifact movement is ids and a count, while
// one chat commit can carry a thousand messages with excerpts
// ([chatlet.MaxExcerpt], [statelog.ApplyTxRowBudget]) and a datastore batch
// whole documents. HALF OF [queue.MaxPayloadBytes], as a chat transcript page:
// an answer is a suffix of the ring, so the whole ring fits one reply with
// room for the envelope. It need hold only one round trip's commits (a few
// kilobytes at the declared census); an overflow, or one batch larger than
// the ring, is a detected gap and a re-read ([liveRelay.record]). A BOUND, NOT
// A SIZE: with no copy out of service anywhere it holds nothing
// ([liveRelayHold]).
const liveRelayRingBytes = queue.MaxPayloadBytes / 2

// liveRelayHold is how long a server keeps a batch a follower has not taken:
// one ask's ROUND TRIP after the server's latest answer, or after the batch
// itself while an ask is held open.
//
// [statelog.ReadBudget], two seconds: a follower asks again once an answer
// lands and is handed to listeners that never block, so two transits lie
// between, which is what the estate's reads give a peer and what
// [liveRelay.ask] allows beyond the server's wait. A slower follower is
// answered a gap and re-reads.
const liveRelayHold = statelog.ReadBudget

// liveRelayWait is how long a server holds an ask open with nothing newer.
//
// FIVE SECONDS, and it bounds both edges: a follower notices its own copy back
// in service, or its server's out of it, within one wait. The relay exists to
// beat the Inbox screen's 30-second fallback poll, and an idle follower costs
// only one ask per wait, existing only while its node's copy is out of service.
const liveRelayWait = 5 * time.Second

// liveRelayRetry is how long a follower pauses when no data node could serve
// it — every peer silent, out of service itself, or none live — before it asks
// again: [servingRecheck], the cadence the copy's own verdict is read at, so a
// follower is never more eager than the judgement it follows.
const liveRelayRetry = servingRecheck

// liveBatch is one applier's committed batch, announced to this node's sockets.
//
// EXACTLY ONE FIELD IS SET: a batch is one applier's commit, and each applier
// is one domain's ([liveBatch.domain]). A domain that starts announcing is one
// more field here (or [registration.Inbox] source) and one more arm in
// [liveBatch.domain], [liveDomains], [Engine.deliverLive], [Engine.liveGap]
// and [Engine.liveListening].
//
// ADDITIVE ON THE WIRE: a newer server's batch of a domain this build does not
// know decodes with no field set and is delivered to nobody.
type liveBatch struct {
	Inbox        []inbox.Movement        `json:"inbox,omitempty"`
	Chat         []chatlet.Applied       `json:"chat,omitempty"`
	Artifacts    []artifactsdomain.Moved `json:"artifacts,omitempty"`
	ArtifactData []artifactdata.Moved    `json:"artifact_data,omitempty"`
}

// domain is the domain whose applier committed b (for an inbox batch the
// source its movements name, one per batch), or "" for a kind this build does
// not know.
func (b liveBatch) domain() string {
	switch {
	case len(b.Inbox) > 0:
		return b.Inbox[0].Source
	case len(b.Chat) > 0:
		return chatlet.Domain{}.Name()
	case len(b.Artifacts) > 0:
		return artifactsdomain.Domain{}.Name()
	case len(b.ArtifactData) > 0:
		return artifactdata.Domain{}.Name()
	}
	return ""
}

// liveDomains is every domain whose applier announces to this node's sockets,
// each judged on its own verdict, in a fixed order: the inbox sources
// ([inboxSources]), chat, the artifacts and the datastore. EACH ONCE: the
// artifacts announce both their moves and their comments' inbox, on one copy.
func liveDomains() []string {
	out := inboxSources()
	for _, d := range []string{chatlet.Domain{}.Name(), artifactsdomain.Domain{}.Name(),
		artifactdata.Domain{}.Name()} {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// relayedBatch is one batch a server's appliers announced, numbered by the
// server's own sequence — the form it crosses the wire in.
type relayedBatch struct {
	Seq uint64 `json:"seq"`
	liveBatch
}

// heldBatch is one batch as the server holds it, encoded once when announced,
// so the ring is bounded by what it costs to send ([liveRelayRingBytes]) and
// no answer encodes it again.
type heldBatch struct {
	seq uint64
	raw json.RawMessage
}

// relayAsk is what a follower sends: the epoch and sequence of the last batch
// it took from this server (none for a follower new to it), signed whole with
// the id and deadline that bind it ([signedask.Request]).
type relayAsk struct {
	// ID and Deadline bind the ask, drawn per ask, so a follower believes only
	// the answer to the ask it just sent.
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`

	Epoch string `json:"epoch,omitempty"`
	After uint64 `json:"after"`

	// Domains is every domain the follower relays; the server answers only
	// while its own copy of each serves.
	Domains []string `json:"domains,omitempty"`

	// From is the asking node, for the server's log.
	From string `json:"from,omitempty"`
}

// Binding is the ask's id and deadline ([signedask.Request]).
func (a relayAsk) Binding() (string, time.Time) { return a.ID, a.Deadline }

// Writes is false: the relay answers what the peer's appliers committed and
// changes nothing (internal/signedask).
func (a relayAsk) Writes() bool { return false }

// relayAnswer is a server's answer: where it stands and every held batch after
// the follower's ([relayedBatch]), or that it cannot answer for a domain the
// follower relays (Unserved) or no longer reaches back that far (Gap). Signed
// whole, naming the ask it answers ([signedask.Reply]).
type relayAnswer struct {
	// Request is the id of the ask this answers, empty only where the ask
	// could not be read at all.
	Request  string            `json:"request,omitempty"`
	Epoch    string            `json:"epoch"`
	Head     uint64            `json:"head"`
	Batches  []json.RawMessage `json:"batches,omitempty"`
	Gap      bool              `json:"gap,omitempty"`
	Unserved bool              `json:"unserved,omitempty"`
	// Detail says why an answer is unserved, for the follower's log: the ask
	// was not taken (not the fleet's, an unheld key, past its deadline), or a
	// relayed domain's copy here is out of service.
	Detail string `json:"detail,omitempty"`
}

// Answers is the id of the ask this answers ([signedask.Reply]).
func (a relayAnswer) Answers() string { return a.Request }

// relayQueue is the two ephemeral verbs the relay rides. AskEach, not Ask, so
// a follower judges each reply as it arrives and waits past one it cannot
// believe ([liveRelay.ask]).
type relayQueue interface {
	AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// liveRelay is one data node's half of the relay: the server's ring, and the
// follower's state.
type liveRelay struct {
	self  string
	queue relayQueue

	// wire signs this node's asks and answers and verifies its peers';
	// taken is the ask ids admitted as a server, each until its deadline
	// ([signedask.Guard]).
	wire  *relayWire
	taken *signedask.Guard

	// roster is every live data node, from memory ([Engine.dataRoster]).
	roster func(context.Context) ([]string, error)

	// domains is every announcing domain ([liveDomains]); outOfService says
	// which of them this node's copies are out of service, in that order,
	// without waiting ([localEstate.outOfService]).
	domains      []string
	outOfService func(ctx context.Context, domains ...string) []string

	// listening reports whether anything here hears its appliers at all: a
	// follower relaying to nobody would be a loop reported as work. deliver
	// hands a batch to its domain's listener, and gap tells the named
	// domains' screens to read again. All read the engine's listeners per
	// call ([Engine.liveListening], [Engine.deliverLive], [Engine.liveGap]).
	listening func() bool
	deliver   func(liveBatch)
	gap       func(domains []string)
	// answers is the node's answer budget an answer holds its weight of,
	// after its wait ([relayAnswerWeight]).
	answers *queue.AnswerBudget

	// relayed is the domains this node's sockets hear from a peer now, in
	// [liveDomains]' order; nil or empty for none.
	relayed atomic.Pointer[[]string]

	mu sync.Mutex
	// epoch names the sequence below; drawn at boot and again whenever this
	// node's copy comes back into service.
	epoch string
	seq   uint64
	// ring is the newest batches, oldest first, and held is what their
	// encodings weigh together — never more than [liveRelayRingBytes].
	ring []heldBatch
	held int
	// moved is closed and replaced by every batch, so an ask waiting for a
	// batch wakes the instant one lands.
	moved chan struct{}
	// heldUntil is when the latest answer's [liveRelayHold] runs out, and
	// waiting how many asks are held open now: while either says a
	// follower may still ask, a batch is kept ([liveRelay.record]).
	heldUntil time.Time
	waiting   int
	// out is the domains this node last found out of service when answering,
	// so the next answer that finds one back draws a new epoch.
	out []string
}

// newLiveRelay builds a relay with a fresh epoch, judging domains.
func newLiveRelay(self string, q relayQueue, wire *relayWire,
	roster func(context.Context) ([]string, error), domains []string,
	outOfService func(context.Context, ...string) []string, listening func() bool,
	deliver func(liveBatch), gap func([]string), answers *queue.AnswerBudget) *liveRelay {

	return &liveRelay{self: self, queue: q, wire: wire, taken: signedask.NewGuard(),
		roster: roster, domains: domains, outOfService: outOfService,
		listening: listening, deliver: deliver, gap: gap, answers: answers, epoch: rand.Text(),
		moved: make(chan struct{})}
}

// relays reports whether this node's sockets hear domain from a peer now,
// and so whether this node's own announcements of it are withheld.
func (r *liveRelay) relays(domain string) bool {
	relayed := r.relayed.Load()
	return relayed != nil && slices.Contains(*relayed, domain)
}

// record keeps one batch this node's appliers announced, for its followers.
//
// ENCODED ONCE, here on the applier's goroutine: the ring is bounded by what a
// follower is sent, and encoding per follower would remake the same bytes. A
// BATCH NOBODY CAN ASK FOR (none answered within [liveRelayHold], none
// waiting) takes its sequence number, is not kept, and drops the ring: the
// normal state costs a counter and no allocation, not even the wake-up, since
// an ask waits on it only while counted in waiting. A BATCH NO ANSWER COULD
// CARRY (larger than the ring, or unencodable) takes its number and EMPTIES
// the ring, so every follower behind it meets a gap;
// skipped with older batches kept, a follower would be handed the batches on
// either side and never learn one was missing.
func (r *liveRelay) record(b liveBatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	if r.waiting == 0 && !time.Now().Before(r.heldUntil) {
		r.ring, r.held = nil, 0
		return
	}
	defer func() {
		close(r.moved)
		r.moved = make(chan struct{})
	}()
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
}

// answer is what this node answers one follower's ask, holding it open up to
// wait for a batch it does not hold yet.
//
// WHILE IT IS HELD OPEN it counts as a follower that may still ask, so every
// batch recorded meanwhile is kept for it ([liveRelay.record]).
func (r *liveRelay) answer(ctx context.Context, ask relayAsk, wait time.Duration) relayAnswer {
	r.mu.Lock()
	r.waiting++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.waiting--
		r.mu.Unlock()
	}()
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
	outOfService := r.outOfService(ctx, r.domains...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.ContainsFunc(r.out, func(d string) bool {
		return !slices.Contains(outOfService, d)
	}) {
		// A COPY BACK IN SERVICE: what its applier announced while out of
		// service is no sequence a follower can trust whole, so draw another
		// epoch (a re-read for every follower) and drop the ring it numbered.
		r.epoch = rand.Text()
		r.ring, r.held = nil, 0
	}
	r.out = outOfService
	if wrong := slices.DeleteFunc(slices.Clone(ask.Domains), func(d string) bool {
		return !slices.Contains(outOfService, d)
	}); len(wrong) > 0 {
		// A RELAYED DOMAIN this node's copy cannot vouch for either: the
		// follower moves to a server whose copy does.
		return relayAnswer{Unserved: true, Detail: fmt.Sprintf(
			"%s holds its copy of %s out of service", r.self, words.And(wrong))}, nil
	}
	// ANSWERED: whatever it is told, this follower asks again within a
	// round trip, and is owed every batch recorded until then.
	if until := time.Now().Add(liveRelayHold); until.After(r.heldUntil) {
		r.heldUntil = until
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
		// A FOLLOWER THIS RING NO LONGER REACHES (evicted, too large to
		// keep, or asked after [liveRelayHold]) or claiming a batch this
		// epoch never numbered: it re-reads and starts again from here.
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
	// WORK DONE NOW: an answer reads this node's ring and returns. Its weight
	// is held AFTER its wait, never through it: what it carries is known
	// then, and a wait holding the budget would hold every other answer.
	return r.queue.Serve(ctx, liveRelaySubject(r.self), queue.ConcurrentAnswers,
		func(ctx context.Context, raw []byte) ([]byte, error) {
			out := r.answerSealed(ctx, raw, time.Now())
			wait, cancel := context.WithTimeout(ctx, liveRelayHold)
			release, err := r.answers.Acquire(wait, queue.AnswerLiveRelay, relayAnswerWeight(out))
			cancel()
			if err != nil {
				return nil, fmt.Errorf("engine: answer a live relay ask: %w", err)
			}
			defer release()
			return r.wire.SealReply(out)
		})
}

// relayAnswerWeight is what an answer holds of the node's answer budget
// ([queue.AnswerBudget]) while it is sealed and its reply written: the batches
// it carries five times, from the code (the ring's own bytes, their encoding
// as raw JSON, the encoder's buffer, and the seal's base64 at 4/3), and
// [relayEnvelopeBytes] for the rest of the reply.
func relayAnswerWeight(out relayAnswer) int64 {
	carried := 0
	for _, b := range out.Batches {
		carried += len(b)
	}
	return 5*int64(carried) + relayEnvelopeBytes
}

// relayEnvelopeBytes is the most an answer's fields beside its batches weigh,
// sealed: an epoch, two sequences and a sentence, a few hundred bytes, held
// at 4 KiB.
const relayEnvelopeBytes = 4 << 10

// answerSealed answers one sealed ask, and ALWAYS answers: a silent server is
// indistinguishable from a down one, and its follower would wait out its whole
// ask. It answers from its ring only an ask it admits
// ([signedask.Channel.Admit]); any other is answered unserved so its follower
// moves on, bound to the ask's id only where the id is the fleet's to name.
func (r *liveRelay) answerSealed(ctx context.Context, raw []byte, now time.Time) relayAnswer {
	admitted := r.wire.Admit(ctx, raw, r.taken, now)
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S: not even its id is believed. At ERROR: whoever
		// sent it reached the broker and is not the fleet.
		log.ErrorContext(ctx, "live_relay_ask_refused", "node", r.self,
			"disposition", string(admitted.Disposition), "detail", admitted.Why)
		return relayAnswer{Unserved: true, Detail: fmt.Sprintf(
			"%s refused an ask that is not signed by this fleet", r.self)}
	case signedask.KeyUnknown:
		// A FACT ABOUT THIS SERVER (an operator adds the key): unserved,
		// bound to the follower's id, so it asks a server that holds it.
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
	case signedask.Stale, signedask.Unrecorded:
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

// follow runs until ctx ends: idle while every announcing domain's copy here
// serves, and following a sound peer's appliers for those that do not.
func (r *liveRelay) follow(ctx context.Context) {
	var (
		source string
		epoch  string
		after  uint64
		// relayed is the domains this follower relays now, in
		// [liveDomains]' order.
		relayed []string
		// tried is the servers asked since the last that served, so a
		// follower tries the others before it pauses.
		tried = map[string]bool{}
	)
	switchSource := func() {
		source, epoch, after = "", "", 0
	}
	for ctx.Err() == nil {
		var want []string
		if r.listening() {
			want = r.outOfService(ctx, r.domains...)
		}
		if !slices.Equal(want, relayed) {
			r.switchDomains(ctx, source, epoch != "", relayed, want)
			relayed = want
		}
		if len(relayed) == 0 {
			switchSource()
			clear(tried)
			if !sleepCtx(ctx, servingRecheck) {
				return
			}
			continue
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
		got, ok := r.ask(ctx, source, relayAsk{Epoch: epoch, After: after, Domains: relayed})
		switch {
		case !ok || got.Unserved:
			switchSource()
			continue
		case got.Epoch != epoch || got.Gap:
			// A SERVER NEW TO THIS FOLLOWER, or one it cannot place
			// itself in: relay from here, and re-read the relayed
			// domains before it.
			epoch, after = got.Epoch, got.Head
			clear(tried)
			r.gap(relayed)
			continue
		}
		clear(tried)
		batches, ok := decodeRelayed(got.Batches)
		if !ok {
			// AN ANSWER THIS BUILD CANNOT READ: deliver none of it,
			// re-read up to the server's head, and go on from there.
			log.WarnContext(ctx, "live_relay_unreadable", "server", source,
				"detail", "a relayed batch this build cannot decode; every "+
					"relayed domain's screens read again")
			after = got.Head
			r.gap(relayed)
			continue
		}
		for _, b := range batches {
			if b.Seq <= after {
				continue
			}
			after = b.Seq
			// ONLY RELAYED DOMAINS: every other domain's own applier
			// here already told its sockets.
			if slices.Contains(relayed, b.domain()) {
				r.deliver(b.liveBatch)
			}
		}
	}
}

// switchDomains moves this node's sockets onto a peer for the domains in want
// and not had, and back onto their own appliers for those in had and not want:
// each a switch of THAT domain's source, so a re-read of its screens alone. A
// domain coming back is re-read once its own announcements flow again (after
// relayed is stored). One starting to be relayed is re-read here only when
// the follower is already placed; otherwise the answer that places it does.
func (r *liveRelay) switchDomains(ctx context.Context, server string, placed bool,
	had, want []string) {

	started := slices.DeleteFunc(slices.Clone(want), func(d string) bool {
		return slices.Contains(had, d)
	})
	stopped := slices.DeleteFunc(slices.Clone(had), func(d string) bool {
		return slices.Contains(want, d)
	})
	relayed := slices.Clone(want)
	r.relayed.Store(&relayed)
	if len(started) > 0 {
		log.WarnContext(ctx, "live_relay_started", "domains", started,
			"detail", "this node's copies of these domains are out of service, "+
				"so their appliers announce nothing its dashboards can trust; "+
				"their inbox movements, chat and artifact rooms are relayed from "+
				"a data node whose copies of them serve")
		if placed {
			r.gap(started)
		}
	}
	if len(stopped) > 0 {
		// BACK TO ITS OWN APPLIERS, which either committed nothing for
		// what the relay carried or announce it twice: read again.
		log.InfoContext(ctx, "live_relay_stopped", "server", server, "domains", stopped,
			"detail", "this node's copies of these domains are back in service, "+
				"or nothing on it listens for what its appliers announce any "+
				"more, so its dashboards hear its own appliers for them again")
		r.gap(stopped)
	}
}

// decodeRelayed reads every batch of one answer, or reports false when one
// will not decode: never delivered in part, since a follower taking the
// batches around it would read on as though it had heard everything.
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

// ask asks one server, reporting false when no answer this follower believes
// came.
//
// EACH REPLY IS JUDGED AS IT ARRIVES, and one it cannot believe (not the
// fleet's, an unheld key, bound to another ask) is logged and passed over
// while the ask goes on: the transport carries no sender, so a forged reply
// that ended the ask would let anybody on the broker decide this server did
// not answer. A server whose answer never comes is silent, and is left.
func (r *liveRelay) ask(ctx context.Context, node string, a relayAsk) (relayAnswer, bool) {
	if r.wire == nil {
		log.ErrorContext(ctx, "live_relay_unanswered", "server", node,
			"error", errNoRelayWire.Error())
		return relayAnswer{}, false
	}
	// THE WAIT AND A ROUND TRIP: the server answers by its wait's end at the
	// latest, plus statelog.ReadBudget, what the estate's reads give a peer.
	// The ask's deadline is the same instant, and is how
	// long its server remembers its id.
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
			// it (a newer server): nothing else will come.
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

// announce is an applier's post-commit hook for one batch: always KEPT FOR
// THIS NODE'S RELAY FOLLOWERS ([liveRelay.answerNow] decides whether to serve
// them), and handed to this node's sockets only while they are not relayed a
// peer's announcements of its domain: one source per domain at a time.
func (e *Engine) announce(b liveBatch) {
	relay := e.liveRelay.Load()
	if relay != nil {
		relay.record(b)
		if relay.relays(b.domain()) {
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

// liveGap tells this node's listeners for each of domains that what they heard
// of it has a gap, in that domain alone: an inbox source's watched inboxes,
// chat's screens, and the artifacts' and datastore's listeners as their own
// reset (a movement naming nothing: every room re-read, every remembered
// verdict on the artifact origin forgotten).
func (e *Engine) liveGap(domains []string) {
	for _, domain := range domains {
		switch domain {
		case chatlet.Domain{}.Name():
			e.chatGap()
		case artifactsdomain.Domain{}.Name():
			// BOTH OF ITS ANNOUNCEMENTS: what its commits moved, and the
			// inbox its comments moved.
			e.artifactsListeners.tell([]artifactsdomain.Moved{artifactsdomain.Reset()})
			e.inboxGap(domain)
		case artifactdata.Domain{}.Name():
			e.artifactDataListeners.tell([]artifactdata.Moved{artifactdata.ResetMove()})
		default:
			e.inboxGap(domain)
		}
	}
}

// liveListening reports whether anything on this node hears its appliers: the
// API's inbox listener, a chat hub serving sockets here (not one attached
// only for the working indicator, [ChatLive.ServesSockets]), or an artifact
// or datastore listener (the socket's rooms, the artifact origin's cache).
func (e *Engine) liveListening() bool {
	return e.inboxListening() || e.chatListening() ||
		e.artifactsListeners.listening() || e.artifactDataListeners.listening()
}

// armLiveRelay makes this data node a server of its live relay and starts its
// follower. A node without `data` has no applier to relay and no socket to
// relay to (ingress requires `data`).
func (e *Engine) armLiveRelay(ctx context.Context) error {
	if e.local == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	relay := newLiveRelay(e.id, e.backends.Queue, e.relayWire, e.dataRoster,
		liveDomains(), e.local.outOfService, e.liveListening, e.deliverLive, e.liveGap, e.answerBudget)
	stop, err := relay.serve(ctx)
	if err != nil {
		return fmt.Errorf("engine: serve the live relay: %w", err)
	}
	// THE FOLLOWER'S LIFETIME IS THE ENGINE'S, ended by [Engine.stopLiveRelay],
	// not the boot context it was armed under.
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
