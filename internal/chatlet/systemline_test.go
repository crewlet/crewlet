package chatlet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE'S LINE IS NO SEAT'S, WHATEVER SEAT SHARES ITS NAME. The engine
// writes its line under a name of its own; a seat of that name (refused by the
// org chart, and judged here by kind all the same) neither edits nor deletes
// it, joins no thread by it, is not woken by a later reply in a private thread
// the engine wrote in, has no receipt answered by it, and reads neither it nor
// a reaction to it as its own. The thread counts the line as a reply and the
// engine as no replier.
//
// Mutation: compare an edit's or a delete's author by name alone, let the
// engine's line join its thread or answer a receipt, count it as a replier, or
// read it as the namesake's sent message or reacted-to message, and a case goes
// red.
func TestTheEnginesLineIsNoSeats(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	stale := statelog.Freshness{Level: statelog.ReadStale}
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	engine := chatlet.Actor{Handle: "sessions", Kind: chatlet.AuthorSystem}
	namesake := seat("sessions", "run-1")

	made, err := r.store.CreateChannel(ctx, person("jane"), chatlet.NewChannel{
		Name: "board", Kind: chatlet.KindPrivate, Members: members("bob"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	board := made.Channel.ID
	root := r.say(person("jane"), board, "where are we with the release", "k-1")
	told, err := r.store.Reply(ctx, engine, board, root.Message.ID,
		chatlet.NewMessage{Body: "The run did not continue its session."})
	if err != nil {
		t.Fatalf("the engine's line: %v", err)
	}
	r.drain()

	if _, err := r.store.Edit(ctx, namesake, board, told.Message.ID,
		chatlet.EditMessage{Body: "all is well"}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("a seat named as the engine edited the engine's line: %v", err)
	}
	if _, err := r.store.Delete(ctx, namesake, board, told.Message.ID,
		chatlet.MessageAuthority{}); !errors.Is(err, chatlet.ErrForbidden) {
		t.Errorf("a seat named as the engine deleted the engine's line: %v", err)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_thread_participants WHERE handle = ?`,
		"sessions"); got != 0 {
		t.Errorf("the engine's line made %q a participant of its thread", "sessions")
	}

	if _, err := r.store.Reply(ctx, keyed(person("jane"), requested("k-2")), board,
		root.Message.ID, chatlet.NewMessage{Body: "thanks, rerunning it"}); err != nil {
		t.Fatalf("jane's later reply: %v", err)
	}
	r.drain()
	if n := r.lastRecord().Notify; n != nil && slices.ContainsFunc(n.Recipients,
		func(seat chatlet.Recipient) bool { return seat.Handle == "sessions" }) {
		t.Errorf("a later reply in the private thread woke the seat named as the engine: %+v",
			n.Recipients)
	}
	page, err := r.reader().Messages(ctx, "jane", chatlet.TranscriptQuery{ChannelID: board}, stale)
	if err != nil {
		t.Fatalf("read the room: %v", err)
	}
	for _, view := range page.Messages {
		if view.Message.ID != root.Message.ID {
			continue
		}
		if s := view.Thread; s == nil || s.Replies != 2 || s.ReplierCount != 1 ||
			!slices.Equal(s.Repliers, []string{"jane"}) {
			t.Errorf("the thread's summary is %+v, want 2 replies by jane alone", s)
		}
	}

	// A DIRECT CONVERSATION WITH THE NAMESAKE: jane's ask waits on it, and
	// the engine's line there answers nothing.
	direct, _, err := r.store.OpenDirect(ctx, person("jane"), []string{"sessions"})
	if err != nil {
		t.Fatalf("open the direct conversation: %v", err)
	}
	r.drain()
	r.say(person("jane"), direct.Channel.ID, "can you rerun the release?", "k-3")
	if _, err := r.store.Post(ctx, engine, direct.Channel.ID,
		chatlet.NewMessage{Body: "The run did not continue its session."}); err != nil {
		t.Fatalf("the engine's line in the direct conversation: %v", err)
	}
	r.drain()
	if got := r.count(`SELECT COUNT(*) FROM chat_receipts WHERE handle = ? AND answer_id = ''`,
		"sessions"); got != 1 {
		t.Errorf("%d of the namesake's receipts wait after the engine's line, want jane's ask", got)
	}

	// A PUBLIC ROOM, which the namesake reads: the engine's line there is
	// neither its sent message nor its message somebody reacted to.
	launch := r.room(person("jane"), "launch", "bob", "sessions").Channel.ID
	said, err := r.store.Post(ctx, engine, launch,
		chatlet.NewMessage{Body: "The run did not continue its session."})
	if err != nil {
		t.Fatalf("the engine's line in the public room: %v", err)
	}
	r.drain()
	if _, err := r.store.React(ctx, person("bob"), launch, said.Message.ID, ":eyes:"); err != nil {
		t.Fatalf("bob's reaction: %v", err)
	}
	r.drain()
	sent, err := r.reader().Sent(ctx, "sessions", chatlet.SentQuery{}, stale)
	if err != nil {
		t.Fatalf("Sent: %v", err)
	}
	if len(sent.Messages) != 0 {
		t.Errorf("the namesake's sent messages are %+v, want none", sent.Messages)
	}
	activity, err := r.reader().Activity(ctx, "sessions",
		chatlet.ActivityQuery{Kind: chatlet.ActivityReaction}, stale)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(activity.Items) != 0 {
		t.Errorf("the namesake's activity lists %+v, want no reaction to the engine's line",
			activity.Items)
	}
}

// THE ENGINE'S LINE IS ITS KEY'S, NOT ITS WORDS': the engine keys a line by the
// fact it states, so its key worded anew (a build that words the sentence
// otherwise, retrying a turn's end) is the same line, never a second. A seat's
// words are its message, so its key under new words is a second message.
//
// Mutation: digest the engine's words into its line's id, and the reworded
// retry posts a second line.
func TestTheEnginesLineIsItsKeysNotItsWords(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "bob").Channel.ID
	engine := keyed(chatlet.Actor{Handle: "sessions", Kind: chatlet.AuthorSystem}, requested("k-notice"))
	for _, body := range []string{"The run did not continue its session.", "The run's session was lost."} {
		if _, err := r.store.Post(ctx, engine, room, chatlet.NewMessage{Body: body}); err != nil {
			t.Fatalf("the engine's line %q: %v", body, err)
		}
		r.drain()
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND author_kind = ? AND event = ''`,
		room, string(chatlet.AuthorSystem)); got != 1 {
		t.Errorf("the engine's line under one key, worded twice, landed %d times, want once", got)
	}
	r.say(person("jane"), room, "the release is green", "k-jane")
	r.say(person("jane"), room, "the release is red", "k-jane")
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND author_handle = ? AND event = ''`, room, "jane"); got != 2 {
		t.Errorf("jane's two remarks under one key landed %d times, want twice", got)
	}
}

// ONLY THE ENGINE'S LINE PASSES A PRIVATE ROOM'S MEMBERSHIP: the line tells the
// room what befell a conversation there, and the engine's every other gesture
// is held to the room's membership as anybody's, which it never holds, a member
// sharing its name included.
//
// Mutation: lift membership for the engine's every gesture, or count it a
// member by its name, and its reaction lands.
func TestOnlyTheEnginesLinePassesAPrivateRoomsMembership(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	made, err := r.store.CreateChannel(ctx, person("jane"), chatlet.NewChannel{
		Name: "board", Kind: chatlet.KindPrivate, Members: members("bob"),
	})
	if err != nil {
		t.Fatalf("create the private room: %v", err)
	}
	r.drain()
	board := made.Channel.ID
	said := r.say(person("jane"), board, "where are we with the release", "k-1")
	for _, name := range []string{"sessions", "bob"} {
		engine := chatlet.Actor{Handle: name, Kind: chatlet.AuthorSystem}
		if _, err := r.store.React(ctx, engine, board, said.Message.ID, ":eyes:"); !errors.Is(err,
			chatlet.ErrNotFound) {
			t.Errorf("the engine, as %q, reacted in a private room: %v", name, err)
		}
	}
	engine := chatlet.Actor{Handle: "sessions", Kind: chatlet.AuthorSystem}
	if _, err := r.store.Reply(ctx, engine, board, said.Message.ID,
		chatlet.NewMessage{Body: "The run did not continue its session."}); err != nil {
		t.Errorf("the engine's line in a private room: %v", err)
	}
}
