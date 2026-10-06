package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// DefaultPollInterval is how often the waiter reconnects to each running box.
//
// 15s bounds the completion-detection latency — negligible against coding jobs
// that run minutes — while a tick costs only one reconnect plus a marker probe
// per running box. It also keeps the keepalive ~60x inside the box TTL
// ([DefaultBoxTimeout]). The give-up window for an unreachable box does NOT
// derive from it — see [ConnectGiveUp].
const DefaultPollInterval = 15 * time.Second

// ConnectGiveUp is how long a box may stay unreachable before the waiter gives
// up on it and fires completion anyway.
//
// The box is unreachable, so the run can never produce a result. The engine
// keeps a running box alive on every tick, so this never fires merely because
// a run is long — it means a genuine infra failure: the provider reclaimed the
// box, a network partition, or the engine was down long enough that the
// keepalive lapsed and the orphan was reaped. Firing lets the coordinator free
// the seat and mark the run failed instead of polling a dead box forever.
//
// A DURATION, NOT A TICK COUNT, and that is the fix rather than the style. It
// was four consecutive failures, with the rationale written as "four ticks
// ≈ 1 minute" — true only at [DefaultPollInterval]. The count is applied to
// whatever interval the waiter was built with, so a deployment that polls
// faster shrank the window with it: at the 100 ms cadence the e2e suite drives
// this code at, four failures is 0.4 s, and a box that is briefly slow rather
// than gone is declared dead. What follows is not a retry — the completion
// fires, `collect` cannot reconnect either, and the coordinator settles the
// run FAILED and tears the turn down. Giving up is terminal, so the window has
// to be measured in the unit its own reasoning uses.
const ConnectGiveUp = 60 * time.Second

// MinConnectFailures is how many consecutive failures must have happened
// before [ConnectGiveUp] can fire at all.
//
// The duration alone is not enough at a slow cadence: at a poll interval above
// the give-up window, the FIRST failure is already older than it. Two attempts
// is the smallest number that distinguishes "unreachable" from "one bad
// probe", and it is what makes the rule read the same at every cadence — a box
// is given up on after a minute AND at least one confirmation.
const MinConnectFailures = 2

// MaxConcurrentPolls is how many boxes the waiter works on at once — a poll,
// or the reclaim of a pause past its TTL — across every pass in flight.
//
// CONCURRENT, because one box must never hold up another's keepalive. The
// tick polled its boxes one after another, so a box whose envd accepted a
// request and then said nothing held every box behind it for as long as that
// request's own bound — ten minutes of silence for a command — with their
// keepalives queued toward a box TTL of fifteen; two such boxes and the rest
// of the company's were reaped while their jobs were working.
//
// SIXTEEN, half the shared transport's warm connections per host
// ([httpx.MaxIdleConnsPerHost], 32): every remote box's control-plane calls
// go to one API host, and the launches and collections running beside the
// poll need the other half. At the default cadence that is a poll of a few
// hundred running boxes inside one interval.
//
// WHAT A STUCK BOX COSTS ITS NEIGHBOURS IS A SLOT, never a tick. The next pass
// starts on schedule whatever is still running ([Waiter.pass]) and hands the
// free slots to the boxes whose last poll ended longest ago, so a box that sat
// at its bound goes to the back of the order after its turn. With N boxes
// stuck at the bound a healthy box waits at most ceil(N/16) bounds for a slot
// — half a minute for every sixteen at the default cadence ([Waiter.deadline])
// — against a fifteen-minute TTL.
const MaxConcurrentPolls = 16

// pollBound is the most one box's poll — reaching it, keeping it alive and
// asking its runner — may take before it is abandoned, wherever the duty that
// authorised it does not end sooner ([Waiter.deadline]).
//
// A healthy poll is three or four requests that answer in a second. Sixty
// seconds is the control plane's whole-request budget ([E2BClientTimeout]),
// the engine's standing answer to how long a box's provider may take to
// answer at all, and a poll that has not finished in one such budget is a box
// that is not answering: what it costs is that box's poll, counted against
// reaching it where it was reaching it, and it is asked again by a later
// pass. Without it a poll lasted as long as its box's slowest request.
const pollBound = E2BClientTimeout

// Publisher is the slice of the queue the waiter needs.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// DutyFunc claims the single-owner waiter duty for one tick.
//
// The waiter polls EVERY active run in the company, not just this node's
// seats, because a box can be polled from anywhere — so N nodes running it
// means N reconnects per box per tick and N racing reapers. Nil means "no
// fleet", which is the single-node case.
type DutyFunc func(ctx context.Context) (bool, error)

// WaiterOptions configures a [Waiter].
type WaiterOptions struct {
	Queue   Publisher
	Pending PendingStore

	// Manager is the manager CURRENT at each tick — the coordinator's own
	// [Coordinator.Manager] in the engine.
	//
	// A SOURCE RATHER THAN A VALUE, because the manager is swapped on every
	// apply that changes providers.sandbox and the poll runs for the life
	// of the process. It was a value captured once, so after a reload the
	// poll went on reconnecting, keeping alive and reaping through the
	// backends of the catalogue the node booted with — a rotated
	// credential it no longer resolved, a keepalive to the old TTL, a cell
	// the company had reshaped — while every launch used the new ones.
	Manager func() *Manager

	// Interval is the poll cadence. Zero takes [DefaultPollInterval].
	Interval time.Duration

	// ClaimDuty gates the tick in a fleet. Nil means single-node.
	ClaimDuty DutyFunc

	// DutyTTL is how long one successful [WaiterOptions.ClaimDuty] holds
	// the duty without another — the lease the claim takes. REQUIRED with a
	// claim, and longer than the interval: a box's poll outlives the pass
	// that started it, and this is what stops it outliving the duty that
	// authorised it ([Waiter.deadline]), past which a peer may hold the
	// duty and poll the same box.
	DutyTTL time.Duration

	// Now is the clock, injectable so a test can expire a pause without
	// waiting half an hour.
	Now func() time.Time
}

// Waiter is the completion poll and pause reaper for detached sandbox jobs.
//
// THIS POLL IS THE COMPLETION SIGNAL. A periodic tick reconnects to each still-
// running box by id and asks the runner whether its background command has
// finished — covering a clean finish, a finished-but-never-exited agent, a
// crashed process, and a box that vanished, so a detached run never hangs
// forever. There is deliberately no push callback from inside the box: only the
// poll can see a job that died before reaching its last step, and the tick
// doubles as the box keepalive, so it must run at this cadence regardless. A
// push signal could only shave less than one interval off jobs that run for
// minutes.
//
// On detected completion it publishes SandboxRunCompleted; the coordinator does
// the at-most-once claim, the collection, and the resume of the suspended
// Execute loop. A duplicate signal is harmless — successive ticks can both fire
// before the first claim lands, and queue delivery is at-least-once, but the
// coordinator claims once.
//
// The same tick is also the PAUSE REAPER. A run blocked on a person's answer
// parks its box paused so a quick reply resumes exactly where the coding agent
// stopped — but a paused box has no provider-side TTL, and the keepalive
// deliberately does not touch it. Left alone, one unanswered question strands a
// box forever.
type Waiter struct {
	queue   Publisher
	pending PendingStore
	manager func() *Manager

	interval  time.Duration
	claimDuty DutyFunc
	dutyTTL   time.Duration
	now       func() time.Time

	// pollBound is [pollBound], held on the value so a test can watch a box
	// at its bound without waiting a minute for it.
	pollBound time.Duration

	// slots is [MaxConcurrentPolls], shared by every pass in flight: a
	// pass's tasks outlive it, so a per-pass count would let passes stack
	// past the cap behind boxes that do not answer.
	slots chan struct{}
	// tasks is every walk and every box's task this waiter started, which
	// [Waiter.Stop] waits for.
	tasks sync.WaitGroup

	mu sync.Mutex
	// failures tracks the CONSECUTIVE reconnect failures per turn, cleared
	// on any success. Per-turn rather than per-box because a box id can be
	// cleared and re-minted on a reseed while the run continues.
	failures map[string]connectStreak
	// busy is every turn whose box a task is working on right now, with
	// what stops that task: a box is worked on by one task at a time
	// whoever runs the pass, and a duty this node has definitively lost
	// stops every one of them ([Waiter.standDown]).
	busy map[string]context.CancelFunc
	// settled orders a walk: each turn's [Waiter.seq] when its last task
	// ended, absent for one no task has worked on yet. Oldest first, so the
	// box a stuck poll held a slot from is not the next to take one.
	settled map[string]uint64
	seq     uint64
	// walk stops the walk still handing out slots; the next pass replaces
	// it with one over a fresher listing.
	walk context.CancelFunc

	startOnce sync.Once
	stopOnce  sync.Once
	stop      context.CancelFunc
	stopped   chan struct{}
}

// NewWaiter validates the options and returns the waiter.
func NewWaiter(opts WaiterOptions) (*Waiter, error) {
	if opts.Queue == nil || opts.Pending == nil || opts.Manager == nil {
		return nil, errors.New("sandbox: a waiter needs a queue, a pending store and a manager")
	}
	w := &Waiter{
		queue:     opts.Queue,
		pending:   opts.Pending,
		manager:   opts.Manager,
		interval:  opts.Interval,
		claimDuty: opts.ClaimDuty,
		dutyTTL:   opts.DutyTTL,
		now:       opts.Now,
		pollBound: pollBound,
		slots:     make(chan struct{}, MaxConcurrentPolls),
		failures:  map[string]connectStreak{},
		busy:      map[string]context.CancelFunc{},
		settled:   map[string]uint64{},
		stopped:   make(chan struct{}),
	}
	if w.interval <= 0 {
		w.interval = DefaultPollInterval
	}
	if w.claimDuty != nil && w.dutyTTL <= w.interval {
		return nil, fmt.Errorf("sandbox: a waiter that claims a duty needs that duty's TTL, "+
			"longer than its %v interval (WaiterOptions.DutyTTL is %v): a poll outlives "+
			"the pass that started it and must not outlive the duty", w.interval, w.dutyTTL)
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w, nil
}

// Start runs the poll loop until Stop or the context ends.
func (w *Waiter) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		ctx, w.stop = context.WithCancel(ctx)
		go w.loop(ctx)
		log.InfoContext(ctx, "sandbox_waiter_started", "poll_seconds", w.interval.Seconds())
	})
}

// Stop ends the poll loop and waits for every task it started — each box's
// poll, and each reclaim of an expired pause — to end. A poll stops at once; a
// reclaim that has already won its flip finishes its kill first, within
// [discardGrace], because nothing else names that box any more.
func (w *Waiter) Stop() {
	w.stopOnce.Do(func() {
		if w.stop != nil {
			w.stop()
			<-w.stopped
		}
		log.Info("sandbox_waiter_stopped")
	})
}

func (w *Waiter) loop(ctx context.Context) {
	defer close(w.stopped)
	// Every task a pass started derives from ctx, so the cancellation that
	// ended the loop ends them too; this waits for them to say so.
	defer w.tasks.Wait()
	// Jittered, so a fleet's waiters do not all wake on the same second and
	// contend for the duty claim in lockstep.
	timer := time.NewTimer(w.jittered())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// NOT WAITED FOR: the pass returns once its walk has started, so
		// the next one — and with it the duty's renewal and every other
		// box's poll — comes on schedule however long one box takes.
		if _, err := w.pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.WarnContext(ctx, "sandbox_waiter_tick_failed", "error", err.Error())
		}
		timer.Reset(w.jittered())
	}
}

// jitterFraction spreads wake-ups across ±20% of the interval, the same spread
// the config reconcile loop uses for the same reason.
const jitterFraction = 0.2

func (w *Waiter) jittered() time.Duration {
	return backoff.Jitter(w.interval, jitterFraction)
}

// Tick runs one pass TO ITS END — every running run polled and every expired
// pause reclaimed, as [Waiter.pass] hands them out — and returns how many
// completions it announced.
//
// The loop runs the same pass and does not wait for it, which is the point of
// the pass: see there. Exported so a test drives one pass deterministically
// and reads what it did.
func (w *Waiter) Tick(ctx context.Context) (int, error) {
	r, err := w.pass(ctx)
	if err != nil || r == nil {
		return 0, err
	}
	r.tasks.Wait()
	return int(r.fired.Load()), nil
}

// round is what one pass started, for a caller that waits for it.
type round struct {
	fired atomic.Int64
	// tasks is the pass's walk and every task the walk started.
	tasks sync.WaitGroup
}

// task is one run's work in a walk: a poll of its box, or the reclaim of the
// paused box its expired pause holds.
type task struct {
	run  PendingRun
	reap bool
}

// pass claims the duty, lists the runs, and starts a WALK that hands every
// run needing work to a task of its own; it returns as soon as the walk has
// started. Nil with no error is a pass this node had no duty to run.
//
// A PASS DOES NOT WAIT FOR ITS TASKS. Waiting for its slowest box would tie
// every box's next poll — and the duty's renewal — to that box: one box held
// at its bound would stretch the cadence of every other box from the interval
// to the bound, and the pass past the duty's own TTL, so the duty would lapse
// mid-pass and a peer poll the same boxes beside it. A box whose task is still
// running is passed over by the next walk instead ([Waiter.begin]), and every
// other box is polled on schedule.
//
// WHAT A TASK MAY DO IS BOUNDED BY WHAT AUTHORISED IT: its deadline is the
// duty claim this pass made, not the pass ([Waiter.deadline]), so no task
// outlives the duty — and a duty definitively lost stops them all at the next
// pass ([Waiter.standDown]).
//
// ONE WALK AT A TIME. A walk still waiting for a free slot when the next pass
// starts is stopped and replaced by one over the fresher listing; the tasks it
// already started run on. A walk hands slots out to the runs whose last task
// ended longest ago, so the runs the stopped walk never reached are the first
// the new one serves.
func (w *Waiter) pass(ctx context.Context) (*round, error) {
	// THE INSTANT THE DUTY IS ASKED FOR, taken before the claim: the lease
	// the claim takes runs from when the store applies it, no earlier than
	// this, so a deadline measured from here is never past the lease's own.
	asked := time.Now()
	if !w.mayTick(ctx) {
		return nil, nil
	}
	deadline := w.deadline(asked)
	// ONE MANAGER FOR THE WHOLE PASS, read once: a reload landing mid-pass
	// must not poll half the runs through one catalogue and reap the rest
	// through another.
	manager := w.manager()
	if manager == nil {
		return nil, errors.New("sandbox: the waiter's manager source answered no manager")
	}
	runs, err := w.pending.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	w.forget(runs)
	work := w.order(runs)

	walkCtx, stopWalk := context.WithCancel(ctx)
	if !deadline.IsZero() {
		walkCtx, stopWalk = context.WithDeadline(ctx, deadline)
	}
	w.replaceWalk(stopWalk)
	r := &round{}
	r.tasks.Add(1)
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		defer r.tasks.Done()
		defer stopWalk()
		w.walkTasks(ctx, walkCtx, deadline, manager, work, r)
	}()
	return r, nil
}

// walkTasks starts each task in order, as slots free up, until it runs out of
// tasks or its pass's authority ends.
//
// A task's context derives from the PASS, not from the walk: a walk replaced
// by the next pass stops handing out slots, and the tasks it already started
// are still authorised by the claim they were started under.
func (w *Waiter) walkTasks(ctx, walkCtx context.Context, deadline time.Time, manager *Manager, work []task, r *round) {
	for _, t := range work {
		if walkCtx.Err() != nil {
			return
		}
		if w.isBusy(t.run.TurnID) {
			// Its last task is still working on it — a box at its bound,
			// or a pass another caller is running. One at a time.
			continue
		}
		if !w.acquireSlot(walkCtx) {
			return
		}
		taskCtx, stop := context.WithDeadline(ctx, w.taskDeadline(deadline))
		if !w.begin(walkCtx, t.run.TurnID, stop) {
			stop()
			<-w.slots
			continue
		}
		r.tasks.Add(1)
		w.tasks.Add(1)
		go func() {
			defer w.tasks.Done()
			defer r.tasks.Done()
			defer func() { <-w.slots }()
			defer w.end(t.run.TurnID)
			defer stop()
			if t.reap {
				w.reapOne(taskCtx, manager, t.run)
				return
			}
			if w.pollBounded(ctx, taskCtx, manager, t.run) {
				r.fired.Add(1)
			}
		}()
	}
}

// acquireSlot takes one of [MaxConcurrentPolls] for the walk, waiting for one
// to free up, and reports false — holding none — once the walk's authority has
// ended.
//
// ASKED AGAIN AFTER THE SLOT IS TAKEN: a slot freeing up as the walk is
// replaced or its duty runs out leaves both cases of the wait ready, and Go
// picks between them at random — so half the time the walk would start a task
// it no longer has the authority for, one whose deadline may already be past,
// which then counts a box it never asked as one that did not answer.
func (w *Waiter) acquireSlot(walkCtx context.Context) bool {
	select {
	case w.slots <- struct{}{}:
	case <-walkCtx.Done():
		return false
	}
	if walkCtx.Err() != nil {
		<-w.slots
		return false
	}
	return true
}

// order is the runs needing a task, the one whose last task ended longest ago
// first and the listing's own order between equals.
func (w *Waiter) order(runs []PendingRun) []task {
	now := w.now()
	var work []task
	for _, run := range runs {
		switch {
		case run.Status == StatusRunning && run.SandboxID != "":
			work = append(work, task{run: run})
		case pauseExpired(run, now):
			work = append(work, task{run: run, reap: true})
		}
		// Running is the ONLY pollable state, and the one it most obviously
		// excludes is [StatusLaunching]: that run's job is already
		// executing, but the turn that started it has not yet written the
		// conversation a resume re-enters. Firing there hands the
		// coordinator a claim it cannot resume, and the coordinator's only
		// honest answer to that is to fail the run — so a coding job that
		// finished inside the window destroyed the turn. The next pass finds
		// it suspended.
		//
		// A running row with no box yet is passed over too: a launch writes
		// the row first, so a crash in that window leaves a record rather
		// than a box nothing names. A poll there has nothing to connect to
		// and nothing to keep alive, and asking the provider for "" is how a
		// nameless box comes to be created. The next pass finds it attached.
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	slices.SortStableFunc(work, func(a, b task) int {
		return cmp.Compare(w.settled[a.run.TurnID], w.settled[b.run.TurnID])
	})
	return work
}

// deadline is how long the duty a claim made at asked authorises work for: the
// claim's TTL, less one interval for the margin between this node's clock and
// the one the store measures the lease on. Zero without a duty, where nothing
// but [pollBound] bounds a task.
//
// At the default cadence that is 30 s — a duty of three intervals, less one —
// and a peer can hold the duty only once it has lapsed, so a task ended by
// this never overlaps one of the peer's on the same box.
func (w *Waiter) deadline(asked time.Time) time.Time {
	if w.claimDuty == nil {
		return time.Time{}
	}
	return asked.Add(w.dutyTTL - w.interval)
}

// taskDeadline is one task's deadline: [pollBound] from its start, or the
// duty's own end where that comes first.
func (w *Waiter) taskDeadline(duty time.Time) time.Time {
	bound := time.Now().Add(w.pollBound)
	if !duty.IsZero() && duty.Before(bound) {
		return duty
	}
	return bound
}

// pollBounded polls one box within its task's context and announces its
// completion where the poll found one, reporting whether it did.
//
// THE ANNOUNCEMENT IS NOT UNDER THE BOUND: the bound is on reaching the box,
// and a completion found a second before it ran out is still a completion. A
// duplicate announcement is harmless — the coordinator claims once — so it
// takes the pass's context, not the task's.
func (w *Waiter) pollBounded(ctx, taskCtx context.Context, manager *Manager, run PendingRun) bool {
	switch w.pollOne(taskCtx, manager, run) {
	case pollDone, pollGone:
		// gone → the box vanished; fire anyway so the coordinator frees
		// the seat and marks the run failed (collect will fail to
		// reconnect) instead of hanging.
		if err := w.publishCompletion(ctx, run); err != nil {
			log.WarnContext(ctx, "sandbox_completion_publish_failed",
				"turn_id", run.TurnID, "error", err.Error())
			return false
		}
		log.InfoContext(ctx, "sandbox_waiter_fired", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "sandbox_id", run.SandboxID)
		return true
	}
	return false
}

// isBusy reports whether a task is working on the turn's box right now.
func (w *Waiter) isBusy(turnID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, busy := w.busy[turnID]
	return busy
}

// begin claims the turn's box for one task, reporting false where a task is
// already working on it or the walk starting it has lost its authority; stop
// is what ends the task early.
//
// THE WALK IS ASKED UNDER THE LOCK [Waiter.standDown] stops everything under,
// so a task is either registered before a stand-down, and stopped by it, or
// refused after it — never started in between, where nothing would stop it
// while a peer holds the duty.
func (w *Waiter) begin(walkCtx context.Context, turnID string, stop context.CancelFunc) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if walkCtx.Err() != nil {
		return false
	}
	if _, busy := w.busy[turnID]; busy {
		return false
	}
	w.busy[turnID] = stop
	return true
}

// end releases the claim [Waiter.begin] took and dates it for the next walk's
// order.
func (w *Waiter) end(turnID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.busy, turnID)
	w.seq++
	w.settled[turnID] = w.seq
}

// replaceWalk stops the walk a previous pass left handing out slots and
// records the new one's stop.
func (w *Waiter) replaceWalk(stop context.CancelFunc) {
	w.mu.Lock()
	previous := w.walk
	w.walk = stop
	w.mu.Unlock()
	if previous != nil {
		previous()
	}
}

// standDown stops the walk and every task in flight: this node has
// definitively lost the duty, so a peer may hold it and work the same boxes.
//
// UNDER THE LOCK, so a walk between taking a slot and registering its task
// cannot slip a task past it ([Waiter.begin]). A cancel only closes channels,
// so nothing it runs comes back for the lock.
func (w *Waiter) standDown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.walk != nil {
		w.walk()
	}
	for _, stop := range w.busy {
		stop()
	}
}

// mayTick reports whether this node holds the waiter duty this pass.
//
// Re-claimed every pass rather than held: the duty lease is short, so a node
// that dies mid-poll releases it by lapsing and a peer takes over on its next
// pass, with no handoff protocol.
func (w *Waiter) mayTick(ctx context.Context) bool {
	if w.claimDuty == nil {
		return true
	}
	holds, err := w.claimDuty(ctx)
	if err != nil {
		log.WarnContext(ctx, "sandbox_waiter_duty_claim_failed", "error", err.Error())
		// FAIL CLOSED. Not knowing whether this node holds the duty and
		// polling anyway is the multi-poller case the duty exists to
		// prevent — and skipping a pass costs one interval, which the next
		// pass recovers. What is already running carries on: it ends by
		// the deadline of the claim that authorised it, which is still
		// this node's whatever the store could not say just now.
		return false
	}
	if !holds {
		// DEFINITIVELY NOT HELD: a peer holds the duty, or will on its next
		// claim, and works these boxes — so this node's tasks stop now
		// rather than at their deadline.
		w.standDown()
	}
	return holds
}

// connectStreak is one turn's run of consecutive failed reconnects.
//
// BOTH HALVES, because the give-up rule is both: since is what the duration is
// measured from, and attempts is what stops a single probe against a slow
// cadence from being the whole streak.
type connectStreak struct {
	since    time.Time
	attempts int
}

// forget drops failure counters and walk order for runs that are no longer
// active, so a reused turn id never inherits a dead run's streak or place.
func (w *Waiter) forget(runs []PendingRun) {
	active := make(map[string]bool, len(runs))
	for _, run := range runs {
		active[run.TurnID] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.failures {
		if !active[id] {
			delete(w.failures, id)
		}
	}
	for id := range w.settled {
		if !active[id] {
			delete(w.settled, id)
		}
	}
}

type pollState int

const (
	pollRunning pollState = iota // not yet, or a transient error — retry next pass
	pollDone                     // the job finished
	pollGone                     // the box has vanished and can never complete
)

// pollOne reconnects and asks the runner whether the job has finished.
func (w *Waiter) pollOne(ctx context.Context, manager *Manager, run PendingRun) pollState {
	provider, err := manager.Provider(Placement(run.Placement))
	if err != nil {
		// A row naming a cell this build or this company does not have can
		// never complete, and retrying it every tick would keep the seat
		// busy forever. Reported as gone, which settles the run and frees
		// the seat — the operator sees one failure naming the placement
		// rather than a run that is silently stuck.
		log.ErrorContext(ctx, "sandbox_poll_no_backend",
			"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
		return pollGone
	}
	box, err := w.reach(ctx, provider, run)
	if box == nil && err == nil {
		// The run moved on between the listing and the read, and its box
		// was paused by whatever moved it — or its record could not be
		// read to say: nothing to keep alive, nothing to poll this tick,
		// and not a failure to reach the box.
		return pollRunning
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// STOPPED, not unanswered: the duty moved to a peer or the
			// waiter is stopping, and neither says anything about whether
			// this box can be reached. A task ended at its DEADLINE is
			// counted below — a box that did not answer inside the bound.
			return pollRunning
		}
		streak, giveUp := w.fail(run.TurnID)
		log.WarnContext(ctx, "sandbox_connect_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"attempts", streak.attempts,
			"unreachable_for_s", w.now().Sub(streak.since).Seconds(),
			"error", err.Error())
		if giveUp {
			return pollGone
		}
		return pollRunning
	}
	w.succeed(run.TurnID)

	// KEEPALIVE. The engine imposes NO run-time TTL on a coding job, so
	// refresh the box's kill timer every tick to keep a running job alive for
	// as long as it needs. The box is bounded only by how long the engine can
	// go WITHOUT this heartbeat, never by a fixed run deadline: completion is
	// detected by tracking the job, not by a clock.
	if err = box.SetTimeout(ctx, manager.BoxTimeout().Seconds()); err != nil {
		log.DebugContext(ctx, "sandbox_keepalive_failed", "turn_id", run.TurnID, "error", err.Error())
	}

	runner, err := manager.RunnerFor(run.CodingAgent)
	if err != nil {
		// A misconfigured runner cannot be polled, and retrying forever
		// would hold the seat busy for the life of the deployment.
		log.ErrorContext(ctx, "sandbox_poll_runner_missing",
			"turn_id", run.TurnID, "coding_agent", run.CodingAgent, "error", err.Error())
		return pollGone
	}
	done, err := runner.Poll(ctx, box, run.Handle())
	if err != nil {
		log.WarnContext(ctx, "sandbox_poll_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "error", err.Error())
		return pollRunning
	}
	if done {
		return pollDone
	}
	return pollRunning
}

// reach is a running run's box, for the poll: attached WITHOUT resuming it
// ([Provider.Attach]), and resumed only where the run's own record says a
// paused box is the running run's.
//
// THE POLL'S RECORD IS A MOMENT OLD. The collection claims a run off running,
// reads its box and pauses it, and a poll that listed the run before the
// claim and reconnected after the pause used to RESUME it — through Connect,
// which wakes whatever it reaches. Nothing touched the box again, since the
// poll keeps alive only running records, so on E2B the woken box ran out its
// timer and the parked run lost the snapshot it had been paused to keep.
//
// A PAUSED BOX UNDER A RECORD THAT IS STILL RUNNING is the one the poll must
// wake: a collection that read and paused the box, then could not resume the
// turn, hands the claim back to running — and that box is the run's again,
// to be kept alive and collected anew. So a paused box sends the poll to the
// record, and only a record still running the same job, in the same box, has
// it resumed. Anything else answers no box and no error: the run moved on.
func (w *Waiter) reach(ctx context.Context, provider Provider, run PendingRun) (Sandbox, error) {
	box, err := provider.Attach(ctx, run.SandboxID)
	if !errors.Is(err, ErrBoxPaused) {
		return box, err
	}
	current, ok, err := w.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		// The STORE did not answer, which says nothing about the box: not
		// counted against reaching it, and the next tick asks again.
		log.WarnContext(ctx, "sandbox_poll_record_unread", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "error", err.Error())
		return nil, nil
	case !ok || current.LaunchID != run.LaunchID || current.Status != StatusRunning ||
		current.SandboxID != run.SandboxID:
		log.DebugContext(ctx, "sandbox_poll_left_paused", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "detail", "the box is paused and its run is no "+
				"longer running it, so the poll leaves it as it is")
		return nil, nil
	}
	return provider.Connect(ctx, run.SandboxID)
}

// fail records one more failed reconnect and reports whether the streak has
// run long enough to give up on.
func (w *Waiter) fail(turnID string) (connectStreak, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	streak, seen := w.failures[turnID]
	if !seen {
		streak.since = w.now()
	}
	streak.attempts++
	w.failures[turnID] = streak
	spent := w.now().Sub(streak.since)
	return streak, streak.attempts >= MinConnectFailures && spent >= ConnectGiveUp
}

func (w *Waiter) succeed(turnID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.failures, turnID)
}

// pauseExpired reports whether a run's paused box is past its pause TTL and
// is the reaper's to reclaim.
//
// Scoped to runs parked on a clarification: that is the one state whose box is
// deliberately held for an open-ended human wait, so it is the one that needs
// an engine-side expiry. Every other paused box belongs to a tail that is
// actively being driven — a completion being collected, an Execute loop
// resuming — and is settled by that tail within the turn. Expiring those from
// here would kill a box out from under live work.
//
// WHETHER A BOX IS HELD IS THE ROW'S ANSWER, NOT A STAMP'S. A parked row
// naming a box describes a box being paid for whether or not the pause
// instant was ever written, so the reading is [PendingRun.HeldSince] — see
// there for why the deadline can be dated from the park itself. Skipping an
// unstamped row instead made a run whose pause record failed — one warn-only
// store write — a remote box billed until a person noticed.
func pauseExpired(run PendingRun, now time.Time) bool {
	if run.Status != StatusAwaiting {
		return false
	}
	heldSince, held := run.HeldSince()
	if !held {
		return false
	}
	// A zero TTL is not a deadline to enforce here: it means "never hold a
	// blocked box", so the coordinator already tore this one down when the
	// run blocked and there is no snapshot left to expire.
	if run.PauseTTLSeconds <= 0 {
		return false
	}
	return now.Sub(heldSince) >= time.Duration(run.PauseTTLSeconds*float64(time.Second))
}

// reapOne reclaims one run's paused box past its TTL ([pauseExpired]),
// reporting whether it did.
//
// A TASK OF ITS OWN, like a poll, so a reclaim that takes its time — a kill
// is a control-plane call — holds up no other box.
func (w *Waiter) reapOne(ctx context.Context, manager *Manager, run PendingRun) bool {
	// CLAIM FIRST, DESTROY SECOND. The reaper decides from a snapshot taken
	// seconds ago, and the clarification answer that un-pauses the run may
	// have arrived since: the claim has already flipped the row to resumed
	// and the Execute loop is reconnecting to this very box. Killing before
	// the compare-and-set destroyed it underneath that resume — and then the
	// CAS refused, so the reaper walked away silently and the answered run
	// failed on a box that no longer existed. The CAS is the authority for
	// the whole reap, not just for the status write.
	won, err := w.pending.ExpirePause(ctx, run.TurnID)
	if err != nil || !won {
		return false
	}
	// The box record was cleared by the flip itself, so an answer claiming
	// this run can no longer be told to continue in a checkout that is about
	// to be destroyed. The id survives only here, in the snapshot the reaper
	// is acting on.
	sandboxID := run.SandboxID
	// THE KILL OUTLIVES THE TASK'S BOUND. The flip has already released the
	// row and nothing else names this box now, so a kill abandoned when the
	// task's deadline or a stop cancelled it would leave a paused snapshot
	// billed with nobody left to reclaim it — a teardown takes a context of
	// its own for exactly that.
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	// Kill by id: Connect would auto-resume the snapshot, booting the box
	// back up purely to shut it down.
	provider, err := manager.Provider(Placement(run.Placement))
	if err != nil {
		log.WarnContext(ctx, "sandbox_pause_reap_no_backend",
			"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
	} else if err := provider.Kill(killCtx, sandboxID); err != nil {
		// An already-gone box is the normal case for an old snapshot; the
		// row is released either way.
		log.WarnContext(ctx, "sandbox_pause_reap_kill_failed",
			"turn_id", run.TurnID, "sandbox_id", sandboxID, "error", err.Error())
	}
	heldSince, _ := run.HeldSince()
	// Said out loud: a reaped box is a checkout an operator will find gone,
	// and the pause TTL is the knob that decides it.
	log.InfoContext(ctx, "sandbox_pause_ttl_reaped",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", sandboxID,
		"paused_for_s", w.now().Sub(heldSince).Seconds(), "pause_ttl_s", run.PauseTTLSeconds)
	return true
}

// publishCompletion announces the completion and routes the resume to the
// owner.
//
// TWO PUBLISHES, TWO PURPOSES. The crewlet.events.* copy is an ANNOUNCEMENT:
// the dashboard's broadcast stream watches that subject space, and dropping it
// would blank the running-sandboxes panel. The per-seat control copy is a
// COMMAND, and it goes to the seat's own topic because only the node holding
// that seat has the suspended Execute conversation to resume — a single
// fleet-wide group would hand it to a non-owner (N-1)/N of the time.
func (w *Waiter) publishCompletion(ctx context.Context, run PendingRun) error {
	completion := types.SandboxRunCompleted{
		Agent:       run.AgentID,
		AgentHandle: run.AgentHandle,
		RoleName:    run.Role,
		TurnID:      run.TurnID,
		// [PendingRun.UnitOfWork] rather than the raw field: a run parked
		// by a build from before ADR-0017 carries its work key in TurnID
		// and nothing rewrites a parked row, so the raw field would
		// announce an empty unit of work for every run that outlived the
		// upgrade.
		WorkKey: run.UnitOfWork(),
		// The job this tick saw finish, and the only one the completion
		// may claim: see [Tail].
		LaunchID:    run.LaunchID,
		SandboxID:   run.SandboxID,
		CodingAgent: run.CodingAgent,
	}
	// Carry the original trace so the completion turn nests under the turn
	// that started the job rather than opening a root of its own.
	announcement := events.New(completion, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	announcement.Source = run.Role

	if err := w.queue.Publish(ctx, topics.Event(completion.EventType()), announcement); err != nil {
		return err
	}
	control := topics.AgentControl(run.AgentHandle)
	if control == "" {
		// No handle means no routable seat. The announcement still went out,
		// so the failure is visible rather than silent.
		log.WarnContext(ctx, "sandbox_completion_unroutable", "turn_id", run.TurnID)
		return nil
	}
	return w.queue.Publish(ctx, control, announcement)
}
