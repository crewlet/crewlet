package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrAnswerBound refuses an [EventQueue.Serve] registration bounded below one
// answer at a time, which would hold every request until its askers gave up.
// A caller's mistake no layer branches on; a sentinel so both backends refuse
// alike through [NewAnswerGate] and the suite can say so.
var ErrAnswerBound = errors.New("queue: a registration answers at least one request at a time")

// ErrGateClosed is a request offered to a gate whose registration is being
// withdrawn ([AnswerGate.Offer]): it is answered by nobody, as one that
// arrived after the withdrawal.
var ErrGateClosed = errors.New("queue: the registration is withdrawn")

// ErrRequestDropped marks a [DroppedRequest].
var ErrRequestDropped = errors.New("queue: a registration dropped a request past what it holds waiting")

// DroppedRequest is a request a gate dropped because the requests it already
// holds waiting for a slot are at its bound, in count or in bytes
// ([NewServeGate]): its asker hears nothing, as a slow consumer's does. First
// is the first drop since the gate last took a request, so a burst is said
// once, as the NATS client reports one slow consumer per episode.
type DroppedRequest struct {
	Waiting, WaitingBytes, Size int
	First                       bool
}

func (d *DroppedRequest) Error() string {
	return fmt.Sprintf("%v: %d requests and %d bytes were waiting, and this one is %d bytes",
		ErrRequestDropped, d.Waiting, d.WaitingBytes, d.Size)
}

func (d *DroppedRequest) Unwrap() error { return ErrRequestDropped }

// AnswerGate is one [EventQueue.Serve] registration's admission: the slots
// that bound how many of its answers run at once, the requests waiting for one
// ([AnswerGate.Offer]), and the count a withdrawal waits on.
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
	// waiting is the answers held because every slot was taken, in
	// [AnswerGate.Enter] or in queue: what tells a held request from one not
	// yet arrived ([AnswerGate.Occupancy]).
	waiting int
	closed  bool
	idle    chan struct{}

	// queue is the requests [AnswerGate.Offer] holds for a slot, in arrival
	// order, queued their bytes, and maxQueued and maxBytes the bound past
	// which it drops ([NewServeGate]); dropping is set from a drop until the
	// gate takes a request again.
	queue               []offered
	queued              int
	maxQueued, maxBytes int
	dropping            bool
}

// offered is one request waiting in a gate's queue.
type offered struct {
	arrived time.Time
	size    int
	run     func(arrived time.Time)
}

// NewAnswerGate builds a gate with answers slots, refusing a bound below one
// ([ErrAnswerBound]). It holds no request waiting: an answer past its slots
// waits in [AnswerGate.Enter], on its caller's goroutine.
func NewAnswerGate(answers int) (*AnswerGate, error) {
	if answers < 1 {
		return nil, fmt.Errorf("%w: %d", ErrAnswerBound, answers)
	}
	return &AnswerGate{slots: make(chan struct{}, answers)}, nil
}

// NewServeGate is [NewAnswerGate] for a Serve registration, whose requests are
// [AnswerGate.Offer]ed: past its slots it holds up to waiting requests and
// waitingBytes of them for a slot, and drops the rest. Each backend states the
// bound where it builds the gate (EventQueue.Serve).
func NewServeGate(answers, waiting, waitingBytes int) (*AnswerGate, error) {
	g, err := NewAnswerGate(answers)
	if err != nil {
		return nil, err
	}
	g.maxQueued, g.maxBytes = waiting, waitingBytes
	return g, nil
}

// Offer hands the gate one request that reached this process at arrived, of
// size bytes, and NEVER BLOCKS: run starts on a goroutine of its own, handed
// arrived, once a slot is held, at once or as a slot frees, in arrival order;
// the slot is the gate's to take back when run returns. It answers
// [ErrGateClosed] for a gate being withdrawn, and a [DroppedRequest] past the
// bound, and run never starts for either.
//
// NOT [AnswerGate.Enter], which parks its caller: a delivery goroutine parked
// for a slot leaves every later request in the transport's buffer, where
// nothing counts its wait, and its wait is what an answer is bounded by.
func (g *AnswerGate) Offer(arrived time.Time, size int, run func(arrived time.Time)) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return ErrGateClosed
	}
	if len(g.queue) == 0 {
		select {
		case g.slots <- struct{}{}:
			g.n++
			g.dropping = false
			g.mu.Unlock()
			go g.start(offered{arrived: arrived, size: size, run: run})
			return nil
		default:
		}
	}
	if len(g.queue) >= g.maxQueued || g.queued+size > g.maxBytes {
		drop := &DroppedRequest{Waiting: len(g.queue), WaitingBytes: g.queued, Size: size, First: !g.dropping}
		g.dropping = true
		g.mu.Unlock()
		return drop
	}
	g.queue = append(g.queue, offered{arrived: arrived, size: size, run: run})
	g.queued += size
	g.waiting++
	g.dropping = false
	g.mu.Unlock()
	return nil
}

// start runs one offered request in the slot it holds, and gives the slot back.
func (g *AnswerGate) start(o offered) {
	defer g.Leave()
	o.run(o.arrived)
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

// Leave releases an answer [AnswerGate.Enter] admitted. Where requests wait in
// the queue the slot passes to the first of them instead, never once the gate
// is withdrawn ([AnswerGate.Shut]), so no request queued before a withdrawal
// is answered after it.
func (g *AnswerGate) Leave() {
	g.mu.Lock()
	if !g.closed && len(g.queue) > 0 {
		next := g.queue[0]
		g.queue[0] = offered{}
		g.queue = g.queue[1:]
		g.queued -= next.size
		g.waiting--
		g.mu.Unlock()
		go g.start(next)
		return
	}
	// RELEASED UNDER THE LOCK, in the step that found the queue empty: freed
	// after unlocking, an Offer between the two found every slot taken and
	// queued, and the slot came free with nobody left to start it. The
	// receive cannot block, since the caller holds a slot.
	<-g.slots
	g.n--
	if g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
	g.mu.Unlock()
}

// Shut refuses every later answer and drops the requests waiting in the queue,
// at once: a withdrawal's first step, so nothing it held is answered after it.
func (g *AnswerGate) Shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shutLocked()
}

// shutLocked is [AnswerGate.Shut] under the gate's lock.
func (g *AnswerGate) shutLocked() {
	g.closed = true
	g.waiting -= len(g.queue)
	g.queue, g.queued = nil, 0
}

// Close shuts the gate ([AnswerGate.Shut]) and waits for the admitted
// answers, bounded by ctx so an answerer ignoring its own context cannot hold
// a shutdown for ever.
func (g *AnswerGate) Close(ctx context.Context) error {
	g.mu.Lock()
	g.shutLocked()
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

// arrivedKey is the context key of a request's arrival ([Arrived]).
type arrivedKey struct{}

// Arriving is ctx carrying the instant a request reached this process, on its
// runtime clock: what each backend hands an answer ([Arrived]).
func Arriving(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, arrivedKey{}, at)
}

// Arrived is when the request an answer runs for reached this process, on its
// runtime clock, stamped by the backend before any wait for a slot
// ([AnswerGate.Offer]), so an answerer counts its asker's wait from there. An
// answer not handed by a backend, one a caller runs in process, arrived as it
// is asked: now.
func Arrived(ctx context.Context) time.Time {
	if at, ok := ctx.Value(arrivedKey{}).(time.Time); ok {
		return at
	}
	return time.Now()
}

// LogDropped says, at WARN, that subject's registration dropped a request
// ([DroppedRequest]), once per burst: as visible as the slow-consumer drop it
// replaces, which the NATS client reports once per episode and the engine logs
// at WARN (internal/queue/jetstream's client errors), with no metric or alarm.
// Any other err, an [ErrGateClosed] or none, says nothing.
func LogDropped(l *slog.Logger, subject string, err error) {
	var drop *DroppedRequest
	if !errors.As(err, &drop) || !drop.First {
		return
	}
	l.Warn("scatter_request_dropped", "subject", subject, "waiting", drop.Waiting,
		"waiting_bytes", drop.WaitingBytes, "detail", "this node's answerer of the subject holds as many "+
			"requests waiting for a slot as its subscription buffers, so this one and any more until one is "+
			"taken are dropped and their askers hear nothing; the node is answering slower than it is asked")
}
