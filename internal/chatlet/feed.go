package chatlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// log is the package's logger. It lives beside the feed because the feed and
// the parser are where "the company stopped being told about its own
// conversation" is diagnosed, and every other file here reports through an
// error a caller reads.
var log = logging.Get("chatlet")

// Source is the notification source name Chatlet registers under.
//
// BARE, for the reason [github.com/crewlet/crewlet/internal/worklet.Source]
// is: it appears in every log line, every event's source column and every
// dashboard filter, and a first-party source has nothing to disambiguate
// itself from.
//
// IT IS ALSO THE DELIVERY SURFACE. The reply obligation is source-scoped —
// the turn's delivery gate compares the surface a posting tool declares with
// `tools.DeliversTo` against the notification source the inbound wake
// carried — so the three chat posting tools declare THIS constant and the
// parser publishes wakes under it. A second spelling on either side does not
// fail: the gate finds no delivery for the source, falls back to "any
// delivery counts", and every addressed chat turn is then allowed to end in
// silence. That failure has shipped once already, which is why both sides
// name one constant rather than one string twice.
const Source = "chat"

// FeedGroup is the durable consumer's name, and it is STABLE for the
// deployment's life.
//
// THE NAME IS WHERE THE FLEET'S POSITION IS. A durable consumer is addressed
// by its name, so a rename does not move the consumer — it starts a SECOND
// one at the head of the log and abandons everything the first had not
// handled yet. For this domain that is a company whose people go on talking
// while no seat is woken by any of it, with no error anywhere and nothing to
// replay from. It is chosen once and never improved.
const FeedGroup = "crewlet-chat-feed"

// Translator turns one committed chat record into the delivery the parser
// reads.
//
// # What it contributes, and what it deliberately does not
//
// The feed knows how to run a durable consumer exactly once. This is the only
// place that decides whether a change can wake anybody AT ALL; WHO it wakes is
// the parser's, and whether they have been woken already is the inbox's.
//
// # Why the whole record travels in the body
//
// The node that WINS a feed message is rarely the node running the seat that
// gets woken, and is usually behind on the row: a message is committed on the
// publishing node's own apply and this consumer is a second, independent
// position over the same log. Routing from a local read would either route
// from rows that do not have the message in them yet, or block the feed until
// they did. The record carries its own routing snapshot ([Notify]) precisely
// so neither is necessary.
//
// # Why it holds no setting, and what it does hold
//
// Every decision below is a property of the RECORD. A translator with a
// setting would be a wake filter a redelivery months later could not
// reproduce — the knowledge base's
// ([github.com/crewlet/crewlet/internal/doclet.Translator]) holds one, the
// reserved skills container, and states the cost of reading it per change
// rather than capturing it. Nothing here needs one.
//
// What it holds is this node's [Decisions]: the applier's verdict on a post or
// an edit is the record's own decision, identical on every node, and a record
// the applier refused wakes nobody ([Translator.Translate]).
type Translator struct {
	decisions Decisions
}

// NewTranslator builds Chatlet's feed translator over this node's
// decisions.
//
// NIL IS A TRANSLATOR THAT ASKS NOTHING and derives every wake from the record
// alone, which is what one built where no estate is held answers — and what
// every record's wake was before the applier decided messages. A node that
// runs the feed holds the estate (its group needs data), so the engine always
// hands it one.
func NewTranslator(decisions Decisions) *Translator {
	return &Translator{decisions: decisions}
}

// Source is the estate this translator serves: the notification source name
// the parser registers under, and this domain's own fleet-wide consumer
// group.
//
// THE GROUP IS THE DOMAIN'S NAME rather than a family's, because a log
// delivery has no family and no key class. A group rather than a duty, so a
// lease flap on one node cannot stall the whole company's notifications for
// work that is stateless.
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether a committed record wakes anybody, and hands the
// parser the whole thing.
//
// # The five ways a record wakes nobody, and why every one of them is an ACK
//
// A MACHINERY SUBJECT — a barrier, a generation, an eviction — is the fleet
// talking to itself. A wake per barrier would page the company once per
// linearizable read.
//
// AN OPERATION THAT WAKES NOBODY: see [wakesAnybody].
//
// A PAYLOAD FROM A NEWER BUILD. The envelope decoded, so this node knows what
// the record is about and knows it cannot read the rules — and waking
// somebody from a payload it cannot decode would be inventing a notification.
// A peer on that build wins the next redelivery, so the record is handled
// here rather than lost.
//
// A NIL ROUTING SNAPSHOT is how a record says "wake nobody" — the engine
// narrating its own room, a reaction and a prune are the records it exists
// for: each is a line to render or a row to remove, not a question to answer.
//
// A RECORD THE APPLIER REFUSED — a post into a room a record ahead of it
// archived, or by an author taken out of it; an edit of a message taken back
// — is in no room, and a seat woken for it would open a room to answer words
// nobody can read. Where this node has not decided the record, the wake is
// derived from the record as it always was: see [Decisions] for why the feed
// never waits on an applier that cannot answer.
//
// All five are DECISIONS, and a decision is handling: acked rather than
// naked, because a record naked for having nothing to say comes back for
// ever.
func (t *Translator) Translate(ctx context.Context, rec changefeed.Record) (
	changefeed.Delivery, bool, error) {

	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"chatlet: read the envelope on %s: %w", rec.Key, err)
	}
	// TWO FILTERS BEFORE THE SECOND DECODE, and they answer different
	// questions: whether this object is one anybody watches, and whether
	// this gesture is one anybody is told about. Both are read off the
	// envelope, which decodes at EVERY version — so a record from a newer
	// build is filtered on the same terms as one from this build.
	if !envelope.Subject.Kind.Routable() {
		return changefeed.Delivery{}, false, nil
	}
	if !wakesAnybody(envelope.Op) {
		return changefeed.Delivery{}, false, nil
	}

	record, err := Decode(rec.Payload)
	if err != nil {
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			log.DebugContext(ctx, "chat_change_from_a_newer_build",
				"subject", envelope.Subject.String(), "version", future.Got)
			return changefeed.Delivery{}, false, nil
		}
		return changefeed.Delivery{}, false, fmt.Errorf(
			"chatlet: read the record on %s: %w", rec.Key, err)
	}
	if record.Notify == nil {
		// THE ABSENCE IS ON THE RECORD rather than a parameter to the
		// feed, so a redelivery months later still knows not to wake
		// anybody — which a runtime flag could not.
		return changefeed.Delivery{}, false, nil
	}

	if !t.landed(ctx, rec, record) {
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

// landed reports whether a record may wake anybody as far as this node's
// applier has decided it: false only for a verdict that says it did not land.
//
// FAILS OPEN on everything else — no decisions, no decision yet, and a verdict
// this node could not read — because the wake was the record's before the
// applier decided anything, and a wake for a refused post costs a seat one
// look at a room while a wake withheld over a failed read is a person nobody
// tells. A read that failed is logged, since it is the one of the three that
// says something here is wrong.
func (t *Translator) landed(ctx context.Context, rec changefeed.Record,
	record MutationRecord) bool {

	if t.decisions == nil {
		return true
	}
	at := statelog.Position{Stream: rec.Stream, Generation: uint32(rec.Gen),
		Seq: rec.Position}
	v, ok, err := t.decisions.Decided(ctx, record.OpID, at)
	switch {
	case err != nil:
		log.WarnContext(ctx, "chat_wake_verdict_unread", "subject",
			record.Subject.String(), "op", record.OpID, "position", at.String(),
			"error", err.Error(),
			"detail", "the wake is derived from the record, as it is where this "+
				"node has not decided it, and may be one for a message the "+
				"applier refused")
		return true
	case !ok:
		return true
	case v.Landed():
		return true
	}
	log.DebugContext(ctx, "chat_wake_withheld", "subject", record.Subject.String(),
		"op", record.OpID, "verdict", string(v))
	return false
}

// wakesAnybody reports an op whose records can carry a notification.
//
// TWO, AND IT IS A CLOSED SET: somebody saying something, and the edit of
// something somebody said — an edit because a colleague the new text NOW
// NAMES has been addressed by it and has heard nothing about it at all.
//
// THE OTHER TEN ARE QUIET, and each is quiet for its own reason rather than
// by omission:
//
//   - A PRUNE and an ERASE remove what was said. A wake would send a seat to
//     open an absence.
//   - A REACTION is a toggle on somebody else's message — and the same op
//     carries the un-reaction, since a set-valued record would let the later
//     of two simultaneous reactors erase the earlier. Waking a room for one
//     would make an emoji cost a model call.
//   - A MEMBERSHIP REPLACEMENT and a PATCH are a room's own state. Not even a
//     patch that moves the topic, which is the one somebody expects to be
//     announced: the room's own screen shows it, and a company where every
//     topic edit woke every member would have agents triaging furniture.
//   - A DELETE tombstones a message, a CREATE makes an empty room, and an
//     EVICTION, a GENERATION and a BARRIER are the fleet's own bookkeeping.
//
// A CLOSED SET RATHER THAN A NEGATIVE ONE, on [ObjectKind.Routable]'s
// reasoning: an op added later is silently quiet rather than silently loud,
// and the failure of a missing wake is one person not hearing something while
// the failure of an unintended one is every seat in the company woken by a
// bookkeeping append.
//
// The record's own [MutationRecord.Notify] remains the authority — this is
// the half that can be answered without the second decode, on the ops that
// are the overwhelming majority of a busy company's traffic.
func wakesAnybody(op OpKind) bool {
	switch op {
	case OpPost, OpEdit:
		return true
	}
	return false
}

// recordBody renders a record as the map a parser reads.
//
// THROUGH JSON rather than field by field, so the body is exactly the record —
// carried unknown fields included. A parser on a newer node reading a record
// an older node relayed sees everything the writer wrote.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := Encode(record)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("chatlet: render the change body on %s: %w",
			record.Subject, err)
	}
	return body, nil
}

// FeedSource opens this domain's own durable consumer over its log.
//
// A SEAM the engine fills, because which broker the log lives on is the
// engine's business and what a record MEANS is this package's. It is the
// domain's own consumer rather than the framework's: a change feed is a
// SECOND reader over the same stream the applier reads, at its own position,
// so a wake is derived by something that OUTLIVES the writer rather than
// published by the writer's goroutine as a courtesy.
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
		return nil, fmt.Errorf("chatlet: the change feed has no log to consume, " +
			"so nothing derives a wake from a committed message and every " +
			"seat in the company is deaf to what is said to it — give " +
			"chatlet.FeedSource the running domain's log")
	}
	return s.Log.Consume(ctx, group)
}
