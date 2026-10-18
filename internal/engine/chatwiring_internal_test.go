package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// A RUNNING DATA NODE ON CHATLET HANDS ITS SEATS EVERY CHAT TOOL.
//
// The engine's chat seams are assembled in one place and handed to the tools
// in another ([Engine.equip]), and a seam left out of the second is not an
// error anywhere: every chat tool is gated on the half it needs, so a
// registry equipped with an empty [builtin.ChatDeps] is a registry with no
// chat tools at all — the domain running, its records applied, its seats woken
// about messages they have nothing to answer with. So this is the node a
// company actually boots, and the tools are read off the registry its epoch
// was equipped with.
func TestARunningChatletNodeEquipsEveryChatTool(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	c := e.Company()
	if c == nil || c.Tools == nil {
		t.Fatal("the node published no equipped company")
	}
	for _, name := range []string{
		builtin.PostMessageTool, builtin.ReplyInThreadTool, builtin.SendDMTool,
		builtin.ReadChannelTool, builtin.SearchMessagesTool, builtin.ListChannelsTool,
		builtin.ReactToMessageTool, builtin.JoinChannelTool, builtin.LeaveChannelTool,
	} {
		if _, ok := c.Tools.Lookup(name); !ok {
			t.Errorf("a data node running Chatlet equipped its seats with no %s", name)
		}
	}
}

// A RUNNING DATA NODE ARMS THE PRUNE, AND ITS STOP ENDS IT.
//
// [TestTheChatPruneIsArmedOnlyWhereTheRoomsAre] holds the duty's own gate;
// this holds the two lines that reach it. Armed by nothing, a company's
// message horizon validates, is documented, and deletes nothing for the life
// of the deployment. Left running at the stop, the duty publishes into a log
// whose appliers the teardown is closing — and it runs before the core's, so
// the order is the teardown's own rather than this case's.
func TestARunningChatletNodeArmsThePruneAndItsStopEndsIt(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	if e.chatPrune == nil {
		t.Fatal("a publishing data node running Chatlet armed no prune duty")
	}
	e.Stop(context.Background())
	if e.chatPrune != nil {
		t.Fatal("the node's stop left the chat prune duty armed")
	}
}

// A NODE HOLDING NO DATA RUNS THE CHAT HALF ITS COMPANY DECLARES.
//
// A stateless node keeps no copy of chat, so its seats reach it through the
// router — but whether they reach it at all is the record [Engine.startRemote]
// keeps of which halves the company runs natively, and a half it forgot is a
// seat with no chat tools on that node and a working set on every data node,
// which reads as a seat that is merely quiet. A company on a vendor's chat has
// no native half to reach, here or anywhere.
func TestANodeHoldingNoDataRunsTheChatHalfItsCompanyDeclares(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    gateway:
      agent: codex
      model: gpt-4o
chat:
  backend: CHAT
roles:
  - name: Dev
    handle: dev
`
	for backend, want := range map[string]bool{"chatlet": true, "none": false} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			e, _ := statelessOver(t)
			if err := e.startRemote(companyFor(t, strings.Replace(doc, "CHAT", backend, 1))); err != nil {
				t.Fatalf("start the stateless runtime: %v", err)
			}
			runs := e.remote.Load()
			if runs == nil || runs.chat != want {
				t.Fatalf("a stateless node on chat.backend %s runs %+v, want chat=%v",
					backend, runs, want)
			}
			if _, ok := e.chatHalves(); ok != want {
				t.Errorf("a stateless node on chat.backend %s hands its seats chat "+
					"halves=%v, want %v", backend, ok, want)
			}
		})
	}
}
