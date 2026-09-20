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

// THE TAGS ARE [store.ExtractTags]'s, and that is the point.
//
// "Which agent does this event concern" is a RULE, not a field, and one copy
// of it is all this codebase should have. This file used to hold a second
// one — a map of wire field to tag name, beside the store's own, with nothing
// asserting they agreed — and both halves of the divergence were live: the
// store's list had `notification_source` and this one did not, so the tag the
// Integrations room counts merges and drops by reached no row and every one
// of those counts read zero; this one had `turn_id` and the store's did not,
// so the one list that WAS exercised by a test was the one production never
// called. This package imports the store, so it asks rather than answers.
//
// It is handed the BYTES it already marshalled, not a decoded map, so the
// derivation is a shallow decode on the publishing goroutine — see
// [store.ExtractTags].

// encode is the event as the bytes the store row carries.
//
// Marshalled ONCE per event and threaded through, because this is on the
// publish path of every event the engine produces: the first version of this
// file serialized three times to answer three questions about one event.
//
// It also makes the stored payload byte-identical to what was published,
// which is what a reader opening a row expects.
func encode(ev *events.Event) []byte {
	if ev == nil {
		return nil
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return raw
}

// Record renders an event as a store row, reporting false for a type that is
// not persisted — see [categories].
func Record(ev *events.Event) (store.EventRecord, bool) {
	if ev == nil {
		return store.EventRecord{}, false
	}
	category := Category(ev.Type)
	if category == "" {
		return store.EventRecord{}, false
	}
	// The WHOLE event, not the payload: a reader opening one row expects
	// the envelope's trace ids and delegation chain beside the body, and
	// re-assembling them from columns would be a second serialization of
	// something the event already knows how to write.
	raw := encode(ev)
	if raw == nil {
		return store.EventRecord{}, false
	}
	at := ev.Timestamp
	if at.IsZero() {
		// A zero time lands in year 1, permanently below every read
		// floor: the row exists and no query returns it. The store
		// refuses it outright, which would drop the event; stamping it
		// now keeps it, one write late rather than lost.
		at = time.Now().UTC()
	}
	return store.EventRecord{
		// SET HERE, from the bytes body already produced. The store
		// derives it when a caller does not, and this is the one
		// production caller — so leaving it nil made that fallback the
		// only branch ever taken, re-decoding the engine's largest
		// payload on the publishing goroutine of every LLM call.
		Spend:        store.SpendFor(ev.Type, raw),
		ID:           ev.ID.String(),
		Type:         ev.Type,
		Source:       ev.Source,
		Time:         at,
		Category:     category,
		Summary:      ev.Summary(),
		Actor:        ev.Actor(),
		TraceID:      ev.TraceID,
		SpanID:       ev.SpanID,
		ParentSpanID: ev.ParentSpanID,
		Tags:         store.ExtractTags(raw),
		Payload:      raw,
	}, true
}

// Envelope renders an event for the live projection.
//
// Returns false for an event the projection has no use for: one that is
// neither categorised nor deliberately live-only. Note the asymmetry with
// [Record] — a live-only type has NO category and still produces an envelope,
// which is what lets agent_turn_progress drive a seat's live row without
// joining the persisted feed.
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

// payloadOf is the projection's half: the event as one flat JSON object — the
// envelope and the payload together, which is what the wire contract says a
// payload is and what the projection's own accessors read `role`, `agent_id`
// and `turn_id` off.
//
// The DEEP decode lives here rather than on [Record]'s path because only the
// live envelope needs the map: a store row wants the bytes and a shallow read
// of the fields a tag names, and deep-decoding a phase completion's whole
// prompt and tool log to fill a handful of tags was that cost paid on every
// LLM call.
func payloadOf(ev *events.Event) map[string]any {
	raw := encode(ev)
	if raw == nil {
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
