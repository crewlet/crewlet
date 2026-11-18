package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// republisher coalesces the re-activations provisioning passes ask for (see
// [Engine.rebuildForSealedSecrets]) into one apply per burst.
//
// One Connect seals several credentials within a second, and an apply marks
// every surface stale, which re-runs the pass: a pass that never converges
// would otherwise seal and apply as fast as an apply completes. The first
// request in a quiet period runs at once, since a person is waiting; requests
// inside the window fold into ONE trailing run, never skipped, since a skipped
// rebuild leaves a sealed credential nothing running uses. No run is on the
// caller's goroutine: see [republisher.request].
type republisher struct {
	mu sync.Mutex

	// last is when a re-activation last ran; zero means never, so a node's
	// first request runs at once.
	last time.Time

	// pending is armed while a request is owed inside the window. by is the
	// LATEST party to ask: the burst's one revision is credited to them.
	pending *time.Timer
	by      iam.Actor

	// inflight is the immediate run's zero-delay timer, so the caller does
	// not block. Separate from pending: a burst arms the trailing run while
	// this one is still going.
	inflight *time.Timer

	// stopped is set by [republisher.stop] so a timer that fires during
	// shutdown does not re-activate a revision on a node that is leaving.
	stopped bool

	// runs counts re-activations started and not yet returned; cancelRuns
	// cancels the context each was given. [republisher.stop] needs both: a
	// fired timer is already past the `stopped` check, so the cancel makes
	// its apply give up and the wait makes stop mean "nothing is writing
	// any more" rather than "nothing NEW will start".
	runs       sync.WaitGroup
	cancelRuns context.CancelFunc
	runCtx     context.Context

	// now and window are injectable so the cases can drive the clock
	// rather than sleep through it. Nil and zero take the real ones.
	now    func() time.Time
	window time.Duration
}

// republishWindow is how long one burst of seals is folded into.
//
// Fifteen seconds sits between two bounds: above one Connect's burst (one
// request per surface, Atlassian's alone three revisions in a row, plus a
// vendor pass's round trips) and below the wait at which a person watching
// the card wonders whether it worked. A constant, not config: neither bound
// varies by where the engine runs.
const republishWindow = 15 * time.Second

// begin registers a run about to start and hands it the context
// [republisher.stop] cancels, reporting false once shutdown has overtaken it.
//
// THE CALLER HOLDS r.mu: stop sets `stopped` under it before waiting, so a
// run told yes is counted before any wait begins, and a later one is told no.
func (r *republisher) begin() (context.Context, bool) {
	if r.stopped {
		return nil, false
	}
	if r.runCtx == nil {
		// Lazily, because the zero value must work: it is a field on the
		// engine, never constructed.
		r.runCtx, r.cancelRuns = context.WithCancel(context.Background())
	}
	r.runs.Add(1)
	return r.runCtx, true
}

func (r *republisher) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *republisher) every() time.Duration {
	if r.window > 0 {
		return r.window
	}
	return republishWindow
}

// request asks for a re-activation, crediting it to by.
//
// run is passed rather than captured so the type knows nothing of the engine,
// and the cases can assert the coalescing itself with a recorder.
func (r *republisher) request(by iam.Actor, run func(context.Context, iam.Actor)) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	now := r.clock()
	if elapsed := now.Sub(r.last); r.last.IsZero() || elapsed >= r.every() {
		r.last = now
		r.mu.Unlock()
		// AT ONCE, BUT NOT ON THIS GOROUTINE. The caller is
		// [refreshingSink.Flush], inside the surface lease: the minute
		// [setup.PassDeadline] leaves inside [setup.LeaseTTL] belongs to the
		// status write that records the pass, and a reload run here spends
		// it, landing that write outside the lease. The reload needs no lease
		// (it goes through the config plane's own compare-and-set). The
		// timer is the engine's, [republisher.stop] disarms it, and
		// [Engine.rebuildForSealedSecrets] bounds the run.
		r.startNow(run, by)
		return
	}
	// Inside the window: not dropped, but carried by the trailing run.
	r.by = by
	if r.pending != nil {
		r.mu.Unlock()
		return
	}
	wait := r.every() - now.Sub(r.last)
	r.pending = time.AfterFunc(wait, func() { r.fire(run) })
	r.mu.Unlock()
}

// startNow performs one run off the caller's goroutine, credited to by. Its
// own timer, not [republisher.pending], which a burst arms while this one is
// in flight.
func (r *republisher) startNow(run func(context.Context, iam.Actor), by iam.Actor) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.inflight = time.AfterFunc(0, func() {
		r.mu.Lock()
		r.inflight = nil
		ctx, ok := r.begin()
		r.mu.Unlock()
		if !ok {
			return
		}
		defer r.runs.Done()
		// Detached from the caller, whose context is not here;
		// [Engine.rebuildForSealedSecrets] sets the deadline. Bound to
		// shutdown by begin's context.
		run(ctx, by)
	})
	r.mu.Unlock()
}

// fire performs the run one or more requests inside the window asked for.
func (r *republisher) fire(run func(context.Context, iam.Actor)) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.pending = nil
	r.last = r.clock()
	by := r.by
	r.by = iam.Actor{}
	ctx, ok := r.begin()
	r.mu.Unlock()
	if !ok {
		return
	}
	defer r.runs.Done()
	// Detached from the caller, bound to shutdown by begin's context.
	run(ctx, by)
}

// stop ends every re-activation this type owns and returns once none is
// running, so a leaving node never writes a config revision (crediting an
// operator, waking every peer) while [Engine.Stop] closes the backends.
//
// Disarming covers only unfired timers: a fired one is already past the
// `stopped` check. So cancel, then WAIT: the cancel makes an apply in flight
// give up rather than sit out a full config write, and the wait lets a caller
// read the return as "nothing is writing any more". The wait is outside the
// lock, which a finishing run takes: holding it would deadlock.
func (r *republisher) stop() {
	r.mu.Lock()
	r.stopped = true
	if r.pending != nil {
		r.pending.Stop()
		r.pending = nil
	}
	if r.inflight != nil {
		r.inflight.Stop()
		r.inflight = nil
	}
	cancel := r.cancelRuns
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.runs.Wait()
}
