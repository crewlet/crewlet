package builtin_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// EVERY CURATED WRITE EXECUTES AS AN OPERATOR TOOL IT CAN STAND ON.
//
// Walked over the operator catalogue with every half on. A row of the
// catalogue's exposure table naming an operator tool this build does not serve
// is a curated write that fails on every call; a row's tool is never a proven
// read; and its verb is one the authority table lets an approval carry. Every
// other operator tool is the person's own through the generic change, on the
// act route, decided by its own verb (ADR-0052). And each catalogue write that
// executes as an operator tool is held to it: the destructive hint its tier is
// derived from is the tool's own annotation, and every argument the model may
// send is one the act tool declares, of the same type, so an approved request
// is one the tool reads whole, `removes` aside, which admits a removal the
// preview found and is never sent. Mutation: name an operator tool that is
// not served, mark update_task not destructive, or declare an argument
// create_work_item does not take, and this fails.
func TestEveryCuratedWriteExecutesAsAnOperatorTool(t *testing.T) {
	t.Parallel()
	served := map[string]map[string]any{}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		served[tool.Name()] = tool.Parameters()
	}
	if len(served) < 40 {
		t.Fatalf("the operator catalogue serves %d tools, so every half is not on", len(served))
	}
	for _, e := range catalog.Exposures() {
		if e.Tool == "" {
			continue
		}
		if _, ok := served[e.Tool]; !ok {
			t.Errorf("%s executes as the operator tool %s, which this build does not serve",
				e.Name, e.Tool)
			continue
		}
		if crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(e.Tool)) {
			t.Errorf("%s is a proven read and the catalogue proposes it as a write", e.Tool)
		}
		if class, _ := authz.AssistOf(authz.Action(e.Tool)); class != authz.AssistApprove {
			t.Errorf("%s is %q to the assistant and a curated write proposes it", e.Tool, class)
		}
	}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite {
			continue
		}
		params, ok := served[tool.Act]
		if !ok && strings.Contains(tool.Act, ".") {
			// A VERB NO TOOL IS NAMED FOR (the principles'): the write is
			// sent on its own route, which internal/api's route walk holds.
			continue
		}
		if !ok {
			t.Errorf("%s executes as %s, which the operator catalogue does not serve",
				tool.Name, tool.Act)
			continue
		}
		destructive := builtin.AnnotationsFor(tool.Act).Destructive == crewletmcp.Yes
		if tool.Destructive != destructive {
			t.Errorf("%s states destructive %v and %s's annotation says %v", tool.Name,
				tool.Destructive, tool.Act, destructive)
		}
		if len(tool.Routes) != 1 || tool.Routes[0] != catalog.ActRoute {
			continue // the shares execute on the artifacts surface, not the act route
		}
		actProps, _ := params["properties"].(map[string]any)
		props, _ := tool.Parameters["properties"].(map[string]any)
		for arg, p := range props {
			if arg == catalog.RemovesArgument {
				continue
			}
			theirs, declared := actProps[arg].(map[string]any)
			if !declared {
				t.Errorf("%s takes %q, which %s does not declare", tool.Name, arg, tool.Act)
				continue
			}
			if !sameType(p.(map[string]any)["type"], theirs["type"]) {
				t.Errorf("%s takes %q as %v and %s as %v", tool.Name, arg,
					p.(map[string]any)["type"], tool.Act, theirs["type"])
			}
		}
	}
}

// A CURATED CHAT WRITE TAKES WHAT THE ROOM TAKES.
//
// The catalogue may not import internal/chatlet, a domain's writer, so the bounds
// its chat writes hold their arguments to are copies of the domain's, held to
// it here: a body, a topic or a purpose the schema admits is one the room
// stores, and a list of links, members or people one it takes, each link as
// long as the room takes one. Mutation: bound a chat write's body by the
// catalogue's own body bound, a link by a name's, or let a direct message reach
// one person more than a conversation holds, and this fails.
func TestACuratedChatWriteTakesWhatTheRoomTakes(t *testing.T) {
	t.Parallel()
	bound := func(tool, arg, key string) any {
		write, ok := catalog.Lookup(tool)
		if !ok {
			t.Fatalf("the catalogue has no %s", tool)
		}
		props, _ := write.Parameters["properties"].(map[string]any)
		schema, _ := props[arg].(map[string]any)
		// AN ENTRY'S BOUND IS ITS ITEMS': `links.maxLength` is how long one
		// link may be.
		if item, ok := strings.CutPrefix(key, "items."); ok {
			schema, _ = schema["items"].(map[string]any)
			key = item
		}
		return schema[key]
	}
	for _, c := range []struct {
		tool, arg, key string
		want           int
	}{
		{"post_chat_message", "body", "maxLength", chatlet.MaxBody},
		{"reply_in_chat_thread", "body", "maxLength", chatlet.MaxBody},
		{"send_direct_message", "body", "maxLength", chatlet.MaxBody},
		{"edit_chat_message", "body", "maxLength", chatlet.MaxBody},
		{"post_chat_message", "links", "maxItems", chatlet.MaxLinks},
		{"edit_chat_message", "links", "maxItems", chatlet.MaxLinks},
		{"post_chat_message", "links", "items.maxLength", chatlet.MaxLinkBytes},
		{"reply_in_chat_thread", "links", "items.maxLength", chatlet.MaxLinkBytes},
		{"send_direct_message", "links", "items.maxLength", chatlet.MaxLinkBytes},
		{"edit_chat_message", "links", "items.maxLength", chatlet.MaxLinkBytes},
		{"update_chat_room", "topic", "maxLength", chatlet.MaxTopic},
		{"update_chat_room", "purpose", "maxLength", chatlet.MaxPurpose},
		{"set_chat_room_members", "members", "maxItems", chatlet.MaxMembers},
		{"send_direct_message", "to", "maxItems", chatlet.MaxDMParticipants - 1},
	} {
		if got := bound(c.tool, c.arg, c.key); got != c.want {
			t.Errorf("%s holds %s to %s %v, and the room takes %d", c.tool, c.arg, c.key,
				got, c.want)
		}
	}
}

// THE ASSISTANT SAYS WHO A CHAT MESSAGE WAKES, AND THE CAP IT STATES IS THE
// ROOM'S.
//
// A model left to guess told a person the agents in a room "read room
// messages on their next turn", which is false: an agent that is not woken
// never reads the message. So the tool that posts and the tool that reads a
// room both state the routing, and the number of agents one `@channel` asks
// is a copy of the domain's cap the catalogue may not import, held to it
// here. Mutation: drop the paragraph from either tool, or move
// chatlet.MaxCollectiveRecipients without the catalogue's copy, and this
// fails.
func TestTheAssistantSaysWhoAChatMessageWakes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"post_chat_message", "get_chat_room"} {
		tool, ok := catalog.Lookup(name)
		if !ok {
			t.Fatalf("the catalogue has no %s", name)
		}
		for _, said := range []string{
			"mention (@handle) wakes the agent",
			"@channel asks every agent in the room to answer, up to " +
				strconv.Itoa(chatlet.MaxCollectiveRecipients),
			"wakes nobody else wakes one agent to answer it",
			"most senior agent member",
			"woken as news",
			"never reads the message later",
		} {
			if !strings.Contains(tool.Description, said) {
				t.Errorf("%s does not say %q:\n%s", name, said, tool.Description)
			}
		}
	}
}

// A CURATED CHAT WRITE SAYS WHAT A LINK IS. The room refuses a link that is
// not an absolute address in one of [chatlet.LinkSchemes], and the catalogue
// may not import internal/chatlet, so its chat writes state the rule in their
// own words, held to the room's set here: a model is told before it proposes
// a link, rather than the person meeting the refusal after approving the
// card. Mutation: drop mailto from the catalogue's rule, and every chat write
// that takes links fails.
func TestACuratedChatWriteSaysWhatALinkIs(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"post_chat_message", "reply_in_chat_thread",
		"send_direct_message", "edit_chat_message"} {
		write, ok := catalog.Lookup(tool)
		if !ok {
			t.Fatalf("the catalogue has no %s", tool)
		}
		props, _ := write.Parameters["properties"].(map[string]any)
		links, _ := props["links"].(map[string]any)
		said, _ := links["description"].(string)
		words := strings.FieldsFunc(said, func(r rune) bool { return r < 'a' || r > 'z' })
		if !slices.Contains(words, "absolute") {
			t.Errorf("%s describes its links as %q, which does not say a link is an "+
				"absolute address", tool, said)
		}
		for _, scheme := range chatlet.LinkSchemes() {
			if !slices.Contains(words, scheme) {
				t.Errorf("%s describes its links as %q, which does not say a link may "+
					"be %s", tool, said, scheme)
			}
		}
	}
}

// sameType reports whether a catalogue argument's type is one the act tool's
// schema admits: the same type, or one of a list it names (a nullable field).
func sameType(ours, theirs any) bool {
	if ours == theirs {
		return true
	}
	switch list := theirs.(type) {
	case []any:
		for _, t := range list {
			if t == ours {
				return true
			}
		}
	case []string:
		for _, t := range list {
			if t == ours {
				return true
			}
		}
	}
	return false
}
