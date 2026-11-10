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

	mu sync.Mutex
	n  int
	// waiting is the answers [AnswerGate.Enter] holds because every slot
	// was taken when they arrived — the bound's own decision, and the one
	// fact that tells a request the bound is holding from one that has not
	// arrived yet. See [AnswerGate.Occupancy].
	waiting int
	closed  bool
	idle    chan struct{}
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
	waited := false
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	default:
		// EVERY SLOT IS TAKEN, so this answer waits, and is counted while
		// it does. Reached only when neither case above is ready, so where
		// a free slot and an ended context are both ready the select still
		// chooses between them, and counting changes no admission.
		g.mu.Lock()
		g.waiting++
		g.mu.Unlock()
		select {
		case g.slots <- struct{}{}:
			waited = true
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.mu.Unlock()
			return false
		}
	}
	g.mu.Lock()
	// UNCOUNTED AS WAITING IN THE SAME STEP that counts it admitted or
	// refuses it, so no reading of the gate finds an answer in neither.
	if waited {
		g.waiting--
	}
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

// Occupancy reports the answers the gate has admitted and not yet seen leave,
// and those it is holding because every slot was taken when they arrived.
//
// Reported so the conformance suite can see the bound hold a request. "The one
// past the bound waits" is otherwise an absence — the answerer was not called
// — and an absence read after a pause passes a registration that ignores its
// bound whenever the request it should have held reaches the answerer late. A
// held request is counted here by the bound's own decision, and a registration
// that admits it instead counts it running, so the suite reads one of the two
// rather than waiting to see whether the answerer is called.
func (g *AnswerGate) Occupancy() (running, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n, g.waiting
}
