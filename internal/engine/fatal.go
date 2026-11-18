package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/queue"
)

// brokerLoss is what the engine asks the jetstream queue about the connection
// under it. Declared here rather than on the queue contract: the memory twin
// has no connection to lose, so it lacks these and the engine never reports a
// loss for it, which is the true answer.
type brokerLoss interface {
	Lost() <-chan struct{}
	LostCause() error
	AcksLost() <-chan struct{}
	LostDuring(err error) error
}

// lostDuring is err, from a boot that failed over q, carrying the cause of a
// broker connection NATS closed for good, prefixed as [Engine.FatalCause]
// prefixes it so a failed start and a self-stop name one cause one way; err
// unchanged when nothing was lost. The failed step itself only says
// "connection closed"; the actionable why (refused credentials, a lowered
// max_payload, the embedded broker gone) is on the queue. Callers ask it BEFORE
// their cleanup closes the backends ([jetstream.Queue.LostDuring]).
func lostDuring(q queue.EventQueue, err error) error {
	lost, ok := q.(brokerLoss)
	if !ok || err == nil || lost.LostCause() == nil {
		return err
	}
	return fmt.Errorf("engine: stream: %w", lost.LostDuring(err))
}

// Fatal is closed once this node has lost something it cannot run without,
// and [Engine.FatalCause] then says what: a broker connection the NATS client
// closed FOR GOOD, the queue's own or an embedded broker's coordination store's
// ([jetstream.Queue.DialWatched]). An embedded broker declaring itself dead
// closes no connection, so the engine ends the node itself
// ([Engine.watchBroker]).
//
// The engine reports the loss rather than stopping: `crewlet run` owns the one
// shutdown order (drain, listener, teardown), and a second path inside the
// engine would be a second order to keep correct.
//
// Nil (never closes) on a queue with no connection to lose.
func (e *Engine) Fatal() <-chan struct{} {
	if lost, ok := e.backends.Queue.(brokerLoss); ok {
		return lost.Lost()
	}
	return nil
}

// FatalCause is why [Engine.Fatal] closed, naming what to change, and nil
// before it has.
func (e *Engine) FatalCause() error {
	lost, ok := e.backends.Queue.(brokerLoss)
	if !ok {
		return nil
	}
	if cause := lost.LostCause(); cause != nil {
		return fmt.Errorf("engine: stream: %w", cause)
	}
	return nil
}

// AcksLost is closed once the loss behind [Engine.Fatal] includes the QUEUE'S
// OWN connection, which every delivery is acked over, not only the
// coordination store's. The drain then cancels the running turns, whose
// conclusions cannot be acked ([node.Node.Drain]); this is for `crewlet run`'s
// engine_draining line, which otherwise promises they finish first.
//
// Nil (never closes) on a queue with no connection to lose.
func (e *Engine) AcksLost() <-chan struct{} {
	if lost, ok := e.backends.Queue.(brokerLoss); ok {
		return lost.AcksLost()
	}
	return nil
}
