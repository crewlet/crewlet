package worklet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// Source is the notification source name this tracker registers under. Bare:
// it is in every log line, source column and dashboard filter, and a
// first-party source has nothing to disambiguate itself from.
const Source = "work"

// FeedGroup is the durable consumer's group, STABLE for the deployment's life:
// the name holds the fleet's position. A rename is a fresh consumer starting
// from the log's first retained record, so every change still held replays as
// a wake (deduped only within the change feed's claim and wake-id windows) and
// its acknowledgement floor holds back the log's trim until it catches up.
//
// The broker's durable name is derived from the group and the stream, so
// `nats consumer ls CREWLET_TRACKER_LOG` lists it and this string alone finds
// nothing.
const FeedGroup = "crewlet-tracker-feed"

// Translator turns one mutation record into the delivery a parser reads. It
// is the only place that decides whether a change wakes anybody at all; who,
// and whether they were woken already, belong to the parser and the inbox.
//
// The whole record travels in the body: the node winning a feed message is
// rarely the woken seat's and is often behind on its rows, so routing from a
// local read would use a stale head or block the feed. The record carries its
// own routing snapshot instead.
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
// A record with no notification, or on a machinery subject nobody watches, is
// a decision and is acked: a record naked for having nothing to say comes back
// for ever. A record this build cannot decode past its envelope is a newer
// build's, so it is deferred to a peer ([changefeed.Defer]) rather than acked:
// the consumer is the fleet's, and a record acked here no peer would ever see.
func (t *Translator) Translate(ctx context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"worklet: read the envelope on %s: %w", rec.Key, err)
	}
	if !wakesAnybody(envelope.Subject.Kind) {
		return changefeed.Delivery{}, false, nil
	}

	record, err := Decode(rec.Payload)
	if err != nil {
		// A newer build's record: deferred, never "no wake" (see the doc
		// comment), since waking from it would invent a notification.
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return changefeed.Delivery{}, false, changefeed.Defer(changefeed.DeferNewerRecord, err)
		}
		return changefeed.Delivery{}, false, fmt.Errorf(
			"worklet: read the record on %s: %w", rec.Key, err)
	}
	if record.Notify == nil {
		// The absence is on the record, not a feed parameter, so a
		// redelivery months later still wakes nobody.
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
// The machinery kinds are named, not derived from "has a notification": a
// task record without one is a change somebody chose not to announce, and a
// barrier can never carry one.
func wakesAnybody(kind ObjectKind) bool {
	switch kind {
	case KindBarrier, KindTrimmed, KindGeneration, KindEviction, KindTurn, KindRankOrder,
		KindRoster,
		// A file is read from its project, never woken into an inbox
		// ([ObjectKind.Routable]).
		KindFile:
		return false
	}
	return true
}

// recordBody renders a record as the map a parser reads.
//
// Through JSON rather than field by field, so the body keeps the fields this
// build carries but does not understand, and a newer parser sees everything
// the writer wrote.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := record.Encode()
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("worklet: render the record body: %w", err)
	}
	return body, nil
}

// FeedSource opens the tracker's own durable consumer over its log.
//
// A second consumer over the applier's stream, at its own position, so a wake
// is derived by something that outlives the writer rather than published by
// the writer's goroutine.
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
		return nil, fmt.Errorf("worklet: the change feed has no log to consume, " +
			"so nothing derives a wake from a committed record and every " +
			"notification in the company depends on the writer's own goroutine")
	}
	return s.Log.Consume(ctx, group)
}
