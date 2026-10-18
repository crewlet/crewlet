package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// THE BROADCAST VERBS RUN ON CORE NATS, for every reason the scatter verbs do
// (see scatter.go): a heartbeat on a stream would be retained for the event
// retention window, audited by the publish listener, replayed to a consumer
// restarted after its moment, and provisioned a stream per subject. Core
// publish and subscribe keep nothing: a message with no subscriber is gone,
// and a subscriber that falls behind is told so by the client and loses the
// overflow, which is what at most once means.

// Broadcast publishes payload to every current listener of subject.
func (q *Queue) Broadcast(ctx context.Context, subject string, payload []byte) error {
	if err := queue.LiteralSubject(subject); err != nil {
		return fmt.Errorf("%w: %w", ErrSubject, err)
	}
	if q.isClosed() {
		return ErrClosed
	}
	if len(payload) > queue.MaxPayloadBytes {
		return fmt.Errorf("broadcast %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(payload), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}
	// NOT INTO A CONNECTION THAT IS DOWN, for Ask's reason: the client would
	// hold the message in its reconnect buffer and send it once the
	// connection came back, a frame delivered after its moment to listeners
	// that have since been told otherwise.
	if err := q.reachable(); err != nil {
		return fmt.Errorf("broadcast %s: %w", subject, err)
	}
	if err := q.nc.Publish(subject, payload); err != nil {
		// TOO LARGE FOR THIS SERVER inside the contract's ceiling, as for
		// Ask: a server announcing less than the contract may since have
		// been reached, and the refusal is the same permanent one.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "broadcast", subject,
				len(payload)), queue.ErrTooLarge, err)
		}
		return fmt.Errorf("broadcast %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return nil
}

// Listen makes this process a listener on pattern until the returned
// Unsubscribe runs.
//
// A PLAIN SUBSCRIPTION, NEVER A QUEUE GROUP: every listener must hear every
// message, for the reason Serve gives.
func (q *Queue) Listen(ctx context.Context, pattern string, h queue.ListenFunc) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, ErrNilHandler
	}
	if pattern == "" {
		return nil, fmt.Errorf("%w: empty pattern", ErrSubject)
	}
	if q.isClosed() {
		return nil, ErrClosed
	}

	// THE LISTENER'S OWN LIFETIME, for the reason Serve keeps one: a core
	// callback carries no context, and the caller's is a registration scope.
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))
	// stopped and the gate together are what make the withdrawal's promise
	// true: a message can already be in the client's callback path when the
	// caller unsubscribes, so stopped refuses one that has not started, and
	// the gate is waited on for one that has. The gate is the scatter's
	// ([queue.AnswerGate]), shared by both backends for the same rule.
	var stopped atomic.Bool
	gate := queue.NewAnswerGate()
	// THE CLIENT'S OWN GOROUTINE RUNS THE HANDLER, one message at a time,
	// which is the contract's ordering: an async subscription delivers on
	// one goroutine per subscription. Unlike Serve's answers there is
	// nothing to reply to, so nothing here is worth a goroutine per
	// message.
	sub, err := q.nc.Subscribe(pattern, func(msg *nats.Msg) {
		if stopped.Load() || !gate.Enter(serving) {
			return
		}
		defer gate.Leave()
		listen(q, pattern, h, msg.Subject, msg.Data)
	})
	if err != nil {
		retire()
		return nil, fmt.Errorf("listen %s: %w", pattern, q.brokerFailed(ctx, err))
	}
	// FLUSHED BEFORE RETURNING, for Serve's reason: the interest is
	// registered when it reaches the server, and a broadcast sent through
	// this server straight after the call would otherwise race past it.
	if err := q.flush(ctx); err != nil {
		retire()
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("listen %s: register the subscription: %w", pattern,
			q.brokerFailed(ctx, err))
	}
	q.log.Debug("broadcast_listener_added", "pattern", pattern)

	return func(ctx context.Context) error {
		stopped.Store(true)
		var unsubErr error
		if err := sub.Unsubscribe(); err != nil && !q.isClosed() &&
			!errors.Is(err, nats.ErrBadSubscription) {
			unsubErr = fmt.Errorf("stop listening on %s: %w", pattern, err)
		}
		retire()
		if err := gate.Close(ctx); err != nil {
			return errors.Join(unsubErr,
				fmt.Errorf("stop listening on %s: a handler is still running: %w", pattern, err))
		}
		q.log.Debug("broadcast_listener_removed", "pattern", pattern)
		return unsubErr
	}, nil
}

// listen runs one handler on one message, turning a panic into a lost message:
// one bad listener must not stop the subscription's delivery goroutine, which
// every later message on the pattern arrives through.
func listen(q *Queue, pattern string, h queue.ListenFunc, subject string, payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("broadcast_listener_panicked", "pattern", pattern,
				"subject", subject, "panic", fmt.Sprint(r))
		}
	}()
	h(subject, payload)
}
