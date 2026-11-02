package notify_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

// A PERSON'S MESSAGE WAITING BEHIND A BUSY SEAT IS SHOWN AS QUEUED, AND THE
// TURN THAT PICKS IT UP TAKES THE SAME LINE OVER IN PLACE.
//
// On a live company a person's direct message reached a seat that was working
// through a backlog, and nothing in the conversation said it had arrived until
// the seat got to it. The queued line says it has, and how many tasks are
// ahead of it; it is the same indicator the turn then raises, so the line
// moves on to the turn's phase without coming down and going up again.
func TestAQueuedLineBecomesTheTurnsWorkingLine(t *testing.T) {
	p := quiet()
	d := driver(t, p, notify.StatusAddressed)

	if !d.Queue(t.Context(), "pm", "wake-1", chatMeta(nil)) {
		t.Fatal("a person's direct message waiting behind the seat raised no queued line")
	}
	shown := p.shownAtLeast(t, 1)
	if shown[0] != "is queued behind its current task" || !p.queuedPosted()[0] {
		t.Fatalf("the queued line said %q (queued %v)", shown[0], p.queuedPosted()[0])
	}

	s := d.Begin(t.Context(), "pm", "turn-1", "execute", chatMeta(nil))
	if s == nil {
		t.Fatal("the turn that picked the message up raised nothing")
	}
	shown = p.shownAtLeast(t, 2)
	if queued := p.queuedPosted(); queued[1] || shown[1] == "" ||
		shown[1] == "is queued behind its current task" {
		t.Fatalf("the turn's line is %q (queued %v), want the turn's own phase", shown[1], queued[1])
	}
	if p.clears() != 0 {
		t.Fatalf("the line came down %d time(s) between queued and working; it moves in place", p.clears())
	}

	d.Dequeue(t.Context(), "pm", []string{"wake-1"})
	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatalf("the indicator was cleared %d times once the turn ended, want once", p.clears())
	}
}

// A SECOND PERSON WAITING IS TOLD HOW MANY TASKS ARE AHEAD, and when the first
// one's message is picked up, the count comes down.
func TestASecondQueuedMessageCountsWhatIsAheadOfIt(t *testing.T) {
	p := quiet()
	d := driver(t, p, notify.StatusAddressed)

	d.Queue(t.Context(), "pm", "wake-1", chatMeta(nil))
	p.shownAtLeast(t, 1)
	other := chatMeta(func(m map[string]string) { m["channel"], m["ts"] = "C2", "1718.010" })
	d.Queue(t.Context(), "pm", "wake-2", other)
	if shown := p.shownAtLeast(t, 2); shown[1] != "is queued behind 2 tasks" {
		t.Fatalf("the second message's line is %q, want it behind 2 tasks", shown[1])
	}

	d.Dequeue(t.Context(), "pm", []string{"wake-1"})
	shown := p.shownAtLeast(t, 3)
	if last := shown[len(shown)-1]; last != "is queued behind its current task" {
		t.Fatalf("after the first message was picked up the second says %q", last)
	}
	if p.clears() != 1 {
		t.Fatalf("taking the first message's wait away cleared %d line(s), want its own", p.clears())
	}
}

// A TURN THAT ENDS WHILE A MESSAGE IN ITS THREAD STILL WAITS LEAVES THE LINE
// UP, NOW QUEUED: the turn was not the one the message waits for.
func TestATurnEndingBeforeAWaitingMessageLeavesItQueued(t *testing.T) {
	p := quiet()
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "pm", "turn-1", "execute", chatMeta(nil))
	p.shownAtLeast(t, 1)
	d.Queue(t.Context(), "pm", "wake-2", chatMeta(nil))
	s.End(t.Context(), false)
	shown := p.shownAtLeast(t, 2)
	if last := shown[len(shown)-1]; last != "is queued behind its current task" {
		t.Fatalf("after the turn ended the line says %q, want it queued", last)
	}
	if p.clears() != 0 {
		t.Fatal("the line came down while a message still waited in its thread")
	}
	d.Dequeue(t.Context(), "pm", []string{"wake-2"})
	if p.clears() != 1 {
		t.Fatalf("the last wait taken away cleared %d line(s), want one", p.clears())
	}
}

// A TYPING INDICATOR SAYS "WRITING" WHATEVER IT IS TOLD, so a backend that draws
// no words raises nothing for a message that is only waiting.
func TestNoQueuedLineWhereNoWordsAreDrawn(t *testing.T) {
	p := quiet()
	p.text = false
	d := driver(t, p, notify.StatusAddressed)
	if d.Queue(t.Context(), "pm", "wake-1", chatMeta(nil)) {
		t.Fatal("a backend that draws no words raised a queued indicator")
	}
}

// A MESSAGE NOBODY ADDRESSED TO THE SEAT IS NOT QUEUED ON IT, by the same rule
// the working indicator follows.
func TestAMessageThatAsksTheSeatNothingIsNotQueued(t *testing.T) {
	p := quiet()
	d := driver(t, p, notify.StatusAddressed)
	room := chatMeta(func(m map[string]string) { m["channel_type"] = "channel" })
	if d.Queue(t.Context(), "pm", "wake-1", room) {
		t.Fatal("a room message that asks the seat nothing raised a queued line")
	}
}

// AN ANNOUNCEMENT THAT ARRIVES AFTER ITS MESSAGE WAS PICKED UP QUEUES NOTHING.
// The wake and its announcement travel separately, so the turn can start, and
// settle the message, before the node hears it was waiting.
//
// Mutation: drop the settled check, and the late wait holds the line up as
// "queued" when that turn ends, for a message already answered.
func TestALateAnnouncementForASettledMessageQueuesNothing(t *testing.T) {
	p := quiet()
	d := driver(t, p, notify.StatusAddressed)
	s := d.Begin(t.Context(), "pm", "turn-1", "execute", chatMeta(nil))
	p.shownAtLeast(t, 1)
	d.Dequeue(t.Context(), "pm", []string{"wake-1"})
	if d.Queue(t.Context(), "pm", "wake-1", chatMeta(nil)) {
		t.Fatal("a message already picked up was queued again")
	}
	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatalf("the line was cleared %d times when the turn ended, want once", p.clears())
	}
	for _, line := range p.shown() {
		if line == "is queued behind its current task" {
			t.Fatal("the line said queued for a message already picked up")
		}
	}
}
