package chatlet

import (
	"slices"

	"github.com/crewlet/crewlet/internal/toolname"
)

// Chatlet's tool names, BARE rather than prefixed on
// [github.com/crewlet/crewlet/internal/worklet]'s reasoning: a prefix costs
// tokens on every catalogue line for every seat.
//
// These are the most generic names in the tree, and the registry REFUSES a
// duplicate ([github.com/crewlet/crewlet/internal/tools.ErrDuplicate]), so
// Chatlet beside a server publishing `post_message` fails at registration
// naming both sides: two tools with one name are two rooms with one address.
// Re-check a new spelling against `grep -rh 'Tool\s*=\s*"' internal/`.
//
// They live in the DOMAIN because the builtin surface implements them and this
// package's notification prompt names them; neither may import the other.
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
	SendDMTool = toolname.SendDM

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
	// [github.com/crewlet/crewlet/internal/worklet.SearchWorkItemsTool] is
	// one beside the board: a transcript is a position in one room and
	// keeps that room's order, and this ranks a corpus so its answer IS
	// the order.
	SearchMessagesTool = toolname.SearchMessages

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

// The gestures a PERSON makes in chat that no seat is given — the operator
// catalogue's, served to the dashboard's act transport and to a person's own
// assistant alike.
//
// DECLARED HERE beside the nine, although no prompt names them, because they
// are the same vocabulary: a membership set is the state the two seat
// membership tools write ([CallFamily] counts them together), and the names
// must be checked against one list for the collision the head of this block
// warns about.
//
// Each is a person's rather than a seat's for its own reason, and the reasons
// are the domain's, which is why they are written here:
const (
	// EditMessageTool rewrites a message's words. A seat corrects what it
	// said by saying the correction — a reply the room sees arrive — because
	// a remark rewritten after colleagues acted on it is a record the next
	// turn reads as having always said the new thing.
	EditMessageTool = "edit_message"

	// DeleteMessageTool takes a message back, leaving its row so a thread
	// hung off it keeps its root. Its author may, and a moderator — an
	// admin, by the authority table — may take anybody's ([MessageAuthority]).
	// A seat has no use for it that is not hiding what it said.
	DeleteMessageTool = "delete_message"

	// CreateChannelTool makes a named room. A room is furniture a company
	// arranges, and a seat that made rooms as a side effect of its work would
	// leave a sidebar nobody chose; a unit's own room is the org chart's and
	// refused here by the store.
	CreateChannelTool = "create_channel"

	// OpenDMTool opens the direct conversation between the caller and the
	// people named, or reaches the one already open, WITHOUT saying anything
	// — the dashboard's "new message" before the first word. A seat opens one
	// only to say something in it, which is [SendDMTool].
	OpenDMTool = "open_dm"

	// UpdateChannelTool changes a room's settings — its topic, its purpose,
	// whether it is archived. A room's settings are its people's to decide,
	// and archiving one closes the conversation for everybody in it, which
	// is why that half is its creator's, its unit's lead's or an admin's
	// ([ChannelAuthority]). How long a room keeps what is said is NOT here:
	// a shorter horizon destroys, and it is an admin's own gesture
	// ([Store.SetRetention]) on a surface that can ask a fresh proof.
	UpdateChannelTool = "update_channel"

	// SetChannelMembersTool states a room's WHOLE membership. Who may read a
	// private room is the most sensitive fact chat holds; a seat moves only
	// its own membership ([JoinChannelTool], [LeaveChannelTool]).
	SetChannelMembersTool = "set_channel_members"

	// AddChannelMembersTool puts people in a room, and nobody out: what a
	// person adding a colleague means, decided against the room as it
	// stands when it lands ([Store.AddMembers]), so a membership somebody
	// else moved a moment before is never stated back over. A person's for
	// [SetChannelMembersTool]'s reason.
	AddChannelMembersTool = "add_channel_members"

	// MarkChatReadTool records how far a person has read, what they muted and
	// whether they are in do-not-disturb. A read cursor is a person's
	// attention; a seat is woken rather than browsing, and has none.
	MarkChatReadTool = "mark_chat_read"
)

// OperatorOnlyTools are the ones the operator surface adds to [Tools].
func OperatorOnlyTools() []string {
	return []string{
		EditMessageTool, DeleteMessageTool, CreateChannelTool, OpenDMTool,
		UpdateChannelTool, SetChannelMembersTool, AddChannelMembersTool,
		MarkChatReadTool,
	}
}

// Tools are the nine a seat holds, so a caller registering them names one
// thing.
//
// # There is no follow_thread and no unfollow_thread
//
// `chat_follows` is DERIVED by the APPLIER from the thread's own mentions
// (being named subscribes you, the name taken back unsubscribes you), and the
// record alphabet has no op that subscribes anybody ([Store] says so). Tools
// the domain cannot express would fail at every call, which teaches a model to
// distrust the catalogue. Adding them is a value-layer change (an op, a
// payload, an applier arm, and what an unfollow means for a row the next
// mention writes back), not one at this seam.
func Tools() []string {
	return []string{
		PostMessageTool, ReplyInThreadTool, SendDMTool,
		ReadChannelTool, SearchMessagesTool, ListChannelsTool,
		ReactToMessageTool, JoinChannelTool, LeaveChannelTool,
	}
}

// WriteTools are the three that count as a DELIVERY.
//
// A turn woken because somebody spoke to it answers by SAYING SOMETHING, or the
// delivery gate corrects and loops it for having "done nothing". The other
// writes are left out on purpose: a REACTION wakes no seat and obliges nothing,
// and JOINING or LEAVING changes only where this seat listens. Reading is not
// delivering: a turn that only read is the one the gate exists to catch.
func WriteTools() []string {
	return []string{PostMessageTool, ReplyInThreadTool, SendDMTool}
}

// MembershipTools are the tools that write ONE state — a room's membership —
// in opposite directions: joining and leaving move the caller's own place in
// it, and [SetChannelMembersTool] states the whole of it.
//
// DECLARED AS A SET because a turn derives their operations by it
// ([CallFamily]). A count kept per tool cannot see the other half: join, leave,
// join derives the second join as the first's key and answers it with the
// first's success while the seat stays outside. A membership set that takes the
// caller out between two joins is that leave by another name, so it is in the
// family though no seat holds it today: a family is drawn by what a tool WRITES.
func MembershipTools() []string {
	return []string{JoinChannelTool, LeaveChannelTool, SetChannelMembersTool,
		AddChannelMembersTool}
}

// CallFamily is the tools a call to tool is COUNTED AMONG when a turn derives
// its key ([Actor.OpKey]) — the repeat count a turn's call carries — or nil,
// which counts the tool alone.
//
// A family is tools that write one piece of state in OPPOSITE directions, and
// only those: [MembershipTools]. A reaction is one tool with a `remove` flag.
// The posting tools are not one: nothing one says can undo another, and the
// tool is part of every key. A NEW TOOL THAT UNDOES ANOTHER JOINS ITS FAMILY
// HERE, in the same change; too wide a family only turns a repeat separated by
// the other tool's call into a fresh operation rather than a retry.
func CallFamily(tool string) []string {
	if slices.Contains(MembershipTools(), tool) {
		return MembershipTools()
	}
	return nil
}
