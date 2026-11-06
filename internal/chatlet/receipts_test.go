package chatlet_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON CAN TELL WHOM A MESSAGE REACHED, AND WHO ANSWERED.
//
// The routing decided both long before this: every post's record carries the
// seats it woke and why ([chatlet.Notify.Recipients]). Nothing kept it, so a
// person who posted in a room could not tell a message that reached nobody
// from one whose agent simply had not answered, and found out by posting test
// messages and waiting. The applier now writes the routed set onto the post
// ([chatlet.Message.Reach]) and marks each seat answered when it speaks in the
// post's thread.
//
// Mutation: drop the reach from applyPost and the room's message carries none;
// drop answerReceipts and the reply never answers it.
func TestAPersonsPostSaysWhomItReachedAndWhoAnswered(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane").withRank("cto", "dev"))
	room := r.room(person("jane"), "engineering", "dev", "cto").Channel.ID

	asked := r.say(person("jane"), room, "where are we on the release?", "k-ask")
	want := &chatlet.Reach{Recipients: []chatlet.Reached{
		{Handle: "cto", Reason: chatlet.ReasonAnswerer}}}
	if got := asked.Message.Reach; !sameReach(got, want) {
		t.Errorf("the post answered reach %+v, want %+v: the poster is told at once", got, want)
	}
	view := readOne(t, r, room, asked.Message.ID)
	if !sameReach(view.Message.Reach, want) {
		t.Fatalf("the message reads reach %+v, want %+v", view.Message.Reach, want)
	}
	if view.Delivery != chatlet.DeliveryWaiting || len(view.Answered) != 0 {
		t.Fatalf("before any answer the message reads %q answered by %v, want waiting",
			view.Delivery, view.Answered)
	}

	// A TOP-LEVEL REMARK IN A NAMED ROOM IS NOT AN ANSWER: the answer a
	// person can see is in the thread their message started.
	r.say(seat("cto", "run-1"), room, "deploying the hotfix now", "k-remark")
	if view = readOne(t, r, room, asked.Message.ID); len(view.Answered) != 0 {
		t.Fatalf("a top-level remark answered the message: %v", view.Answered)
	}

	reply, err := r.store.Reply(t.Context(), keyed(seat("cto", "run-2"), requested("k-reply")),
		room, asked.Message.ID, chatlet.NewMessage{Body: "the release is on track for Friday"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	r.drain()
	view = readOne(t, r, room, asked.Message.ID)
	if view.Delivery != chatlet.DeliveryAnswered || len(view.Answered) != 1 ||
		view.Answered[0].Handle != "cto" || view.Answered[0].MessageID != reply.Message.ID {
		t.Fatalf("after cto's reply the message reads %q answered by %+v, want cto with %s",
			view.Delivery, view.Answered, reply.Message.ID)
	}

	// AN AGENT'S POST CARRIES NO REACH: a receipt is a person's. The reply
	// is read in its thread, the only place a reply is read.
	thread, err := r.reader().Thread(t.Context(), "jane",
		chatlet.ThreadQuery{ChannelID: room, RootID: asked.Message.ID},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(thread.Replies) != 1 {
		t.Fatalf("read the thread: %d replies, %v", len(thread.Replies), err)
	}
	if agent := thread.Replies[0]; agent.Message.Reach != nil ||
		agent.Delivery != "" {
		t.Errorf("an agent's reply reads reach %+v and %q, want none", agent.Message.Reach,
			agent.Delivery)
	}
}

// A POST THAT REACHED NO AGENT SAYS SO, rather than reading like one nobody
// recorded: a reach with no seats in it.
func TestAPostThatReachedNoAgentSaysSo(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob").withRank("cto"))
	room := r.room(person("jane"), "people", "bob").Channel.ID

	said := r.say(person("jane"), room, "anyone around?", "k-1")
	view := readOne(t, r, room, said.Message.ID)
	if view.Message.Reach == nil || len(view.Message.Reach.Recipients) != 0 {
		t.Fatalf("the message reads reach %+v, want an empty one", view.Message.Reach)
	}
	if view.Delivery != chatlet.DeliveryNoAgent {
		t.Errorf("the message reads %q, want %q", view.Delivery, chatlet.DeliveryNoAgent)
	}
}

// A PERSON'S POST WHOSE REACH NOBODY RECORDED IS UNKNOWN, NEVER "NO AGENT".
//
// A message stored before posts recorded their reach carries none, and saying
// it reached no agent would be a claim about a post nobody can make: it may
// well have woken a seat. So it reads [chatlet.DeliveryUnknown], which a
// reader draws no receipt for, while a recorded empty reach stays
// [chatlet.DeliveryNoAgent]. An agent's post and a room event read neither.
//
// Mutation: read a missing reach as an empty one in deliveryOf and the old
// message reads no_agent.
func TestAPostWithNoRecordedReachIsUnknownNotNoAgent(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob").withRank("cto"))
	room := r.room(person("jane"), "people", "bob").Channel.ID
	said := r.say(person("jane"), room, "anyone around?", "k-1")
	agent := r.say(seat("cto", "run-1"), room, "here", "k-2")

	// THE ROW AS A BUILD BEFORE RECEIPTS STORED IT: the same message, its
	// document with no reach.
	err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		var document []byte
		if err := tx.QueryRowContext(t.Context(),
			`SELECT document FROM chat_messages WHERE id = ?`, said.Message.ID).
			Scan(&document); err != nil {
			return err
		}
		message, err := chatlet.DecodeMessage(document)
		if err != nil {
			return err
		}
		message.Reach = nil
		stripped, err := chatlet.EncodeMessage(message)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(),
			`UPDATE chat_messages SET document = ? WHERE id = ?`, stripped, said.Message.ID)
		return err
	})
	if err != nil {
		t.Fatalf("store the message as an earlier build did: %v", err)
	}
	if view := readOne(t, r, room, said.Message.ID); view.Delivery != chatlet.DeliveryUnknown ||
		view.Message.Reach != nil {
		t.Errorf("a person's post with no recorded reach reads %q (reach %+v), want %q",
			view.Delivery, view.Message.Reach, chatlet.DeliveryUnknown)
	}
	if view := readOne(t, r, room, agent.Message.ID); view.Delivery != "" {
		t.Errorf("an agent's post reads delivery %q, want none: a receipt is a person's",
			view.Delivery)
	}
}

// A DIRECT CONVERSATION IS ANSWERED ANYWHERE IN IT, since it exists only
// between its parties: the agent's next word in it answers what it was asked.
func TestADirectMessageIsAnsweredByTheNextWordInIt(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	direct, _, err := r.store.OpenDirect(t.Context(), person("jane"), []string{"dev"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	r.drain()
	id := direct.Channel.ID

	asked := r.say(person("jane"), id, "can you check the build?", "k-ask")
	answer := r.say(seat("dev", "run-1"), id, "it is green again", "k-answer")
	view := readOne(t, r, id, asked.Message.ID)
	if view.Delivery != chatlet.DeliveryAnswered || len(view.Answered) != 1 ||
		view.Answered[0].MessageID != answer.Message.ID {
		t.Fatalf("the direct message reads %q answered by %+v, want dev with %s",
			view.Delivery, view.Answered, answer.Message.ID)
	}
	if got := view.Message.Reach.Recipients; len(got) != 1 || got[0].Reason != chatlet.ReasonDM {
		t.Errorf("the direct message reached %+v, want dev as a direct message", got)
	}
}

// A DRAFT IS TOLD WHOM IT WOULD REACH BY THE POST'S OWN DECISION, before
// anything is sent, and why none where it reaches no agent. Every case is
// checked against the post the same words then make: the two cannot differ.
//
// Mutation: route the dry run by a rule of its own (say, every agent member)
// and the cases disagree with the posts they precede.
func TestADraftReachesWhomThePostWould(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob").withRank("cto", "dev"))
	open := r.room(person("jane"), "engineering", "dev", "cto", "bob").Channel.ID
	people := r.room(person("jane"), "people", "bob").Channel.ID

	for _, tc := range []struct {
		name     string
		room     string
		in       chatlet.NewMessage
		quiet    chatlet.Quiet
		reaching []string
	}{
		{"a room's answerer", open, chatlet.NewMessage{Body: "status?"}, "", []string{"cto"}},
		{"a mention", open, chatlet.NewMessage{Body: "@dev can you look?",
			Mentions: []string{"dev"}}, "", []string{"dev"}},
		{"a person named", open, chatlet.NewMessage{Body: "@bob lunch?",
			Mentions: []string{"bob"}}, chatlet.QuietAddressed, nil},
		{"a room with no agent", people, chatlet.NewMessage{Body: "hello"},
			chatlet.QuietNoAgent, nil},
	} {
		draft, err := r.store.Reach(t.Context(), person("jane"), tc.room, "", tc.in)
		if err != nil {
			t.Fatalf("%s: dry run: %v", tc.name, err)
		}
		if draft.Quiet != tc.quiet || !slices.Equal(reachedHandles(draft.Reach), tc.reaching) {
			t.Errorf("%s: the draft reaches %v (quiet %q), want %v (quiet %q)", tc.name,
				reachedHandles(draft.Reach), draft.Quiet, tc.reaching, tc.quiet)
		}
		posted, err := r.store.Post(t.Context(), keyed(person("jane"), requested("k-"+tc.name)),
			tc.room, tc.in)
		if err != nil {
			t.Fatalf("%s: post: %v", tc.name, err)
		}
		r.drain()
		if !sameReach(posted.Message.Reach, draft.Reach) {
			t.Errorf("%s: the draft said %+v and the post reached %+v", tc.name,
				draft.Reach, posted.Message.Reach)
		}
	}
	// AN EMPTY BOX IS NOT REFUSED: whom it would reach is what a composer
	// shows before anybody types.
	if _, err := r.store.Reach(t.Context(), person("jane"), open, "",
		chatlet.NewMessage{}); err != nil {
		t.Errorf("an empty draft was refused: %v", err)
	}
	// AND A ROOM THE POST WOULD BE REFUSED IN IS REFUSED THE SAME WAY.
	archived := r.room(person("jane"), "old", "dev").Channel.ID
	closed := true
	if _, err := r.patch(person("jane"), archived, chatlet.ChannelPatch{Archived: &closed},
		chatlet.ChannelAuthority{Archive: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := r.store.Reach(t.Context(), person("jane"), archived, "",
		chatlet.NewMessage{Body: "hi"}); !errors.Is(err, chatlet.ErrArchived) {
		t.Errorf("a draft in an archived room answered %v, want the post's refusal", err)
	}
}

// readOne is one message as a room's transcript reads it.
func readOne(t *testing.T, r *writeRound, channelID, id string) chatlet.MessageView {
	t.Helper()
	page, err := r.reader().Messages(t.Context(), "jane",
		chatlet.TranscriptQuery{ChannelID: channelID},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the room: %v", err)
	}
	for _, view := range page.Messages {
		if view.Message.ID == id {
			return view
		}
	}
	t.Fatalf("message %s is not on the room's page", id)
	return chatlet.MessageView{}
}

func reachedHandles(r *chatlet.Reach) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, seat := range r.Recipients {
		out = append(out, seat.Handle)
	}
	return out
}

func sameReach(a, b *chatlet.Reach) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Truncated == b.Truncated && slices.Equal(a.Recipients, b.Recipients)
}
