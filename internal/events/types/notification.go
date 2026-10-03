package types

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// Messages reaching the org from outside it, and the bookkeeping around what
// the engine did with them: what got merged, what got skipped, what was already
// worked.

func init() {
	events.Register[ExternalNotification]()
	events.Register[TurnTriggerSkipped]()
	events.Register[NotificationsCoalesced]()
	events.Register[NotificationSkipped]()
}

// CoalescedMessage is one constituent inbound message inside a coalesced
// notification.
//
// When several same-conversation notifications are merged into one trigger, the
// merged ExternalNotification keeps each original here in chronological order —
// full fidelity for the learning workers (per-sender profiling, salient-text
// filters), while the rendering decisions (supersede rules, digest layout) live
// in the merged Body.
type CoalescedMessage struct {
	Sender string `json:"sender"`
	// Body is this constituent's SALIENT body: the raw inbound message, with no
	// prompt scaffolding.
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
	// SourceEventType is the integration's own name for what happened.
	SourceEventType string `json:"source_event_type"`
	// Metadata is the constituent's full notification metadata, so per-message
	// sender identity (Slack user id, Jira account id, …) stays extractable
	// after the merge.
	Metadata map[string]string `json:"metadata,omitempty"`
	// ContextRequiresRecon is this constituent's OWN recon flag. The merged
	// event's flat flag is the conservative any() across constituents because
	// it gates whole-turn prefetch logic, so without this per-message copy the
	// original flags would be unrecoverable and a worker reasoning per message
	// would silently inherit whole-turn semantics.
	ContextRequiresRecon bool `json:"context_requires_recon"`

	// Addressed is this constituent's OWN "somebody is waiting on this
	// seat" flag, carried per message for the same reason the recon flag
	// is: the merged event's flat flag is an any() across constituents, so
	// without this copy a worker reasoning per message could not tell the
	// one direct mention in a burst from the four broadcasts around it.
	Addressed bool `json:"addressed,omitempty"`
}

// ExternalNotification is an inbound notification from an external system, and
// the most common thing that wakes an agent.
type ExternalNotification struct {
	NotificationSource string `json:"notification_source"`
	SourceEventType    string `json:"source_event_type"`
	RecipientEmail     string `json:"recipient_email"`
	Agent              string `json:"agent_id"`
	Sender             string `json:"sender"`
	Subject            string `json:"subject"`
	// Body is the ENRICHED trigger prompt: the notification builder front-loads
	// triage and how-to boilerplate (for Slack, ~1.5k characters) before the
	// actual message.
	Body string `json:"body"`
	// SalientBody is the raw inbound message, stripped of that scaffolding.
	//
	// A pointer because nil and "" are different facts and the learning workers
	// act on the difference. nil means "this producer emitted no distinct raw
	// message" (an extension event, or a producer that does not set it) and
	// workers fall back to Body. An empty string means the producer set a
	// salient body and it was genuinely empty, so workers must NOT fall back to
	// the scaffolding-laden Body — a filter keyed on a prefix of that never sees
	// a message at all, only boilerplate identical on every turn.
	SalientBody *string           `json:"salient_body"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	// ContextRequiresRecon is true when Body is a POINTER (a webhook naming a
	// thing-that-changed) rather than the context itself. The turn-start
	// relevance-filter prefetches skip their aux-LLM call when it is set.
	ContextRequiresRecon bool `json:"context_requires_recon"`

	// Addressed is true when somebody is waiting on this seat for an
	// answer — a direct message, a personal mention, an assignment —
	// as the source's own [notify.Prompt] reads its routing.
	//
	// The turn engine derives the turn's delivery obligation from it: an
	// addressed turn may not end in silence. ABSENT MEANS UNADDRESSED, and
	// that is the safe half in both directions — an older build's events
	// decode as unaddressed, which is the freedom to stay silent rather
	// than an obligation to post.
	Addressed bool `json:"addressed,omitempty"`

	// Owes is the CHAT SURFACE this turn must post on before it is done,
	// when that is not the notification's own source — `mattermost` or
	// `slack`, from the answer to a decision whose asker said it would
	// report the outcome there. The turn engine raises the delivery
	// obligation on it rather than on NotificationSource, so a turn woken
	// by the tracker is held open until a chat tool has run rather than
	// being closed out by a comment on the item.
	//
	// ADDITIVE AND ABSENT-SAFE, like Addressed: an older build's events
	// decode with no surface owed, which is the obligation that event's
	// writer recorded.
	Owes string `json:"owes,omitempty"`

	// Messages are the constituents when this event is a COALESCED trigger.
	//
	// Empty — the overwhelmingly common case — means a plain single-webhook
	// notification and the flat Sender/SalientBody/Metadata fields are the
	// message. Non-empty means Body is a digest and the flat fields mirror the
	// LATEST constituent; workers iterate this list so every sender in the
	// merged conversation is observed, not just the last one.
	Messages []CoalescedMessage `json:"messages,omitempty"`
}

// EventType is the "external_notification" wire type.
func (ExternalNotification) EventType() string { return "external_notification" }

// AgentID is the seat the notification was routed to. It is the chain's last
// resort here, since Actor already answers with the human who sent it.
func (e ExternalNotification) AgentID() string { return e.Agent }

// Integration is the system the message came from, and the string the dashboard
// builds its branded badge out of.
func (e ExternalNotification) Integration() string { return e.NotificationSource }

// IntegrationSender is the human the integration identified, when it did.
func (e ExternalNotification) IntegrationSender() string { return e.Sender }

// IntegrationEventType is the integration's own name for what happened —
// "message", "issue_commented" — not one of this catalogue's type strings.
func (e ExternalNotification) IntegrationEventType() string { return e.SourceEventType }

// Actor is the person who sent it.
//
// The event carries no role, so without this the chain would report the actor
// of an inbound Slack message as "notification_service.slack" — a machine name
// sitting in a column of human ones, directly beside the branded badge already
// built from the same string. Empty when the integration named no sender, which
// defers to the rest of the chain rather than replacing a value with nothing.
func (e ExternalNotification) Actor() string { return e.Sender }

// Brief is the ask: the enriched trigger prompt the notification builder
// assembled, which is the message plus the triage guidance wrapped around it.
//
// Body rather than SalientBody, because the scaffolding is not noise here —
// it is the integration prompt that tells the agent how to read this surface.
// The salient half is what the LEARNING workers want (see
// engine.salientBody), and the two readers wanting different halves is
// exactly why both fields exist.
func (e ExternalNotification) Brief() string { return e.Body }

// Summary leads with the sender and subject. The integration is surfaced by the
// dashboard as a badge built from NotificationSource, so repeating the source
// name here would say it twice.
func (e ExternalNotification) Summary() string {
	head := "Notification"
	switch {
	case len(e.Messages) > 0:
		head = fmt.Sprintf("%d messages", len(e.Messages))
		if e.Sender != "" {
			head += " from " + e.Sender
		}
	case e.Sender != "":
		head = "Message from " + e.Sender
	}
	if e.Subject != "" {
		return head + ": " + e.Subject
	}
	return head
}

// TurnTriggerSkipped records a trigger that was NOT worked because a previous
// turn already did it — the narrow window where a turn finished, its outbound
// effects shipped, and the owning node died before the delivery was acked.
//
// It exists because the alternative is invisibility. Without it a skipped
// trigger shows no turn on the dashboard, no error in the logs, and nothing
// anywhere distinguishes "the agent never answered" from "the agent already
// answered, on a node that has since died" — the same observation and opposite
// problems.
type TurnTriggerSkipped struct {
	AgentHandle string `json:"agent_handle"`
	Agent       string `json:"agent_id"`
	TriggerID   string `json:"trigger_id"`
	TriggerType string `json:"trigger_type"`
	Reason      string `json:"reason"`
}

// EventType is the "turn_trigger_skipped" wire type.
func (TurnTriggerSkipped) EventType() string { return "turn_trigger_skipped" }

// AgentID is the seat that did NOT run a turn for this trigger.
func (e TurnTriggerSkipped) AgentID() string { return e.Agent }

// Summary fills in both blanks it can have. A skip whose type or reason went
// unstated is the case an operator most needs to read, so neither is allowed to
// render as a gap in the sentence.
func (e TurnTriggerSkipped) Summary() string {
	kind := e.TriggerType
	if kind == "" {
		kind = "event"
	}
	reason := e.Reason
	if reason == "" {
		reason = "unspecified"
	}
	return "trigger " + kind + " skipped for " + e.AgentHandle + " (" + reason + ")"
}

// NotificationsCoalesced is the observability counterpart of a merged
// ExternalNotification: operators watch this stream to see when and how hard
// inbox batching kicks in — a busy agent draining a thread's backlog as one
// turn, or a linger window absorbing a webhook burst.
type NotificationsCoalesced struct {
	AgentHandle string `json:"agent_handle"`

	// PartitionKey is the inbox PARTITION that merged — the batch, not the
	// conversation it belongs to. What this event says is "N deliveries
	// became one turn", and the batch is the subject of that sentence.
	//
	// THE NAME MOVED AND SO DID THE WIRE STRING. It was
	// `conversation_key`, and that field is the conversation IDENTITY on
	// every other event that spells it — the two sandbox events, both turn
	// completions and the phase record. One promoted tag therefore meant
	// the identity or the partition depending on which row a filter
	// happened to match, on a pair of values that differ exactly where it
	// matters: a direct message's identity is the bare channel and its
	// partition can be a thread inside it.
	//
	// CARRYING BOTH SPELLINGS IS NOT THE MILDER FIX IT LOOKS LIKE, because
	// store.ExtractTags promotes by WIRE KEY ALONE and is type-blind on
	// purpose — that is what lets a newer node's unknown event type still
	// be indexed by the dimensions it does carry. A payload spelling both
	// keys is therefore tagged both, and the `conversation_key` tag goes on
	// holding a partition for exactly the rows this exists to repair.
	//
	// WHAT A ROLLING PEER LOSES, stated rather than waved at, since a
	// rename is a dropped field on whichever half has not upgraded
	// (ADR-0006). An older build decoding a newer build's copy of this
	// event finds nothing under its own ConversationKey, so its Summary
	// names the seat without the batch — on the LIVE SOCKET only, for the
	// length of the upgrade. Nothing else reads it: the store row is
	// written by a publish listener inline on the PUBLISHING node, so the
	// build that authored a payload is the build that tags it and no row is
	// ever tagged by a rule its payload was not written for; and the socket
	// envelope carries the payload verbatim, so the key reaches an older
	// node's dashboard intact under its new name. A degraded sentence for
	// one upgrade window is the whole price, against a tag that answers the
	// wrong question for the life of the deployment.
	//
	// Which is the opposite of the trade notify.PartitionField makes for
	// the notification payload, and why that one kept its wire string:
	// there the value is what two builds partition each other's wakes by,
	// so renaming it would leave every cross-build wake unkeyed. Here
	// nothing DECIDES anything from this event — it is published to the
	// event topic, never into a seat's inbox, so notify.KeyOf never reads
	// it.
	PartitionKey       string `json:"partition_key"`
	NotificationSource string `json:"notification_source"`
	// Count is the number of constituent notifications; FirstAt and LastAt
	// bound the span they arrived in (ISO 8601).
	Count   int    `json:"count"`
	FirstAt string `json:"first_at"`
	LastAt  string `json:"last_at"`
}

// EventType is the "notifications_coalesced" wire type.
func (NotificationsCoalesced) EventType() string { return "notifications_coalesced" }

// Integration is the system whose notifications were merged.
func (e NotificationsCoalesced) Integration() string { return e.NotificationSource }

// IntegrationSender is always empty: a merge spans constituents that may come
// from several senders, so naming one would misattribute the rest.
func (e NotificationsCoalesced) IntegrationSender() string { return "" }

// IntegrationEventType is always empty: the merge is the engine's own doing,
// not something the integration reported.
func (e NotificationsCoalesced) IntegrationEventType() string { return "" }

// Summary names the partition key, which is what the coalescing keyed on —
// without it two merges for the same seat are indistinguishable.
func (e NotificationsCoalesced) Summary() string {
	return fmt.Sprintf("%d notifications coalesced for %s (%s)",
		e.Count, e.AgentHandle, e.PartitionKey)
}

// NotificationSkipped records a notification dropped, with the reason.
type NotificationSkipped struct {
	Handle             string `json:"handle"`
	Reason             string `json:"reason"`
	NotificationSource string `json:"notification_source"`
}

// EventType is the "notification_skipped" wire type.
func (NotificationSkipped) EventType() string { return "notification_skipped" }

// Integration is the system whose notification was dropped.
func (e NotificationSkipped) Integration() string { return e.NotificationSource }

// IntegrationSender is always empty: a drop is decided before a sender is
// resolved, so this event never learns one.
func (e NotificationSkipped) IntegrationSender() string { return "" }

// IntegrationEventType is always empty, for the same reason as
// IntegrationSender.
func (e NotificationSkipped) IntegrationEventType() string { return "" }

// Summary always states the reason. A dropped notification with no explanation
// is indistinguishable from one that never arrived.
func (e NotificationSkipped) Summary() string {
	if e.Handle != "" {
		return "Skipped for " + e.Handle + ": " + e.Reason
	}
	return "Skipped: " + e.Reason
}
