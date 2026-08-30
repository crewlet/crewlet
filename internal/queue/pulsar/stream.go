package pulsar

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// streamSub is one live broadcast subscription.
type streamSub struct {
	cons   pulsar.Consumer
	cancel context.CancelFunc
	done   chan struct{}
	// stopped gates dispatch rather than relying on the consume context
	// alone: a message can already be in the handler path when Unsubscribe
	// lands, and a dashboard that keeps receiving after it unsubscribed is
	// a subscription that never really ended.
	stopped chan struct{}
	once    sync.Once
}

// close ends the subscription. It waits briefly for the loop to leave so the
// consumer is closed from one goroutine, but never indefinitely: a stream
// handler that blocks forever is a browser tab's problem and must not become
// a hung shutdown.
func (s *streamSub) close() {
	s.once.Do(func() {
		close(s.stopped)
		s.cancel()
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
		}
		s.cons.Close()
	})
}

// SubscribeStream creates an ephemeral per-caller broadcast subscription.
//
// Unlike a durable group subscription, EVERY stream subscriber receives every
// matching event — this is the dashboard's live feed, not a work queue.
// Best-effort by design: messages are acked whatever the handler does, so a
// slow consumer misses events rather than holding them, and the authoritative
// path for anything that matters answers from a query instead.
//
// On Pulsar a subject pattern becomes a REGEX subscription over the
// namespace, which is also where the tenant boundary does its work: a pattern
// can only ever match topics inside this company's namespace, so a `>`
// wildcard is a fleet-wide feed rather than an estate-wide one.
//
// The subscription is NON-DURABLE and uniquely named, so it disappears with
// its consumer. The Python engine used a durable one and called unsubscribe()
// on close, which fails on a Shared subscription with more than one consumer
// and leaves an orphan pinning events on disk when a browser tab dies
// uncleanly.
func (q *Queue) SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: nil handler for stream %s", ErrSubject, pattern)
	}
	local, err := patternRegex(pattern)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	closed := q.closed
	q.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}

	cons, err := q.client.Subscribe(pulsar.ConsumerOptions{
		// The client parses the persistent://tenant/namespace/ prefix to
		// learn which namespace to watch, then matches the remainder as a
		// regex against each fully-qualified topic it finds there.
		TopicsPattern:       q.cfg.fullTopic(local),
		AutoDiscoveryPeriod: q.cfg.discoveryPeriod(),
		SubscriptionName:    "stream-" + uuid.NewString(),
		Type:                pulsar.Shared,
		SubscriptionMode:    pulsar.NonDurable,
		// EARLIEST, and this is the one place a Pulsar pattern subscription
		// forces a choice the other backends never face.
		//
		// A pattern consumer is really N per-topic consumers, and a topic
		// that did not exist when the pattern was registered gets its
		// consumer at the NEXT discovery tick. At Latest that consumer
		// starts after whatever was published in between — so the FIRST
		// events on a brand-new subject are not delayed, they are lost,
		// permanently and silently. Measured: every case in queuetest's
		// Stream group failed exactly that way, because each publishes to
		// a subject it has just created.
		//
		// "The first events on a new seat's subject" is precisely what an
		// operator has the live feed open to watch, so losing them is the
		// worse failure. What Earliest costs instead is a replay of
		// whatever backlog a matching topic still retains when a dashboard
		// connects — bounded, because a stream subscription is
		// non-durable and acks everything immediately, and because the
		// subjects it watches (crewlet.events.>) carry a fleet-wide
		// durable group that acks them. The replay IS the unhandled
		// routing backlog, which is not the worst thing to show an
		// operator on arrival.
		SubscriptionInitialPosition: pulsar.SubscriptionPositionEarliest,
		ReceiverQueueSize:           streamReceiverQueueSize,
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe stream %s: %w", pattern, err)
	}

	// The subscription outlives the call's context on purpose: a caller
	// that registered a dashboard feed inside a request scope must not have
	// its feed torn down when that request returns. Unsubscribe (or Stop)
	// is what ends it.
	//
	// WithoutCancel rather than a bare Background, so the caller's VALUES —
	// the trace the feed was opened under, the logging fields — reach the
	// handler while its cancellation does not. The JetStream backend takes
	// the same shape, and a stream handler that could not be correlated
	// with the request that opened it is the one thing this feed is for.
	consumeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &streamSub{cons: cons, cancel: cancel, done: make(chan struct{}), stopped: make(chan struct{})}

	q.mu.Lock()
	q.streams = append(q.streams, s)
	q.mu.Unlock()

	go func() {
		defer close(s.done)
		q.runStream(consumeCtx, s, pattern, h)
	}()

	return func(context.Context) error {
		q.forgetStream(s)
		s.close()
		return nil
	}, nil
}

func (q *Queue) runStream(ctx context.Context, s *streamSub, pattern string, h queue.StreamHandler) {
	for {
		if ctx.Err() != nil {
			return
		}
		msg, err := receive(ctx, s.cons, q.cfg.receiveWait())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !isIdle(err) {
				q.log.Warn("stream_receive_failed", "pattern", pattern, "error", err.Error())
				if sleep(ctx, consumeErrorBackoff) != nil {
					return
				}
			}
			continue
		}
		select {
		case <-s.stopped:
			return
		default:
		}
		var ev events.Event
		if err := json.Unmarshal(msg.Payload(), &ev); err != nil {
			q.log.Warn("stream_decode_failed", "pattern", pattern, "error", err.Error())
		} else {
			q.runStreamHandler(ctx, h, q.cfg.localSubject(msg.Topic()), &ev)
		}
		// Acked regardless of what the handler did: stream delivery is
		// best-effort, and an unacked backlog on a per-tab subscription
		// would pin events on disk for as long as the tab is open.
		if err := s.cons.Ack(msg); err != nil {
			q.log.Warn("stream_ack_failed", "pattern", pattern, "error", err.Error())
		}
	}
}

// runStreamHandler isolates a stream handler's failure. A dashboard callback
// that panics must not take down the feed for every other subscriber.
func (q *Queue) runStreamHandler(ctx context.Context, h queue.StreamHandler, subject string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			queue.LogStreamHandlerPanic(q.log, subject, ev, r)
		}
	}()
	h(ctx, subject, ev)
}

func (q *Queue) forgetStream(s *streamSub) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, other := range q.streams {
		if other == s {
			q.streams = append(q.streams[:i], q.streams[i+1:]...)
			return
		}
	}
}
