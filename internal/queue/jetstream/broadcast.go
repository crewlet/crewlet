package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE BROADCAST VERBS RUN ON CORE NATS, for the scatter verbs' reasons (see
// scatter.go): on a stream a heartbeat would be retained, audited, replayed
// after its moment and given a stream per subject. Core NATS keeps nothing: a
// message with no subscriber is gone, and a subscriber that falls behind loses
// the overflow, which is what at most once means.

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
	// NOT INTO A CONNECTION THAT IS DOWN, for Ask's reason: the reconnect
	// buffer would deliver the frame after its moment.
	if err := q.reachable(); err != nil {
		return fmt.Errorf("broadcast %s: %w", subject, err)
	}
	if err := q.nc.Publish(subject, payload); err != nil {
		// TOO LARGE FOR THIS SERVER inside the contract's ceiling, as for
		// Ask: the same permanent refusal.
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

	// stopped and the gate keep the withdrawal's promise: a message may
	// already be in the callback path when the caller unsubscribes, so
	// stopped refuses one not yet started and the gate ([queue.AnswerGate])
	// is waited on for one that has. Bounded at ONE because the client runs
	// this callback one message at a time.
	gate, err := queue.NewAnswerGate(1)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", pattern, err)
	}
	var stopped atomic.Bool
	// THE LISTENER'S OWN LIFETIME, for the reason Serve keeps one: a core
	// callback carries no context, and the caller's is a registration scope.
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))
	// THE CLIENT'S OWN GOROUTINE RUNS THE HANDLER, one message at a time,
	// which is the contract's ordering; with nothing to reply to, nothing is
	// worth a goroutine per message (package doc, "A handler runs inline on
	// the goroutine that took its delivery";
	// TestAHandlerRunsInlineOnTheGoroutineThatTookItsDelivery).
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
	// FLUSHED BEFORE RETURNING, for Serve's reason: a broadcast sent right
	// after the call would otherwise race past the interest.
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
			panics.Recovered(panics.ScopeQueue, r)
			q.log.Error("broadcast_listener_panicked", "pattern", pattern,
				"subject", subject, "panic", fmt.Sprint(r))
		}
	}()
	h(subject, payload)
}
