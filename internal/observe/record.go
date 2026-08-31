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

// Tags are an event's filterable dimensions.
//
// Computed here rather than read out of the payload downstream because "which
// agent does this event concern" is a RULE, not a field — and one copy of it
// is all this codebase should have. They matter beyond convenience: a listing
// never selects the payload column, so a dimension that is not a tag cannot be
// filtered on at all once the event is history.
func Tags(ev *events.Event) map[string]string {
	_, fields := body(ev)
	return tagsFrom(fields)
}

// taggedKeys map a WIRE field name onto the tag it becomes.
//
// Wire names, read off the event's own JSON, rather than Go fields read by
// reflection: these are ordinary values on a dozen unrelated payloads, an
// interface each would be a dozen one-method interfaces implemented once, and
// reflection would key the tags on Go identifiers — so a payload that renamed
// a json tag would silently stop being filterable while still compiling.
//
// Only `role` is renamed, to `agent_role`, because "role" alone is ambiguous
// beside the org's own notion of one.
var taggedKeys = map[string]string{
	"agent_id":   "agent_id",
	"role":       "agent_role",
	"task_id":    "task_id",
	"channel_id": "channel_id",
	"sender":     "sender",
	// The identity of one unit of agent work. Almost every event a turn
	// publishes carries it — each phase record, the turn's own completion,
	// a provider fallback, a guard breach, a sandbox suspend and its
	// resume — and until it was promoted none of them could be found BY
	// it, so "everything that happened in this turn" was a question with
	// no answer. A trace is NOT a substitute: one trace can span several
	// turns, and a turn resumed after a restart can span several traces.
	"turn_id": "turn_id",
	// Which conversation (a Slack thread, a Jira issue, a PR) the event
	// belongs to. channel_id does NOT cover it: that is set on A2A events
	// alone and never on the phase records that carry the model's
	// reasoning, so without this no query can ask history for one thread's
	// turns.
	"conversation_key": "conversation_key",
	// A2A participants, for cross-referencing a channel's traffic.
	"requester": "requester",
	"target":    "target",
	"recipient": "recipient",
	"closed_by": "closed_by",
}

func tagsFrom(body map[string]any) map[string]string {
	if body == nil {
		return nil
	}
	tags := map[string]string{}
	for key, tag := range taggedKeys {
		if s, ok := body[key].(string); ok && s != "" {
			tags[tag] = s
		}
	}
	// An A2A-triggered turn is tagged with the channel that woke it, which
	// is nested rather than top-level.
	if ctx, ok := body["a2a_context"].(map[string]any); ok {
		if id, ok := ctx["channel_id"].(string); ok && id != "" {
			tags["a2a_channel_id"] = id
		}
	}
	// Whether the work this event reports actually FAILED. A tag rather
	// than a payload read for the same reason as the rest: a feed
	// hydrating from history never sees the payload, and one that renders
	// a failed turn identically to a successful one is the whole problem
	// this dimension exists to close. Set only when true, so it doubles as
	// a filter.
	if failed, ok := body["failed"].(bool); ok && failed {
		tags["failed"] = "true"
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// body is the event as one flat JSON object — the envelope and the payload
// together, which is what the wire contract says a payload is and what
// the projection's own accessors read `role`, `agent_id` and `turn_id` off.
//
// Marshalled ONCE per event and threaded through, because this is on the
// publish path of every event the engine produces: the first version of this
// file serialized three times to answer three questions about one event.
//
// It returns the BYTES as well as the map, because the bytes are what the
// store row needs and this function already had them. Discarding them and
// having the caller re-encode the map made the "marshalled once" above false
// for the one caller that matters — every persisted event was serialized
// twice on the publishing goroutine, the second time from a map that had just
// been decoded from the first.
//
// It also makes the stored payload byte-identical to what was published,
// which is what a reader opening a row expects and what the doc above already
// implies.
func body(ev *events.Event) ([]byte, map[string]any) {
	if ev == nil {
		return nil, nil
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, nil
	}
	return raw, out
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
	raw, fields := body(ev)
	if fields == nil {
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
		Tags:         tagsFrom(fields),
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

// payloadOf is the projection's half of [body], which wants the map alone.
func payloadOf(ev *events.Event) map[string]any {
	_, fields := body(ev)
	return fields
}
