// Package changefeed turns a committed change record into a wake.
//
// # The principle
//
// A wake is derived from a DURABLE, NEVER-COMPACTED record by something that
// outlives the writer — never published by the writer's own goroutine as a
// courtesy. The engine has both failure modes already: the Mattermost socket
// path swallows a failed publish, and the webhook path is safe only because
// the vendor retries. A native write has no vendor to retry it, so the
// durable record is the retry: the change key is created, and a fleet-wide
// consumer over that key is what eventually reaches a seat. A node that dies
// between the write and the publish costs a redelivery, not a lost wake.
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

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("changefeed")

// Group is the durable consumer name for a family's change feed.
//
// ONE PER FAMILY, and stable for the life of the deployment: the name IS the
// fleet's position, so renaming it creates a second consumer at the head and
// silently abandons whatever the first had not yet handled.
func Group(family coord.Family) string { return "crewlet-" + string(family) + "-feed" }

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

// Translator turns one change record into the delivery a parser will read.
//
// DECLARED HERE, implemented by the package that owns the family's documents.
// This package knows how to run a durable feed exactly once; it knows nothing
// about what a work item or a page is, and adding that knowledge would put
// two domains into one loop.
type Translator interface {
	// Family is which family this translator serves.
	Family() coord.Family

	// Class is the key class the feed filters on.
	Class() string

	// Source is the notification source name a parser registers under.
	Source() string

	// Translate turns a change into a delivery body, reporting whether it
	// should wake anybody at all.
	//
	// FALSE IS AN ORDINARY OUTCOME and is ACKED rather than naked: a
	// quiet import, a purge marker, a record this build cannot decode.
	// Every one of those is a DECISION, and a decision is handling.
	Translate(ctx context.Context, change coord.Change) (Delivery, bool, error)
}

// Delivery is what a translated change becomes on the inbound topic.
type Delivery struct {
	// Body is the payload a parser reads. The whole record travels in it,
	// so the node that wins a feed message routes without reading anything
	// — a projection that had not caught up would otherwise route from a
	// stale head, or block the feed until it had.
	Body map[string]any

	// ID is the change's own id, used for the claim and as the seed of
	// every recipient's deterministic wake id.
	ID string

	// Actor is the handle that made the change, so a parser can decline to
	// wake somebody about their own write.
	Actor string
}

// Feed runs one family's change feed.
type Feed struct {
	feeder     coord.Feeder
	publisher  Publisher
	claims     Claims
	translator Translator
	now        func() time.Time
}

// Options configure a feed.
type Options struct {
	Feeder     coord.Feeder
	Publisher  Publisher
	Claims     Claims
	Translator Translator

	// Now is the clock the claim is taken on. Nil takes the wall clock.
	Now func() time.Time
}

// New builds a feed.
func New(opts Options) (*Feed, error) {
	switch {
	case opts.Feeder == nil:
		return nil, errors.New("changefeed: a feeder is required")
	case opts.Publisher == nil:
		return nil, errors.New("changefeed: a publisher is required")
	case opts.Translator == nil:
		return nil, errors.New("changefeed: a translator is required")
	}
	f := &Feed{
		feeder: opts.Feeder, publisher: opts.Publisher,
		claims: opts.Claims, translator: opts.Translator, now: opts.Now,
	}
	if f.now == nil {
		f.now = func() time.Time { return time.Now().UTC() }
	}
	return f, nil
}

// Run consumes the feed until the context ends.
func (f *Feed) Run(ctx context.Context) error {
	family := f.translator.Family()
	feed, err := f.feeder.FeedDocuments(ctx, family, f.translator.Class(), Group(family))
	if err != nil {
		return fmt.Errorf("changefeed: open the %s feed: %w", family, err)
	}
	defer func() { _ = feed.Stop() }()

	log.InfoContext(ctx, "changefeed_started", "family", string(family),
		"source", f.translator.Source(), "group", Group(family))
	for {
		delivery, err := feed.Next(ctx)
		if err != nil {
			return fmt.Errorf("changefeed: read the %s feed: %w", family, err)
		}
		if delivery == nil {
			return nil
		}
		f.handle(ctx, delivery)
	}
}

// handle processes one delivery, settling it exactly once.
func (f *Feed) handle(ctx context.Context, delivery *coord.Delivery) {
	body, wake, err := f.translator.Translate(ctx, delivery.Change)
	if err != nil {
		// A translation failure is a RECORD THIS BUILD CANNOT READ, and a
		// redelivery will not make it readable. It is naked all the same,
		// because the consumer's own redelivery cap is what ends it — and
		// the alternative (acking) would drop a wake permanently on a
		// build that is about to be upgraded past the problem.
		log.WarnContext(ctx, "changefeed_untranslatable", "key", delivery.Key,
			"revision", delivery.Revision, "error", err.Error(),
			"detail", "returned for redelivery; the consumer's own cap ends it "+
				"if no node can read it")
		f.nak(ctx, delivery)
		return
	}
	if !wake {
		// A DECISION IS HANDLING. A quiet import, a purge marker, an
		// actor-only change: acked, because naking would circle it to the
		// dead-letter path for having been handled correctly.
		f.ack(ctx, delivery)
		return
	}

	if !f.claim(ctx, body.ID) {
		log.DebugContext(ctx, "changefeed_already_handled", "change", body.ID)
		f.ack(ctx, delivery)
		return
	}

	ev := events.New(types.RawWebhook{Body: body.Body, Handle: body.Actor}, events.NewTrace())
	ev.Source = f.translator.Source()
	if err := f.publisher.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
		// THE CLAIM IS RELEASED BEFORE THE NAK. A claim held over a
		// delivery that never published would make the redelivery skip
		// it, which is a wake lost to a broker hiccup — the exact failure
		// the durable record exists to prevent.
		f.release(ctx, body.ID)
		log.WarnContext(ctx, "changefeed_publish_failed", "change", body.ID,
			"error", err.Error())
		f.nak(ctx, delivery)
		return
	}
	f.ack(ctx, delivery)
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
	won, err := f.claims.Claim(ctx, ClaimKey(f.translator.Source(), id), ClaimTTL, f.now())
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
	if err := f.claims.Release(ctx, ClaimKey(f.translator.Source(), id)); err != nil {
		log.WarnContext(ctx, "changefeed_claim_release_failed", "change", id,
			"error", err.Error())
	}
}

// ClaimKey is the dedupe key for one change.
//
// SCOPED BY SOURCE, because two families mint ids independently and a bare id
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

func (f *Feed) ack(ctx context.Context, delivery *coord.Delivery) {
	if err := delivery.Ack(); err != nil {
		log.WarnContext(ctx, "changefeed_ack_failed", "key", delivery.Key,
			"error", err.Error(),
			"detail", "the change will be redelivered; the claim collapses it")
	}
}

func (f *Feed) nak(ctx context.Context, delivery *coord.Delivery) {
	if err := delivery.Nak(nakDelay); err != nil {
		log.WarnContext(ctx, "changefeed_nak_failed", "key", delivery.Key,
			"error", err.Error())
	}
}
