package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrAnswerBound refuses an [EventQueue.Serve] registration bounded below one
// answer at a time, which would hold every request it is sent until its
// askers gave up.
//
// A MISTAKE IN THE CALLER, never a condition: no layer branches on it. It is a
// sentinel so that both backends refuse alike — through [NewAnswerGate], the
// one place the bound is read — and the suite can say they do.
var ErrAnswerBound = errors.New("queue: a registration answers at least one request at a time")

// AnswerGate is one [EventQueue.Serve] registration's admission: the slots
// that bound how many of its answers run at once, and the count a withdrawal
// waits on.
//
// SHARED BY BOTH BACKENDS, because the rule is the contract's rather than
// either broker's — at most the registration's own bound at once, a request
// past them waits rather than drops, and a withdrawal waits for what is in
// flight — and two copies of a concurrency rule are how the twin comes to
// certify a shape the real backend does not have. That is exactly how
// answering one request at a time went unnoticed: the twin answered in
// parallel.
//
// ONE PER REGISTRATION, never one per process or per subject family: a
// request waiting for a slot waits only behind its own registration's
// answers, which is what lets a registration whose answers wait on a queue of
// their own sit beside one whose answers do not without either holding the
// other's requests.
//
// NOT a [sync.WaitGroup]: an answer can be admitted while a withdrawal is
// starting its wait, and a WaitGroup's Add racing its Wait at zero is a
// misuse the race detector reports. Admission and closing take one lock, so
// an answer is either counted before the wait begins or refused.
type AnswerGate struct {
	slots chan struct{}

	mu     sync.Mutex
	n      int
	closed bool
	idle   chan struct{}
}

// NewAnswerGate builds a gate with answers slots, refusing a bound below one
// ([ErrAnswerBound]).
func NewAnswerGate(answers int) (*AnswerGate, error) {
	if answers < 1 {
		return nil, fmt.Errorf("%w: %d", ErrAnswerBound, answers)
	}
	return &AnswerGate{slots: make(chan struct{}, answers)}, nil
}

// Enter admits one answer, waiting for a slot. It reports false — and holds
// nothing — when the gate is closed or ctx ends first; true obliges the caller
// to call [AnswerGate.Leave] exactly once.
func (g *AnswerGate) Enter(ctx context.Context) bool {
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		<-g.slots
		return false
	}
	g.n++
	g.mu.Unlock()
	return true
}

// Leave releases an answer [AnswerGate.Enter] admitted.
func (g *AnswerGate) Leave() {
	<-g.slots
	g.mu.Lock()
	g.n--
	if g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
	g.mu.Unlock()
}

// Close refuses every later answer and waits for the admitted ones, bounded by
// ctx — an answerer that ignores its own context must not hold a shutdown for
// ever, and a withdrawal that says what it gave up on is one its caller can
// log.
func (g *AnswerGate) Close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	if g.n == 0 {
		g.mu.Unlock()
		return nil
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
