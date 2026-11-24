package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/executorkeep"
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
