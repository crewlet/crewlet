package notify

// THE CHAT METADATA VOCABULARY.
//
// Every key of a chat notification's map[string]string is a contract between
// the parser that writes it and a reader that acts on it. As literals it is
// invisible: a reader of a key nobody writes gets "" and neither fails nor
// logs. These names are the only spelling, so a rename is a compile error.
//
// FIVE KEYS ARE DECLARED ELSEWHERE. [ChannelKindField] and [ActorField] are
// stamped by every source that has the fact, [RecipientField] and
// [PartitionField] by the spine after resolution. [TransportField] IS chat's
// but is declared in conversation.go, whose doc names every reader that must
// agree on it (ADR-0008). [ChatKeys] lists all five.
const (
	// ChannelField is the backend's own id for the surface a message
	// arrived on. Opaque: only the producing backend may read structure
	// out of it, which is what [AddressRule.DMPrefix] is guarded by.
	ChannelField = "channel"

	// ChannelTypeField is the BACKEND'S OWN word for that surface — "im",
	// "D", "channel". Read only against a rule the same backend declared;
	// see [AddressRule].
	ChannelTypeField = "channel_type"

	// ChannelNameField is the human-readable name of the room, where the
	// backend supplies one. Rendered into the trigger, because "eng" is
	// what a person would say and the id is not something an agent can
	// reason about.
	ChannelNameField = "channel_name"

	// MessageIDField is this message's own id — a Slack timestamp, a
	// Mattermost post id — which is the reference for acting on the
	// message rather than replying to it.
	MessageIDField = "ts"

	// ThreadField is the thread a message is a reply IN, and EMPTY for a
	// top-level message. That emptiness is what the whole follow model
	// turns on, so a backend that stamped a top-level message's own id
	// here would make every reply in every thread a delivery.
	ThreadField = "thread_ts"

	// ThreadAnchorField is where a reply GOES, which is not the same
	// question — see [Anchor], the one derivation behind it.
	ThreadAnchorField = "thread_anchor"

	// UserField is the sender's RAW external id, never a display name:
	// the learning subsystem resolves counterparty identity from it, and
	// the prompt annotates a known colleague by it.
	UserField = "user"

	// FollowReasonField is why THIS MESSAGE reached the seat — one of the
	// [FollowReason] values, and empty for a top-level message that
	// triggered nothing.
	FollowReasonField = "thread_follow_reason"

	// FollowingField is why the seat is in this THREAD at all, present
	// only on a reply that rode a follow rather than a top-level message.
	//
	// It carried "true" once, and the reply obligation read the presence
	// of that string as "somebody is waiting" — so a thread a seat had
	// merely spoken in once obliged it to answer every later message in
	// it, for ever. A follow is not an ask; WHICH follow is the whole
	// question, so the reason is the value.
	FollowingField = "thread_following"

	// ReplayedField marks a message re-read over the backend's REST API
	// across a reconnect gap rather than delivered live. The trigger says
	// so, because "this arrived while I was disconnected" changes how
	// stale a seat should assume the conversation is.
	ReplayedField = "replayed"
)

// ChatKeys is the whole SHARED vocabulary, in the order a message is
// described by it: which backend, which surface, which message, which
// conversation, who, and why it reached this seat.
//
// Shared is the operative word. A backend may carry private keys beside
// these — Slack's `app_id`, Mattermost's `bot_username` — and they are read
// by that backend's own prompt and by nothing else, which is why they are
// not here. What IS here is read across the tree, so a key in this list that
// nothing reads is work that looks done, and a spine reader that spells one
// itself is a contract with no other end.
func ChatKeys() []string {
	return []string{
		TransportField,
		ChannelField,
		ChannelTypeField,
		ChannelKindField,
		ChannelNameField,
		MessageIDField,
		ThreadField,
		ThreadAnchorField,
		UserField,
		ActorField,
		FollowReasonField,
		FollowingField,
		RecipientField,
		PartitionField,
		ReplayedField,
	}
}

// RequiredChatKeys are the keys a chat parser stamps on EVERY delivery,
// whatever the message was.
//
// PRESENCE, not content: [ThreadField] and [FollowReasonField] are empty on a
// top-level or untriggered message and still stamped, since a conformance
// check cannot tell an absent key from an empty one.
//
// The conditional keys are NOT here: [ChannelNameField] exists only where the
// backend names rooms, [FollowingField] only on a reply that rode a follow,
// [ReplayedField] only on a replay. [RecipientField] and [PartitionField] are
// stamped by the spine after the recipient cascade, which a parser cannot know.
func RequiredChatKeys() []string {
	return []string{
		TransportField,
		ChannelField,
		ChannelTypeField,
		ChannelKindField,
		MessageIDField,
		ThreadField,
		ThreadAnchorField,
		UserField,
		ActorField,
		FollowReasonField,
	}
}

// Anchor is where a reply to a chat message goes: the thread it is in, or —
// for a top-level message, which has no thread yet — the message itself,
// which BECOMES the thread the moment anybody answers under it.
//
// ONE derivation, shared by the parsers stamping [ThreadAnchorField], the
// working indicator, the inbox's partition key and the trigger's reply target:
// a disagreement does not fail, it puts the spinner in one place and the
// reply in another.
//
// THE STAMPED ANCHOR WINS where a producer supplied one: a backend whose
// reply target is not simply the root states it there, and a reader that
// re-derived would overrule the only code that knows.
func Anchor(metadata map[string]string) string {
	if anchor := metadata[ThreadAnchorField]; anchor != "" {
		return anchor
	}
	if thread := metadata[ThreadField]; thread != "" {
		return thread
	}
	return metadata[MessageIDField]
}
