package chatlet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events/types"
)

// AN AGENT'S MESSAGE THAT ASKS NOTHING HOLDS THE AGENT IT NAMES, AND SAYS SO:
// through the feed and the parser it is one held wake for that seat, under the
// reason the quiet rule held it at, on the message's own id, and no delivery.
func TestAnAgentsQuietMessageRecordsTheSeatItHeld(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(seat("pm", "run-0"), "launch", "pm", "eng", "ops")
	if _, err := r.store.Post(t.Context(), keyed(seat("pm", "run-1"), requested("k-note")),
		room.Channel.ID, chatlet.NewMessage{Body: "@eng the deploy went out, nothing for you.",
			Mentions: []string{"eng"}, Asks: false}); err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	rec := r.lastRecord()
	if rec.Notify == nil || len(rec.Notify.Held) == 0 {
		t.Fatal("the quiet message held nobody, so there is no held wake to record")
	}
	var held, woken int
	for _, w := range wakes(t, rec, company(t, []string{"pm", "eng", "ops"})) {
		switch {
		case w.Held && w.To.Handle == "eng":
			held++
			if w.Origin.Kind != types.OriginChatMessage || w.Origin.Reason != string(chatlet.ReasonMention) ||
				w.WakeID.String() == "00000000-0000-0000-0000-000000000000" {
				t.Errorf("eng's held wake is %+v, want the message under the mention it was held at", w)
			}
		case w.To.Handle == "eng":
			woken++
		}
	}
	if held != 1 || woken != 0 {
		t.Errorf("eng was held %d times and woken %d, want held once and never woken", held, woken)
	}
}
