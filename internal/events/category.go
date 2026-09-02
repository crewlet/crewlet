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

import "sort"

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
	"agent_spawned":             "lifecycle",
	"agent_terminated":          "lifecycle",
	"agent_reassigned":          "lifecycle",
	"role_updated":              "lifecycle",
	"config_revision_activated": "lifecycle",
	"config_revision_applied":   "lifecycle",

	// Task: work being created, assigned and done — including a detached
	// sandbox run, which is the execution of a task, and a schedule firing,
	// which creates one.
	"task_created":                    "task",
	"task_assigned":                   "task",
	"task_started":                    "task",
	"task_completed":                  "task",
	"task_failed":                     "task",
	"task_delegated":                  "task",
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

	"message_sent": "communication",

	"a2a_channel_opened":    "a2a",
	"a2a_message_sent":      "a2a",
	"a2a_message_delivered": "a2a",
	"a2a_channel_closed":    "a2a",

	// DACI is behavioural guidance carried on the org's own chat surfaces,
	// not an engine subsystem — nothing in Crewlet publishes these four.
	// They stay mapped as the seam an extension that DOES model decisions
	// writes through, and they are why the dashboard has a `decision`
	// category to filter on at all.
	"decision_requested":     "decision",
	"decision_resolved":      "decision",
	"contribution_requested": "decision",
	"contribution_received":  "decision",

	"document_created": "knowledge",
	"document_updated": "knowledge",

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
	"execute.missing_tool":         "system",
	"phase.tool_activated":         "system",
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
	"budget_reported": "a snapshot of LIVE, in-memory meters whose values mean " +
		"nothing outside the engine run that produced them; persisting it lets " +
		"a dashboard hydrate a dead process's counters and render them as the " +
		"current ones — a number that is not merely stale but describes a " +
		"different run",
	"raw_webhook": "the delivery is ALREADY a row, written by the webhook " +
		"receiver under its own id with the raw provider bytes as its payload. " +
		"This event is the wake it publishes onto a seat's inbox, so " +
		"categorising it would write a second row per delivery saying the same " +
		"thing under a different id",
}

// liveOnly is the subset of [excluded] that still drives the live projection.
//
// A subset rather than the same set: agent_turn_progress moves a seat's live
// row and budget_reported moves the budget slice, both without joining the
// activity feed — while raw_webhook reaches the projector not at all, because
// it is published onto a seat's inbox rather than onto crewlet.events.*, and
// the receiver ingests its own envelope for it.
var liveOnly = map[string]bool{
	"agent_turn_progress": true,
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
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
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
		sort.Strings(group)
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
