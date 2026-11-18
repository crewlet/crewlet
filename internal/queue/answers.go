package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrAnswerBound refuses an [EventQueue.Serve] registration bounded below one
// answer at a time, which would hold every request until its askers gave up.
// A caller's mistake no layer branches on; a sentinel so both backends refuse
// alike through [NewAnswerGate] and the suite can say so.
var ErrAnswerBound = errors.New("queue: a registration answers at least one request at a time")

// AnswerGate is one [EventQueue.Serve] registration's admission: the slots
// that bound how many of its answers run at once, and the count a withdrawal
// waits on.
//
// Shared by both backends: the rule (at most the bound at once, a request past
// it waits rather than drops, a withdrawal waits for what is in flight) is the
// contract's, and a second copy lets the twin certify a shape the real backend
// lacks. One per registration, so a request waits only behind its own
// registration's answers. Not a [sync.WaitGroup], whose Add racing Wait at
// zero is a misuse: admission and closing share one lock, so an answer is
// counted before the wait begins or refused.
type AnswerGate struct {
	slots chan struct{}

	mu sync.Mutex
	n  int
	// waiting is the answers [AnswerGate.Enter] holds because every slot
	// was taken: what tells a held request from one not yet arrived
	// ([AnswerGate.Occupancy]).
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
		// Every slot is taken: counted while it waits. Reached only when
		// neither case above is ready, so counting changes no admission.
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
	// Uncounted as waiting in the same step that admits or refuses it, so
	// no reading finds an answer in neither.
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
// ctx so an answerer ignoring its own context cannot hold a shutdown for ever.
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
// Reported so the conformance suite reads the bound holding a request as a
// count, not as an absence (the answerer not called) that a pause would pass
// whenever the request reached the answerer late.
func (g *AnswerGate) Occupancy() (running, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n, g.waiting
}
