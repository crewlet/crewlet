package chatlet_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// THE WIRE NAMES ARE PINNED, because every one of them is a value outside this
// process: a model is told to call it, a registry is keyed on it, an
// operator's MCP client lists it, and a Tool Skill somebody wrote names it in
// prose. Renaming one is not a refactor — it is a seat instructed to call
// something that is not there, with nothing to report the mismatch.
func TestTheChatToolNamesAreTheOnesEverySurfaceSpells(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"post_message":     chatlet.PostMessageTool,
		"reply_in_thread":  chatlet.ReplyInThreadTool,
		"send_dm":          chatlet.SendDMTool,
		"read_channel":     chatlet.ReadChannelTool,
		"search_messages":  chatlet.SearchMessagesTool,
		"list_channels":    chatlet.ListChannelsTool,
		"react_to_message": chatlet.ReactToMessageTool,
		"join_channel":     chatlet.JoinChannelTool,
		"leave_channel":    chatlet.LeaveChannelTool,
	}
	for spelling, constant := range want {
		if constant != spelling {
			t.Errorf("the constant for %q is %q", spelling, constant)
		}
	}
	if got := chatlet.Tools(); len(got) != len(want) {
		t.Errorf("the catalogue is %v, want the %d names above", got, len(want))
	}
	for _, name := range chatlet.Tools() {
		if _, named := want[name]; !named {
			t.Errorf("%s is in the catalogue and is not pinned here", name)
		}
	}
	// AND EACH APPEARS ONCE. A duplicate is refused by the registry at
	// boot ([tools.ErrDuplicate]), which turns a typo here into a node
	// that will not start.
	seen := map[string]bool{}
	for _, name := range chatlet.Tools() {
		if seen[name] {
			t.Errorf("%s is in the catalogue twice", name)
		}
		seen[name] = true
	}
}

// THE DELIVERY SET IS ONLY WHAT REACHES SOMEBODY.
//
// The gate asks whether the person waiting was answered. A REACTION wakes
// nobody, so if it counted every addressed turn could discharge its obligation
// with a thumb; JOINING a room changes where this seat listens and leaves the
// asker exactly as unanswered; and a READ is the turn the gate exists to
// catch.
func TestOnlyTheThreePostingToolsAreDeliveries(t *testing.T) {
	t.Parallel()
	writes := chatlet.WriteTools()
	if len(writes) != 3 {
		t.Fatalf("the delivery set is %v", writes)
	}
	for _, name := range writes {
		if !slices.Contains(chatlet.Tools(), name) {
			t.Errorf("%s counts as a delivery and is in no seat's catalogue", name)
		}
	}
	for _, name := range []string{
		chatlet.ReactToMessageTool, chatlet.JoinChannelTool, chatlet.LeaveChannelTool,
		chatlet.ReadChannelTool, chatlet.ListChannelsTool, chatlet.SearchMessagesTool,
	} {
		if slices.Contains(writes, name) {
			t.Errorf("%s counts as a delivery, so a turn that answered nobody "+
				"would pass the gate", name)
		}
	}
}

// toolShaped matches a name this build would register: a backticked
// lower_snake word, which is the only way a prompt here names a tool.
var toolShaped = regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")

// THE PROMPT MAY ONLY NAME TOOLS THIS BUILD SHIPS.
//
// This is the whole reason the names live in the domain rather than beside the
// implementations: the notification prompt tells a woken seat which tool to
// reach for, and a prompt naming one the registry does not carry is a seat
// instructed to call something that is not there — which costs it a round and
// then a guess. Nothing else compares the two lists.
func TestTheNotificationPromptNamesNoToolThisBuildDoesNotShip(t *testing.T) {
	t.Parallel()
	built := chatlet.NewPrompt().Build(trigger(t, "eng", func(*chatlet.Notify) {}), nil)
	found := toolShaped.FindAllStringSubmatch(built, -1)
	if len(found) == 0 {
		t.Fatal("the prompt names no tool at all, so this case asserts nothing")
	}
	for _, match := range found {
		name := match[1]
		if !slices.Contains(chatlet.Tools(), name) {
			t.Errorf("the prompt tells a seat to call %q, which this build "+
				"does not register — its catalogue is %v", name, chatlet.Tools())
		}
	}
	// AND THE ANSWERING TOOL IS NAMED, or a seat told it owes an answer
	// has to guess which of its tools gives one.
	if !strings.Contains(built, "`"+chatlet.ReplyInThreadTool+"`") {
		t.Errorf("an addressed wake never names %s: %s", chatlet.ReplyInThreadTool, built)
	}
}

// THERE IS NO FOLLOW GESTURE, and the catalogue must not pretend otherwise.
//
// `chat_follows` is derived by the APPLIER from the thread's own mentions —
// being named subscribes you — and this build's record alphabet has no op that
// subscribes or unsubscribes anybody. A `follow_thread` in this list would be
// a tool that fails at every call, which is how a model learns to distrust the
// whole catalogue; adding one is a change to the value layer rather than to
// this file.
func TestTheCatalogueClaimsNoFollowGesture(t *testing.T) {
	t.Parallel()
	for _, name := range chatlet.Tools() {
		if strings.Contains(name, "follow") {
			t.Errorf("%s is offered and the write path has no op behind it", name)
		}
	}
}

// THE OPERATOR'S CHAT GESTURES ARE PINNED TOO, and none of them is a seat's.
//
// They are wire names for the same reason the nine are — the dashboard's
// action table names each, and a person's assistant lists them — and a gesture
// that drifted into [chatlet.Tools] would be offered to every seat in the company:
// a seat that could rewrite its own remarks, delete a colleague's or decide
// who may read a private room.
func TestTheOperatorsChatGesturesAreNoSeatsAndArePinned(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"edit_message":        chatlet.EditMessageTool,
		"delete_message":      chatlet.DeleteMessageTool,
		"create_channel":      chatlet.CreateChannelTool,
		"open_dm":             chatlet.OpenDMTool,
		"update_channel":      chatlet.UpdateChannelTool,
		"set_channel_members": chatlet.SetChannelMembersTool,
		"mark_chat_read":      chatlet.MarkChatReadTool,
	}
	for spelling, constant := range want {
		if constant != spelling {
			t.Errorf("the constant for %q is %q", spelling, constant)
		}
	}
	only := chatlet.OperatorOnlyTools()
	if len(only) != len(want) {
		t.Errorf("the operator's gestures are %v, want the %d names above", only, len(want))
	}
	seen := map[string]bool{}
	for _, name := range only {
		if _, named := want[name]; !named {
			t.Errorf("%s is an operator gesture and is not pinned here", name)
		}
		if slices.Contains(chatlet.Tools(), name) {
			t.Errorf("%s is in every seat's catalogue", name)
		}
		if seen[name] {
			t.Errorf("%s is listed twice", name)
		}
		seen[name] = true
	}
}

// A MEMBERSHIP SET IS COUNTED WITH JOIN AND LEAVE, because it writes the state
// they write: a set that took the caller out between two joins is a leave by
// another name, and a family that left it out would hand the second join the
// first one's operation and its success while the caller stayed outside.
func TestAMembershipSetIsCountedWithJoinAndLeave(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{chatlet.JoinChannelTool, chatlet.LeaveChannelTool,
		chatlet.SetChannelMembersTool} {
		family := chatlet.CallFamily(tool)
		for _, member := range []string{chatlet.JoinChannelTool, chatlet.LeaveChannelTool,
			chatlet.SetChannelMembersTool} {
			if !slices.Contains(family, member) {
				t.Errorf("%s is counted among %v, without %s", tool, family, member)
			}
		}
	}
}
