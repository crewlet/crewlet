package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE FEED: a seat hears about a change by a WAKE DERIVED FROM THE LOG
// (ADR-0012), never published by the writer that decided the record. The code
// log's own fleet-wide consumer ([FeedGroup]) reads every committed record on
// whichever data node wins it; the translator here decides whether a record
// can wake a seat at all, the parser ([Parser]) decides which seats from the
// routing snapshot the record carries ([ChangeRouting]), and the notification
// spine publishes each wake to its seat's inbox. The applier's notice rows
// (apply_notices.go) are the record of whom a record concerned; the wakes are
// the seats among them the company still employs, and nothing else.
//
// # Only seats are woken
//
// A person is never queue-woken: their code inbox is composed from the notice
// rows the applier writes, which is where a person reads. So a record whose
// routing tells only persons — an answer to a person's finding, a merge of a
// person's change — is no delivery at all, and the feed acknowledges it.
//
// # A change's wake names its work item
//
// A change implements one work item (ADR-0067), and its routing carries it, so
// every wake about the change names it ([Prompt.WorkItem]) and the turn the
// wake starts is charged to it (ADR-0022's trigger rule). No change row
// carries spend of its own: spend is the turn's, charged to the item.

// FeedGroup is the code log's wake consumer: the fleet-wide group the code
// feed runs as ([Domain.FeedGroup]) and the one the log's trim waits on.
//
// STABLE FOR THE DEPLOYMENT'S LIFE. The fleet's position on the log IS this
// consumer, and a renamed group is a fresh consumer that starts from the log's
// first retained record: every change the log still holds would come back as a
// wake, which only the change feed's claim and the derived wake id collapse,
// and only inside their own windows. Reserved by the code host's first
// milestone and never renamed.
//
// The group, not the broker's consumer name: the durable consumer is named
// from the group and the stream together (`<group>__CREWLET_CODE_LOG__<digest>`).
const FeedGroup = "crewlet-code-feed"

// RecordID is the identity one record about a change is told by: its operation
// id, else its position on the log — never an id minted by whoever reads it,
// which would differ between two nodes reading one record. ONE RULE for the
// two readers that key on a record: the applier's notice row
// (code_notifications.record_id) and the feed's claim, and through
// [NoticeID] — the record and the change a notice is about — the wake ids
// ([changefeed.WakeID]), so a notice and the wake it records name one record
// and one change.
func RecordID(opID string, at statelog.Position) string {
	if opID != "" {
		return opID
	}
	return at.String()
}

// Translator decides whether a committed code record can wake a seat, and
// hands the parser the whole record.
//
// THE WHOLE RECORD TRAVELS, as the tracker's does: the node that wins a feed
// message is rarely the one running the seat it wakes and is often behind on
// its own rows, and the record carries the routing its writer decided, so the
// parser routes without reading a row.
type Translator struct{}

// NewTranslator builds the code log's feed translator.
func NewTranslator() *Translator { return &Translator{} }

// Source is the estate this translator serves: [Source], the code host's
// surface name every code write delivers to, and [FeedGroup].
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether rec wakes anybody.
//
// # The ways a record wakes nobody, each an ACK
//
// A record on a subject no change is about — a name, an eviction, a
// generation, the company's activation, a person's marks, the barrier and the
// trimmed record — is machinery or bookkeeping, and so is every record of a
// repository's checks but an attempt's ending: its queue, its start, a
// dispatch and a sweep carry no routing, and are acknowledged off the
// envelope, never decoded ([wakesAnybody]). A record this build
// cannot decode past its envelope is a newer build's, whose routing rules it
// does not have. A record about a repository or a change that carries no
// routing is one that concerns nobody: every record from before changes
// existed, a push that moves no change, a compaction. And a record whose
// routing tells only its own actor, or only persons, has no seat to wake.
//
// Each is a DECISION, and a decision is handling: acknowledged rather than
// returned, because a record returned for having nothing to say comes back for
// ever and every wake behind it waits.
func (t *Translator) Translate(_ context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf("code: read the envelope on %s: %w", rec.Key, err)
	}
	if !wakesAnybody(envelope.Subject.Kind, envelope.Op) {
		return changefeed.Delivery{}, false, nil
	}
	record, err := Decode(rec.Payload)
	if err != nil {
		// A RECORD FROM A NEWER BUILD. Its envelope decoded, so this node
		// knows what it is about and that it cannot read its rules — and
		// waking somebody from a payload it cannot decode would be inventing
		// a notification.
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return changefeed.Delivery{}, false, nil
		}
		return changefeed.Delivery{}, false, fmt.Errorf("code: read the record on %s: %w", rec.Key, err)
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		// A PAIR THIS BUILD DOES NOT KNOW at a version it reads: the applier
		// stops on it too, so the feed waits beside it rather than
		// acknowledging a record nobody here has applied.
		return changefeed.Delivery{}, false, fmt.Errorf("code: read the payload on %s: %w", rec.Key, err)
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

// wakesAnybody reports a record, by its envelope, that can carry a routing: a
// repository's (its changes' lifecycle, the pushes that merge or open one, and
// its queues' merges, ejections, landings and broken landings), a change's
// (its conversation and its home's facts), and of a repository's checks an
// attempt's ending alone (a failed check of a change's head:
// CheckFinished.Routing). Every other kind, and every other op of the checks,
// is named by its absence, because a new one routes nobody until its records
// carry a routing and this names it.
//
// BY THE OP TOO for the checks, the one kind most of whose records route
// nobody: every attempt is queued, and every one a runner takes is started,
// and a queued attempt carries its whole definition, so decoding those only
// to find no routing would parse records the checks subject carries for every
// attempt for nothing. An ending is decoded, since whether it failed a head is
// in its payload.
func wakesAnybody(kind ObjectKind, op OpKind) bool {
	switch kind {
	case KindRepo, KindChange:
		return true
	case KindChecks:
		return op == OpCheckFinished
	}
	return false
}

// wakesASeat reports whether concerns tell a seat anything: some handle
// hears ([Concern.Hears]) that is not a person's.
func wakesASeat(concerns []Concern) bool {
	for _, c := range concerns {
		if c.Hears() && !c.Change.Person(c.Handle) {
			return true
		}
	}
	return false
}

// routedActor is who made the change a payload records — the one actor its
// routings share — or "" for a record of several actors (a push record of
// several pushers) or of none (the home's own records).
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

// recordBody renders a record as the map a parser reads, THROUGH THE DOMAIN'S
// OWN ENCODER, so the body is exactly the record — fields this build carries
// and does not understand included.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := Encode(record)
	if err != nil {
		return nil, fmt.Errorf("code: render the record body: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("code: render the record body: %w", err)
	}
	return body, nil
}

// FeedSource opens the code log's own durable consumer — a SECOND consumer
// over the stream the applier reads, at its own position, so a wake is derived
// by something that outlives the writer. A seam the engine fills, because
// which broker the log lives on is the engine's business.
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
		return nil, fmt.Errorf("code: the change feed has no log to consume, so nothing derives a wake " +
			"from a committed record and no seat hears about a change")
	}
	return s.Log.Consume(ctx, group)
}
