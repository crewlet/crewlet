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
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// DefaultPollInterval is how often the waiter reconnects to each running box.
//
// 15s bounds the completion-detection latency of a run that ends WITHOUT
// submitting over its bridge — one that died, was cut off, or never called
// `submit_work` — while a tick costs only one reconnect plus a marker probe
// per running box; a run that submits is polled at once and on a short
// schedule after ([Waiter.Nudge], [nudgeOffsets]), so this is the fallback
// rather than every turn's wait. It also touches a running box's stamp ~60
// times inside the orphan reaper's cutoff ([DefaultHostGrace]). The give-up
// window for an unreachable box does NOT derive from it — see [ConnectGiveUp].
const DefaultPollInterval = 15 * time.Second

// ConnectGiveUp is how long a box may stay unreachable before the waiter gives
// up on it and fires completion anyway.
//
// The keepalive runs every tick, so this never fires merely because a run is
// long: it means an infra failure. Firing frees the seat: a box its backend says
// is GONE took its job with it and its round is carried on ([LossBoxGone]); one
// only unreachable is collected, retried, and settled FAILED.
//
// A DURATION, NOT A TICK COUNT: a count scales with the waiter's interval (at
// the e2e suite's 100 ms cadence, four failures is 0.4 s and a briefly slow box
// is declared dead), and giving up is terminal for the job.
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
// CONCURRENT, because one box must never hold up another's: polled in turn,
// every box behind one whose poll hangs to [pollBound] waits that bound before
// its ending is seen. SIXTEEN is a judgment: a container poll forks up to two
// runtime CLI processes against the node's one daemon, so a wedged runtime
// holds at most ~32, and with a healthy poll sub-second, 16 slots serve
// hundreds of boxes in one pass of [DefaultPollInterval]. A stuck box costs its
// neighbours a slot, never a tick: passes start on schedule ([Waiter.pass]) and
// serve the longest-unpolled first, so a healthy box waits at most ceil(N/16)
// bounds.
const MaxConcurrentPolls = 16

// pollBound is the most one box's poll — reaching it, keeping it alive and
// asking its runner — may take before it is abandoned.
//
// A healthy poll is a stat or a runtime CLI call or two, each sub-second. It
// is [ConnectGiveUp]: a poll unfinished within the window after which an
// unreachable box is given up is a box not answering, so what it costs is that
// box's poll, counted against reaching it, and a later pass asks again.
// Without it a poll lasted as long as its box's slowest request.
const pollBound = ConnectGiveUp

// Publisher is the slice of the queue the waiter needs.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// DutyFunc claims the fleet's launch-node watch for one pass: whether this node
// is, for now, the one judge of which launch nodes left for good
// ([Waiter.goneAfter]).
//
// ONE JUDGE, because a verdict announces each run it ends on the event topic,
// once per job ([Waiter.publishCompletion]), which N judges would make N
// copies; and an absence is counted only by the judge that watched it whole
// ([Waiter.forgetAbsences]).
type DutyFunc func(ctx context.Context) (bool, error)

// NodeRoster answers whether a node of the fleet is present: whether it holds
// its presence lease (internal/coord's node class) now. An error is a roster
// that could not say, never an absent node.
type NodeRoster interface {
	Present(ctx context.Context, node string) (bool, error)
}

// WaiterOptions configures a [Waiter].
type WaiterOptions struct {
	Queue   Publisher
	Pending PendingStore

	// Manager is the manager CURRENT at each tick — the coordinator's own
	// [Coordinator.Manager] in the engine.
	//
	// A SOURCE RATHER THAN A VALUE, because the manager is swapped on every
	// apply that changes providers.sandbox and the poll runs for the life
	// of the process: a value captured once would go on reconnecting and
	// reaping through the backends of the catalogue the node booted with —
	// a cell the company had reshaped — while every launch used the new ones.
	Manager func() *Manager

	// Node is the node this waiter runs on, whose launched runs it drives
	// ([PendingRun.LaunchNode]): no other node reaches their boxes. Empty is
	// a node with no id — a test's — whose runs record none either.
	Node string

	// Roster tells the waiter which launch nodes are present, so that, while
	// it holds the launch-node watch, it finds a node that left for good and
	// ends its runs ([Waiter.goneAfter]). Given with ClaimDuty or not at
	// all: neither is a deployment with no fleet, one node, whose runs are
	// all its own.
	Roster NodeRoster

	// Interval is the poll cadence. Zero takes [DefaultPollInterval].
	Interval time.Duration

	// ClaimDuty claims the launch-node watch each pass ([DutyFunc]). It
	// gates the verdict on gone launch nodes alone: this node's own boxes
	// are driven whatever it answers, since no other node can drive them.
	ClaimDuty DutyFunc

	// DutyTTL is how long one successful [WaiterOptions.ClaimDuty] holds
	// the watch without another — the lease the claim takes. REQUIRED with a
	// claim, and longer than the interval: a verdict outlives the pass that
	// started it, and this is what stops it outliving the claim that
	// authorised it ([Waiter.deadline]), past which a peer may judge.
	DutyTTL time.Duration

	// Now is the clock, injectable so a test can expire a pause without
	// waiting half an hour.
	Now func() time.Time

	// Refresh is handed every running box this poll reaches, after its
	// keepalive, to bring up to date what the box holds that expires while
	// the job runs ([RefreshFunc]). Nil refreshes nothing.
	Refresh RefreshFunc

	// Record is where a refresh of a box this waiter polls records each
	// token before it writes it there: the coordinator's
	// [Coordinator.RecordHanded] in the engine, which adds it to the sealed
	// set beside the job's row that every capture of the job's session is
	// scrubbed by. REQUIRED WITH Refresh, since a token the refresh wrote that
	// nothing recorded would be a live secret no capture is scrubbed of.
	Record func(ctx context.Context, run PendingRun, value string, expires time.Time) error

	// Save is offered every box this waiter's poll found running, not past
	// its turn's cap, with the runner the poll asked, while the waiter lives
	// ([SaveFunc]): the coordinator's [Coordinator.SaveRunning] in the
	// engine. Nil offers nothing.
	Save SaveFunc
}

// SaveFunc is offered a running box for a save of its job's session as it
// works. offerer is the life of the waiter that offered it, done once that
// waiter stops; the offer decides from memory, starts any work on a lifetime
// of its own and returns at once, since the poll is the box's keepalive.
type SaveFunc func(offerer context.Context, run PendingRun, runner Runner)

// RefreshFunc rewrites the files of a box's job that carry a limited-time
// credential, when it is renewed: on every poll, since a job is never stopped on
// a clock and a launch-time credential can expire hours in; and at the start of
// a job in a kept box ([CoordinatorOptions.Refresh]). A FILE, because the
// environment cannot change under a running process.
//
// EVERY TOKEN IT WRITES IS RECORDED FIRST, through record
// ([Coordinator.RecordHanded]): a value the launch never saw is one no capture
// of the session is scrubbed of, and an AWS token is printed bare by a
// transcript. A token record refuses is not written; the next poll asks again.
// A failed refresh is the implementation's to log. manager is the one the poll
// or launch reached the box through.
type RefreshFunc func(ctx context.Context, manager *Manager, run PendingRun, box Sandbox, record RecordHanded)

// RecordHanded records value — a token a refresh is about to write into the
// box, which its issuer said lapses at expires (zero for never) — among what
// the job's box was handed ([HandedSecrets]), for every capture of the job's
// session to be scrubbed of, whole and by every string it holds. Nil is a
// token the refresh may write; an error is one it does not write, and never
// names the value. See [RefreshFunc].
type RecordHanded func(ctx context.Context, value string, expires time.Time) error

// Waiter is one node's completion poll and pause reaper for the detached jobs
// it launched: no other node reaches their boxes. Each pass also reclaims the
// boxes on its host no run names ([Waiter.sweep]) and, while this node holds
// the launch-node watch, ends the runs of launch nodes gone ([Waiter.goneAfter]).
// THE POLL IS THE COMPLETION SIGNAL: each tick asks every running box's runner
// whether its command finished, catching what nothing inside the box can (a
// process group dead without a done marker, a job lost with the engine, a
// vanished box), and doubles as the keepalive. What the bridge sees only BRINGS
// IT FORWARD ([Waiter.Nudge]). Completion publishes ExecutorFinished; duplicates
// are harmless, the coordinator claims once. The same tick is the PAUSE REAPER:
// nothing else reclaims a paused box, so one unanswered question would strand it.
type Waiter struct {
	queue   Publisher
	pending PendingStore
	manager func() *Manager

	node   string
	roster NodeRoster

	interval  time.Duration
	claimDuty DutyFunc
	dutyTTL   time.Duration
	now       func() time.Time
	refresh   RefreshFunc
	record    func(ctx context.Context, run PendingRun, value string, expires time.Time) error
	save      SaveFunc

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
	// busy is every turn whose box a task is working on right now: a box is
	// worked on by one task at a time whoever runs the pass, a nudge's
	// included ([Waiter.begin]).
	busy map[string]bool
	// settled orders a walk: each turn's [Waiter.seq] when its last task
	// ended, absent for one no task has worked on yet. Oldest first, so the
	// box a stuck poll held a slot from is not the next to take one.
	settled map[string]uint64
	seq     uint64
	// announced is the job each turn's completion was last announced for,
	// how often and when: the re-announcement schedule
	// ([Waiter.announcing]).
	announced map[string]announced
	// walk stops the walk still handing out slots; the next pass replaces
	// it with one over a fresher listing.
	walk context.CancelFunc
	// following is every turn a nudge's schedule is polling now, one
	// schedule per turn ([Waiter.follow]).
	following map[string]bool
	// absent is when this waiter first found each launch node of a run
	// without its presence, for as long as it stays without it and this
	// waiter holds the launch-node watch ([Waiter.goneAfter],
	// [Waiter.forgetAbsences]).
	absent map[string]time.Time
	// sweeping is whether a sweep of this host's boxes is running: one at
	// a time ([Waiter.sweep]).
	sweeping bool
	// life is the context Start ran the loop under, which a nudge's
	// schedule runs under and every save is offered under
	// ([WaiterOptions.Save]); nil before Start. closed is set as the loop
	// ends, after which a nudge starts nothing ([Waiter.closeTasks]).
	life   context.Context
	closed bool

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
	switch {
	case (opts.Roster == nil) != (opts.ClaimDuty == nil):
		return nil, errors.New("sandbox: a waiter judges launch nodes gone with both the fleet's roster " +
			"(WaiterOptions.Roster) and the launch-node watch that makes it the one judge " +
			"(WaiterOptions.ClaimDuty), or with neither")
	case opts.Refresh != nil && opts.Record == nil:
		return nil, errors.New("sandbox: a waiter that refreshes the boxes it polls records each token " +
			"before it is written there (WaiterOptions.Record), or no capture of the job is scrubbed of it")
	}
	w := &Waiter{
		queue:     opts.Queue,
		pending:   opts.Pending,
		manager:   opts.Manager,
		node:      opts.Node,
		roster:    opts.Roster,
		interval:  opts.Interval,
		claimDuty: opts.ClaimDuty,
		dutyTTL:   opts.DutyTTL,
		now:       opts.Now,
		refresh:   opts.Refresh,
		record:    opts.Record,
		save:      opts.Save,
		pollBound: pollBound,
		slots:     make(chan struct{}, MaxConcurrentPolls),
		failures:  map[string]connectStreak{},
		busy:      map[string]bool{},
		settled:   map[string]uint64{},
		announced: map[string]announced{},
		following: map[string]bool{},
		absent:    map[string]time.Time{},
		stopped:   make(chan struct{}),
	}
	if w.interval <= 0 {
		w.interval = DefaultPollInterval
	}
	if w.claimDuty != nil && w.dutyTTL <= w.interval {
		return nil, fmt.Errorf("sandbox: a waiter that claims the launch-node watch needs its TTL, "+
			"longer than its %v interval (WaiterOptions.DutyTTL is %v): a verdict outlives "+
			"the pass that started it and must not outlive the claim", w.interval, w.dutyTTL)
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
		w.mu.Lock()
		w.life = ctx
		w.mu.Unlock()
		go w.loop(ctx)
		log.InfoContext(ctx, "sandbox_waiter_started", "node", w.node,
			"watches_launch_nodes", w.claimDuty != nil, "poll_seconds", w.interval.Seconds())
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
	// AND NO NUDGE STARTS ONE AFTER: a task is counted under the same lock
	// this closes, so it is counted before the wait or never started.
	defer w.closeTasks()
	// Jittered, so a fleet's waiters do not all wake on the same second and
	// contend for the launch-node watch in lockstep.
	timer := time.NewTimer(w.jittered())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// NOT WAITED FOR: the pass returns once its walk has started, so
		// the next one — and with it the watch's renewal and every other
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
	if err != nil {
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
	// listed is when the pass asked the store for the listing run was read
	// from, which an announcement its poll makes says ([rowAskedField]).
	listed time.Time
}

// pass lists the runs and starts a WALK that hands every run of this node's
// needing work to a task of its own, beside a sweep of the boxes on its host
// and, while this node holds the launch-node watch, the verdict on launch nodes
// gone, and returns as soon as they have started.
// A PASS DOES NOT WAIT FOR ITS TASKS: one box held at its bound would stretch
// every cadence, and the watch would lapse between two of its claims. A box
// whose task still runs is passed over ([Waiter.begin]); a task is bounded by
// [pollBound], the verdict by the claim that authorised it ([Waiter.deadline]).
//
// ONE WALK AT A TIME: a walk still waiting for a slot when the next pass starts
// is replaced by one over the fresher listing; slots go to the runs whose last
// task ended longest ago, so the runs it never reached are served first.
func (w *Waiter) pass(ctx context.Context) (*round, error) {
	// THE INSTANT THE WATCH IS ASKED FOR, taken before the claim: the lease
	// the claim takes runs from when the store applies it, no earlier than
	// this, so a deadline measured from here is never past the lease's own.
	asked := time.Now()
	watching := w.watches(ctx)
	// ONE MANAGER FOR THE WHOLE PASS, read once: a reload landing mid-pass
	// must not poll half the runs through one catalogue and reap the rest
	// through another.
	manager := w.manager()
	if manager == nil {
		return nil, errors.New("sandbox: the waiter's manager source answered no manager")
	}
	// WHEN THE STORE WAS ASKED FOR THE LISTING, which every announcement made
	// from it says ([rowAskedField]): that store answered this waiter after
	// it, whatever a box's poll took before the announcement is made.
	listed := w.now()
	runs, err := w.pending.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	w.forget(runs)
	work := w.order(runs, listed)

	// ONE CHILD OF ctx PER WALK, made once: ctx is the loop's, which lives
	// as long as the process, and a child it was never told to forget —
	// a cancel func overwritten before anybody called it — is held in its
	// list of children until the loop ends, one more every pass.
	walkCtx, stopWalk := context.WithCancel(ctx)
	w.replaceWalk(stopWalk)
	r := &round{}
	// WHAT ELSE THE PASS OWES, each a task of its own beside the walk, so
	// neither holds up a poll: the boxes on this host no run names any
	// more, and, while this node holds the watch, the launch nodes gone for
	// good — that one BOUNDED BY THE CLAIM, past which a peer may judge.
	w.spawn(r, func() { w.sweep(ctx, manager, runs, "") })
	if watching {
		w.spawn(r, func() {
			goneCtx, stop := context.WithDeadline(ctx, w.deadline(asked))
			defer stop()
			w.goneAfter(goneCtx, manager, runs)
		})
	}
	r.tasks.Add(1)
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		defer r.tasks.Done()
		defer stopWalk()
		w.walkTasks(ctx, walkCtx, manager, work, r)
	}()
	return r, nil
}

// spawn runs f as one of the pass's tasks, which the pass's caller and
// [Waiter.Stop] wait for.
func (w *Waiter) spawn(r *round, f func()) {
	r.tasks.Add(1)
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		defer r.tasks.Done()
		f()
	}()
}

// drivesRun reports whether run's box is one this waiter drives: one its node
// launched, which no other node reaches.
func (w *Waiter) drivesRun(run PendingRun) bool { return run.LaunchNode == w.node }

// walkTasks starts each task in order, as slots free up, until it runs out of
// tasks or the walk is replaced.
//
// A task's context derives from the PASS, not from the walk: a walk replaced
// by the next pass stops handing out slots, and the tasks it already started
// run on to their bound.
func (w *Waiter) walkTasks(ctx, walkCtx context.Context, manager *Manager, work []task, r *round) {
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
		if !w.begin(t.run.TurnID) {
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
			taskCtx, stop := context.WithTimeout(ctx, w.pollBound)
			defer stop()
			if t.reap {
				w.reapOne(taskCtx, manager, t.run)
				return
			}
			if w.pollBounded(ctx, taskCtx, manager, t.run, t.listed) {
				r.fired.Add(1)
			}
		}()
	}
}

// acquireSlot takes one of [MaxConcurrentPolls] for the walk, waiting for one
// to free up, and reports false — holding none — once the walk has been
// replaced or its pass's context has ended.
//
// ASKED AGAIN AFTER THE SLOT IS TAKEN: a slot freeing up as the walk is
// replaced leaves both cases of the wait ready, and Go picks between them at
// random — so half the time the walk would start a task from the listing the
// next pass has superseded, for a run that may have moved on since, or under
// a context already ended.
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
// first and the listing's own order between equals; listed is when the store
// was asked for that listing.
func (w *Waiter) order(runs []PendingRun, listed time.Time) []task {
	now := w.now()
	var work []task
	for _, run := range runs {
		switch {
		case !w.drivesRun(run):
			// ANOTHER NODE'S: its box is on the host of the node that
			// launched it, whose own waiter drives it.
		case run.Ending != nil:
			// A RUN WHOSE ENDING IS DECIDED is that ending's to finish —
			// its decider's retry, or the seat's next holder — and nothing
			// a poll does can help it: a completion it announced would be
			// refused its claim, a keepalive would keep alive the very box
			// the ending is reclaiming, and a reap would race the ending's
			// own reclaim.
		case run.Status == StatusRunning && run.SandboxID != "" && !run.Held(now):
			work = append(work, task{run: run, listed: listed})
		case pauseExpired(run, now):
			work = append(work, task{run: run, reap: true})
		}
		// Running is the ONLY pollable state. [StatusLaunching] is excluded:
		// its turn has not yet written the conversation a resume re-enters, so
		// a completion fired there is a claim the coordinator can only answer
		// by failing the run, destroying the turn. A running row with no box
		// yet is passed over too: the backend refuses an empty id, and polled
		// it would count as a box not reached and be given up while its launch
		// still provisions. The next pass finds both.
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	slices.SortStableFunc(work, func(a, b task) int {
		return cmp.Compare(w.settled[a.run.TurnID], w.settled[b.run.TurnID])
	})
	return work
}

// deadline is how long the launch-node watch a claim made at asked authorises
// a verdict for: the claim's exclusive window, with one interval as its margin
// ([coord.ExclusiveThrough] says why).
//
// At the default cadence that is 30 s — a watch of three intervals, less one —
// and a peer can hold the watch only once it has lapsed, so a verdict ended by
// this never overlaps one of the peer's on the same launch node.
func (w *Waiter) deadline(asked time.Time) time.Time {
	return coord.ExclusiveThrough(asked, w.dutyTTL, w.interval)
}

// rowContext is the context a task's write of a run's row is made under: the
// bound ctx carries ([pollBound] for a box's task, the claim's window for
// [Waiter.goneAfter]) plus the time a row at its bound takes to cross this
// node's store connection ([RowBudget]). It lives under the waiter's own
// lifetime, so a stop ends the write ([Waiter.Start]); under [Waiter.Tick]
// only the deadline bounds it.
//
// THE BOUND IS A BOX'S AND A CLAIM'S, NEVER A ROW'S: a row of megabytes written
// within either alone was cut mid-transfer on a slow link and resent by every
// pass, so a parked box was never reaped (crewlet/crewlet#1076). Past the window
// another writer may write the same row; each write is a compare-and-set, so the
// second changes nothing.
func (w *Waiter) rowContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return context.WithCancel(ctx)
	}
	w.mu.Lock()
	life := w.life
	w.mu.Unlock()
	if life == nil {
		life = context.WithoutCancel(ctx)
	}
	return context.WithDeadline(life, deadline.Add(w.pending.RowCrossing()))
}

// pollBounded polls one box within its task's context and announces its
// completion where the poll found one, reporting whether it did; read is when
// the store was asked for the row run was read from, which the announcement
// says ([rowAskedField]).
//
// THE ANNOUNCEMENT IS NOT UNDER THE BOUND: the bound is on reaching the box,
// and a completion found a second before it ran out is still a completion. A
// duplicate announcement is harmless — the coordinator claims once — so it
// takes the pass's context, not the task's.
func (w *Waiter) pollBounded(ctx, taskCtx context.Context, manager *Manager, run PendingRun, read time.Time) bool {
	state := w.pollOne(taskCtx, manager, run)
	// A LOSS IS RECORDED BEFORE IT IS ANNOUNCED, as the verdict on a gone
	// launch node is ([Waiter.endGone]): the completion only routes it to
	// the seat's holder, which carries the round on from what the row says.
	if cause := lossOf(state); cause != "" && !w.lose(ctx, run, cause) {
		return false
	}
	switch state {
	case pollDone, pollGone, pollLostJob, pollLostBox, pollCollected:
		// UNREACHED FOR THE GIVE-UP WINDOW, the completion goes out anyway,
		// so the coordinator frees the seat and settles the run (its
		// collection fails to reach the box too) instead of it hanging.
		first, due := w.announcing(run)
		if !due {
			return false
		}
		if err := w.publishCompletion(ctx, run, first, read); err != nil {
			w.unannounce(run.TurnID)
			log.WarnContext(ctx, "sandbox_completion_publish_failed",
				"turn_id", run.TurnID, "error", err.Error())
			return false
		}
		if !first {
			// QUIETLY: a job its node has not resumed yet (held for a
			// condition, refused by its gate, handed back for a retry)
			// is announced again on the schedule, and its first
			// announcement said so already.
			log.DebugContext(ctx, "sandbox_waiter_reannounced", "turn_id", run.TurnID,
				"launch_id", run.LaunchID, "sandbox_id", run.SandboxID)
			return true
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
// already working on it: a pass's, or a nudge's ([Waiter.pollNow]).
func (w *Waiter) begin(turnID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy[turnID] {
		return false
	}
	w.busy[turnID] = true
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

// watches reports whether this node holds the launch-node watch this pass: never
// without a claim, which is no fleet. Its own boxes are driven whatever this
// answers; only the verdict on gone launch nodes waits on it.
//
// Re-claimed every pass rather than held: the lease is short, so a node that
// dies mid-verdict releases it by lapsing and a peer takes over on its next
// pass, with no handoff protocol.
func (w *Waiter) watches(ctx context.Context) bool {
	if w.claimDuty == nil {
		return false
	}
	holds, err := w.claimDuty(ctx)
	if err != nil {
		// FAIL CLOSED, and the absences it was timing are forgotten, as on
		// a lost watch: a peer may hold it while this node cannot tell, and
		// an absence this node did not watch is no absence it may count.
		// Skipping one verdict costs an interval of a grace of minutes.
		log.WarnContext(ctx, "sandbox_launch_node_watch_claim_failed", "error", err.Error())
		w.forgetAbsences()
		return false
	}
	if !holds {
		w.forgetAbsences()
	}
	return holds
}

// forgetAbsences drops every launch node's absence this waiter was timing
// ([Waiter.goneAfter]): its stint of the launch-node watch is over, or may be.
//
// A VERDICT NEEDS AN ABSENCE WATCHED WHOLE, within one stint of holding the
// watch. Kept across stints, an absence this waiter first saw before it lost
// the watch was still dated then when it got the watch back: a node that had
// returned meanwhile — seen by the peer that held the watch, never by this one
// — and happened to be away again on this waiter's first pass back was judged
// gone at once, and its live runs ended as lost.
func (w *Waiter) forgetAbsences() {
	w.mu.Lock()
	defer w.mu.Unlock()
	clear(w.absent)
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
	for id := range w.announced {
		if !active[id] {
			delete(w.announced, id)
		}
	}
}

type pollState int

const (
	pollRunning   pollState = iota // not yet, or a transient error — retry next pass
	pollDone                       // the job finished
	pollGone                       // the box could not be reached for the give-up window
	pollLostJob                    // the job went with the engine that last saw it running
	pollLostBox                    // the box is gone from where it ran, its job with it
	pollCollected                  // the job was read back already, and its box is asleep
)

// lossOf is the loss a poll state found, "" for a state that is no loss.
func lossOf(state pollState) LossCause {
	switch state {
	case pollLostJob:
		return LossEngineRestarted
	case pollLostBox:
		return LossBoxGone
	}
	return ""
}

// pollOne reconnects and asks the runner whether the job has finished, and
// offers a job still running, within its turn's cap, for a save while the
// waiter lives ([WaiterOptions.Save]).
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
	box, collected, err := w.reach(ctx, provider, run)
	if collected {
		// ITS JOB IS OVER AND WAS READ BACK, its box paused by that
		// collection: announced again, and the box left asleep, unpolled
		// and not kept alive — see [Waiter.reach].
		w.succeed(run.TurnID)
		return pollCollected
	}
	if box == nil && err == nil {
		// The run moved on between the listing and the read, and its box
		// was paused by whatever moved it — or its record could not be
		// read to say: nothing to keep alive, nothing to poll this tick,
		// and not a failure to reach the box.
		return pollRunning
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// STOPPED, not unanswered: the waiter is stopping, which says
			// nothing about whether this box can be reached. A task ended at
			// its DEADLINE is counted below — a box that did not answer
			// inside the bound.
			return pollRunning
		}
		streak, giveUp := w.fail(run.TurnID)
		log.WarnContext(ctx, "sandbox_connect_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"attempts", streak.attempts,
			"unreachable_for_s", w.now().Sub(streak.since).Seconds(),
			"error", err.Error())
		switch {
		case !giveUp:
			return pollRunning
		case errors.Is(err, ErrBoxGone):
			// GONE, NOT UNREACHED: its host says the box no longer
			// exists, and its job went with it — a box root that did not
			// outlive its node's restart, a box reaped while the engine
			// was away. What it did is not there to read, and its round
			// is carried on ([JobLoss]).
			return pollLostBox
		}
		return pollGone
	}
	w.succeed(run.TurnID)

	// KEEPALIVE. The engine imposes NO run-time TTL on a launched job: it
	// touches the box's stamp every tick ([Sandbox.Touch]), so the orphan
	// reaper sees a box its waiter still drives where the box's record of its
	// run did not land, and completion is detected by tracking the job.
	if err = box.Touch(ctx); err != nil {
		log.DebugContext(ctx, "sandbox_keepalive_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	if w.refresh != nil {
		// RECORDED BESIDE THE JOB'S ROW before it is written, against the
		// job this poll listed: a row that has moved on refuses it.
		w.refresh(ctx, manager, run, box, func(ctx context.Context, value string, expires time.Time) error {
			return w.record(ctx, run, value, expires)
		})
	}

	runner, err := manager.RunnerFor(run.Harness)
	if err != nil {
		// A misconfigured runner cannot be polled, and retrying forever
		// would hold the seat busy for the life of the deployment.
		log.ErrorContext(ctx, "sandbox_poll_runner_missing",
			"turn_id", run.TurnID, "harness", run.Harness, "error", err.Error())
		return pollGone
	}
	state, err := runner.Poll(ctx, box, run.Handle())
	if err != nil {
		log.WarnContext(ctx, "sandbox_poll_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "error", err.Error())
		if run.PastDeadline(w.now()) {
			// PAST THE TURN'S CAP A POLL THAT CANNOT SAY IS NOT A
			// REASON TO KEEP THE JOB OUT: the box answered, so its
			// collection can read it, and that ends the job as the
			// deadline below does. Read as still running, a job whose
			// poll kept failing was kept alive with no bound at all.
			log.InfoContext(ctx, "sandbox_run_past_deadline",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", run.SandboxID,
				"deadline", run.Deadline.Format(time.RFC3339),
				"detail", "the turn's wall-clock cap has passed and its job could not be "+
					"polled; the run is ended and its turn resumed with what it did")
			return pollDone
		}
		return pollRunning
	}
	switch state {
	case JobEnded:
		return pollDone
	case JobLost:
		// KILLED WITH ITS ENGINE, not ended: see [JobState]. What it left
		// is not its result, and its round is carried on ([JobLoss]).
		return pollLostJob
	}
	if run.PastDeadline(w.now()) {
		// THE TURN'S CAP HAS PASSED, and the run is ended as if it had
		// finished: its completion goes out, and the collection ENDS ITS JOB
		// as a stop's read does, reads what it did up to then and says why —
		// see [PendingRun.Deadline], [Coordinator.readBox]. Kept alive above
		// all the same, so the collection finds a box to read.
		log.InfoContext(ctx, "sandbox_run_past_deadline",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", run.SandboxID,
			"deadline", run.Deadline.Format(time.RFC3339),
			"detail", "the turn's wall-clock cap has passed with its run still working; "+
				"the run is ended and its turn resumed with what it did")
		return pollDone
	}
	if offerer := w.offerer(ctx); w.save != nil && offerer.Err() == nil {
		w.save(offerer, run, runner)
	}
	return pollRunning
}

// offerer is what a save is offered under: the waiter's life ([Waiter.Start]),
// so no save is offered once it stops, or, before Start, ctx's values alone,
// as [Waiter.rowContext] takes them, which no pass's end cancels.
func (w *Waiter) offerer(ctx context.Context) context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.life == nil {
		return context.WithoutCancel(ctx)
	}
	return w.life
}

// reach is a running run's box, for the poll: attached WITHOUT resuming it
// ([Provider.Attach]), and resumed only where a paused box holds the running
// run's job and no collection has read it back (collected reports one has).
// THE POLL'S RECORD IS A MOMENT OLD: a poll listed before a collection claimed
// the run must not RESUME the box after its pause (Connect wakes it, and only
// running records are polled, so its job would run on with nothing watching).
// A paused box under a record still running the same job turns on
// [LaunchRecord.Collected]. READ BACK: the poll announces the job again and
// leaves the box asleep for the next collection ([Manager.Reconnect]); woken, a
// box held for a model's limit ([Coordinator.keepRefused]) would be kept alive
// every poll for an hour. NOT READ BACK: woken, kept alive, polled. Anything
// else answers no box and no error.
func (w *Waiter) reach(ctx context.Context, provider Provider, run PendingRun) (box Sandbox, collected bool, err error) {
	box, err = provider.Attach(ctx, run.SandboxID)
	if !errors.Is(err, ErrBoxPaused) {
		return box, false, err
	}
	current, ok, err := w.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		// The STORE did not answer, which says nothing about the box: not
		// counted against reaching it, and the next tick asks again.
		log.WarnContext(ctx, "sandbox_poll_record_unread", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "error", err.Error())
		return nil, false, nil
	case !ok || current.LaunchID != run.LaunchID || current.Status != StatusRunning ||
		current.SandboxID != run.SandboxID:
		log.DebugContext(ctx, "sandbox_poll_left_paused", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "detail", "the box is paused and its run is no "+
				"longer running it, so the poll leaves it as it is")
		return nil, false, nil
	case current.Launch.Collected:
		return nil, true, nil
	}
	box, err = provider.Connect(ctx, run.SandboxID)
	return box, false, err
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
// Scoped to runs parked on a clarification, the one state whose box is held for
// an open-ended human wait and so needs an engine-side expiry. Every other
// paused box belongs to a tail being driven (a collection, a resuming Execute
// loop) and is settled by it; expiring those would kill a box under live work.
//
// WHETHER A BOX IS HELD IS THE ROW'S ANSWER, NOT A STAMP'S: a parked row naming
// a box holds its directory and memory whether or not the pause instant was
// written, so the reading is [PendingRun.HeldSince]; skipping an unstamped row
// would strand the box after one warn-only store write failed.
func pauseExpired(run PendingRun, now time.Time) bool {
	// An ANSWERED run's box is held for as long as its resume waits, which
	// on a seat nobody holds is open-ended — see [PendingStore.ExpirePause].
	if run.Status != StatusAwaiting && run.Status != StatusAnswered {
		return false
	}
	heldSince, held := run.HeldSince()
	if !held {
		return false
	}
	// A zero TTL is not a deadline to enforce here: it means "never hold a
	// blocked box", so the coordinator already tore this one down when the
	// run blocked and there is no paused box left to expire.
	if run.PauseTTLSeconds <= 0 {
		return false
	}
	return now.Sub(heldSince) >= time.Duration(run.PauseTTLSeconds*float64(time.Second))
}

// reapOne reclaims one run's paused box past its TTL ([pauseExpired]),
// reporting whether it did.
//
// A TASK OF ITS OWN, like a poll, so a reclaim that takes its time — a kill
// waits on a process group or the runtime's `rm -f` — holds up no other box.
func (w *Waiter) reapOne(ctx context.Context, manager *Manager, run PendingRun) bool {
	// CLAIM FIRST, DESTROY SECOND. The reaper decides from a snapshot taken
	// seconds ago, and the clarification answer that un-pauses the run may
	// have arrived since: the claim has already flipped the row to resumed
	// and the Execute loop is reconnecting to this very box. Killing before
	// the compare-and-set destroyed it underneath that resume — and then the
	// CAS refused, so the reaper walked away silently and the answered run
	// failed on a box that no longer existed. The CAS is the authority for
	// the whole reap, not just for the status write.
	//
	// THE FLIP WRITES THE ROW WHOLE, a parked turn's state and all, so it is
	// given the row's crossing beyond the task's bound ([Waiter.rowContext]).
	flip, flipped := w.rowContext(ctx)
	won, err := w.pending.ExpirePause(flip, run.TurnID)
	flipped()
	if err != nil || !won {
		return false
	}
	// The box record was cleared by the flip itself, so an answer claiming
	// this run can no longer be told to continue in a checkout that is about
	// to be destroyed. The id survives only here, in the snapshot the reaper
	// is acting on.
	sandboxID := run.SandboxID
	// THE KILL OUTLIVES THE TASK'S BOUND, a teardown's context of its own: the
	// flip has released the row, so a kill the task's deadline or a stop cut
	// short would hold the paused box until its host's sweep came round.
	killCtx, cancel := detached(ctx)
	defer cancel()
	// Kill by id: Connect would auto-resume the box, letting its frozen
	// processes run again purely to shut them down.
	killed := false
	provider, err := manager.Provider(Placement(run.Placement))
	if err != nil {
		log.WarnContext(ctx, "sandbox_pause_reap_no_backend",
			"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
	} else if err := provider.Kill(killCtx, sandboxID); err != nil {
		// THE ROW IS RELEASED EITHER WAY: the box keeps the record of its run,
		// which no row names now, so its host's sweep tries again.
		log.WarnContext(ctx, "sandbox_pause_reap_kill_failed",
			"turn_id", run.TurnID, "sandbox_id", sandboxID, "error", err.Error())
	} else {
		killed = true
	}
	heldSince, _ := run.HeldSince()
	// Said out loud: a reaped pause's box is a checkout an operator will find
	// gone, now or once its host's sweep kills it, and the pause TTL is the
	// knob that decides it.
	log.InfoContext(ctx, "sandbox_pause_ttl_reaped",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", sandboxID, "box_killed", killed,
		"paused_for_s", w.now().Sub(heldSince).Seconds(), "pause_ttl_s", run.PauseTTLSeconds)
	return true
}

// announcementOf is the completion of run's job, and a fresh announcement of
// it, as the waiter that drives the job's box makes one: what a node that owes
// the job's completion a re-fire with no delivery in hand offers its
// coordinator ([Coordinator.RecoverSeat]), so the two never say the job
// differently.
func announcementOf(run PendingRun) (types.ExecutorFinished, *events.Event) {
	completion := types.ExecutorFinished{
		Agent:       run.AgentID,
		AgentHandle: run.AgentHandle,
		RoleName:    run.Role,
		TurnID:      run.TurnID,
		WorkKey:     run.WorkKey,
		// The job this tick saw finish, and the only one the completion
		// may claim: see [Tail].
		LaunchID:  run.LaunchID,
		SandboxID: run.SandboxID,
		Harness:   run.Harness,
	}
	// Carry the original trace so the completion turn nests under the turn
	// that started the job rather than opening a root of its own.
	announcement := events.New(completion, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	announcement.Source = run.Role
	return completion, announcement
}

// publishCompletion announces the completion and routes the resume to the
// owner; read is when this waiter asked its store for the run's row it
// announces the job from, which the announcement says ([rowAskedField],
// [announcedAfter]). Two publishes: the crewlet.events.* copy is an
// announcement the dashboard's running-sandboxes panel watches, and the
// per-seat control copy is a command to the one node holding the seat, which a
// fleet-wide group would hand to a non-owner (N-1)/N of the time.
func (w *Waiter) publishCompletion(ctx context.Context, run PendingRun, public bool, read time.Time) error {
	completion, announcement := announcementOf(run)
	askedRow(announcement, read)

	// THE EVENTS COPY ONCE PER JOB: it is the record that the job finished,
	// which the dashboard and the event store keep, and a job announced
	// again is the same fact. Every announcement is routed to the seat.
	if public {
		if err := w.queue.Publish(ctx, topics.Event(completion.EventType()), announcement); err != nil {
			return err
		}
	}
	control := controlSubject(run.AgentID)
	if control == "" {
		// No seat id means no routable seat. The announcement still went
		// out, so the failure is visible rather than silent.
		log.WarnContext(ctx, "sandbox_completion_unroutable", "turn_id", run.TurnID,
			"agent", run.AgentHandle)
		return nil
	}
	return w.queue.Publish(ctx, control, announcement)
}
