package store

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Category reports the dashboard category an event type is filed under, and
// whether it has one.
//
// THE TAXONOMY IS [events]'s, and delegating to it is the point: one map, in
// the package that owns the type registry, read by this package and by
// internal/observe alike, so a type cannot be written here and filed under
// something else there.
//
// A type with no category is NOT WRITTEN unless [events.Unlisted] names it,
// and that is deliberate for the types events.Exclusions names and a hazard
// for every other one: the sandbox panel once drew rows that vanished on
// reload and 404'd when clicked, because its events reached the live stream
// and never the store.
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
	"agent_id":   "agent_id",
	"role":       "agent_role",
	"task_id":    "task_id",
	"channel_id": "channel_id",
	"sender":     "sender",
	// Which conversation (a Slack thread, a Jira issue, a PR) the event
	// belongs to. channel_id does not cover it: that is set on A2A events
	// alone and never on the phase records that carry the model's
	// reasoning, so without this no query can ask history for one
	// thread's turns.
	"conversation_key": "conversation_key",
	// The two turn identities, which [EventLog.Append] also reads back out
	// of the tags into their columns. turn_id names ONE RUN — every phase
	// record, the turn's own completion, a fallback, a breach — and a trace
	// is no substitute for it: one trace can span several turns, and a turn
	// resumed after a restart several traces. work_key names the unit of
	// work behind the run, which a redelivered trigger's second run repeats
	// (ADR-0017).
	"turn_id":  "turn_id",
	"work_key": "work_key",
	// A2A participants, for cross-referencing a channel's traffic.
	"requester": "requester",
	"target":    "target",
	"recipient": "recipient",
	"closed_by": "closed_by",
	// Which THIRD-PARTY APP a notification event concerns. A tag rather than a
	// payload read for the same reason as `failed` below: a listing
	// deliberately never selects the payload column, so the Integrations
	// room aggregating "how many of this third-party app's deliveries were
	// dropped by the routing gate" has no other way to read it. Rows written
	// before this tag existed read back without it — a real discontinuity
	// at that point in the timeline, not a bug to paper over.
	"notification_source": "notification_source",
}

// RecordFor builds the stored form of an event, reporting false when the event
// is not one this store keeps (see [Category] and [events.Unlisted]).
//
// THE ONE BUILDER OF A ROW, and internal/observe's writer — the engine's only
// production writer — builds through it: which dimensions become tags, which
// spend becomes columns and what an unlisted type's row carries are rules
// about this store's columns, so they are stated beside them, once.
//
// Pure apart from the clock: it touches no database, so the mapping is
// testable on its own.
//
// A ZERO TIMESTAMP IS STAMPED NOW. Year one is permanently below every read
// floor, so the row would exist and no query would return it, and [EventLog.Append]
// refuses one outright; stamping it keeps the event, one write late rather
// than lost.
func RecordFor(ev *events.Event) (EventRecord, bool, error) {
	if ev == nil {
		return EventRecord{}, false, nil
	}
	category, listed := events.Category(ev.Type)
	unlisted := events.Unlisted(ev.Type) != ""
	if !listed && !unlisted {
		return EventRecord{}, false, nil
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return EventRecord{}, false, fmt.Errorf("store: encode event %s: %w", ev.ID, err)
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = now()
	}
	if unlisted {
		// STORAGE FOR ANOTHER ROW, NOT AN EVENT: its identity, its bytes and
		// a summary line for the one kind of read that reaches it — a point
		// read by its id — and NOTHING a listing filters on. It names no
		// trace, turn, seat or party, so a read keyed on any of them cannot
		// reach it and no party row is written for it; the unkeyed listings
		// refuse its type by name (see [ListQuery.predicate]).
		return EventRecord{
			ID: ev.ID.String(), Type: ev.Type, Time: at,
			Summary: ev.Summary(), Payload: payload,
		}, true, nil
	}
	// ONE shallow decode, for the tags and the spend alike: a phase
	// completion carries the phase's whole prompt and tool log, and this
	// runs on the publishing goroutine of every event the engine keeps.
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(payload, &flat); err != nil {
		flat = nil
	}
	return EventRecord{
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
		Tags:         tagsOf(flat),
		Spend:        spendOf(ev.Type, flat, flat != nil),
		Payload:      payload,
	}, true, nil
}

// spendEventType is the one event that carries an LLM call's cost.
//
// Gated on the type rather than on "does the payload happen to have these
// fields", because several other events carry a `model` or a `turn_id` and a
// rollup that counted them would be counting calls that never happened.
const spendEventType = "agent_phase_completed"

// SpendFor pulls one LLM call's cost out of a phase completion.
//
// Read from the event's serialized form for the same reason [tagsOf] is: an
// event type this build has never heard of still arrives with its fields
// intact in the envelope, so a newer node's phase completions are recorded
// here exactly as a known one's are. Reaching through the decoded payload
// instead would see nothing at all on an unknown type.
//
// Nil for every other event, which is what leaves the promoted columns at
// their defaults — see schema/0015 for why they are columns.
// It reads the SHALLOW form: nine scalars are wanted, and decoding into
// map[string]any would deep-decode the engine's largest payload — a phase
// completion carries the phase's whole prompt and tool log.
// map[string]json.RawMessage leaves everything it is not asked for as bytes.
//
// A struct decode would be shorter and is wrong here for the reason the
// per-field accessors exist: it fails the whole call on one wrong-typed
// field, where these zero only the offender.
func SpendFor(eventType string, payload []byte) *Spend {
	if eventType != spendEventType {
		return nil
	}
	var body map[string]json.RawMessage
	err := json.Unmarshal(payload, &body)
	return spendOf(eventType, body, err == nil)
}

// spendOf is [SpendFor] over a body already decoded. decoded is false when the
// payload would not decode, which still answers a spend: the call happened,
// and dropping it because its payload would not decode understates the spend
// this exists to report.
func spendOf(eventType string, body map[string]json.RawMessage, decoded bool) *Spend {
	if eventType != spendEventType {
		return nil
	}
	if !decoded {
		return &Spend{}
	}
	spend := &Spend{
		Phase:        jsonString(body["phase"]),
		HostPhase:    jsonString(body["host_phase"]),
		Worker:       jsonString(body["worker"]),
		Model:        jsonString(body["model"]),
		TurnID:       jsonString(body["turn_id"]),
		WorkKey:      jsonString(body["work_key"]),
		Iteration:    jsonInt(body["iteration"]),
		InputTokens:  jsonInt(body["input_tokens"]),
		OutputTokens: jsonInt(body["output_tokens"]),
		TotalTokens:  jsonInt(body["total_tokens"]),
	}
	if spend.Model == "" {
		// An entry that names no model is identified by the provider
		// slot it ran on. The backfill in schema/0015 does the same, so
		// history and new rows agree on what "model" means.
		spend.Model = jsonString(body["provider_key"])
	}
	return spend
}

// tagsOf pulls the filterable dimensions out of an event's serialized form,
// decoded shallowly — the one derivation of them, see [RecordFor].
//
// "Which agent does this event concern" is a RULE, not a field, and one copy
// of it is all this codebase should have — which is why it lives beside the
// columns it feeds rather than being re-derived by each reader downstream.
// A body that would not decode yields no tags rather than no row.
func tagsOf(flat map[string]json.RawMessage) map[string]string {
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

// jsonInt reads a number out of a raw JSON field.
//
// json.Number rather than float64, so a token count past 2^53 is not silently
// rounded on its way into a column an operator bills from.
func jsonInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	v, err := n.Int64()
	if err != nil {
		return 0
	}
	return int(v)
}

// jsonString reads a JSON value as a string, yielding "" for anything that is
// not one: absent, null, and a number that happens to sit in a field a tag
// names.
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
