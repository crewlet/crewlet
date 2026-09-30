package engine

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errTickOverran is the cause a [tickBound] cancels its tick with: the tick
// went its whole budget without showing progress.
var errTickOverran = errors.New("engine: the tick went its budget without progress")

// tickBound cuts a tick off once it has gone its whole budget without showing
// PROGRESS: a clock that restarts from nothing whenever the tick says it has
// finished a bounded piece of its work ([tickBound.Advanced]), stops while a
// step it EXEMPTS is running ([tickBound.Exempt]) — whose end is progress too —
// and cancels the tick, [errTickOverran] its cause, once it has run for the
// whole budget. It is the embedding duty's bound ([embedTickBudget]), and a
// [search.Budget].
//
// # Why not a deadline
//
// A tick is bounded to cut off one that WEDGED — a read that never returns, a
// provider that never answers — which the renewals of the duty's lease would
// otherwise keep holding the duty for ever. A wedge is the ABSENCE of
// progress, and a deadline measures something else: how long the tick has
// been alive. The two differ exactly where a node is slow rather than stuck.
// On a node allowed one core, shared with the searches it answers, a training
// at the largest partition an index serves is seven and a half minutes of
// k-means and filing, and three and a half more of reading every code and
// making the exact pass — every stretch of it progress — and a deadline of
// five minutes cut off every training such a node began, publishing nothing,
// so the next tick began it again: five minutes of the node's only core spent
// every tick, for ever, on an index that never arrived. Measured against progress, that node finishes the training
// once, while a step that stops advancing is cut off as soon as it ever was.
//
// # Exempt, and advancing
//
// The two ways a step shows progress suit two kinds of step. One that STREAMS
// — reading rows — calls Advanced every bounded stretch of them, so a wedge
// between two calls is noticed within the budget. One that is pure ARITHMETIC
// over values in memory cannot wedge at all — it reads its context every
// stride, so a lost lease or a stopping engine still ends it — and runs
// exempt, the whole of it one step whose end is progress, rather than
// reporting from inside functions that know nothing of a tick. What does
// neither — the provider calls, the publishes — is held to the budget as a
// whole, as every tick always was.
//
// # What it is not
//
// It is not a second lease. The tick's other bound is the duty's lease,
// renewed while the tick runs and cutting it off the moment a renewal does not
// confirm it ([embedDuty.keepClaimed]) — progress and exemptions touch this
// clock and nothing else, so neither keeps a tick running on a node that no
// longer holds the duty.
type tickBound struct {
	budget time.Duration
	cancel context.CancelCauseFunc

	mu sync.Mutex
	// quiet is how long the clock ran without progress before since, and
	// since is when the clock last started or progress was last shown —
	// neither meaning anything while an exemption is open.
	quiet time.Duration
	since time.Time
	// charged and exempt are how long the clock has run in all, and how
	// long exemptions kept it stopped, for the tick's log line — the
	// stretch in progress counted from since or exemptSince.
	charged     time.Duration
	exempt      time.Duration
	exemptSince time.Time
	open        int
	// timer fires when the quiet stretch may have run the whole budget,
	// for the start of the clock named by turn: a timer armed for an
	// earlier start, racing the exemption that stopped it, finds a
	// different turn and does nothing. Progress does not re-arm it —
	// Advanced is called every stride of a read, and must cost a lock and a
	// clock read — so a timer that fires measures the quiet stretch itself
	// and re-arms for whatever is left of it.
	timer   *time.Timer
	turn    uint64
	stopped bool
}

// boundTick derives the tick's context from ctx, bounded by budget, and
// returns it with its bound and the function that ends the tick — which
// cancels it with [context.Canceled] and stops the clock, and must be called
// when the tick returns.
func boundTick(ctx context.Context, budget time.Duration) (context.Context, *tickBound, context.CancelFunc) {
	tick, cancel := context.WithCancelCause(ctx)
	b := &tickBound{budget: budget, cancel: cancel}
	b.mu.Lock()
	b.start()
	b.mu.Unlock()
	return tick, b, func() {
		b.mu.Lock()
		if !b.stopped {
			b.pause(time.Now())
			if b.open > 0 {
				// The time the open exemption ran counts as exempt;
				// pause charged it nothing, since the clock was stopped.
				b.exempt += time.Since(b.exemptSince)
			}
			b.stopped = true
		}
		b.mu.Unlock()
		cancel(nil)
	}
}

// Advanced says the tick has just finished a bounded piece of its work, so the
// clock starts again from nothing. While an exemption is open the clock is
// stopped anyway, and this changes nothing.
func (b *tickBound) Advanced() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || b.open > 0 {
		return
	}
	now := time.Now()
	b.charged += now.Sub(b.since)
	b.quiet, b.since = 0, now
}

// Exempt stops the clock until the returned function is called, which starts
// it again from nothing — the exempt step's end is progress. Exemptions nest,
// and the clock runs again only when the last one open resumes; a resume
// called twice resumes once.
func (b *tickBound) Exempt() func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open == 0 && !b.stopped {
		now := time.Now()
		b.pause(now)
		b.exemptSince = now
	}
	b.open++
	return sync.OnceFunc(b.resume)
}

// resume closes one exemption, and starts the clock from nothing when it was
// the last.
func (b *tickBound) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open--
	if b.open == 0 && !b.stopped {
		b.exempt += time.Since(b.exemptSince)
		b.quiet = 0
		b.start()
	}
}

// pause stops a running clock at now, counting what it ran since it started
// or last saw progress, and disarms its timer; a clock already stopped by an
// exemption counts nothing. Called with mu held.
func (b *tickBound) pause(now time.Time) {
	if b.open == 0 {
		ran := now.Sub(b.since)
		b.charged += ran
		b.quiet += ran
	}
	b.turn++
	if b.timer != nil {
		b.timer.Stop()
	}
}

// start starts the clock with what is left of the budget. Called with mu held.
func (b *tickBound) start() {
	if b.stopped {
		return
	}
	b.since = time.Now()
	b.turn++
	b.arm(b.turn, b.budget-b.quiet)
}

// arm sets the timer for turn to fire after left, cancelling the tick at once
// when nothing is left — which only a timer racing the step that stopped the
// clock can leave. Called with mu held.
func (b *tickBound) arm(turn uint64, left time.Duration) {
	if left <= 0 {
		b.cancel(errTickOverran)
		return
	}
	b.timer = time.AfterFunc(left, func() { b.expire(turn) })
}

// expire is the timer for turn firing: it cancels the tick when the quiet
// stretch has run the whole budget, and otherwise — progress having been shown
// since the timer was armed — re-arms for what is left of it.
func (b *tickBound) expire(turn uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if turn != b.turn || b.open > 0 || b.stopped {
		return
	}
	b.arm(turn, b.budget-(b.quiet+time.Since(b.since)))
}

// spent is how long the clock has run, and how long exemptions have kept it
// stopped, as of now.
func (b *tickBound) spent() (charged, exempt time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	charged, exempt = b.charged, b.exempt
	switch {
	case b.stopped:
	case b.open == 0:
		charged += time.Since(b.since)
	default:
		exempt += time.Since(b.exemptSince)
	}
	return charged, exempt
}
