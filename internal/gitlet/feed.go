package gitlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE FEED: a seat hears about a change by a wake derived from the log
// (ADR-0012), never published by the record's writer. The log's fleet-wide
// consumer ([FeedGroup]) reads every committed record; the translator here
// decides whether a record can wake a seat, the parser ([Parser]) which seats,
// from the routing the record carries ([ChangeRouting]). The applier's notice
// rows (apply_notices.go) record whom a record concerned; the wakes are the
// seats among them the company still employs. A person is never queue-woken
// (their inbox is the notice rows), so a record routed only to persons is
// acknowledged. Every wake about a change names its work item
// ([Prompt.WorkItem], ADR-0097) and its turn is charged to it (ADR-0022): a
// change carries no spend. The package doc's "A change wakes the seats its
// records concern" has the rest.

// FeedGroup is the code log's wake consumer: the fleet-wide group the code
// feed runs as ([Domain.FeedGroup]) and the one the log's trim waits on.
//
// Never renamed: the fleet's position on the log IS this consumer, and a
// renamed group starts from the log's first retained record, re-waking every
// change the log holds beyond what the feed's claim and the derived wake id
// collapse inside their windows. It is the group, not the durable consumer's
// name (`<group>__CREWLET_CODE_LOG__<digest>`).
const FeedGroup = "crewlet-code-feed"

// RecordID is the identity a record about a change is told by: its operation
// id, else its log position — never an id minted by its reader, which would
// differ between nodes. The applier's notice row (code_notifications.record_id),
// the feed's claim and, through [NoticeID], the wake ids ([changefeed.WakeID])
// all key on it, so a notice and its wake name one record and one change.
func RecordID(opID string, at statelog.Position) string {
	if opID != "" {
		return opID
	}
	return at.String()
}

// Translator decides whether a committed code record can wake a seat, and
// hands the parser the whole record: the node that wins a feed message is
// often behind on its own rows, so the parser routes from the routing the
// record carries, never from a row.
type Translator struct{}

// NewTranslator builds the code log's feed translator.
func NewTranslator() *Translator { return &Translator{} }

// Source is the estate this translator serves: [Source], Gitlet's
// surface name every code write delivers to, and [FeedGroup].
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether rec wakes anybody. A record that wakes nobody is
// acknowledged, since one returned for having nothing to say comes back for
// ever and holds every wake behind it: one [wakesAnybody] turns away off the
// envelope, undecoded; one carrying no routing (from before changes, a push
// moving no change, a compaction); and one routed only to its own actor or to
// persons.
//
// A record this build cannot decode past its envelope is a newer build's,
// whose routing rules this node lacks: it is DEFERRED to a peer
// ([changefeed.Defer]), never acknowledged, since the consumer is the fleet's
// and a record acked here is one no peer ever sees (#838).
func (t *Translator) Translate(_ context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf("gitlet: read the envelope on %s: %w", rec.Key, err)
	}
	if !wakesAnybody(envelope.Subject.Kind, envelope.Op) {
		return changefeed.Delivery{}, false, nil
	}
	record, err := Decode(rec.Payload)
	if err != nil {
		// A newer build's record: waking anybody from a payload this node
		// cannot read would invent a notification, so it defers (see
		// Translate).
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return changefeed.Delivery{}, false, changefeed.Defer(changefeed.DeferNewerRecord, err)
		}
		return changefeed.Delivery{}, false, fmt.Errorf("gitlet: read the record on %s: %w", rec.Key, err)
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		// A pair this build does not know at a version it reads: the applier
		// stops on it too, so the feed waits beside it rather than ack a
		// record nobody here has applied.
		return changefeed.Delivery{}, false, fmt.Errorf("gitlet: read the payload on %s: %w", rec.Key, err)
	}
	if !wakesASeat(Concerns(RoutingsOf(payload))) {
		return changefeed.Delivery{}, false, nil
	}
	body, err := recordBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	at := statelog.Position{Stream: rec.Stream, Generation: uint32(rec.Gen), Seq: rec.Position}
	return changefeed.Delivery{Body: body, ID: RecordID(record.OpID, at), Actor: routedActor(payload)}, true, nil
}

// wakesAnybody reports, by its envelope, a record that can carry a routing:
// any repository record but its viewed key, any change record, and of the
// checks only an attempt's ending (a failed check of a change's head:
// CheckFinished.Routing). Every other kind is named by its absence, so a new
// one routes nobody until its records carry a routing and this names it. The
// checks are filtered by op because most of their records route nobody: every
// attempt is queued and started, a queued one carrying its whole definition,
// and decoding those would find nothing.
func wakesAnybody(kind ObjectKind, op OpKind) bool {
	switch kind {
	case KindRepo:
		return op != OpViewedKey
	case KindChange:
		return true
	case KindChecks:
		return op == OpCheckFinished
	}
	return false
}

// routedActor is who made the change a payload records — the one actor its
// routings share — or "" for a record of several actors (a push record of
// several pushers) or of none (the home's records but a revision's facts and
// a landing).
func routedActor(payload any) string {
	actor := ""
	for i, r := range RoutingsOf(payload) {
		if i > 0 && r.Actor != actor {
			return ""
		}
		actor = r.Actor
	}
	return actor
}

// recordBody renders a record as the map a parser reads through the domain's
// own encoder, so the body is exactly the record, fields this build carries
// but does not understand included.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := Encode(record)
	if err != nil {
		return nil, fmt.Errorf("gitlet: render the record body: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("gitlet: render the record body: %w", err)
	}
	return body, nil
}

// FeedSource opens the code log's own durable consumer: a second consumer over
// the stream the applier reads, at its own position, so a wake is derived by
// something that outlives the writer. The engine fills it, since which broker
// holds the log is the engine's business.
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
		return nil, fmt.Errorf("gitlet: the change feed has no log to consume, so nothing derives a wake " +
			"from a committed record and no seat hears about a change")
	}
	return s.Log.Consume(ctx, group)
}
