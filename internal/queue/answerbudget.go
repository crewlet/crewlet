package queue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// The node's answer budget: the package doc's "The node's answer budget" has
// the argument.

// AnswerBudgetBytes is what every payload answer on a node holds at once, each
// at its weight (its byte bound times its measured encode multiple). 256 MiB:
// one box read at its cap (internal/sandbox's BoxReadHeldBytes, 160 MiB) beside
// a dozen answers at a page's 20 MiB, so the largest answer is admitted while
// others run, and a node holds what its reserve (internal/nodebudget's Answers
// line) keeps for it. Fixed: no measurement shows a large node starved by it.
const AnswerBudgetBytes = 256 << 20

// ErrAnswerBudget reports a payload answer that waited for the node's answer
// budget until its context ended: [AnswerBudgetBytes] were held by other
// answers for the whole wait.
var ErrAnswerBudget = errors.New("queue: the node's answer budget (queue.AnswerBudgetBytes) " +
	"stayed held by other answers for the whole wait")

// errAnswerWeight refuses a weight no budget can admit: none, or more than the
// whole budget, which would wait for ever.
var errAnswerWeight = errors.New("queue: an answer's weight is outside the node's answer budget")

// errAnswerHeldTwice refuses a second hold in one answer: a FIFO budget can
// make an answer wait behind itself, so a payload handler acquires once.
var errAnswerHeldTwice = errors.New("queue: an answer acquired the node's answer budget while it held it")

// AnswerKind is which payload answer a hold is for, the `answer` attribute of
// its wait: a closed set.
type AnswerKind string

const (
	// AnswerBoxRead is a read of a box (internal/sandbox/executor's Collect).
	AnswerBoxRead AnswerKind = "box_read"
	// AnswerHistory is a turn's or an agent's phases' history (internal/eventfan).
	AnswerHistory AnswerKind = "history"
	// AnswerMemory is a page of a seat's memory (internal/learning/memread).
	AnswerMemory AnswerKind = "memory"
	// AnswerReplay is an assistant turn's replay (internal/assistant/live).
	AnswerReplay AnswerKind = "replay"
	// AnswerLiveRelay is a live relay's batches (internal/engine).
	AnswerLiveRelay AnswerKind = "live_relay"
	// AnswerBridge is a bridged HTTP exchange's open or frame (internal/httprelay).
	AnswerBridge AnswerKind = "bridge"
	// AnswerTail is a computed tail of a run's output (internal/sandbox).
	AnswerTail AnswerKind = "tail"
	// AnswerCodeSearch is one Gitlet node's part of a code search
	// (internal/codesearch).
	AnswerCodeSearch AnswerKind = "codesearch"
	// AnswerSlice is one node's slice of a knowledge search (internal/search).
	AnswerSlice AnswerKind = "search_slice"
	// AnswerMCPCheck is one node's test of an MCP server (internal/engine).
	AnswerMCPCheck AnswerKind = "mcp_check"
	// AnswerEstate is a data node's page or capped list of the estate
	// (internal/estate).
	AnswerEstate AnswerKind = "estate"
)

// AnswerKinds is every kind this build holds for.
var AnswerKinds = []AnswerKind{AnswerBoxRead, AnswerHistory, AnswerMemory, AnswerReplay,
	AnswerLiveRelay, AnswerBridge, AnswerTail, AnswerCodeSearch, AnswerSlice, AnswerMCPCheck, AnswerEstate}

// Valid reports whether this build knows the kind.
func (k AnswerKind) Valid() bool { return slices.Contains(AnswerKinds, k) }

// AnswerOutcome is how a wait for the budget ended, its `outcome` attribute.
type AnswerOutcome string

const (
	// AnswerAdmitted is a hold taken, at once or after waiting.
	AnswerAdmitted AnswerOutcome = "admitted"
	// AnswerEnded is a wait its context ended first ([ErrAnswerBudget]).
	AnswerEnded AnswerOutcome = "ended"
)

// AnswerOutcomes is every outcome this build reports.
var AnswerOutcomes = []AnswerOutcome{AnswerAdmitted, AnswerEnded}

// Valid reports whether this build knows the outcome.
func (o AnswerOutcome) Valid() bool { return slices.Contains(AnswerOutcomes, o) }

// AnswerMeter is told what the budget measures: each wait, by kind and how it
// ended, and the bytes held whenever they move. The engine records both into
// the node's metrics; this package links no state log.
type AnswerMeter interface {
	AnswerWaited(kind AnswerKind, outcome AnswerOutcome, waited time.Duration)
	AnswerHeld(bytes int64)
}

// AnswerBudget is the node's answer budget: a FIFO weighted semaphore
// ([semaphore.Weighted] serves waiters in arrival order, so a large waiter is
// never passed by smaller ones behind it) with the bytes it holds counted for
// the meter. One per engine, shared by every payload answerer it registers.
type AnswerBudget struct {
	sem   *semaphore.Weighted
	size  int64
	meter AnswerMeter

	mu   sync.Mutex
	held int64
}

// NewAnswerBudget is an empty budget of [AnswerBudgetBytes] told to meter (nil
// measures nothing).
func NewAnswerBudget(meter AnswerMeter) *AnswerBudget {
	b := &AnswerBudget{sem: semaphore.NewWeighted(AnswerBudgetBytes), size: AnswerBudgetBytes, meter: meter}
	if meter != nil {
		meter.AnswerHeld(0)
	}
	return b
}

// Acquire waits, within ctx and in arrival order, for weight bytes of the
// budget, for an answer of kind. In an answer ([Replying]'s context) the hold
// is given back once the answer's reply is written or dropped, and the
// returned release does nothing; elsewhere (a box collected for this node's
// own turn) the release gives it back. Call the release in both cases.
func (b *AnswerBudget) Acquire(ctx context.Context, kind AnswerKind, weight int64) (func(), error) {
	if weight <= 0 || weight > b.size {
		return nil, fmt.Errorf("%w: a %s answer weighs %d bytes, of a budget of %d", errAnswerWeight,
			kind, weight, b.size)
	}
	reply, _ := ctx.Value(replyKey{}).(*replyHold)
	if reply != nil && !reply.take() {
		return nil, fmt.Errorf("%w: a %s answer of %d bytes", errAnswerHeldTwice, kind, weight)
	}
	began := time.Now()
	if err := b.sem.Acquire(ctx, weight); err != nil {
		waited := time.Since(began)
		if reply != nil {
			reply.untake()
		}
		b.waited(kind, AnswerEnded, waited)
		return nil, fmt.Errorf("%w: a %s answer of %d bytes waited %s while %d of its %d bytes were held: %w",
			ErrAnswerBudget, kind, weight, waited.Round(time.Millisecond), b.heldBytes(), b.size, err)
	}
	b.count(weight)
	b.waited(kind, AnswerAdmitted, time.Since(began))
	give := sync.OnceFunc(func() { b.give(weight) })
	if reply != nil && reply.after(give) {
		return func() {}, nil
	}
	return give, nil
}

// count moves the bytes held by d and tells the meter, in one step.
func (b *AnswerBudget) count(d int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held += d
	if b.meter != nil {
		b.meter.AnswerHeld(b.held)
	}
}

// give releases n bytes and stops counting them. Counted first, so a reading
// is never below what the semaphore still holds.
func (b *AnswerBudget) give(n int64) {
	b.count(-n)
	b.sem.Release(n)
}

// waited tells the meter how a wait ended.
func (b *AnswerBudget) waited(kind AnswerKind, outcome AnswerOutcome, d time.Duration) {
	if b.meter != nil {
		b.meter.AnswerWaited(kind, outcome, d)
	}
}

// heldBytes is what the budget's holds hold now.
func (b *AnswerBudget) heldBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held
}

// replyKey is the context key of an answer's [replyHold].
type replyKey struct{}

// replyHold is one answer's: whether it holds the budget, and what to give
// back once its reply is written or dropped.
type replyHold struct {
	mu      sync.Mutex
	holds   bool
	replied bool
	gives   []func()
}

// Replying is the context a backend runs one answer under, and the done it
// calls once that answer's reply is written to the transport or dropped (an
// error, a panic, a reply refused): every [AnswerBudget] hold taken under the
// context is given back by done, so a hold covers the reply's encoding and its
// write, not only the read ([EventQueue.Serve]). Done is safe twice.
func Replying(ctx context.Context) (context.Context, func()) {
	h := &replyHold{}
	return context.WithValue(ctx, replyKey{}, h), h.done
}

// take marks the answer holding, false where it already does.
func (h *replyHold) take() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holds {
		return false
	}
	h.holds = true
	return true
}

// untake unmarks a hold whose wait ended without it.
func (h *replyHold) untake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holds = false
}

// after defers give until the reply is done, false where it already is (an
// answer's goroutine outliving its reply), leaving give to the caller.
func (h *replyHold) after(give func()) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.replied {
		return false
	}
	h.gives = append(h.gives, give)
	return true
}

// done gives back every hold the answer took.
func (h *replyHold) done() {
	h.mu.Lock()
	gives := h.gives
	h.gives, h.replied = nil, true
	h.mu.Unlock()
	for _, give := range gives {
		give()
	}
}
