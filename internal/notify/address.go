package notify

import (
	"slices"
	"strings"
)

// THE ONE RULE FOR "IS SOMEBODY WAITING ON THIS SEAT?".
//
// Three subsystems ask it and none of them may answer differently: the
// working indicator raises "is thinking…" while the agent works, the
// delivery check refuses to let an addressed turn end in silence, and the
// prompt tells the agent which of the two it is in. A spinner over a turn
// that is allowed to end in silence, and a silence after a spinner, are the
// same bug — one question answered twice.
//
// It is a VALUE EACH BACKEND DECLARES, not a package-level list. The list was
// the union of the two shipped backends' channel types, which made the answer
// right by coincidence rather than by construction: the conversation key read
// the backend's OWN kinds and the reply obligation read the global, so a
// backend whose channel_type is its own — the engine's native chat is the
// first — was DIRECT to one and NOT ADDRESSED to the other for the same
// message. Nothing about that fails; the seat just stops owing anybody an
// answer in its own direct messages.

// AddressRule is one backend's answer to "was this message addressed to this
// seat?".
//
// THE ZERO VALUE ADDRESSES NOTHING, which is the conservative half of
// [Prompt.Addressed] and the right default for a backend that has not said:
// a seat wrongly told nobody is waiting keeps the freedom to stay silent,
// while one wrongly told somebody is must post something on every broadcast
// it observes.
type AddressRule struct {
	// DirectKinds are the values this backend writes to
	// [ChannelTypeField] for a conversation with no room around it — a
	// one-to-one DM and a group DM.
	//
	// A PRIVATE CHANNEL IS NOT ONE. "Direct" is what tells a seat the
	// message was addressed to it alone, and a five-person private
	// channel is a room.
	DirectKinds []string

	// DMPrefix marks a direct message by CHANNEL ID alone, for the
	// backend whose ids carry their kind — the belt-and-braces answer for
	// an event that arrives with no channel type at all, such as Slack's
	// app_mention.
	//
	// OPT-IN, and it must stay empty where a backend's ids are opaque: a
	// prefix test against arbitrary alphanumerics marks random public
	// channels as direct messages, and then every seat in them owes an
	// answer to traffic nobody addressed to any of them.
	DMPrefix string

	// Follows are the reasons for being in a conversation that oblige a
	// reply. [AddressingFollows] is that set for the vocabulary this
	// package defines, and every shipped backend states it.
	//
	// STATED RATHER THAN INHERITED, because the reason a seat is in a
	// conversation belongs to the BACKEND: the engine's native chat
	// routes on reasons this package has never heard of, and a rule that
	// silently fell back to these four would read every one of them as
	// "does not address".
	Follows []FollowReason
}

// AddressingFollows are the follow reasons that oblige a reply.
//
// DERIVED from [FollowReason.Addresses] rather than written out again, so the
// vocabulary's own answer and every rule built from it cannot come apart.
func AddressingFollows() []FollowReason {
	out := make([]FollowReason, 0, len(followReasons))
	for _, reason := range followReasons {
		if reason.Addresses() {
			out = append(out, reason)
		}
	}
	return out
}

// IsDirect reports whether an event happened in a direct conversation.
func (r AddressRule) IsDirect(metadata map[string]string) bool {
	// An EMPTY channel type is "the backend did not say", never a kind —
	// Slack's app_mention omits the field entirely. Matching it would let
	// a rule that happened to list the empty string read every room as a
	// direct message, so the emptiness is refused before the lookup
	// rather than trusted to the list.
	if kind := metadata[ChannelTypeField]; kind != "" && slices.Contains(r.DirectKinds, kind) {
		return true
	}
	return r.DMPrefix != "" && strings.HasPrefix(metadata[ChannelField], r.DMPrefix)
}

// Addressed reports whether this message obliges the seat an answer.
//
// TWO KEYS, because they are two different facts and a message can carry
// either alone: [FollowReasonField] is why THIS MESSAGE reached the seat —
// the only answer a top-level mention has, since it rode no follow — and
// [FollowingField] is why the seat is in this THREAD at all, which is the
// only answer a later reply has once the message that named it has scrolled
// away.
func (r AddressRule) Addressed(metadata map[string]string) bool {
	if r.IsDirect(metadata) {
		return true
	}
	return r.addressing(FollowReason(metadata[FollowReasonField])) ||
		r.addressing(FollowReason(metadata[FollowingField]))
}

// addressing reports whether being in a conversation for this reason obliges
// a reply under this rule.
func (r AddressRule) addressing(reason FollowReason) bool {
	return reason != "" && slices.Contains(r.Follows, reason)
}
