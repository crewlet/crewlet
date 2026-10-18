package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/notify"
)

// A TURN WOKEN IN A CHATLET THREAD IS HANDED THAT THREAD.
//
// [engine.Engine.ChatThreads] is what the turn-start prefetch reads a chat
// thread through, one reader per surface, matched on the trigger's own
// `transport`. It held the vendor surfaces' readers alone, so every Chatlet
// thread reply rendered the "could not be read from this node" hint and sent
// the seat to read its thread with its tools. This reads a real thread back
// through the engine's own set, as the seat it woke.
func TestATurnWokenInAChatletThreadIsHandedTheThread(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{
		Company:     parsedCompany(t, unitRoomCompany("ada", "bob")),
		ActivatedAt: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC),
	})
	rooms, ok := engine.OperatorChatWriter(e)
	if !ok {
		t.Fatal("a Chatlet company's data node hands out no chat writer")
	}
	ada := chatlet.Actor{Handle: "ada", Kind: chatlet.AuthorAgent}
	bob := chatlet.Actor{Handle: "bob", Kind: chatlet.AuthorAgent}
	room, err := rooms.CreateChannel(t.Context(), ada, chatlet.NewChannel{
		Name: "launch", Kind: chatlet.KindPublic,
	})
	if err != nil {
		t.Fatalf("create #launch: %v", err)
	}
	root, err := rooms.Post(t.Context(), ada, room.Channel.ID,
		chatlet.NewMessage{Body: "what ships on friday?"})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}
	if _, err := rooms.Reply(t.Context(), bob, room.Channel.ID,
		root.Message.ID, chatlet.NewMessage{Body: "the importer"}); err != nil {
		t.Fatalf("reply: %v", err)
	}

	thread := notify.Thread{Backend: chatlet.Source, Channel: room.Channel.ID,
		Root: root.Message.ID}
	got, ok := e.ChatThreads().ReadThread(t.Context(), "bob", thread)
	if !ok {
		t.Fatal("the Chatlet thread could not be read — the prefetch would hand " +
			"the woken seat the unreadable hint")
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "what ships on friday?" ||
		got.Messages[1].Text != "the importer" {
		t.Fatalf("the thread read back as %+v", got.Messages)
	}
	if got.Messages[0].Own || !got.Messages[1].Own {
		t.Errorf("read as bob, the root is marked own=%v and bob's reply own=%v",
			got.Messages[0].Own, got.Messages[1].Own)
	}
}
