// Package changefeed turns a committed record into a wake.
//
// # The principle
//
// A wake is derived from a DURABLE, NEVER-COMPACTED record by something that
// outlives the writer — never published by the writer's own goroutine as a
// courtesy. That sentence is ADR-0012.
//
// The engine has both failure modes already: the Mattermost socket path
// swallows a failed publish, and the webhook path is safe only because the
// vendor retries. A native write has no vendor to retry it, so the
// durable record is the retry: the record is committed, and a fleet-wide
// consumer over it is what eventually reaches a seat. A node that dies
// between the write and the publish costs a redelivery, not a lost wake.
//
// # Why the estate is a seam and not a family
//
// This package once spoke the coordination estate's own vocabulary — a
// document family, a bucket key class, a change record — and those types are
// gone with the last projector, so the names below are history rather than
// links. A durable record does not have to live in a bucket: a state
// machine's LOG is the other shape, and a log delivery has no family, no key
// class and no change. Manufacturing a family for one would have been worse
// than the vocabulary it fixed, because a family was also what started a
// projector — the engine would have stood one up over a bucket that did not
// exist.
//
// So the seam is one level up. A [Source] names the estate as a STRING, an
// [Opener] opens one durable consumer over it, and a [Record] is one delivered
// change — and every estate left is a state log's domain, which supplies its
// own opener. Everything below this comment is unchanged by that and is why
// the package exists.
//
// # Why a group and not a duty
//
// Every node pulls from one durable consumer, so a change is handled by
// whichever node gets there first. As a singleton DUTY it would sit behind a
// lease, and a flap on the duty holder would stall the whole company's
// notifications for a lease TTL — for work that is stateless and needs no
// ownership at all.
//
// # A feed outlives its consumer
//
// [Feed.Run] returns only when its context ends. A consumer that fails to
// open, fails a read or closes underneath the feed is stopped and its durable
// group opened again, on a doubling, jittered wait ([reopenBase] to
// [reopenCeiling]), and the reopened group hands it every record the group
// has not acknowledged, so nothing it owed is lost.
//
// THE LOOP IS HERE rather than in whatever starts a feed, because the
// failure it answers is the consumer's and every caller has it: a broker
// that drops a heartbeat (a host that slept, a leader election, a client
// reconnect) ends a read with an error that the next open would not see.
// Before the loop, Run returned on that error, the engine logged it and the
// goroutine exited, so a node that had lost its broker for a second routed
// no wake for hours, until the process restarted. A supervisor in the engine
// would have to restate which outcomes end a feed and which do not, which
// this package already decides, and every other caller would be one more
// place to forget it.
//
// AND A FEED THAT IS DOWN SAYS SO ([Feed.Outage]). A feed that cannot reach
// its consumer publishes no wake, so `wake_latency`, which judges the wakes
// that were published, sees nothing at all; the engine reads each feed's
// outage into the alarm table instead (`feed_stopped`).
//
// # Two independent dedupe layers, and why both
//
// A feed message can be redelivered (a node that died after publishing but
// before acking), and a publish can be repeated. They need different guards:
//
//   - A CLAIM on the change id collapses a redelivered feed message before
//     anything is published. It FAILS OPEN, exactly as the webhook edge does:
//     a coordination store that cannot be reached must not silently stop the
//     company's notifications, so an unknown claim publishes and relies on
//     the second layer.
//   - A DETERMINISTIC WAKE ID, derived from the change and the recipient,
//     lets the inbox's own same-id dedupe and the fleet completion ledger
//     collapse the duplicate that the open claim let through. Without it the
//     ids would be random and neither dedupe could recognise the pair.
//
// # A record this node cannot read is handed back, never acknowledged
//
// The group is the fleet's, so an ACK IS THE FLEET'S: a record one node acks
// is a record no node ever sees again. A node that cannot read a record — a
// newer build's version or envelope, a frame sealed under a key it has not
// been given, or a record its translator fails on — has decided nothing about
// it, and acking it lost the wake a peer would have derived (#838). So such a
// record is handed back for redelivery with a backoff ([DeferDelay]), counted
// by why ([DeferReason]), and a roll that finishes within the hour loses no
// wake.
//
// THE BROKER CAPS NO REDELIVERY, so the feed bounds it: a record handed back
// for [DeadLetterAge] is kept whole as a dead letter in the queue's
// dead-letter stream ([DeadLetters]) — its sealed frame the entry's body, and
// what the feed recorded of it the entry's header ([DeadLetter]), which every
// listing reads without the frame — and only then acked, so it stops holding
// a slot of the consumer's pending set without anybody's wake being lost.
// The entry is kept under the record's IDENTITY, the message the broker stored
// for it — its log, its position there and the broker's instant for it, which
// every node reads off the delivery whether or not it can open the record
// ([keepID]) — which makes a second keep of the record harmless: a keep waits
// as long as the frame takes to cross the node's link, which can outlast the
// delivery's thirty-second acknowledgement window, and the record is then
// handed to another node, which keeps it too. Whichever of the two keeps
// lands first is stored — the slow first keeper's as often as the peer's —
// and the queue answers the other that the entry was already kept, so both
// nodes ack one entry. The record's dead-lettering is COUNTED ONCE, by
// whichever node takes its dead-letter claim ([Feed.deadLetter]), never by
// which keep was answered stored: a keep can store its entry and still answer
// an error, its answer lost on the way back. Nothing the keeping node sends
// while its keep crosses could stop the hand-back: it crosses the same link
// behind the keep's own bytes. An entry is kept for the event stream's retention and then expires;
// the first node whose feed starts able to read it replays it ([Feed.replay])
// through the same claim a delivery passes — the one reader of its frame — and
// drops it; and an operator who has decided its wake can be lost discards it
// (the engine's operator surface, `crewlet feeds discard`). ADR-0012's
// amendment records the decision.
package changefeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("changefeed")

// ClaimTTL is how long a handled change stays claimed.
//
// Five minutes, matching the webhook edge's delivery dedupe and sized for the
// same thing: a redelivery after a node died mid-handle, and an operator's
// replay. It is deliberately NOT the log's retention, because the claim
// answers "did somebody already publish this wake", which stops mattering as
// soon as no consumer could still be holding the message.
const ClaimTTL = 5 * time.Minute

// nakDelay is how long a failed handle waits before coming back.
//
// Two seconds: long enough that a broker reconnect or a leader election
// completes inside one delay rather than being retried through, short enough
// that a person waiting on a notification does not notice.
//
// THE BROKER CAPS NO RETRY: a domain feed's consumer sets `MaxDeliver: -1`
// deliberately (internal/queue/jetstream's domainconsumer.go), since a budget
// that ran out would drop a wake silently. So the FEED bounds what it hands
// back. A publish that failed — the broker, a moment ago — comes back every
// two seconds until it lands. A record this node has decided nothing about —
// one DEFERRED to a peer, or one no build here can translate — backs off from
// this delay to [DeferCap] instead ([DeferDelay]), and once it is
// [DeadLetterAge] old the feed keeps it as a dead letter and lets it go.
const nakDelay = 2 * time.Second

// The reopen schedule: how long a feed waits before each attempt to bring its
// consumer back.
const (
	// reopenBase is the first wait. One second, the interval the broker
	// connection's own reconnect attempts are spaced by, so a feed whose
	// read failed across a client reconnect does not try again before the
	// client has had one attempt at its broker.
	reopenBase = time.Second

	// reopenCeiling is the longest wait, and how long a consumer must stay
	// open for its failure to begin a new series at [reopenBase]. It is
	// [statelog.FeedReopenCeiling], half the grace `feed_stopped` fires
	// at, declared there because the alarm's remedy states it.
	reopenCeiling = statelog.FeedReopenCeiling

	// reopenJitter spreads each wait by a fifth either way, the spread the
	// engine's other retry schedules use, so the feeds a broker outage
	// stopped together do not all ask it again in the same second.
	reopenJitter = 0.2
)

// Source identifies the estate a feed reads.
//
// A STRING, because every estate this reads is now a LOG: the document
// families it was written against are gone, and a log delivery has no family,
// no key class and no change record to name one with.
type Source struct {
	// Name is the notification source a parser registers under, and the
	// scope of this estate's claim keys — "work", "page", "chat".
	Name string

	// Group is the durable consumer's name, and it is STABLE for the
	// deployment's life: the name is where the fleet is. A rename is a
	// fresh consumer, and a fresh one starts from the log's first retained
	// record, so it redelivers every record the log still holds as a wake —
	// only the two dedupe layers in the package doc hold those back, and
	// only within their own retention windows — and until it catches up its
	// acknowledgement floor holds back the log's trim.
	//
	// On the broker the consumer is not literally named this: its durable
	// name is derived from the group AND the stream it reads, so one group
	// on two streams is two consumers.
	//
	// EACH ESTATE DECLARES ITS OWN, as a constant beside the translator
	// that reads it. The helper that used to derive one from a document
	// family is gone with the families — and what it was for survives as
	// a rule rather than a function: a name is chosen once and never
	// improved.
	//
	// A state log's DOMAIN declares the same name
	// ([statelog.Domain.FeedGroup]), because the log's trim waits on this
	// consumer from nodes that build no translator — and the engine
	// refuses to start a feed whose group is not the one its domain
	// declares.
	Group string
}

// Opener opens one durable consumer over an estate.
//
// DECLARED HERE, by the consumer, and satisfied by each state log's own
// domain. The group name is passed rather than held so there is exactly one
// place it comes from — the translator's own [Source].
type Opener interface {
	Open(ctx context.Context, group string) (Records, error)
}

// Records is an open durable consumer.
type Records interface {
	// Next blocks for the next message until the context ends.
	//
	// A nil Message with a nil error means the consumer has closed, which
	// is how a caller tells a shutdown from a failure. A feed whose
	// context is still live reads that close as a failure and opens the
	// group again, because a consumer closed by anything but its own
	// shutdown is one the feed still has to read.
	Next(ctx context.Context) (*Message, error)

	// Reopen reads frame — the bytes a delivery of this consumer's log once
	// carried ([Message.Frame]), which a dead letter keeps as its body —
	// exactly as Next reads a delivery now, against this node's keyring and
	// this build: at is where the frame was delivered from, which the frame
	// does not carry (its position, stream, subject, stored instant and
	// delivery count, and its generation where the node that kept it had
	// read it), and the answer is the Message Next would hand over for it,
	// Deferred where this node still cannot read it.
	//
	// ITS Ack AND Nak ARE NIL: a dead letter is no delivery, and the feed
	// settles one by dropping it or leaving it ([Feed.replay]). A frame
	// this fleet did not write answers [ErrForeignFrame].
	Reopen(frame []byte, at Record) (*Message, error)

	// Stop ends the consumer. The DURABLE POSITION survives, which is what
	// makes a restart resume rather than replay: it is the fleet's
	// position, not this process's.
	Stop() error
}

// Message is one delivered record, with the settlement the estate expects.
type Message struct {
	Record

	// Ack marks the record handled. Called after whatever the handler
	// produced is itself durable, never before.
	Ack func() error

	// Nak returns the record for redelivery after a delay. A handler that
	// could not reach something it needs naks; one that decided the record
	// means nothing to it ACKS, because a decision is handling.
	Nak func(delay time.Duration) error

	// Deferred is set by an opener that met a record this node cannot open
	// and a peer can — a frame sealed under a key it does not hold, an
	// envelope a newer build writes — and carries no readable payload then.
	// The feed defers it exactly as a translator's [Deferral] (#838); nil for
	// every record the opener could read.
	Deferred *Deferral

	// Frame is the delivery's own bytes, exactly as the broker handed them
	// over — for a state log's record the sealed frame, before anything was
	// verified or decoded — on every message, a deferred one included.
	//
	// WHAT A DEAD LETTER KEEPS, as its entry's body beside the header the
	// feed writes of it ([DeadLetter]), so a replay reads the record again
	// as this delivery was read rather than trusting what this node made of
	// it. A message without one cannot be dead-lettered — an entry with no
	// frame is a wake no node could replay — so past [DeadLetterAge] it goes
	// on being handed back, never acked bare.
	Frame []byte
}

// Record is one delivered change, whatever log it came from.
type Record struct {
	// ID is the estate's own identity for this delivery: the log record's
	// operation id. Stable across redeliveries, which is what lets a
	// translator that has no id of its own use it as one.
	ID string

	// Position is the delivery's place in its log — the record's sequence.
	//
	// # Why the three fields below travel with it
	//
	// A position alone is only comparable against the same stream and the
	// same generation. The node that WINS a message is rarely the node
	// that runs the woken seat, and a reanchor renumbers a log — so a
	// wake stamped with a bare position is a number the woken node cannot
	// safely compare with its own.
	Position uint64

	// Stream is the log's stream name.
	Stream string

	// Gen is the log's generation.
	Gen uint64

	// Key is the subject this delivery arrived on. For diagnosis, and for
	// a translator that parses it.
	Key string

	// StoredAt is the broker's own instant for the record — when the change
	// that may wake somebody was stored, which every node reads alike — and
	// what the wake it becomes is timed from ([Options.Objectives]). Zero
	// from an estate that cannot say, which times nothing.
	StoredAt time.Time

	// Delivered is how many times the broker has delivered this record, this
	// delivery included — one the first time — which a deferral's backoff
	// grows with ([DeferDelay]). Zero from an estate that cannot say, which
	// waits the first step.
	Delivered uint64

	// Payload is the record's bytes, verbatim.
	Payload []byte
}

// Publisher is the queue surface this package publishes wakes through.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// DeadLetters is where a feed keeps a record it stopped handing back, and
// where its start finds them again: the queue contract's dead-letter verbs
// ([queue.EventQueue.KeepDeadLetter]), which every backend implements and the
// conformance suite certifies, narrowed to what this package calls — a keep,
// the listing of a subject's headers, the read of one entry's frame and a
// drop.
//
// THE QUEUE'S, rather than a store of this package's own, because a dead
// letter is the broker's in every other part of the engine — kept by age in
// one place under one retention, `CREWLET_DLQ` — and the feed's records are no
// different: kept there, an entry outlives the node that kept it and is
// replayed by whichever node first starts able to read it.
type DeadLetters interface {
	KeepDeadLetter(ctx context.Context, subject, id string, header, frame []byte) (bool, error)
	DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error)
	DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error)
	DropDeadLetter(ctx context.Context, subject string, seq uint64) error
}

// Claims is the dedupe surface, narrowed to what this package uses.
//
// THE THREE-VALUED ANSWER MATTERS HERE. [coord.Claims.Claim] reports held,
// definitively not held, or an error — and this package treats the error as
// "publish anyway", which is the opposite of what a naive reading suggests.
// See the package doc.
type Claims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
	Release(ctx context.Context, key string) error
}

// Translator turns one record into the delivery a parser will read.
//
// DECLARED HERE, implemented by the package that owns the estate's records.
// This package knows how to run a durable consumer exactly once; it knows
// nothing about what a work item or a page is, and adding that knowledge
// would put two domains into one loop.
type Translator interface {
	// Source is the estate this translator serves.
	Source() Source

	// Translate turns a record into a delivery body, reporting whether it
	// should wake anybody at all.
	//
	// FALSE IS AN ORDINARY OUTCOME and is ACKED rather than naked: a quiet
	// import, a draft, a subject nobody watches. Every one of those is a
	// DECISION, and a decision is handling.
	//
	// A RECORD A NEWER BUILD WROTE IS NOT ONE: this build has no rule for
	// it, so it has decided nothing, and it answers [Defer] — the feed hands
	// the record back for a peer that has the rules (#838).
	Translate(ctx context.Context, rec Record) (Delivery, bool, error)
}

// Delivery is what a translated record becomes on the inbound topic.
type Delivery struct {
	// Body is the payload a parser reads. The whole record travels in it,
	// so the node that wins a feed message routes without reading anything
	// — a copy that had not caught up with its log would otherwise route
	// from a stale head, or block the feed until it had.
	Body map[string]any

	// ID is the record's own id, used for the claim and as the seed of
	// every recipient's deterministic wake id.
	//
	// THE TRANSLATOR'S, not [Record.ID]: the id a wake is deduplicated on
	// is the one the record carries INSIDE it, which the parser stamps and
	// the recipient's inbox compares. Only the estate's owner can read it
	// out of the payload.
	ID string

	// Actor is the handle that made the change, so a parser can decline to
	// wake somebody about their own write.
	Actor string
}

// Feed runs one estate's change feed.
type Feed struct {
	opener     Opener
	publisher  Publisher
	claims     Claims
	translator Translator
	now        func() time.Time

	// metrics is the process's one recorder. Nil records nothing, which
	// is a legal deployment — an embedded engine and every test — and
	// leaves the log line as the only surface, which is where it was.
	metrics *metrics.Recorder

	// judge is the node's objective meter; nil records nothing.
	judge Objectives

	// deadLetters is where a record handed back for [DeadLetterAge] is
	// kept; nil keeps nothing ([Options.DeadLetters]).
	deadLetters DeadLetters

	// replayed is whether this process has replayed the feed's dead
	// letters: at its first open that could list them ([Feed.replay]).
	// Read and written only by Run's goroutine.
	replayed bool

	// reopenBase is the reopen schedule's first wait, [reopenBase] in every
	// feed [New] builds, and it scales the whole schedule: the ceiling stays
	// the same multiple of it. Only a test changes it, so a schedule plays
	// out in milliseconds.
	reopenBase time.Duration

	// mu guards the outage below, which [Feed.Run] writes and
	// [Feed.Outage] reads from the alarm evaluation's goroutine.
	mu sync.Mutex

	// running is whether Run is consuming or trying to, open whether it
	// holds a consumer now, and held whether it has held one since Run
	// began, which tells its first open from a reopen.
	running, open, held bool

	// down is when the feed last stopped holding a consumer, or when Run
	// began for a feed that has never held one. failures counts the
	// failed attempts since, and cause is what the latest said.
	down     time.Time
	failures int
	cause    string

	// series counts the attempts the schedule has waited for, which sets
	// the next wait. It OUTLIVES ONE OUTAGE on purpose: a consumer that
	// opens and fails again at once is a flapping broker, which the
	// doubling exists to back off from, so only a consumer that stayed
	// open for the ceiling begins a new series.
	series int
}

// Outage is a feed that is not consuming.
type Outage struct {
	// Source is the estate the feed reads ([Source.Name]).
	Source string

	// Since is when the feed last stopped holding a consumer, or when it
	// began, for a feed that has never opened one.
	Since time.Time

	// Failures counts the failed attempts since: the read that ended the
	// consumer, and each open that failed after it. Zero for a feed whose
	// first open has not answered yet.
	Failures int

	// Cause is what the latest failure said, empty when none has failed.
	Cause string
}

// Objectives is the node's service objective meter, as the feed records into
// it: one WAKE LATENCY event for every wake this node publishes, timed from
// the broker's instant for the record that wakes somebody to the wake on the
// inbound edge.
//
// HERE, because every domain that wakes anybody wakes them through this feed,
// so one recording site times the tracker's, the knowledge base's and chat's
// alike — and the wake is derived from the durable record, so from its storing
// to the inbound edge is the engine's whole share of a wake before the
// notification service routes it. ONE EVENT PER PUBLISHED WAKE: a record that
// wakes nobody is a decision and no wake; a record another node already
// claimed was that node's event; and a publish that failed is no event of its
// own, since the redelivery that publishes it is timed from the same stored
// instant and so carries the delay. Satisfied by [*objectives.Meter].
type Objectives interface {
	RecordSince(n objectives.Name, stamp time.Time, answered bool)
}

// Options configure a feed.
type Options struct {
	Opener     Opener
	Publisher  Publisher
	Claims     Claims
	Translator Translator

	// Metrics is where a record handed back, dead-lettered or left
	// untranslated is counted. Optional.
	Metrics *metrics.Recorder

	// Objectives is where every published wake is timed ([Objectives]).
	// Optional.
	Objectives Objectives

	// DeadLetters is where a record handed back for [DeadLetterAge] is kept
	// when the feed gives up on it, and what the feed's start replays
	// ([Feed.replay]). The engine hands every feed its queue.
	//
	// NIL KEEPS NOTHING, and so ACKS NOTHING it gave up on: such a record
	// goes on being handed back with the backoff, holding its slot in the
	// consumer, because an ack with no entry kept is the lost wake this
	// package exists to prevent. A feed that never meets a record past the
	// age — an embedded engine, most tests — needs none.
	DeadLetters DeadLetters

	// Now is the clock the claim is taken on, a record's age is read on and
	// a dead letter is stamped by. Nil takes the wall clock.
	Now func() time.Time
}

// New builds a feed.
func New(opts Options) (*Feed, error) {
	switch {
	case opts.Opener == nil:
		return nil, errors.New("changefeed: an opener is required")
	case opts.Publisher == nil:
		return nil, errors.New("changefeed: a publisher is required")
	case opts.Translator == nil:
		return nil, errors.New("changefeed: a translator is required")
	}
	src := opts.Translator.Source()
	switch {
	case src.Name == "":
		return nil, errors.New("changefeed: the translator's source has no name, " +
			"so its claims would share a key space with every other estate")
	case src.Group == "":
		return nil, errors.New("changefeed: the translator's source has no group, " +
			"and the group name IS the fleet's position")
	}
	f := &Feed{
		opener: opts.Opener, publisher: opts.Publisher,
		claims: opts.Claims, translator: opts.Translator, now: opts.Now,
		metrics: opts.Metrics, judge: opts.Objectives, deadLetters: opts.DeadLetters,
		reopenBase: reopenBase,
	}
	if f.now == nil {
		f.now = func() time.Time { return time.Now().UTC() }
	}
	return f, nil
}

// Run consumes the feed until the context ends, and returns only then. A feed
// runs once: Run is not called twice on one feed.
//
// A consumer that cannot be opened, fails a read or closes while the context
// is live is stopped, and its group is opened again on the reopen schedule
// (see the package doc). SETTLEMENT IS UNTOUCHED BY IT: a reopen happens
// between two messages and never inside [Feed.handle], so every message a
// consumer handed over was acked or naked before that consumer stopped, and
// the group hands whatever it delivered and nobody settled to the next
// consumer that reads it, on this node or another.
func (f *Feed) Run(ctx context.Context) {
	src := f.translator.Source()
	f.begin()
	defer f.end()
	for {
		opened, err := f.consume(ctx, src)
		if ctx.Err() != nil {
			return
		}
		down, series := f.failed(err, opened)
		wait := f.wait(series)
		if opened.IsZero() {
			log.WarnContext(ctx, "changefeed_open_failed", "source", src.Name,
				"group", src.Group, "attempt", series, "retry_in", wait.String(),
				"down_for", f.now().Sub(down).String(), "error", err.Error(),
				"detail", "this node publishes no wake from this feed until it "+
					"opens, and tries again on its own; `feed_stopped` is raised "+
					"once it has been down for the stall grace")
		} else {
			log.WarnContext(ctx, "changefeed_interrupted", "source", src.Name,
				"group", src.Group, "attempt", series, "retry_in", wait.String(),
				"error", err.Error(),
				"detail", "the feed opens its group again on its own and is handed "+
					"every record the group has not acknowledged, so no wake is lost; "+
					"until then this node publishes none from it, and any other node "+
					"consuming the group still does")
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// consume opens the feed's group and handles what it delivers until the
// consumer ends, returning when it opened (zero when the open failed) and
// why it ended. A nil error is a consumer that ended with the context.
func (f *Feed) consume(ctx context.Context, src Source) (time.Time, error) {
	records, err := f.opener.Open(ctx, src.Group)
	if err != nil {
		return time.Time{}, fmt.Errorf("changefeed: open the %s feed: %w", src.Name, err)
	}
	defer func() { _ = records.Stop() }()

	opened := f.now()
	if was, first := f.opened(); first {
		log.InfoContext(ctx, "changefeed_started", "source", src.Name, "group", src.Group)
	} else {
		log.InfoContext(ctx, "changefeed_reopened", "source", src.Name,
			"group", src.Group, "down_for", opened.Sub(was.Since).String(),
			"failures", was.Failures, "was", was.Cause)
	}
	// ONCE A PROCESS, before the first delivery: see [Feed.replay]. A
	// listing that failed is asked again at the next open.
	if !f.replayed {
		f.replayed = f.replay(ctx, src, records)
	}
	for {
		msg, err := records.Next(ctx)
		if err != nil {
			return opened, fmt.Errorf("changefeed: read the %s feed: %w", src.Name, err)
		}
		if msg == nil {
			if ctx.Err() != nil {
				return opened, nil
			}
			return opened, fmt.Errorf("changefeed: the %s feed's consumer closed "+
				"while the feed was running", src.Name)
		}
		f.handle(ctx, msg)
	}
}

// Outage reports whether the feed is down, and since when: from the moment it
// stopped holding a consumer, or began without one, until it holds one again.
// False while it holds one, and before Run starts or after it returns.
func (f *Feed) Outage() (Outage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running || f.open {
		return Outage{}, false
	}
	return Outage{Source: f.translator.Source().Name, Since: f.down,
		Failures: f.failures, Cause: f.cause}, true
}

// begin marks the feed running without a consumer, from now.
func (f *Feed) begin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running, f.open, f.held = true, false, false
	f.down, f.failures, f.cause, f.series = f.now(), 0, "", 0
}

// end marks the feed stopped, which is no outage: nothing is meant to be
// consuming.
func (f *Feed) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running, f.open = false, false
}

// opened marks the feed holding a consumer, and reports the outage it ends
// and whether this is the feed's first consumer.
func (f *Feed) opened() (Outage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := Outage{Since: f.down, Failures: f.failures, Cause: f.cause}
	first := !f.held
	f.open, f.held, f.failures, f.cause = true, true, 0, ""
	return was, first
}

// failed records one failed attempt, given when its consumer opened (zero for
// an open that failed), beginning the outage if the feed held a consumer
// until now. It returns when the outage began and the attempt's place in its
// series.
func (f *Feed) failed(err error, opened time.Time) (time.Time, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if !opened.IsZero() && now.Sub(opened) >= f.ceiling() {
		f.series = 0
	}
	f.series++
	if f.open {
		f.open, f.down, f.failures = false, now, 0
	}
	f.failures++
	f.cause = err.Error()
	return f.down, f.series
}

// wait is how long the feed waits before attempt n of a series to bring its
// consumer back, counting from one: [reopenBase] doubling to the ceiling,
// spread by [reopenJitter].
func (f *Feed) wait(n int) time.Duration {
	return backoff.Jitter(backoff.Doubling(n, f.reopenBase, f.ceiling()), reopenJitter)
}

// ceiling is the schedule's longest wait before its spread: [reopenCeiling]
// in every feed [New] builds.
func (f *Feed) ceiling() time.Duration {
	return f.reopenBase * (reopenCeiling / reopenBase)
}

// sleep waits d, and reports whether the context is still live: a feed told
// to stop while it waits stops at once rather than after the wait.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// handle processes one message, settling it exactly once.
func (f *Feed) handle(ctx context.Context, msg *Message) {
	// A RECORD A PEER CAN READ AND THIS NODE CANNOT — met by the opener, or
	// by the translator's future-version arm below — is DEFERRED to that
	// peer, never acked (#838).
	if msg.Deferred != nil {
		f.deferRecord(ctx, msg, msg.Deferred)
		return
	}
	switch out, deferral, err := f.derive(ctx, msg.Record, false); out {
	case outcomeDeferred:
		f.deferRecord(ctx, msg, deferral)
	case outcomeUntranslatable:
		// A translation failure is a RECORD THIS BUILD CANNOT READ, and a
		// redelivery will not make it readable here. It is handed back
		// all the same, because the alternative — acking — would drop a
		// wake permanently on a build that is about to be upgraded past
		// the problem, and a peer on that build is somewhere in the group.
		//
		// WITH THE DEFERRAL'S BACKOFF AND ITS BOUND: the broker caps no
		// redelivery of a domain feed's consumer (`MaxDeliver: -1`, a
		// budget that ran out would drop a wake silently), so the feed
		// does — handed back from two seconds to thirty, and dead-lettered
		// once it is [DeadLetterAge] old, so it stops holding a slot in
		// the consumer without anybody's wake being lost. Its only other
		// symptom is a wake that never comes, so it is counted, and the
		// count is what `feed_unreadable` fires on.
		f.count(metrics.TrackerFeedUnreadable)
		log.WarnContext(ctx, "changefeed_untranslatable", "source", f.translator.Source().Name,
			"key", msg.Key, "position", msg.Position, "stream", msg.Stream, "generation", msg.Gen,
			"delivered", msg.Delivered, "error", err.Error(),
			"detail", "returned for redelivery with a backoff, and dead-lettered once it "+
				"is an hour old: no build on this node can read it, and a node that "+
				"can derives its wake")
		f.holdBack(ctx, msg, &Deferral{Reason: DeferUntranslatable, Err: err})
	case outcomeUnpublished:
		// THE BROKER, A MOMENT AGO, with the claim already released (see
		// [Feed.derive]): back at the feed's ordinary delay.
		f.nak(ctx, msg)
	default:
		f.ack(ctx, msg)
	}
}

// outcome is what the feed made of a record it could open.
type outcome int

const (
	// outcomeHandled is a record settled for good: a decision that wakes
	// nobody, or a wake published here.
	outcomeHandled outcome = iota
	// outcomeClaimed is a wake whose claim another holder has: published
	// already, or being published now. A delivery is settled by it, as
	// handled; a replay is not ([Feed.replayOne]).
	outcomeClaimed
	// outcomeDeferred is the translator's [Deferral]: a peer may read it.
	outcomeDeferred
	// outcomeUntranslatable is the translator's failure: no build on this
	// node can read it.
	outcomeUntranslatable
	// outcomeUnpublished is a wake the broker did not take, its claim
	// released so a retry is not taken for a duplicate.
	outcomeUnpublished
)

// derive is everything a record this node could open becomes: translated, and
// the wake it derives published through the claim.
//
// ONE PATH FOR A DELIVERY ([Feed.handle]) AND A DEAD LETTER'S REPLAY
// ([Feed.replay]), so a replayed wake passes the dedupe a delivered one does:
// two nodes starting together and replaying the same entry publish it once.
//
// replayed says which, and the wake says it too (types.RawWebhook.Replayed):
// a replay publishes the wakes a roll piled up one after another, a burst the
// live path would have spread over the roll, so the notification service paces
// each by the seat's rate valve rather than skip one the valve refuses — the
// replay drops an entry once its wake is published, so a skipped wake was lost
// with its entry.
func (f *Feed) derive(ctx context.Context, rec Record, replayed bool) (outcome, *Deferral, error) {
	body, wake, err := f.translator.Translate(ctx, rec)
	if d, ok := deferralOf(err); ok {
		return outcomeDeferred, d, nil
	}
	if err != nil {
		return outcomeUntranslatable, nil, err
	}
	if !wake {
		// A DECISION IS HANDLING. A quiet import, a draft, an actor-only
		// change: settled, because handing it back would circle it to the
		// dead-letter path for having been handled correctly.
		return outcomeHandled, nil, nil
	}
	if !f.claim(ctx, body.ID) {
		log.DebugContext(ctx, "changefeed_already_handled", "change", body.ID)
		return outcomeClaimed, nil, nil
	}
	ev := events.New(types.RawWebhook{
		Body: body.Body, Handle: body.Actor, Trigger: triggerOf(rec), FirstParty: true,
		Replayed: replayed,
	}, events.NewTrace())
	ev.Source = f.translator.Source().Name
	if err := f.publisher.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
		// THE CLAIM IS RELEASED BEFORE THE RECORD IS HANDED BACK. A claim
		// held over a delivery that never published would make the
		// redelivery skip it, which is a wake lost to a broker hiccup —
		// the exact failure the durable record exists to prevent.
		f.release(ctx, body.ID)
		log.WarnContext(ctx, "changefeed_publish_failed", "change", body.ID,
			"error", err.Error())
		return outcomeUnpublished, nil, err
	}
	if f.judge != nil {
		// FROM THE RECORD'S OWN STORED INSTANT, a replayed wake's
		// included: the hour it spent dead-lettered is a delay the wake
		// carried, which is what the objective measures.
		f.judge.RecordSince(objectives.WakeLatency, rec.StoredAt, true)
	}
	return outcomeHandled, nil, nil
}

// deferRecord hands back a record a peer can read and this node cannot (#838),
// counted apart from a record no build here can translate.
//
// SAID AT INFO ONCE AND AT DEBUG AFTER: a roll's ordinary traffic is a record
// handed back a few times until a node on the newer build takes it, and the
// broker's delivery count is the group's, so the one node that met it first
// says so and every redelivery after it is quiet.
func (f *Feed) deferRecord(ctx context.Context, msg *Message, d *Deferral) {
	if !f.holdBack(ctx, msg, d) {
		// PAST THE AGE: the dead-letter path settled it, or handed it back
		// for a keep that failed, and said which — no hand-back to count.
		return
	}
	f.countReason(metrics.TrackerFeedDeferred, d.Reason)
	level := slog.LevelDebug
	if msg.Delivered <= 1 {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "changefeed_record_deferred", "source", f.translator.Source().Name,
		"reason", string(d.Reason), "key", msg.Key, "position", msg.Position,
		"stream", msg.Stream, "delivered", msg.Delivered,
		"retry_in", DeferDelay(msg.Delivered).String(), "error", errText(d.Err),
		"detail", "handed back for a peer that can read it — a node on a newer build, or "+
			"one holding the key — with a backoff, and dead-lettered once it is an hour "+
			"old: a roll that finishes inside the hour loses no wake")
}

// holdBack returns a record this node has decided nothing about for
// redelivery, with the backoff ([DeferDelay]) — or, once it is [DeadLetterAge]
// old, dead-letters it ([Feed.deadLetter]). NEVER ACKED BARE: an ack is the
// fleet's, and a record acked with nothing kept is a wake no node derives.
//
// It reports whether it handed the record back with the backoff: false for a
// record past the age, which the dead-letter path settles — or hands back
// itself, after a keep that failed — and logs.
func (f *Feed) holdBack(ctx context.Context, msg *Message, d *Deferral) bool {
	if age, dated := f.ageOf(msg.Record); dated && age >= DeadLetterAge {
		f.deadLetter(ctx, msg, d, age)
		return false
	}
	f.nakAfter(ctx, msg, DeferDelay(msg.Delivered))
	return true
}

// ageOf is how long ago the broker stored rec, on this feed's clock, and false
// for a record its estate dates nothing for — which is never dead-lettered
// ([DeadLetterAge]).
func (f *Feed) ageOf(rec Record) (time.Duration, bool) {
	if rec.StoredAt.IsZero() {
		return 0, false
	}
	return f.now().Sub(rec.StoredAt), true
}

// errNoDeadLetters is what a feed built with no [Options.DeadLetters] answers
// for a record it would dead-letter.
var errNoDeadLetters = errors.New("changefeed: this feed keeps no dead letters " +
	"(no Options.DeadLetters), so it cannot let the record go")

// deadLetter keeps msg — a record handed back for [DeadLetterAge] — in its
// feed's dead-letter subject ([DeadLetterSubject]) as a self-contained entry,
// and only THEN acks it: an ack before the entry is durable is a wake lost
// between the two.
//
// A KEEP ANSWERED WITH AN ERROR hands the record back with the backoff, and
// claims and counts nothing. The error says the keep was refused or its answer
// lost, never that nothing was stored ([queue.ErrUnavailable]): its entry may
// be stored even so, and the record's next keep is then answered already kept.
//
// A KEEP ANSWERED STORED OR ALREADY KEPT settles the record: either way an
// entry of it was stored under the record's identity ([keepID]) — by this
// keep, a peer's, or this node's own earlier one — and is still held, or has
// been dropped since: an operator discarded it, or a feed start's replay
// derived its wake. The record's dead-lettering is COUNTED ONCE, by whichever
// node takes its dead-letter claim ([Feed.countsDeadLetter]): that node counts
// `crewlet.tracker.feed.dead_lettered` and logs the record's line, which says
// the entry is held only where this keep stored it, and every other keep of
// the record says it was counted already and counts nothing. Never by the answer: of two keeps of one record whichever
// lands first is stored — the slow first keeper's as often as a peer's — and
// the keep that stored the entry may be the one whose answer was lost, which
// counted nothing.
//
// THE ORDER IS COUNT, THEN ACK: the ack is what lets the record go fleet-wide,
// so it comes after everything this node owes the record, and a delivery
// handed back because its ack was lost finds the identity already kept and the
// claim held, and counts nothing again.
func (f *Feed) deadLetter(ctx context.Context, msg *Message, d *Deferral, age time.Duration) {
	src := f.translator.Source()
	subject := DeadLetterSubject(src)
	err := errNoDeadLetters
	already := false
	if f.deadLetters != nil {
		already, err = f.keep(ctx, src, subject, msg, d)
	}
	if err != nil {
		log.WarnContext(ctx, "changefeed_dead_letter_failed", "source", src.Name,
			"reason", string(d.Reason), "key", msg.Key, "position", msg.Position,
			"stream", msg.Stream, "age", age.String(), "error", err.Error(),
			"detail", "handed back again rather than acknowledged: the keep was refused or "+
				"its answer lost, so its entry may be stored or may not, and an ack with "+
				"none stored is a wake no node could ever replay; the record goes on "+
				"circling until a keep is answered, which is answered already kept if this "+
				"one's entry was stored")
		f.nakAfter(ctx, msg, DeferDelay(msg.Delivered))
		return
	}
	if !f.countsDeadLetter(ctx, src, msg.Record) {
		log.InfoContext(ctx, "changefeed_dead_letter_already_kept", "source", src.Name,
			"reason", string(d.Reason), "key", msg.Key, "position", msg.Position,
			"stream", msg.Stream, "age", age.String(), "delivered", msg.Delivered,
			"subject", subject, "stored", !already,
			"detail", "another keep of the record took its dead-letter claim and counted "+
				"and logged it — this node's own earlier keep, or a peer's, whichever "+
				"took the claim first — so this one counts nothing, and the record is "+
				"acknowledged")
		f.ack(ctx, msg)
		return
	}
	f.countReason(metrics.TrackerFeedDeadLettered, d.Reason)
	log.WarnContext(ctx, "changefeed_record_dead_lettered", "source", src.Name,
		"reason", string(d.Reason), "key", msg.Key, "position", msg.Position,
		"stream", msg.Stream, "age", age.String(), "delivered", msg.Delivered,
		"subject", subject, "stored", !already, "error", errText(d.Err),
		"detail", deadLetteredDetail(already))
	f.ack(ctx, msg)
}

// deadLetteredDetail is the detail of a record's dead-lettered line: what the
// counting node knows of the record's entry. A keep answered stored knows the
// entry is held. One answered already kept knows only that an earlier keep
// stored it — a peer's, or this node's own whose answer was lost — and an
// operator may have discarded it since, or a feed start's replay derived its
// wake and dropped it, so its line promises no entry that may be gone.
func deadLetteredDetail(already bool) string {
	if already {
		return "no node read it in the hour it was handed back, and an earlier keep " +
			"stored its entry in CREWLET_DLQ, frame and all: held, it expires after " +
			"stream.event_retention_hours and raises `feed_dead_lettered`, and the " +
			"first node whose feed starts able to read it derives its wake; it may " +
			"be gone already, its wake derived by a feed start's replay or the entry " +
			"discarded by an operator"
	}
	return "no node read it in the hour it was handed back, so it is kept in " +
		"CREWLET_DLQ, frame and all, for stream.event_retention_hours and then " +
		"expires; the first node whose feed starts able to read it derives its " +
		"wake, and `feed_dead_lettered` is raised until then"
}

// countsDeadLetter takes the record's DEAD-LETTER CLAIM and reports whether
// this node is the one that counts the record's dead-lettering: the claim keyed
// on the identity the record's dead letter is kept under
// ([deadLetterClaimKey]), held for the queue's window ([queue.DeadLetterWindow])
// — the span in which another keep of the record is answered already kept
// rather than storing an entry of its own, which is counted again.
//
// IT FAILS OPEN, counting wherever the claim cannot be decided — a store that
// cannot be reached or answers an error, and a feed given no claims at all —
// and says so: a rare double count beats a dead letter nobody counted, which
// no reading of the count could ever notice.
func (f *Feed) countsDeadLetter(ctx context.Context, src Source, rec Record) bool {
	if f.claims == nil {
		return true
	}
	won, err := f.claims.Claim(ctx, deadLetterClaimKey(keepID(src, rec)), queue.DeadLetterWindow, f.now())
	if err != nil {
		log.WarnContext(ctx, "changefeed_dead_letter_claim_unavailable", "source", src.Name,
			"key", rec.Key, "position", rec.Position, "stream", rec.Stream,
			"error", err.Error(),
			"detail", "counted here anyway: a dead letter two nodes count reads one too "+
				"many; one nobody counted is never read at all")
		return true
	}
	return won
}

// deadLetterClaimKey is the claim a record's dead-lettering is counted under
// ([Feed.countsDeadLetter]), for the identity id its dead letter is kept under
// ([keepID]).
//
// IN A NAMESPACE OF ITS OWN: every wake's claim key ([ClaimKey]) holds the
// separator `|` between its source and its id, and this one — a word, a colon
// and a hex digest — holds none, so a record's count and a wake's claim can
// never take each other's key. DIGESTED, so the key is one length whatever
// names the identity quotes.
func deadLetterClaimKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "changefeed-dead-letter:" + hex.EncodeToString(sum[:])
}

// keep writes msg's dead letter under the record's identity ([keepID]): the
// header the feed records of it, and its frame as the entry's body. It answers
// whether the queue had kept the record's entry already.
func (f *Feed) keep(ctx context.Context, src Source, subject string, msg *Message,
	d *Deferral) (bool, error) {

	if len(msg.Frame) == 0 {
		return false, errors.New("changefeed: the delivery carries no frame, so an entry " +
			"could not be replayed")
	}
	header, err := EncodeDeadLetter(DeadLetter{
		Source: src.Name, Group: src.Group,
		Stream: msg.Stream, Seq: msg.Position, Gen: msg.Gen, Key: msg.Key, OpID: msg.ID,
		Reason: d.Reason, Detail: errText(d.Err),
		StoredAt: msg.StoredAt, Delivered: msg.Delivered, DeadLetteredAt: f.now(),
	})
	if err != nil {
		return false, err
	}
	return f.deadLetters.KeepDeadLetter(ctx, subject, keepID(src, msg.Record), header, msg.Frame)
}

// keepID is the identity a record's dead letter is kept under
// ([queue.EventQueue.KeepDeadLetter]): the record's STORED MESSAGE — the feed's
// source, the log it was delivered from, its position there, and the broker's
// own instant for it ([Record.StoredAt]), in nanoseconds since the epoch.
//
// WHAT IS KEPT, NEVER THE DELIVERY, AND NOTHING READ OUT OF THE FRAME: every
// node handed the record reads all four off the delivery itself, whether or
// not it can open the envelope — whichever delivery of it reached them and
// however many times it was delivered before — so a second keep of it, a
// peer's handed the record while a keep of it was still crossing a link, or
// this node's own after an answer that was lost, is answered already kept
// rather than kept twice. The instant is what tells one incarnation of a log
// from the next: a reanchored or rebuilt log numbers its records again, and
// the record at a reused position was stored at another instant. The log's
// generation is no substitute: a stream deleted and remade, or restored from
// an older copy and written past, keeps its generation, which only a reanchor
// moves. It is LEFT OUT besides, because only a node that read the envelope
// knows it: a node that could not would key the same record apart from one
// that could, and both entries would be kept.
//
// NEVER ZERO HERE: the broker stamps every message it stores, and a record its
// estate dates nothing for is never dead-lettered ([Feed.ageOf]). Quoted, so no
// source or stream name can run into another's.
func keepID(src Source, rec Record) string {
	return fmt.Sprintf("%q %q %d %d", src.Name, rec.Stream, rec.Position, rec.StoredAt.UnixNano())
}

// replay re-derives every dead letter this feed's group holds, once a process,
// at the first open whose listing answered, before the first delivery is
// handled. It reports whether the listing answered.
//
// # Why at the start, and only there
//
// What makes an entry readable is a change of THIS NODE: a newer build, or a
// key it did not hold. Both arrive only with a restart — the build obviously,
// and the keyring because a node's [recordseal.Verifier] is built once and
// never changes (it is read at boot, and the rotation runbook restarts every
// node to add a key) — so the start of a feed is the one moment an entry this
// node could not read before may have become one it can, and a replay there
// covers a key's arrival as well as a build's.
//
// # What becomes of each entry
//
// The listing names each entry by its header alone ([DeadLetter]), and the
// replay is the one reader of a FRAME: each entry's is read by its sequence,
// from the broker itself ([DeadLetters]), only when the replay acts on it. A
// record already settled by this replay, under another entry of it, needs no
// frame at all, and an entry gone by the time its frame is asked for — a
// peer's replay or an operator's discard dropped it, or the retention let it
// go — is nothing this replay has to settle. The listing is a copy's, which
// may be a moment behind the broker: an entry kept in that moment is replayed
// by the next start, as one kept a moment after this start is.
//
// Each frame is opened again exactly as a delivery is ([Records.Reopen]): the
// frame verified, the envelope read, the translator asked. A record that wakes
// somebody is published through the same claim a delivery passes
// ([Feed.derive]), and its entry is DROPPED once the wake is published, or once
// the translator decided it wakes nobody — and so is every other entry of the
// same record, kept further apart than the window its broker holds the
// record's identity for ([keepID]). One whose claim a peer holds
// is LEFT for that peer, which drops it once its publish lands: dropped on the
// claim's word, it would be a wake nobody has the moment the peer's publish
// failed. One this node still cannot read (any deferral, a record it cannot
// translate, an entry a newer build wrote) is LEFT for a node that can, and so
// is one whose frame could not be read. A frame this fleet did not write is
// dropped, loudly, since no node will ever act on it. Nothing else this
// package does clears an entry: an operator's discard (internal/engine's
// FeedDeadLetters, through the same [DeadLetters]) and the retention are the
// other two ways one goes.
func (f *Feed) replay(ctx context.Context, src Source, records Records) bool {
	if f.deadLetters == nil {
		return true
	}
	subject := DeadLetterSubject(src)
	kept, err := f.deadLetters.DeadLettersOn(ctx, subject)
	if err != nil {
		log.WarnContext(ctx, "changefeed_dead_letters_unread", "source", src.Name,
			"subject", subject, "error", err.Error(),
			"detail", "the feed's dead letters could not be listed, so none was replayed "+
				"now; they are listed again at the feed's next open, and stay kept until "+
				"a node replays them or their retention lets them go")
		return false
	}
	fates := map[entryFate]int{}
	settled := map[recordPlace]bool{}
	for _, entry := range kept {
		fates[f.replayOne(ctx, src, subject, records, entry, settled)]++
	}
	if len(kept) > 0 {
		log.InfoContext(ctx, "changefeed_dead_letters_replayed", "source", src.Name,
			"subject", subject, "kept", len(kept), "dropped", fates[entryDropped],
			"left", fates[entryLeft], "gone", fates[entryGone])
	}
	return true
}

// entryFate is what one replay made of one listed entry.
type entryFate int

const (
	// entryLeft is an entry the replay left for a node, or a start, that
	// can settle it.
	entryLeft entryFate = iota
	// entryDropped is an entry the replay dropped.
	entryDropped
	// entryGone is an entry gone before the replay read its frame.
	entryGone
)

// recordPlace names one record of a log as its dead letters name it: the
// stream it was delivered from, its sequence there and the broker's instant
// for it, which every entry kept of it carries ([keepID]) and the record a
// rebuilt log stored at a reused sequence does not share.
//
// THE INSTANT IN NANOSECONDS, NEVER A time.Time: a map compares a time.Time's
// location beside its instant, and the instant a header decodes to carries
// another location than the one a delivery handed over. Its header carries
// it as RFC 3339 with nanoseconds ([DeadLetter.StoredAt]), so the nanoseconds
// read back exactly.
type recordPlace struct {
	stream string
	seq    uint64
	stored int64
}

// replayOne re-derives one dead letter and answers what became of its entry.
// settled is every record this replay has settled the wake of, which replayOne
// adds to.
func (f *Feed) replayOne(ctx context.Context, src Source, subject string, records Records,
	entry queue.Kept, settled map[recordPlace]bool) entryFate {

	letter, err := DecodeDeadLetter(entry.Header)
	if err != nil {
		// A NEWER BUILD'S ENTRY, or bytes that are no entry: left for a
		// build that can judge it, never dropped by one that cannot.
		log.WarnContext(ctx, "changefeed_dead_letter_unreadable", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "error", err.Error())
		return entryLeft
	}
	place := recordPlace{stream: letter.Stream, seq: letter.Seq, stored: letter.StoredAt.UnixNano()}
	if settled[place] {
		// THE SAME RECORD KEPT TWICE, under its one identity ([keepID]),
		// by keeps further apart than the window its broker holds the
		// identity for (a restart never leaves two: it rebuilds the window
		// from the entries it still holds, so it forgets only a dropped
		// entry's identity), whose wake this replay has settled already:
		// dropped with nothing derived again, and its frame never read.
		// Asked of the claim instead, it would answer held — by this very
		// replay — and the entry would be left as though a peer held it. A
		// rebuilt log's record at the same sequence was stored at another
		// instant, so it is another place, and derived on its own.
		return f.drop(ctx, src, subject, entry)
	}
	frame, held, err := f.deadLetters.DeadLetterFrame(ctx, subject, entry.Seq)
	switch {
	case err != nil:
		log.WarnContext(ctx, "changefeed_dead_letter_frame_unread", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq, "error", err.Error(),
			"detail", "left, and replayed at the next start: its frame could not be read")
		return entryLeft
	case !held:
		// GONE SINCE THE LISTING — a peer's replay or an operator's discard
		// dropped it, or the retention let it go: whoever took it accounted
		// for it, and there is nothing of it left for this replay to settle.
		log.DebugContext(ctx, "changefeed_dead_letter_gone", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq)
		return entryGone
	}
	msg, err := records.Reopen(frame, letter.at())
	switch {
	case errors.Is(err, ErrForeignFrame):
		log.ErrorContext(ctx, "changefeed_dead_letter_unverified", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq, "error", err.Error(),
			"detail", "dropped, and NO wake is derived from it: a frame this fleet did "+
				"not write can never be acted on, by this node or any other")
		return f.drop(ctx, src, subject, entry)
	case err != nil:
		log.WarnContext(ctx, "changefeed_dead_letter_unopened", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "error", err.Error())
		return entryLeft
	case msg.Deferred != nil:
		log.InfoContext(ctx, "changefeed_dead_letter_still_deferred", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq, "reason", string(msg.Deferred.Reason),
			"detail", "this node cannot read it either; it is left for a node that can")
		return entryLeft
	}
	switch out, deferral, err := f.derive(ctx, msg.Record, true); out {
	case outcomeHandled:
		settled[place] = true
		return f.drop(ctx, src, subject, entry)
	case outcomeClaimed:
		// A PEER HOLDS THE CLAIM ON ITS WAKE — replaying the same entry at
		// the same start — and drops the entry once its publish lands. LEFT,
		// never dropped on the claim's word: a peer whose publish fails
		// releases the claim and leaves the entry for the next start, and an
		// entry dropped here meanwhile would be a wake nobody has. A peer
		// that published and stopped before its drop leaves the entry to the
		// next start, whose second wake the claim collapses inside its
		// window and the deterministic wake id after it.
		log.InfoContext(ctx, "changefeed_dead_letter_claimed_elsewhere", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq,
			"detail", "a peer holds the claim on its wake and drops the entry once its "+
				"publish lands; left for that peer, and replayed at the next start if "+
				"its publish failed")
	case outcomeDeferred:
		log.InfoContext(ctx, "changefeed_dead_letter_still_deferred", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq, "reason", string(deferral.Reason),
			"detail", "this node cannot read it either; it is left for a node that can")
	default:
		// UNTRANSLATABLE HERE TOO, or a wake the broker did not take: left,
		// and replayed by the next feed start that can.
		log.WarnContext(ctx, "changefeed_dead_letter_not_replayed", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "stream", letter.Stream,
			"position", letter.Seq, "error", errText(err))
	}
	return entryLeft
}

// drop removes a replayed entry, answering whether it went. One that did not
// is replayed again at the next feed start: the claim collapses a second wake
// inside its window, and the deterministic wake id after it.
func (f *Feed) drop(ctx context.Context, src Source, subject string, entry queue.Kept) entryFate {
	if err := f.deadLetters.DropDeadLetter(ctx, subject, entry.Seq); err != nil {
		log.WarnContext(ctx, "changefeed_dead_letter_drop_failed", "source", src.Name,
			"subject", subject, "seq", entry.Seq, "error", err.Error())
		return entryLeft
	}
	return entryDropped
}

// errText is err's words, empty for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// claim reports whether this node should publish the wake.
//
// FAILS OPEN, on the webhook edge's rule: a coordination store that cannot be
// reached must not silently stop the company's notifications. The duplicate
// that an open claim lets through is collapsed by the deterministic wake id
// downstream — which is why both layers exist.
func (f *Feed) claim(ctx context.Context, id string) bool {
	if f.claims == nil {
		return true
	}
	won, err := f.claims.Claim(ctx, ClaimKey(f.translator.Source().Name, id), ClaimTTL, f.now())
	if err != nil {
		log.WarnContext(ctx, "changefeed_claim_unavailable", "change", id,
			"error", err.Error(),
			"detail", "publishing anyway; a duplicate is collapsed by the wake "+
				"id, where a swallowed change is a wake nobody is ever told about")
		return true
	}
	return won
}

func (f *Feed) release(ctx context.Context, id string) {
	if f.claims == nil {
		return
	}
	if err := f.claims.Release(ctx, ClaimKey(f.translator.Source().Name, id)); err != nil {
		log.WarnContext(ctx, "changefeed_claim_release_failed", "change", id,
			"error", err.Error())
	}
}

// triggerOf is where the record a delivery was derived from was committed, as
// the token a wake carries ([types.RawWebhook.Trigger]) — or nothing, for a
// delivery whose position is not a valid one. Every log delivery carries one,
// so an empty trigger is a producer's fault the wake survives, weaker by a
// floor, rather than a token naming a position that does not exist.
//
// THE COMMITTING RECORD'S OWN POSITION, because that is what the woken seat
// must read no older than: the change that woke it. The turn hands it to its
// node's floors before its first read, so whichever data node answers that
// read has applied the change — and this is the floor that names what the seat
// was woken for.
func triggerOf(rec Record) string {
	if rec.Position == 0 {
		return ""
	}
	at := statelog.Position{Stream: rec.Stream, Generation: uint32(rec.Gen), Seq: rec.Position}
	if at.Valid() != nil {
		return ""
	}
	return at.String()
}

// ClaimKey is the dedupe key for one change.
//
// SCOPED BY SOURCE, because two estates mint ids independently and a bare id
// would let a page change suppress a work change that happened to collide.
func ClaimKey(source, id string) string { return source + "|" + id }

// WakeID is the deterministic id of the wake one change produces for one
// recipient.
//
// DERIVED rather than random, which is what makes a duplicate recognisable at
// all: the inbox's same-id dedupe and the fleet completion ledger both key on
// it, and with random ids any producer retry wakes a seat twice with neither
// layer able to see the pair.
//
// PER RECIPIENT, because one change legitimately produces several wakes — an
// assignment that also names two watchers is three — and a single id for all
// of them would have the first delivered and the rest deduplicated away.
func WakeID(changeID, handle string) uuid.UUID {
	return uuid.NewSHA1(wakeNamespace, []byte(changeID+"\x00"+handle))
}

// wakeNamespace scopes derived wake ids. Fixed for the life of the
// deployment: a new one would make every redelivery a fresh wake.
var wakeNamespace = uuid.MustParse("2b3c4d5e-6f70-5182-93a4-b5c6d7e8f901")

func (f *Feed) ack(ctx context.Context, msg *Message) {
	if err := msg.Ack(); err != nil {
		log.WarnContext(ctx, "changefeed_ack_failed", "key", msg.Key,
			"error", err.Error(),
			"detail", "the change will be redelivered; the claim collapses it")
	}
}

// count records one event against this feed's own source.
//
// THE SOURCE IS THE ATTRIBUTE, because a company runs one feed per estate and
// "the change feed is stuck" is not actionable without knowing which.
func (f *Feed) count(name string) {
	if f.metrics == nil {
		return
	}
	f.metrics.Add(name, 1, metrics.Attrs{"source": f.translator.Source().Name})
}

// countReason records one event against this feed's source and the reason it
// handed a record back: a closed set ([DeferReason.Valid]), so the attribute
// opens no series per record.
func (f *Feed) countReason(name string, reason DeferReason) {
	if f.metrics == nil {
		return
	}
	f.metrics.Add(name, 1, metrics.Attrs{"source": f.translator.Source().Name,
		"reason": string(reason)})
}

// nak returns msg for redelivery at the feed's ordinary delay: a failure the
// next attempt may not meet.
func (f *Feed) nak(ctx context.Context, msg *Message) { f.nakAfter(ctx, msg, nakDelay) }

// nakAfter returns msg for redelivery after delay.
func (f *Feed) nakAfter(ctx context.Context, msg *Message, delay time.Duration) {
	if err := msg.Nak(delay); err != nil {
		log.WarnContext(ctx, "changefeed_nak_failed", "key", msg.Key,
			"error", err.Error(),
			"detail", "the broker redelivers it once its ack window ends; nothing is lost")
	}
}
