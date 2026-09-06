package sandbox

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

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
	Manager *Manager

	// Interval is the poll cadence. Zero takes [DefaultPollInterval].
	Interval time.Duration

	// ClaimDuty gates the tick in a fleet. Nil means single-node.
	ClaimDuty DutyFunc

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
	manager *Manager

	interval  time.Duration
	claimDuty DutyFunc
	now       func() time.Time

	mu sync.Mutex
	// failures tracks the CONSECUTIVE reconnect failures per turn, cleared
	// on any success. Per-turn rather than per-box because a box id can be
	// cleared and re-minted on a reseed while the run continues.
	failures map[string]connectStreak

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
		now:       opts.Now,
		failures:  map[string]connectStreak{},
		stopped:   make(chan struct{}),
	}
	if w.interval <= 0 {
		w.interval = DefaultPollInterval
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

// Stop ends the poll loop and waits for the in-flight tick to finish.
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
		if _, err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.WarnContext(ctx, "sandbox_waiter_tick_failed", "error", err.Error())
		}
		timer.Reset(w.jittered())
	}
}

// jitterFraction spreads wake-ups across ±20% of the interval, the same spread
// the config reconcile loop uses for the same reason.
const jitterFraction = 0.2

func (w *Waiter) jittered() time.Duration {
	spread := float64(w.interval) * jitterFraction
	return w.interval + time.Duration((rand.Float64()*2-1)*spread)
}

// Tick polls every running run once and reaps expired pauses. Returns how many
// completions fired.
//
// Exported so a test drives one pass deterministically without the loop.
func (w *Waiter) Tick(ctx context.Context) (int, error) {
	if !w.mayTick(ctx) {
		return 0, nil
	}
	runs, err := w.pending.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	w.forget(runs)

	fired := 0
	for _, run := range runs {
		if run.Status != StatusRunning {
			// Running is the ONLY pollable state, and the one it most
			// obviously excludes is [StatusLaunching]: that run's job is
			// already executing, but the turn that started it has not yet
			// written the conversation a resume re-enters. Firing there
			// hands the coordinator a claim it cannot resume, and the
			// coordinator's only honest answer to that is to fail the run
			// — so a coding job that finished inside the window destroyed
			// the turn. The next tick finds it suspended.
			continue
		}
		if run.SandboxID == "" {
			// The row exists before its box is attached — a launch writes
			// it first, so a crash in that window leaves a record rather
			// than a box nothing names. A poll landing in that window has
			// nothing to connect to and nothing to keep alive, and asking
			// the provider for "" is how a nameless box comes to be
			// created. The next tick finds it attached.
			continue
		}
		switch w.pollOne(ctx, run) {
		case pollDone, pollGone:
			// gone → the box vanished; fire anyway so the coordinator frees
			// the seat and marks the run failed (collect will fail to
			// reconnect) instead of hanging.
			if err := w.publishCompletion(ctx, run); err != nil {
				log.WarnContext(ctx, "sandbox_completion_publish_failed",
					"turn_id", run.TurnID, "error", err.Error())
				continue
			}
			fired++
		}
	}
	if fired > 0 {
		log.InfoContext(ctx, "sandbox_waiter_fired", "completions", fired)
	}
	if reaped := w.reapExpiredPauses(ctx, runs); reaped > 0 {
		// Said out loud: a reaped box is a checkout an operator will find
		// gone, and the pause TTL is the knob that decides it.
		log.InfoContext(ctx, "sandbox_paused_boxes_reaped", "count", reaped)
	}
	return fired, nil
}

// mayTick reports whether this node holds the waiter duty this tick.
//
// Re-claimed every tick rather than held: the duty lease is short, so a node
// that dies mid-poll releases it by lapsing and a peer takes over on its next
// tick, with no handoff protocol.
func (w *Waiter) mayTick(ctx context.Context) bool {
	if w.claimDuty == nil {
		return true
	}
	holds, err := w.claimDuty(ctx)
	if err != nil {
		log.WarnContext(ctx, "sandbox_waiter_duty_claim_failed", "error", err.Error())
		// FAIL CLOSED. Not knowing whether this node holds the duty and
		// polling anyway is the multi-poller case the duty exists to
		// prevent — and skipping a tick costs one interval, which the next
		// tick recovers.
		return false
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

// forget drops failure counters for runs that are no longer active, so a
// reused turn id never inherits a dead run's streak.
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
}

type pollState int

const (
	pollRunning pollState = iota // not yet, or a transient error — retry next tick
	pollDone                     // the job finished
	pollGone                     // the box has vanished and can never complete
)

// pollOne reconnects and asks the runner whether the job has finished.
func (w *Waiter) pollOne(ctx context.Context, run PendingRun) pollState {
	provider, err := w.manager.Provider(Placement(run.Placement))
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
	box, err := provider.Connect(ctx, run.SandboxID)
	if err != nil {
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
	if err = box.SetTimeout(ctx, w.manager.BoxTimeout().Seconds()); err != nil {
		log.DebugContext(ctx, "sandbox_keepalive_failed", "turn_id", run.TurnID, "error", err.Error())
	}

	runner, err := w.manager.RunnerFor(run.CodingAgent)
	if err != nil {
		// A misconfigured runner cannot be polled, and retrying forever
		// would hold the seat busy for the life of the deployment.
		log.ErrorContext(ctx, "sandbox_poll_runner_missing",
			"turn_id", run.TurnID, "coding_agent", run.CodingAgent, "error", err.Error())
		return pollGone
	}
	done, err := runner.Poll(ctx, box, RunHandle{
		CommandID: run.CommandID,
		SessionID: run.SessionID,
	})
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

// reapExpiredPauses kills paused boxes past their pause TTL.
//
// Scoped to runs parked on a clarification: that is the one state whose box is
// deliberately held for an open-ended human wait, so it is the one that needs
// an engine-side expiry. Every other paused box belongs to a tail that is
// actively being driven — a completion being collected, an Execute loop
// resuming — and is settled by that tail within the turn. Expiring those from
// here would kill a box out from under live work.
func (w *Waiter) reapExpiredPauses(ctx context.Context, runs []PendingRun) int {
	now := w.now()
	reaped := 0
	for _, run := range runs {
		if run.Status != StatusAwaiting || run.SandboxID == "" || run.PausedAt.IsZero() {
			continue
		}
		// A zero TTL is not a deadline to enforce here: it means "never hold
		// a blocked box", so the coordinator already tore this one down when
		// the run blocked and there is no snapshot left to expire.
		if run.PauseTTLSeconds <= 0 {
			continue
		}
		pausedFor := now.Sub(run.PausedAt)
		if pausedFor < time.Duration(run.PauseTTLSeconds*float64(time.Second)) {
			continue
		}

		// CLAIM FIRST, DESTROY SECOND. The reaper decides from a snapshot
		// taken seconds ago, and the clarification answer that un-pauses the
		// run may have arrived since: the claim has already flipped the row
		// to resumed and the Execute loop is reconnecting to this very box.
		// Killing before the compare-and-set destroyed it underneath that
		// resume — and then the CAS refused, so the reaper walked away
		// silently and the answered run failed on a box that no longer
		// existed. The CAS is the authority for the whole reap, not just for
		// the status write.
		won, err := w.pending.ExpirePause(ctx, run.TurnID)
		if err != nil || !won {
			continue
		}
		// The box record was cleared by the flip itself, so an answer
		// claiming this run can no longer be told to continue in a
		// checkout that is about to be destroyed. The id survives only
		// here, in the snapshot the reaper is acting on.
		sandboxID := run.SandboxID
		// Kill by id: Connect would auto-resume the snapshot, booting the box
		// back up purely to shut it down.
		provider, err := w.manager.Provider(Placement(run.Placement))
		if err != nil {
			log.WarnContext(ctx, "sandbox_pause_reap_no_backend",
				"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
		} else if err := provider.Kill(ctx, sandboxID); err != nil {
			// An already-gone box is the normal case for an old snapshot;
			// the row is released either way.
			log.WarnContext(ctx, "sandbox_pause_reap_kill_failed",
				"turn_id", run.TurnID, "sandbox_id", sandboxID, "error", err.Error())
		}
		reaped++
		log.InfoContext(ctx, "sandbox_pause_ttl_reaped",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", sandboxID,
			"paused_for_s", pausedFor.Seconds(), "pause_ttl_s", run.PauseTTLSeconds)
	}
	return reaped
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
