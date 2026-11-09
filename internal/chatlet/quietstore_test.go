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

// THE SEAT THAT ASKED IS WOKEN BY ANY REPLY IN ITS THREAD (ADR-0113): a
// person's reply naming somebody else reaches the agent that asked, and so
// does another agent's reply that is not addressed to it.
func TestTheSeatThatAskedIsWokenByAnyReply(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("sara", "jane"))
	room := r.room(seat("amir", "run-0"), "launch", "amir", "dev", "sara", "jane")
	ask, err := r.store.Post(t.Context(), keyed(seat("amir", "run-1"), requested("k-ask")),
		room.Channel.ID, chatlet.NewMessage{Body: "@sara can we ship friday?",
			Mentions: []string{"sara"}, Asks: true})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	if _, err := r.store.Reply(t.Context(), keyed(person("jane"), requested("k-jane")),
		room.Channel.ID, ask.Message.ID, chatlet.NewMessage{Body: "@sara see the release notes",
			Mentions: []string{"sara"}}); err != nil {
		t.Fatalf("jane's reply: %v", err)
	}
	r.drain()
	if got := wokenBy(r); got["amir"] != chatlet.ReasonAwaiting {
		t.Fatalf("a person's reply naming sara in amir's open question woke %v, want amir", got)
	}
	if _, err := r.store.Reply(t.Context(), keyed(seat("dev", "run-2"), requested("k-dev")),
		room.Channel.ID, ask.Message.ID, chatlet.NewMessage{Body: "the build is green"}); err != nil {
		t.Fatalf("dev's reply: %v", err)
	}
	r.drain()
	if got := wokenBy(r); got["amir"] != chatlet.ReasonAwaiting {
		t.Fatalf("another agent's reply in amir's open question woke %v, want amir", got)
	}
}

// A TOP-LEVEL ANSWER IN A NAMED ROOM ANSWERS THE AGENT THAT ASKED there, at
// the top level: it wakes the asker once and closes the ask, so a second
// remark wakes nobody.
func TestATopLevelAnswerInANamedRoomReachesItsAsker(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(seat("b", "run-0"), "platform", "a", "b")
	if _, err := r.store.Post(t.Context(), keyed(seat("b", "run-1"), requested("k-ask")),
		room.Channel.ID, chatlet.NewMessage{Body: "@a is the migration done?",
			Mentions: []string{"a"}, Asks: true}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	say := func(key, body string) map[string]chatlet.Reason {
		t.Helper()
		if _, err := r.store.Post(t.Context(), keyed(seat("a", "run-"+key), requested(key)),
			room.Channel.ID, chatlet.NewMessage{Body: body}); err != nil {
			t.Fatalf("post %q: %v", body, err)
		}
		r.drain()
		return wokenBy(r)
	}
	if got := say("k-done", "The migration is done."); got["b"] != chatlet.ReasonAnswer {
		t.Fatalf("a's top-level answer woke %v, want b, who asked", got)
	}
	if got := say("k-more", "Also cleaned the old tables."); len(got) != 0 {
		t.Fatalf("a's next remark woke %v, want nobody: the ask was answered", got)
	}
}
