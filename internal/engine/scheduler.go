package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
)

// The scheduler's wiring, and the reason this file exists at all.
//
// `internal/schedule` shipped complete — the cron grammar, the at-most-once
// claim ledger, the missed-tick catchup, the fleet-singleton duty, its own
// contract suite over two ledger implementations — and NOTHING EVER BUILT
// ONE. `schedule.New` had no caller outside its own tests, so every role and
// unit schedule a founder wrote was parsed, validated, rendered in the
// dashboard and never fired. The subsystem was not broken; it was
// unreachable, which is the failure mode that survives a green test suite.
//
// Everything the wiring needs already existed: `engine/duty.go` names "the
// scheduler tick" as one of the fleet singletons, `startMaintenance` already
// builds the ledger for its retention sweep, and `internal/config`'s
// Scheduling block already carried the knobs. Only the constructor was
// missing.

// schedulerRuns reports whether this company should have a tick loop.
//
// The three conditions the config block has always documented: the operator
// has not switched it off, this node has a store to hold the claim ledger,
// and the company actually declares a schedule. The last one is what keeps a
// company with no schedules from claiming a fleet duty every ten seconds
// forever, and it is re-evaluated on every config apply so a founder's FIRST
// schedule starts the loop without a restart.
func (e *Engine) schedulerRuns(c *Company) bool {
	if c == nil || !c.Config.Scheduling.Runs() {
		return false
	}
	if e.backends == nil || e.backends.Store == nil || e.backends.Queue == nil {
		return false
	}
	return schedule.HasSchedules(c.Org)
}

// startScheduler arms the tick loop if this company wants one.
//
// Started LAST, with the sandbox waiter and the retention sweep, because its
// duty is claimed under the node's own incarnation.
func (e *Engine) startScheduler(ctx context.Context) {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	e.armSchedulerLocked(ctx, e.Company())
}

// armSchedulerLocked starts the loop, or logs why it did not.
func (e *Engine) armSchedulerLocked(ctx context.Context, c *Company) {
	if e.scheduler.cancel != nil || !e.schedulerRuns(c) {
		return
	}
	cfg := c.Config.Scheduling
	tick := cfg.Tick()
	s, err := schedule.New(schedule.Options{
		Publisher: e.backends.Queue,
		// A FUNCTION, not the value: the org is swapped wholesale on every
		// apply, so a captured pointer would pin the scheduler to the
		// company that existed when it was built and fire deleted seats'
		// schedules for the life of the process.
		Org:    func() *org.Organization { return e.Company().Org },
		Ledger: sqlledger.New(e.backends.Store.SQL()),
		// The node's own config posture. A node that cannot apply the
		// current epoch must not fire the previous company's crons — and
		// unlike a delivery there is no queued copy to fall back on, so a
		// shedding tick skips whole rather than firing something stale.
		Admits:          e.admits,
		Duty:            e.workerDuty(schedule.DutyName, schedule.DutyTTL(tick)),
		DefaultTimezone: cfg.DefaultTimezone,
		Tick:            tick,
		Jitter:          time.Duration(cfg.JitterSeconds) * time.Second,
		CatchupMin:      time.Duration(cfg.CatchupMinSeconds) * time.Second,
		CatchupMax:      cfg.CatchupMax(),
	})
	if err != nil {
		// Not fatal, and said loudly. Every New error names a wiring
		// mistake rather than an operator one, so failing the whole engine
		// would take a company down over work it can run without — but a
		// silent nil here is precisely how this subsystem went missing.
		log.Error("scheduler_not_started", "error", err,
			"detail", "role and unit schedules will not fire on this node")
		return
	}
	// Detached from the signal context for the same reason the node's own
	// loops are: stopScheduler is what ends it, so its lifetime matches the
	// engine's rather than the first SIGTERM's.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.scheduler.cancel = cancel
	e.scheduler.done = make(chan struct{})
	done := e.scheduler.done
	go func() {
		defer close(done)
		s.Run(loopCtx)
	}()
	log.Info("scheduler_armed",
		"schedules", schedule.CountSchedules(c.Org),
		"tick_seconds", tick.Seconds())
}

// reconcileScheduler starts or stops the loop as the company changes.
//
// A founder who adds the first schedule to a live company gets a firing
// scheduler on that apply, and one who removes the last gets the duty claim
// released — neither needs a restart. The tick KNOBS are read when the loop
// is armed, so changing `tick_seconds` on a running scheduler lands at the
// next start, exactly as the retention sweep's horizons do.
func (e *Engine) reconcileScheduler(ctx context.Context, next *Company) {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if e.schedulerRuns(next) {
		// The apply's own context, which armSchedulerLocked strips the
		// cancellation from: the loop must outlive the request that armed
		// it, but it should still inherit that request's trace and values.
		e.armSchedulerLocked(ctx, next)
		return
	}
	e.disarmSchedulerLocked()
}

// disarmSchedulerLocked stops the loop and waits for the in-flight tick.
//
// Waiting matters: a tick that has claimed a fire and not yet published it
// would otherwise be abandoned mid-dispatch, and the ledger row it already
// wrote means no peer will ever fire that run.
func (e *Engine) disarmSchedulerLocked() {
	if e.scheduler.cancel == nil {
		return
	}
	e.scheduler.cancel()
	<-e.scheduler.done
	e.scheduler.cancel, e.scheduler.done = nil, nil
	log.Info("scheduler_disarmed")
}

// stopScheduler ends the tick loop.
func (e *Engine) stopScheduler() {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	e.disarmSchedulerLocked()
}

// SchedulerRunning reports whether this node is ticking schedules.
//
// Exported for the same reason [Engine.Maintenance] is: "are my schedules
// actually running?" is an operator question, and the whole reason this
// subsystem sat dead was that nothing anywhere could answer it.
func (e *Engine) SchedulerRunning() bool {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	return e.scheduler.cancel != nil
}

// schedulerLoop is the running tick loop's handle.
type schedulerLoop struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}
