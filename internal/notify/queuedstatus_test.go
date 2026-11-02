package notify_test

import (
	"context"
	"sync"
	"testing"
	"time"

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

// A QUEUED LINE NOTHING ENDS COMES DOWN AT ITS MAXIMUM AGE. Every way a wait
// is meant to end (the turn that picks it up, the dispatch that settles it,
// the seat leaving this node, the driver stopping) can be missed by a path
// nobody foresaw, and a line nothing ends is re-asserted on every heartbeat
// for the life of the process. An hour after it was queued it is taken down;
// before that it stands.
func TestAQueuedLineNothingEndsComesDownAtItsMaximumAge(t *testing.T) {
	var (
		mu  sync.Mutex
		now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }
	p := newPoster() // a heartbeat every 20 ms, which is where the age is checked
	d := notify.NewStatusDriver(notify.StatusOptions{Poster: p, Mode: notify.StatusAddressed, Now: clock})
	t.Cleanup(func() { d.Stop(context.Background()) })

	if !d.Queue(t.Context(), "pm", "wake-1", chatMeta(nil)) {
		t.Fatal("no queued line was raised")
	}
	p.shownAtLeast(t, 2)
	advance(59 * time.Minute)
	shown := len(p.shown())
	p.shownAtLeast(t, shown+1)
	if p.clears() != 0 {
		t.Fatalf("the line came down after 59 minutes; it stands for an hour")
	}
	advance(2 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for p.clears() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a queued line an hour and a minute old is still up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(d.Live()); got != 0 {
		t.Fatalf("the driver still holds %d indicators after the line expired", got)
	}
}
