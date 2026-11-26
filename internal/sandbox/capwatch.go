package sandbox

import (
	"context"
	"time"
)

// # The turn cap's watch
//
// The node that launched a capped run counts its model requests WHILE IT RUNS
// ([Runner.Count]), from the same stream and with the same decoder its
// collection counts with, and ends its job once the count already makes that
// collection fail it as past its cap ([RequestCount.PastCap]): recorded on the
// job's record ([LaunchRecord.EndedAtTurnCap]), then announced as a passed
// deadline is, and its collection ends the job before it reads it. Without it,
// a run whose in-box enforcer died made requests until its job ended. See
// docs/concepts/code-sandbox.md, "What ends a run".

// capWatchTick is how often the watch reads each capped running run's count:
// livefeed's cadence and callsDrainTick's. MEASURED against an instant stand-in
// model, the fastest pinned harness made about 31 requests in 2 s (Cursor Agent
// about 10 a second), and no runner's in-flight allowance is above 1, so a run
// whose cap nothing in its box held runs past cap+InFlight by about one tick's
// requests at most. A TIMER, because no box (direct, container, e2b) offers a
// signal that its stream grew, and a read of a stream that did not grow costs
// one open that hands over nothing ([Counter]).
const capWatchTick = 2 * time.Second

// capWatchReads is how many runs' counts are read at once, [MaxConcurrentPolls]
// for its reason, in slots of the watch's own: a slow read holds up no poll,
// and no stuck poll holds up the watch.
const capWatchReads = MaxConcurrentPolls

// RequestCount is what a job's stream shows of its model requests so far
// ([Decoder.Requests] and its two allowances, in internal/sandbox/executor):
// the requests counted, those its runner's enforcer cannot stop once it fires,
// and the skipped lines that may each hide one.
type RequestCount struct {
	Requests, InFlight, Undecided int
}

// Exceeds reports whether the count is past turnCap by more than the requests
// in flight as an enforcer stops: a collection judges such a run past its cap
// whatever its stream says of a stop. Zero turnCap is no cap.
func (c RequestCount) Exceeds(turnCap int) bool {
	return turnCap > 0 && c.Requests > turnCap+max(c.InFlight, 0)
}

// Unjudged reports whether lines the count could not read may hide requests
// that take it past turnCap, which a collection fails as a cap it could not
// judge: each such line hides one request at most.
func (c RequestCount) Unjudged(turnCap int) bool {
	return turnCap > 0 && c.Undecided > 0 && c.Requests+c.Undecided > turnCap
}

// PastCap reports whether the count already makes the job's collection fail it
// at turnCap, whatever lines follow: the two verdicts the collection gives
// ([RequestCount.Exceeds], [RequestCount.Unjudged]), so the watch and the
// verdict never drift. Past turnCap by no more than the requests in flight is
// not past it here: the harness's own stop ends such a run, and ending it
// first would erase that stop from its stream.
func (c RequestCount) PastCap(turnCap int) bool { return c.Exceeds(turnCap) || c.Unjudged(turnCap) }

// Counter is a LIVE COUNT of the model requests one running job's stream shows
// ([Runner.Count]). Each Read takes, from a box attached for the purpose
// ([Provider.Attach]), only what the stream grew by, up to a bound per read,
// and never skips: what it leaves is taken by the next. The count it answers
// never goes down, a stream rewritten shorter included. Not safe for
// concurrent use: one reading, one reader.
type Counter interface {
	Read(ctx context.Context, box Sandbox) (RequestCount, error)
}

// capWatch is one capped running run the watch counts, for the life of its
// job on this node: dropped once a listing shows it no longer running, and
// begun again from the stream's start for a new job or after a restart.
type capWatch struct {
	run PendingRun
	// listed is when the store was asked for the row run was read from, which
	// an announcement made from it says ([rowAskedField]); since is the
	// listing ([Waiter.listings]) current when it was watched.
	listed time.Time
	since  uint64
	// counter is the job's count, begun at the first read; uncounted is a
	// runner whose stream shows its requests only once its harness exits.
	counter   Counter
	uncounted bool
	busy      bool
}

// watchable reports whether run is one the watch counts: a running job of this
// node's with a turn cap, its end decided by nothing yet.
func (w *Waiter) watchable(run PendingRun, now time.Time) bool {
	return w.drivesRun(run) && run.Status == StatusRunning && run.SandboxID != "" && run.Ending == nil &&
		run.Launch.TurnCap > 0 && !run.Launch.EndedAtTurnCap && !run.Held(now)
}

// listing begins a listing of the runs and answers its number, which
// [Waiter.track] is handed once the listing is read.
func (w *Waiter) listing() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listings++
	return w.listings
}

// track makes the watch count every watchable run of a listing asked at
// listed, numbered gen, and stop counting every other run, but for one watched
// after that listing began ([Waiter.Watch]), which the listing may predate.
func (w *Waiter) track(runs []PendingRun, listed time.Time, gen uint64) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		if w.watchable(run, now) {
			seen[run.TurnID] = true
			w.watchLocked(run, listed)
		}
	}
	for turnID, c := range w.watched {
		if !seen[turnID] && c.since < gen {
			delete(w.watched, turnID)
		}
	}
}

// watchLocked counts run, as of a row read at listed, keeping the count of its
// job where it is watched already. w.mu is held.
func (w *Waiter) watchLocked(run PendingRun, listed time.Time) {
	if c, ok := w.watched[run.TurnID]; ok && c.run.LaunchID == run.LaunchID {
		c.run, c.listed, c.since = run, listed, w.listings
		return
	}
	w.watched[run.TurnID] = &capWatch{run: run, listed: listed, since: w.listings}
}

// Watch tells this waiter that this node has just opened turnID's run to its
// poll, so a run with a turn cap is counted from the watch's next tick rather
// than from its next pass's listing, up to an interval later: one read of the
// run's row, made before it returns, so the run is watched once it has. A row
// the store did not answer for is left to the next pass's listing.
func (w *Waiter) Watch(ctx context.Context, turnID string) {
	listed := w.now()
	run, found, err := w.pending.Get(ctx, turnID)
	if err != nil {
		log.DebugContext(ctx, "sandbox_cap_watch_unread", "turn_id", turnID, "error", err.Error())
		return
	}
	if !found || !w.watchable(run, w.now()) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watchLocked(run, listed)
}

// watchLoop reads every watched run's count each [capWatchTick] until ctx ends.
// A round does not wait for its reads: a run whose read is still going is
// passed over by the next ([capWatch.busy]).
func (w *Waiter) watchLoop(ctx context.Context) {
	defer close(w.watchEnded)
	ticker := time.NewTicker(capWatchTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		w.watchRound(ctx)
	}
}

// WatchTick runs one round of the cap watch TO ITS END, every watched run's
// count read once, and returns how many runs it ended. The loop runs the same
// round on [capWatchTick] without waiting for it; exported so a test drives
// one round deterministically.
func (w *Waiter) WatchTick(ctx context.Context) int {
	r := w.watchRound(ctx)
	r.tasks.Wait()
	return int(r.fired.Load())
}

// watchRound starts a read of every watched run's count not being read now, in
// [capWatchReads] slots, and returns as soon as the reads are handed out.
func (w *Waiter) watchRound(ctx context.Context) *round {
	r := &round{}
	manager := w.manager()
	if manager == nil {
		return r
	}
	w.mu.Lock()
	var due []*capWatch
	for _, c := range w.watched {
		if !c.busy && !c.uncounted {
			c.busy = true
			due = append(due, c)
		}
	}
	w.mu.Unlock()
	w.spawnRead(r, func() {
		for i, c := range due {
			select {
			case w.watchSlots <- struct{}{}:
			case <-ctx.Done():
				w.unbusy(due[i:]...)
				return
			}
			w.spawnRead(r, func() {
				defer func() { <-w.watchSlots }()
				defer w.unbusy(c)
				taskCtx, stop := context.WithTimeout(ctx, w.pollBound)
				defer stop()
				if w.countOne(ctx, taskCtx, manager, c) {
					r.fired.Add(1)
				}
			})
		}
	})
	return r
}

// spawnRead runs f as one of a watch round's tasks, which the round's caller
// and the waiter's stop wait for ([Waiter.watchReads]).
func (w *Waiter) spawnRead(r *round, f func()) {
	r.tasks.Add(1)
	w.watchReads.Add(1)
	go func() {
		defer w.watchReads.Done()
		defer r.tasks.Done()
		f()
	}()
}

// unbusy releases each run a round took for a read.
func (w *Waiter) unbusy(watched ...*capWatch) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range watched {
		c.busy = false
	}
}

// countOne reads one watched run's count within taskCtx and ends its job where
// the count is past its cap ([Waiter.endAtTurnCap]), reporting whether it did.
// A box not reached and a read that failed are the poll's to judge, never the
// watch's: the next tick reads again from where the count stands.
func (w *Waiter) countOne(ctx, taskCtx context.Context, manager *Manager, c *capWatch) bool {
	w.mu.Lock()
	run, listed, counter := c.run, c.listed, c.counter
	w.mu.Unlock()
	if counter == nil {
		runner, err := manager.RunnerFor(run.Harness)
		if err != nil {
			return false
		}
		counter = runner.Count(run.Handle())
		w.mu.Lock()
		c.counter, c.uncounted = counter, counter == nil
		w.mu.Unlock()
		if counter == nil {
			return false
		}
	}
	box, _, err := manager.Attach(taskCtx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		log.DebugContext(ctx, "sandbox_cap_watch_unreached", "turn_id", run.TurnID, "error", err.Error())
		return false
	}
	count, err := counter.Read(taskCtx, box)
	if err != nil {
		log.DebugContext(ctx, "sandbox_cap_watch_unread", "turn_id", run.TurnID, "error", err.Error())
		return false
	}
	if !count.PastCap(run.Launch.TurnCap) {
		return false
	}
	return w.endAtTurnCap(ctx, taskCtx, run, listed, count)
}

// endAtTurnCap records on run's job's record that its stream showed it past its
// turn cap while it ran ([PendingStore.MarkEndedAtTurnCap]), before anything is
// asked of the job, and announces its completion as a poll past a deadline
// does: its collection then ends the job and fails the run at its cap
// ([Coordinator.readBox]). A record that does not land is asked for again by
// the next tick; a row that moved on is the poll's.
func (w *Waiter) endAtTurnCap(ctx, taskCtx context.Context, run PendingRun, listed time.Time,
	count RequestCount) bool {

	row, cancel := w.rowContext(taskCtx)
	marked, err := w.pending.MarkEndedAtTurnCap(row, run.TurnID, run.LaunchID)
	cancel()
	if err != nil {
		log.WarnContext(ctx, "sandbox_turn_cap_end_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "error", err.Error(),
			"detail", "the run's stream shows it past its turn cap, and its record could not say so; "+
				"the next tick of the watch asks again before its job is ended")
		return false
	}
	w.unwatch(run)
	if !marked {
		return false
	}
	log.InfoContext(ctx, "sandbox_run_past_turn_cap", "turn_id", run.TurnID, "launch_id", run.LaunchID,
		"agent", run.AgentHandle, "turn_cap", run.Launch.TurnCap, "requests", count.Requests,
		"undecided", count.Undecided,
		"detail", "the run's stream shows it past its turn cap while it works; the run is ended and "+
			"its turn resumed with what it did")
	return w.announce(ctx, run, listed)
}

// unwatch stops counting run's job.
func (w *Waiter) unwatch(run PendingRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.watched[run.TurnID]; ok && c.run.LaunchID == run.LaunchID {
		delete(w.watched, run.TurnID)
	}
}
