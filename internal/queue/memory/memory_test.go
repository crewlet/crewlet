package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// TestConformance is nearly the whole test surface of this package.
//
// Almost everything the twin does is the EventQueue contract, so a memory-only
// test is usually a second, weaker statement of the same thing that can drift
// away from the suite the other backends answer to. Anything shared belongs in
// queuetest, where every backend has to satisfy it.
//
// "Nearly" is doing real work, and this paragraph used to say "the whole" and
// "no behaviour of its own". That was untested and false, and a defect had
// already moved into the gap: this backend's DEFAULT delivery budget is a
// property of this backend alone, so the shared suite cannot assert it, and
// does not. It configures the budget explicitly in every case that touches
// one, which is correct for a portable suite and means the default is
// exercised by nothing — the default was one attempt out of step with the
// broker it tracked for as long as it existed, and the suite passed
// identically before and after the fix. A per-backend default is exactly the
// shape of thing "it all belongs in the shared suite" cannot hold.
func TestConformance(t *testing.T) {
	t.Parallel()
	queuetest.RunWith(t,
		func(_ *testing.T, opts ...queue.Option) queue.EventQueue {
			return memory.New(memory.Contract(opts...))
		},
		queuetest.Capabilities{
			Peer: func(_ *testing.T, q queue.EventQueue) queue.EventQueue {
				return q.(*memory.Queue).Client()
			},
			// This backend's knob counts redeliveries after the first
			// delivery, so N attempts is a budget of N-1. The suite asks
			// for the observable and leaves the translation here, where
			// the convention actually lives.
			WithDeliveryAttempts: func(_ *testing.T, attempts int) queue.EventQueue {
				return memory.New(memory.WithMaxRedeliveries(attempts - 1))
			},
			// Total, so there is no failure to report — which is the
			// point of the signature rather than an exemption from it: the
			// twin cannot fail to answer, and the backend that can must say
			// so instead of answering "nothing".
			Backlog: func(_ *testing.T, q queue.EventQueue, topic, group string) []*events.Event {
				return q.(*memory.Queue).Backlog(topic, group)
			},
			DeadLetters: func(_ *testing.T, q queue.EventQueue, topic, group string) []*events.Event {
				return q.(*memory.Queue).DeadLetters(topic, group)
			},
			Attachments: func(q queue.EventQueue) [][2]string {
				return q.(*memory.Queue).Attachments()
			},
			PauseHolds: func(q queue.EventQueue, topic, group string) []string {
				return q.(*memory.Queue).PauseHolds(topic, group)
			},
			Quiescing: func(q queue.EventQueue, topic, group string) bool {
				return q.(*memory.Queue).Quiescing(topic, group)
			},
			History: func(q queue.EventQueue) []*events.Event {
				return q.(*memory.Queue).History()
			},
			// Publish drains before returning, so batch boundaries are
			// deterministic and a member rotation is exact. Both are
			// deliberate properties of the twin, and both are things a
			// backend with real fetch latency cannot promise.
			InlineDispatch:   true,
			StrictRoundRobin: true,
			// NEITHER THE DEFERRAL COST NOR THE NAK ORDER IS ON THIS LIST
			// ANY MORE, and that is the difference between a degradation
			// and a divergence. This twin used to declare a free deferral,
			// and then a failure replayed from the head (HeadReplayOnNak)
			// — two properties the only shipped broker does not have, so
			// the engine and node suites that run here certified a seat
			// production does not run: one whose handoffs were free, and
			// one that retried a failed message before its conversation's
			// newer mail. The twin spends a delivery on every return and
			// puts a failure behind the mail that was waiting, as
			// JetStream does, and both capabilities are gone.
			RequiresStart: true,
			// Stop is a client disconnect, not a teardown: the broker and
			// its mail outlive it, so Start serves again.
			Restartable: true,
		})
}

// TestDefaultDeliveryBudgetMatchesJetStream pins the DEFAULT budget, which the
// conformance suite cannot: the correct value is a property of one backend, so
// the suite sets it explicitly in every case that reads one.
//
// Twenty-five TOTAL attempts, matching internal/queue/jetstream's maxDeliver.
// The two constants are written in different currencies — this backend counts
// redeliveries after the first, JetStream counts total deliveries — so they
// agree at 24 and 25 respectively, and agreeing at 25 and 25 is the bug this
// catches. Asserted on the OBSERVABLE (how many times a handler runs) rather
// than on the constant, because the constant is the half that can be wrong
// while reading correct.
//
// JetStream is the twin to track because it is the only broker this engine
// ships. Its budget is 25 rather than the 10 a free-handoff broker needs
// because every path back to it — the Nak a deferral, a hold, a pause or a
// detach returns through — spends an attempt.
//
// THAT REASONING NOW APPLIES TO BOTH BACKENDS, and this paragraph used to say
// the opposite twenty lines under the capability note that retired it: it
// argued the default had to be 25 because "this twin defers for free and would
// otherwise sit on less than half the production budget". The twin has not
// deferred for free since b1cff6f — it spends one for the same reason the
// broker does — so what makes the value load-bearing is no longer a
// divergence to compensate for but the plain fact that a handoff costs the
// same on both, and a twin sized differently would calibrate every test
// written against it to a broker nobody runs.
//
// SCOPE, measured by counterfactual rather than assumed, because a landing
// check on this test proves only that the test reads the constant. The one
// consumer of the default is internal/node's fleet suite, which builds the twin
// with no options — and instrumenting the redelivery counter shows it reaches a
// depth of 0 there: the fleet suite never Naks a message, so the budget is
// unreachable from it (positive control: the same instrument reads a non-zero
// depth on this package's own tests, so the 0 is a real absence and not a dead
// probe).
//
// So this test is the only thing holding the value, and that is the honest
// claim rather than "it fixes a live bug" — a twin that silently disagrees
// with its subject is a latent hazard for the day a case does reach the
// branch, which is reason enough to keep it.
func TestDefaultDeliveryBudgetMatchesJetStream(t *testing.T) {
	t.Parallel()
	const jetstreamTotalAttempts = 25

	q := memory.New()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	var mu sync.Mutex
	var attempts int
	err := q.Subscribe(context.Background(), "topic", "grp",
		func(context.Context, *events.Event) queue.Result {
			mu.Lock()
			attempts++
			mu.Unlock()
			return queue.Nak(errors.New("permanent failure"))
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := q.Publish(context.Background(), "topic", &events.Event{
		ID:        uuid.New(),
		Type:      "poison",
		Timestamp: time.Now().UTC(),
		Source:    "memory_test",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != jetstreamTotalAttempts {
		t.Errorf("default budget ran the handler %d times, want %d "+
			"(JetStream's maxDeliver; see defaultMaxRedeliveries)", got, jetstreamTotalAttempts)
	}
	if dl := q.DeadLetters("topic", "grp"); len(dl) != 1 {
		t.Errorf("exhausted event reached the dead-letter subject %d times, want 1", len(dl))
	}
}

// TestPublishRefusesAnUnencodablePayloadWithoutRecordingIt covers the twin's
// serialization boundary, which the conformance suite structurally cannot
// reach: every payload it publishes is a string, documented there as a
// deliberate choice so partitioning assertions never depend on a codec. That
// makes json.Marshal succeed in every shared case, so the failure branch is
// exercised by nothing portable and has to be pinned here.
//
// Measured before writing this: making the marshal failure return nil instead
// of an error passes BOTH packages. A caller would get a successful publish
// with the event never delivered, which on a queue is close to the worst silent
// failure available.
//
// Two observables, because "returns an error" is only half of it. A publish
// that refuses correctly but has already recorded the event tells the caller no
// while leaving the event behind — refusing and mutating are independent, and
// only the second assertion sees the difference. That is why the serialization
// sits above the lock in Publish, and this is what holds it there.
func TestPublishRefusesAnUnencodablePayloadWithoutRecordingIt(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	delivered := make(chan struct{}, 1)
	err := q.Subscribe(context.Background(), "topic", "grp",
		func(context.Context, *events.Event) queue.Result {
			delivered <- struct{}{}
			return queue.Ack()
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Three triggers, because they are not equally instructive. A channel
	// has no JSON representation at all, which is the obvious case and the
	// one nobody hits by accident. The json.Number pair is the case that
	// actually happens: meta assembled from previously decoded JSON carries
	// json.Number as a matter of course, so anything built from config can
	// hold one, and an overflowing or malformed number reaches this branch
	// through ordinary data. Probed: they also take different routes inside
	// Event.MarshalJSON — the malformed number fails marshalling the
	// envelope, the overflowing one fails remapping it — so the three
	// together cover the encode failure modes rather than one of them
	// three times.
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"unencodable_type", map[string]any{"ch": make(chan int)}},
		{"number_overflows_float64", map[string]any{"n": json.Number("1e1000")}},
		{"malformed_number", map[string]any{"n": json.Number("not-a-number")}},
	} {
		ev := &events.Event{
			ID:        uuid.New(),
			Type:      tc.name,
			Timestamp: time.Now().UTC(),
			Source:    "memory_test",
			Payload:   tc.payload,
		}
		if err := q.Publish(context.Background(), "topic", ev); err == nil {
			t.Errorf("%s: Publish accepted an unencodable payload; a dropped event must "+
				"never report success — the caller has no other way to learn it was lost",
				tc.name)
		}
	}

	if got := q.History(); len(got) != 0 {
		t.Errorf("a refused publish recorded %d events in history, want 0; "+
			"serialization must happen before anything is mutated", len(got))
	}
	if got := q.Backlog("topic", "grp"); len(got) != 0 {
		t.Errorf("a refused publish left %d events in the subscription mail, want 0", len(got))
	}
	select {
	case <-delivered:
		t.Error("a refused publish still reached a handler")
	default:
	}
}

// TestHistoryTrimKeepsTheNewestEntries covers WithMaxHistory and the trim
// branch, a third twin-only path the portable suite cannot reach: History is a
// memory-only inspection, and the measured high-water mark of the buffer across
// a full conformance run is 5 against a default ceiling of 10000, so the branch
// never executes there. Before this, no test in internal/queue referred to
// WithMaxHistory or the trim at all.
//
// Two observables again, and the second is the load-bearing one. A trim that
// keeps the WRONG END satisfies the length assertion perfectly while making the
// buffer useless — history exists to answer "what just happened", so dropping
// the newest entries inverts its only purpose and a size check cannot see it.
func TestHistoryTrimKeepsTheNewestEntries(t *testing.T) {
	t.Parallel()
	const ceiling = 3
	q := memory.New(memory.WithMaxHistory(ceiling))
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	for _, label := range []string{"e1", "e2", "e3", "e4", "e5"} {
		if err := q.Publish(context.Background(), "topic", &events.Event{
			ID:        uuid.New(),
			Type:      label,
			Timestamp: time.Now().UTC(),
			Source:    "memory_test",
		}); err != nil {
			t.Fatalf("Publish %s: %v", label, err)
		}
	}

	got := q.History()
	if len(got) != ceiling {
		t.Fatalf("history holds %d events, want the ceiling of %d", len(got), ceiling)
	}
	var types []string
	for _, ev := range got {
		types = append(types, ev.Type)
	}
	want := []string{"e3", "e4", "e5"}
	if !slices.Equal(types, want) {
		t.Errorf("history kept %v, want the NEWEST %d (%v); a trim that drops the newest "+
			"entries satisfies the size check while inverting what the buffer is for",
			types, ceiling, want)
	}
}

// TestABlockedDrainRestoresBeforeAFailureNotBehindIt pins the one twin
// mechanism the shared return-order case cannot reach: a batch drain whose
// earlier partition FAILED and whose later partition DEFERRED, on a mailbox
// that held nothing else.
//
// The deferral stops the drain, and what the drain never dispatched goes back
// as a hand-back — at the head, behind the deferred partition. The failure is
// a retry, which goes BEHIND everything that was waiting. The splice that puts
// the undispatched partitions back finds the deferral's run by scanning the
// mailbox's leading events for this chunk's own, and with nothing else in the
// mailbox the failed partition was the next of them: counted into the run, it
// put the undispatched partition behind a retry the broker serves after it.
//
// Three conversations, one event each, published while held so one chunk
// carries all three: a fails, b defers, c is never dispatched. The mailbox
// must then hold b (the deferral, at the head), c (handed back behind it), a
// (the retry, behind both).
func TestABlockedDrainRestoresBeforeAFailureNotBehindIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(ctx) })

	key := func(ev *events.Event) string { return ev.Type }
	var mu sync.Mutex
	var seen []string
	first := map[string]bool{}
	err := q.SubscribeBatch(ctx, "topic", "grp", func(_ context.Context, evs []*events.Event) queue.Result {
		mu.Lock()
		defer mu.Unlock()
		label := evs[0].Type
		seen = append(seen, label)
		if first[label] {
			return queue.Ack()
		}
		first[label] = true
		switch label {
		case "a":
			return queue.Nak(errors.New("a fails once"))
		case "b":
			return queue.Defer("b is handed back once")
		}
		return queue.Ack()
	}, key, queue.NewBatchOptions(0, 10))
	if err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	if err := q.PauseTopic(ctx, "topic", "grp", "test"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	base := time.Now().UTC()
	for i, label := range []string{"a", "b", "c"} {
		if err := q.Publish(ctx, "topic", &events.Event{
			ID: uuid.New(), Type: label, Source: "memory_test",
			// Oldest first, so the drain dispatches a, then b, then c.
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Publish(%s): %v", label, err)
		}
	}
	if err := q.ResumeTopic(ctx, "topic", "grp", "test"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	if got := q.Backlog("topic", "grp"); !slices.EqualFunc(got, []string{"b", "c", "a"},
		func(ev *events.Event, label string) bool { return ev.Type == label }) {
		labels := make([]string, 0, len(got))
		for _, ev := range got {
			labels = append(labels, ev.Type)
		}
		t.Fatalf("mailbox after the deferral = %v, want [b c a]: the deferral at the head, "+
			"the undispatched partition handed back behind it, and the failure behind both", labels)
	}
	if _, err := q.Unquiesce(ctx, "topic", "grp"); err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}
	// The next drain carries all three in ONE call each, ordered by their
	// own timestamps (queue.OrderForDispatch) — the mailbox order above is
	// what decides which DRAIN a message is in, not which call goes first.
	if got := q.Backlog("topic", "grp"); len(got) != 0 {
		t.Fatalf("%d message(s) still waiting after the attachment resumed", len(got))
	}
}
