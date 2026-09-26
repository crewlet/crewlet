package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/queue"
)

// serveSub is one client's registration as an answerer on a subject.
//
// Registered on the BROKER rather than on the client, for the reason
// [Queue.SubscribeStream] gives for the same choice: a scatter is a broker
// fact, and a twin where one client's ask could not reach another client's
// server would model a fleet in which the fan-out never leaves the node.
type serveSub struct {
	owner   *Queue
	subject string
	answer  queue.AnswerFunc

	// serving is the ANSWERER's context, cancelled when its registration
	// ends — which is what the contract hands an answerer, and what the
	// real backend hands it. This twin used to pass the asker's instead,
	// so an answer here ended when its asker left and one on the broker
	// ran on, and a case written against one taught nothing about the
	// other.
	serving context.Context
	retire  context.CancelFunc

	// gate bounds the answers in flight and is what a withdrawal waits
	// on — the contract's rule, in the one implementation both backends
	// share.
	gate *queue.AnswerGate
}

// Serve makes this client one of subject's answerers.
func (q *Queue) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	if h == nil {
		return nil, ErrNilHandler
	}
	if subject == "" {
		return nil, ErrEmptySubject
	}
	serving, retire := context.WithCancel(context.WithoutCancel(ctx))
	sub := &serveSub{owner: q, subject: subject, answer: h,
		serving: serving, retire: retire, gate: queue.NewAnswerGate()}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		retire()
		return nil, ErrNotStarted
	}
	q.broker.servers = append(q.broker.servers, sub)
	q.broker.mu.Unlock()
	log.Debug("scatter_server_added", "subject", subject)

	return func(ctx context.Context) error {
		q.broker.mu.Lock()
		before := len(q.broker.servers)
		q.broker.servers = slices.DeleteFunc(q.broker.servers,
			func(s *serveSub) bool { return s == sub })
		removed := before != len(q.broker.servers)
		q.broker.mu.Unlock()
		// CANCELLED BEFORE THE WAIT, so an answer watching its context
		// returns now rather than finishing work nobody will read.
		sub.retire()
		if err := sub.gate.Close(ctx); err != nil {
			return fmt.Errorf("memory: stop serving %s: answers still in "+
				"flight: %w", subject, err)
		}
		if removed {
			log.Debug("scatter_server_removed", "subject", subject)
		}
		return nil
	}, nil
}

// Ask scatters one request and collects what answers.
//
// # Concurrently, unlike every other dispatch in this twin
//
// The twin dispatches inline everywhere else, and the suite is written to
// tolerate that. Here it may not: a serial scatter takes the SUM of the
// answerers' latencies where a broker takes the MAX, so a deadline that bounds
// the real backend's ask would not bound this one, and the two would disagree
// about the one thing this verb promises — that an ask costs at most the time
// its caller gave it. An answerer that ignores its context would additionally
// make every later answerer unreachable rather than merely itself.
func (q *Queue) Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error) {
	if subject == "" {
		return nil, ErrEmptySubject
	}
	if len(request) > queue.MaxPayloadBytes {
		return nil, fmt.Errorf("memory: ask %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(request), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return nil, ErrNotStarted
	}
	servers := make([]*serveSub, 0, len(q.broker.servers))
	for _, s := range q.broker.servers {
		if s.subject == subject {
			servers = append(servers, s)
		}
	}
	q.broker.mu.Unlock()
	if len(servers) == 0 {
		return nil, nil
	}

	// THE SCATTER'S OWN CONTEXT, cancelled when the collection ends. It is
	// what stops an answerer running on after its reply stopped mattering
	// — the caller left, or want was reached — which is the difference
	// between a bounded ask and a goroutine per query that outlives it.
	scatter, done := context.WithCancel(ctx)
	defer done()

	answers := make(chan []byte, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A SLOT FIRST, waited for no longer than the asker waits:
			// past the cap a request queues rather than drops, exactly
			// as it does on the broker.
			if !s.gate.Enter(scatter) {
				return
			}
			//nolint:contextcheck // the ANSWERER's context, which the
			// contract hands an answerer: the asker's deadline bounds
			// the wait for a slot above and the collection below, never
			// the answer — see [serveSub.serving].
			reply, err := s.call(s.serving, request)
			s.gate.Leave()
			if err != nil {
				// AN ERROR ANSWERS NOTHING, which is the same
				// fact as a server that was down — see
				// [queue.AnswerFunc].
				log.Debug("scatter_answer_failed",
					"subject", subject, "error", err.Error())
				return
			}
			answers <- reply
		}()
	}
	// Closed by a goroutine of its own rather than after the loop, because
	// the collection below has to be able to leave on its deadline while
	// answerers are still running.
	go func() { wg.Wait(); close(answers) }()

	replies := make([][]byte, 0, len(servers))
	for {
		if want > 0 && len(replies) >= want {
			return replies, nil
		}
		select {
		case reply, open := <-answers:
			if !open {
				return replies, nil
			}
			replies = append(replies, reply)
		case <-ctx.Done():
			// WHAT ARRIVED, NOT AN ERROR. A deadline reached
			// mid-scatter is the ordinary outcome this verb is
			// written for, and only the caller knows what a
			// missing answer costs it.
			return replies, nil
		}
	}
}

// ErrEmptySubject is returned by the scatter verbs for a subject nobody could
// serve.
var ErrEmptySubject = errors.New("memory: empty scatter subject")

// call runs one answerer, turning a panic into a failure to answer.
//
// A PANICKING ANSWERER MUST NOT TAKE THE ASKER DOWN, on the reasoning
// [queue.LogListenerPanic] carries for a publish listener: a scatter reaches
// every process on the subject, so one bad answerer would otherwise fail every
// ask in the fleet rather than its own.
func (s *serveSub) call(ctx context.Context, request []byte) (reply []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("memory: scatter answerer on %s panicked: %v", s.subject, r)
		}
	}()
	return s.answer(ctx, request)
}
