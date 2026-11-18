package jetstream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// TestBacklogReportsMailThePublishAlreadyAcked is the READ-YOUR-OWN-WRITE
// requirement queuetest.Capabilities places on every inspection function a
// backend supplies, asserted against the broker this one actually runs.
//
// A NET, NOT A PROOF. The property breaks only when the server's own
// accounting falls behind, under CPU contention and never on demand, so this
// case cannot fail reliably when it is broken; it never fails when it holds,
// costs nothing, and CI is the loaded machine where the violation lives. What
// defends the property is the reasoning at Backlog and queuetest.Capabilities.
func TestBacklogReportsMailThePublishAlreadyAcked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	// Read through the SECOND client, as the conformance suite's capabilities
	// do: the consumer's count lags far more often seen from another
	// connection than from the publisher's own.
	reader := inspector(q)

	const rounds = 100
	var consumerWasBehind int
	for i := range rounds {
		topic := fmt.Sprintf("seat.sync%d", i)
		const group = "grp"
		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		// Resolved BEFORE the publish so each read below is one round trip:
		// resolving them afterwards is delay enough to hide a lagging count.
		stream, err := reader.streamFor(ctx, topic)
		if err != nil {
			t.Fatalf("streamFor: %v", err)
		}
		cons, err := reader.js.Consumer(ctx, stream, consumerName(topic, group))
		if err != nil {
			t.Fatalf("Consumer: %v", err)
		}

		if err := q.Publish(ctx, topic, ev(i)); err != nil {
			t.Fatalf("Publish: %v", err)
		}

		info, err := cons.Info(ctx)
		if err != nil {
			t.Fatalf("Consumer Info: %v", err)
		}
		if int(info.NumPending)+info.NumAckPending == 0 {
			consumerWasBehind++
		}
		got, err := reader.Backlog(context.Background(), topic, group)
		if err != nil {
			t.Fatalf("Backlog: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("round %d: Backlog(%s) returned %d events for a publish that had already "+
				"been acked, want 1 — an inspection that lags its own completed write reports "+
				"whatever runs next as the thing that changed the mail",
				i, topic, len(got))
		}
	}
	// Reported, not asserted: the gap depends on load, so a quiet machine
	// would fail it.
	t.Logf("the consumer's pending count was behind on %d of %d reads; the stream's on none",
		consumerWasBehind, rounds)
}

// TestBacklogNamesThisGroupsOwnMail pins the half that IS deterministic: a
// backlog is the mail this group still holds, not the oldest mail on the
// subject.
//
// Under interest retention a message stays on the subject until every group
// with interest has acked it, so one group's finished mail sits in front of
// the other's, and a count taken from the subject's head answers with
// somebody else's mail.
func TestBacklogNamesThisGroupsOwnMail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newQueue(t)
	const topic = "seat.shared"
	const mine, theirs = "mine", "theirs"

	// Both subscriptions exist before the publish, so the stream keeps every
	// message until BOTH have acked it.
	for _, g := range []string{mine, theirs} {
		if _, err := q.EnsureSubscription(ctx, topic, g); err != nil {
			t.Fatalf("EnsureSubscription(%s): %v", g, err)
		}
	}
	for n := 1; n <= 2; n++ {
		if err := q.Publish(ctx, topic, ev(n)); err != nil {
			t.Fatalf("Publish(%d): %v", n, err)
		}
	}

	// This group finishes with the first message and hands the second back,
	// which is the ordinary shape of a seat losing its lease mid-turn.
	var once sync.Once
	handed := make(chan struct{})
	if err := q.Subscribe(ctx, topic, mine, func(_ context.Context, e *events.Event) queue.Result {
		if p, _ := events.DataAs[*probe](e); p != nil && p.N == 1 {
			return queue.Ack()
		}
		once.Do(func() { close(handed) })
		return queue.Defer(queue.DeferSeatMoved, "seat lease moved")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-handed:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the second event never reached the handler")
	}

	if got := settledBacklog(t, q, topic, mine, 1); len(got) != 1 || got[0] != 2 {
		t.Errorf("Backlog(%s, %s) = %v, want [2] — the first event is this group's finished "+
			"mail and is on the subject only because %q has not acked it",
			topic, mine, got, theirs)
	}
}

// TestBacklogExcludesMailThisGroupHasAcked is the guard on the other side of
// the same arithmetic: reading from the stream must not start reporting mail
// this group is done with just because another group still holds it.
//
// Sizing the read from the stream makes it read-your-own-write, but alone it
// would answer with every message the subject still carries, so the window's
// bottom is this group's own ack floor.
func TestBacklogExcludesMailThisGroupHasAcked(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newQueue(t)
	const topic = "seat.finished"
	const mine, theirs = "mine", "theirs"

	for _, g := range []string{mine, theirs} {
		if _, err := q.EnsureSubscription(ctx, topic, g); err != nil {
			t.Fatalf("EnsureSubscription(%s): %v", g, err)
		}
	}
	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	acked := make(chan struct{})
	var once sync.Once
	if err := q.Subscribe(ctx, topic, mine, func(context.Context, *events.Event) queue.Result {
		once.Do(func() { close(acked) })
		return queue.Ack()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-acked:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the event never reached the handler")
	}

	if got := settledBacklog(t, q, topic, mine, 0); len(got) != 0 {
		t.Errorf("Backlog(%s, %s) = %v, want none — this group acked it, and it is on the "+
			"subject only because %q has not", topic, mine, got, theirs)
	}
}

// settledBacklog reads a backlog until it holds want events, and returns each
// one's probe number.
//
// Retried because an ack is in flight: the cases assert WHICH events a
// settled backlog names, never how quickly an ack lands.
func settledBacklog(t *testing.T, q *Queue, topic, group string, want int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		evs, err := q.Backlog(context.Background(), topic, group)
		if err != nil {
			t.Fatalf("Backlog: %v", err)
		}
		var got []int
		for _, e := range evs {
			if p, _ := events.DataAs[*probe](e); p != nil {
				got = append(got, p.N)
			}
		}
		if len(got) == want || time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			return got
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}
