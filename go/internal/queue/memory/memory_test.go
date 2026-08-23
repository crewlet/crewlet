package memory_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// TestConformance is the whole test surface of this package.
//
// The twin has no behaviour of its own to test: everything it does is the
// EventQueue contract, so a memory-only test would be a second, weaker
// statement of the same thing that could drift away from the suite the other
// backends answer to. Anything worth asserting here belongs in queuetest,
// where every backend has to satisfy it.
func TestConformance(t *testing.T) {
	t.Parallel()
	queuetest.RunWith(t,
		func(*testing.T) queue.EventQueue { return memory.New() },
		queuetest.Capabilities{
			Peer: func(_ *testing.T, q queue.EventQueue) queue.EventQueue {
				return q.(*memory.Queue).Client()
			},
			WithRedeliveryBudget: func(_ *testing.T, budget int) queue.EventQueue {
				return memory.New(memory.WithMaxRedeliveries(budget))
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
			HeadReplayOnNak:           true,
			RejectsPublishBeforeStart: true,
			// Stop is a client disconnect, not a teardown: the broker and
			// its mail outlive it, so Start serves again.
			Restartable: true,
		})
}
