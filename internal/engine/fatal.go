package engine

import "fmt"

// brokerLoss is what the engine asks of the queue it runs on about the
// connection under it: the jetstream backend's [jetstream.Queue.Lost] and
// [jetstream.Queue.LostCause].
//
// DECLARED HERE, by the one consumer, rather than on the queue contract: the
// memory twin holds no connection that can be closed under it, so it has
// nothing to promise and does not have these — and a queue that does not is
// one this engine never reports a loss for, which is the true answer for it.
type brokerLoss interface {
	Lost() <-chan struct{}
	LostCause() error
}

// Fatal is closed once this node has lost something it cannot run without, and
// [Engine.FatalCause] then says what: today, a broker connection the NATS
// client closed FOR GOOD — the queue's own, or on an embedded broker the
// coordination store's beside it ([jetstream.Queue.DialWatched]).
//
// # Why the engine reports it rather than acts on it
//
// What a node does about it is stop — the seat watchdog's rule for a process
// that can no longer do its work, since leaving is what lets a supervisor start
// it again — and stopping is the CALLER's: `crewlet run` owns the one shutdown
// a signal takes, the drain, the listener and the teardown in their order, and
// a second path inside the engine would be a second order to keep correct. So
// the engine carries the loss up and the caller waits on it beside its signals.
//
// Nil — a channel that never closes — on a queue with no connection to lose.
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
