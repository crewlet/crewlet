package artifactsdomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// The artifacts feed derives a comment's wake from the committed record (ADR-0012):
// the translator relays a record whose routing snapshot wakes an agent, and the
// parser wakes those agents. People are told, never queue-woken.

// Source is the notification source the artifacts log's wakes are delivered
// under, and the surface a tool that answers one on the artifact delivers to.
// Bare, as the tracker's `work` is.
const Source = "artifact"

// FeedGroup is the artifacts log's wake consumer, which the trim waits on.
// Stable for the deployment's life: a renamed group replays every retained
// record as a wake.
const FeedGroup = "crewlet-artifacts-feed"

// Translator decides whether a committed artifacts record can wake a seat, and
// hands the parser the whole record, which carries the routing its writer
// decided, so the parser routes without reading a row.
type Translator struct{}

// NewTranslator builds the artifacts log's feed translator.
func NewTranslator() *Translator { return &Translator{} }

// Source is the estate this translator serves: [Source] and [FeedGroup].
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: FeedGroup}
}

// Translate decides whether rec wakes anybody: only a comment record whose
// snapshot names an agent does. A newer build's record is deferred to a peer
// ([changefeed.Defer]), never acknowledged unread (#838).
func (t *Translator) Translate(_ context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	envelope, err := DecodeEnvelope(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf("artifactsdomain: read the envelope on %s: %w",
			rec.Key, err)
	}
	if !wakesAnybody(envelope.Subject.Kind, envelope.Op) {
		return changefeed.Delivery{}, false, nil
	}
	record, err := Decode(rec.Payload)
	if err != nil {
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return changefeed.Delivery{}, false, changefeed.Defer(changefeed.DeferNewerRecord, err)
		}
		return changefeed.Delivery{}, false, fmt.Errorf("artifactsdomain: read the record on %s: %w",
			rec.Key, err)
	}
	notify, err := notifyOfRecord(record)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf("artifactsdomain: read the payload on %s: %w",
			rec.Key, err)
	}
	if notify == nil || len(notify.Agents) == 0 {
		return changefeed.Delivery{}, false, nil
	}
	body, err := recordBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	return changefeed.Delivery{Body: body, ID: record.OpID, Actor: record.Actor}, true, nil
}

// wakesAnybody reports a record, by its envelope, whose payload can carry a
// routing: a comment's open, reply, resolve or reopen.
func wakesAnybody(kind ObjectKind, op OpKind) bool {
	switch op {
	case OpCommentOpen, OpCommentReply, OpCommentResolve, OpCommentReopen:
		return kind == KindComment
	}
	return false
}

// notifyOfRecord is the routing snapshot a comment record carries, nil for
// one that carries none.
func notifyOfRecord(record MutationRecord) (*CommentNotify, error) {
	payload, err := DecodeMutation(record)
	if err != nil {
		return nil, err
	}
	switch p := payload.(type) {
	case CommentOpenPayload:
		return p.Notify, nil
	case CommentReplyPayload:
		return p.Notify, nil
	case CommentStatusPayload:
		return p.Notify, nil
	}
	return nil, nil
}

// recordBody renders a record as the map a parser reads, through the domain's
// own encoder, so the body is exactly the record, carried unknown fields
// included.
func recordBody(record MutationRecord) (map[string]any, error) {
	data, err := Encode(record)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: render the record body: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("artifactsdomain: render the record body: %w", err)
	}
	return body, nil
}

// FeedSource opens the artifacts log's own durable consumer, a second consumer
// over the stream the applier reads. A seam the engine fills, because which
// broker the log lives on is the engine's business.
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
		return nil, errors.New("artifactsdomain: the change feed has no log to consume, so " +
			"nothing derives a wake from a committed comment and no seat hears of one")
	}
	return s.Log.Consume(ctx, group)
}
