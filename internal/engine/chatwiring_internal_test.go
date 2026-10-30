package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
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
  sandbox: {fake: true}
  llm:
    gateway:
      model: sonnet
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

// rawChat appends a peer's chat records straight onto a running node's log, as
// another node's writer would publish them — signed, with NO expectation — and
// waits for this node's runner to commit each.
type rawChat struct {
	t       *testing.T
	running *runningLog
}

// peer is one record from another node, about subject.
func (r rawChat) peer(subject chatlet.Subject, op chatlet.OpKind, opID string,
	payload any) chatlet.MutationRecord {

	r.t.Helper()
	mutation, err := json.Marshal(payload)
	if err != nil {
		r.t.Fatal(err)
	}
	return chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			Writer: "node-peer", Scope: chatlet.ScopeSet{Subject: true},
		},
		Actor: "grace", ActorKind: chatlet.AuthorAgent, Mutation: mutation,
	}
}

// append publishes rec and reports where it landed, without waiting.
func (r rawChat) append(rec chatlet.MutationRecord) changefeed.Record {
	r.t.Helper()
	body, err := chatlet.Encode(rec)
	if err != nil {
		r.t.Fatalf("encode %s: %v", rec.OpID, err)
	}
	subject := topics.ChatLogSubject(string(rec.Subject.Kind), rec.Subject.ID)
	seq, _, err := r.running.log.Append(r.t.Context(), subject, rec.OpID, nil,
		r.running.signer.Seal(body))
	if err != nil {
		r.t.Fatalf("append %s: %v", rec.OpID, err)
	}
	return changefeed.Record{ID: rec.OpID, Position: seq, Stream: r.running.spec.Name,
		Key: subject, Payload: body}
}

// applied publishes rec and waits for this node's runner to commit it.
func (r rawChat) applied(rec chatlet.MutationRecord) changefeed.Record {
	r.t.Helper()
	out := r.append(rec)
	at := statelog.Position{Stream: r.running.spec.Name, Seq: out.Position}
	wait, cancel := context.WithTimeout(r.t.Context(), 20*time.Second)
	defer cancel()
	if err := r.running.runner.WaitCommitted(wait, at); err != nil {
		held, holds := r.running.runner.Deferred()
		fault, _ := r.running.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget))
		r.t.Fatalf("the node never applied %s at %d: %v (committed %s, holding %v %+v, "+
			"retrying %q)", rec.OpID, out.Position, err, r.running.runner.Committed(),
			holds, held, fault)
	}
	return out
}

// A RUNNING DATA NODE'S CHAT FEED WAKES BY WHAT ITS OWN APPLIER DECIDED.
//
// [chatlet.Translator] reads a verdict through the estate and the applier the
// engine hands it ([Engine.feedFor]), and a feed handed neither fails open: it
// derives every wake from the record alone, which is exactly the behaviour a
// refused post is wrong under — and indistinguishable from a working feed on
// every post that landed. So this is the node a company boots, its feed built
// as the native half builds it, and three posts its applier decided: one by a
// member of a private room, which wakes; one by somebody the room does not
// hold, which the applier refused and which wakes nobody; and one by that
// member after a peer archived the room — the archive a raw record on the
// room's own subject, with no expectation, as another node's writer publishes
// it — which the applier refused too.
//
// Mutation: hand the feed a zero estate in chatRegistrations, or no applier in
// feedFor, and the refused posts wake; let the applier land a post into an
// archived room and the third wakes.
func TestARunningChatletNodesFeedWakesByItsOwnApplier(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	running := e.core.Load().log.Domain(chatlet.Domain{}.Name())
	if running == nil {
		t.Fatal("the chat domain is not running")
	}
	translator, _, err := e.feedFor(running)
	if err != nil || translator == nil {
		t.Fatalf("the chat feed was not built: %v", err)
	}
	raw := rawChat{t: t, running: running}
	appended, peer := raw.applied, raw.peer
	const room = "9e1f0c3a-0000-4000-8000-0000000000a1"
	appended(peer(chatlet.ChannelSubject(room), chatlet.OpCreate, "op-create-room",
		chatlet.ChannelCreate{V: chatlet.DocumentVersion, ChannelID: room, Kind: chatlet.KindPrivate,
			Members: []chatlet.Member{{Handle: "grace"}, {Handle: "ada"}}, CreatedBy: "grace",
			CreatedByKind: chatlet.AuthorAgent}))
	post := func(author, messageID string) changefeed.Record {
		rec := peer(chatlet.MessageSubject(room), chatlet.OpPost, messageID, chatlet.MessagePost{
			V: chatlet.DocumentVersion, MessageID: messageID, Body: "@ada are you there?",
			Mentions: []string{"ada"}, Author: author, AuthorKind: chatlet.AuthorAgent,
			MembersOnly: true})
		rec.Actor = author
		rec.Notify = &chatlet.Notify{MessageID: messageID, ChannelID: room,
			ChannelKind: chatlet.KindPrivate, Author: author, AuthorKind: chatlet.AuthorAgent,
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
	archived := true
	appended(peer(chatlet.ChannelSubject(room), chatlet.OpPatch, "op-archive-room",
		chatlet.ChannelPatch{V: chatlet.DocumentVersion, Archived: &archived}))
	if _, wake, err := translator.Translate(t.Context(),
		post("grace", "01a2b3c4-0000-7000-8000-0000000000b3")); err != nil || wake {
		t.Errorf("a post into a room a peer archived ahead of it — which this node's "+
			"applier refused — woke somebody (%v)", err)
	}
}

// A RECORD NAMING A ROOM THIS NODE HOLDS NO ROW FOR IS HELD, NEVER COMMITTED —
// and that is the one shape in which a raw peer patch on a running node is.
//
// Round 4 of the chat series met a raw ChannelPatch, appended on a room's own
// subject with no expectation, that this node's runner never committed: the
// wait hung for ten minutes while the same record applied in the chat write
// harness. Reproduced here, it is not the expectation, the subject's
// arbitration, a gate or how the record is filed — the archive above is that
// same record and commits — but the ROW: a patch whose room this node does not
// hold is, under a strict replay, a record this build applied incorrectly (its
// create is below it on every node), so the applier refuses it and the runner
// retries it in place, holding the checkpoint below it, and reports it as its
// Fault once [statelog.ApplyRetryBudget] has passed — the path a stall takes
// to take this copy out of service. It is neither retained nor skipped,
// because a skip would leave this node's rows short of every peer's for good.
// The harness applied it because its room existed there, created through the
// store; the engine test's patch named a room no create had made on the node.
// So the hang was the test's shape and the framework's designed answer to it,
// not a product fault.
//
// Mutation: make the chat applier skip a patch of a missing room (answer 0
// rows rather than the error) and the record commits.
func TestARawPatchOfARoomTheNodeHoldsNoRowForIsHeldNotCommitted(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	running := e.core.Load().log.Domain(chatlet.Domain{}.Name())
	if running == nil {
		t.Fatal("the chat domain is not running")
	}
	raw := rawChat{t: t, running: running}
	const room = "9e1f0c3a-0000-4000-8000-0000000000c1"
	created := raw.applied(raw.peer(chatlet.ChannelSubject(room), chatlet.OpCreate,
		"op-create-room", chatlet.ChannelCreate{V: chatlet.DocumentVersion, ChannelID: room,
			Kind: chatlet.KindPublic, Name: "launch", CreatedBy: "grace",
			CreatedByKind: chatlet.AuthorAgent}))

	archived := true
	ghost := raw.append(raw.peer(chatlet.ChannelSubject("9e1f0c3a-0000-4000-8000-0000000000ff"),
		chatlet.OpPatch, "op-archive-ghost",
		chatlet.ChannelPatch{V: chatlet.DocumentVersion, Archived: &archived}))
	at := statelog.Position{Stream: running.spec.Name, Seq: ghost.Position}
	wait, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := running.runner.WaitCommitted(wait, at); err == nil {
		t.Fatal("a patch of a room this node holds no row for was committed — the " +
			"applier skipped what is, under a strict replay, its own fault")
	}
	if got := running.runner.Committed(); got.Seq != created.Position {
		t.Errorf("the checkpoint is %s, want it held at the create (%d) below the "+
			"patch it cannot apply", got, created.Position)
	}
	if held, holds := running.runner.Deferred(); holds {
		t.Errorf("the runner retained the patch (%+v): a record it can decode and "+
			"cannot apply is retried in place, never set aside", held)
	}
	fault, faulted := running.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget))
	if !faulted || !strings.Contains(fault, "no row for") {
		t.Errorf("the runner reports %q (%v) once its retry budget passes, want the "+
			"applier's own refusal of a room it holds no row for", fault, faulted)
	}
}
