package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlaswake"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
)

// CHATLET CONTRIBUTES BOTH HALVES OF THE INBOUND EDGE, each silent when
// missing: without the parser a company's messages commit and wake nobody;
// without the prompt, or with a zero `chatlet.Prompt{}` ([notify.NewPrompts]
// skips an empty Source), chat wakes render through the generic fallback,
// which names none of the tools a turn discharges its obligation with.
//
// The arm is reached on a runtime field, not a booted engine: the WIRING is
// under test, not [chatlet.NewStore]. And on a node holding no data too: every
// node pulls the inbound group, and a stateless node without the parser drops
// the wake unparsed; a parser reads only the record's routing snapshot.
func TestChatletContributesBothAParserAndAPrompt(t *testing.T) {
	t.Parallel()
	// A node's halves are read off its read sides ([Engine.nativeHalves]).
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

	// Set aside the arms every node registers ([Engine.nativeParsers]): the
	// halves' parsers are chat's and nothing else.
	var sources []string
	for _, p := range parsers {
		if !everyNodeRoutes(p.Source()) {
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
		if !everyNodeRoutes(p.Source()) {
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

	// The CONSTRUCTED prompt, not a `chatlet.Prompt{}`: the Source would
	// survive that, but a zero [notify.ChatPrompt.Address] reads a DM as a
	// room, so a person's consecutive messages land in as many turns, silently.
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

// everyNodeRoutes reports a source every node routes whatever halves it runs:
// the alarms', the access reviews', the atlas's and the artifacts' comments.
func everyNodeRoutes(source string) bool {
	return slices.Contains([]string{alarmwake.Source, accessreview.Source, atlaswake.Source,
		artifactsdomain.Source}, source)
}
