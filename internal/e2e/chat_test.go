package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The company's own chat, end to end.
//
// Everything below this level has its own suite: internal/chatlet certifies the
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

// ---- what a person writes as ------------------------------------------- //

// speaker is a person writing as themselves, in the one shape a person's chat
// write takes: the `kind: human` seat they hold, as a human, through their
// session — built by the one function the operator surface builds it with
// ([builtin.ChatActorOf]) over the principal a person's session resolves to.
// The act transport, the CLI and the operator's MCP surface all resolve a
// person to it, and so does the human write surface's prune — so the same
// actor posts here and prunes below.
//
// BENEATH THE TRANSPORT, handing the store exactly the actor the transport
// would have resolved, because the cases that use this are about what the
// log does with a write rather than about who may make it: the shared
// fleet's company signs nobody in, and [say] names the operation keys a
// case's assertions are about. A case whose subject is what a person's
// screen does writes through the transport instead ([openRoomAs], [sayAs]).
func speaker(t *testing.T, handle string) chatlet.Actor {
	t.Helper()
	actor, err := builtin.ChatActorOf(iam.Principal{
		Kind: iam.KindPerson, Login: handle, Seat: handle,
		Via: iam.SessionPrefix + "e2e-" + handle,
	})
	if err != nil {
		t.Fatalf("%s as a person writes: %v", handle, err)
	}
	return actor
}

// room creates a room on this node and waits for this node to apply it.
//
// THE WAIT IS PART OF THE GESTURE rather than the caller's to remember: a post
// into a room the applier has not written yet is refused inside the decide —
// deliberately, because under a strict replay the create sits below the post
// on the same log — so a caller that skipped it would get an intermittent
// "no such record" that says nothing about what it was testing.
func room(t *testing.T, n *node, actor chatlet.Actor, in chatlet.NewChannel) chatlet.Written {
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

// chatKeysBegan is when every operation key [say] posts under began: the test
// binary's start, to the millisecond an operation id carries. Every case names
// its own keys, so one instant shared between them makes no two of them one.
var chatKeysBegan = time.Now().UTC().Truncate(time.Millisecond)

// say posts one message and reports what was written.
//
// UNDER A KEY of the case's own naming, the way a person's request carries
// one ([chatlet.Actor.OpKey]): the message's id is derived from it, so the same
// key and words said twice are one message — which is what a person's retry
// is.
//
// THE KEY'S INSTANT IS FIXED ([chatKeysBegan]), because a retry reproduces it:
// a request's operation key carries the instant it was minted, and every send
// of that request carries the same one. A key minted at each post would put
// the two sends in different milliseconds, and the derived id carries its
// key's instant, so they would be two messages.
func say(t *testing.T, n *node, actor chatlet.Actor, channelID, key, body string,
	mentions ...string) chatlet.Written {

	t.Helper()
	return sayWhole(t, n, actor, channelID, key, chatlet.NewMessage{Body: body, Mentions: mentions})
}

// sayWhole is [say] for a message the case states whole: an `@channel`, which
// a person's composer marks by the flag the message carries.
func sayWhole(t *testing.T, n *node, actor chatlet.Actor, channelID, key string,
	in chatlet.NewMessage) chatlet.Written {

	t.Helper()
	actor.OpKey = statelog.DeriveOpID(chatKeysBegan, "", "crewlet.e2e.chat", key)
	written, err := n.engine.ChatStore().Post(t.Context(), actor, channelID, in)
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
func transcript(t *testing.T, n *node, viewer, channelID string) chatlet.Transcript {
	t.Helper()
	page, err := n.engine.Chat().Messages(t.Context(), viewer,
		chatlet.TranscriptQuery{ChannelID: channelID},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read %s back: %v", channelID, err)
	}
	return page
}

// bodies is the transcript rendered as "seq author: body", oldest first, which
// is what a failure has to print: a message id names nothing a reader can act
// on, and the SEQUENCE is half of what these cases are about. A room event is
// rendered as what it records ("seq author [joined]"), since it says nothing
// anybody wrote.
func bodies(page chatlet.Transcript) []string {
	out := make([]string, 0, len(page.Messages))
	for _, view := range slices.Backward(page.Messages) {
		if event := view.Message.Event; event != nil {
			out = append(out, fmt.Sprintf("%d %s [%s]",
				view.ChannelSeq, view.Message.Author, event.Kind))
			continue
		}
		out = append(out, fmt.Sprintf("%d %s: %s",
			view.ChannelSeq, view.Message.Author, view.Message.Body))
	}
	return out
}

// posts is what was said in a transcript, newest first: its messages without
// the room's events (who joined, was added or left, and what its words were
// set to), which share the room's sequence and say nothing anybody wrote.
// Every case that counts or picks a room's messages reads them through this,
// by the message's kind and never by its place.
func posts(page chatlet.Transcript) []chatlet.MessageView {
	return slices.DeleteFunc(slices.Clone(page.Messages), func(v chatlet.MessageView) bool {
		return v.Message.Event != nil
	})
}

// ---- a message written on one node, and a fleet that agrees about it --- //

// A MESSAGE POSTED ON ONE MEMBER WAKES A SEAT ON ANOTHER, WHOSE ANSWER LANDS
// IN THE SAME ROOM; EVERY MEMBER ENDS UP WITH THE SAME TRANSCRIPT; AND A PRUNE
// TAKES THE SAME ROWS FROM EVERY MEMBER.
//
// A CLAIM OF [TestAFleetOfThree]'S rather than a case with a fleet of its own:
// what it holds is a property of a running fleet, not of its boot, and a fleet
// of its own would be a whole bring-up and teardown in the one package that
// pays for every one. It reads only the room it makes, under a name and keys
// no other claim uses, and the fleet's one member that runs no seats
// ([chatWriterNode]) is stood up that way for it.
//
// Three claims in one, and they cannot be separated — the first two share a
// conversation and the third destroys it, which is the only order they can
// run in anyway:
//
//  1. THE WAKE CROSSES THE FLEET. This is the whole reason a wake is derived
//     from the durable record by a fleet-wide consumer rather than published
//     by the writer's own goroutine: the node that wins a feed message is
//     rarely the node running the seat that has to answer. The message is
//     therefore posted on a member that CANNOT hold the seat, and the
//     assertion is that the seat answered — in the room, as the room holds
//     it — from another member.
//  2. EVERY MEMBER MINTS ONE SEQUENCE. `channel_seq` is the one value in this
//     domain that no record carries — the applier mints it from log order —
//     so members agreeing about it is exactly what cannot be asserted anywhere
//     below a fleet, and a divergence in it is permanent: every later post in
//     the room inherits the wrong number. The seat's answer is published by a
//     DIFFERENT member from the person's two messages, racing the second of
//     them, and its number still has to be the one the log's order gives it.
//  3. A PRUNE TAKES THE SAME ROWS EVERYWHERE. The horizon is enforced by a
//     record carrying a cutoff INSTANT rather than by a sweep reading each
//     node's own clock, and this is the difference between the two: a sweep
//     would delete a different set on every node, for ever, and nothing would
//     ever report it.
func claimAMessageOnOneNodeWakesASeatOnAnotherAndTheFleetAgrees(t *testing.T, c *cluster) {
	// THE WRITER IS THE SEAT-LESS MEMBER, by name rather than by search: it
	// is the one [TestAFleetOfThree] stands up with every role but
	// [placement.RoleSeats], so it is the only member guaranteed not to be
	// holding the ceo seat at any point in this claim.
	//
	// NOT ONE OBSERVED TO BE HOLDING NOTHING, which is what this was before
	// the role existed, and it was flaky through four attempts to pin it by
	// observation — a single poll, then a stability window, then asserting
	// on any member but the writer. Placement converges while a fleet comes
	// up, so the seat moves after any check that it has settled, and the
	// last failure was the seat migrating ONTO the writer, where the wake
	// then ran locally and correctly and crossed nothing:
	//
	//	writer=node-2; ceo is now on node-2; phases seen:
	//	  node-0=[aux:knowledge onboarding onboarding]
	//	  node-2=[aux:knowledge execute ... review]
	//
	// A member that runs no seats makes the premise a fact instead: it is a
	// valid writer for the life of the fleet and the seat cannot arrive on
	// it, by construction rather than by timing.
	var writer *node
	for _, n := range c.nodes {
		if n.id == chatWriterNode {
			writer = n
		}
	}
	if writer == nil {
		t.Fatalf("no member of a fleet of %d is %s, the one stood up to run "+
			"no seats", len(c.nodes), chatWriterNode)
	}
	// AND THE WRITER REALLY IS HOLDING NOTHING, which the role makes true
	// and this confirms rather than assumes. It is the premise the whole
	// first arm rests on, and the way it fails is not a failure: if the
	// writer also held the seat, the wake would be served by its own
	// mailbox and the claim would pass or fail on which consumer won — a
	// coin flip dressed as an assertion. Checked because the shape that
	// produced it once already was coordination NOT being shared across the
	// fleet; see [memberBootstrap]'s note.
	if held := writer.engine.Node().Host().Held(); len(held) != 0 {
		t.Fatalf("%s was stood up to run no seats and holds %v — there is "+
			"then no member this wake has to cross to", writer.id, held)
	}

	// NO AGENT IN THE ROOM: the seat is reached by being named, which a
	// public room carries to a seat that has not joined. With it a member,
	// the founder's second line, which names nobody, would wake it again as
	// the room's answerer, and this claim is about one turn racing that
	// line rather than a second turn answering it.
	created := room(t, writer, speaker(t, "founder"), chatlet.NewChannel{
		Name: "rollbacks", Kind: chatlet.KindPublic,
		Topic: "what to do when a deploy hangs",
	})
	made := created.Channel
	// EVERY MEMBER APPLIES THE ROOM before anything is said in it. The
	// assertions below read the room on every member, and a room one of
	// them has not written yet is a missing create rather than a
	// divergence — which would fail this claim for the wrong reason.
	for i, n := range c.nodes {
		if err := n.engine.WaitCommitted(t.Context(), created.Outcome.Position); err != nil {
			t.Fatalf("member %d never applied the room: %v", i, err)
		}
	}

	// THE SEAT'S ANSWER IS SCRIPTED ON EVERY MEMBER THAT COULD RUN IT, and
	// that is every member but the writer: which of them placement put the
	// seat on is placement's business, and this claim must not depend on
	// it. Taken off again when the claim ends, so a turn on this fleet
	// afterwards meets the endpoint every other claim does.
	const answer = "Drained the node and retried; the rollback completed."
	for _, n := range c.nodes {
		if n != writer {
			n.model.script(&chatScript{channel: made.ID, answer: answer})
		}
	}
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.model.script(nil)
		}
	})

	const asked = "@ceo the staging deploy hung on rollback again"
	first := say(t, writer, speaker(t, "founder"), made.ID, "fleet-chat-1", asked, "ceo")
	second := say(t, writer, speaker(t, "founder"), made.ID, "fleet-chat-2",
		"and the node never drained")

	// (1) THE SEAT ANSWERS IN THE ROOM, FROM A MEMBER THAT IS NOT THE WRITER.
	//
	// THE ROOM IS THE ASSERTION, read on the writer: a turn that ran is not
	// a turn that answered, and the only thing that distinguishes the two is
	// whether the words reached the room — here, reached it on the member
	// that never ran the seat, which only the log can have carried them to.
	var answered chatlet.MessageView
	waitFor(t, "the seat's answer in the room, as the writer holds it", func() bool {
		page := transcript(t, writer, "founder", made.ID)
		i := slices.IndexFunc(page.Messages, func(v chatlet.MessageView) bool {
			return v.Message.Author == "ceo"
		})
		if i < 0 {
			return false
		}
		answered = page.Messages[i]
		return true
	}, func() string {
		var saw []string
		for _, n := range c.nodes {
			saw = append(saw, fmt.Sprintf("%s=%v", n.id, n.model.seen()))
		}
		return fmt.Sprintf("writer=%s; ceo is now on %s; phases seen: %s",
			writer.id, chatSeatHolder(c), strings.Join(saw, " "))
	})
	if answered.Message.Body != answer || answered.Message.AuthorKind != chatlet.AuthorAgent {
		t.Errorf("the room holds %q by a %s where the seat's answer should be",
			answered.Message.Body, answered.Message.AuthorKind)
	}
	// AND THE MEMBER THAT ANSWERED WAS WOKEN BY THIS MESSAGE. Each member
	// has its own scripted endpoint, so the one whose script posted is the
	// one that ran the turn — and it must have been shown what was asked,
	// or the turn that answered was woken by something else and the wake
	// this claim is about crossed nothing.
	var ran []string
	for _, n := range c.nodes {
		if n.model.chatPosted() {
			ran = append(ran, n.id)
			if !n.model.sawText(asked) {
				t.Errorf("%s answered in the room without ever being shown %q",
					n.id, asked)
			}
		}
	}
	if len(ran) != 1 || ran[0] == writer.id {
		t.Fatalf("the seat answered from %v, want exactly one member and not %s, "+
			"the one the message was posted on", ran, writer.id)
	}
	// AND THAT TURN DELIVERED, rather than only posted: the round after the
	// post submits citing it, and the engine accepts that as the turn's
	// answer. Waited for before anything else, because the scripts come off
	// when this claim ends ([t.Cleanup] above) — a citing round that reached
	// the endpoint after that would meet the default reply, an uncited
	// claim the engine refuses until the round cap, and the turn would end
	// on the rescue path while the next claim runs on this fleet. The
	// script says it SENT the submission; the member's own record of the
	// turn says the engine took it.
	answerer := c.nodes[slices.IndexFunc(c.nodes, func(n *node) bool { return n.id == ran[0] })]
	waitFor(t, "the turn that answered to submit citing its answer",
		answerer.model.chatSubmitted, func() string {
			return fmt.Sprintf("%s's model saw %v", answerer.id, answerer.model.seen())
		})
	deliveredThrough(t, answerer, chatlet.PostMessageTool)

	// (2) AND EVERY MEMBER HOLDS THE SAME TRANSCRIPT, sequence included.
	// EVERY MEMBER WAITS ON BOTH the second message and the seat's answer:
	// the two race, so either may be the room's newest record, and a member
	// that holds one of them need not yet hold the other. The first was said
	// before either and sits below both on the log.
	for i, n := range c.nodes {
		for _, at := range []statelog.Position{second.Outcome.Position, answered.Position} {
			if err := n.engine.WaitCommitted(t.Context(), at); err != nil {
				t.Fatalf("member %d never applied the room's record at %s: %v", i, at, err)
			}
		}
	}
	want := transcript(t, c.nodes[0], "founder", made.ID)
	if len(posts(want)) != 3 {
		t.Fatalf("member 0 holds %v for a room three messages were said in",
			bodies(want))
	}
	// THE SEQUENCE IS CONTIGUOUS, STARTS AT ONE AND FOLLOWS THE LOG, which
	// is what a client holding 41 and handed 43 relies on to ask for 42
	// rather than for the whole room. The first message was said before
	// either of the others could be; the second and the seat's answer race,
	// so each takes 2 or 3 by where the LOG put it — and a number minted
	// from anything but log order fails one of these.
	//
	// WHERE THE LOG PUT EACH is the post's own position: the second's is its
	// write's outcome, and the answer's is its [chatlet.MessageView.Position],
	// which names the record that LAST changed the row — the post itself
	// only because nothing in this claim edits, reacts to or deletes it.
	//
	// THE ROOM'S FOUNDING LINE IS ONE: its members joined, written by the
	// record that made it, before anybody said anything, so the first
	// message is two.
	seqs := map[string]int64{}
	for _, view := range want.Messages {
		seqs[view.Message.ID] = view.ChannelSeq
	}
	oldest := want.Messages[len(want.Messages)-1]
	if oldest.ChannelSeq != 1 || oldest.Message.Event == nil ||
		oldest.Message.Event.Kind != chatlet.EventJoined {
		t.Errorf("the room's first row is not its founding line: %v", bodies(want))
	}
	if got := seqs[first.Message.ID]; got != 2 {
		t.Errorf("the first message sits at channel_seq %d: %v", got, bodies(want))
	}
	later := []string{second.Message.ID, answered.Message.ID}
	if answered.Position.Packed() < second.Outcome.Position.Packed() {
		slices.Reverse(later)
	}
	for i, id := range later {
		if got := seqs[id]; got != int64(i+3) {
			t.Errorf("message %s is number %d on the log and sits at channel_seq "+
				"%d: %v", id, i+3, got, bodies(want))
		}
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
	agreeOnMessages(t, c, "with the conversation in it")

	// (3) AND A PRUNE TAKES THE SAME ROWS FROM EVERY MEMBER.
	//
	// THE CUTOFF IS READ OFF THE CLOCK ONLY AFTER EVERY MEMBER HAS APPLIED
	// THE NEWEST MESSAGE, so every message above is stored strictly before
	// it — the instants on the rows are the BROKER's, and this is the one
	// place that relationship has to hold for the claim to mean anything.
	//
	// AS A PERSON ([speaker]): the human write surface's prune route and
	// `crewlet chatlet prune` write as the person or the credential that sent
	// them, the retention duty writes as the engine itself
	// ([chatlet.AuthorSystem]), and the prune's own guard admits those kinds and
	// no agent. So a seat can never prune — one that could would delete a
	// year of the company's decisions as a turn's side effect.
	pruned, err := c.nodes[0].engine.ChatStore().Prune(t.Context(),
		speaker(t, "founder"), statelog.NewOpID(time.Now(), "prune-"+made.ID),
		made.ID, time.Now().UTC())
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
	agreeOnMessages(t, c, "after the prune")
}

// agreeOnMessages fails unless every member's chat_messages digests as member
// 0's does — every member, because a fleet of three where the third was never
// compared is a fleet of two with a passenger.
func agreeOnMessages(t *testing.T, c *cluster, when string) {
	t.Helper()
	first := digestTable(t, c.nodes[0], "chat_messages")
	for i, n := range c.nodes[1:] {
		if got := digestTable(t, n, "chat_messages"); got != first {
			t.Errorf("%s, chat_messages digests %s on member %d and %s on member 0",
				when, got, i+1, first)
		}
	}
}

// ---- a private room, at the socket ------------------------------------- //

// peopleDoc adds two people to the golden company: two `kind: human` seats a
// person is invited onto ([alice], [bob]).
//
// A PERSON IN CHAT IS A SEAT. A session resolves to the human seat its person
// is bound to in the identity directory, and a caller may never name a seat —
// so a case about who may read a room needs both halves of that binding, the
// seat in the company and the person on it, or it is not testing the rule at
// all.
//
// THE FIRST `roles:` IS THE SEATS' — see [companyDoc] for why the units are
// last and why that matters to every fixture that patches this document.
func peopleDoc(doc string) string {
	return strings.Replace(doc, "roles:\n", "roles:\n"+
		"  - name: Alice\n"+
		"    handle: alice\n"+
		"    kind: human\n"+
		"  - name: Bob\n"+
		"    handle: bob\n"+
		"    kind: human\n", 1)
}

// alice and bob are the people [peopleDoc]'s seats are for, each holding chat
// and nothing else: what a case here asserts about a room is chat's own rule,
// and a person holding admin as well would pass it for the wrong reason.
var (
	alice = person{
		address: "alice@example.com", login: "alice.smith", name: "Alice Smith",
		password: "alice-has-a-perfectly-fine-passphrase", seat: "alice",
		grants: []iam.Grant{iam.GrantChatlet},
	}
	bob = person{
		address: "bob@example.com", login: "bob.jones", name: "Bob Jones",
		password: "bob-has-a-perfectly-fine-passphrase", seat: "bob",
		grants: []iam.Grant{iam.GrantChatlet},
	}
)

// signedIn signs who in on n through a browser of their own, which it returns.
func signedIn(t *testing.T, n *node, who person) *browser {
	t.Helper()
	b := newBrowser(t)
	signInAs(t, n, b, who)
	return b
}

// chatRoomReceipt is what create_channel answers: the room it made.
type chatRoomReceipt struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
}

// chatPostReceipt is what a posting tool answers: the message it said, and the
// thread it was said in where it was a reply.
type chatPostReceipt struct {
	MessageID  string `json:"message_id"`
	Channel    string `json:"channel"`
	ThreadRoot string `json:"thread_root"`
}

// openRoomAs makes a room from the tab of the person b is signed in as — the
// dashboard's "New room" — and waits for this node to apply it, for
// [room]'s reason: a post into a room the applier has not written yet is
// refused inside the decide.
func openRoomAs(t *testing.T, n *node, b *browser, args map[string]any) chatRoomReceipt {
	t.Helper()
	return receiptOf[chatRoomReceipt](t, b.acted(n, chatlet.CreateChannelTool, args))
}

// sayAs posts one message from the tab of the person b is signed in as — the
// composer's send — and waits for this node to apply it.
func sayAs(t *testing.T, n *node, b *browser, channelID, body string) chatPostReceipt {
	t.Helper()
	return receiptOf[chatPostReceipt](t, b.acted(n, chatlet.PostMessageTool,
		map[string]any{"channel": channelID, "body": body}))
}

// ask puts one question on the socket's query channel and waits for its
// answer — asked as whoever the socket is, which its handshake settled for its
// life: a frame carries no credential of its own. A refusal comes back as its
// code and, for one on authority, the rule's reason, which the `unauthorized`
// frame carries beside it.
func ask(t *testing.T, conn *websocket.Conn, id int64, what string,
	params map[string]any) (json.RawMessage, string, authz.Reason) {

	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"kind": "query", "id": id, "what": what, "params": params,
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
			Kind   stream.Kind     `json:"kind"`
			ID     int64           `json:"id"`
			Data   json.RawMessage `json:"data"`
			Error  string          `json:"error"`
			Reason authz.Reason    `json:"reason"`
		}
		if json.Unmarshal(raw, &env) != nil || env.ID != id {
			continue
		}
		if env.Kind == stream.KindError {
			return nil, env.Error, env.Reason
		}
		return env.Data, "", ""
	}
}

// A PRIVATE ROOM IS NOT SERVED TO SOMEBODY WHO IS NOT IN IT, ASKED AT THE
// SOCKET.
//
// # Why this is not a unit test of the filter
//
// Because every layer between the filter and the wire is one that can lose it,
// and each has done so somewhere in this tree's history: a principal that
// resolves to no seat, a question registered without its permission, a
// transport that reduces a refusal to a code that reads as an empty room. The
// only place all of them are live at once is a real socket on a real node, and
// the only honest shape for the assertion is a COMPARISON — the member is
// served the very message the non-member is refused, and the non-member is
// served the room he IS in, the same question on the same connection.
//
// Without either half this case would pass identically against a node that
// served nobody anything: a refusal on a connection that is never answered at
// all says nothing about the filter.
//
// # One connection is one person
//
// A socket's principal is settled at its handshake and held for its life — a
// session cookie, or a bearer — so each person here asks on a socket of their
// own, as each person's tab does.
//
// # And why it is the socket's QUERY channel rather than its push
//
// Because they are different filters and each deserves its own subject. This
// one is the READ path: a question asked, answered or refused. The push path —
// a committed record becoming a frame — is
// [TestACommittedMessageReachesAWatchingSocket] below.
func TestAPrivateRoomIsNotServedToANonMemberOverTheSocket(t *testing.T) {
	t.Parallel()
	n := startNode(t, nodeSpec{company: peopleDoc, boot: withSignIn})
	waitFor(t, "the native backends to hydrate", hydrated(t, n.engine))
	alices, bobs := signedIn(t, n, alice), signedIn(t, n, bob)

	// ALICE'S ROOM, AND ALICE IS ITS ONLY MEMBER, made and said in from her
	// own tab — through the act transport, as the dashboard writes. A create
	// puts its author in the room it made, so nothing here adds her
	// explicitly.
	made := openRoomAs(t, n, alices, map[string]any{
		"channel": "founders", "kind": string(chatlet.KindPrivate),
		"topic": "things not everybody should read",
	})
	const said = "the term sheet lands on Thursday"
	sayAs(t, n, alices, made.Channel, said)
	// AND A ROOM BOB IS IN, said in by her too: his half of the comparison.
	lobby := openRoomAs(t, n, alices, map[string]any{
		"channel": "lobby", "kind": string(chatlet.KindPublic), "members": []any{bob.seat},
	})
	const open = "the offsite is booked for May"
	sayAs(t, n, alices, lobby.Channel, open)

	served := func(conn *websocket.Conn, id int64, who, room, want string) {
		t.Helper()
		data, code, _ := ask(t, conn, id, "chat_messages", map[string]any{"channel_id": room})
		if code != "" {
			t.Fatalf("%s was refused a room they are in: %s", who, code)
		}
		var page chatlet.Transcript
		if err := json.Unmarshal(data, &page); err != nil {
			t.Fatalf("decode the transcript: %v", err)
		}
		if said := posts(page); len(said) != 1 || said[0].Message.Body != want {
			t.Fatalf("%s was served %v", who, bodies(page))
		}
	}

	// THE MEMBER'S HALF FIRST, so the refusal below is falsifiable.
	served(dialAs(t, n, alices), 1, "Alice", made.Channel, said)

	// AND THE NON-MEMBER IS TOLD THE ROOM IS NOT THERE, rather than told
	// it is not his — on a connection that IS answered, the room he is in,
	// so the refusal is the filter's and not a socket that answers nobody.
	// A private room's EXISTENCE is information — who is talking to whom is
	// most of what a conversation discloses — so the two answers have to
	// read identically from outside.
	bobsTab := dialAs(t, n, bobs)
	served(bobsTab, 1, "Bob", lobby.Channel, open)
	if _, code, _ := ask(t, bobsTab, 2, "chat_messages",
		map[string]any{"channel_id": made.Channel}); code != stream.CodeNotFound {
		t.Errorf("a non-member asking for #%s was answered %q, not %q — and if "+
			"he was served the page, a transcript has left this deployment",
			made.Name, code, stream.CodeNotFound)
	}

	// AND A CREDENTIAL ACTING AS ITSELF READS NOTHING AT ALL, which is
	// chat's own rule and stricter than every other surface here: the
	// deployment's token holds admin and is a seat of nobody's, and a room is
	// read as a seat or not at all — by the authority table's own row, a
	// principal it seats nowhere is refused before any room is read. The
	// REASON is held as well as the code, because the code alone does not
	// say which rule answered: the token holds admin, which confers chat, so
	// the missing seat is the one thing that may refuse it — and the reader
	// behind the table, which refuses an empty viewer as a backstop, answers
	// otherwise.
	if _, code, reason := ask(t, n.dial(t), 1, "chat_messages",
		map[string]any{"channel_id": made.Channel}); code != stream.CodeUnauthorized ||
		reason != authz.ReasonUnseated {
		t.Errorf("a socket that is no seat asking for #%s was answered %q (%q), not %q (%q)",
			made.Name, code, reason, stream.CodeUnauthorized, authz.ReasonUnseated)
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
	// channel is the room the seat answers in, and message the one it
	// acknowledges first with a reaction — or none, for a seat that goes
	// straight to claiming it has answered. Both are known before the
	// trigger is written, a room's id being minted by its create and a
	// message's by its post, which is what lets a script be armed before
	// the wake it answers.
	channel string
	message string

	// thread is the root of the thread the seat was asked in, where it
	// answers with [chatlet.ReplyInThreadTool] — beside the question, where
	// everybody in the thread hears it — or "" for a seat asked in the room
	// itself, which answers there with [chatlet.PostMessageTool].
	thread string

	// answer is what the seat finally says, and what the room is checked
	// for afterwards.
	answer string

	// reacted and posted are one-shot: a seat that reacted twice would
	// keep the phase in a loop, and one that posted twice would leave two
	// answers in the room for a case that asserts there is one.
	reacted bool
	posted  bool

	// submitted is set when the round after the post has been served: the
	// submission citing it, which is what the engine accepts as the turn's
	// delivery. A case waits on it before it takes the script off, so the
	// turn ends on the submission rather than on whatever the endpoint
	// answers once nothing is armed.
	submitted bool

	// hold, when set, keeps the round that POSTS waiting until it is
	// closed, for a case whose subject is what a room shows while a seat
	// is at work: the working indicator is up for exactly as long as the
	// turn runs, and a scripted turn runs faster than the presence hub's
	// fan-out window, so without it the indicator can be raised and
	// withdrawn inside one window, which presents nothing, and never shown
	// to anybody. The case closes it once it has seen what it waits for.
	hold <-chan struct{}
}

// tool is the posting tool the seat answers with: in the thread it was asked
// in, or in the room.
func (s *chatScript) tool() string {
	if s.thread != "" {
		return chatlet.ReplyInThreadTool
	}
	return chatlet.PostMessageTool
}

// chatRefusal is the engine's own words when a turn claims delivery on a
// surface nothing was delivered on.
//
// THE SURFACE IS IN IT, which is the whole of what this case turns on: the
// delivery gate compares a posting tool's declared surface against the
// notification source the wake carried, and both sides name [chatlet.Source]. A
// second spelling on either side does not fail — the gate finds no delivery
// for the source, falls back to "any delivery counts", and every addressed
// chat turn is then free to end in silence. Matching on the sentence the
// surface-named refusal carries is how a test can see the difference.
//
// THE SENTENCE IS THE RUNNER'S OWN ([runner.NotDeliveredOn]) rather than a
// copy: a copy here lost the backticks the engine puts round the surface, so
// the scripted model never saw the refusal and the case timed out waiting
// for an answer it was never going to post.
var chatRefusal = runner.NotDeliveredOn(chatlet.Source)

// script arms the next turn's executor.
func (m *scriptedModel) script(s *chatScript) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chat = s
}

// chatAnswer is the scripted executor's reply for one request, or false to
// leave the shared endpoint's own behaviour in place — and, for the request
// that posts, what to wait on before answering ([chatScript.hold]). The wait
// is the CALLER's, outside this endpoint's lock: every other request it
// serves, and every case reading what it saw, takes that lock too.
//
// The refusal is looked for IN A TOOL RESULT ([modelRequest.toolResults]),
// which is where the engine puts it: it is the submit tool's own answer,
// handed back inside the run that made the claim or in the transcript a later
// run starts from.
func (m *scriptedModel) chatAnswer(req modelRequest) (modelReply, <-chan struct{}, bool) {
	refused := slices.ContainsFunc(req.toolResults(), func(result string) bool {
		return strings.Contains(result, chatRefusal)
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.chat
	switch {
	case s == nil:
		return modelReply{}, nil, false
	case !s.reacted && s.message != "":
		// AN ACKNOWLEDGEMENT, WHICH IS NOT AN ANSWER. It is a real write
		// through the real path — the reaction has to actually land, or
		// the refusal below would be the engine noticing that the turn
		// called nothing at all, which is a different rule.
		s.reacted = true
		return toolUse(chatlet.ReactToMessageTool, map[string]any{
			"channel": s.channel, "message_id": s.message, "emoji": ":eyes:",
		}), nil, true
	case !s.posted && refused:
		s.posted = true
		if s.thread != "" {
			return toolUse(chatlet.ReplyInThreadTool, map[string]any{
				"channel": s.channel, "reply_to": s.thread, "body": s.answer,
			}), s.hold, true
		}
		return toolUse(chatlet.PostMessageTool, map[string]any{
			"channel": s.channel, "body": s.answer,
		}), s.hold, true
	case s.posted:
		// THE CITATION IS WHAT THE ENGINE CHECKS AGAINST ITS OWN RECORD,
		// so it names the call that was really made. A submission citing
		// nothing is refused again even after a real delivery, which is
		// the engine declining to take a model's word for its own work.
		s.submitted = true
		return toolUse("submit_work", map[string]any{
			"outcome":    "delivered",
			"summary":    "Answered in the room.",
			"deliveries": []any{s.tool()},
		}), nil, true
	default:
		// THE CLAIM THE ENGINE REFUSES: delivered, with nothing but a
		// reaction behind it.
		return toolUse("submit_work", map[string]any{
			"outcome": "delivered",
			"summary": "Acknowledged it.",
		}), nil, true
	}
}

// chatPosted reports whether the armed script has posted its answer — which
// only the endpoint of the member that ran the seat's turn can have done.
func (m *scriptedModel) chatPosted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chat != nil && m.chat.posted
}

// chatSubmitted reports whether the armed script has served the submission
// citing its answer ([chatScript.submitted]).
func (m *scriptedModel) chatSubmitted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chat != nil && m.chat.submitted
}

// sawText reports whether any request this endpoint served carried this text
// — a tool result, a correction or a prompt alike.
//
// THE WHOLE CONVERSATION, and the system prompt beside it, because what has
// to be observed here arrives as a TOOL RESULT rather than in the system
// prompt: a refused submission rides the conversation back to the model,
// inside the run that made it as well as in a later run's transcript
// ([modelRequest.toolResults]), which is exactly where a reader of the
// prompts alone would miss it.
func (m *scriptedModel) sawText(want string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, text := range slices.Concat(m.prompts, m.systems, slices.Concat(m.results...)) {
		if strings.Contains(text, want) {
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
	n := start(t)
	waitFor(t, "the native backends to hydrate", hydrated(t, n.engine))
	waitForSeat(t, n, "ceo")

	// NO AGENT IN THE ROOM, so the line said first wakes nobody: a person's
	// message in a room with an agent member is answered by its most senior
	// agent ([TestAPersonIsNeverAnsweredBySilence]), and that turn would
	// take this case's script. The seat is reached below by being named,
	// which a public room carries to a seat that has not joined.
	made := room(t, n, speaker(t, "founder"), chatlet.NewChannel{
		Name: "incidents", Kind: chatlet.KindPublic,
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
	earlier := say(t, n, speaker(t, "founder"), made.ID, "seat-chat-0",
		"the staging deploy went out at 14:00")
	if err := n.engine.WaitCommitted(t.Context(), earlier.Outcome.Position); err != nil {
		t.Fatalf("wait for the first message: %v", err)
	}

	const answer = "Drained the node and retried; the rollback completed."
	n.model.script(&chatScript{
		channel: made.ID, message: earlier.Message.ID, answer: answer,
	})

	say(t, n, speaker(t, "founder"), made.ID, "seat-chat-1",
		"@ceo did the staging rollback ever finish?", "ceo")

	// THE ROOM IS THE ASSERTION. Not the model's call log: a tool call
	// that was made and failed is a turn that answered nobody, and the
	// only thing that distinguishes the two is whether the words are in
	// the room.
	var page chatlet.Transcript
	waitFor(t, "the seat to answer in the room", func() bool {
		page = transcript(t, n, "founder", made.ID)
		return slices.ContainsFunc(page.Messages, func(v chatlet.MessageView) bool {
			return v.Message.Author == "ceo"
		})
	}, func() string {
		return fmt.Sprintf("room holds %v; the model saw %v",
			bodies(page), n.model.seen())
	})
	said := posts(page)[0]
	if said.Message.Body != answer {
		t.Errorf("the seat said %q", said.Message.Body)
	}
	if said.Message.AuthorKind != chatlet.AuthorAgent {
		t.Errorf("the seat's own message is attributed as %q", said.Message.AuthorKind)
	}

	// AND THE REACTION LANDED, which is what makes the refusal below
	// evidence rather than an accident: an engine that refused a turn for
	// having called nothing at all would say the same words.
	written := posts(page)
	oldest := written[len(written)-1]
	if oldest.Message.ID != earlier.Message.ID {
		t.Fatalf("the oldest message in the room is %s, not the one the seat "+
			"was scripted to acknowledge", oldest.Message.ID)
	}
	if !slices.ContainsFunc(oldest.Reactions, func(r chatlet.ReactionCount) bool {
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
	// AND THEN TOOK THE SAME CLAIM, citing the post: the other direction of
	// the one rule, and the half a gate that refused everything would fail.
	deliveredThrough(t, n, chatlet.PostMessageTool)
}

// A PERSON IS NEVER ANSWERED BY SILENCE, AND A PERSON'S `@channel` IS A
// REQUEST.
//
// The two failures these hold shut, as a person met them: they posted in a
// room made by hand, its agents members who follow nothing in particular, and
// named nobody, and nothing was woken and nothing said so; and their
// `@channel` reached every agent as news, which each was told to leave alone.
// Now the first wakes the room's answerer, its most senior agent member, and
// the second asks every agent in the room, and both oblige an answer.
//
// THE ANSWER IN THE ROOM IS THE ASSERTION, and it is evidence of the obligation
// rather than of the wake alone: the scripted seat posts only after the engine
// refuses its claim to have delivered with nothing said, and the engine
// refuses that claim only on a turn the routing addressed. A seat woken as
// news ends on the claim, and the room holds no answer. The seat is also
// shown why, in the routing's own words.
func TestAPersonIsNeverAnsweredBySilence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   chatlet.NewMessage
		why  string
	}{
		{"a message naming nobody", chatlet.NewMessage{Body: "where are we on the release?"},
			"you are the most senior agent in this room"},
		{"a person's @channel", chatlet.NewMessage{
			Body: "@channel where are we on the release?", Collective: true},
			"so this is a request to every seat in it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := start(t)
			waitFor(t, "the native backends to hydrate", hydrated(t, n.engine))
			waitForSeat(t, n, "ceo")
			// A ROOM MADE BY HAND, its one agent a member that follows
			// nothing in particular: before, this was a room nobody heard.
			made := room(t, n, speaker(t, "founder"), chatlet.NewChannel{
				Name: "release", Kind: chatlet.KindPublic,
				Members: []chatlet.Member{{Handle: "ceo"}},
			}).Channel
			const answer = "The branch is cut; the notes go out tomorrow."
			n.model.script(&chatScript{channel: made.ID, answer: answer})
			t.Cleanup(func() { n.model.script(nil) })

			sayWhole(t, n, speaker(t, "founder"), made.ID, "silence-"+tc.name, tc.in)

			var page chatlet.Transcript
			waitFor(t, "the seat to answer in the room", func() bool {
				page = transcript(t, n, "founder", made.ID)
				return slices.ContainsFunc(page.Messages, func(v chatlet.MessageView) bool {
					return v.Message.Author == "ceo"
				})
			}, func() string {
				return fmt.Sprintf("room holds %v; the model saw %v", bodies(page),
					n.model.seen())
			})
			if said := posts(page)[0]; said.Message.Body != answer {
				t.Errorf("the seat said %q", said.Message.Body)
			}
			if !n.model.sawText(chatRefusal) {
				t.Errorf("the seat was never refused %q, so its answer is not "+
					"evidence that the wake obliged one", chatRefusal)
			}
			if !n.model.sawText(tc.why) {
				t.Errorf("the seat was never told %q, the routing's reason for "+
					"waking it", tc.why)
			}
			waitFor(t, "the seat to submit citing its answer", n.model.chatSubmitted)
			deliveredThrough(t, n, chatlet.PostMessageTool)
		})
	}
}

// ---- a node that was not there while the company talked ---------------- //

// coldEngine starts an engine over paths the CALLER owns, on the keyring ring
// — the one every node that reads this log holds — and hands back the stop so
// a case can end one and start the next.
//
// It is the one thing [startNode] cannot do: that fixture takes its own
// temporary directories and registers its teardown for the end of the test,
// which is right for every case whose node lives as long as the case — and
// impossible for one whose subject is what a SECOND node makes of the first
// one's log. No API, because nothing here reads through one — and so no
// bridge, [nodeOptions]' "" for a node that serves none: no seat takes a
// turn here, so nothing launches a box that would dial one.
//
// BUILT AS EVERY OTHER NODE HERE IS ([bootNodeWith] over [nodeOptions]): the
// harness's activation instant, its own environment rather than the process's,
// a store seeded from the migrated image and its chat hub beside its engine.
// The seed leaves a file that is already there alone, so a path booted twice
// reopens its own estate.
func coldEngine(t *testing.T, nodeID, storePath, streamDir string, ring keyring) (
	*node, func()) {

	t.Helper()
	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	boot.Node.ID = nodeID
	boot.Store.Path = storePath
	brokertest.StoreIn(&boot.Stream, streamDir)
	ring.hold(&boot)

	n := bootNodeWith(t, nodeOptions(&boot, cfg, nil, ""), model)
	// THE HUB FIRST, THEN THE ENGINE, in the order `crewlet run`'s shutdown
	// stops them ([newEngine]): the engine's teardown closes the broker the
	// hub's answerer is subscribed on, and the case that calls this boots
	// another node straight after, beside a hub that would otherwise still be
	// running over a stopped engine until the test ends.
	//
	// ON A CONTEXT THE TEST'S OWN CANNOT CANCEL, which is this tree's rule
	// for every teardown: the failure a stop is undoing is often the
	// cancellation itself. The cleanup [newNode] registered stops both again
	// at the end of the test, which each one's Stop makes a no-op.
	stop := func() {
		n.chat.Stop(context.WithoutCancel(t.Context()))
		n.engine.Stop(context.WithoutCancel(t.Context()))
	}
	waitFor(t, nodeID+"'s native backends to hydrate", hydrated(t, n.engine))
	return n, stop
}

// A NODE THAT WAS NOT RUNNING WHILE A ROOM FILLED UP DERIVES THE SAME
// TRANSCRIPT FROM THE LOG, SEQUENCE AND POSITION INCLUDED.
//
// # What this adds over the fleet's claim
//
// [claimAMessageOnOneNodeWakesASeatOnAnotherAndTheFleetAgrees] has every member
// applying side by side from the moment any of them writes anything, so every
// record reaches them within a batch of each other. What it cannot show is the
// state this domain is most exposed by: an applier starting from NOTHING
// against a log that already holds a room and a conversation. `channel_seq` is
// minted from LOG ORDER rather than carried, so a node catching up has to
// arrive at the numbers its predecessor minted live — and a node that arrives
// at different ones never converges, with every later message in that room
// inheriting the difference.
//
// # Why it is one engine after another rather than a member of a fleet
//
// Because what is under test is the APPLIER, and an applier's input is the log
// rather than whoever wrote it. A node added to a company whose log has been
// written and never trimmed sits one below the first record, so it replays
// from there rather than asking a peer for a snapshot (the engine's "why a
// fresh node does not join") — the same path whether the records were
// published by a peer that is still running or by an engine that has since
// stopped. One machine states exactly that: the company talks, that node goes
// away, and a node with an EMPTY ESTATE comes up on the same log and has to
// derive the lot. Its own node id is what makes it a new arrival rather than a
// restart: the durable consumer is named after the node, so a fresh name reads
// the log from the beginning, which is exactly what a node joining a running
// company does.
//
// A fleet would add nothing the claim does not already hold — records crossing
// members through a quorum — and would cost what this package pays most for:
// a cluster brought up, and a member taken away and replaced while the other
// two keep their quorum, where this runs beside every other single-node case.
func TestANodeThatWasNotRunningCatchesUpOnTheRoom(t *testing.T) {
	t.Parallel()
	// ONE STREAM DIRECTORY, TWO ESTATES: the log is the company's and
	// survives the node, and each node's SQL is its own. AND ONE KEYRING, as
	// for any two nodes on one log: every record on it is signed, and a node
	// keyed differently would refuse every one its predecessor wrote.
	stream := t.TempDir()
	ring := fleetRing(t)
	wrote, stopWriter := coldEngine(t, "node-wrote",
		filepath.Join(t.TempDir(), "crewlet.db"), stream, ring)

	made := room(t, wrote, speaker(t, "founder"), chatlet.NewChannel{
		Name: "postmortems", Kind: chatlet.KindPublic,
	}).Channel
	var last chatlet.Written
	for i := range 3 {
		last = say(t, wrote, speaker(t, "founder"), made.ID,
			fmt.Sprintf("catchup-chat-%d", i),
			fmt.Sprintf("line %d of something nobody was listening to", i))
	}
	if err := wrote.engine.WaitCommitted(t.Context(), last.Outcome.Position); err != nil {
		t.Fatalf("the writing node never applied its own last message: %v", err)
	}
	// AND THE LAST ONE SAID AGAIN UNDER ITS OWN KEY, as a person's retry
	// sends it: the same key and the same words are the same message, so
	// the room still holds three — the property [say]'s fixed instant is
	// for.
	again := say(t, wrote, speaker(t, "founder"), made.ID, "catchup-chat-2",
		"line 2 of something nobody was listening to")
	if again.Message.ID != last.Message.ID {
		t.Fatalf("the same key and words said again made message %s beside %s — a "+
			"person's retry would say it twice", again.Message.ID, last.Message.ID)
	}

	// EVERYTHING COMPARED IS READ BEFORE THE NODE GOES AWAY, because it is
	// what the arriving node has to match and there will be nobody to ask.
	want := transcript(t, wrote, "founder", made.ID)
	wantRows := digestTable(t, wrote, "chat_messages")
	wantAt := wrote.engine.Chat().At()
	if len(posts(want)) != 3 {
		t.Fatalf("the writing node holds %v for a room three messages were "+
			"said in", bodies(want))
	}
	stopWriter()

	joined, _ := coldEngine(t, "node-joined",
		filepath.Join(t.TempDir(), "crewlet.db"), stream, ring)

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

// dialAs opens the live socket the way the dashboard's LiveSocket opens one for
// a person: from their browser, carrying its session cookie and the
// deployment's Origin, so the server settles the socket's identity at ACCEPT
// time and holds it for the socket's life.
//
// The ordinary [node.dial] is the deployment's own token instead — a script
// acting as itself. A person's tab differs where it matters here: the PUSH
// stream. A chat frame is decided per recipient SEAT before it is sent, so a
// socket that is nobody's seat is sent none ([stream.ChatHub]).
func dialAs(t *testing.T, n *node, b *browser) *websocket.Conn {
	t.Helper()
	conn, res, err := b.dial(t, n, deploymentURL)
	if err != nil {
		t.Fatalf("dial %s%s from a person's browser: %v (%s)", n.server.URL,
			"/ws/stream", err, answered(res))
	}
	// The frames a live company produces are far larger than the default
	// cap: a snapshot carries the whole roster.
	conn.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// A COMMITTED MESSAGE REACHES A SOCKET WATCHING THE ROOM, AND A PRIVATE ROOM'S
// RECORDS REACH NO SOCKET OUTSIDE IT.
//
// # What this is actually asserting
//
// The whole push path, which is four hand-offs and no fewer: the applier
// commits a batch, announces it to its [chatlet.Observer], the hub decides per
// watcher whether that viewer may see the room, and the frame reaches the wire.
// Every one of them is a place the frame can be lost, and the first was lost
// for the entire life of this feature — the engine built its applier with a
// nil observer and no process ever supplied one, so nothing committed here
// became a frame anywhere.
//
// That is why this is an e2e case rather than a unit test of the hub. The hub's
// own suite already proves it fans out what it is given, against rooms of its
// own; what nobody could prove without a running node is that anything ever
// gives it anything — or that the decision it makes per watcher is made over
// the node's real rooms (the router's, which [api.NewEngineChatLive] hands the
// hub) rather than over a reader that answers yes.
//
// # Why each person dials their own socket
//
// See [dialAs]. A push is decided per recipient, so the identity has to exist
// before the frame does, and it is the handshake's.
//
// # And why the message is somebody else's, said from their own tab
//
// Because that is the frame a watching tab exists for, and the one only the
// per-recipient decision can deliver: Bob says it, through the act transport
// as his dashboard would, and Alice's socket is told — about Bob's message,
// by the id the engine handed BOB, which is the one thing that ties the frame
// to the write rather than to any post in the room.
//
// # And what Bob's own tab is never told
//
// Before he speaks, Alice makes a private room with nobody else in it and says
// something there. Bob's tab is watching too, and the assertion is an ORDERED
// ABSENCE: his socket is read until his own message's frame arrives, and no
// frame before it may name her room. The hub announces records in log order
// and hers are below his, so a filter that let them through would have put
// them on his socket first — and an absence asserted without the frame that
// bounds it would pass on a socket that was simply never sent anything.
func TestACommittedMessageReachesAWatchingSocket(t *testing.T) {
	t.Parallel()
	n := startNode(t, nodeSpec{company: peopleDoc, boot: withSignIn})
	waitFor(t, "the native backends to hydrate", hydrated(t, n.engine))
	alices, bobsBrowser := signedIn(t, n, alice), signedIn(t, n, bob)

	made := openRoomAs(t, n, alices, map[string]any{
		"channel": "shipping", "kind": string(chatlet.KindPublic), "topic": "what is going out",
	})

	// WATCHING BEFORE THE MESSAGE EXISTS. A push is live, not replayed:
	// the hub serves what is committed while a socket is attached and
	// nothing else, so a socket that arrived afterwards would be asserting
	// against a frame that was correctly never sent to it. Each socket has
	// joined its hub once its snapshot has arrived, which is sent after.
	conn := dialAs(t, n, alices)
	focusOn(t, conn, made.Channel)
	awaitFrame(t, conn, stream.KindSnapshot, "alice's snapshot",
		func(json.RawMessage) bool { return true })
	bobs := dialAs(t, n, bobsBrowser)
	focusOn(t, bobs, made.Channel)
	awaitFrame(t, bobs, stream.KindSnapshot, "bob's snapshot",
		func(json.RawMessage) bool { return true })

	// ALICE'S PRIVATE ROOM, said in, both below Bob's message on the log.
	private := openRoomAs(t, n, alices, map[string]any{
		"channel": "founders", "kind": string(chatlet.KindPrivate),
		"topic": "things not everybody should read",
	})
	sayAs(t, n, alices, private.Channel, "the term sheet lands on Thursday")

	said := sayAs(t, n, bobsBrowser, made.Channel, "the build is green")

	// READ PAST EVERY OTHER FRAME, for [ask]'s reason: a health tick, a
	// roster and a presence frame all arrive unasked. The one this case is
	// about is the posted frame naming Bob's message.
	deadline, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	for {
		_, raw, err := conn.Read(deadline)
		if err != nil {
			t.Fatalf("no chat_posted frame for Bob's message reached a socket "+
				"watching #%s: %v — the applier's observer is not reaching the hub",
				made.Name, err)
		}
		var env struct {
			Kind stream.Kind       `json:"kind"`
			Data stream.ChatChange `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != stream.KindChatPosted ||
			env.Data.ChannelID != made.Channel {
			continue
		}
		if env.Data.MessageID != said.MessageID {
			t.Fatalf("a posted frame named message %q in #%s, not Bob's %q",
				env.Data.MessageID, made.Name, said.MessageID)
		}
		if env.Data.Author != bob.seat || env.Data.AuthorKind != chatlet.AuthorHuman {
			t.Errorf("Bob's message was framed as %s's, a %s — a person writing "+
				"from the dashboard writes as the human seat they hold",
				env.Data.Author, env.Data.AuthorKind)
		}
		break
	}

	// AND BOB'S TAB, up to his own message, heard nothing of Alice's room.
	for {
		_, raw, err := bobs.Read(deadline)
		if err != nil {
			t.Fatalf("Bob's own message never reached his socket watching #%s: %v",
				made.Name, err)
		}
		var env struct {
			Kind stream.Kind     `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if namesRoom(env.Kind, env.Data, private.Channel) {
			t.Fatalf("Bob, who is not in #%s, was pushed a %s frame naming it: %s",
				private.Name, env.Kind, raw)
		}
		var change stream.ChatChange
		if env.Kind == stream.KindChatPosted && json.Unmarshal(env.Data, &change) == nil &&
			change.MessageID == said.MessageID {
			return
		}
	}
}

// namesRoom reports whether a pushed frame of kind, carrying data, is about the
// room channelID: a record announced in it, or its presence.
func namesRoom(kind stream.Kind, data json.RawMessage, channelID string) bool {
	switch {
	case kind == stream.KindChatPresence:
		var frame stream.ChatPresence
		return json.Unmarshal(data, &frame) == nil && frame.ChannelID == channelID
	case slices.Contains(stream.ChatPushKinds(), kind) && kind != stream.KindChatCursorMoved:
		var change stream.ChatChange
		return json.Unmarshal(data, &change) == nil && change.ChannelID == channelID
	}
	return false
}

// chatSeatHolder is the member holding the ceo seat right now, for a
// diagnostic, or "nobody".
func chatSeatHolder(c *cluster) string {
	for _, n := range c.nodes {
		if slices.Contains(n.engine.Node().Host().Held(), "ceo") {
			return n.id
		}
	}
	return "nobody"
}

// chatWriterNode is the member the fleet's chat claim posts from, and the one
// [TestAFleetOfThree] stands up running no seats ([withoutSeats]).
//
// NAMED rather than discovered, because the claim's premise is that the seat
// can never be on it, and only a member configured that way makes the premise
// a fact. The id is the harness's own (`node-<i>`, minted in
// [memberBootstrap]), and member zero is chosen for no reason beyond being the
// one that always exists.
const chatWriterNode = "node-0"
