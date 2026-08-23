package memory_test

import (
	"context"
	"errors"
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
// property of this backend alone — Pulsar ships 10 total attempts, JetStream
// 25 — so the shared suite cannot assert it, and does not. It configures the
// budget explicitly in every case that touches one, which is correct for a
// portable suite and means the default is exercised by nothing. The default
// was one attempt out of step with Pulsar for as long as it existed, and the
// suite passed identically before and after the fix. A per-backend default is
// exactly the shape of thing "it all belongs in the shared suite" cannot hold.
func TestConformance(t *testing.T) {
	t.Parallel()
	queuetest.RunWith(t,
		func(*testing.T) queue.EventQueue { return memory.New() },
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
			Backlog: func(q queue.EventQueue, topic, group string) []*events.Event {
				return q.(*memory.Queue).Backlog(topic, group)
			},
			DeadLetters: func(q queue.EventQueue, topic, group string) []*events.Event {
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
			// Pulsar-shaped replay: a NAK returns the event to the head,
			// ahead of what queued behind it. The engine no longer
			// depends on it (see rewrite/decisions/102), but the fleet
			// suite runs against this twin, so the twin must not quietly
			// stop modelling the broker it was built to model.
			// A deferral here returns the events untouched: nothing is
			// acked, nothing is counted. Pulsar's free handoff, modelled.
			FreeDeferral:              true,
			HeadReplayOnNak:           true,
			RejectsPublishBeforeStart: true,
			// Stop is a client disconnect, not a teardown: the broker and
			// its mail outlive it, so Start serves again.
			Restartable: true,
		})
}

// TestDefaultDeliveryBudgetMatchesPulsar pins the DEFAULT budget, which the
// conformance suite cannot: the correct value differs per backend, so the suite
// sets it explicitly in every case that reads one.
//
// Ten TOTAL attempts, matching internal/queue/pulsar's maxDeliveries = 10. The
// two constants are written in different currencies — this backend counts
// redeliveries after the first, Pulsar counts total deliveries — so they agree
// at 9 and 10 respectively, and agreeing at 10 and 10 is the bug this catches.
// Asserted on the OBSERVABLE (how many times a handler runs) rather than on the
// constant, because the constant is the half that was already wrong while
// reading correct.
//
// Pulsar is the twin to track, not JetStream: both this backend and Pulsar
// deliver a deferral for free, while JetStream spends an attempt on one and
// budgets 25 to cover handoff. If Pulsar's number moves, this moves with it.
func TestDefaultDeliveryBudgetMatchesPulsar(t *testing.T) {
	t.Parallel()
	const pulsarTotalAttempts = 10

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
	if got != pulsarTotalAttempts {
		t.Errorf("default budget ran the handler %d times, want %d "+
			"(Pulsar's maxDeliveries; see defaultMaxRedeliveries)", got, pulsarTotalAttempts)
	}
	if dl := q.DeadLetters("topic", "grp"); len(dl) != 1 {
		t.Errorf("exhausted event reached the dead-letter subject %d times, want 1", len(dl))
	}
}
