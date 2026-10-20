package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RUNNING DATA NODE ON NATIVE CHAT HANDS ITS SEATS EVERY CHAT TOOL.
//
// The engine's chat seams are assembled in one place and handed to the tools
// in another ([Engine.equip]), and a seam left out of the second is not an
// error anywhere: every chat tool is gated on the half it needs, so a
// registry equipped with an empty [builtin.ChatDeps] is a registry with no
// chat tools at all — the domain running, its records applied, its seats woken
// about messages they have nothing to answer with. So this is the node a
// company actually boots, and the tools are read off the registry its epoch
// was equipped with.
func TestARunningNativeChatNodeEquipsEveryChatTool(t *testing.T) {
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
			t.Errorf("a data node running native chat equipped its seats with no %s", name)
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
func TestARunningNativeChatNodeArmsThePruneAndItsStopEndsIt(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	if e.chatPrune == nil {
		t.Fatal("a publishing data node running native chat armed no prune duty")
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
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
chat:
  backend: CHAT
roles:
  - name: Dev
    handle: dev
`
	for backend, want := range map[string]bool{"native": true, "none": false} {
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

// A RUNNING DATA NODE'S CHAT FEED WAKES BY WHAT ITS OWN APPLIER DECIDED.
//
// [chat.Translator] reads a verdict through the estate and the applier the
// engine hands it ([Engine.feedFor]), and a feed handed neither fails open: it
// derives every wake from the record alone, which is exactly the behaviour a
// refused post is wrong under — and indistinguishable from a working feed on
// every post that landed. So this is the node a company boots, its feed built
// as the native half builds it, and two posts its applier decided: one by a
// member of a private room, which wakes, and one by somebody the room does not
// hold, which the applier refused and which wakes nobody.
//
// Mutation: hand the feed a zero estate in chatRegistrations, or no applier in
// feedFor, and the refused post wakes.
func TestARunningNativeChatNodesFeedWakesByItsOwnApplier(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	running := e.core.Load().log.Domain(chat.Domain{}.Name())
	if running == nil {
		t.Fatal("the chat domain is not running")
	}
	translator, _, err := e.feedFor(running)
	if err != nil || translator == nil {
		t.Fatalf("the chat feed was not built: %v", err)
	}

	appended := func(rec chat.MutationRecord) changefeed.Record {
		t.Helper()
		body, err := chat.Encode(rec)
		if err != nil {
			t.Fatalf("encode %s: %v", rec.OpID, err)
		}
		subject := topics.ChatLogSubject(string(rec.Subject.Kind), rec.Subject.ID)
		seq, _, err := running.log.Append(t.Context(), subject, rec.OpID, nil,
			running.signer.Seal(body))
		if err != nil {
			t.Fatalf("append %s: %v", rec.OpID, err)
		}
		at := statelog.Position{Stream: running.spec.Name, Seq: seq}
		wait, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		if err := running.runner.WaitCommitted(wait, at); err != nil {
			held, holds := running.runner.Deferred()
			t.Fatalf("the node never applied %s at %d: %v (committed %s, holding %v %+v)",
				rec.OpID, seq, err, running.runner.Committed(), holds, held)
		}
		return changefeed.Record{ID: rec.OpID, Position: seq, Stream: running.spec.Name,
			Key: subject, Payload: body}
	}
	peer := func(subject chat.Subject, op chat.OpKind, opID string, payload any) chat.MutationRecord {
		t.Helper()
		mutation, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return chat.MutationRecord{
			RecordEnvelope: chat.RecordEnvelope{
				V: chat.RecordVersion, OpID: opID, Subject: subject, Op: op,
				Writer: "node-peer", Scope: chat.ScopeSet{Subject: true},
			},
			Actor: "grace", ActorKind: chat.AuthorAgent, Mutation: mutation,
		}
	}
	const room = "9e1f0c3a-0000-4000-8000-0000000000a1"
	appended(peer(chat.ChannelSubject(room), chat.OpCreate, "op-create-room",
		chat.ChannelCreate{V: chat.DocumentVersion, ChannelID: room, Kind: chat.KindPrivate,
			Members: []chat.Member{{Handle: "grace"}, {Handle: "ada"}}, CreatedBy: "grace",
			CreatedByKind: chat.AuthorAgent}))
	post := func(author, messageID string) changefeed.Record {
		rec := peer(chat.MessageSubject(room), chat.OpPost, messageID, chat.MessagePost{
			V: chat.DocumentVersion, MessageID: messageID, Body: "@ada are you there?",
			Mentions: []string{"ada"}, Author: author, AuthorKind: chat.AuthorAgent,
			MembersOnly: true})
		rec.Actor = author
		rec.Notify = &chat.Notify{MessageID: messageID, ChannelID: room,
			ChannelKind: chat.KindPrivate, Author: author, AuthorKind: chat.AuthorAgent,
			Excerpt: "are you there?"}
		return appended(rec)
	}

	if _, wake, err := translator.Translate(t.Context(),
		post("grace", "01a2b3c4-0000-7000-8000-0000000000b1")); err != nil || !wake {
		t.Errorf("a post the node's applier applied woke nobody (%v)", err)
	}
	if _, wake, err := translator.Translate(t.Context(),
		post("mallory", "01a2b3c4-0000-7000-8000-0000000000b2")); err != nil || wake {
		t.Errorf("a post by somebody the private room does not hold — which this node's "+
			"applier refused — woke somebody (%v)", err)
	}
}
