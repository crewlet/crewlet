package pages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// Source is the notification source name the native knowledge base registers
// under. Bare, for the reason [work.Source] is.
const Source = "page"

// FeedGroup is the durable consumer's name, and it is STABLE for the
// deployment's life.
//
// The name is where the fleet's position IS. A rename is a fresh consumer,
// and a fresh one starts from the log's first retained record rather than its
// head, so it loses nothing and REPLAYS instead: every change the log still
// holds comes back as a wake, which only the change feed's claim and wake-id
// dedupe collapse, and only within their own windows — a company re-woken
// for page changes it already handled — and until the new consumer catches
// up, its acknowledgement floor holds back the log's trim.
//
// This is the GROUP, not the broker's consumer name: the durable name is
// derived from the group and the stream together, so `nats consumer ls
// CREWLET_PAGES_LOG` lists it and this string alone finds nothing. The same
// derivation is why the spelling — the one the coordination-family feed this
// domain replaced ran under — carried no position over to this log: that feed
// read a bucket, a different stream, so it was a different consumer.
const FeedGroup = "crewlet-pages-feed"

// Translator turns a change record into the delivery the parser reads.
type Translator struct {
	// skills names the container whose pages wake nobody. A tool skill is
	// machinery, and an edit to one must not notify a team about a
	// procedure written for a phase of a turn.
	//
	// READ PER CHANGE, never captured, for the reason [Searcher] gives:
	// `knowledge.skills_container` is Tier B, and a feed is per-NODE — it
	// follows a coordination family and is deliberately not rebuilt by an
	// apply, so a captured key would outlive every configuration that
	// moved it.
	skills func() string
}

// NewTranslator builds the knowledge base's feed translator.
//
// skills names the reserved tool-skills container. Nil, or a function
// returning empty, quiets nothing — which is the company that has turned
// tool skills off.
func NewTranslator(skills func() string) *Translator { return &Translator{skills: skills} }

// skillsContainer is the reserved container this instant, or empty.
func (t *Translator) skillsContainer() string {
	if t.skills == nil {
		return ""
	}
	return strings.TrimSpace(t.skills())
}

// Source is the estate this translator serves: the notification source name
// parsers register under, and this domain's own fleet-wide consumer group.
//
// THE GROUP IS THE DOMAIN'S NAME rather than a family's, because a log
// delivery has no family and no key class. A group rather than a duty, so a
// lease flap on one node cannot stall the whole company's notifications for
// work that is stateless.
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether a committed record wakes anybody.
func (t *Translator) Translate(ctx context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"pages: read the envelope on %s: %w", rec.Key, err)
	}
	if !wakesAnybody(envelope.Subject.Kind) {
		return changefeed.Delivery{}, false, nil
	}

	record, err := Decode(rec.Payload)
	if err != nil {
		// A RECORD FROM A NEWER BUILD. Its envelope decoded, so this
		// node knows what it is about and knows it cannot read the
		// rules — and waking somebody from a payload it cannot decode
		// would be inventing a notification. A peer on the newer build
		// wins the next redelivery; this one is handled, not lost.
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return changefeed.Delivery{}, false, nil
		}
		return changefeed.Delivery{}, false, fmt.Errorf(
			"pages: read the record on %s: %w", rec.Key, err)
	}
	if record.Notify == nil {
		// A CHANGE NOBODY ASKED TO HEAR ABOUT. The absence is on the
		// RECORD rather than a parameter to the feed, so a redelivery
		// months later still knows not to wake anybody — which a runtime
		// flag could not.
		return changefeed.Delivery{}, false, nil
	}
	if skills := t.skillsContainer(); skills != "" &&
		strings.EqualFold(record.Notify.Container, skills) {
		// A TOOL SKILL. The applier picks it up on its own — natively
		// there is no page webhook and this feed deliberately drops
		// these changes — and waking a team about one would put a
		// procedure written for one phase of one turn in front of
		// somebody as knowledge.
		log.DebugContext(ctx, "pages_change_in_skills_container",
			"record", record.OpID, "container", record.Notify.Container)
		return changefeed.Delivery{}, false, nil
	}

	body, err := recordBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	return changefeed.Delivery{
		Body: body, ID: record.OpID, Actor: record.Actor,
	}, true, nil
}

// wakesAnybody reports a kind whose records can carry a notification.
//
// ONLY THE TWO THAT TOUCH A PAGE. A container's settings, an eviction, a
// generation and a barrier are machinery: nobody watches them, and a delivery
// for one would be a wake with no recipient and no card to render.
func wakesAnybody(kind ObjectKind) bool {
	return kind == KindPage || kind == KindTitle
}

// recordBody renders a record as the map a parser reads, through JSON so the
// body is exactly the record — carried unknown fields included.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := Encode(record)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("pages: render the change body: %w", err)
	}
	return body, nil
}

// FeedSource opens this domain's own durable consumer.
//
// A SEAM the engine fills, because which broker the log lives on is the
// engine's business and what a record means is this package's.
type FeedSource struct {
	Log DomainConsumer
}

// DomainConsumer is the half of a log this feed needs.
type DomainConsumer interface {
	Consume(ctx context.Context, group string) (changefeed.Records, error)
}

// Open opens the consumer.
func (s FeedSource) Open(ctx context.Context, group string) (changefeed.Records, error) {
	if s.Log == nil {
		return nil, fmt.Errorf("pages: the change feed has no log to consume, " +
			"so nothing derives a wake from a committed record and every " +
			"notification about the knowledge base depends on the writer's own " +
			"goroutine")
	}
	return s.Log.Consume(ctx, group)
}
