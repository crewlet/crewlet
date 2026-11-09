package chatlet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// TWO AGENTS THANKING EACH OTHER STOP AT THE ANSWER, through the write path
// and the receipts its applier keeps (ADR-0113).
//
// Before the rule, a thread between two agents ran on: each reply woke the
// other as the thread's author or one of its speakers, and the root's author
// was obliged to answer. Now the asking message gives the asked seat a receipt,
// the answer closes it and wakes the asker, and the thanks after it, which
// asks nothing and answers nothing, wakes nobody.
func TestTwoAgentsThankingEachOtherStopAtTheAnswer(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(seat("pm", "run-0"), "launch", "pm", "eng", "ops")

	ask, err := r.store.Post(t.Context(), keyed(seat("pm", "run-1"), requested("k-ask")),
		room.Channel.ID, chatlet.NewMessage{Body: "@eng is the deploy done?",
			Mentions: []string{"eng"}, Asks: true})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	if got := wokenBy(r); len(got) != 1 || got["eng"] != chatlet.ReasonMention {
		t.Fatalf("the ask woke %v, want eng, obliged", got)
	}
	if asked := r.lastRecord().Notify.Asked; len(asked) != 1 || asked[0] != "eng" {
		t.Fatalf("the ask asked %v, want eng given a receipt", asked)
	}

	reply := func(actor chatlet.Actor, key, body string) map[string]chatlet.Reason {
		t.Helper()
		if _, err := r.store.Reply(t.Context(), keyed(actor, requested(key)),
			room.Channel.ID, ask.Message.ID, chatlet.NewMessage{Body: body}); err != nil {
			t.Fatalf("reply %q: %v", body, err)
		}
		r.drain()
		return wokenBy(r)
	}
	if got := reply(seat("eng", "run-2"), "k-done", "Done, it is live."); len(got) != 1 ||
		got["pm"] != chatlet.ReasonAnswer {
		t.Fatalf("the answer woke %v, want pm, who asked", got)
	}
	if got := reply(seat("pm", "run-3"), "k-thanks", "Thanks!"); len(got) != 0 {
		t.Fatalf("the thanks woke %v, want nobody", got)
	}
	if got := reply(seat("eng", "run-4"), "k-np", "Anytime."); len(got) != 0 {
		t.Fatalf("the reply to the thanks woke %v, want nobody", got)
	}
	if held := r.lastRecord().Notify.Held; len(held) == 0 {
		t.Error("the quiet reply reported nobody held back, so an operator cannot see the rule work")
	}
}

// A SEAT STILL OWING AN ANSWER HEARS WHAT IS SAID IN ITS THREAD: the asker's
// follow-up, which asks nothing new, reaches the seat it asked and nobody else.
func TestAFollowUpReachesTheSeatThatStillOwesAnAnswer(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(seat("pm", "run-0"), "launch", "pm", "eng", "ops")
	ask, err := r.store.Post(t.Context(), keyed(seat("pm", "run-1"), requested("k-ask")),
		room.Channel.ID, chatlet.NewMessage{Body: "@eng can you deploy?",
			Mentions: []string{"eng"}, Asks: true})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	if _, err := r.store.Reply(t.Context(), keyed(seat("pm", "run-2"), requested("k-more")),
		room.Channel.ID, ask.Message.ID, chatlet.NewMessage{Body: "to staging first"}); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	r.drain()
	if got := wokenBy(r); len(got) != 1 || got["eng"] != chatlet.ReasonOwing {
		t.Fatalf("the follow-up woke %v, want eng, which still owes the answer", got)
	}
}

// wokenBy is whom the last record woke, by reason.
func wokenBy(r *writeRound) map[string]chatlet.Reason {
	out := map[string]chatlet.Reason{}
	for _, c := range chatlet.Route(chatlet.Candidates(r.lastRecord().Notify), agents().Seat,
		r.lastRecord().Notify.Author) {
		out[c.Handle] = c.Reason
	}
	return out
}
