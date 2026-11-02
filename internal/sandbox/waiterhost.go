package sandbox

import (
	"context"
	"maps"
	"slices"
	"time"
)

// What a waiter does beside its poll: the fleet's finds the launch nodes that
// left for good, a node's own reclaims the boxes on its host that no run names
// any more, and either polls a run the moment it is told its work is in.

// NudgeWhy is why a waiter is told to look at a run now.
type NudgeWhy string

const (
	// NudgeSubmitted is a run whose work was just submitted over its tool
	// bridge (`submit_work`), or whose question was just asked there
	// (`ask_and_wait`): its CLI is about to end its turn and exit, so
	// the waiter that drives it polls it now and on a short schedule after
	// ([nudgeOffsets]) rather than at its next pass.
	NudgeSubmitted NudgeWhy = "submitted"
	// NudgeReleased is a run whose box on another node's host a node has
	// just left to that host — an ending that would have reclaimed it, a
	// relaunch that does not continue in it — so that host's waiter looks
	// at once for a box no run names ([Waiter.sweep]).
	NudgeReleased NudgeWhy = "released"
)

// NudgeWhys are the two.
var NudgeWhys = []NudgeWhy{NudgeSubmitted, NudgeReleased}

// Valid reports whether this build knows why.
func (why NudgeWhy) Valid() bool { return slices.Contains(NudgeWhys, why) }

// Nudge tells whichever waiter drives a run to look at it now.
//
// A HINT, NEVER AN INSTRUCTION, which is why it travels unsigned on an
// ephemeral broadcast ([topics.SandboxNudge]): what the waiter it reaches does
// is read the run's row and do what its own pass would — poll a running job,
// reclaim a box no row names — so a nudge that is forged, repeated or lost
// changes only WHEN that happens, never whether. The pass is the guarantee.
type Nudge struct {
	TurnID string `json:"turn_id"`
	// LaunchID is the job the nudge is about: a job the row has moved on
	// from is not polled for it.
	LaunchID string   `json:"launch_id,omitempty"`
	Why      NudgeWhy `json:"why"`
}

// nudgeFirstGap is how long after the poll a nudge starts the next is made;
// each one after waits twice as long as the one before.
//
// ONE SECOND, from what is left of a run once it has submitted: its CLI makes
// one more model call, the closing message of its turn — a few seconds, the
// time to first token of a short answer — and exits, and its wrapper writes
// the done marker the poll reads as it does. Polls at 0, 1, 3 and 7 seconds
// find most runs within a second or two of exiting; the doubling keeps a run
// whose closing call is slow from being asked more than a few times; and the
// schedule stops at the poll interval ([nudgeOffsets]), past which the
// periodic poll comes no later than the schedule would.
const nudgeFirstGap = time.Second

// nudgeOffsets is when a nudged run is polled, measured from the nudge: at
// once, then after [nudgeFirstGap] and doubling gaps, for as long as the next
// would still come before one poll interval has passed. At the default
// interval that is 0, 1, 3 and 7 seconds; at an interval of a second or less,
// the one immediate poll.
func nudgeOffsets(interval time.Duration) []time.Duration {
	offsets := []time.Duration{0}
	for gap, at := nudgeFirstGap, nudgeFirstGap; at < interval; gap, at = 2*gap, at+2*gap {
		offsets = append(offsets, at)
	}
	return offsets
}

// Nudge tells this waiter to look at a run now — see [Nudge]. A nudge about a
// run this waiter does not drive, or before [Waiter.Start] or after
// [Waiter.Stop], is nothing. It returns at once: what the nudge starts runs on
// the waiter's own lifetime.
func (w *Waiter) Nudge(n Nudge) {
	if n.TurnID == "" || !n.Why.Valid() {
		return
	}
	if n.Why == NudgeReleased && w.drives != DrivesLocal {
		// A box handed back to its host is the host's own waiter's.
		return
	}
	w.mu.Lock()
	life := w.life
	if life == nil || w.closed || life.Err() != nil ||
		(n.Why == NudgeSubmitted && w.following[n.TurnID]) {
		w.mu.Unlock()
		return
	}
	if n.Why == NudgeSubmitted {
		w.following[n.TurnID] = true
	}
	// COUNTED UNDER THE LOCK the loop's end closes, so a task is counted
	// before [Waiter.Stop]'s wait or not started at all.
	w.tasks.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.tasks.Done()
		if n.Why == NudgeReleased {
			if manager := w.manager(); manager != nil {
				w.sweep(life, manager, nil, n.TurnID)
			}
			return
		}
		defer func() {
			w.mu.Lock()
			delete(w.following, n.TurnID)
			w.mu.Unlock()
		}()
		w.follow(life, n)
	}()
}

// closeTasks marks the loop as ended, after which a nudge starts nothing.
func (w *Waiter) closeTasks() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// follow polls a run its nudge named on the nudge's schedule ([nudgeOffsets]),
// until a poll announces its completion or the run is no longer one to poll:
// its job replaced, claimed, parked or ended, held, another waiter's — or, on
// the fleet's waiter, the duty no longer this node's.
func (w *Waiter) follow(ctx context.Context, n Nudge) {
	start := time.Now()
	for _, offset := range nudgeOffsets(w.interval) {
		if wait := time.Until(start.Add(offset)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		manager := w.manager()
		if manager == nil || !w.holdsDuty() {
			return
		}
		run, found, err := w.pending.Get(ctx, n.TurnID)
		if err != nil {
			// The STORE did not answer, which says nothing about the run:
			// asked again at the next offset, and the pass after that.
			log.DebugContext(ctx, "sandbox_nudge_unread", "turn_id", n.TurnID, "error", err.Error())
			continue
		}
		if !found || run.LaunchID != n.LaunchID || run.Status != StatusRunning || run.Ending != nil ||
			run.SandboxID == "" || run.Held(w.now()) || !w.drivesRun(manager, run) {
			return
		}
		if w.pollNow(ctx, manager, run) {
			return
		}
	}
}

// pollNow polls one run as a pass's task would — a slot of its own, the box
// claimed for it alone, bounded by the duty it runs under — and reports
// whether it announced the run's completion. A run a pass's task is already
// polling is left to it.
func (w *Waiter) pollNow(ctx context.Context, manager *Manager, run PendingRun) bool {
	if !w.acquireSlot(ctx) {
		return false
	}
	defer func() { <-w.slots }()
	w.mu.Lock()
	duty := w.dutyUntil
	w.mu.Unlock()
	taskCtx, stop := context.WithDeadline(ctx, w.taskDeadline(duty))
	defer stop()
	if !w.begin(ctx, run.TurnID, stop) {
		return false
	}
	defer w.end(run.TurnID)
	return w.pollBounded(ctx, taskCtx, manager, run)
}

// holdsDuty reports whether this waiter may poll now: one that claims no duty
// always, and the fleet's while the duty it last claimed still authorises work
// ([Waiter.deadline]).
func (w *Waiter) holdsDuty() bool {
	if w.claimDuty == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Now().Before(w.dutyUntil)
}

// goneAfter is the fleet's waiter finding the launch nodes that left for good,
// and ending what their local runs were waiting on.
//
// A LOCAL BOX GOES WITH ITS NODE. A `direct` or `container` box is driven by
// the node that launched it alone ([Drives]), so a node that leaves the fleet
// for good takes with it every poll, keepalive and reclaim its runs would ever
// get: a running job is never announced, and its seat stays held behind it for
// ever. So the fleet's waiter watches the presence of every node a local run
// names ([NodeRoster]), and once one has been without it for the box timeout
// ([Manager.BoxTimeout]) — the bound a box in any cell gets without its
// engine's keepalive, which on E2B is when the provider reclaims it — its
// runs are ended for it:
//
//   - A RUNNING JOB has the verdict recorded on its row as lost
//     ([PendingStore.MarkLost], [LossLaunchNodeGone]) and its completion
//     announced, which the seat's holder claims and carries the job's round
//     on from in a box of its own ([Coordinator.carryOnLost]) — announced
//     again on every pass until a claim takes it, as a finished job's is.
//   - A RUN PARKED ON A QUESTION, or answered and waiting on its resume, has
//     its box released as an expired pause's is ([PendingStore.ExpirePause]),
//     with nothing to kill: the run waits for its answer without a box, and
//     its resume starts from the pushed branch.
//
// NEVER WHILE THE NODE IS MERELY RESTARTING. Presence is what the fleet knows
// a node by, and a node that drains gives it up at the drain's first step, so
// its absence at one instant is no verdict at all: the bound is the box
// timeout, measured from when this waiter first found the node without it, and
// a node that is back within it — its own waiter picking its runs up again —
// is never found gone. A roster that cannot answer is no evidence either way,
// and changes nothing.
//
// The clock is this waiter's own and starts again where the duty moves — every
// absence is forgotten when the duty is lost, or cannot be confirmed
// ([Waiter.forgetAbsences]) — which can only make a verdict later.
func (w *Waiter) goneAfter(ctx context.Context, manager *Manager, runs []PendingRun) {
	if w.roster == nil {
		return
	}
	now := w.now()
	byNode := map[string][]PendingRun{}
	for _, run := range runs {
		if !manager.OnLaunchHost(run) || run.LaunchNode == "" || run.Ending != nil {
			continue
		}
		parked := (run.Status == StatusAwaiting || run.Status == StatusAnswered) && run.HasBox()
		if run.Status != StatusRunning && !parked {
			continue
		}
		byNode[run.LaunchNode] = append(byNode[run.LaunchNode], run)
	}
	w.mu.Lock()
	for node := range w.absent {
		if _, waiting := byNode[node]; !waiting {
			delete(w.absent, node)
		}
	}
	w.mu.Unlock()
	grace := manager.BoxTimeout()
	for _, node := range slices.Sorted(maps.Keys(byNode)) {
		if ctx.Err() != nil {
			return
		}
		present, err := w.roster.Present(ctx, node)
		if err != nil {
			log.WarnContext(ctx, "sandbox_launch_node_unknown", "node", node, "error", err.Error(),
				"detail", "the fleet's roster could not say whether the node that launched these "+
					"runs is present; nothing is decided about them this pass")
			continue
		}
		w.mu.Lock()
		since, seen := w.absent[node]
		switch {
		case present:
			delete(w.absent, node)
		case !seen:
			since = now
			w.absent[node] = now
		}
		w.mu.Unlock()
		if present || now.Sub(since) < grace {
			continue
		}
		for _, run := range byNode[node] {
			w.endGone(ctx, run, since, now)
		}
	}
}

// endGone ends what one run of a gone launch node was waiting on — see
// [Waiter.goneAfter].
func (w *Waiter) endGone(ctx context.Context, run PendingRun, since, now time.Time) {
	if run.Status != StatusRunning {
		released, err := w.pending.ExpirePause(ctx, run.TurnID)
		if err != nil {
			log.WarnContext(ctx, "sandbox_launch_node_gone_unrecorded", "turn_id", run.TurnID,
				"node", run.LaunchNode, "error", err.Error())
			return
		}
		if released {
			log.WarnContext(ctx, "sandbox_launch_node_gone_box_released", "turn_id", run.TurnID,
				"agent", run.AgentHandle, "node", run.LaunchNode, "sandbox_id", run.SandboxID,
				"absent_for_s", now.Sub(since).Seconds(),
				"detail", "the run's box was on the host of a node that left the fleet; the run "+
					"waits for its answer without it, and resumes from its pushed branch")
		}
		return
	}
	marked, err := w.pending.MarkLost(ctx, run.TurnID, run.LaunchID,
		JobLoss{Cause: LossLaunchNodeGone, Node: run.LaunchNode, At: now})
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_launch_node_gone_unrecorded", "turn_id", run.TurnID,
			"node", run.LaunchNode, "error", err.Error())
		return
	case !marked:
		// The row moved on since the listing — a claim, a relaunch, an
		// ending — and what it holds now was not judged.
		return
	}
	if run.Lost == nil {
		log.WarnContext(ctx, "sandbox_launch_node_gone", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "node", run.LaunchNode, "sandbox_id", run.SandboxID,
			"absent_for_s", now.Sub(since).Seconds(),
			"detail", "the run's box was on the host of a node that left the fleet and did not "+
				"come back within the box timeout; its completion is announced so the seat's "+
				"holder carries its round on in a box of its own")
	}
	if run.Held(now) {
		// Its resume is held back for now ([PendingRun.Held]): announced
		// once the hold lifts, by a later pass.
		return
	}
	if err := w.publishCompletion(ctx, run); err != nil {
		log.WarnContext(ctx, "sandbox_completion_publish_failed", "turn_id", run.TurnID,
			"error", err.Error())
	}
}

// lose records on a running job's row that a poll found it lost with the host
// it ran on ([PendingStore.MarkLost]), reporting whether the row carries a
// loss for the job now, so that its completion may be announced.
//
// THE WAITER THAT DRIVES THE BOX IS THE ONE THAT CAN TELL: its poll asked the
// box ([JobState]) or found it gone. The seat's holder, wherever it is, acts on
// the row. A row that moved on since the listing — claimed, relaunched, ended —
// holds a job this poll did not judge, and nothing is announced for it; a
// store that did not answer is asked again by the next pass.
func (w *Waiter) lose(ctx context.Context, manager *Manager, run PendingRun, cause LossCause) bool {
	loss := JobLoss{Cause: cause, At: w.now()}
	if manager.OnLaunchHost(run) {
		loss.Node = run.LaunchNode
	}
	marked, err := w.pending.MarkLost(ctx, run.TurnID, run.LaunchID, loss)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_run_lost_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "cause", string(cause), "error", err.Error())
		return false
	case !marked:
		return false
	}
	if run.Lost == nil {
		log.WarnContext(ctx, "sandbox_run_lost", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "launch_id", run.LaunchID, "sandbox_id", run.SandboxID,
			"node", loss.Node, "cause", string(cause),
			"detail", "the run's job was lost before anything saw it end: "+cause.Why()+
				"; its completion is announced so the seat's holder carries its round on")
	}
	return true
}

// sweep is a node's own waiter reclaiming the boxes on its host that no run
// names any more: each box that records the run it was made for ([HostBox]) is
// looked up by that run, and reclaimed where the run's row no longer names it
// ([hostBoxFree]). With only set, the boxes of that one turn — a nudge's
// ([NudgeReleased]); otherwise every box on the host, once per pass.
//
// THE HOST IS THE ONE PARTY THAT CAN. A box here is a process tree or a
// container no other node reaches, so a run that moved on elsewhere — ended,
// stopped or relaunched by the node that holds its seat now — left its box
// here with nobody else able to reclaim it ([Coordinator.reclaimBox]). And
// because the question is asked of the box rather than of the runs this node
// remembers, a box whose run ended while this node was stopped is reclaimed
// by the first pass after it starts.
//
// RECLAIMED ONLY ON A FRESH READ of the run: the pass's listing is a moment
// old, and a row that names the box again since — a relaunch continuing in it
// — keeps it. A listing that cannot be made, or a row that cannot be read,
// reclaims nothing.
func (w *Waiter) sweep(ctx context.Context, manager *Manager, runs []PendingRun, only string) {
	if only == "" {
		w.mu.Lock()
		if w.sweeping {
			w.mu.Unlock()
			return
		}
		w.sweeping = true
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			w.sweeping = false
			w.mu.Unlock()
		}()
	}
	boxes, err := manager.HostBoxes()
	if err != nil {
		log.WarnContext(ctx, "sandbox_host_boxes_unlisted", "error", err.Error(),
			"detail", "the boxes on this host could not be listed, so none is reclaimed this pass")
		return
	}
	listed := make(map[string]PendingRun, len(runs))
	for _, run := range runs {
		listed[run.TurnID] = run
	}
	for _, box := range boxes {
		if (only != "" && box.TurnID != only) || ctx.Err() != nil {
			continue
		}
		if run, ok := listed[box.TurnID]; ok && !hostBoxFree(run, box) {
			continue
		}
		run, found, err := w.pending.Get(ctx, box.TurnID)
		if err != nil {
			log.WarnContext(ctx, "sandbox_host_box_unchecked", "turn_id", box.TurnID,
				"sandbox_id", box.SandboxID, "error", err.Error())
			continue
		}
		if found && !hostBoxFree(run, box) {
			continue
		}
		w.reclaimHostBox(ctx, manager, box, run, found)
	}
}

// hostBoxFree reports whether a run's row no longer names a box on this host
// that was made for it, so the box is its host's to reclaim:
//
//   - not while the run is LAUNCHING, which may be about to continue in it;
//   - where the row names another box, or none;
//   - where its ending is decided to reclaim the box and nothing of the ending
//     reads it any more — a stop's read of what the run spent is done — which
//     is the moment the ending's own reclaim would have killed it.
func hostBoxFree(run PendingRun, box HostBox) bool {
	switch {
	case run.Status == StatusLaunching:
		return false
	case run.SandboxID != box.SandboxID:
		return true
	case run.Ending != nil && run.Ending.Reclaim && (run.Stop == nil || run.Stop.Collected):
		return true
	}
	return false
}

// reclaimHostBox kills one box on this host that no run names, through the
// backend its own record names, under a context of its own: nothing else will
// ever name it again, so a kill abandoned with the task would leave it for
// good.
func (w *Waiter) reclaimHostBox(ctx context.Context, manager *Manager, box HostBox, run PendingRun, found bool) {
	provider, err := manager.Provider(box.Placement)
	if err != nil {
		log.WarnContext(ctx, "sandbox_host_box_no_backend", "turn_id", box.TurnID,
			"sandbox_id", box.SandboxID, "placement", string(box.Placement), "error", err.Error())
		return
	}
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if err := provider.Kill(killCtx, box.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_host_box_reclaim_failed", "turn_id", box.TurnID,
			"sandbox_id", box.SandboxID, "error", err.Error())
		return
	}
	named := ""
	if found {
		named = run.SandboxID
	}
	log.InfoContext(ctx, "sandbox_host_box_reclaimed", "turn_id", box.TurnID,
		"sandbox_id", box.SandboxID, "placement", string(box.Placement),
		"run_found", found, "run_box", named,
		"detail", "the box is on this node's host and its run no longer names it")
}
