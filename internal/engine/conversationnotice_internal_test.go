package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A TURN'S CONVERSATION ON THE ENGINE'S CHAT IS TOLD WHAT ITS RUN LOST: by the
// engine, in the thread the turn answers or its direct room, in the verdict's
// sentence and never its detail, under one operation however often asked. A
// conversation on another surface is told nothing here.
//
// Mutation: post as the seat, put the detail in the line, mint the operation
// per ask, or post a direct conversation into a thread, and a case goes red.
func TestATurnsConversationIsToldOnTheEnginesChat(t *testing.T) {
	t.Parallel()
	verdict := *executorkeep.Lost(executorkeep.LossTooLarge, executorkeep.InsteadFresh, "the store said ghp_secret")
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		key, room, anchor string
		told              bool
	}{
		"a thread in a room": {"chat:room-1:msg-0", "room-1", "msg-0", true},
		"a direct room":      {"chat:room-1", "room-1", "", true},
		"another surface":    {"slack:C1:1700000000.1", "", "", false},
	} {
		rooms := &noticeRooms{}
		tellConversation(t.Context(), rooms, at, "t1", tc.key, verdict)
		tellConversation(t.Context(), rooms, at, "t1", tc.key, verdict)
		if !tc.told {
			if len(rooms.said) != 0 {
				t.Errorf("%s: told %+v, want nothing", name, rooms.said)
			}
			continue
		}
		if len(rooms.said) != 2 || rooms.said[0].actor.OpKey == "" ||
			rooms.said[0].actor.OpKey != rooms.said[1].actor.OpKey {
			t.Fatalf("%s: told %+v, want the same line under one operation each time", name, rooms.said)
		}
		got := rooms.said[0]
		if got.room != tc.room || got.anchor != tc.anchor || got.actor.Kind != chatlet.AuthorSystem ||
			got.body != verdict.Sentence() || strings.Contains(got.body, "ghp_secret") {
			t.Errorf("%s: told %+v, want the engine saying %q in room %q under %q", name, got,
				verdict.Sentence(), tc.room, tc.anchor)
		}
	}
}

// A NOTICE'S OPERATION IS THE VERDICT'S FACTS: the line's id leaves its words
// out (chatlet's askOf), so the turn's key names what it states, the loss, what
// the run did instead and the turns either side, and never the detail, which
// it does not say. Another verdict is another line; the same one with another
// detail is the same line.
//
// Mutation: derive the key from the turn alone, or put the detail in it, and a
// case goes red.
func TestANoticesOperationIsTheVerdictsFacts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	keyOf := func(v executorkeep.NotContinued) string {
		rooms := &noticeRooms{}
		tellConversation(t.Context(), rooms, at, "t1", "chat:room-1", v)
		return rooms.said[0].actor.OpKey
	}
	lost := *executorkeep.Lost(executorkeep.LossTooLarge, executorkeep.InsteadFresh, "the store said no")
	otherDetail := lost
	otherDetail.Detail = "the store said nothing"
	older := *executorkeep.Lost(executorkeep.LossCaptureLate, executorkeep.InsteadOlder, "")
	older.Older = &executorkeep.CapturePlace{LaunchSeq: 2, CaptureSeq: 1}
	olderStill := older
	olderStill.Older = &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 3}
	if keyOf(lost) != keyOf(otherDetail) {
		t.Errorf("one verdict with another detail is told under another operation")
	}
	if keyOf(lost) == keyOf(older) {
		t.Errorf("another loss is told under the same operation, so it would never be said")
	}
	if keyOf(older) == keyOf(olderStill) {
		t.Errorf("another older capture is told under the same operation, so it would never be said")
	}
}

// A NOTICE IS MINTED AT THE TURN'S OWN INSTANT, so the turn's end asked twice
// tells its conversation once, through the store that keeps the room: the glue
// reads the instant the turn's other operations are minted at, never the
// asking call's.
//
// Mutation: mint the notice's instant per ask, and the room holds two lines.
func TestATurnsEndAskedTwiceTellsItsConversationOnce(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	rooms, ok := e.chatHalves()
	if !ok {
		t.Fatal("a data node running Chatlet hands out no chat writer")
	}
	ceo := chatlet.Actor{Handle: "ceo", Kind: chatlet.AuthorAgent, WrittenFor: types.ForTurn(nil)}
	room, err := rooms.CreateChannel(t.Context(), ceo, chatlet.NewChannel{Name: "launch", Kind: chatlet.KindPublic})
	if err != nil {
		t.Fatalf("create #launch: %v", err)
	}
	root, err := rooms.Post(t.Context(), ceo, room.Channel.ID, chatlet.NewMessage{Body: "ship it?"})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}
	run := sandbox.PendingRun{AgentHandle: "ceo", TurnID: "t1", WorkKey: "wk-1",
		WorkSince:       time.Now().UTC(),
		ConversationKey: chatlet.Source + ":" + room.Channel.ID + ":" + root.Message.ID}
	verdict := *executorkeep.Lost(executorkeep.LossTooLarge, executorkeep.InsteadFresh, "")
	for range 2 {
		conversationNotice{engine: e}.NotContinued(t.Context(), run, verdict)
	}
	thread := notify.Thread{Backend: chatlet.Source, Channel: room.Channel.ID, Root: root.Message.ID}
	got, ok := e.ChatThreads().ReadThread(t.Context(), "ceo", thread)
	if !ok {
		t.Fatal("the thread could not be read back")
	}
	if len(got.Messages) != 2 || got.Messages[1].Text != verdict.Sentence() {
		t.Fatalf("the thread reads %+v, want the root and the engine's line once", got.Messages)
	}
}

// A CONVERSATION THIS NODE CANNOT TELL IS LOGGED, never dropped in silence: a
// node that hands out no chat writer says the conversation is not told.
//
// Mutation: return without the line, and the case goes red.
func TestAConversationThisNodeCannotTellIsLogged(t *testing.T) {
	logs := captureLogs(t)
	run := sandbox.PendingRun{TurnID: "t1", ConversationKey: chatlet.Source + ":room-1:msg-0"}
	conversationNotice{engine: &Engine{}}.NotContinued(t.Context(), run,
		*executorkeep.Lost(executorkeep.LossTooLarge, executorkeep.InsteadFresh, ""))
	if got := readingLines(t, logs, 0, "conversation_notice_unsaid", "t1"); len(got) != 1 {
		t.Fatalf("logged %d conversation_notice_unsaid lines for turn t1, want one", len(got))
	}
}

// noticeRooms is a [conversationWriter] that keeps what it was told.
type noticeRooms struct{ said []noticeLine }

type noticeLine struct {
	actor              chatlet.Actor
	room, anchor, body string
}

func (r *noticeRooms) Post(_ context.Context, actor chatlet.Actor, channelID string,
	in chatlet.NewMessage) (chatlet.Written, error) {
	r.said = append(r.said, noticeLine{actor: actor, room: channelID, body: in.Body})
	return chatlet.Written{}, nil
}

func (r *noticeRooms) Reply(_ context.Context, actor chatlet.Actor, channelID, replyTo string,
	in chatlet.NewMessage) (chatlet.Written, error) {
	r.said = append(r.said, noticeLine{actor: actor, room: channelID, anchor: replyTo, body: in.Body})
	return chatlet.Written{}, nil
}
