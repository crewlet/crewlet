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
// A NET, NOT A PROOF, and the comment matters more than the assertion. The
// property is violated only when the server's own accounting falls behind,
// which happens under CPU contention and not on demand: sizing a backlog read
// from the consumer's pending count — the shape this replaced — was measured
// wrong about twice in two thousand reads on an idle machine and could not be
// provoked at all by loading the broker, yet it was frequent enough under a
// full parallel test run to turn CI red on a merge that touched nothing here.
// So this case cannot fail reliably when the property is broken. It also
// cannot fail when the property holds, which is what makes it worth keeping:
// it costs nothing, and CI is the loaded machine where the violation lives.
//
// What actually defends the property is the reasoning written at Backlog and
// at queuetest.Capabilities. That is the remedy the suite itself prescribes
// for a requirement no case can discover, and it is deliberately the load
// this test does not carry.
func TestBacklogReportsMailThePublishAlreadyAcked(t *testing.T) {
	ctx := t.Context()
	q := openForTest(t, Config{})
	// Read through the SECOND client, because that is the connection the
	// conformance suite's capabilities read through, and the gap depends on
	// it: measured on the publisher's own connection the consumer's count
	// was behind on about one read in nine, and from another connection on
	// about two in three.
	reader := inspector(q)

	const rounds = 100
	var consumerWasBehind int
	for i := range rounds {
		topic := fmt.Sprintf("seat.sync%d", i)
		const group = "grp"
		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		// Resolved BEFORE the publish so the reads below are one round trip
		// each. Resolving them afterwards is itself enough delay for the
		// consumer to catch up, which is how a lagging count stays invisible
		// in ordinary code.
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
	// Reported rather than asserted: the gap is a function of how busy the
	// machine is, so requiring it here would fail on a quiet one.
	t.Logf("the consumer's pending count was behind on %d of %d reads; the stream's on none",
		consumerWasBehind, rounds)
}

// TestBacklogNamesThisGroupsOwnMail pins the half that IS deterministic: a
// backlog is the mail this group still holds, not the oldest mail on the
// subject.
//
// Under interest retention a message stays on the subject until every group
// with interest has acked it, so two groups sharing a subject leave one
// group's finished mail sitting in front of the other's. Taking a count from
// the head of the subject therefore answers with somebody else's mail — which
// is what sizing the read from the consumer and starting from the head used
// to do, and this case fails every time against it.
func TestBacklogNamesThisGroupsOwnMail(t *testing.T) {
	ctx := t.Context()
	q := newQueue(t)
	const topic = "seat.shared"
	const mine, theirs = "mine", "theirs"

	// Both subscriptions exist before anything is published, so the stream
	// keeps every message until BOTH have acked it.
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
		return queue.Defer("seat lease moved")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-handed:
	case <-time.After(10 * time.Second):
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
// The pair matters. Sizing the read from the stream is what makes it
// read-your-own-write, and taken alone it would answer with every message the
// subject still carries — so the window is closed at the bottom by this
// group's own ack floor, and this case is what says so.
func TestBacklogExcludesMailThisGroupHasAcked(t *testing.T) {
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
	case <-time.After(10 * time.Second):
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
// Retried because an ack is in flight: every case here asserts WHICH events a
// settled backlog names, never how quickly an ack lands, and reading once
// would quietly test the latter.
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
		if len(got) == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}
