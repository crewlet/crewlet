package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
)

// The scheduler's wiring: the only caller of `schedule.New`, run as the
// fleet-singleton duty `engine/duty.go` names "the scheduler tick". Without it
// every schedule parses, validates and renders, and never fires.

// schedulerRuns reports whether this company should have a tick loop.
//
// It does when scheduling is on, this node publishes, it reaches the FLEET's
// claim store (the duty moves, so a node-local ledger would hand its successor
// an empty one; the node's own store holds only the dashboard's audit rows and
// is no condition), the company declares a schedule (else it claims a duty
// every tick forever) and configures a model. Re-evaluated on every apply, so
// a first schedule or first provider arms the loop without a restart.
//
// The publish check is here, not at the apply's call: a maintenance-mode node
// never reaches [Engine.startScheduler], but it does run applies, and this
// predicate is all that stands between one and an armed loop.
func (e *Engine) schedulerRuns(c *Company) bool {
	if c == nil || !c.Config.Scheduling.Runs() || !e.mode.Publishes() {
		return false
	}
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return false
	}
	// No model, no loop: each inbox holds its work until a provider
	// exists (nomodels.go), so an armed loop would stack every fire behind
	// the hold and replay the backlog when one arrived, the stale replay
	// the catchup clamp refuses. The apply adding the provider arms a
	// fresh loop that catches up only the latest missed fire.
	if c.Models == nil {
		return false
	}
	return schedule.HasSchedules(c.Org)
}

// noteSchedulesWaiting says so when a company's schedules are idle for want of
// a model: the one reason for an idle loop an operator could not otherwise see.
func (e *Engine) noteSchedulesWaiting(ctx context.Context, c *Company) {
	if c == nil || c.Models != nil || !c.Config.Scheduling.Runs() || !schedule.HasSchedules(c.Org) {
		return
	}
	log.WarnContext(ctx, "schedules_waiting_for_a_model",
		"schedules", schedule.CountSchedules(c.Org),
		"detail", "the company configures no model provider, so its schedules "+
			"do not fire; the apply that adds one under providers.llm arms them, "+
			"and a fire missed in the meantime is caught up only within the "+
			"catchup window, as after a restart")
}

// scheduleClaimer joins the fleet's at-most-once claim to this node's ledger.
func (e *Engine) scheduleClaimer() (schedule.Claimer, error) {
	var history schedule.Ledger
	if db := e.backends.Store; db != nil {
		history = sqlledger.New(db.SQL())
	}
	return schedule.NewSharedClaimer(e.backends.Fleet, history)
}

// startScheduler arms the tick loop if this company wants one.
//
// Started last, with the sandbox waiter and the retention sweep: its duty is
// claimed under the node's own incarnation.
func (e *Engine) startScheduler(ctx context.Context) {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	company := e.Company()
	e.noteSchedulesWaiting(ctx, company)
	e.armSchedulerLocked(ctx, company)
}

// armSchedulerLocked starts the loop, or logs why it did not.
func (e *Engine) armSchedulerLocked(ctx context.Context, c *Company) {
	if e.scheduler.cancel != nil || !e.schedulerRuns(c) {
		return
	}
	cfg := c.Config.Scheduling
	tick := cfg.Tick()
	claimer, err := e.scheduleClaimer()
	if err != nil {
		log.ErrorContext(ctx, "scheduler_not_started", "error", err,
			"detail", "role and unit schedules will not fire on this node")
		return
	}
	s, err := schedule.New(schedule.Options{
		// Through the node's wake queue ([Engine.wakes]): a fire to a seat
		// whose mailbox is not complete is refused, not discarded.
		Publisher: e.wakes(),
		// Confirmed before it is claimed, so the next tick retries a refused
		// fire rather than spending a claim on nothing.
		Mailboxes: e.mailboxConfirmer(),
		// A function, not the value: every apply swaps the org, and a
		// captured pointer would fire deleted seats' schedules forever.
		Org:    e.Chart,
		Ledger: claimer,
		// How long the fleet keeps fire claims: a restart judges no fire
		// older than that missed.
		ClaimRetention: coord.FireRetention,
		// A node that cannot apply the current epoch must not fire the
		// previous company's crons; with no queued copy to fall back on, a
		// shedding tick skips whole.
		Admits: e.admits,
		Duty:   e.workerDuty(schedule.DutyName, schedule.DutyTTL(tick)),
		// A paused seat's fire is recorded skipped, not sent into its held
		// inbox; every node watches the pause, since the duty moves.
		Paused: e.seatPaused,
		// Read per tick, as the org is: the loop outlives the apply that
		// armed it, and a captured zone would go stale.
		Zone:       e.Zone,
		Tick:       tick,
		Jitter:     time.Duration(cfg.JitterSeconds) * time.Second,
		CatchupMin: time.Duration(cfg.CatchupMinSeconds) * time.Second,
		CatchupMax: cfg.CatchupMax(),
	})
	if err != nil {
		// Loud but not fatal: a New error is a wiring mistake, not
		// worth taking the company down over work it can run without.
		log.ErrorContext(ctx, "scheduler_not_started", "error", err,
			"detail", "role and unit schedules will not fire on this node")
		return
	}
	// Detached from the signal context: stopScheduler ends it, so it
	// lives as long as the engine, not until the first SIGTERM.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.scheduler.cancel = cancel
	e.scheduler.done = make(chan struct{})
	done := e.scheduler.done
	go func() {
		defer close(done)
		s.Run(loopCtx)
	}()
	log.InfoContext(ctx, "scheduler_armed",
		"schedules", schedule.CountSchedules(c.Org),
		"tick_seconds", tick.Seconds())
}

// reconcileScheduler starts or stops the loop as the company changes.
//
// Adding the first schedule or removing the last takes effect on that apply.
// The tick knobs are read when the loop is armed, so a changed `tick_seconds`
// lands at the next start, as the retention sweep's horizons do.
func (e *Engine) reconcileScheduler(ctx context.Context, next *Company) {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	e.noteSchedulesWaiting(ctx, next)
	if e.schedulerRuns(next) {
		// armSchedulerLocked strips the cancellation but keeps the
		// apply's trace and values.
		e.armSchedulerLocked(ctx, next)
		return
	}
	e.disarmSchedulerLocked()
}

// disarmSchedulerLocked stops the loop and waits for the in-flight tick.
//
// Waiting matters: a tick abandoned between claiming a fire and publishing it
// leaves a ledger row that stops every peer from ever firing that run.
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
// Exported, as [Engine.Maintenance] is, to answer the operator's "are my
// schedules actually running?".
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

// ScheduleRunToolDeps is what a person runs one schedule now through on this
// node. Who asks and the chart are the surface's, set per call. The zero value
// where this node reaches no fleet claim store or no queue: the tool is not
// served there.
func ScheduleRunToolDeps(e *Engine) builtin.ScheduleRunDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return builtin.ScheduleRunDeps{}
	}
	return builtin.ScheduleRunDeps{Runner: e}
}

// RunScheduleNow fires one schedule on a person's ask, from THIS node.
//
// Not the duty holder's: the tick is a singleton only to avoid duplicated
// evaluation. The claim is the fleet's and the fire lands on the runner seat's
// inbox, so any node may claim and send an ask; its ledger row reaches the
// schedule's history through this node's usage day.
//
// Under [Engine.schedulerRuns]'s conditions, less "declares a schedule", which
// an ask naming one has answered.
func (e *Engine) RunScheduleNow(ctx context.Context, ask schedule.ManualAsk) ([]schedule.ManualRun, error) {
	c := e.Company()
	switch {
	case c == nil || !e.mode.Publishes() || e.backends == nil || e.backends.Fleet == nil ||
		e.backends.Queue == nil:
		return nil, schedule.ErrNotPublishing
	case !c.Config.Scheduling.Runs():
		return nil, schedule.ErrSchedulingOff
	case c.Models == nil:
		return nil, schedule.ErrNoModel
	case !e.admits():
		// Behind the current epoch it fires nothing, as its tick does
		// not: the schedule it would fire is the previous company's.
		return nil, schedule.ErrNotPublishing
	}
	claimer, err := e.scheduleClaimer()
	if err != nil {
		return nil, err
	}
	manual, err := schedule.NewManual(schedule.ManualOptions{
		Publisher: e.wakes(),
		Ledger:    claimer,
		Paused:    e.seatPaused,
		Mailboxes: e.mailboxConfirmer(),
	})
	if err != nil {
		return nil, err
	}
	return manual.Fire(ctx, ask)
}
