package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// eventCategory maps an event type onto the category the dashboard's filters
// group by.
//
// It is also the ADMISSION LIST for the event log: a type absent here is not
// written. That is deliberate for two types and a hazard for every other one —
// the sandbox panel once drew rows that vanished on reload and 404'd when
// clicked, because its events reached the live stream and never the store.
// Every event type the engine publishes belongs here.
//
// The three deliberate absences:
//
//   - agent_turn_progress fires once per LLM round as a live-only signal;
//     the matching agent_phase_completed is its durable record.
//   - budget_reported is a snapshot of LIVE, in-memory meters whose values
//     mean nothing outside the run that produced them. Persisting it would
//     let a dashboard hydrate a dead process's counters and render them as
//     the current ones — a number that is not merely stale but describes a
//     different run.
//   - raw_webhook is the API's inbound edge waking a transport. The edge has
//     ALREADY written the delivery's row itself, carrying the provider's exact
//     bytes under category "webhook" — so admitting the queue envelope too
//     would store every inbound delivery twice, once as what arrived and once
//     as what was forwarded.
var eventCategory = map[string]string{
	"org_started":      "lifecycle",
	"org_stopped":      "lifecycle",
	"agent_spawned":    "lifecycle",
	"agent_terminated": "lifecycle",
	"agent_reassigned": "lifecycle",
	"role_updated":     "lifecycle",

	"task_created":   "task",
	"task_assigned":  "task",
	"task_started":   "task",
	"task_completed": "task",
	"task_failed":    "task",
	"task_delegated": "task",

	"message_sent": "communication",

	"a2a_channel_opened":    "a2a",
	"a2a_message_sent":      "a2a",
	"a2a_message_delivered": "a2a",
	"a2a_channel_closed":    "a2a",

	// DACI is behavioural guidance carried on the org's own chat surfaces,
	// not an engine subsystem — nothing in Crewlet publishes these four.
	// They stay mapped as the seam an extension that DOES model decisions
	// writes through, and they are why the dashboard has a decision
	// category to filter on.
	"decision_requested":     "decision",
	"decision_resolved":      "decision",
	"contribution_requested": "decision",
	"contribution_received":  "decision",

	"document_created": "knowledge",
	"document_updated": "knowledge",

	"external_notification": "notification",
	"notification_skipped":  "notification",
	// N same-conversation events merged into one digest trigger. The event
	// exists FOR the store and the dashboard — operators watch when and how
	// hard batching kicks in.
	"notifications_coalesced": "notification",
	// A redelivered trigger the completion ledger short-circuited. The
	// whole point of emitting it is that a skipped trigger should not be
	// invisible.
	"turn_trigger_skipped": "notification",

	"budget_exhausted":             "system",
	"llm_unavailable":              "system",
	"agent_turn_completed":         "system",
	"agent_phase_started":          "system",
	"agent_phase_completed":        "system",
	"execute.missing_tool":         "system",
	"phase.tool_activated":         "system",
	"prompt.size":                  "system",
	"turn.guard_breach":            "system",
	"provider_fallback":            "system",
	"phase.tool_skill_blocked":     "system",
	"skill_telemetry_write_failed": "system",
	"subagent_batched":             "system",

	// The learning subsystem, all under one category so a dashboard filter
	// can include or exclude reflection traffic with one toggle.
	"turn_completed":               "learning",
	"episode_written":              "learning",
	"persist_decider_completed":    "learning",
	"counterparty_profile_updated": "learning",
	"skill_synthesized":            "learning",
	"skill_refined":                "learning",
	"skill_promoted":               "learning",
	"reflection_completed":         "learning",
	"plan_prefetch_summary":        "learning",
	"relevant_knowledge_refetched": "learning",
	"skill_used":                   "learning",
	"skill_staled":                 "learning",
	"skill_archived":               "learning",
	"skill_revived":                "learning",
	"compaction_requested":         "learning",
	"compaction_completed":         "learning",

	// Detached sandbox runs are the execution of a task.
	"sandbox_run_started":             "task",
	"sandbox_clarification_requested": "task",
	"sandbox_run_completed":           "task",
	// A schedule firing creates work; same category as the assignment it
	// produces.
	"scheduled_task_fired": "task",

	// Configuration changes are org lifecycle, and the class of event an
	// operator is most likely to go looking for after the fact.
	"config_revision_activated": "lifecycle",
	"config_revision_applied":   "lifecycle",
}

// Category reports the dashboard category an event type is filed under, and
// whether the type is stored at all. See eventCategory.
func Category(eventType string) (string, bool) {
	c, ok := eventCategory[eventType]
	return c, ok
}

// tagKeys are the flat JSON fields promoted out of an event into its tags.
//
// Reading them from the event's own JSON rather than from typed struct fields
// is what keeps this list independent of the event catalogue: an event type
// this build has never heard of still arrives with its fields intact in the
// envelope, so a newer node's events are indexed here exactly as a known one's
// are. The Python writer used attribute lookups and therefore saw nothing at
// all on an unknown type.
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
// is not one this store keeps (see eventCategory).
//
// Pure: it touches no database, so the mapping is testable on its own.
func RecordFor(ev *events.Event) (EventRecord, bool, error) {
	if ev == nil {
		return EventRecord{}, false, nil
	}
	category, tracked := eventCategory[ev.Type]
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
		Payload:      payload,
	}, true, nil
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
