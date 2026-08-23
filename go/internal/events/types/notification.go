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
	events.Register(MessageSent{})
	events.Register(ExternalNotification{})
	events.Register(TurnTriggerSkipped{})
	events.Register(NotificationsCoalesced{})
	events.Register(NotificationSkipped{})
}

// MessageSent records a message the org put on a channel.
type MessageSent struct {
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

func (MessageSent) EventType() string { return "message_sent" }

// SummaryFor prefers the message's own sender: a message published by the
// notification service on someone's behalf is still that person's message.
func (e MessageSent) SummaryFor(actor string) string {
	who := e.Sender
	if who == "" {
		who = actor
	}
	if e.Channel != "" {
		return lead(who, "sent a message to "+e.Channel)
	}
	return lead(who, "sent a message")
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
	// Body is the ENRICHED planner prompt: the notification builder front-loads
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
	// thing-that-changed) rather than the context itself. The Plan-phase
	// relevance-filter prefetches skip their aux-LLM call when it is set.
	ContextRequiresRecon bool `json:"context_requires_recon"`
	// Messages are the constituents when this event is a COALESCED trigger.
	//
	// Empty — the overwhelmingly common case — means a plain single-webhook
	// notification and the flat Sender/SalientBody/Metadata fields are the
	// message. Non-empty means Body is a digest and the flat fields mirror the
	// LATEST constituent; workers iterate this list so every sender in the
	// merged conversation is observed, not just the last one.
	Messages []CoalescedMessage `json:"messages,omitempty"`
}

func (ExternalNotification) EventType() string { return "external_notification" }

func (e ExternalNotification) AgentID() string { return e.Agent }

func (e ExternalNotification) Integration() string          { return e.NotificationSource }
func (e ExternalNotification) IntegrationSender() string    { return e.Sender }
func (e ExternalNotification) IntegrationEventType() string { return e.SourceEventType }

// Actor is the person who sent it.
//
// The event carries no role, so without this the chain would report the actor
// of an inbound Slack message as "notification_service.slack" — a machine name
// sitting in a column of human ones, directly beside the branded badge already
// built from the same string. Empty when the integration named no sender, which
// defers to the rest of the chain rather than replacing a value with nothing.
func (e ExternalNotification) Actor() string { return e.Sender }

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

func (TurnTriggerSkipped) EventType() string { return "turn_trigger_skipped" }

func (e TurnTriggerSkipped) AgentID() string { return e.Agent }

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
	AgentHandle        string `json:"agent_handle"`
	ConversationKey    string `json:"conversation_key"`
	NotificationSource string `json:"notification_source"`
	// Count is the number of constituent notifications; FirstAt and LastAt
	// bound the span they arrived in (ISO 8601).
	Count   int    `json:"count"`
	FirstAt string `json:"first_at"`
	LastAt  string `json:"last_at"`
}

func (NotificationsCoalesced) EventType() string { return "notifications_coalesced" }

func (e NotificationsCoalesced) Integration() string          { return e.NotificationSource }
func (e NotificationsCoalesced) IntegrationSender() string    { return "" }
func (e NotificationsCoalesced) IntegrationEventType() string { return "" }

func (e NotificationsCoalesced) Summary() string {
	return fmt.Sprintf("%d notifications coalesced for %s (%s)",
		e.Count, e.AgentHandle, e.ConversationKey)
}

// NotificationSkipped records a notification dropped, with the reason.
type NotificationSkipped struct {
	Handle             string `json:"handle"`
	Reason             string `json:"reason"`
	NotificationSource string `json:"notification_source"`
}

func (NotificationSkipped) EventType() string { return "notification_skipped" }

func (e NotificationSkipped) Integration() string          { return e.NotificationSource }
func (e NotificationSkipped) IntegrationSender() string    { return "" }
func (e NotificationSkipped) IntegrationEventType() string { return "" }

func (e NotificationSkipped) Summary() string {
	if e.Handle != "" {
		return "Skipped for " + e.Handle + ": " + e.Reason
	}
	return "Skipped: " + e.Reason
}
