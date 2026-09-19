// Package notify is the backend-neutral notification spine.
//
// Everything an inbound message needs on its way from a third-party app's
// webhook to a seat's turn, with no third-party app in it: which conversation
// an event belongs to, how several events in one conversation merge into a
// single trigger, who the parties are, which events a seat must not be woken
// by, and how often it may be woken at all.
//
// The third-party apps sit on top and contribute only what is genuinely
// theirs (a parser, a transport, a prompt, a mention grammar). That split is
// load-bearing rather than tidy: a spine built after its first integration is
// a spine with that integration's assumptions welded into it, and the second
// integration then arrives to find its own shape unrepresentable.
package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// THE TWO KEYS, AND WHICH QUESTION EACH ANSWERS.
//
// An event carries two identities because SIX subsystems read one, and they
// do not all ask the same thing:
//
//   - THE PARTITION KEY — "which other events is this one handled WITH?" The
//     broker partitions a seat's inbox by it (internal/node), the coalescer
//     merges a partition into one trigger, and a parked sandbox run matches
//     a person's answer back to the question that asked it by exact equality
//     against the value its row was written with.
//   - THE CONVERSATION IDENTITY — "which ongoing conversation is this event
//     part of?" The conversation ledger keys on it, the turn telemetry
//     carries it into the event store's conversation_key tag and the
//     episodes column, and the API's answerable-in-chat predicate reads it.
//
// They were ONE value, and the one case where the two questions have
// different answers is what that cost: a direct message's top-level burst
// partitions on the bare channel so a person typing four times costs one turn,
// while a reply in the thread that burst started partitions on the thread —
// so a seat's own prior turn on the DM was filed under a key its next turn
// never looked up, and the ledger read as a first turn every time.
//
// THE PARTITION REFINES THE IDENTITY. Every event in one partition carries
// the SAME identity, and a partition key is either that identity or a finer
// cut of it — on every source, not just chat. That is the property that makes
// the ledger well-defined: a turn is a partition and its entry is written
// once, so if two constituents of one coalesced trigger disagreed about the
// identity the ledger key would depend on which event sorted first. It is
// stated and enforced where a source decides both — see
// [Prompt.ConversationIdentity].
//
// Both are STAMPED BY THE PRODUCER and read by everyone else. The
// notification layer knows the third-party app and can derive them; the
// broker's partition function is then a field read that cannot disagree with
// what the producer meant. Deriving either again at partition time would put
// integration knowledge in the queue layer and give two places a chance to
// answer differently.

// PartitionField is the payload field a producer stamps the partition key
// into.
//
// THE GO NAME MOVED WITH THE CONCEPT; THE WIRE STRING DID NOT. This constant
// was KeyField and its value is still "conversation_key", because the value is
// what two builds exchange over one stream while the name is only what this
// build calls it. A rolling upgrade has an older peer stamping that field and
// a newer node partitioning by it: rename the VALUE and every wake from the
// other half of the fleet arrives unkeyed, every partition becomes a
// singleton, and ten comments on one thread wake a seat ten times — the
// outage [Service.deliver] already records having shipped once.
//
// The same trade the engine already made for agent/phase's "execute", which
// "keeps its wire string although the phase it names now decides as well as
// acts — the value is a column in the event store and read by every
// dashboard, so renaming it would buy a better word at the cost of a value
// migration".
const PartitionField = "conversation_key"

// ConversationField is the payload field the durable conversation identity is
// stamped into.
//
// NEW, and additive: an event from a build that predates the split carries
// only [PartitionField] — see [ConversationIdentityOf] for what a reader does
// with that.
const ConversationField = "conversation_identity"

// RecipientField carries the handle a notification was resolved to.
//
// Stamped by the inbound service after the recipient cascade, because it is
// the one fact about a notification a parser genuinely cannot know: which
// seat a third-party app's account id or email belongs to is answered by the
// org, not by the payload.
const RecipientField = "recipient_handle"

// ChannelKindField carries the CANONICAL shape of the surface a message
// arrived on — one of [types.ChannelKind].
//
// Stamped by the parser and never derived downstream, for the reason
// [PartitionField] gives: the raw value is integration-specific (Mattermost
// says "D", Slack says the id starts with "D", a tracker has no channel at
// all), and mapping it anywhere but in the third-party app's own parser puts
// integration knowledge in a layer that must not have it, where it would
// quietly mark arbitrary surfaces as direct messages the first time a
// third-party app changed its encoding.
//
// A source with no channel concept stamps nothing, which reads back as
// [types.ChannelUnknown]. That is a real answer for a tracker or a code
// host — "this did not arrive on a channel" — rather than a gap.
const ChannelKindField = "channel_kind"

// EventPrefix namespaces the fallback key.
//
// Its own namespace so it can never collide with a derived key: a
// third-party app's local key is namespaced by source, and no source is
// called "event".
const EventPrefix = "event:"

// Fallback is the key for an event with no derivable conversation.
//
// UNIQUE PER EVENT, which is what makes it correct rather than a placeholder:
// an event that cannot name its conversation must never be merged with
// another, and a shared fallback would merge every one of them. A task
// assignment, a schedule tick and an A2A wake each become a partition of one,
// which is exactly the pre-coalescing dispatch path.
//
// IT SERVES BOTH KEYS, and must: [Derived] gates whether a turn is recorded
// at all and whether a parked run is offered as answerable in chat, so an
// event naming neither key has to be underivable under both readings or a row
// gets written that no later message can read back.
func Fallback(eventID string) string { return EventPrefix + eventID }

// Namespaced turns a third-party app's SOURCE-LOCAL key into a global one.
//
// Two third-party apps can and do mint the same local key (a Jira issue and
// a GitLab issue are both plausibly "42"), and an un-namespaced key would
// merge their events into one trigger. The prompt returns the local half
// precisely so it never has to know this rule.
func Namespaced(source, local string) string {
	if source == "" || local == "" {
		return ""
	}
	return source + ":" + local
}

// KeyOf is the partition function: the partition key an event carries, or its
// fallback.
//
// UNCHANGED BY THE SPLIT, deliberately. The partition path is the only one an
// older peer can get wrong — it is what the broker groups on — so it goes on
// reading exactly the field every build has always stamped.
func KeyOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if key, _ := ev.Payload[PartitionField].(string); key != "" {
		return key
	}
	return Fallback(ev.ID.String())
}

// KeyOfAll is the partition key for a whole partition.
//
// Every event in a partition shares a partition key by construction — that is
// what the broker's partition function guarantees — so a later event naming a
// different one is a routing bug. Taking the FIRST keeps the answer stable
// rather than letting it depend on which event happened to sort last.
func KeyOfAll(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if key, _ := ev.Payload[PartitionField].(string); key != "" {
			return key
		}
	}
	return ""
}

// ConversationIdentityOf is the durable conversation an event belongs to, or
// its fallback.
//
// Named for what it answers rather than as a sibling of [KeyOf], and NOT
// "ConversationOf" — that name is taken by the chat thread resolver the
// working indicator raises its spinner on ([ConversationOf] in status.go),
// which answers a different question from different metadata.
//
// THE FALLBACK TO [PartitionField] IS A PEER CONTRACT, not a compatibility
// path for an unreleased surface. An event published by a build from before
// the split carries only "conversation_key", and its value is what that build
// would have handed the ledger — so reading it here reproduces the old
// behaviour for an old event exactly, which is the safe half: the worst it
// costs is the miss this split exists to fix (a DM thread reply filed under
// its thread), where reading the absence as "no conversation" would refuse to
// record the turn at all and lose history a person can see. Same shape as
// [types.ExternalNotification.Addressed], where absent means unaddressed.
//
// THE WINDOW IS NOT AN UPGRADE WINDOW. CREWLET_AGENT is interest retention
// with no maxAge, a seat's mailbox retains while nothing is attached, a parked
// seat republishes its deliveries for hours and a removed seat's mail is kept
// for 24 hours — so an event stamped by one build reaches another whenever,
// and this branch is permanent rather than something a deploy retires.
func ConversationIdentityOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if id, _ := ev.Payload[ConversationField].(string); id != "" {
		return id
	}
	if key, _ := ev.Payload[PartitionField].(string); key != "" {
		return key
	}
	return Fallback(ev.ID.String())
}

// ConversationIdentityOfAll is the conversation identity for a whole
// partition.
//
// The FIRST event that names one, and here that is a lookup rather than a
// choice: every event in a partition carries the same identity by
// construction, because a source's partition key refines its identity. See
// [Prompt.ConversationIdentity].
//
// Per event it falls back to [PartitionField] for the peer reason
// [ConversationIdentityOf] gives — a partition of old-build events must not
// read as having no conversation. A partition that names neither yields "",
// so the caller decides what that means rather than being handed one event's
// fallback as if it described the whole partition.
func ConversationIdentityOfAll(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if id, _ := ev.Payload[ConversationField].(string); id != "" {
			return id
		}
		if key, _ := ev.Payload[PartitionField].(string); key != "" {
			return key
		}
	}
	return ""
}

// Derived reports whether a key names a real conversation somebody could reply
// into, as opposed to the per-event fallback.
//
// The question a parked sandbox run asks: a run started by a schedule tick or
// an A2A wake stored a fallback key, which no inbound message can ever
// reproduce, so telling somebody to "reply in the thread" would send them to a
// thread that does not exist.
func Derived(key string) bool {
	return key != "" && !strings.HasPrefix(key, EventPrefix)
}

// Stamp writes both keys onto an event's payload.
//
// THE ONE WRITER FOR BOTH, so a producer cannot spell either field
// differently from its readers, and so the two can never be stamped by
// different code paths that disagree about which value went where. A key that
// is empty is not stamped at all: an absent field and an empty one would be
// the same to every reader, and leaving it absent keeps the fallback in one
// place.
func Stamp(ev *events.Event, partition, conversation string) {
	if ev == nil || (partition == "" && conversation == "") {
		return
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	if partition != "" {
		ev.Payload[PartitionField] = partition
	}
	if conversation != "" {
		ev.Payload[ConversationField] = conversation
	}
}
