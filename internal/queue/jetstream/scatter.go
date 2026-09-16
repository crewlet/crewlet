package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// THE SCATTER VERBS RUN ON CORE NATS, DELIBERATELY OUTSIDE JETSTREAM.
//
// Every other verb in this backend goes through [Queue.streamFor], which
// provisions a stream and makes the message durable. That is exactly wrong for
// this one, and in four separate ways: a request would be retained for the
// event retention window, a reply with it; the event store's publish listener
// would write an audit row per query; a consumer restarted mid-flight would be
// handed a request whose asker left minutes ago; and a subject per in-flight
// ask would provision a stream per query.
//
// So Ask and Serve use the connection's own request/reply, which is what core
// NATS is for: no stream, no consumer, no ack, no retention, and a reply
// mailbox that exists only while its asker is waiting on it. A request nobody
// serves is a request that never existed.

// ErrNilHandler is returned when an answerer is registered with no function.
var ErrNilHandler = errors.New("jetstream: answerer is nil")

// Serve makes this process one of subject's answerers.
//
// A QUEUE GROUP IS NOT USED, which is the difference between this and
// [Queue.Subscribe]: every server of a subject must see every request, because
// a scatter divides work by what the request NAMES rather than by who happens
// to receive it.
func (q *Queue) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, ErrNilHandler
	}
	if subject == "" {
		return nil, fmt.Errorf("%w: empty subject", ErrSubject)
	}
	if q.isClosed() {
		return nil, ErrClosed
	}

	// THE ANSWERER'S OWN LIFETIME, cancelled by the returned Unsubscribe.
	// A core NATS callback carries no context of its own, and the one the
	// caller passed to Serve is a registration scope rather than a
	// process one — so an answerer handed it would be cancelled the moment
	// whoever registered it moved on, while its subscription kept
	// delivering.
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))

	// stopped gates dispatch rather than relying on the unsubscribe alone,
	// for the reason [Queue.SubscribeStream] gives: a message can already
	// be in the client's callback path when the caller unsubscribes, and
	// an answerer that keeps answering after it withdrew is a registration
	// that never really ended.
	var stopped atomic.Bool
	sub, err := q.nc.Subscribe(subject, func(msg *nats.Msg) {
		if stopped.Load() || msg.Reply == "" {
			return
		}
		// THE PROCESS'S OWN CONTEXT, not a request's: a core NATS
		// callback carries none, and the answerer's deadline is the
		// ASKER's — enforced there, because only the asker knows it.
		// What bounds this side is the asker walking away, which frees
		// the reply mailbox and makes the respond below a no-op.
		reply, err := answer(serving, subject, h, msg.Data)
		if err != nil {
			// AN ERROR ANSWERS NOTHING — see [queue.AnswerFunc].
			// Nothing is published, so the asker counts this
			// server as one that did not answer, which is the
			// same fact as a server that was not running.
			q.log.Debug("scatter_answer_failed",
				"subject", subject, "error", err.Error())
			return
		}
		if err := msg.Respond(reply); err != nil {
			q.log.Debug("scatter_reply_failed",
				"subject", subject, "error", err.Error(),
				"detail", "the asker's reply mailbox is gone, which is what "+
					"a deadline that passed looks like from here")
		}
	})
	if err != nil {
		retire()
		return nil, fmt.Errorf("serve %s: %w", subject, err)
	}
	// FLUSHED BEFORE RETURNING, and it is a correctness requirement rather
	// than tidiness. A core NATS subscription is registered when the
	// interest reaches the SERVER, not when Subscribe returns — so an
	// asker that scattered immediately after this call could publish
	// before this answerer existed, and would count a running node as one
	// that did not answer. Measured: the conformance suite's peer arm
	// failed exactly that way under load, intermittently, which is the
	// worst shape a missing flush has.
	if err := q.nc.Flush(); err != nil {
		retire()
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("serve %s: register the subscription: %w", subject, err)
	}
	q.log.Debug("scatter_server_added", "subject", subject)

	return func(context.Context) error {
		stopped.Store(true)
		retire()
		if err := sub.Unsubscribe(); err != nil && !q.isClosed() {
			return fmt.Errorf("stop serving %s: %w", subject, err)
		}
		q.log.Debug("scatter_server_removed", "subject", subject)
		return nil
	}, nil
}

// answer runs one answerer, turning a panic into a failure to answer.
func answer(ctx context.Context, subject string, h queue.AnswerFunc, request []byte) (reply []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jetstream: scatter answerer on %s panicked: %v", subject, r)
		}
	}()
	return h(ctx, request)
}

// Ask scatters one request and collects what answers before ctx is done.
func (q *Queue) Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error) {
	if subject == "" {
		return nil, fmt.Errorf("%w: empty subject", ErrSubject)
	}
	if q.isClosed() {
		return nil, ErrClosed
	}
	if len(request) > queue.MaxPayloadBytes {
		return nil, fmt.Errorf("ask %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(request), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}

	// ONE REPLY MAILBOX, SUBSCRIBED BEFORE THE REQUEST GOES OUT. A server
	// on the same host can answer inside a microsecond, so a subscription
	// established after the publish would miss the fastest answerer in the
	// fleet — the one a fan-out most wants.
	inbox := nats.NewInbox()
	replies, err := q.nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("ask %s: open a reply mailbox: %w", subject, err)
	}
	defer func() { _ = replies.Unsubscribe() }()
	if err := q.nc.PublishRequest(subject, inbox, request); err != nil {
		return nil, fmt.Errorf("ask %s: %w", subject, err)
	}
	if err := q.nc.Flush(); err != nil {
		return nil, fmt.Errorf("ask %s: flush: %w", subject, err)
	}

	out := make([][]byte, 0, max(want, 1))
	for want <= 0 || len(out) < want {
		msg, err := replies.NextMsgWithContext(ctx)
		if err != nil {
			// WHAT ARRIVED, NOT AN ERROR. A deadline reached
			// mid-scatter is the ordinary outcome this verb is
			// written for, and only the caller knows what a
			// missing answer costs it.
			return out, nil
		}
		out = append(out, msg.Data)
	}
	return out, nil
}
