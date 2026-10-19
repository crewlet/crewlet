package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// listenSub is one client's registration as a listener on a pattern.
//
// Registered on the BROKER rather than on the client, for the reason
// [Queue.SubscribeStream] gives for the same choice: a broadcast is a broker
// fact, and a twin where one client's broadcast could not reach another
// client's listener would model a fleet in which presence never leaves the
// node.
//
// # Delivered from a queue of its own, one message at a time
//
// The real backend hands a core subscription's messages to its callback one at
// a time, on a goroutine of the client's, and never on the publisher's: a
// broadcast returns as soon as the bytes are written, whatever the listeners
// are doing. This twin does the same, so a listener that takes its own lock
// and a publisher that broadcasts while holding that lock behave here as they
// do in production. An inline dispatch would deadlock the one and certify the
// other, and would let a slow listener hold every publisher.
type listenSub struct {
	owner   *Queue
	pattern string
	handler queue.ListenFunc

	// inbox is the messages accepted and not yet handled. A broadcast that
	// finds it full is dropped for this listener, which is the broker's own
	// answer to a slow consumer: at most once means a listener that falls
	// behind loses messages rather than holding the publisher.
	inbox chan broadcastMsg

	// serving is the listener's lifetime, cancelled when its registration
	// ends; gate is what a withdrawal waits on. The gate is the scatter's
	// ([queue.AnswerGate]), because the rule is the same one: a withdrawal
	// waits for what is in flight.
	serving context.Context
	retire  context.CancelFunc
	gate    *queue.AnswerGate
	done    chan struct{}
}

// broadcastMsg is one message as a listener receives it: the literal subject
// and the listener's own copy of the bytes.
type broadcastMsg struct {
	subject string
	payload []byte
}

// listenerBacklog is how many messages one listener may hold unhandled before
// further ones are dropped for it.
//
// 4096: a presence room fans out at most a few frames a second per node, so
// even a fleet of busy rooms on one listener stays far below it, while a
// listener that has truly stopped is bounded at a few megabytes of the
// smallest frames. The real broker's own default is larger and is bounded by
// bytes as well; the twin needs only the behaviour, which is that the
// overflow is dropped rather than queued for ever.
const listenerBacklog = 4096

// Broadcast hands payload to every listener whose pattern matches subject.
func (q *Queue) Broadcast(_ context.Context, subject string, payload []byte) error {
	if err := queue.LiteralSubject(subject); err != nil {
		return fmt.Errorf("memory: broadcast: %w", err)
	}
	if len(payload) > queue.MaxPayloadBytes {
		return fmt.Errorf("memory: broadcast %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(payload), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return ErrNotStarted
	}
	if q.broker.down {
		// REFUSED, never buffered: see [queue.EventQueue.Broadcast].
		q.broker.mu.Unlock()
		return ErrBrokerDown
	}
	var targets []*listenSub
	for _, l := range q.broker.listeners {
		// topics.Match, never a private copy, for the reason
		// dispatchStreams gives.
		if topics.Match(l.pattern, subject) {
			targets = append(targets, l)
		}
	}
	q.broker.mu.Unlock()

	dropped := 0
	for _, l := range targets {
		// EACH LISTENER ITS OWN COPY, because a broker is a serialization
		// boundary: a listener that kept or edited the slice it was handed
		// must not change what the next one reads.
		msg := broadcastMsg{subject: subject, payload: bytes.Clone(payload)}
		select {
		case l.inbox <- msg:
		case <-l.serving.Done():
		default:
			dropped++
		}
	}
	if dropped > 0 {
		log.Debug("broadcast_dropped_for_slow_listeners", "subject", subject,
			"listeners", dropped)
	}
	return nil
}

// Listen makes this client a listener on pattern.
func (q *Queue) Listen(ctx context.Context, pattern string, h queue.ListenFunc) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, ErrNilHandler
	}
	if pattern == "" {
		return nil, ErrEmptySubject
	}
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))
	l := &listenSub{owner: q, pattern: pattern, handler: h,
		inbox: make(chan broadcastMsg, listenerBacklog), serving: serving, retire: retire,
		gate: queue.NewAnswerGate(), done: make(chan struct{})}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		retire()
		return nil, ErrNotStarted
	}
	if q.broker.down {
		q.broker.mu.Unlock()
		retire()
		return nil, ErrBrokerDown
	}
	q.broker.listeners = append(q.broker.listeners, l)
	q.broker.mu.Unlock()
	go l.run()
	log.Debug("broadcast_listener_added", "pattern", pattern)

	return func(ctx context.Context) error {
		q.broker.mu.Lock()
		removed := q.broker.removeListenerLocked(l)
		q.broker.mu.Unlock()
		if err := l.close(ctx); err != nil {
			return fmt.Errorf("memory: stop listening on %s: %w", pattern, err)
		}
		if removed {
			log.Debug("broadcast_listener_removed", "pattern", pattern)
		}
		return nil
	}, nil
}

// removeListenerLocked takes l off the broker, reporting whether it was on it.
// Caller holds mu.
func (b *Broker) removeListenerLocked(l *listenSub) bool {
	before := len(b.listeners)
	b.listeners = slices.DeleteFunc(b.listeners, func(s *listenSub) bool { return s == l })
	return before != len(b.listeners)
}

// run delivers l's messages one at a time until its registration ends.
func (l *listenSub) run() {
	defer close(l.done)
	for {
		select {
		case <-l.serving.Done():
			return
		case msg := <-l.inbox:
			if !l.gate.Enter(l.serving) {
				return
			}
			l.deliver(msg)
			l.gate.Leave()
		}
	}
}

// deliver runs the handler on one message, turning a panic into a lost
// message: a broadcast reaches every listener on its subject, so one bad
// handler must cost its own message and nothing else.
func (l *listenSub) deliver(msg broadcastMsg) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("broadcast_listener_panicked", "pattern", l.pattern,
				"subject", msg.subject, "panic", fmt.Sprint(r))
		}
	}()
	l.handler(msg.subject, msg.payload)
}

// close ends l's delivery and waits, bounded by ctx, for a handler already
// running. Idempotent.
func (l *listenSub) close(ctx context.Context) error {
	// CANCELLED BEFORE THE WAIT, so the delivery loop takes nothing more.
	l.retire()
	if err := l.gate.Close(ctx); err != nil {
		return fmt.Errorf("a handler is still running: %w", err)
	}
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return errors.Join(errors.New("the delivery loop has not ended"), ctx.Err())
	}
}
