package store

import (
	"encoding/json"

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
// THE ONE LIST, and the reason it is here rather than beside the publish
// listener that uses it is the reason [Category] delegates to internal/events:
// this was a map here and another in internal/observe, with nothing asserting
// they agreed — so a dimension added to one and forgotten in the other was
// written by nobody or read by nobody, and no test anywhere could see it.
// That is not hypothetical. `notification_source` lived only here, read only
// by a mapping function in this package that had no production caller —
// observe.Record is the writer — so the tag the Integrations room counts its
// merges and drops by was never written at all, and every one of those counts
// read zero on a company whose third-party apps were delivering fine. That
// function is gone; internal/observe imports this package, so it calls
// [ExtractTags] rather than keeping a second opinion.
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
	// The identity of one unit of agent work. Almost every event a turn
	// publishes carries it — each phase record, the turn's own completion,
	// a provider fallback, a guard breach, a sandbox suspend and its
	// resume — and until it was promoted none of them could be found BY
	// it, so "everything that happened in this turn" was a question with
	// no answer. A trace is NOT a substitute: one trace can span several
	// turns, and a turn resumed after a restart can span several traces.
	// [EventLog.Append] reads it back out of here for the turn_id column,
	// and it was MISSING here while internal/observe's copy of this map
	// carried it — so this mapping and the one the engine actually wrote
	// through disagreed about a dimension Append reads back out of the
	// tags. One map is what makes that disagreement unrepresentable.
	"turn_id": "turn_id",
	// The unit of work behind that run: turn_id names one EXECUTION, which
	// is what a phase row, a live call and the turns list are keyed on, and
	// a trigger that fails without acting is redelivered — so one unit of
	// work legitimately produces several. This is what still groups them,
	// and the only identity a re-run reproduces. See ADR-0017.
	"work_key": "work_key",
	// WHICH CONVERSATION — a chat thread, a work item, a page — the event
	// belongs to. channel_id does NOT cover it: that is set on A2A events
	// alone and never on the phase records that carry the model's
	// reasoning, so without this no query can ask history for one thread's
	// turns.
	//
	// ONE MEANING ACROSS EVERY EVENT TYPE, which is a property of the wire
	// field rather than of this line: the tag is the conversation IDENTITY
	// because every payload that spells a field `conversation_key` holds
	// the identity. The coalescing record used to promote its inbox
	// PARTITION through it and now names that `partition_key` below, so a
	// filter on this tag cannot mean the thread a seat is talking on for
	// one row and the batch a wake arrived in for the next — two values
	// that differ exactly where it matters, since a direct message's
	// identity is the bare channel and its partition can be a thread
	// inside it. A tag whose meaning depends on the row is worse than an
	// absent one: the query still answers.
	"conversation_key": "conversation_key",
	// WHICH INBOX PARTITION a coalescing record merged.
	//
	// ITS OWN TAG rather than nothing, because the partition is the
	// subject of the only event that carries it — "N deliveries became one
	// turn" is a fact about a batch — and an operator watching how hard
	// batching is kicking in asks it per line: which thread, which DM,
	// which issue is arriving faster than its seat can answer. A listing
	// deliberately never selects the payload column, so without a tag that
	// question is unaskable of history and the panel is left with a count
	// per third-party app. Its own tag rather than the conversation's for
	// the reason stated there: on a direct message the two differ, and one
	// tag meaning either is a dimension that answers neither question.
	"partition_key": "partition_key",
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

// spendEventType is the one event that carries an LLM call's cost.
//
// Gated on the type rather than on "does the payload happen to have these
// fields", because several other events carry a `model` or a `turn_id` and a
// rollup that counted them would be counting calls that never happened.
const spendEventType = "agent_phase_completed"

// SpendFor pulls one LLM call's cost out of a phase completion.
//
// Read from the event's serialized form for the same reason [extractTags] is:
// an event type this build has never heard of still arrives with its fields
// intact in the envelope, so a newer node's phase completions are recorded
// here exactly as a known one's are. Reaching through the decoded payload
// instead would see nothing at all on an unknown type.
//
// Nil for every other event, which is what leaves the promoted columns at
// their defaults — see schema/0015 for why they are columns.
// It reads the SHALLOW form, like [extractTags] fifty lines below and unlike
// the version this replaces: thirteen scalars are wanted, and decoding into
// map[string]any deep-decoded the engine's largest payload — a phase
// completion carries the phase's whole prompt and tool log — on the
// publishing goroutine of every LLM call. map[string]json.RawMessage leaves
// everything it is not asked for as bytes.
//
// A struct decode would be shorter and is wrong here for the reason the
// per-field accessors exist: it fails the whole call on one wrong-typed
// field, where these zero only the offender.
func SpendFor(eventType string, payload []byte) *Spend {
	if eventType != spendEventType {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		// The call happened, and dropping it because its payload would
		// not decode understates the spend this exists to report.
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

		CacheReadTokens:  jsonInt(body["cache_read_tokens"]),
		CacheWriteTokens: jsonInt(body["cache_write_tokens"]),
		ProviderKey:      jsonString(body["provider_key"]),
	}
	if spend.Model == "" {
		// An entry that names no model is identified by the provider
		// slot it ran on. The backfill in schema/0015 does the same, so
		// history and new rows agree on what "model" means.
		spend.Model = jsonString(body["provider_key"])
	}
	return spend
}

// ExtractTags pulls the filterable dimensions out of an event's serialized
// form.
//
// "Which agent does this event concern" is a RULE, not a field, and one copy
// of it is all this codebase should have — which is why it lives beside the
// columns it feeds rather than being re-derived by each reader downstream.
// Exported for internal/observe, which is the publish listener that actually
// writes the rows; see [tagKeys] for what a second copy of this cost.
//
// It takes the SERIALIZED event rather than a decoded map so the caller on
// the publishing goroutine pays a shallow decode: map[string]json.RawMessage
// leaves everything no tag names as bytes, where map[string]any deep-decodes
// the engine's largest payload — a phase completion carries the whole prompt
// and tool log — on every LLM call.
func ExtractTags(payload []byte) map[string]string {
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
