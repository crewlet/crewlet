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

// tickBound cancels a tick, cause [errTickOverran], after a whole budget
// without PROGRESS: [tickBound.Advanced] restarts the clock, which stops while
// an [tickBound.Exempt] step runs, whose end is progress. It is the embedding
// duty's bound ([embedTickBudget]) and a [search.Budget].
//
// Not a deadline: a WEDGED tick, which the lease's renewals would hold for
// ever, lacks progress, not youth, and a deadline cuts off a slow node's honest
// training every tick, so the index never publishes. A STREAMING step calls
// Advanced every bounded stretch; in-memory ARITHMETIC, checking its context
// every stride, runs exempt; the rest is held to the budget whole. Progress
// and exemptions never touch the duty's lease ([dutyRenewal.keep]), which
// still ends the tick.
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
	// timer fires when the quiet stretch may have run the budget, for the
	// clock start numbered turn; a stale timer racing an exemption finds
	// another turn and does nothing. Progress does not re-arm it (Advanced
	// runs every stride of a read and must stay a lock and a clock read), so
	// a firing timer measures the quiet stretch and re-arms for what is left.
	timer   *time.Timer
	turn    uint64
	stopped bool
}

// boundTick derives the tick's context from ctx, bounded by budget, with its
// bound and the release the tick must call when it returns, which cancels it
// with [context.Canceled] and stops the clock.
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
// clock starts again from nothing; under an open exemption it changes nothing.
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
// when nothing is left (only a timer racing the step that stopped the clock
// can leave that). Called with mu held.
func (b *tickBound) arm(turn uint64, left time.Duration) {
	if left <= 0 {
		b.cancel(errTickOverran)
		return
	}
	b.timer = time.AfterFunc(left, func() { b.expire(turn) })
}

// expire is the timer for turn firing: it cancels the tick when the quiet
// stretch has run the whole budget, and otherwise re-arms for what is left.
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
