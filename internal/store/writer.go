package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Category reports the dashboard category an event type is filed under, and
// whether the type is stored at all.
//
// THE TAXONOMY IS [events]'s, and delegating to it is the point: this was an
// identical map here and another in internal/observe, with nothing asserting
// they agreed — so a type placed in one and forgotten in the other would be
// written and never shown, or shown and never written, and no test anywhere
// could see it. internal/observe imports this package, so neither could import
// the other; the one map lives in the package that owns the type registry.
//
// An absent type is NOT WRITTEN, and that is deliberate for three types and a
// hazard for every other one: the sandbox panel once drew rows that vanished on
// reload and 404'd when clicked, because its events reached the live stream and
// never the store. See events.Exclusions for which three, and why.
func Category(eventType string) (string, bool) { return events.Category(eventType) }

// tagKeys are the flat JSON fields promoted out of an event into its tags.
//
// Reading them from the event's own JSON rather than from typed struct fields
// is what keeps this list independent of the event catalogue: an event type
// this build has never heard of still arrives with its fields intact in the
// envelope, so a newer node's events are indexed here exactly as a known one's
// are. A writer that reaches through the decoded payload instead sees nothing
// at all on an unknown type.
//
// The value is the tag name; the key is the JSON field it comes from. They
// differ in one place — `role` on the event is `agent_role` in the tags,
// because that is the name every filter and index uses.
var tagKeys = map[string]string{
	"agent_id":         "agent_id",
	"role":             "agent_role",
	"task_id":          "task_id",
	"channel_id":       "channel_id",
	"sender":           "sender",
	"conversation_key": "conversation_key",
	"requester":        "requester",
	"target":           "target",
	"recipient":        "recipient",
	"closed_by":        "closed_by",
	// Which VENDOR a notification event concerns. A tag rather than a
	// payload read for the same reason as `failed` below: a listing
	// deliberately never selects the payload column, so the Integrations
	// room aggregating "how many of this vendor's deliveries were dropped
	// by the routing gate" has no other way to read it. Rows written
	// before this tag existed read back without it — a real discontinuity
	// at that point in the timeline, not a bug to paper over.
	"notification_source": "notification_source",
}

// RecordFor builds the stored form of an event, reporting false when the event
// is not one this store keeps (see [Category]).
//
// Pure: it touches no database, so the mapping is testable on its own.
func RecordFor(ev *events.Event) (EventRecord, bool, error) {
	if ev == nil {
		return EventRecord{}, false, nil
	}
	category, tracked := events.Category(ev.Type)
	if !tracked {
		return EventRecord{}, false, nil
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return EventRecord{}, false, fmt.Errorf("store: encode event %s: %w", ev.ID, err)
	}
	return EventRecord{
		ID:           ev.ID.String(),
		Type:         ev.Type,
		Source:       ev.Source,
		Time:         ev.Timestamp,
		Category:     category,
		Summary:      ev.Summary(),
		Actor:        ev.Actor(),
		TraceID:      ev.TraceID,
		SpanID:       ev.SpanID,
		ParentSpanID: ev.ParentSpanID,
		Tags:         extractTags(payload),
		Spend:        extractSpend(ev.Type, payload),
		Payload:      payload,
	}, true, nil
}

// spendEventType is the one event that carries an LLM call's cost.
//
// Gated on the type rather than on "does the payload happen to have these
// fields", because several other events carry a `model` or a `turn_id` and a
// rollup that counted them would be counting calls that never happened.
const spendEventType = "agent_phase_completed"

// extractSpend pulls one LLM call's cost out of a phase completion.
//
// Read from the event's serialized form for the same reason [extractTags] is:
// an event type this build has never heard of still arrives with its fields
// intact in the envelope, so a newer node's phase completions are recorded
// here exactly as a known one's are. Reaching through the decoded payload
// instead would see nothing at all on an unknown type.
//
// Nil for every other event, which is what leaves the promoted columns at
// their defaults — see schema/0015 for why they are columns.
func extractSpend(eventType string, payload []byte) *Spend {
	if eventType != spendEventType {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		// The call happened, and dropping it because its payload would
		// not decode understates the spend this exists to report.
		return &Spend{}
	}
	spend := &Spend{
		Phase:        payloadString(body, "phase"),
		HostPhase:    payloadString(body, "host_phase"),
		Worker:       payloadString(body, "worker"),
		Model:        payloadString(body, "model"),
		TurnID:       payloadString(body, "turn_id"),
		Iteration:    payloadInt(body, "iteration"),
		InputTokens:  payloadInt(body, "input_tokens"),
		OutputTokens: payloadInt(body, "output_tokens"),
		TotalTokens:  payloadInt(body, "total_tokens"),
	}
	if spend.Model == "" {
		// An entry that names no model is identified by the provider
		// slot it ran on. The backfill in schema/0015 does the same, so
		// history and new rows agree on what "model" means.
		spend.Model = payloadString(body, "provider_key")
	}
	return spend
}

// Record writes an event to the log, skipping types the store does not keep.
//
// Errors are returned rather than swallowed, but the caller is a publish
// listener and must treat them as fire-and-forget: an observability write that
// fails must not disrupt the event pipeline that produced it.
func (l *EventLog) Record(ctx context.Context, ev *events.Event) error {
	rec, tracked, err := RecordFor(ev)
	if err != nil {
		return err
	}
	if !tracked {
		return nil
	}
	return l.Append(ctx, rec)
}

// extractTags pulls the filterable dimensions out of an event's serialized
// form.
//
// "Which agent does this event concern" is a RULE, not a field, and one copy
// of it is all this codebase should have — which is why it lives beside the
// columns it feeds rather than being re-derived by each reader downstream.
func extractTags(payload []byte) map[string]string {
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(payload, &flat); err != nil {
		return map[string]string{}
	}
	tags := map[string]string{}
	for field, tag := range tagKeys {
		if s := jsonString(flat[field]); s != "" {
			tags[tag] = s
		}
	}
	// Whether the work this event reports failed. A tag rather than a
	// payload read because a listing deliberately never selects the payload
	// column, so a dashboard hydrating its feed from history has no other
	// way to know a phase died — and a feed that renders a failed turn
	// identically to a successful one is the bug this dimension closes.
	// Only set when true, so the tag doubles as a filter.
	var failed bool
	if raw, ok := flat["failed"]; ok {
		_ = json.Unmarshal(raw, &failed)
	}
	if failed {
		tags["failed"] = "true"
	}
	// Turns triggered by A2A carry their channel one level down, so the
	// cross-reference from a turn back to the conversation that caused it
	// needs this one nested read.
	if raw, ok := flat["a2a_context"]; ok {
		var ctxObj struct {
			ChannelID string `json:"channel_id"`
		}
		if json.Unmarshal(raw, &ctxObj) == nil && ctxObj.ChannelID != "" {
			tags["a2a_channel_id"] = ctxObj.ChannelID
		}
	}
	return tags
}

// jsonString reads a JSON value as a string, yielding "" for anything that is
// not one — including absent, null, and a number that happens to sit in a
// field a tag names.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
