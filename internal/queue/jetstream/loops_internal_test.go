package jetstream

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// loopEndsWithin bounds a wait for a consume loop to return. LIVENESS: a loop
// let go of returns once its fetch does, which the harness's poll keeps to a
// few tens of milliseconds, and one that never returns is what fails it.
const loopEndsWithin = 10 * time.Second

// A LOOP LET GO OF LEAVES NOTHING BEHIND IN LOOPS, however it ends.
//
// Detach and Stop keep what they let go of in Loops until each loop has
// returned, so the conformance suite's Settle can wait out a loop still inside
// a handler. What keeps that from being a leak is the loop forgetting itself as
// it returns — and a loop that had returned already when it was let go of never
// being kept, since nothing would forget it then.
//
// Mutation: drop the loop's own loopEnded, and the detached and stopped cases
// keep their entries; keep a loop that has ended on a Detach, and the last case
// does.
func TestALoopLetGoOfLeavesNothingBehindInLoops(t *testing.T) {
	t.Parallel()

	t.Run("detached", func(t *testing.T) {
		t.Parallel()
		q := openForTest(t, Config{})
		subscribeAcking(t, q, "loops.detached")
		a := attachmentOf(t, q, "loops.detached")
		if _, err := q.Detach(t.Context(), "loops.detached", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		awaitLoopEnded(t, a)
		assertNoLoops(t, q)
	})

	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		q := openForTest(t, Config{})
		subscribeAcking(t, q, "loops.stopped")
		if err := q.SubscribeBatch(t.Context(), "loops.stopped.batch", "grp",
			func(context.Context, []*events.Event) queue.Result { return queue.Ack() },
			func(*events.Event) string { return "" }, queue.DefaultBatchOptions()); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}
		single, batched := attachmentOf(t, q, "loops.stopped"), attachmentOf(t, q, "loops.stopped.batch")
		if err := q.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		awaitLoopEnded(t, single)
		awaitLoopEnded(t, batched)
		assertNoLoops(t, q)
	})

	t.Run("ended_before_it_was_let_go_of", func(t *testing.T) {
		t.Parallel()
		q := openForTest(t, Config{})
		// A loop that returns at once, as a loop that crashed does as far as
		// the bookkeeping can tell: it ends while still attached.
		if err := q.attach(t.Context(), "loops.ended", "grp", func(context.Context, *attachment) {}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		a := attachmentOf(t, q, "loops.ended")
		awaitLoopEnded(t, a)
		if _, err := q.Detach(t.Context(), "loops.ended", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		assertNoLoops(t, q)
	})
}

// A LOOP LET GO OF IS REPORTED UNTIL IT RETURNS, which is what lets the
// conformance suite's Settle wait it out: Detach does not join a running
// handler, so a loop it lets go of can still be inside one, and that handler
// can still record a delivery. A Loops that forgot it at the Detach would read
// as the loop ended while it could still dispatch.
//
// Mutation: drop the retirement from Detach, and the loop is gone from Loops
// while its handler is still running.
func TestALoopLetGoOfIsReportedUntilItReturns(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	var enterOnce sync.Once
	if err := q.Subscribe(t.Context(), "loops.held", "grp",
		func(context.Context, *events.Event) queue.Result {
			enterOnce.Do(func() { close(entered) })
			<-release
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	a := attachmentOf(t, q, "loops.held")
	if err := q.Publish(t.Context(), "loops.held", &events.Event{Type: "held"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(loopEndsWithin): //fixedwait:liveness
		t.Fatal("the handler was never handed the published event")
	}

	if _, err := q.Detach(t.Context(), "loops.held", "grp"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if !slices.ContainsFunc(q.Loops(), func(l Loop) bool { return l.ID == a.id }) {
		t.Fatal("a loop let go of while its handler ran is gone from Loops, " +
			"which reads as ended while it could still dispatch")
	}
	releaseAll()
	awaitLoopEnded(t, a)
	assertNoLoops(t, q)
}

// subscribeAcking attaches a handler that acknowledges everything to topic.
func subscribeAcking(t *testing.T, q *Queue, topic string) {
	t.Helper()
	if err := q.Subscribe(t.Context(), topic, "grp",
		func(context.Context, *events.Event) queue.Result { return queue.Ack() }); err != nil {
		t.Fatalf("Subscribe(%s): %v", topic, err)
	}
}

// attachmentOf is q's one attachment to (topic, "grp").
func attachmentOf(t *testing.T, q *Queue, topic string) *attachment {
	t.Helper()
	atts := q.lookup(topic, "grp")
	if len(atts) != 1 {
		t.Fatalf("q holds %d attachments to (%s, grp), want 1", len(atts), topic)
	}
	return atts[0]
}

// awaitLoopEnded waits for a's consume loop to return.
func awaitLoopEnded(t *testing.T, a *attachment) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(loopEndsWithin): //fixedwait:liveness
		t.Fatalf("the consume loop of (%s, %s) never returned", a.key.topic, a.key.group)
	}
}

// assertNoLoops fails if q reports a loop, or keeps one it has let go of.
func assertNoLoops(t *testing.T, q *Queue) {
	t.Helper()
	if got := q.Loops(); len(got) != 0 {
		t.Errorf("Loops = %v after every loop returned, want none", got)
	}
	q.mu.Lock()
	kept := len(q.retired)
	q.mu.Unlock()
	if kept != 0 {
		t.Errorf("q still keeps %d attachment(s) it let go of after their loops returned", kept)
	}
}
