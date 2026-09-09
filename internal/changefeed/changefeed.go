// Package changefeed turns a committed record into a wake.
//
// # The principle
//
// A wake is derived from a DURABLE, NEVER-COMPACTED record by something that
// outlives the writer — never published by the writer's own goroutine as a
// courtesy. The engine has both failure modes already: the Mattermost socket
// path swallows a failed publish, and the webhook path is safe only because
// the vendor retries. A native write has no vendor to retry it, so the
// durable record is the retry: the record is committed, and a fleet-wide
// consumer over it is what eventually reaches a seat. A node that dies
// between the write and the publish costs a redelivery, not a lost wake.
//
// # Why the estate is a seam and not a family
//
// This package once spoke the coordination estate's own vocabulary: a
// [coord.Family], a bucket key class, a [coord.Change]. A durable record does
// not have to live in a bucket — a state machine's LOG is the other shape,
// and a log delivery has no family, no key class and no change. Manufacturing
// a family for one would be worse than the vocabulary it fixed: a family is
// also what starts a projector, so the engine would stand one up over a
// bucket that does not exist.
//
// So the seam is one level up. A [Source] names the estate as a STRING, an
// [Opener] opens one durable consumer over it, and a [Record] is one delivered
// change from either — the bucket adapter is [DocumentSource] here, and a log
// domain supplies its own. Everything below this comment is unchanged by that
// and is why the package exists.
//
// # Why a group and not a duty
//
// Every node pulls from one durable consumer, so a change is handled by
// whichever node gets there first. As a singleton DUTY it would sit behind a
// lease, and a flap on the duty holder would stall the whole company's
// notifications for a lease TTL — for work that is stateless and needs no
// ownership at all.
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
package changefeed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("changefeed")

// ClaimTTL is how long a handled change stays claimed.
//
// Five minutes, matching the webhook edge's delivery dedupe and sized for the
// same thing: a redelivery after a node died mid-handle, and an operator's
// replay. It is deliberately NOT the retention of the change record — that is
// a year — because the claim answers "did somebody already publish this
// wake", which stops mattering as soon as no consumer could still be holding
// the message.
const ClaimTTL = 5 * time.Minute

// nakDelay is how long a failed handle waits before coming back.
//
// Two seconds: long enough that a broker reconnect or a leader election
// completes inside one delay rather than being retried through, short enough
// that a person waiting on a notification does not notice. The consumer's own
// redelivery cap is what ends a change that keeps failing.
const nakDelay = 2 * time.Second

// Source identifies the estate a feed reads.
//
// A STRING, because every estate this reads is now a LOG: the document
// families it was written against are gone, and a log delivery has no family,
// no key class and no change record to name one with.
type Source struct {
	// Name is the notification source a parser registers under, and the
	// scope of this estate's claim keys — "work", "page", "tracker".
	Name string

	// Group is the durable consumer's name, and it is STABLE for the
	// deployment's life: the name is where the fleet is, so a rename
	// starts a second consumer at the head and abandons everything the
	// first had not handled.
	//
	// EACH ESTATE DECLARES ITS OWN, as a constant beside the translator
	// that reads it. The helper that used to derive one from a document
	// family is gone with the families — and what it was for survives as
	// a rule rather than a function: a name is chosen once and never
	// improved.
	Group string
}

// Opener opens one durable consumer over an estate.
//
// DECLARED HERE, by the consumer, and satisfied by [DocumentSource] for a
// bucket family and by a state log's own domain for a log. The group name is
// passed rather than held so there is exactly one place it comes from — the
// translator's own [Source].
type Opener interface {
	Open(ctx context.Context, group string) (Records, error)
}

// Records is an open durable consumer.
type Records interface {
	// Next blocks for the next message until the context ends.
	//
	// A nil Message with a nil error means the consumer has closed, which
	// is how a caller tells a shutdown from a failure.
	Next(ctx context.Context) (*Message, error)

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
}

// Record is one delivered change, whatever estate it came from.
type Record struct {
	// ID is the estate's own identity for this delivery: a create-only
	// bucket key, or a log record's operation id. Stable across
	// redeliveries, which is what lets a translator that has no id of its
	// own use it as one.
	ID string

	// Position is the delivery's place in its estate — the composed log
	// position, or the bucket revision.
	//
	// # Why the three fields below travel with it
	//
	// A position alone is only comparable against the same stream and the
	// same generation. The node that WINS a message is rarely the node
	// that runs the woken seat, and a reanchor renumbers a log — so a
	// wake stamped with a bare position is a number the woken node cannot
	// safely compare with its own. A bucket feed leaves them empty and
	// zero, which is the honest answer for an estate that has neither.
	Position uint64

	// Stream is the log's stream name, empty for a bucket feed.
	Stream string

	// Gen is the log's generation, zero for a bucket feed.
	Gen uint64

	// Key is the bucket key, or the subject, this delivery arrived on. For
	// diagnosis, and for a translator that parses it.
	Key string

	// Payload is the record's bytes, verbatim.
	Payload []byte

	// Removed marks a delivery that says the record is GONE rather than
	// carrying one — a retention sweep or an operator's delete on the
	// bucket estate. A log never produces one: its records are its
	// history.
	//
	// The framework acks it and never translates it, because the decision
	// is the same in every estate and cannot be otherwise: the wake this
	// record once produced was delivered when it was written, and there is
	// nothing left to derive a second one from.
	Removed bool
}

// Publisher is the queue surface this package publishes wakes through.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
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
	// import, a draft, a record this build has no rule for. Every one of
	// those is a DECISION, and a decision is handling.
	Translate(ctx context.Context, rec Record) (Delivery, bool, error)
}

// Delivery is what a translated record becomes on the inbound topic.
type Delivery struct {
	// Body is the payload a parser reads. The whole record travels in it,
	// so the node that wins a feed message routes without reading anything
	// — a projection that had not caught up would otherwise route from a
	// stale head, or block the feed until it had.
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
}

// Options configure a feed.
type Options struct {
	Opener     Opener
	Publisher  Publisher
	Claims     Claims
	Translator Translator

	// Now is the clock the claim is taken on. Nil takes the wall clock.
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
	}
	if f.now == nil {
		f.now = func() time.Time { return time.Now().UTC() }
	}
	return f, nil
}

// Run consumes the feed until the context ends.
func (f *Feed) Run(ctx context.Context) error {
	src := f.translator.Source()
	records, err := f.opener.Open(ctx, src.Group)
	if err != nil {
		return fmt.Errorf("changefeed: open the %s feed: %w", src.Name, err)
	}
	defer func() { _ = records.Stop() }()

	log.InfoContext(ctx, "changefeed_started", "source", src.Name, "group", src.Group)
	for {
		msg, err := records.Next(ctx)
		if err != nil {
			return fmt.Errorf("changefeed: read the %s feed: %w", src.Name, err)
		}
		if msg == nil {
			return nil
		}
		f.handle(ctx, msg)
	}
}

// handle processes one message, settling it exactly once.
func (f *Feed) handle(ctx context.Context, msg *Message) {
	if msg.Removed {
		// The record is gone. Nothing to tell anybody: the wake it once
		// produced was delivered when it was written.
		log.DebugContext(ctx, "changefeed_record_removed", "key", msg.Key)
		f.ack(ctx, msg)
		return
	}
	body, wake, err := f.translator.Translate(ctx, msg.Record)
	if err != nil {
		// A translation failure is a RECORD THIS BUILD CANNOT READ, and a
		// redelivery will not make it readable. It is naked all the same,
		// because the consumer's own redelivery cap is what ends it — and
		// the alternative (acking) would drop a wake permanently on a
		// build that is about to be upgraded past the problem.
		log.WarnContext(ctx, "changefeed_untranslatable", "key", msg.Key,
			"position", msg.Position, "stream", msg.Stream, "generation", msg.Gen,
			"error", err.Error(),
			"detail", "returned for redelivery; the consumer's own cap ends it "+
				"if no node can read it")
		f.nak(ctx, msg)
		return
	}
	if !wake {
		// A DECISION IS HANDLING. A quiet import, a draft, an actor-only
		// change: acked, because naking would circle it to the
		// dead-letter path for having been handled correctly.
		f.ack(ctx, msg)
		return
	}

	if !f.claim(ctx, body.ID) {
		log.DebugContext(ctx, "changefeed_already_handled", "change", body.ID)
		f.ack(ctx, msg)
		return
	}

	ev := events.New(types.RawWebhook{Body: body.Body, Handle: body.Actor}, events.NewTrace())
	ev.Source = f.translator.Source().Name
	if err := f.publisher.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
		// THE CLAIM IS RELEASED BEFORE THE NAK. A claim held over a
		// delivery that never published would make the redelivery skip
		// it, which is a wake lost to a broker hiccup — the exact failure
		// the durable record exists to prevent.
		f.release(ctx, body.ID)
		log.WarnContext(ctx, "changefeed_publish_failed", "change", body.ID,
			"error", err.Error())
		f.nak(ctx, msg)
		return
	}
	f.ack(ctx, msg)
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

func (f *Feed) nak(ctx context.Context, msg *Message) {
	if err := msg.Nak(nakDelay); err != nil {
		log.WarnContext(ctx, "changefeed_nak_failed", "key", msg.Key,
			"error", err.Error())
	}
}
