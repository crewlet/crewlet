package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

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

	// ONE GOROUTINE PER ANSWER, never the callback's own. A core NATS
	// subscription delivers its messages one at a time on one goroutine, so
	// an answer run inside the callback held every later request behind it
	// — every asker in the fleet waited for the sum of the answers ahead of
	// it — while the in-memory twin answered them in parallel and certified
	// the contract clean. The slots bound it ([queue.MaxConcurrentAnswers]),
	// and a request past them waits in the callback, which is backpressure
	// on this one subscription rather than a dropped request.
	gate := queue.NewAnswerGate()
	sub, err := q.nc.Subscribe(subject, func(msg *nats.Msg) {
		if stopped.Load() || msg.Reply == "" {
			return
		}
		if !gate.Enter(serving) {
			return
		}
		go func() {
			defer gate.Leave()
			// THE PROCESS'S OWN CONTEXT, not a request's: a core NATS
			// callback carries none, and the answerer's deadline is the
			// ASKER's — enforced there, because only the asker knows it.
			// What bounds this side is the asker walking away, which
			// frees the reply mailbox and makes the respond below a
			// no-op.
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
		}()
	})
	if err != nil {
		retire()
		return nil, fmt.Errorf("serve %s: %w", subject, q.brokerFailed(ctx, err))
	}
	// FLUSHED BEFORE RETURNING, and it is a correctness requirement rather
	// than tidiness. A core NATS subscription is registered when the
	// interest reaches the SERVER, not when Subscribe returns — so an
	// asker on this same server that scattered immediately after this call
	// could publish before this answerer existed, and would count a running
	// node as one that did not answer. Measured: the conformance suite's
	// peer arm failed exactly that way under load, intermittently, which is
	// the worst shape a missing flush has.
	//
	// THE FLUSH REACHES THIS CONNECTION'S OWN SERVER AND NO FURTHER. On a
	// cluster the other servers hear of the interest over their routes
	// afterwards, so an asker connected elsewhere can still miss this
	// answerer for that moment — which [queue.EventQueue] states, and
	// which no flush from this side could close: the client holds no route
	// and cannot see when a peer server has heard.
	if err := q.flush(ctx); err != nil {
		retire()
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("serve %s: register the subscription: %w", subject,
			q.brokerFailed(ctx, err))
	}
	q.log.Debug("scatter_server_added", "subject", subject)

	return func(ctx context.Context) error {
		stopped.Store(true)
		var unsubErr error
		if err := sub.Unsubscribe(); err != nil && !q.isClosed() {
			unsubErr = fmt.Errorf("stop serving %s: %w", subject, err)
		}
		// CANCELLED BEFORE THE WAIT, so an answer watching its context
		// returns now rather than finishing work nobody will read.
		retire()
		if err := gate.Close(ctx); err != nil {
			return errors.Join(unsubErr,
				fmt.Errorf("stop serving %s: answers still in flight: %w", subject, err))
		}
		q.log.Debug("scatter_server_removed", "subject", subject)
		return unsubErr
	}, nil
}

// flushBudget bounds the round trip that confirms the broker holds what this
// connection has sent, where the caller's context sets no shorter deadline of
// its own.
//
// TEN SECONDS IS THE CLIENT'S OWN Flush() DEFAULT, which both scatter verbs
// called before this existed, and the value is kept: on a connection that is
// up the round trip is a PING and a PONG, and on one that is down it ends when
// the connection returns, so a registration that has heard nothing in ten
// seconds is on a connection the reconnect budget is deciding about — waiting
// longer would only defer that decision to it. What changed is that the
// caller's own deadline and cancellation now end the wait as well, because
// Flush() honours neither.
const flushBudget = 10 * time.Second

// flush waits for the broker to confirm what this connection has sent, for at
// most [flushBudget] and never past ctx.
func (q *Queue) flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, flushBudget)
	defer cancel()
	return q.nc.FlushWithContext(ctx)
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
	out := make([][]byte, 0, max(want, 1))
	err := q.AskEach(ctx, subject, request, func(reply []byte) bool {
		out = append(out, reply)
		return want <= 0 || len(out) < want
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AskEach scatters one request and hands each reply to each as it arrives,
// until ctx is done or each answers false.
func (q *Queue) AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error {
	if subject == "" {
		return fmt.Errorf("%w: empty subject", ErrSubject)
	}
	if q.isClosed() {
		return ErrClosed
	}
	if len(request) > queue.MaxPayloadBytes {
		return fmt.Errorf("ask %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(request), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}
	// NOT INTO A CONNECTION THAT IS DOWN. The client would take the request
	// into its reconnect buffer and answer nothing, and the buffer would
	// send it once the connection came back — to servers answering a
	// mailbox nobody reads, after this asker had already been told the
	// ask failed. A scatter retains nothing, its requests included; see
	// [queue.EventQueue.Ask].
	if err := q.reachable(); err != nil {
		return fmt.Errorf("ask %s: %w", subject, err)
	}

	// ONE REPLY MAILBOX, SUBSCRIBED BEFORE THE REQUEST GOES OUT. A server
	// on the same host can answer inside a microsecond, so a subscription
	// established after the publish would miss the fastest answerer in the
	// fleet — the one a fan-out most wants.
	inbox := nats.NewInbox()
	replies, err := q.nc.SubscribeSync(inbox)
	if err != nil {
		return fmt.Errorf("ask %s: open a reply mailbox: %w", subject,
			q.brokerFailed(ctx, err))
	}
	defer func() { _ = replies.Unsubscribe() }()
	if err := q.nc.PublishRequest(subject, inbox, request); err != nil {
		// TOO LARGE FOR THIS SERVER, inside the contract's ceiling: a
		// server announcing less is refused when the queue opens, but
		// the connection may since have reached one that does — a
		// cluster member configured apart, a server restarted with less
		// — and a request the check above admitted can be one it
		// refuses. It is the same permanent refusal, so it answers the
		// contract's error.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "ask", subject,
				len(request)), queue.ErrTooLarge, err)
		}
		return fmt.Errorf("ask %s: %w", subject, q.brokerFailed(ctx, err))
	}
	// BOUNDED BY THE ASK'S OWN DEADLINE, which the client's Flush() was
	// not: it waits ten seconds whatever its caller has, so an ask given
	// two seconds against a connection that dropped after the check above
	// took ten — the one thing this verb promises not to do.
	//
	// AND A FLUSH THAT FAILS IS NOT A FAILED ASK. The request is already in
	// the connection's outbound buffer, and the flush only waits for the
	// broker to confirm it: what failed is that confirmation, so the
	// request may have reached a server — and goes out with the buffer if
	// it has not. An error here says the ask could not be made at all,
	// which a caller reads as "nobody received it": a routed write told
	// its caller it was NOT made, and a note to a running turn that nothing
	// was sent, when either may have been served. So the collection below
	// runs as it would have: what arrives before the deadline is the
	// answer, and a deadline already spent, or a connection closed for
	// good, ends it at once with whatever that is.
	if err := q.flush(ctx); err != nil {
		q.log.Debug("scatter_ask_unflushed", "subject", subject,
			"error", q.brokerFailed(ctx, err).Error())
	}

	for {
		msg, err := replies.NextMsgWithContext(ctx)
		if err != nil {
			// WHAT ARRIVED, NOT AN ERROR. A deadline reached
			// mid-scatter is the ordinary outcome this verb is
			// written for, and only the caller knows what a
			// missing answer costs it.
			return nil
		}
		if !each(msg.Data) {
			return nil
		}
	}
}
