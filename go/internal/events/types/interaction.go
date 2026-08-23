package types

// The canonical, platform-agnostic boundary type for inbound messages, ported
// from src/crewlet/learning/interaction.py. It rides on TurnCompleted, which is
// why it lives here rather than waiting for the learning package: an event's
// wire shape cannot be described without the types its fields hold.
//
// Only the data is ported. The platform-aware constructor
// (InboundInteraction.list_from_trigger_event) is the learning subsystem's one
// place that touches Slack/Jira/GitHub metadata keys, and it belongs with the
// learning workers that consume it, not with the catalogue.

// ChannelKind is the coarse category of the surface a message arrived on. Used
// for prompt-context flavour only — nothing branches on it for behaviour.
//
// Note "unknown" is Python's default and "" is Go's zero value; a producer that
// does not classify the channel should set ChannelUnknown explicitly.
type ChannelKind string

const (
	ChannelDM       ChannelKind = "dm"
	ChannelGroup    ChannelKind = "group"
	ChannelPublic   ChannelKind = "public"
	ChannelInternal ChannelKind = "internal"
	ChannelUnknown  ChannelKind = "unknown"
)

// CanonicalIdentity is a platform-agnostic identifier for an inbound sender.
//
// Either Handle (a resolved Crewlet agent) or ExternalID (a platform id for an
// unresolved external human) identifies the sender; Platform carries provenance
// for a later lookup and DisplayName is best-effort for prompt rendering.
//
// An empty identity — no handle, no external id — means "no identifiable
// sender": an internal TaskAssigned trigger, a scheduled tick, a system event.
// Workers short-circuit on that rather than inspecting the original event type.
type CanonicalIdentity struct {
	Handle      string `json:"handle"`
	ExternalID  string `json:"external_id"`
	Platform    string `json:"platform"`
	DisplayName string `json:"display_name"`
}

// InboundInteraction is one inbound message that triggered a turn.
//
// A turn is triggered by a LIST of these: usually one, but a coalesced trigger
// carries one per constituent message, possibly from several senders. Workers
// that reason about text join the bodies; workers that reason about people
// iterate per distinct sender.
type InboundInteraction struct {
	Sender CanonicalIdentity `json:"sender"`
	Body   string            `json:"body"`
	// ChannelKind is descriptive context, never a behaviour switch.
	ChannelKind ChannelKind `json:"channel_kind"`
	// RawEventType is captured for audit and debugging only — it is the one
	// field learning workers must never branch on. Carrying it lets an
	// operator-facing tool resolve back to the kind of trigger that produced
	// this interaction without re-fetching the original event.
	RawEventType string `json:"raw_event_type"`
	// RequiresRecon is true when the trigger is a POINTER (a webhook naming a
	// thing-that-changed) rather than self-contained context: the agent must
	// fetch the issue, read the thread or pull the diff before it has anything
	// substantive. The Plan-phase relevance filters skip their aux-LLM call on
	// it — filtering against a bare pointer is near-guaranteed low value, and
	// the planner is already told to re-query after recon. This is the one
	// field workers MAY branch on, because it is a normalized platform-agnostic
	// property rather than an event-type check.
	RequiresRecon bool `json:"requires_recon"`
}
