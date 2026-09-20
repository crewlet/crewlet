package chat_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// THE WIRE NAMES ARE PINNED, because every one of them is a value outside this
// process: a model is told to call it, a registry is keyed on it, an
// operator's MCP client lists it, and a Tool Skill somebody wrote names it in
// prose. Renaming one is not a refactor — it is a seat instructed to call
// something that is not there, with nothing to report the mismatch.
func TestTheChatToolNamesAreTheOnesEverySurfaceSpells(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"post_message":     chat.PostMessageTool,
		"reply_in_thread":  chat.ReplyInThreadTool,
		"send_dm":          chat.SendDMTool,
		"read_channel":     chat.ReadChannelTool,
		"search_messages":  chat.SearchMessagesTool,
		"list_channels":    chat.ListChannelsTool,
		"react_to_message": chat.ReactToMessageTool,
		"join_channel":     chat.JoinChannelTool,
		"leave_channel":    chat.LeaveChannelTool,
	}
	for spelling, constant := range want {
		if constant != spelling {
			t.Errorf("the constant for %q is %q", spelling, constant)
		}
	}
	if got := chat.Tools(); len(got) != len(want) {
		t.Errorf("the catalogue is %v, want the %d names above", got, len(want))
	}
	for _, name := range chat.Tools() {
		if _, named := want[name]; !named {
			t.Errorf("%s is in the catalogue and is not pinned here", name)
		}
	}
	// AND EACH APPEARS ONCE. A duplicate is refused by the registry at
	// boot ([tools.ErrDuplicate]), which turns a typo here into a node
	// that will not start.
	seen := map[string]bool{}
	for _, name := range chat.Tools() {
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
	writes := chat.WriteTools()
	if len(writes) != 3 {
		t.Fatalf("the delivery set is %v", writes)
	}
	for _, name := range writes {
		if !slices.Contains(chat.Tools(), name) {
			t.Errorf("%s counts as a delivery and is in no seat's catalogue", name)
		}
	}
	for _, name := range []string{
		chat.ReactToMessageTool, chat.JoinChannelTool, chat.LeaveChannelTool,
		chat.ReadChannelTool, chat.ListChannelsTool, chat.SearchMessagesTool,
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
	built := chat.NewPrompt().Build(trigger(t, "eng", func(*chat.Notify) {}), nil)
	found := toolShaped.FindAllStringSubmatch(built, -1)
	if len(found) == 0 {
		t.Fatal("the prompt names no tool at all, so this case asserts nothing")
	}
	for _, match := range found {
		name := match[1]
		if !slices.Contains(chat.Tools(), name) {
			t.Errorf("the prompt tells a seat to call %q, which this build "+
				"does not register — its catalogue is %v", name, chat.Tools())
		}
	}
	// AND THE ANSWERING TOOL IS NAMED, or a seat told it owes an answer
	// has to guess which of its tools gives one.
	if !strings.Contains(built, "`"+chat.ReplyInThreadTool+"`") {
		t.Errorf("an addressed wake never names %s: %s", chat.ReplyInThreadTool, built)
	}
}

// THERE IS NO FOLLOW GESTURE, and the catalogue must not pretend otherwise.
//
// `chat_follows` is written by the APPLIER from a message's own mentions —
// being named subscribes you — and this build's record alphabet has no op that
// subscribes or unsubscribes anybody. A `follow_thread` in this list would be
// a tool that fails at every call, which is how a model learns to distrust the
// whole catalogue; adding one is a change to the value layer rather than to
// this file.
func TestTheCatalogueClaimsNoFollowGesture(t *testing.T) {
	t.Parallel()
	for _, name := range chat.Tools() {
		if strings.Contains(name, "follow") {
			t.Errorf("%s is offered and the write path has no op behind it", name)
		}
	}
}
