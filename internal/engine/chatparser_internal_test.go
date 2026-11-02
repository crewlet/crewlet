package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
)

// CHATLET CONTRIBUTES BOTH HALVES OF THE INBOUND EDGE, and each half is
// silent when it is missing.
//
// A wake is only ever delivered to the source whose PARSER registered it, and
// only ever rendered by the PROMPT whose Source matches. Drop the parser arm
// and a company's messages commit, feed and wake nobody — every seat simply
// never hears about its own rooms. Drop the prompt arm, or write it as a zero
// `chatlet.Prompt{}`, and [notify.NewPrompts] skips it on an empty Source: every
// chat wake then renders through the generic fallback, which names none of the
// tools a turn discharges its obligation with. Neither failure logs anything.
//
// The arm is reached on a runtime field rather than a booted engine, because
// what is under test is the WIRING and not the store: [chatlet.NewStore]'s own
// refusals are its package's.
//
// AND ON A NODE THAT HOLDS NO DATA TOO. Every node pulls the inbound group, so
// a chat wake lands on a stateless node as often as on a data node — and one
// with no chat parser acknowledges it unparsed and drops it, so the seat it was
// for is never woken. A parser reads the record's own routing snapshot and
// never a row, so the stateless node routes it exactly as a data node does.
func TestChatletContributesBothAParserAndAPrompt(t *testing.T) {
	t.Parallel()
	// BOTH SIDES, as a data node running Chatlet builds them: the halves a
	// node runs are read off its read sides ([Engine.nativeHalves]).
	data := &Engine{}
	data.native.Store(chatRuntime(&native{}))
	stateless := &Engine{}
	stateless.remote.Store(&remoteNative{chat: true})
	for name, e := range map[string]*Engine{
		"a data node":            data,
		"a node holding no data": stateless,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			chatContributesBothHalves(t, e)
		})
	}
}

// chatContributesBothHalves holds one node's native parsers and prompts to
// chat's, and to nothing else.
func chatContributesBothHalves(t *testing.T, e *Engine) {
	t.Helper()
	parsers, prompts := e.nativeParsers(&Company{})

	// THE ALARMS' ARM ASIDE: every node registers it whatever halves its
	// company runs ([Engine.nativeParsers]), so what this case holds is that
	// the halves' parsers are chat's and nothing else.
	var sources []string
	for _, p := range parsers {
		if p.Source() != alarmwake.Source {
			sources = append(sources, p.Source())
		}
	}
	if len(sources) != 1 || sources[0] != chatlet.Source {
		t.Fatalf("a Chatlet node registered parsers for %v, want exactly "+
			"[%q] — a company whose chat has no parser commits every message "+
			"and wakes nobody at all", sources, chatlet.Source)
	}

	sources = nil
	var chats []notify.Prompt
	for _, p := range prompts {
		if p.Source() != alarmwake.Source {
			sources = append(sources, p.Source())
			chats = append(chats, p)
		}
	}
	if len(sources) != 1 || sources[0] != chatlet.Source {
		t.Fatalf("a Chatlet node registered prompts for %v, want exactly "+
			"[%q] — a wake whose source no prompt answers for renders through "+
			"the generic fallback, which names none of the tools a turn "+
			"discharges its obligation with", sources, chatlet.Source)
	}

	// AND IT IS THE CONSTRUCTED PROMPT rather than a `chatlet.Prompt{}`. The
	// source would survive that — the package declares Source on the type
	// for exactly this reason — but the EMBEDDED value would not: a zero
	// [notify.ChatPrompt.Address] reads a direct conversation as an
	// ordinary room, so a person's consecutive messages in a DM land in as
	// many turns as they typed and nothing anywhere reports it.
	built, isChats := chats[0].(chatlet.Prompt)
	if !isChats {
		t.Fatalf("the chat prompt is a %T, which is not this package's", chats[0])
	}
	if len(built.Address.DirectKinds) != len(chatlet.AddressRule().DirectKinds) {
		t.Errorf("the registered prompt addresses %v as direct and the package "+
			"says %v — the zero rule reads a DM as a room",
			built.Address.DirectKinds, chatlet.AddressRule().DirectKinds)
	}
}
