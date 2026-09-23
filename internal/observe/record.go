package observe

import (
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// One event, two shapes — and ONE derivation of the fields they share.
//
// A row in the event store and an envelope on a dashboard socket carry the
// same id, type, actor, summary, category and trace. Deriving those twice is
// how a feed comes to disagree with itself across a reload: the live row shows
// what the socket computed, the reloaded row shows what the writer computed,
// and the two were written months apart by different hands.

// Record renders an event as a store row, reporting false for a type that is
// not persisted — see [events.Category] and [events.Unlisted].
//
// THE ROW IS THE STORE'S OWN, built by [store.RecordFor]: which dimensions
// become tags, which spend becomes columns and what an unlisted type's row
// carries are rules about the store's columns, stated once, there. The engine
// writes through this function, so a rule held here as well would be the one
// that ships whatever the store's own said — and the readers that filter and
// tally by those columns live in the store.
//
// A failure to encode reads as "not persisted": the caller is a publish
// listener, which must never fail the publish, and an event that cannot be
// encoded is one the transport has already refused.
func Record(ev *events.Event) (store.EventRecord, bool) {
	rec, persisted, err := store.RecordFor(ev)
	if err != nil || !persisted {
		return store.EventRecord{}, false
	}
	return rec, true
}

// Envelope renders an event for the live projection.
//
// Returns false for an event the projection has no use for: one that is
// neither categorised nor deliberately live-only. Note the asymmetry with
// [Record] — a live-only type has NO category and still produces an envelope,
// which is what lets agent_turn_progress drive a seat's live row without
// joining the persisted feed.
//
// And the asymmetry the other way: an [Unlisted] type is persisted and
// produces NO envelope, having no category and not being live-only. That is
// what keeps a phase record's parts — each up to nearly as large as one event
// — off the projection and off every dashboard socket, which the projection
// alone feeds.
func Envelope(ev *events.Event) (livestate.Envelope, bool) {
	if ev == nil {
		return livestate.Envelope{}, false
	}
	category := Category(ev.Type)
	if category == "" && !LiveOnly(ev.Type) {
		return livestate.Envelope{}, false
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return livestate.Envelope{
		ID:   ev.ID.String(),
		Type: ev.Type,
		// RFC3339Nano because that is what the projection's own stamp
		// parser reads first, and what every other timestamp on this wire
		// serializes to. A different spelling still orders — the parser
		// degrades to lexicographic — but it orders WRONGLY against the
		// ones that parsed.
		Timestamp:    at.Format(time.RFC3339Nano),
		Source:       ev.Source,
		Actor:        ev.Actor(),
		Summary:      ev.Summary(),
		Category:     category,
		TraceID:      ev.TraceID,
		SpanID:       ev.SpanID,
		ParentSpanID: ev.ParentSpanID,
		Topic:        topics.Event(ev.Type),
		Payload:      payloadOf(ev),
	}, true
}

// payloadOf is the event as one flat JSON object — the envelope and the
// payload together, which is what the wire contract says a payload is and what
// the projection's own accessors read `role`, `agent_id` and `turn_id` off.
func payloadOf(ev *events.Event) map[string]any {
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// FeedRow renders a stored event as the activity feed's row, the shape the
// live projection builds from an [Envelope].
//
// Here, beside [Record] and [Envelope], for the reason this file exists: a row
// a restarted process seeds from the store has to read exactly like the row it
// would have built had it seen the event live, or a reload changes the feed a
// reader was looking at. The fields the store keeps are the same derivations
// (the actor, the summary, the category and the failure mark were computed by
// [Record] when the row was written); the TOPIC is the one the store does not
// keep, and it is derived the way each live producer names it.
func FeedRow(rec store.EventRecord) livestate.FeedRow {
	topic := topics.Event(rec.Type)
	if rec.Category == events.WebhookCategory {
		topic = livestate.WebhookTopic(rec.Source)
	}
	return livestate.FeedRow{
		ID: rec.ID, Type: rec.Type,
		// RFC3339Nano in UTC, the spelling [Envelope] gives a live row, so
		// the two order against each other by instant.
		Timestamp: rec.Time.UTC().Format(time.RFC3339Nano),
		Source:    rec.Source, Actor: rec.Actor, Summary: rec.Summary,
		Category: rec.Category, TraceID: rec.TraceID, SpanID: rec.SpanID,
		ParentSpanID: rec.ParentSpanID, Topic: topic,
		Failed: rec.Failed,
	}
}
