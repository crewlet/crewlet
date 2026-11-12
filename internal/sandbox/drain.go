package sandbox

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
)

// Every collection of a run waits for the calls its run's log holds in flight
// before it reads what the run did.
//
// # Why a collection waits
//
// A run's turn settles its outcome by the kinds of change its bridged calls
// committed, read off the run's log as the run is collected ([BridgeLog.Changes]),
// and its reviewer, its tool log and the account of an ending read the same log.
// A bridged call runs on its opening's own context, which nothing cancels, so it
// goes on after its harness stopped waiting for it, after its job's session on
// the bridge ended (internal/api/mcpbridge, "An opening ends without waiting for
// its calls"), and after its seat moved to another node. A collection that read
// the log while such a call ran read it without what the call did: the call's
// update landed after the read, in no collection of the run, or on no record
// once the run's ending had deleted it — a work item the call filed, a message
// it posted, in no outcome of the turn. Where that call made the turn's only
// change, the turn was booked as one that changed nothing.
//
// # On the run's shared record, so any node may collect
//
// Every call's entry is on the run's log before the call runs
// ([BridgeLog.InFlight]), and leaves it in the write that records what the call
// did. So a collection ENDS THE JOB'S SESSION TO NEW CALLS ([CoordinatorOptions.Ended]),
// then waits until the log holds no entry of the run's round, and only then
// reads ([Coordinator.drainCalls]). It waits on the log itself, which every node
// reads, never on the bridge's own registry of the calls it runs: that is the
// memory of the node that runs them, and the collection after a seat was handed
// on is its next holder's, which may be another node.
//
// # What ends the wait
//
// Each entry is waited for until the FIRST of:
//
//   - its update, which takes it out of the log — read off a watch of the log
//     ([PendingStore.WatchLog]), never a poll: the log is a run's largest value,
//     read and written whole, and a wait can last as long as a call's bound;
//   - the turn's wall-clock cap ([PendingRun.Deadline]), where the turn has one;
//   - the call's own bound ([BridgeCall.Until]: when the bridge took it, its
//     tool's bound and the bridge's margin), which every entry carries, since
//     the turn's cap is zero for every turn but a scheduled fire: without it an
//     entry whose update the store refused would hold a collection for ever;
//   - its runner gone: the process running the call ([BridgeCall.Runner]) holds
//     its taker lease at no epoch, read in the coordination store
//     ([Coordinator.runnerGone]) as the runner first appears in the log and
//     again every [callsDrainTick], the way a capture's taker is read as
//     stopped — never by a clock. A crashed node's calls are given up within a
//     lease's TTL of its last renewal rather than at their bound; a node that
//     drains holds its lease until its calls are recorded, and is waited for.
//
// An entry still in flight when the wait stops is named at warning
// (`sandbox_calls_unfinished`), and the read that follows reads it as a call
// whose outcome is unknown: the resumed turn's record lists it, failed, in words
// that say its effects may stand (internal/agent/runner's UnfinishedNote), the
// turn's call log counts it by its identity, an ending reads delivery as unknown
// beside it, and its update, landing later while the record stands, takes the
// entry out and lists the call with its outcome for every later reader. The
// ending's delete names any entry still in flight as it removes the record
// (`sandbox_run_deleted_with_calls_in_flight`), whose update then finds no record.
//
// # Where nothing may wait
//
// An ending reached on a frame somebody waits on — a completion's handler, a
// seat's acquisition — waits for nothing that is not the frame's own, so it
// hands a run whose log holds a call still waited for to this node's retry of
// the ending ([Coordinator.drainEnding], [errAskAway]), which waits. A seat's
// retirement ends every run of the seat in one loop and gives the seat up after
// it, this node's retry of an ending with it ([Coordinator.ReleaseSeat]), so it
// leaves such a run to its own next tick, and answers it as waiting rather than
// as a failure ([Coordinator.RetireSeat], [SeatRunsWaiting]).
//
// WHAT IT COSTS: a collection holds the run's seat for as long as it waits, as
// the resumed turn does after it, up to the first of those bounds; one watch of
// the log while it waits, and one read of each runner's lease as it first
// appears and every [callsDrainTick] after.

// callsDrainTick is how often a collection waiting on a run's calls in flight
// reads the taker lease of each call's runner again: TWO SECONDS, the cadence a
// wait for a capture reads its taker's lease at (captureWaitTick), for the same
// read — a wait on another process's work reads that process's lease each pass,
// one coordination read a runner, so a crash is named within a pass of its lease
// lapsing. None before: no collection waited for a call.
const callsDrainTick = 2 * time.Second

// callsRewatchBase and callsRewatchCeiling space the watches of a run's log a
// wait makes again after one broke: from a tenth of a second, doubling to five,
// jittered by a fifth ([callsRewatchJitter]) — the note watch's spacing
// (internal/engine's steerRewatchBase and steerRewatchCeiling), for the same
// case of a watch that stopped hearing. None before.
const (
	callsRewatchBase    = 100 * time.Millisecond
	callsRewatchCeiling = 5 * time.Second
	callsRewatchJitter  = 0.2
)

// callDigestPrefix is how much of a call's digest a line naming it carries:
// eight hex digits, as the bridge ledger's lines name a call (internal/engine's
// nameDigestPrefix).
const callDigestPrefix = 8

// SeatRunsWaiting is what a seat's retirement left for its next tick
// ([Coordinator.RetireSeat]): the runs whose tool log holds a call a collection
// would still wait for, named by those calls. Its zero value is a retirement
// that left no run waiting.
//
// NOT AN ERROR, so it is answered beside the retirement's error and never inside
// it. The call is running, which is no fault of the retirement's or of the
// store's, and the retirement's next tick ends the run once the call is
// recorded, or once the last of its bounds passes or its process stops; an
// error is a run the retirement could not end, which somebody may have to act
// on. Folded into the error, a wait failed the mailbox duty's tick at warning on
// every tick it lasted, and a real failure beside it was one more member of the
// same joined error, which no caller could tell from the wait without walking
// it. Two answers, so a retirement can give both at once and its caller reads
// each for what it is.
type SeatRunsWaiting struct {
	// Calls names each call still waited for, as the lines naming a call in
	// flight do — its tool, its place and the opening of its digest — with the
	// turn of the run it is a call of. Empty where no run waits.
	Calls []string
	// Until is the latest instant any of them is still waited for: for each,
	// the earlier of its turn's cap and its own bound, whichever is set
	// ([waitEnds]). Zero where no run waits, and where one of the calls has
	// neither, which only its update or its process's end stops a collection
	// waiting for — an entry the bridge writes always has its bound.
	Until time.Time
}

// seatWait gathers, run by run, what a retirement leaves waiting.
type seatWait struct {
	waiting SeatRunsWaiting
	// unbounded is whether a call it names has no bound at all, which makes
	// the whole wait's bound none.
	unbounded bool
}

// add names each of calls, run's calls a collection would still wait for, and
// takes its bound into the wait's.
func (w *seatWait) add(run PendingRun, calls []BridgeCall) {
	for _, call := range calls {
		w.waiting.Calls = append(w.waiting.Calls, fmt.Sprintf("%s, in turn %s", inFlightName(call), run.TurnID))
		switch ends := waitEnds(run, call); {
		case ends.IsZero():
			w.unbounded = true
		case ends.After(w.waiting.Until):
			w.waiting.Until = ends
		}
	}
}

// answer is the wait as the retirement answers it.
func (w *seatWait) answer() SeatRunsWaiting {
	if w.unbounded {
		return SeatRunsWaiting{Calls: w.waiting.Calls}
	}
	return w.waiting
}

// waitEnds is the instant past which no collection waits for call of run,
// whatever becomes of it ([Coordinator.stillWaited]): the earlier of the turn's
// cap and the call's own bound, whichever is set; zero where neither is, and
// only the call's update or its process's end stops the wait.
func waitEnds(run PendingRun, call BridgeCall) time.Time {
	switch {
	case run.Deadline.IsZero():
		return call.Until
	case call.Until.IsZero() || run.Deadline.Before(call.Until):
		return run.Deadline
	default:
		return call.Until
	}
}

// unfinishedCall is an entry a wait stopped waiting for, and why.
type unfinishedCall struct {
	call BridgeCall
	why  string
}

// drainCalls ends job run's session on the bridge to new calls
// ([CoordinatorOptions.Ended]) and waits for every call of its round its log
// holds in flight ([Coordinator.awaitCalls]), naming each it stopped waiting for
// (`sandbox_calls_unfinished`); and reports whether the log held any, so the
// caller reads what their updates wrote to the run's record — their spend —
// again. An error is the wait's: its context ended, which the collection
// judges as it judges every read its context ended; or the store could not be
// watched ([ErrStoreSilent]), which hands the collection back uncounted, since
// the store's silence is no failure of the box ([Coordinator.collectFailed]).
func (c *Coordinator) drainCalls(ctx context.Context, run PendingRun) (bool, error) {
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	began := c.now()
	left, held, err := c.awaitCalls(ctx, run)
	if err != nil {
		return held > 0, err
	}
	if held > 0 {
		log.InfoContext(ctx, "sandbox_calls_drained", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"calls", held, "unfinished", len(left), "waited_s", c.now().Sub(began).Seconds())
	}
	c.nameUnfinished(ctx, run, left)
	return held > 0, nil
}

// nameUnfinished names, at warning, each call a collection of run stopped
// waiting for while it was still in flight, with why.
func (c *Coordinator) nameUnfinished(ctx context.Context, run PendingRun, left []unfinishedCall) {
	for _, u := range left {
		log.WarnContext(ctx, "sandbox_calls_unfinished", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"call", inFlightName(u.call), "runner", u.call.Runner, "why", u.why,
			"detail", "the collection stopped waiting for a call still in flight and reads it as one "+
				"whose outcome is unknown: what it committed is in no outcome of the turn unless its "+
				"update lands while the run's record stands")
	}
}

// awaitCalls waits until run's log holds no entry of its round in flight
// ([BridgeLog.InFlight]), or until each it still holds is one no collection
// waits for any longer — its own bound passed, the turn's cap passed, or its
// runner gone — and answers those, with how many entries it saw in flight at
// any point. See this file's doc for what ends the wait.
//
// THE WATCH IS ITS OWN, started and stopped here: a watch that stops hearing is
// watched again, after a jittered pause, and the store's watch reads the log
// again as it begins ([PendingStore.WatchLog]), so nothing written between the
// two is missed. A watch that cannot be made is an error: a store that cannot be
// read could not have answered the read the collection makes next either. It is
// the store's silence ([ErrStoreSilent]), the first watch's and one made again
// alike, and never the box's.
func (c *Coordinator) awaitCalls(ctx context.Context, run PendingRun) ([]unfinishedCall, int, error) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	views, err := c.pending.WatchLog(ctx, run.TurnID)
	if err != nil {
		return nil, 0, storeSilent(ctx, fmt.Errorf("sandbox: watching the log of run %s for its calls in flight: %w",
			run.TurnID, err))
	}
	tick := time.NewTicker(callsDrainTick)
	defer tick.Stop()
	var (
		current []BridgeCall
		known   bool
		seen    = map[string]bool{}
		read    = map[string]bool{}
		gone    = map[string]bool{}
		rewatch int
	)
	for {
		if known {
			waiting, left := c.stillWaited(current, run, gone)
			if len(waiting) == 0 {
				return left, len(seen), nil
			}
		}
		timer := time.NewTimer(c.nextDue(current, run))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, len(seen), ctx.Err()
		case view, ok := <-views:
			timer.Stop()
			if !ok {
				if views, err = c.watchAgain(ctx, run.TurnID, rewatch); err != nil {
					return nil, len(seen), err
				}
				rewatch++
				continue
			}
			rewatch = 0
			current, known = callsOf(view, run), true
			for _, call := range current {
				seen[entryKey(call)] = true
			}
			// A RUNNER THE WAIT HAS NOT READ YET is read at once, so a call
			// whose process is already gone holds the wait no tick.
			var unread []string
			for _, runner := range runnersOf(current) {
				if !read[runner] {
					unread = append(unread, runner)
				}
			}
			c.readRunners(ctx, unread, read, gone)
		case <-tick.C:
			timer.Stop()
			c.readRunners(ctx, runnersOf(current), read, gone)
		case <-timer.C:
		}
	}
}

// watchAgain watches turnID's log again once its watch stopped hearing, after a
// pause doubling from [callsRewatchBase] to [callsRewatchCeiling] with each
// attempt in a row, jittered.
func (c *Coordinator) watchAgain(ctx context.Context, turnID string, attempt int) (<-chan LogView, error) {
	pause := time.NewTimer(backoff.Jitter(backoff.Doubling(attempt, callsRewatchBase, callsRewatchCeiling),
		callsRewatchJitter))
	select {
	case <-ctx.Done():
		pause.Stop()
		return nil, ctx.Err()
	case <-pause.C:
	}
	views, err := c.pending.WatchLog(ctx, turnID)
	if err != nil {
		return nil, storeSilent(ctx, fmt.Errorf("sandbox: watching the log of run %s again for its calls in "+
			"flight: %w", turnID, err))
	}
	return views, nil
}

// readRunners reads the taker lease of each of runners not already read gone,
// marking each it read in read and each whose process stopped in gone. A read
// that fails is UNKNOWN, never gone, and leaves the runner unread for the next
// pass to ask again.
func (c *Coordinator) readRunners(ctx context.Context, runners []string, read, gone map[string]bool) {
	for _, runner := range runners {
		if gone[runner] {
			continue
		}
		stopped, err := c.runnerGone(ctx, runner)
		if err != nil {
			continue
		}
		read[runner] = true
		if stopped {
			gone[runner] = true
		}
	}
}

// stillWaited splits the calls in flight into those a collection still waits
// for and those it stops waiting for, each with why, read as the clock stands
// now: the turn's cap passed, the call's own bound passed, or its runner gone.
func (c *Coordinator) stillWaited(inFlight []BridgeCall, run PendingRun, gone map[string]bool,
) (waiting []BridgeCall, left []unfinishedCall) {
	now := c.now()
	for _, call := range inFlight {
		switch {
		case !run.Deadline.IsZero() && !now.Before(run.Deadline):
			left = append(left, unfinishedCall{call: call, why: "the turn's cap passed"})
		case !call.Until.IsZero() && !now.Before(call.Until):
			left = append(left, unfinishedCall{call: call, why: "its own bound passed"})
		case call.Runner != "" && gone[call.Runner]:
			left = append(left, unfinishedCall{call: call, why: "its process stopped"})
		default:
			waiting = append(waiting, call)
		}
	}
	return waiting, left
}

// nextDue is how long a wait sleeps before it looks again unless the log
// changes or a lease is read first: until the earliest bound among the calls in
// flight — the turn's cap, or a call's own — and never past [callsDrainTick].
func (c *Coordinator) nextDue(inFlight []BridgeCall, run PendingRun) time.Duration {
	now := c.now()
	due := callsDrainTick
	at := func(t time.Time) {
		if !t.IsZero() {
			due = min(due, max(t.Sub(now), 0))
		}
	}
	if len(inFlight) > 0 {
		at(run.Deadline)
	}
	for _, call := range inFlight {
		at(call.Until)
	}
	return due
}

// runnerGone reports whether the process running a call — the incarnation its
// entry names ([BridgeCall.Runner]) — has stopped: it holds its taker lease at
// no epoch. A lease that lapsed and was claimed again at a new epoch is the same
// process, still running its calls. One coordination read; an error is
// UNKNOWN, never gone, and a coordinator with no lease store knows nothing of
// it, so its wait ends at the call's bound or the turn's cap.
func (c *Coordinator) runnerGone(ctx context.Context, runner string) (bool, error) {
	if c.taker == nil || runner == "" {
		return false, nil
	}
	lease, err := c.taker.leases.Get(ctx, coord.TakerResource(runner))
	if err != nil {
		return false, err
	}
	return lease == nil || lease.Owner != runner, nil
}

// callsOf is the calls in flight a state of run's log holds of the run's round:
// none where there is no log, or where the log is on another round and another
// job — a log a later round opened afresh, whose calls are not this round's.
func callsOf(view LogView, run PendingRun) []BridgeCall {
	logged := view.Log
	if !view.Found || (logged.Launch != run.LaunchID && (run.Round == "" || logged.Round != run.Round)) {
		return nil
	}
	return slices.Clone(logged.InFlight)
}

// runnersOf is the distinct runners calls name, none for an entry naming none.
func runnersOf(calls []BridgeCall) []string {
	var out []string
	for _, call := range calls {
		if call.Runner != "" && !slices.Contains(out, call.Runner) {
			out = append(out, call.Runner)
		}
	}
	return out
}

// entryKey is a call's identity in flight, as a wait counts the calls it saw.
func entryKey(call BridgeCall) string {
	return fmt.Sprintf("%d|%s|%s", call.Seq, call.StartedAt.UTC().Format(time.RFC3339Nano), call.Name)
}

// inFlightName names a call in flight for a line about it: its tool, its place
// and the opening of its digest, or that its entry keeps none.
func inFlightName(call BridgeCall) string {
	identity := "no digest kept"
	if call.Digest != "" {
		identity = "digest " + call.Digest[:min(len(call.Digest), callDigestPrefix)]
	}
	return fmt.Sprintf("%s at place %d, %s", call.Name, call.Seq, identity)
}

// callsInFlight is run's calls of its round still in flight, as one read of its
// log finds them: what an ending that may not wait reads before it decides, and
// what the ending's delete names.
func (c *Coordinator) callsInFlight(ctx context.Context, run PendingRun) ([]BridgeCall, error) {
	logged, err := c.pending.TurnLog(ctx, run)
	if err != nil {
		return nil, err
	}
	return callsOf(LogView{Log: logged, Found: true}, run), nil
}

// callsStillRunning ends job run's session on the bridge to new calls
// ([CoordinatorOptions.Ended]) and splits, off one read of its log and one read
// of the lease of each process running one of them, the calls of its round in
// flight into those a collection would still wait for ([Coordinator.stillWaited])
// and those it would not: what an ending reached where it may not wait asks
// before it decides the turn's end on the log, and a seat's retirement before it
// ends a run. An error is the log's read.
func (c *Coordinator) callsStillRunning(ctx context.Context, run PendingRun,
) ([]BridgeCall, []unfinishedCall, error) {
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	held, err := c.callsInFlight(ctx, run)
	if err != nil {
		return nil, nil, err
	}
	gone := map[string]bool{}
	c.readRunners(ctx, runnersOf(held), map[string]bool{}, gone)
	waiting, left := c.stillWaited(held, run, gone)
	return waiting, left, nil
}

// drainEnding waits, for an ending about to decide its turn's end on run's log,
// for the calls the log holds in flight ([Coordinator.drainCalls]) where mayAsk
// says this attempt is the ending's own retry, which may wait; and where it may
// not, hands a run whose log holds a call still waited for to that retry
// ([errAskAway]), which waits, as an ending hands every step that waits on what
// is not its frame's own.
//
// A LOG THE STORE CANNOT READ is the decision's to read again: it hands the
// engine why the log was unread ([RunCalls.Unread]), as it did before any
// ending waited, and nothing here knows of a call to wait for.
func (c *Coordinator) drainEnding(ctx context.Context, run PendingRun, mayAsk bool) error {
	if mayAsk {
		if _, err := c.drainCalls(ctx, run); err != nil {
			return fmt.Errorf("sandbox: ending run %s, whose calls in flight could not be waited for: %w",
				run.TurnID, err)
		}
		return nil
	}
	waiting, left, err := c.callsStillRunning(ctx, run)
	if err == nil && len(waiting) > 0 {
		return errAskAway
	}
	if err == nil {
		c.nameUnfinished(ctx, run, left)
	}
	return nil
}

// nameDeletedInFlight names, at warning, every call of run's round its log
// still holds in flight as the ending deletes the run's record, which takes the
// log with it: such a call's update finds no record, and this line is what is
// left of the call that ran. A log the store cannot read names nothing, and
// costs the delete nothing.
func (c *Coordinator) nameDeletedInFlight(ctx context.Context, run PendingRun) {
	held, err := c.callsInFlight(ctx, run)
	if err != nil {
		return
	}
	for _, call := range held {
		log.WarnContext(ctx, "sandbox_run_deleted_with_calls_in_flight", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "call", inFlightName(call), "runner", call.Runner,
			"detail", "the run's ending deleted its record with this call still in flight: its outcome "+
				"is in no outcome of the turn, and its update finds no record of the run")
	}
}
