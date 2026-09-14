// The event-type taxonomy: which category each type is filed under, and which
// types are deliberately kept out of the durable log.
//
// IT LIVES HERE, in the package that owns the type registry, because two
// packages consume it and neither may import the other: internal/store writes
// the row and internal/observe decides what reaches the activity feed, and
// internal/observe already imports internal/store. It was two identical maps
// in those two packages, with nothing asserting they agreed — so a type placed
// in one and forgotten in the other would be written to the store and never
// shown, or shown and never written, with no test anywhere able to see it.

package events

import (
	"maps"
	"slices"
)

// categories maps an event type to its dashboard category.
//
// THE MAP IS THE ADMISSION LIST. A type that is not here is not written to the
// event store and does not reach the activity feed — the live projection keys
// "is this a persisted event" on the category being non-empty, so an absent
// entry is a silent drop in both places at once. That is how the sandbox panel
// once ended up showing rows that vanished on the next reload and 404'd when
// clicked.
//
// So the exclusions below are deliberate, and stated, rather than left as gaps.
var categories = map[string]string{
	// Lifecycle: the org and its seats coming and going, plus the config
	// changes an operator is most likely to go looking for after the fact.
	"org_started":               "lifecycle",
	"org_stopped":               "lifecycle",
	"config_revision_activated": "lifecycle",
	"config_revision_applied":   "lifecycle",

	// Task: work reaching a seat — including a detached sandbox run, which
	// is the execution of one, and a schedule firing, which creates one.
	//
	// THERE IS NO TASK LIFECYCLE HERE. The five that were — created,
	// started, completed, failed, delegated — described an engine-owned
	// task object the native tracker replaced, and none of them ever had a
	// publisher. Work's own record is the tracker's log, its history rows
	// and its change feed.
	"task_assigned":                   "task",
	"sandbox_run_started":             "task",
	"sandbox_clarification_requested": "task",
	"sandbox_run_completed":           "task",
	// A run settled WITHOUT its turn resuming. Categorised beside the
	// completion rather than kept live-only: this is the durable record
	// that a turn was lost, and it is the one an operator goes looking for
	// after the fact — a live-only failure would be swept before anybody
	// asked why the work never came back.
	"sandbox_run_failed":   "task",
	"scheduled_task_fired": "task",

	"a2a_channel_opened": "a2a",
	"a2a_message_sent":   "a2a",
	"a2a_channel_closed": "a2a",

	// DACI is behavioural guidance carried on the org's own chat surfaces,
	// not an engine subsystem — nothing in Crewlet publishes these four.
	// They stay mapped as the seam an extension that DOES model decisions
	// writes through, and they are why the dashboard has a `decision`
	// category to filter on at all.
	"decision_requested":     "decision",
	"decision_resolved":      "decision",
	"contribution_requested": "decision",
	"contribution_received":  "decision",

	// Notification: what arrived from outside, and what the engine decided
	// to do about it. Coalescing and a ledger-skipped redelivery are here
	// because operators watch when and how hard batching kicks in — and
	// because the entire point of emitting a skipped trigger is that it
	// should not be invisible.
	"external_notification":   "notification",
	"notification_skipped":    "notification",
	"notifications_coalesced": "notification",
	"turn_trigger_skipped":    "notification",

	// System: the engine talking about itself.
	"budget_exhausted":             "system",
	"llm_unavailable":              "system",
	"agent_turn_completed":         "system",
	"agent_phase_started":          "system",
	"agent_phase_completed":        "system",
	"phase.tool_skill_blocked":     "system",
	"prompt.size":                  "system",
	"turn.guard_breach":            "system",
	"provider_fallback":            "system",
	"skill_telemetry_write_failed": "system",
	"subagent_batched":             "system",

	// Learning: the reflection subsystem and the skill lifecycle, grouped
	// so a dashboard's category filter can include or exclude all of that
	// traffic with one toggle.
	"turn_completed":               "learning",
	"episode_written":              "learning",
	"persist_decider_completed":    "learning",
	"counterparty_profile_updated": "learning",
	"skill_synthesized":            "learning",
	"skill_refined":                "learning",
	"skill_promoted":               "learning",
	"reflection_completed":         "learning",
	"prefetch_summary":             "learning",
	"skill_used":                   "learning",
	"skill_staled":                 "learning",
	"skill_archived":               "learning",
	"skill_revived":                "learning",
	"compaction_requested":         "learning",
	"compaction_completed":         "learning",
}

// excluded are the types deliberately kept OUT of the event store, each with
// the reason it must stay out.
//
// A map to reasons rather than a set, because the whole hazard here is that an
// exclusion and an oversight look identical from the outside: both are a type
// that is published and then vanishes. Writing the reason down is what makes
// the next reader able to tell which one they are looking at — and the
// completeness test prints it.
var excluded = map[string]string{
	"agent_turn_progress": "fires once per LLM round as a live-only signal; the " +
		"matching agent_phase_completed is its durable record, so persisting " +
		"this would fill the log with intermediate states of rows it also " +
		"holds finished",
	"agent_spawned": "placement moves a seat between nodes on every " +
		"rebalance, so a durable row per claim would fill the audit log with " +
		"a fact about SCHEDULING rather than about the company. The live seat " +
		"state is what asks 'is this seat running, and where'",
	"agent_terminated": "the counterpart of agent_spawned, and excluded for " +
		"the same reason; it is what returns a released seat to `terminated` " +
		"on a live screen rather than leaving it showing whatever it last did",
	"a2a_request": "the ASK is already a row: the A2A service publishes " +
		"a2a_channel_opened and a2a_message_sent for the same exchange, under " +
		"the ids the audit trail is keyed on. This event is the WAKE it puts " +
		"on the target seat's inbox, so categorising it would write a second " +
		"row per ask saying the same thing — the same reason raw_webhook is " +
		"kept out below",
	"a2a_message": "the ANSWER is already a row (a2a_message_sent). This " +
		"event is the wake it puts on the requester's inbox; see a2a_request",
	"budget_reported": "a ROLLUP of live meters on a 15-second tick, so a " +
		"durable row per tick is about two million a year to answer a " +
		"question the live projection answers for free. What the audit log " +
		"holds instead is the per-turn spend the rollup is a sum OF — " +
		"agent_turn_completed rows, which internal/tokens aggregates — so " +
		"\"what did we spend last month\" is answerable and \"what were the " +
		"meters reading at 14:03:15\" is not a question anybody asks",
	"tool_skill_page_changed": "a NUDGE between nodes that one tool-skill " +
		"page moved, and the delivery that caused it is already a row, " +
		"written by the webhook receiver. What a skill change did to a " +
		"registry is a log line on each node, so a durable row per node " +
		"would record the same edit once for the wiki and again for every " +
		"member of the fleet",
	"raw_webhook": "the delivery is ALREADY a row, written by the webhook " +
		"receiver under its own id with the raw provider bytes as its payload. " +
		"This event is the wake it publishes onto a seat's inbox, so " +
		"categorising it would write a second row per delivery saying the same " +
		"thing under a different id",
}

// liveOnly is the subset of [excluded] that still drives the live projection.
//
// A subset rather than the same set: agent_turn_progress, the two seat
// lifecycle events and the budget rollup move a live row without joining the
// activity feed, while raw_webhook reaches the projector not at all, because it is published
// onto a seat's inbox rather than onto crewlet.events.*, and the receiver
// ingests its own envelope for it.
var liveOnly = map[string]bool{
	"agent_turn_progress": true,
	"agent_spawned":       true,
	"agent_terminated":    true,
	"budget_reported":     true,
}

// Category names an event type's dashboard category and reports whether the
// type is placed at all. An unplaced type is neither persisted nor fed to the
// activity feed.
func Category(eventType string) (string, bool) {
	c, ok := categories[eventType]
	return c, ok
}

// LiveOnly reports whether a type is excluded from the store while still
// driving the live projection.
func LiveOnly(eventType string) bool { return liveOnly[eventType] }

// Excluded reports why a type is kept out of the event store, or "" if it is
// not deliberately excluded — which, for a type with no category either, means
// nobody has placed it.
func Excluded(eventType string) string { return excluded[eventType] }

// WebhookCategory is the one category no event TYPE carries.
//
// The webhook receiver writes its row directly, under the provider's exact
// bytes and its own delivery id, so nothing in the map above ever produces it —
// but it is a real value of the `category` column, a real option in the
// dashboard's filter, and a vocabulary that omitted it would read as complete
// and be wrong. See internal/api/webhooks.
const WebhookCategory = "webhook"

// CategoryNames is every value the `category` column can hold, sorted, with
// [WebhookCategory] included.
//
// Exported so a docs generator and the guard test read the same list the
// engine files rows under, rather than a copy somebody has to remember to
// update.
func CategoryNames() []string {
	seen := map[string]bool{WebhookCategory: true}
	for _, category := range categories {
		seen[category] = true
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// TypesByCategory groups every placed type under its category, each group
// sorted. The docs table is generated from this.
func TypesByCategory() map[string][]string {
	out := map[string][]string{}
	for eventType, category := range categories {
		out[category] = append(out[category], eventType)
	}
	for _, group := range out {
		slices.Sort(group)
	}
	return out
}

// Exclusions returns every deliberately-excluded type with its reason.
func Exclusions() map[string]string {
	out := make(map[string]string, len(excluded))
	for name, reason := range excluded {
		out[name] = reason
	}
	return out
}
