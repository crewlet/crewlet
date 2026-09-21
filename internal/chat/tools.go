package chat

// The native chat's tool names.
//
// BARE, not prefixed, on [github.com/crewlet/crewlet/internal/tracker]'s
// reasoning: a first-party tool may be named in a prompt — this build
// registers it under a name this build chose — and a prefix would cost tokens
// on every catalogue line for every seat to disambiguate from nothing.
//
// # These are the most generic names in the tree, and that is the risk
//
// `post_message`, `read_channel` and `list_channels` are what a chat MCP
// server would plausibly call its own tools, and the registry REFUSES a
// duplicate rather than overwriting one
// ([github.com/crewlet/crewlet/internal/tools.ErrDuplicate]) — so a company
// running both native chat and a vendor server that publishes one of these
// names gets a refusal at registration, naming both sides, instead of a seat
// silently calling whichever won. That is the outcome to want: two tools with
// one name are two rooms with one address, and a message would land in
// whichever the registration order picked.
//
// None of the nine collides with a first-party name this build ships. The
// shipped set is the tracker's, the wiki's, the learning tools and the
// meta-tools, and it is worth re-checking against
// `grep -rh 'Tool\s*=\s*"' internal/` before any of these is respelled.
//
// # Why they live in the DOMAIN rather than beside the implementations
//
// Two packages need them and neither may import the other: the builtin
// surface implements the tools, and this package's own notification prompt
// NAMES them — it tells a woken seat which tool to reach for, and a prompt
// that named a tool this build does not register is a seat instructed to call
// something that is not there. The domain owns its vocabulary.
const (
	// PostMessageTool starts a new top-level message in a room.
	PostMessageTool = "post_message"

	// ReplyInThreadTool answers a message under its own thread, which is
	// where an answer belongs: a thread here is one level deep, so a reply
	// to a reply carries the same root and the whole exchange stays beside
	// the question.
	ReplyInThreadTool = "reply_in_thread"

	// SendDMTool says something privately, opening the conversation if it
	// is not open yet.
	//
	// ITS OWN VERB rather than a flag on [PostMessageTool], because a
	// direct conversation is ADDRESSED BY ITS PARTICIPANTS and a room is
	// addressed by its id — one call takes handles and the other takes a
	// room, and a single tool would have to decide which of two arguments
	// it was given.
	SendDMTool = "send_dm"

	// ReadChannelTool is a page of a room, or of one thread in it.
	//
	// ONE VERB FOR BOTH, because they are one question asked at two
	// depths — "what was said here" — and the answer differs only in what
	// `thread` narrows it to. Two verbs would make a model choose before
	// it knows whether the message it cares about started a thread.
	ReadChannelTool = "read_channel"

	// SearchMessagesTool finds a message by what it SAYS, ranked over
	// every room the caller may read.
	//
	// ITS OWN VERB beside [ReadChannelTool], for the reason
	// [github.com/crewlet/crewlet/internal/tracker.SearchWorkItemsTool] is
	// one beside the board: a transcript is a position in one room and
	// keeps that room's order, and this ranks a corpus so its answer IS
	// the order.
	SearchMessagesTool = "search_messages"

	// ListChannelsTool is the rooms this seat is in.
	ListChannelsTool = "list_channels"

	// ReactToMessageTool puts one emoji on a message, or takes it back.
	//
	// IT IS NEVER AN ANSWER, which is why it is not in [WriteTools]: the
	// delivery gate asks whether the person waiting was reached, and an
	// emoji that counted would let every addressed turn discharge its
	// obligation with a thumb.
	ReactToMessageTool = "react_to_message"

	// JoinChannelTool and LeaveChannelTool move THIS SEAT's own
	// membership, and nobody else's — the write path refuses any other
	// handle, which is what makes them a seat's own state rather than a
	// write to a shared surface.
	JoinChannelTool  = "join_channel"
	LeaveChannelTool = "leave_channel"
)

// Tools are the nine a seat holds, so a caller registering them names one
// thing.
//
// # There is no follow_thread and no unfollow_thread
//
// Stated here rather than left as an absence, because a reader who knows the
// design's table will come looking for them. `chat_follows` is written by the
// APPLIER from a message's own mentions — being named subscribes you — and
// this build's record alphabet has no op that subscribes or unsubscribes
// anybody; [Store] says so in as many words, and so does the applier.
// Registering the two names against a domain that cannot express the gesture
// would be two tools that fail at every call, which is how a model learns to
// distrust the whole catalogue.
//
// Adding them is a change to the value layer — an op, a payload, an applier
// arm and a decision about what an unfollow means against a row a later
// mention rewrites — and it belongs in the domain rather than at this seam.
func Tools() []string {
	return []string{
		PostMessageTool, ReplyInThreadTool, SendDMTool,
		ReadChannelTool, SearchMessagesTool, ListChannelsTool,
		ReactToMessageTool, JoinChannelTool, LeaveChannelTool,
	}
}

// WriteTools are the three that count as a DELIVERY.
//
// A turn woken because somebody spoke to it answers by SAYING SOMETHING — in
// the thread, in the room, or privately — and the delivery gate has to know
// that, or such a turn is corrected and looped for having "done nothing".
//
// THE OTHER WRITES ARE NOT HERE, and each is left out for its own reason
// rather than by oversight:
//
//   - a REACTION reaches nobody: it wakes no seat and obliges nothing, so a
//     turn that answered a direct question with an emoji has not answered it.
//   - JOINING or LEAVING a room changes only where this seat listens. The
//     person waiting for an answer is no closer to having one.
//
// Reading is not delivering either, which is why the three reads are not
// here: a turn that only read is exactly the turn the gate exists to catch.
func WriteTools() []string {
	return []string{PostMessageTool, ReplyInThreadTool, SendDMTool}
}
