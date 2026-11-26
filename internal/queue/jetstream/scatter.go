package jetstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE SCATTER VERBS RUN ON CORE NATS, DELIBERATELY OUTSIDE JETSTREAM.
//
// [Queue.streamFor]'s durability is wrong here four ways: the request and its
// reply kept for the event retention window, an audit row per query from the
// event store's publish listener, a restarted consumer handed a request whose
// asker left long ago, and a stream per in-flight ask. Core request/reply has
// no stream, consumer, ack or retention, and a reply mailbox that lives only
// while its asker waits: a request nobody serves never existed.

// ErrNilHandler is returned when an answerer is registered with no function.
var ErrNilHandler = errors.New("jetstream: answerer is nil")

// Serve makes this process one of subject's answerers.
//
// NO QUEUE GROUP, unlike [Queue.Subscribe]: every server of a subject sees
// every request, because a scatter divides work by what the request NAMES,
// not by who receives it.
func (q *Queue) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	if h == nil {
		return nil, ErrNilHandler
	}
	if subject == "" {
		return nil, fmt.Errorf("%w: empty subject", ErrSubject)
	}
	// THE REQUESTS HELD WAITING FOR A SLOT ARE BOUND AS THE SUBSCRIPTION'S
	// PENDING BUFFER IS: the client's own defaults, which every subscription
	// here keeps. The callback no longer parks, so they are held in the gate
	// rather than in that buffer, and a request is dropped at the threshold
	// today's slow-consumer drop has: nothing buffered before is dropped now.
	gate, gateErr := queue.NewServeGate(answers, nats.DefaultSubPendingMsgsLimit,
		nats.DefaultSubPendingBytesLimit)
	if gateErr != nil {
		return nil, fmt.Errorf("serve %s: %w", subject, gateErr)
	}
	if q.isClosed() {
		return nil, ErrClosed
	}

	// THE ANSWERER'S OWN LIFETIME, cancelled by the returned Unsubscribe.
	// ctx is a registration scope, not a process one: an answerer handed it
	// would be cancelled once its registrar moved on, while its subscription
	// kept delivering.
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))

	// stopped gates dispatch besides the unsubscribe, for the reason
	// [Queue.SubscribeStream] gives: a message already in the callback path
	// would otherwise be answered after the withdrawal.
	var stopped atomic.Bool

	// ONE GOROUTINE PER ANSWER, never the callback's own, and THE CALLBACK
	// NEVER WAITS: a core NATS subscription delivers on one goroutine, so a
	// callback parked for a slot left every later request in the client's
	// buffer, where nothing counted its wait. The request's arrival is
	// stamped first, and the gate holds it for a slot ([queue.AnswerGate.Offer]).
	sub, err := q.nc.Subscribe(subject, func(msg *nats.Msg) {
		arrived := time.Now()
		if stopped.Load() || msg.Reply == "" {
			return
		}
		err := gate.Offer(arrived, len(msg.Data), func(arrived time.Time) {
			// NO REQUEST CONTEXT: the deadline is the ASKER's, enforced
			// there. Its walking away frees the reply mailbox and makes the
			// respond below a no-op. What the answer holds of the node's
			// answer budget is given back once the respond below returns.
			answering, replied := queue.Replying(queue.Arriving(serving, arrived))
			defer replied()
			reply, err := answer(answering, subject, h, msg.Data)
			if err != nil {
				// AN ERROR ANSWERS NOTHING ([queue.AnswerFunc]): the
				// asker counts this server as one not running.
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
		}, nil) // a request dropped by the withdrawal holds nothing here
		queue.LogDropped(q.log, subject, err)
	})
	if err != nil {
		retire()
		return nil, fmt.Errorf("serve %s: %w", subject, q.brokerFailed(ctx, err))
	}
	// FLUSHED BEFORE RETURNING, for correctness: the subscription exists
	// once its interest reaches the SERVER, so an asker on this server
	// scattering right after could miss it and count a running node as
	// silent (the conformance suite's peer arm, intermittently, under load).
	//
	// THE FLUSH REACHES ONLY THIS CONNECTION'S OWN SERVER: cluster peers hear
	// of the interest over their routes later, so an asker connected
	// elsewhere can miss this answerer for that moment ([queue.EventQueue]
	// states it). No client flush closes that: it cannot see a peer hear.
	if err := q.flush(ctx); err != nil {
		retire()
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("serve %s: register the subscription: %w", subject,
			q.brokerFailed(ctx, err))
	}
	q.answer(subject, gate)
	q.log.Debug("scatter_server_added", "subject", subject)

	return func(ctx context.Context) error {
		stopped.Store(true)
		gate.Shut()
		q.unanswer(subject, gate)
		var unsubErr error
		// A SUBSCRIPTION ALREADY GONE IS NO FAILURE ([queue.Unsubscribe]):
		// a second withdrawal finds nats.ErrBadSubscription, which Listen's
		// withdrawal passes over too.
		if err := sub.Unsubscribe(); err != nil && !q.isClosed() &&
			!errors.Is(err, nats.ErrBadSubscription) {
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

// answer records gate as a standing registration of this client on subject.
func (q *Queue) answer(subject string, gate *queue.AnswerGate) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.answering == nil {
		q.answering = map[string][]*queue.AnswerGate{}
	}
	q.answering[subject] = append(q.answering[subject], gate)
}

// unanswer forgets gate's registration on subject, which its withdrawal does
// first, as it stops the subscription delivering. Idempotent.
func (q *Queue) unanswer(subject string, gate *queue.AnswerGate) {
	q.mu.Lock()
	defer q.mu.Unlock()
	left := slices.DeleteFunc(q.answering[subject], func(g *queue.AnswerGate) bool { return g == gate })
	if len(left) == 0 {
		delete(q.answering, subject)
		return
	}
	q.answering[subject] = left
}

// Answering reports, of THIS client's registrations on subject, the answers
// they are running and the requests they are holding for a slot — the sum of
// their gates' [queue.AnswerGate.Occupancy].
//
// It lets the conformance suite see a registration's bound hold a request: the
// subscription's callback parked in the gate, which the connection's own
// pending count cannot tell from a request not yet handed to the callback.
func (q *Queue) Answering(subject string) (running, waiting int) {
	q.mu.Lock()
	gates := slices.Clone(q.answering[subject])
	q.mu.Unlock()
	for _, g := range gates {
		r, w := g.Occupancy()
		running, waiting = running+r, waiting+w
	}
	return running, waiting
}

// flushBudget bounds the round trip that confirms the broker holds what this
// connection has sent, where the caller's context sets no shorter deadline.
//
// Ten seconds, the client's own Flush() default: on a live connection the
// round trip is a PING and a PONG, and on a down one it ends when the
// connection returns, so ten silent seconds leave it to the reconnect budget
// and waiting longer only defers that. Unlike Flush(), the caller's deadline
// and cancellation end the wait too.
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
			panics.Recovered(panics.ScopeQueue, r)
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
	// NOT INTO A CONNECTION THAT IS DOWN: its reconnect buffer would send
	// the request later, to servers answering a mailbox nobody reads, after
	// this asker was told the ask failed. A scatter retains nothing, its
	// requests included ([queue.EventQueue.Ask]).
	if err := q.reachable(); err != nil {
		return fmt.Errorf("ask %s: %w", subject, err)
	}

	// ONE REPLY MAILBOX, SUBSCRIBED BEFORE THE REQUEST GOES OUT: a server on
	// the same host can answer inside a microsecond, and a subscription made
	// after the publish would miss the fleet's fastest answerer.
	inbox := nats.NewInbox()
	replies, err := q.nc.SubscribeSync(inbox)
	if err != nil {
		return fmt.Errorf("ask %s: open a reply mailbox: %w", subject,
			q.brokerFailed(ctx, err))
	}
	defer func() { _ = replies.Unsubscribe() }()
	if err := q.nc.PublishRequest(subject, inbox, request); err != nil {
		// TOO LARGE FOR THIS SERVER, though inside the contract's ceiling:
		// the connection may since have reached a server announcing less
		// than the one checked at open (a member configured apart, one
		// restarted with less). The same permanent refusal, so the
		// contract's error.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "ask", subject,
				len(request)), queue.ErrTooLarge, err)
		}
		return fmt.Errorf("ask %s: %w", subject, q.brokerFailed(ctx, err))
	}
	// BOUNDED BY THE ASK'S OWN DEADLINE, which Flush() ignores.
	//
	// AND A FLUSH THAT FAILS IS NOT A FAILED ASK: only the broker's
	// confirmation failed, so the buffered request may have reached a server
	// or still go out. An error would read "nobody received it" — a routed
	// write told it was NOT made, a note to a running turn that nothing was
	// sent — when either may have been served. So collection runs as usual:
	// what arrives before the deadline is the answer, and a spent deadline
	// or a connection closed for good ends it at once.
	if err := q.flush(ctx); err != nil {
		q.log.Debug("scatter_ask_unflushed", "subject", subject,
			"error", q.brokerFailed(ctx, err).Error())
	}

	for {
		msg, err := replies.NextMsgWithContext(ctx)
		if err != nil {
			// WHAT ARRIVED, NOT AN ERROR: a deadline mid-scatter is
			// this verb's ordinary outcome, and only the caller knows
			// what a missing answer costs it.
			return nil
		}
		if !each(msg.Data) {
			return nil
		}
	}
}
