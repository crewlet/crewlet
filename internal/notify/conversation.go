// Package notify is the backend-neutral notification spine.
//
// Everything an inbound message needs on its way from a vendor's webhook to a
// seat's turn, with no vendor in it: which conversation an event belongs to,
// how several events in one conversation merge into a single trigger, who the
// parties are, which events a seat must not be woken by, and how often it may
// be woken at all.
//
// The vendors sit on top and contribute only what is genuinely theirs — a
// parser, a transport, a prompt, a mention grammar. That split is load-bearing
// rather than tidy: a spine built after its first vendor is a spine with that
// vendor's assumptions welded into it, and the second vendor then arrives to
// find its own shape unrepresentable.
package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// THE CONVERSATION-KEY GRAMMAR.
//
// A key answers one question — "which ongoing conversation is this event part
// of?" — and three subsystems act on the answer: the broker partitions a
// seat's inbox by it, coalescing merges a partition into one trigger, and a
// parked sandbox run matches a person's answer back to the question that asked
// it. Three readers, so one definition.
//
// It is STAMPED BY THE PRODUCER and read by everyone else. The notification
// layer knows the vendor and can derive the key; the broker's partition
// function is then a field read that cannot disagree with what the producer
// meant. Deriving it again at partition time would put vendor knowledge in the
// queue layer and give two places a chance to answer differently.

// KeyField is the payload field a producer stamps the key into.
const KeyField = "conversation_key"

// RecipientField carries the handle a notification was resolved to.
//
// Stamped by the inbound service after the recipient cascade, because it is
// the one fact about a notification a parser genuinely cannot know: which
// seat a vendor's account id or email belongs to is answered by the org, not
// by the payload.
const RecipientField = "recipient_handle"

// ChannelKindField carries the CANONICAL shape of the surface a message
// arrived on — one of [types.ChannelKind].
//
// Stamped by the parser and never derived downstream, for the reason
// [KeyField] gives: the raw value is vendor-specific (Mattermost says "D",
// Slack says the id starts with "D", a tracker has no channel at all), and
// mapping it anywhere but in the vendor's own parser puts vendor knowledge
// in a layer that must not have it — where it would quietly mark arbitrary
// surfaces as direct messages the first time a vendor changed its encoding.
//
// A source with no channel concept stamps nothing, which reads back as
// [types.ChannelUnknown]. That is a real answer for a tracker or a code
// host — "this did not arrive on a channel" — rather than a gap.
const ChannelKindField = "channel_kind"

// EventPrefix namespaces the fallback key.
//
// Its own namespace so it can never collide with a derived key: a vendor's
// local key is namespaced by source, and no source is called "event".
const EventPrefix = "event:"

// Fallback is the key for an event with no derivable conversation.
//
// UNIQUE PER EVENT, which is what makes it correct rather than a placeholder:
// an event that cannot name its conversation must never be merged with
// another, and a shared fallback would merge every one of them. A task
// assignment, a schedule tick and an A2A wake each become a partition of one,
// which is exactly the pre-coalescing dispatch path.
func Fallback(eventID string) string { return EventPrefix + eventID }

// Namespaced turns a vendor's SOURCE-LOCAL key into a global one.
//
// Two vendors can and do mint the same local key — a Plane work item and a
// GitLab issue are both plausibly "42" — and an un-namespaced key would merge
// their events into one trigger. The prompt returns the local half precisely
// so it never has to know this rule.
func Namespaced(source, local string) string {
	if source == "" || local == "" {
		return ""
	}
	return source + ":" + local
}

// KeyOf is the partition function: the key an event carries, or its fallback.
func KeyOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if key, _ := ev.Payload[KeyField].(string); key != "" {
		return key
	}
	return Fallback(ev.ID.String())
}

// KeyOfAll is the key for a whole partition.
//
// Every event in a partition shares a key by construction — that is what the
// broker's partition function guarantees — so a later event naming a different
// one is a routing bug. Taking the FIRST keeps the answer stable rather than
// letting it depend on which event happened to sort last.
func KeyOfAll(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if key, _ := ev.Payload[KeyField].(string); key != "" {
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

// Stamp writes a key onto an event's payload.
//
// The one writer, so a producer cannot spell the field differently from the
// readers. A key that is empty is not stamped at all: an absent field and an
// empty one would be the same to every reader, and leaving it absent keeps
// the fallback in one place.
func Stamp(ev *events.Event, key string) {
	if ev == nil || key == "" {
		return
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	ev.Payload[KeyField] = key
}
