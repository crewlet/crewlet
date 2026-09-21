package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The company's own chat, end to end.
//
// Everything below this level has its own suite: internal/chat certifies the
// records, the applier, the write authority and the reader against fixtures;
// internal/api certifies the routes and the visibility filter; internal/search
// certifies the index. What none of them can reach is the composition, and for
// this domain the composition is where the interesting claims live — because
// chat is the first domain whose correctness is a statement about SEVERAL
// NODES rather than about one:
//
//   - The wake is derived from the committed record by a fleet-wide consumer,
//     so the node that wins a message is rarely the node running the seat that
//     gets woken. Nothing below this level has two nodes.
//   - The per-channel sequence is MINTED BY THE APPLIER from log order. It is
//     the one value in this domain no writer carries, so two nodes agreeing
//     about it is not a property any unit can assert — and a divergence in it
//     is permanent.
//   - The retention horizon is enforced by a prune RECORD carrying the
//     broker's own instant, precisely so that "older than a year" is one
//     decision rather than each node's local clock. Only a fleet can show that
//     the same rows went.
//   - The delivery obligation is SOURCE-SCOPED: a turn woken in a room owes an
//     answer IN a room, and the three posting tools are the only ones that
//     discharge it. That runs through the real trigger, the real tool registry
//     and the real write path or it runs nowhere.

// ---- what a person and an operator write as ---------------------------- //

// person is a human seat writing as themselves — the shape a message from a
// colleague takes. The handle IS the identity: a caller may never name a seat
// on any chat surface, so every actor here is one the harness already is.
func person(handle string) chat.Actor {
	return chat.Actor{Handle: handle, Kind: chat.AuthorHuman}
}

// chatOperator is the same person acting through a credential, which is what
// the retention duty and `crewlet chat prune` both write as. The kind is what
// the prune's own guard reads — a seat that could prune a room could delete a
// year of the company's decisions as a turn's side effect.
func chatOperator(handle string) chat.Actor {
	return chat.Actor{Handle: handle, Kind: chat.AuthorOperator, OperatorID: "e2e"}
}

// room creates a room on this node and waits for this node to apply it.
//
// THE WAIT IS PART OF THE GESTURE rather than the caller's to remember: a post
// into a room the applier has not written yet is refused inside the decide —
// deliberately, because under a strict replay the create sits below the post
// on the same log — so a caller that skipped it would get an intermittent
// "no such record" that says nothing about what it was testing.
func room(t *testing.T, n *node, actor chat.Actor, in chat.NewChannel) chat.Written {
	t.Helper()
	written, err := n.engine.ChatStore().CreateChannel(t.Context(), actor, in)
	if err != nil {
		t.Fatalf("create #%s: %v", in.Name, err)
	}
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		t.Fatalf("creating #%s resolved to %q on the node that made it",
			in.Name, written.Outcome.Outcome)
	}
	if err := n.engine.WaitCommitted(t.Context(), written.Outcome.Position); err != nil {
		t.Fatalf("wait for #%s to be applied: %v", in.Name, err)
	}
	return written
}

// say posts one message and reports what was written.
//
// The operation id is the caller's own, NOT a fresh uuid: a person's message
// id is derived from (channel, author, operation id), so a stable key here is
// what lets a case name the message it is about before it has been written —
// see [chat.PersonMessageID] and [TestASeatAnswersWhereItWasAsked].
func say(t *testing.T, n *node, actor chat.Actor, channelID, opID, body string,
	mentions ...string) chat.Written {

	t.Helper()
	written, err := n.engine.ChatStore().Post(t.Context(), actor, channelID,
		chat.NewMessage{Body: body, Mentions: mentions, OperationID: opID})
	if err != nil {
		t.Fatalf("post to %s: %v", channelID, err)
	}
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		t.Fatalf("the post resolved to %q on the node that made it",
			written.Outcome.Outcome)
	}
	return written
}

// transcript is one room as this node holds it, newest first.
//
// SESSION, which is the level that answers for this node's own applied rows:
// every caller here has already waited for the position it cares about, and a
// linearizable read would add a barrier append per assertion while answering
// the same question.
func transcript(t *testing.T, n *node, viewer, channelID string) chat.Transcript {
	t.Helper()
	page, err := n.engine.Chat().Messages(t.Context(), viewer,
		chat.TranscriptQuery{ChannelID: channelID},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read %s back: %v", channelID, err)
	}
	return page
}

// bodies is the transcript rendered as "seq author: body", oldest first, which
// is what a failure has to print: a message id names nothing a reader can act
// on, and the SEQUENCE is half of what these cases are about.
func bodies(page chat.Transcript) []string {
	out := make([]string, 0, len(page.Messages))
	for _, view := range slices.Backward(page.Messages) {
		out = append(out, fmt.Sprintf("%d %s: %s",
			view.ChannelSeq, view.Message.Author, view.Message.Body))
	}
	return out
}

// fleetCoordination puts this fleet's LEASES where a fleet's leases go.
//
// [config.CoordinationLocal] is the default and this harness has always taken
// it, which for a cluster case is exactly what the config layer refuses by
// name: leases kept in this process mean every member holds every lease and
// runs every seat, so a two-member fleet has TWO nodes owning `ceo` and two
// consumers on one mailbox. Measured here, with both members reporting
// `Held() == [ceo]` and each one's `ListLive(ClassNode)` naming only itself.
//
// A case whose subject is which node a wake reaches therefore cannot be
// written against the default, and one written anyway would pass or fail on
// which consumer happened to win — so this asks for the replicated KV the
// engine really coordinates through.
//
// It is ONE case's amendment rather than the harness's default, and that is a
// deferral rather than a decision: every other cluster case's subject is the
// state log, which is shared either way, so none of them notices — but the
// harness itself is standing up a shape `crewlet validate` refuses, and moving
// it is a decision about [fleetSize] too, because the config layer permits
// embedded-kv at ONE node or at THREE and refuses two by name (two members
// have no coordination quorum, so the fleet stops serving the moment either
// restarts). This amendment takes the two-member shape anyway, which is honest
// for a case that never restarts a member and would not be for the harness.
func fleetCoordination(boot *config.Bootstrap) {
	boot.Coordination.Type = config.CoordinationEmbeddedKV
}

// ---- a message written on one node, and a fleet that agrees about it --- //

// A MESSAGE POSTED ON ONE NODE WAKES A SEAT OWNED BY ANOTHER, BOTH NODES END
// UP WITH THE SAME TRANSCRIPT, AND A PRUNE TAKES THE SAME ROWS FROM BOTH.
//
// Three claims in one fleet, on [TestAFleetAgreesAboutOneCompany]'s terms:
// standing a second cluster up costs two more embedded brokers and four more
// databases, and the arms do not interfere — the first two share a message and
// the third destroys it, which is the only order they can run in anyway.
//
//  1. THE WAKE CROSSES THE FLEET. This is the whole reason a wake is derived
//     from the durable record by a fleet-wide consumer rather than published
//     by the writer's own goroutine: the node that wins a feed message is
//     rarely the node running the seat that has to answer. The message is
//     therefore posted on the node that does NOT hold the seat, and the
//     assertion is that the OTHER node's model ran a turn.
//  2. THE TWO NODES MINT ONE SEQUENCE. `channel_seq` is the one value in this
//     domain that no record carries — the applier mints it from log order —
//     so two nodes agreeing about it is exactly what cannot be asserted
//     anywhere below a fleet, and a divergence in it is permanent: every later
//     post in the room inherits the wrong number.
//  3. A PRUNE TAKES THE SAME ROWS EVERYWHERE. The horizon is enforced by a
//     record carrying a cutoff INSTANT rather than by a sweep reading each
//     node's own clock, and this is the difference between the two: a sweep
//     would delete a different set on every node, for ever, and nothing would
//     ever report it.
func TestAMessageOnOneNodeWakesASeatOnAnotherAndBothNodesAgree(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize, fleetCoordination)
	c.hydrated(t)

	// WHICH MEMBER HOLDS THE SEAT IS NOT DECIDED BY THIS TEST. Placement
	// is a greedy claim, so the holder is whichever member got there
	// first — and the whole point of the first arm is to post somewhere
	// else, which means finding out rather than assuming.
	var holder, writer *node
	waitFor(t, "a member of the fleet to claim the ceo seat", func() bool {
		for _, n := range c.nodes {
			if slices.Contains(n.engine.Node().Host().Held(), "ceo") {
				holder = n
				return true
			}
		}
		return false
	})
	for _, n := range c.nodes {
		if n != holder {
			writer = n
		}
	}
	if writer == nil {
		t.Fatalf("a fleet of %d left nobody to post from", fleetSize)
	}
	// AND EXACTLY ONE MEMBER HOLDS IT, which is the premise the whole
	// first arm rests on. Checked rather than assumed because the way it
	// fails is not a failure: with leases kept per node both members own
	// the seat, both attach its mailbox, and the case then passes or
	// fails on which consumer happened to win the one wake — a coin flip
	// dressed as an assertion. See [fleetCoordination].
	if slices.Contains(writer.engine.Node().Host().Held(), "ceo") {
		t.Fatalf("both %s and %s hold the ceo seat, so there is no node this "+
			"wake has to cross to — coordination is not shared across this "+
			"fleet", holder.id, writer.id)
	}

	created := room(t, writer, person("founder"), chat.NewChannel{
		Name: "rollbacks", Kind: chat.KindPublic,
		Topic:   "what to do when a deploy hangs",
		Members: []chat.Member{{Handle: "ceo"}},
	})
	made := created.Channel
	// THE PEER APPLIES THE ROOM TOO, before anything is said in it. The
	// assertions below read the room on both members, and a room one of
	// them has not written yet is a missing create rather than a
	// divergence — which would fail this case for the wrong reason.
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), created.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the room: %v", i, err)
		}
	}

	first := say(t, writer, person("founder"), made.ID, "fleet-chat-1",
		"@ceo the staging deploy hung on rollback again", "ceo")
	second := say(t, writer, person("founder"), made.ID, "fleet-chat-2",
		"and the node never drained")

	// (1) THE SEAT THE OTHER MEMBER RUNS WAKES. Its own scripted endpoint
	// is the evidence: each member has one, and only the member running
	// the seat can have called it.
	waitFor(t, "the seat on the other member to run a turn", func() bool {
		return slices.Contains(holder.model.seen(), "execute")
	}, func() string {
		return fmt.Sprintf("holder=%s saw %v; writer=%s saw %v",
			holder.id, holder.model.seen(), writer.id, writer.model.seen())
	})

	// (2) AND BOTH MEMBERS HOLD THE SAME TRANSCRIPT, sequence included.
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), second.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the second message: %v", i, err)
		}
	}
	want := transcript(t, c.nodes[0], "founder", made.ID)
	if len(want.Messages) != 2 {
		t.Fatalf("member 0 holds %v for a room two messages were said in",
			bodies(want))
	}
	// THE SEQUENCE IS CONTIGUOUS AND STARTS AT ONE, which is what a client
	// holding 41 and handed 43 relies on to ask for 42 rather than for the
	// whole room. Asserted against the ORDER THEY WERE SAID IN, so a
	// sequence minted from anything but log order fails here.
	byID := map[string]int64{}
	for _, view := range want.Messages {
		byID[view.Message.ID] = view.ChannelSeq
	}
	if got := byID[first.Message.ID]; got != 1 {
		t.Errorf("the first message sits at channel_seq %d: %v", got, bodies(want))
	}
	if got := byID[second.Message.ID]; got != 2 {
		t.Errorf("the second message sits at channel_seq %d: %v", got, bodies(want))
	}
	for i, n := range c.nodes[1:] {
		got := transcript(t, n, "founder", made.ID)
		if !slices.Equal(bodies(got), bodies(want)) {
			t.Errorf("member %d holds %v and member 0 holds %v — the sequence "+
				"is minted by each node's own applier from log order, so two "+
				"members disagreeing about it never converges and every later "+
				"post in the room inherits the difference",
				i+1, bodies(got), bodies(want))
		}
	}
	// AND EVERY COLUMN, not only the ones a transcript renders: the rows
	// are identity-claimed, so a difference in an author kind, an edit
	// stamp or a thread root is the same failure one column later.
	if got, first := digestTable(t, c.nodes[1], "chat_messages"),
		digestTable(t, c.nodes[0], "chat_messages"); got != first {
		t.Errorf("chat_messages digests %s on member 1 and %s on member 0", got, first)
	}

	// (3) AND A PRUNE TAKES THE SAME ROWS FROM BOTH.
	//
	// THE CUTOFF IS READ OFF THE CLOCK ONLY AFTER BOTH MEMBERS HAVE
	// APPLIED, so every message above is stored strictly before it — the
	// instants on the rows are the BROKER's, and this is the one place
	// that relationship has to hold for the case to mean anything.
	pruned, err := c.nodes[0].engine.ChatStore().Prune(t.Context(),
		chatOperator("founder"), made.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), pruned.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the prune: %v", i, err)
		}
		if left := transcript(t, n, "founder", made.ID); len(left.Messages) != 0 {
			t.Errorf("member %d still holds %v below the cutoff — a horizon "+
				"that removed a different set on each node would be permanent "+
				"and nothing would report it", i, bodies(left))
		}
	}
	if got, first := digestTable(t, c.nodes[1], "chat_messages"),
		digestTable(t, c.nodes[0], "chat_messages"); got != first {
		t.Errorf("after the prune chat_messages digests %s on member 1 and %s "+
			"on member 0", got, first)
	}
}

// ---- a private room, at the socket ------------------------------------- //

// peopleDoc adds two people to the golden company, each bound to one of Tier
// A's own tokens.
//
// A PERSON IN CHAT IS A SEAT. The server resolves a bearer token to the
// `kind: human` seat whose `contact.crewlet_operator_id` names it, and a
// caller may never name a seat — so a case about who may read a room needs
// both halves of that binding, in both tiers, or it is not testing the rule at
// all.
//
// THE FIRST `roles:` IS THE SEATS' — see [companyDoc] for why the units are
// last and why that matters to every fixture that patches this document.
func peopleDoc(doc string) string {
	return strings.Replace(doc, "roles:\n", "roles:\n"+
		"  - name: Alice\n"+
		"    kind: human\n"+
		"    contact:\n"+
		"      crewlet_operator_id: alice\n"+
		"  - name: Bob\n"+
		"    kind: human\n"+
		"    contact:\n"+
		"      crewlet_operator_id: bob\n", 1)
}

// peopleTokens is the Tier A half of the same binding.
func peopleTokens(boot *config.Bootstrap) {
	boot.API.Auth.Tokens = []config.APIToken{
		{ID: "alice", Token: "alice-token"},
		{ID: "bob", Token: "bob-token"},
	}
}

// ask puts one question on the socket's query channel and waits for its
// answer.
//
// THE TOKEN RIDES THE FRAME, which is the socket's own rule: a browser cannot
// set a header on a WebSocket constructor, so a socket opened for anonymous
// reads carries its credential per question. It is what lets ONE connection
// ask the same question as two different people, which is exactly the
// comparison this file needs — two sockets would leave "did the filter run" and
// "did this connection see anything at all" indistinguishable.
func ask(t *testing.T, conn *websocket.Conn, id int64, what, token string,
	params map[string]any) (json.RawMessage, string) {

	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"kind": "query", "id": id, "what": what, "token": token, "params": params,
	})
	if err != nil {
		t.Fatalf("encode the %s query: %v", what, err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatalf("send the %s query: %v", what, err)
	}
	// READ PAST EVERY OTHER FRAME. The socket is a live channel: a health
	// tick, a roster and an event row all arrive unasked, and the answer is
	// correlated by the id this question carried and by nothing else.
	deadline, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	for {
		_, raw, err := conn.Read(deadline)
		if err != nil {
			t.Fatalf("read the answer to %s: %v", what, err)
		}
		var env struct {
			Kind  string          `json:"kind"`
			ID    int64           `json:"id"`
			Data  json.RawMessage `json:"data"`
			Error string          `json:"error"`
		}
		if json.Unmarshal(raw, &env) != nil || env.ID != id {
			continue
		}
		if env.Kind == stream.KindError {
			return nil, env.Error
		}
		return env.Data, ""
	}
}

// A PRIVATE ROOM IS NOT SERVED TO SOMEBODY WHO IS NOT IN IT, ASKED AT THE
// SOCKET.
//
// # Why this is not a unit test of the filter
//
// Because every layer between the filter and the wire is one that can lose it,
// and each has done so somewhere in this tree's history: a credential that
// resolves to no seat, a question registered without its operator gate, a
// transport that reduces a refusal to a code that reads as an empty room. The
// only place all of them are live at once is a real socket on a real node, and
// the only honest shape for the assertion is a COMPARISON — the member is
// served the very message the non-member is refused, on one connection, in the
// same breath.
//
// Without the member's half this case would pass identically against a node
// that served nobody anything.
//
// # And why it is the socket's QUERY channel rather than its push
//
// Because the push does not reach a socket at all on a running node, and an
// absence assertion over a path nothing feeds is the worst shape a test can
// take: it passes identically when the filter works and when the frames were
// never going to arrive. [stream.ChatHub] is the applier's [chat.Observer] and
// [api.App.ChatLive] exposes it "for the engine" — but `applierFor` in
// internal/engine builds `chat.NewApplier(nodeID, nil)`, [engine.Options] has
// no field that could carry a hub, and nothing in cmd/crewlet builds one. So
// no committed chat record becomes a frame anywhere, and the live channel this
// asserts against is the one every chat read already travels: the query
// channel, which is a thin adapter over the same function each REST route
// calls. When the observer is wired, the push filter gets its own case here
// and this one keeps its own subject.
func TestAPrivateRoomIsNotServedToANonMemberOverTheSocket(t *testing.T) {
	t.Parallel()
	n := startWith(t, peopleDoc, peopleTokens)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	// ALICE'S ROOM, AND ALICE IS ITS ONLY MEMBER. A create puts its author
	// in the room it made, so nothing here adds her explicitly.
	made := room(t, n, person("alice"), chat.NewChannel{
		Name: "founders", Kind: chat.KindPrivate,
		Topic: "things not everybody should read",
	}).Channel
	said := say(t, n, person("alice"), made.ID, "socket-chat-1",
		"the term sheet lands on Thursday")
	if err := n.engine.WaitCommitted(t.Context(), said.Outcome.Position); err != nil {
		t.Fatalf("wait for the message to be applied: %v", err)
	}

	conn := n.dial(t)
	params := map[string]any{"channel_id": made.ID}

	// THE MEMBER'S HALF FIRST, so the refusal below is falsifiable.
	data, code := ask(t, conn, 1, "chat_messages", "alice-token", params)
	if code != "" {
		t.Fatalf("a member of #%s was refused her own room: %s", made.Name, code)
	}
	var page chat.Transcript
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode the transcript: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Message.Body != said.Message.Body {
		t.Fatalf("the member was served %v", bodies(page))
	}

	// AND THE NON-MEMBER IS TOLD THE ROOM IS NOT THERE, rather than told
	// it is not his. A private room's EXISTENCE is information — who is
	// talking to whom is most of what a conversation discloses — so the
	// two answers have to read identically from outside.
	if _, code := ask(t, conn, 2, "chat_messages", "bob-token", params); code != stream.CodeNotFound {
		t.Errorf("a non-member asking for #%s was answered %q, not %q — and if "+
			"he was served the page, a transcript has left this deployment",
			made.Name, code, stream.CodeNotFound)
	}

	// AND AN UNBOUND CALLER READS NOTHING AT ALL, which is chat's own rule
	// and stricter than every other personal surface here: this socket was
	// accepted anonymously, as the dashboard's is, and anonymity is not a
	// seat.
	if _, code := ask(t, conn, 3, "chat_messages", "", params); code == "" {
		t.Error("a socket bound to no seat was served a private room's transcript")
	}
}

// ---- a seat answering in the room it was asked in ---------------------- //

// chatScript is an executor that was woken in a room, round by round.
//
// # Why it reads the request rather than counting calls
//
// Because the round this case exists for is the one AFTER the engine refuses
// the delivery claim, and nothing but the request says that refusal arrived: a
// rejected submission goes back to the model as the submit tool's own result,
// inside the same phase, so a script that counted rounds would post on a round
// the engine had not yet corrected and prove nothing about the correction.
// Keying on the refusal's own words is also what a real model does with it.
type chatScript struct {
	// channel and message are the room and the message the seat was woken
	// about. Both are known before the trigger is written — a room's id is
	// minted by its create and a person's message id is DERIVED from
	// (channel, author, operation id) — which is what lets a script be
	// armed before the wake it answers.
	channel string
	message string

	// answer is what the seat finally says, and what the room is checked
	// for afterwards.
	answer string

	// reacted and posted are one-shot: a seat that reacted twice would
	// keep the phase in a loop, and one that posted twice would leave two
	// answers in the room for a case that asserts there is one.
	reacted bool
	posted  bool
}

// chatRefusal is the engine's own words when a turn claims delivery on a
// surface nothing was delivered on.
//
// THE SURFACE IS IN IT, which is the whole of what this case turns on: the
// delivery gate compares a posting tool's declared surface against the
// notification source the wake carried, and both sides name [chat.Source]. A
// second spelling on either side does not fail — the gate finds no delivery
// for the source, falls back to "any delivery counts", and every addressed
// chat turn is then free to end in silence. Matching on the literal sentence
// is how a test can see the difference.
var chatRefusal = "nothing has been delivered on " + chat.Source + " yet"

// script arms the next turn's executor.
func (m *scriptedModel) script(s *chatScript) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chat = s
}

// chatAnswer is the scripted executor's reply for one request, or false to
// leave the shared endpoint's own behaviour in place.
func (m *scriptedModel) chatAnswer(raw []byte) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.chat
	switch {
	case s == nil:
		return "", false
	case !s.reacted:
		// AN ACKNOWLEDGEMENT, WHICH IS NOT AN ANSWER. It is a real write
		// through the real path — the reaction has to actually land, or
		// the refusal below would be the engine noticing that the turn
		// called nothing at all, which is a different rule.
		s.reacted = true
		return toolUse(chat.ReactToMessageTool, map[string]any{
			"channel": s.channel, "message": s.message, "emoji": ":eyes:",
		}), true
	case !s.posted && bytes.Contains(raw, []byte(chatRefusal)):
		s.posted = true
		return toolUse(chat.PostMessageTool, map[string]any{
			"channel": s.channel, "body": s.answer,
		}), true
	case s.posted:
		// THE CITATION IS WHAT THE ENGINE CHECKS AGAINST ITS OWN RECORD,
		// so it names the call that was really made. A submission citing
		// nothing is refused again even after a real delivery, which is
		// the engine declining to take a model's word for its own work.
		return toolUse("submit_work", map[string]any{
			"outcome":    "delivered",
			"summary":    "Answered in the room.",
			"deliveries": []any{chat.PostMessageTool},
		}), true
	default:
		// THE CLAIM THE ENGINE REFUSES: delivered, with nothing but a
		// reaction behind it.
		return toolUse("submit_work", map[string]any{
			"outcome": "delivered",
			"summary": "Acknowledged it.",
		}), true
	}
}

// sawText reports whether any request this endpoint served carried this text —
// a tool result, a correction or a prompt alike.
//
// THE WHOLE BODY, because what has to be observed here arrives as a TOOL
// RESULT rather than in the system prompt: a refused submission rides the
// conversation back to the model, which is exactly where a system-prompt-only
// reader would miss it.
func (m *scriptedModel) sawText(want string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, raw := range m.bodies {
		if bytes.Contains(raw, []byte(want)) {
			return true
		}
	}
	return false
}

// A SEAT WOKEN IN A ROOM ANSWERS IN THAT ROOM, AND A REACTION DOES NOT COUNT.
//
// # The two halves are one turn because they are one rule
//
// The reply obligation is SOURCE-SCOPED: what a turn owes is an answer on the
// surface the ask arrived from, and the engine decides that by comparing the
// surface a posting tool declares with `tools.DeliversTo` against the
// notification source the wake carried. The three posting tools carry it and
// nothing else does — a reaction wakes nobody and obliges nothing, so a turn
// that discharged its obligation with a thumb would leave the person who asked
// exactly as unanswered as silence would.
//
// So one turn shows both directions: the seat reacts, claims delivery, is
// refused IN THOSE WORDS, and then says something — after which the same claim
// is accepted and the room holds its answer. A case that only asserted the
// answer would pass on a build where the gate had fallen back to "any delivery
// counts", which is the failure that has shipped here once already.
func TestASeatAnswersWhereItWasAsked(t *testing.T) {
	t.Parallel()
	// TWO SEPARATE CAPS, and both are this case's subject rather than
	// scenery. The executor needs the rounds to react, be refused, post
	// and submit — four calls against the golden document's three — and
	// asking for more rounds than a turn allows is what the round-cap
	// extension judge exists for, which is a different subsystem
	// answering a different question.
	n := startWith(t, func(doc string) string {
		return strings.Replace(doc, "max_tool_rounds: 3", "max_tool_rounds: 6", 1)
	})
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)
	waitForSeat(t, n, "ceo")

	made := room(t, n, person("founder"), chat.NewChannel{
		Name: "incidents", Kind: chat.KindPublic,
		Members: []chat.Member{{Handle: "ceo"}},
	}).Channel
	// SOMETHING SAID EARLIER, ADDRESSED TO NOBODY, and it is what the seat
	// acknowledges. It is not scene-setting: the wake is derived from the
	// committed record by a consumer with its own position, independent of
	// this node's applier, so a seat can legitimately be woken a moment
	// BEFORE the row it was woken about exists here — and a script that
	// reacted to the trigger itself would intermittently be told there is
	// no such message, which is a race in the harness rather than anything
	// this case is about. Waiting for this one leaves the reaction certain
	// and the trigger free to arrive as fast as it likes.
	earlier := say(t, n, person("founder"), made.ID, "seat-chat-0",
		"the staging deploy went out at 14:00")
	if err := n.engine.WaitCommitted(t.Context(), earlier.Outcome.Position); err != nil {
		t.Fatalf("wait for the first message: %v", err)
	}

	const answer = "Drained the node and retried; the rollback completed."
	n.model.script(&chatScript{
		channel: made.ID, message: earlier.Message.ID, answer: answer,
	})

	say(t, n, person("founder"), made.ID, "seat-chat-1",
		"@ceo did the staging rollback ever finish?", "ceo")

	// THE ROOM IS THE ASSERTION. Not the model's call log: a tool call
	// that was made and failed is a turn that answered nobody, and the
	// only thing that distinguishes the two is whether the words are in
	// the room.
	var page chat.Transcript
	waitFor(t, "the seat to answer in the room", func() bool {
		page = transcript(t, n, "founder", made.ID)
		return slices.ContainsFunc(page.Messages, func(v chat.MessageView) bool {
			return v.Message.Author == "ceo"
		})
	}, func() string {
		return fmt.Sprintf("room holds %v; the model saw %v",
			bodies(page), n.model.seen())
	})
	said := page.Messages[0]
	if said.Message.Body != answer {
		t.Errorf("the seat said %q", said.Message.Body)
	}
	if said.Message.AuthorKind != chat.AuthorAgent {
		t.Errorf("the seat's own message is attributed as %q", said.Message.AuthorKind)
	}

	// AND THE REACTION LANDED, which is what makes the refusal below
	// evidence rather than an accident: an engine that refused a turn for
	// having called nothing at all would say the same words.
	oldest := page.Messages[len(page.Messages)-1]
	if oldest.Message.ID != earlier.Message.ID {
		t.Fatalf("the oldest message in the room is %s, not the one the seat "+
			"was scripted to acknowledge", oldest.Message.ID)
	}
	if !slices.ContainsFunc(oldest.Reactions, func(r chat.ReactionCount) bool {
		return r.Emoji == ":eyes:"
	}) {
		t.Errorf("the seat never reacted (%v), so the refusal below says "+
			"nothing about whether a reaction counts", oldest.Reactions)
	}

	// AND THE ENGINE REFUSED THE REACTION AS A DELIVERY, NAMING CHAT.
	if !n.model.sawText(chatRefusal) {
		t.Errorf("the seat claimed delivery having only reacted and was not "+
			"refused %q — the delivery gate is source-scoped, and a gate that "+
			"finds no deliverer for the source it was asked on falls back to "+
			"\"any delivery counts\", which lets every addressed chat turn end "+
			"in silence", chatRefusal)
	}
}

// ---- a node that was not there while the company talked ---------------- //

// coldEngine starts an engine over paths the CALLER owns, and hands back the
// stop so a case can end one and start the next.
//
// It is the one thing [start] cannot do: that fixture takes its own temporary
// directories and registers its teardown for the end of the test, which is
// right for every case whose node lives as long as the case — and impossible
// for one whose subject is what a SECOND node makes of the first one's log.
// No API, because nothing here reads through one.
func coldEngine(t *testing.T, nodeID, storePath, streamDir string) (*node, func()) {
	t.Helper()
	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	boot.Node.ID = nodeID
	boot.Store.Path = storePath
	boot.Stream.StoreDir = streamDir

	e, err := engine.New(t.Context(), engine.Options{Bootstrap: &boot, Company: cfg})
	if err != nil {
		t.Fatalf("engine.New for %s: %v", nodeID, err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		// ON A CONTEXT THE TEST'S OWN CANNOT CANCEL, which is this
		// tree's rule for every teardown: the failure a stop is undoing
		// is often the cancellation itself.
		e.Stop(context.WithoutCancel(context.Background()))
	}
	t.Cleanup(stop)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start for %s: %v", nodeID, err)
	}
	waitFor(t, nodeID+"'s native backends to hydrate", e.NativeHydrated)
	return &node{engine: e, model: model, id: nodeID}, stop
}

// A NODE THAT WAS NOT RUNNING WHILE A ROOM FILLED UP DERIVES THE SAME
// TRANSCRIPT FROM THE LOG, SEQUENCE AND POSITION INCLUDED.
//
// # What this adds over the fleet case above
//
// That one has both members applying side by side from the moment either
// writes anything, so every record reaches them within a batch of each other.
// What it cannot show is the state this domain is most exposed by: an applier
// starting from NOTHING against a log that already holds a room and a
// conversation. `channel_seq` is minted from LOG ORDER rather than carried, so
// a node catching up has to arrive at the numbers its predecessor minted live
// — and a node that arrives at different ones never converges, with every
// later message in that room inheriting the difference.
//
// # Why it is one engine after another rather than a member of a fleet
//
// Because a two-member fleet cannot write while one member is away, and this
// harness has no third. The streams are created at `stream.replicas` = the
// member count, so a raft quorum of two means the survivor of a stop publishes
// NOTHING and the case would compare an empty room with an empty room. Nor can
// the absent member simply JOIN late: a clustered member whose peer is not
// running never becomes JetStream-current and refuses to serve at all, by the
// engine's own readiness gate.
//
// So the absence is expressed the way a single machine can express it — the
// company talks, that node goes away, and a node with an EMPTY ESTATE comes up
// on the same log and has to derive the lot. Its own node id is what makes it
// a new arrival rather than a restart: the durable consumer is named after the
// node, so a fresh name reads the log from the beginning, which is exactly
// what a node joining a running company does.
func TestANodeThatWasNotRunningCatchesUpOnTheRoom(t *testing.T) {
	t.Parallel()
	// ONE STREAM DIRECTORY, TWO ESTATES: the log is the company's and
	// survives the node, and each node's SQL is its own.
	stream := t.TempDir()
	wrote, stopWriter := coldEngine(t, "node-wrote",
		filepath.Join(t.TempDir(), "crewlet.db"), stream)

	made := room(t, wrote, person("founder"), chat.NewChannel{
		Name: "postmortems", Kind: chat.KindPublic,
	}).Channel
	var last chat.Written
	for i := range 3 {
		last = say(t, wrote, person("founder"), made.ID,
			fmt.Sprintf("catchup-chat-%d", i),
			fmt.Sprintf("line %d of something nobody was listening to", i))
	}
	if err := wrote.engine.WaitCommitted(t.Context(), last.Outcome.Position); err != nil {
		t.Fatalf("the writing node never applied its own last message: %v", err)
	}

	// EVERYTHING COMPARED IS READ BEFORE THE NODE GOES AWAY, because it is
	// what the arriving node has to match and there will be nobody to ask.
	want := transcript(t, wrote, "founder", made.ID)
	wantRows := digestTable(t, wrote, "chat_messages")
	wantAt := wrote.engine.Chat().At()
	if len(want.Messages) != 3 {
		t.Fatalf("the writing node holds %v for a room three messages were "+
			"said in", bodies(want))
	}
	stopWriter()

	joined, _ := coldEngine(t, "node-joined",
		filepath.Join(t.TempDir(), "crewlet.db"), stream)

	// THE POSITION FIRST, because it is what "caught up" MEANS: a node
	// answering the right rows while still below the position they were
	// written at has not finished and cannot say so.
	waitFor(t, "the arriving node to reach the position the room was written at",
		func() bool {
			return joined.engine.Chat().At().Packed() >= wantAt.Packed()
		}, func() string {
			return fmt.Sprintf("arrived at %d, the room was written through %d",
				joined.engine.Chat().At().Packed(), wantAt.Packed())
		})

	// AND THE ROWS IT DERIVED ARE THE ROWS THE OTHER NODE MINTED LIVE.
	got := transcript(t, joined, "founder", made.ID)
	if !slices.Equal(bodies(got), bodies(want)) {
		t.Errorf("the arriving node derived %v and the node that was here "+
			"minted %v — the sequence comes from log order, so a node that "+
			"arrives at different numbers never converges with one that was "+
			"there, and every later message in the room inherits it",
			bodies(got), bodies(want))
	}
	if gotRows := digestTable(t, joined, "chat_messages"); gotRows != wantRows {
		t.Errorf("chat_messages digests %s on the arriving node and %s on the "+
			"one that wrote it", gotRows, wantRows)
	}
}
