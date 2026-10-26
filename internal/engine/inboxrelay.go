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
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INBOX RELAY: how a node whose own copy of a domain is OUT OF SERVICE
// still tells its open dashboards whose inbox that domain moved.
//
// # Why one is needed at all
//
// A node learns whose inbox moved from its OWN appliers, after every committed
// batch ([Engine.SetOnInboxMoved]), and each domain's announcements are
// complete only while that domain's applier applies every record. A copy that
// is WRONG — an applier halted at a record it cannot decode, a stalled prefix,
// an estate file shut, rows below the log — goes out of service and the node
// STAYS in rotation ([localEstate.For]): its seats' calls and its dashboards'
// reads go to a sound data node through the estate's router, and /ready
// follows whether it can route rather than whether its copy is sound. So its
// dashboards keep reading correct data — and an applier that commits nothing
// announces nothing, so every socket on that node stopped receiving
// `inbox_changed` until it reconnected somewhere else, which a node still in
// rotation gives it no reason to do.
//
// # Each domain from its own sound copy
//
// A fault is ONE log's (ADR-0049), so the relay judges each source — each
// domain whose applier announces, [registration.Inbox] — on that domain's own
// copy. While this node's copy of a source serves, its sockets hear that
// source from its own applier and nothing else. While it does not, they hear
// a SOUND PEER's applier for it — a peer whose copy of THAT domain serves,
// whatever its other copies are doing — relayed, and the node's own
// announcements of that source, whatever an out-of-service applier still
// commits, are withheld: two sources would push one movement twice, and the
// one being withheld is the one nobody vouches for. The other domains' copies
// are untouched by it and their appliers are still heard, because a wrong copy
// of one domain leaves every other domain's announcements complete. Every
// SWITCH of a source, either way, tells every watched inbox to read itself
// again ([Engine.SetOnInboxGap]), because what moved between the last
// movement one source delivered and the first the other does is not replayed:
// the rows are the truth, and a screen re-reads them.
//
// # A follower reads ONE server, by that server's own sequence
//
// Every data node numbers, per source, the batches its applier announced, by
// a sequence of that source's own under an EPOCH it draws at boot and draws
// again whenever its copy of that domain comes back into service. A follower
// of a source asks one server for everything after the last batch of it it
// took, and the server answers as soon as it holds one (or at the end of
// [inboxRelayWait]). Positions on the domain's log would have let a follower
// switch servers without a re-read, and are not what a batch has: a batch is
// whatever one node's applier committed together, retained records
// reprocessed out of order included, so two nodes' batches never line up. A
// sequence of the server's own is exact for the one thing it is used for —
// "after the last one I took" — and anything it cannot answer is DETECTED
// rather than lost: another epoch, a sequence the server no longer holds, or
// another server, each of which is a re-read. One sequence PER SOURCE, so a
// domain whose applier is busy never evicts another's batches from what its
// follower still reads.
//
// # A server keeps only what a follower can still ask for
//
// A follower new to a server is answered where that server's stream stands
// and nothing before it, and one that moves to another server starts over
// there ([inboxRelay.followSource]). So NO FOLLOWER IS EVER SERVED A BATCH
// RECORDED BEFORE THE LAST ANSWER IT TOOK, and all a server has to keep of a
// source is what a follower answered earlier has not taken yet: the batches
// recorded since that answer, for one round trip after it, by when the
// follower has asked again ([inboxRelayHold]) — and, while an ask is held
// open, the batches that wake it. Never more than [inboxRelayMoves]
// movements of a source, however recently somebody asked. A RING KEPT BY
// COUNT is the obvious shape and the wrong one: a thousand batches of every
// source on every data node, each a whole apply transaction's movements
// however wide, for followers that do not exist in the normal state — with
// no copy of the source out of service anywhere nobody asks, and a server
// keeps none of it. A follower slower than a round trip, or behind a burst
// wider than the bound, is answered a gap and re-reads, as one behind a
// server back in service is.
//
// # One server per node, on one subject, whatever the domains
//
// A node serves every source's streams on ONE subject, registered once at
// boot ([Engine.armInboxRelay]): an ask names its source. A registration per
// domain would be a subscription, and a flush, per domain on every data node
// for an exchange that runs only while some copy is wrong.
//
// # Not signed, deliberately
//
// The fleet's ephemeral channels that are INSTRUCTIONS are signed
// (internal/signedask): a node carries the request out as somebody. This is a
// question a node answers about itself, and what the follower does with the
// answer is tell a screen to read its inbox again — through the inbox question
// and its own authority, which decide what it sees. A forged answer can cause
// a re-read and nothing else.

// inboxRelaySubject is the one subject node serves its relay on —
// [topics.InboxRelayPrefix] and the node, one node per subject, because a
// follower reads ONE server's sequences. THE NODE ID IS ESCAPED as a subject
// token, for the estate subject's reason: an id may carry a dot, which would
// split it in two.
func inboxRelaySubject(node string) string {
	return topics.InboxRelayPrefix + coord.DocumentKey(node)
}

// inboxRelayHold is how long a server keeps a batch of a source after the
// last moment a follower that has not taken it was answered: one ask's ROUND
// TRIP after the stream's latest answer — or after the batch itself, while an
// ask of the source is held open for it.
//
// [statelog.ReadBudget], TWO SECONDS. A follower asks again the moment an
// answer lands, once it has handed the answer's batches to sockets that never
// block ([Engine.SetOnInboxMoved]), so between an answer leaving and the next
// ask arriving lie two transits and that hand-off: what the budget the
// estate's own reads give a peer to answer in bounds, and exactly what the
// follower's own ask allows beyond the server's wait ([inboxRelay.ask]). A
// follower slower than that is answered a gap and re-reads, never a lost
// movement.
const inboxRelayHold = statelog.ReadBudget

// inboxRelayMoves is the most movements a server keeps of one source, however
// recently a follower asked.
//
// [statelog.ApplyTxRowBudget], FOUR THOUSAND. A batch is one apply
// transaction's movements ([inbox.Batch]), each at least one notice row that
// transaction wrote or deleted, and the row budget closes a transaction at
// the first record boundary past it — so the bound holds a whole
// transaction's movements, but for one whose last record alone is wider than
// the budget. It is also about what an applier commits in [inboxRelayHold] at
// the measured drain rate of some two thousand rows a second (statelog's
// budgets.go). A batch wider than the bound is never kept, and every follower
// that has not taken it re-reads — for a transaction that moved thousands of
// inboxes at once, what their screens would do anyway. And it is what keeps
// an ANSWER inside one reply, since an answer carries every batch the
// follower has not taken and a reply the broker refuses is no answer at all:
// the widest the bound allows — four thousand batches of one movement at the
// longest handle, with a subject and a reason as long — is 1.1 MiB of JSON, a
// seventh of [queue.MaxPayloadBytes] (TestTheWidestRelayAnswerFitsOneReply).
//
// WHAT IT HOLDS, measured once (BenchmarkRecordingAnInboxBatch, -benchtime
// 200x -count 1, two E-cores of an Intel Core Ultra 7 265U, Go 1.27.2): a
// full ring held 795 KiB at one movement a batch, 574 KiB at a hundred and
// 539 KiB at a thousand — the slot, the movement and its handle and subject,
// about 200 bytes a movement at the narrowest batch and 140 at the widest —
// and recorded a batch into it in 156 ns, 3.6 µs and 41 µs, with one
// allocation, its own copy. That is PER SOURCE, and only on the data node its
// followers ask — the first live one by name whose own copy of the source
// serves — while some node's copy of that source is out of service, and a
// ring fills to the bound only where its source commits that many movements
// within [inboxRelayHold]. Every other data node, and every node of a fleet
// with no copy out of service, holds nothing, and records a batch in 73 ns
// with no allocation.
const inboxRelayMoves = statelog.ApplyTxRowBudget

// inboxRelayWait is how long a server holds a follower's ask open when it has
// nothing newer of its source to answer with.
//
// FIVE SECONDS, and it bounds both edges of the relay: a follower notices its
// own copy back in service, or its server's copy out of it, within one wait.
// The relay exists to beat the 30-second poll the Inbox screen falls back on,
// so a wait anywhere near that would buy nothing at either edge — and an idle
// follower costs one ask per wait, and a follower exists only while its node's
// copy of its source is out of service.
const inboxRelayWait = 5 * time.Second

// inboxRelayRetry is how long a follower pauses when no data node could serve
// it — every peer silent, out of service itself, or none live — before it asks
// again: [servingRecheck], the cadence the copy's own verdict is read at, so a
// follower is never more eager than the judgement it follows.
const inboxRelayRetry = servingRecheck

// relayedBatch is one batch a server's applier announced of one source,
// numbered by the server's own sequence of that source. Every movement in it
// names the source ([inbox.Movement.Source]).
type relayedBatch struct {
	Seq   uint64           `json:"seq"`
	Moved []inbox.Movement `json:"moved"`
}

// heldBatch is a batch as its server keeps it: with the instant from which no
// follower that has not taken it can still ask for it.
type heldBatch struct {
	relayedBatch
	until time.Time
}

// relayAsk is what a follower sends: the source it follows, and the epoch and
// the sequence of the last batch of it it took from this server — none, for a
// follower that has taken nothing from it yet and asks only where it stands.
type relayAsk struct {
	Source string `json:"source"`
	Epoch  string `json:"epoch,omitempty"`
	After  uint64 `json:"after"`
}

// relayAnswer is what a server answers about one source: where its stream of
// it stands, and every batch after the follower's that it holds — or that it
// cannot answer for that source from its own copy at all (Unserved), or no
// longer reaches back as far as the follower (Gap).
type relayAnswer struct {
	Source   string         `json:"source"`
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

// relayStream is one source's half of a data node's relay: the batches its
// applier announced, numbered under an epoch of the source's own, those of
// them a follower may still ask for, and whether this node's sockets hear
// that source from a peer now.
type relayStream struct {
	source string

	// relaying is whether this node's sockets hear this source from a peer
	// now rather than from its own applier — read on every announcement,
	// without the lock.
	relaying atomic.Bool

	// Guarded by [inboxRelay.mu].
	//
	// epoch names the sequence; drawn at boot and again whenever this
	// node's copy of the source comes back into service.
	epoch string
	seq   uint64
	// ring is the batches a follower may still ask for, oldest first: a run
	// of consecutive sequences ending at seq, or none at all
	// ([relayStream.keep]), carrying moves movements between them.
	ring  []heldBatch
	moves int
	// heldUntil is the instant until which a follower that has not taken a
	// batch recorded now may still ask for it: [inboxRelayHold] past the
	// stream's latest answer, or past now while an ask waits. It never moves
	// back, so the ring's batches expire oldest first.
	heldUntil time.Time
	// waiting is how many asks are held open on this stream now.
	waiting int
	// moved is closed by the next batch, so an ask waiting for one wakes the
	// instant it lands. Made by the first ask that waits, and nil while none
	// does, so a batch nobody waits for costs no channel.
	moved chan struct{}
	// outOfService is whether this node last answered a follower that its
	// copy of the source is out of service — what tells the next answer it
	// serves to draw a new epoch.
	outOfService bool
}

// inboxRelay is one data node's half of the relay: the server's streams, one
// per source, and their followers.
type inboxRelay struct {
	self  string
	queue relayQueue

	// roster is every live data node, from memory ([Engine.dataRoster]).
	roster func(context.Context) ([]string, error)

	// sources are the domains whose appliers announce movements, in the
	// register's order ([inboxSources]), and streams is one per source —
	// built once, so read without the lock.
	sources []string
	streams map[string]*relayStream

	// faulted is which of the domains it names this node's copy holds out
	// of service, as far as it can tell without waiting
	// ([localEstate.outOfService]).
	faulted func(ctx context.Context, domains ...string) []string

	// listening reports whether anything on this node hears movements at
	// all — a data node that serves no API has nobody to relay to, and a
	// follower that relayed to nobody would be a loop reported as work.
	// deliver hands one batch to this node's sockets, and gap tells every
	// watched inbox to read itself again — the engine's registered
	// listeners, read per call.
	listening func() bool
	deliver   func([]inbox.Movement)
	gap       func(source string)

	// mu guards every stream's state but relaying.
	mu sync.Mutex
}

// newInboxRelay builds a relay with one stream per source, each under a fresh
// epoch.
func newInboxRelay(self string, q relayQueue, roster func(context.Context) ([]string, error),
	sources []string, faulted func(context.Context, ...string) []string,
	listening func() bool, deliver func([]inbox.Movement), gap func(string)) *inboxRelay {

	r := &inboxRelay{self: self, queue: q, roster: roster, sources: slices.Clone(sources),
		streams: make(map[string]*relayStream, len(sources)), faulted: faulted,
		listening: listening, deliver: deliver, gap: gap}
	for _, source := range sources {
		r.streams[source] = &relayStream{source: source, epoch: rand.Text()}
	}
	return r
}

// record numbers one committed batch's movements for this node's followers,
// each source's in its own stream ([relayStream.keep]), and returns the ones
// this node's own sockets hear now: every movement of a source they are not
// being relayed from a peer. A movement of a source this node keeps no stream
// of is no domain the relay judges, and is heard as it is.
//
// moved ITSELF where nothing is withheld, which is every batch while this
// node's copies all serve: a stream that keeps a batch keeps a copy of its
// own, so the caller's slice goes on to the sockets untouched, and a batch
// nobody follows costs no allocation here at all.
func (r *inboxRelay) record(moved []inbox.Movement) []inbox.Movement {
	now := time.Now()
	withheld := false
	r.mu.Lock()
	for _, source := range r.sources {
		n := 0
		for _, m := range moved {
			if m.Source == source {
				n++
			}
		}
		if n == 0 {
			continue
		}
		s := r.streams[source]
		s.keep(now, moved, n)
		withheld = withheld || s.relaying.Load()
	}
	r.mu.Unlock()
	if !withheld {
		return moved
	}
	heard := make([]inbox.Movement, 0, len(moved))
	for _, m := range moved {
		if s := r.streams[m.Source]; s == nil || !s.relaying.Load() {
			heard = append(heard, m)
		}
	}
	return heard
}

// keep numbers the next batch of this stream's source — the n movements of
// moved that name it — keeps it while a follower that has not taken it may
// still ask for it, within [inboxRelayMoves], and wakes every ask waiting for
// one. Called under [inboxRelay.mu].
func (s *relayStream) keep(now time.Time, moved []inbox.Movement, n int) {
	s.seq++
	s.expire(now)
	if s.waiting > 0 {
		// AN ASK IS WAITING FOR THIS VERY BATCH, and the waiter it wakes
		// takes it at once: kept a round trip all the same, so a waiter
		// the scheduler is slow to run still finds it, and so does any
		// other follower between two of its asks.
		s.hold(now)
	}
	switch {
	case !s.heldUntil.After(now):
		// NOBODY CAN ASK FOR IT: no follower was answered within a round
		// trip and none is waiting — the normal state, with no copy of
		// the source out of service anywhere. The ring is empty already:
		// every batch in it was held no longer than this one would be.
	case n > inboxRelayMoves:
		// WIDER THAN THE RING MAY HOLD. Every follower that has not taken
		// it is answered a gap and re-reads, so nothing before it is of
		// use to anybody either.
		s.drop()
	default:
		// A COPY OF ITS OWN, exactly as wide as the batch, since the
		// caller's slice goes on to the sockets.
		batch := make([]inbox.Movement, 0, n)
		for _, m := range moved {
			if m.Source == s.source {
				batch = append(batch, m)
			}
		}
		s.ring = append(s.ring, heldBatch{
			relayedBatch: relayedBatch{Seq: s.seq, Moved: batch}, until: s.heldUntil})
		s.moves += n
		over := 0
		for s.moves > inboxRelayMoves {
			s.moves -= len(s.ring[over].Moved)
			over++
		}
		s.trim(over)
	}
	if s.moved != nil {
		close(s.moved)
		s.moved = nil
	}
}

// hold keeps what this stream records from now on for one more round trip.
func (s *relayStream) hold(now time.Time) {
	if until := now.Add(inboxRelayHold); until.After(s.heldUntil) {
		s.heldUntil = until
	}
}

// expire lets go of every batch no follower can still ask for — the oldest
// first, since heldUntil never moves back.
func (s *relayStream) expire(now time.Time) {
	gone := 0
	for gone < len(s.ring) && !s.ring[gone].until.After(now) {
		s.moves -= len(s.ring[gone].Moved)
		gone++
	}
	s.trim(gone)
}

// trim lets go of the ring's n oldest batches: ZEROED before the slice moves
// past them, so the array the ring goes on using keeps none of their
// movements alive, and the array let go of with the last batch, so an empty
// ring holds none.
func (s *relayStream) trim(n int) {
	if n == 0 {
		return
	}
	clear(s.ring[:n])
	s.ring = s.ring[n:]
	if len(s.ring) == 0 {
		s.ring = nil
	}
}

// drop lets go of every batch.
func (s *relayStream) drop() {
	s.ring, s.moves = nil, 0
}

// reaches reports whether the ring holds every batch after the follower's
// last, after — one at or behind seq.
func (s *relayStream) reaches(after uint64) bool {
	return after+uint64(len(s.ring)) >= s.seq
}

// answer is what this node answers one follower's ask, holding it open up to
// wait for a batch of its source it does not hold yet.
func (r *inboxRelay) answer(ctx context.Context, ask relayAsk, wait time.Duration) relayAnswer {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		out, waitOn := r.answerNow(ctx, ask)
		if waitOn == nil {
			return out
		}
		if !r.await(ctx, r.streams[ask.Source], waitOn, deadline.C) {
			if ctx.Err() == nil {
				out, _ = r.answerNow(ctx, ask)
			}
			return out
		}
	}
}

// await waits for the stream's next batch, the deadline or ctx, COUNTED among
// the stream's waiting asks while it does ([relayStream.keep] keeps a batch
// recorded meanwhile, whenever the ask's last answer was), and reports
// whether a batch woke it. Counted only once [inboxRelay.answerNow] has
// handed it the channel, and nothing falls between: that answer held what is
// recorded for a round trip anyway.
func (r *inboxRelay) await(ctx context.Context, s *relayStream, moved <-chan struct{},
	deadline <-chan time.Time) bool {

	r.mu.Lock()
	s.waiting++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		s.waiting--
		r.mu.Unlock()
	}()
	select {
	case <-moved:
		return true
	case <-deadline:
	case <-ctx.Done():
	}
	return false
}

// answerNow is the answer as this node's stream of the ask's source stands,
// and the channel to wait on where it holds nothing after the follower's last
// batch.
func (r *inboxRelay) answerNow(ctx context.Context, ask relayAsk) (relayAnswer, <-chan struct{}) {
	s := r.streams[ask.Source]
	if s == nil {
		// A SOURCE THIS NODE KEEPS NO STREAM OF — a domain whose
		// movements its appliers do not announce, which a newer peer may
		// follow — is one it cannot answer for.
		return relayAnswer{Source: ask.Source, Unserved: true}, nil
	}
	// ASKED BEFORE THE LOCK: the verdict read may start a read of the
	// broker, which must never sit under the rings' lock. AND OF THIS
	// SOURCE ALONE: a wrong copy of another domain leaves this one's
	// announcements whole.
	faulted := len(r.faulted(ctx, s.source)) > 0
	r.mu.Lock()
	defer r.mu.Unlock()
	if faulted {
		s.outOfService = true
		return relayAnswer{Source: s.source, Unserved: true}, nil
	}
	if s.outOfService {
		// BACK IN SERVICE: what this node's applier announced of the
		// source while its copy was out of service, or did not announce
		// at all, is no sequence a follower can trust to be whole — so it
		// is another epoch, which every follower of it meets as a re-read.
		// And nothing the ring holds is of use to anybody: every batch in
		// it is the old epoch's, which no follower is served again.
		s.outOfService = false
		s.epoch = rand.Text()
		s.drop()
	}
	now := time.Now()
	s.expire(now)
	// THIS FOLLOWER ASKS AGAIN within a round trip of the answer, for
	// whatever is recorded meanwhile — whichever of the cases below it is,
	// since each leaves it placed on this stream.
	s.hold(now)
	out := relayAnswer{Source: s.source, Epoch: s.epoch, Head: s.seq}
	switch {
	case ask.Epoch != s.epoch:
		// A FOLLOWER NEW TO THIS SERVER, or one whose epoch this node no
		// longer serves: where it stands, and nothing it could not place.
		return out, nil
	case ask.After > s.seq || !s.reaches(ask.After):
		// A FOLLOWER THIS RING NO LONGER REACHES BACK TO — the batches
		// after its last expired, were evicted, or were too wide to keep —
		// or one claiming a batch this epoch never numbered: it re-reads
		// and starts again from here.
		out.Gap = true
		return out, nil
	case ask.After < s.seq:
		// THE RING IS CONSECUTIVE, so the follower's next batch is at a
		// known offset from its end.
		held := s.ring[len(s.ring)-int(s.seq-ask.After):]
		out.Batches = make([]relayedBatch, len(held))
		for i, b := range held {
			out.Batches[i] = b.relayedBatch
		}
		return out, nil
	}
	if s.moved == nil {
		s.moved = make(chan struct{})
	}
	return out, s.moved
}

// serve makes this node the answerer of its own relay subject, for every
// source.
//
// ONE REGISTRATION for every source, and ON A REGISTRATION OF ITS OWN, because
// its answers WAIT — up to [inboxRelayWait] for the applier's next
// announcement — and a slot held through a wait is one every other request of
// a shared registration would wait behind. Its bound is the queue those waits
// come from: each follower — one per source a node holds out of service —
// keeps one ask in flight, so [queue.ConcurrentAnswers] is that many such
// followers at once, and one past it waits for a slot inside its own wait
// rather than being refused.
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

// follow runs until ctx ends: one follower per source, each idle while this
// node's own copy of its source serves and following a sound peer's applier
// for it while it does not — see the file head.
func (r *inboxRelay) follow(ctx context.Context) {
	var followers sync.WaitGroup
	for _, source := range r.sources {
		s := r.streams[source]
		followers.Go(func() { r.followSource(ctx, s) })
	}
	followers.Wait()
}

// followSource is one source's follower, until ctx ends.
func (r *inboxRelay) followSource(ctx context.Context, s *relayStream) {
	var (
		server string
		epoch  string
		after  uint64
		// tried is the servers asked since the last one that served, so a
		// follower that lost its server moves through the others before
		// it pauses.
		tried = map[string]bool{}
	)
	switchServer := func() {
		server, epoch, after = "", "", 0
	}
	for ctx.Err() == nil {
		if !r.listening() || len(r.faulted(ctx, s.source)) == 0 {
			if s.relaying.Swap(false) {
				// BACK TO ITS OWN APPLIER, which committed nothing for
				// the batches the relay carried or announces them a
				// second time: either way, read again.
				log.InfoContext(ctx, "inbox_relay_stopped", "source", s.source,
					"server", server,
					"detail", "this node's copy of the source is back in service, or "+
						"nothing on it listens for inbox movements any more, so its "+
						"dashboards hear its own applier again")
				r.gap(s.source)
			}
			switchServer()
			clear(tried)
			if !sleepCtx(ctx, servingRecheck) {
				return
			}
			continue
		}
		if !s.relaying.Swap(true) {
			log.WarnContext(ctx, "inbox_relay_started", "source", s.source,
				"detail", "this node's copy of the source is out of service, so its "+
					"applier announces nothing its dashboards can trust; their inbox "+
					"movements from it are relayed from a data node whose copy of it "+
					"serves")
		}
		if server == "" {
			server = r.pick(ctx, tried)
			if server == "" {
				clear(tried)
				if !sleepCtx(ctx, inboxRelayRetry) {
					return
				}
				continue
			}
			tried[server] = true
		}
		got, ok := r.ask(ctx, server, relayAsk{Source: s.source, Epoch: epoch, After: after})
		switch {
		case !ok || got.Unserved || got.Source != s.source:
			// A SERVER THAT CANNOT ANSWER FOR THIS SOURCE — silent, its
			// own copy of it out of service, or answering for another —
			// is one to move on from.
			switchServer()
			continue
		case got.Epoch != epoch || got.Gap:
			// A SERVER THIS FOLLOWER IS NEW TO, or one it can no longer
			// place itself in: everything from here is relayed, and
			// everything before it is read again.
			epoch, after = got.Epoch, got.Head
			clear(tried)
			r.gap(s.source)
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
				"source", a.Source, "error", fmt.Sprint(err))
		}
		return relayAnswer{}, false
	}
	var got relayAnswer
	if err := json.Unmarshal(replies[0], &got); err != nil {
		log.WarnContext(ctx, "inbox_relay_unreadable", "server", node,
			"source", a.Source, "error", err.Error())
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

// armInboxRelay makes this data node the server of its inbox relay, for every
// source on one subject, and starts the followers that take over its sockets'
// movements of a source whenever its own copy of that domain is out of
// service. A node without `data` serves no API (ingress requires it) and holds
// no applier, so it has nothing to relay and nobody to relay to.
func (e *Engine) armInboxRelay(ctx context.Context) error {
	if e.local == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	relay := newInboxRelay(e.id, e.backends.Queue, e.dataRoster, inboxSources(),
		e.local.outOfService, e.inboxListening, e.deliverInbox, e.inboxGap)
	stop, err := relay.serve(ctx)
	if err != nil {
		return fmt.Errorf("engine: serve the inbox relay: %w", err)
	}
	// THE FOLLOWERS' LIFETIME IS THE ENGINE'S, ended by
	// [Engine.stopInboxRelay]: detached from the context it was armed under,
	// which is the boot's.
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

// stopInboxRelay ends the followers and withdraws this node as a server,
// before the broker closes.
func (e *Engine) stopInboxRelay(ctx context.Context) {
	if e.stopInboxRelayFn == nil {
		return
	}
	e.stopInboxRelayFn(ctx)
	e.stopInboxRelayFn = nil
}
