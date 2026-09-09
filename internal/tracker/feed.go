package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// Source is the notification source name this tracker registers under.
//
// BARE, and the same word the engine's earlier tracker used: it appears in
// every log line, every event's source column and every dashboard filter, and
// a first-party source has nothing to disambiguate itself from. Keeping the
// spelling is also what lets a company's existing notification rules go on
// meaning what they meant.
const Source = "work"

// FeedGroup is the durable consumer's name, and it is STABLE for the
// deployment's life.
//
// The name is where the fleet's position IS: a rename starts a second consumer
// at the head of the log and abandons everything the first had not handled —
// which is a company that silently stops being told about its own work, with
// no error anywhere.
const FeedGroup = "crewlet-tracker-feed"

// Translator turns one mutation record into the delivery a parser reads.
//
// # What this contributes, and what it deliberately does not
//
// The feed knows how to run a durable consumer exactly once. It knows nothing
// about what a task is, and this is the only place that decides whether a
// change should wake anybody at all. Everything else — who, and whether they
// have been woken already — belongs to the parser and the inbox.
//
// # Why the whole record travels in the body
//
// The node that WINS a feed message is rarely the node running the seat that
// gets woken, and is often behind on its own rows. Routing from a local read
// would either use a stale head or block the feed until it caught up; the
// record carries its own routing snapshot precisely so neither is necessary.
type Translator struct{}

// NewTranslator builds the tracker's feed translator.
func NewTranslator() *Translator { return &Translator{} }

// Source is the estate this translator serves.
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether a record wakes anybody, and hands the parser the
// whole thing.
//
// # The three ways a record wakes nobody, and why each is an ACK
//
// A record with NO NOTIFICATION is a change nobody asked to hear about — a
// rank move, a pointer flip, a turn's spend. A record this build cannot decode
// past its envelope is one whose notification rules it does not have. And a
// record on a subject that is not an object anybody watches — a barrier, a
// generation, an eviction — is machinery.
//
// All three are DECISIONS, and a decision is handling: they are acked rather
// than naked, because a record naked for having nothing to say comes back for
// ever.
func (t *Translator) Translate(ctx context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"tracker: read the envelope on %s: %w", rec.Key, err)
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
			"tracker: read the record on %s: %w", rec.Key, err)
	}
	if record.Notify == nil {
		// A CHANGE NOBODY ASKED TO HEAR ABOUT. The absence is on the
		// RECORD rather than a parameter to the feed, so a redelivery
		// months later still knows not to wake anybody — which a
		// runtime flag could not.
		return changefeed.Delivery{}, false, nil
	}

	body, err := recordBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	return changefeed.Delivery{
		Body:  body,
		ID:    record.OpID,
		Actor: record.Actor,
	}, true, nil
}

// wakesAnybody reports a kind whose records are about an object somebody may
// be watching.
//
// THE MACHINERY KINDS ARE NAMED rather than derived from "has a notification",
// because the two are different facts: a task record without one is a change
// somebody chose not to announce, and a barrier can never have one at all.
// Folding them together would make a missing notification on a task
// indistinguishable from a record that could not carry one.
func wakesAnybody(kind ObjectKind) bool {
	switch kind {
	case KindBarrier, KindGeneration, KindEviction, KindTurn, KindRankOrder:
		return false
	}
	return true
}

// recordBody renders a record as the map a parser reads.
//
// THROUGH JSON rather than field by field, so the body is exactly the record —
// including the fields this build carries but does not understand. A parser on
// a newer node reading a record an older node relayed sees everything the
// writer wrote.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := record.Encode()
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("tracker: render the record body: %w", err)
	}
	return body, nil
}

// FeedSource opens the tracker's own durable consumer over its log.
//
// IT IS THE DOMAIN'S, not the framework's: a change feed is a SECOND consumer
// over the same stream the applier reads, at its own position, so that a wake
// is derived by something that outlives the writer rather than published by
// the writer's goroutine as a courtesy.
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
		return nil, fmt.Errorf("tracker: the change feed has no log to consume, " +
			"so nothing derives a wake from a committed record and every " +
			"notification in the company depends on the writer's own goroutine")
	}
	return s.Log.Consume(ctx, group)
}
