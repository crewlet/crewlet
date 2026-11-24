package sandbox

import (
	"context"
	"maps"
	"slices"
	"time"
)

// What a waiter does beside its poll: it reclaims the boxes on its host that no
// run names any more, polls a run the moment it is told its work is in, and,
// while its node holds the launch-node watch, finds the launch nodes that left
// for good.

// NudgeWhy is why a waiter is told to look at a run now.
type NudgeWhy string

const (
	// NudgeSubmitted is a run whose work was just submitted over its tool
	// bridge (`submit_work`), or whose question was just asked there
	// (`ask_and_wait`): its harness is about to end its turn and exit, so
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
// ONE SECOND, from what is left of a run once it has submitted: its harness makes
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
// its job replaced, claimed, parked or ended, held, or another node's.
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
		if manager == nil {
			return
		}
		// WHEN THE STORE WAS ASKED FOR THE ROW, which an announcement made
		// from it says ([rowAskedField]).
		read := w.now()
		run, found, err := w.pending.Get(ctx, n.TurnID)
		if err != nil {
			// The STORE did not answer, which says nothing about the run:
			// asked again at the next offset, and the pass after that.
			log.DebugContext(ctx, "sandbox_nudge_unread", "turn_id", n.TurnID, "error", err.Error())
			continue
		}
		if !found || run.LaunchID != n.LaunchID || run.Status != StatusRunning || run.Ending != nil ||
			run.SandboxID == "" || run.Held(w.now()) || !w.drivesRun(run) {
			return
		}
		if w.pollNow(ctx, manager, run, read) {
			return
		}
	}
}

// pollNow polls one run as a pass's task would — a slot of its own, the box
// claimed for it alone, bounded by [pollBound] — and reports whether it
// announced the run's completion; read is when the store was asked for the row
// run was read from. A run a pass's task is already polling is left to it.
func (w *Waiter) pollNow(ctx context.Context, manager *Manager, run PendingRun, read time.Time) bool {
	if !w.acquireSlot(ctx) {
		return false
	}
	defer func() { <-w.slots }()
	if !w.begin(run.TurnID) {
		return false
	}
	defer w.end(run.TurnID)
	taskCtx, stop := context.WithTimeout(ctx, w.pollBound)
	defer stop()
	return w.pollBounded(ctx, taskCtx, manager, run, read)
}

// goneAfter is the holder of the launch-node watch finding the launch nodes
// that left for good, and ending what their runs were waiting on: a box goes
// with its node. Once a node a run names ([NodeRoster]) has been absent for the
// host grace ([Manager.HostGrace]), a running job is recorded lost
// ([PendingStore.MarkLost], [LossLaunchNodeGone]) and announced, and a run on a
// question has its box released ([PendingStore.ExpirePause]). Never while a
// node merely restarts; an unanswering roster changes nothing; the clock is
// this waiter's own ([Waiter.forgetAbsences]). See the package doc, "Lost
// jobs".
func (w *Waiter) goneAfter(ctx context.Context, manager *Manager, runs []PendingRun) {
	now := w.now()
	byNode := map[string][]PendingRun{}
	for _, run := range runs {
		if run.LaunchNode == "" || run.Ending != nil {
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
	grace := manager.HostGrace()
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
//
// EACH WRITE OF THE RUN'S ROW IS GIVEN THE ROW'S CROSSING beyond the claim's
// window ([Waiter.rowContext]): both are compare-and-sets, which a peer's pass
// after the window makes beside it harmlessly.
func (w *Waiter) endGone(ctx context.Context, run PendingRun, since, now time.Time) {
	if run.Status != StatusRunning {
		releaseCtx, releaseDone := w.rowContext(ctx)
		released, err := w.pending.ExpirePause(releaseCtx, run.TurnID)
		releaseDone()
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
	// WHEN THE STORE WAS ASKED TO RECORD THE LOSS, which the announcement made
	// once it answered says ([rowAskedField]): a write of the run's row it
	// answered, as a read is.
	read := w.now()
	markCtx, markDone := w.rowContext(ctx)
	marked, err := w.pending.MarkLost(markCtx, run.TurnID, run.LaunchID,
		JobLoss{Cause: LossLaunchNodeGone, Node: run.LaunchNode, At: now})
	markDone()
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
				"come back within the host grace; its completion is announced so the seat's "+
				"holder carries its round on in a box of its own")
	}
	if run.Held(now) {
		// Its resume is held back for now ([PendingRun.Held]): announced
		// once the hold lifts, by a later pass.
		return
	}
	first, due := w.announcing(run)
	if !due {
		return
	}
	if err := w.publishCompletion(ctx, run, first, read); err != nil {
		w.unannounce(run.TurnID)
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
func (w *Waiter) lose(ctx context.Context, run PendingRun, cause LossCause) bool {
	loss := JobLoss{Cause: cause, Node: run.LaunchNode, At: w.now()}
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
// names any more: each box recording its run ([HostBox]) is looked up by that
// run and reclaimed where the row no longer names it ([hostBoxFree]). With only
// set, the boxes of that one turn ([NudgeReleased]); otherwise every box on the
// host, once per pass.
//
// THE HOST IS THE ONE PARTY THAT CAN: no other node reaches a box here
// ([Coordinator.reclaimBox]). Asking of the box, not of remembered runs, also
// reclaims one whose run ended while this node was stopped. RECLAIMED ONLY ON A
// FRESH READ of the run: the listing is a moment old, and a row naming the box
// again since (a relaunch continuing in it) keeps it. A listing or row that
// cannot be read reclaims nothing.
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
// backend its own record names, under a context of its own, so a pass's end
// does not cut it short. A kill that fails leaves the box listed, its record
// kept ([Provider.Kill]), for the next pass to try again.
func (w *Waiter) reclaimHostBox(ctx context.Context, manager *Manager, box HostBox, run PendingRun, found bool) {
	provider, err := manager.Provider(box.Placement)
	if err != nil {
		log.WarnContext(ctx, "sandbox_host_box_no_backend", "turn_id", box.TurnID,
			"sandbox_id", box.SandboxID, "placement", string(box.Placement), "error", err.Error())
		return
	}
	killCtx, cancel := detached(ctx)
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
