package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run parked by `wait_for_run` is carried on by the node holding its seat,
// once the run it waits for leaves its seat's slots (ADR-0176): a watch per
// seat with such a run reads the seat's runs, and for each wait whose run has
// ended or parked puts a wake on the seat's control topic
// ([types.ExecutorWaitOver]), on which the holder carries the run on from its
// row, told how that run went ([Engine.waitedFor]), never as an answer
// (internal/sandbox's waitover.go). A run that leaves on this node nudges every
// watch at once; one that leaves on another is found at the backstop. A job
// parked on its own bridged calls is woken by the same watch, at once: its
// calls have returned, or are past waiting for, by the time it parks.

// runWaitBackstop is how long a watch waits for a nudge before it reads its
// seat's waits again. A run leaving a seat on this node nudges at once, so the
// backstop is for one that left on another node (a run of another seat, or a
// nudge lost to a restart): A MINUTE beside a resume that relaunches a box,
// and one listing of the seat's runs plus one read per wait it finds.
const runWaitBackstop = time.Minute

// runWaitReportBytes bounds the report a wait's answer carries: the first
// lines of what the run waited for's reviewer said landed, at most TWO KIB, a
// screen's worth the resumed run reads before it checks the branch or item
// itself; the whole account is on the executor log and its work item.
const runWaitReportBytes = 2 << 10

// runWaits is this node's watches, by seat handle.
type runWaits struct {
	mu      sync.Mutex
	running map[string]*waitWatch
	last    map[string]<-chan struct{}
	// delivered is when each wait (turn and job) was last answered, so a
	// watch answers a wait once per backstop while its answer is recorded.
	delivered map[string]time.Time
	stopped   bool

	// deliver puts the wake carrying a run on, its wait over, on its seat's
	// control topic; nil is the fleet's answer desk ([Engine.waitDesk]), and a
	// case hands its own.
	deliver func(ctx context.Context, run sandbox.PendingRun) error
}

// waitWatch is one seat's watch: its cancel, its end and its nudge.
type waitWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	nudge  chan struct{}
}

// poke asks w for a read, coalescing with one already asked.
func (w *waitWatch) poke() {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// watchWaits starts handle's watch where none runs, so a wait parked on the
// seat is answered once its run leaves; a watch started while the last is
// ending waits for it, so no two read one seat at once. The watch outlives
// ctx's cancellation, which belongs to the park or recovery that started it,
// and ends with the engine ([Engine.stopRunWaits]).
func (e *Engine) watchWaits(ctx context.Context, handle string) {
	waits := &e.runWaits
	waits.mu.Lock()
	defer waits.mu.Unlock()
	if w, running := waits.running[handle]; running {
		w.poke()
		return
	}
	if waits.stopped || handle == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	next := &waitWatch{cancel: cancel, done: make(chan struct{}), nudge: make(chan struct{}, 1)}
	if waits.running == nil {
		waits.running, waits.last = map[string]*waitWatch{}, map[string]<-chan struct{}{}
	}
	prev := waits.last[handle]
	waits.running[handle], waits.last[handle] = next, next.done
	go e.runWaitWatch(ctx, handle, next, prev)
}

// nudgeWaits asks every watch for a read: a run on this node left its seat's
// slots, and any wait may be on it.
func (e *Engine) nudgeWaits() {
	e.runWaits.mu.Lock()
	defer e.runWaits.mu.Unlock()
	for _, w := range e.runWaits.running {
		w.poke()
	}
}

// stopRunWaits stops every watch and waits for each to end; none starts after.
func (e *Engine) stopRunWaits() {
	waits := &e.runWaits
	waits.mu.Lock()
	waits.stopped = true
	for _, w := range waits.running {
		w.cancel()
	}
	waits.running = nil
	ends := slices.Collect(maps.Values(waits.last))
	waits.mu.Unlock()
	for _, end := range ends {
		<-end
	}
}

// runWaitWatch is w's goroutine: once prev has ended, it answers handle's
// waits at once, then on every nudge and at least every [runWaitBackstop],
// until the seat holds no wait or ctx ends.
func (e *Engine) runWaitWatch(ctx context.Context, handle string, w *waitWatch, prev <-chan struct{}) {
	defer close(w.done)
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			return
		}
	}
	timer := time.NewTimer(runWaitBackstop)
	defer timer.Stop()
	for {
		waiting, err := e.answerWaits(ctx, handle)
		if err != nil {
			log.WarnContext(ctx, "seat_run_waits_unread", "seat", handle, "error", err.Error(),
				"detail", "the seat's runs could not be read, so no wait on them is answered until the next read")
		}
		if err == nil && waiting == 0 && e.endWatch(handle, w) {
			return
		}
		timer.Reset(runWaitBackstop)
		select {
		case <-ctx.Done():
			return
		case <-w.nudge:
		case <-timer.C:
		}
	}
}

// endWatch removes w as handle's watch, reporting false where a nudge came in
// since its last read, which it reads first.
func (e *Engine) endWatch(handle string, w *waitWatch) bool {
	waits := &e.runWaits
	waits.mu.Lock()
	defer waits.mu.Unlock()
	select {
	case <-w.nudge:
		return false
	default:
	}
	if waits.running[handle] == w {
		delete(waits.running, handle)
	}
	return true
}

// answerWaits answers every wait of handle's runs whose run has left its
// seat's slots, and reports how many of its runs still wait.
func (e *Engine) answerWaits(ctx context.Context, handle string) (int, error) {
	rt := e.sandbox.Load()
	if rt == nil {
		return 0, nil
	}
	runs, err := rt.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return 0, err
	}
	byTurn := make(map[string]sandbox.PendingRun, len(runs))
	for _, run := range runs {
		byTurn[run.TurnID] = run
	}
	waiting := 0
	for _, run := range runs {
		if !waitsNow(run) {
			if run.EngineAnswers() {
				e.runWaits.forget(run)
			}
			if run.WaitsFor != nil {
				e.Status().Dequeue(context.WithoutCancel(ctx), handle, []string{runWaitWake(run.TurnID, run.LaunchID)})
			}
			continue
		}
		waiting++
		if run.WaitsOnOwnCalls() {
			// ITS CALLS HAVE RETURNED, or are past waiting for: its collection
			// waited for them before the job parked (internal/sandbox's
			// callwait.go), so a parked job is carried on at once.
			if e.runWaits.due(run, e.now()) {
				e.carryCallWaitOn(ctx, run)
			}
			continue
		}
		waited, left := e.waitedAmong(ctx, run, byTurn)
		if left && e.runWaits.due(run, e.now()) {
			e.carryWaitOn(ctx, run, waited)
		}
	}
	return waiting, nil
}

// waitedFor is [sandbox.CoordinatorOptions.WaitedFor]: how the run run waits
// for left its seat's slots, read now, false while it holds one.
func (e *Engine) waitedFor(ctx context.Context, run sandbox.PendingRun) (types.RunWaited, bool) {
	return e.waitedAmong(ctx, run, nil)
}

// waitedAmong is [Engine.waitedFor], its target found among byTurn, the
// seat's runs as just listed, where it is there, else read.
func (e *Engine) waitedAmong(ctx context.Context, run sandbox.PendingRun, byTurn map[string]sandbox.PendingRun,
) (types.RunWaited, bool) {
	if run.WaitsFor == nil {
		return types.RunWaited{}, false
	}
	target, found := byTurn[run.WaitsFor.Turn]
	if !found {
		rt := e.sandbox.Load()
		if rt == nil {
			return types.RunWaited{}, false
		}
		var err error
		if target, found, err = rt.pending.Get(ctx, run.WaitsFor.Turn); err != nil {
			log.WarnContext(ctx, "seat_run_wait_unread", "turn_id", run.TurnID, "target", run.WaitsFor.Turn,
				"error", err.Error(), "detail", "the run waited for could not be read; asked again at the next read")
			return types.RunWaited{}, false
		}
	}
	return e.waitedOf(ctx, run, target, found)
}

// waitsNow reports whether run is parked by `wait_for_run`, or on its own
// calls, with no answer yet.
func waitsNow(run sandbox.PendingRun) bool {
	return run.EngineAnswers() && run.Answer == nil && run.Ending == nil &&
		slices.Contains(sandbox.Awaiting, run.Status)
}

// waitKey names one wait: the run and the job that parked on it.
func waitKey(run sandbox.PendingRun) string { return run.TurnID + "/" + run.LaunchID }

// due reports whether run's wait may be answered now, and marks it answered:
// once per [runWaitBackstop] while its answer has not been recorded.
func (w *runWaits) due(run sandbox.PendingRun, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := waitKey(run)
	if at, ok := w.delivered[key]; ok && now.Sub(at) < runWaitBackstop {
		return false
	}
	if w.delivered == nil {
		w.delivered = map[string]time.Time{}
	}
	w.delivered[key] = now
	return true
}

// forget drops what was delivered to run's wait: it waits no longer.
func (w *runWaits) forget(run sandbox.PendingRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.delivered, waitKey(run))
}

// waitedOf is how the run run waits for left its seat's slots, and false
// where it still holds one: target is its row where found. One whose ending
// is decided has ended, read off the end its row owes; one parked has parked;
// one whose row is gone has ended as the executor log says, or is gone with
// nothing said. Its report is told only for a run of the waiting run's
// requester ([sandbox.RunWait.SameRequester]).
func (e *Engine) waitedOf(ctx context.Context, run, target sandbox.PendingRun, found bool) (types.RunWaited,
	bool) {
	wait := run.WaitsFor
	out := types.RunWaited{Turn: wait.Turn, Seat: wait.Seat, Pushed: types.RunPushedUnknown}
	if !found {
		ending, ok := e.loggedEnd(ctx, wait.Seat, wait.Turn)
		if !ok {
			out.Left = types.RunLeftGone
			return out, true
		}
		out.Left, out.Outcome = types.RunLeftEnded, string(ending.Outcome)
		out.Report = waitReport(wait, ending)
		return out, true
	}
	switch {
	case target.Ending != nil:
		var ending executorkeep.Ending
		if len(target.OwedEnd) == 0 || json.Unmarshal(target.OwedEnd, &ending) != nil {
			// DECIDED, NOT YET OWED: read again at the next nudge, once the
			// ending has written how it went.
			return out, false
		}
		out.Left, out.Outcome = types.RunLeftEnded, string(ending.Outcome)
		out.Report = waitReport(wait, ending)
	case slices.Contains(sandbox.Holding, target.Status), target.WaitsOnOwnCalls():
		// A RUN PARKED ON ITS OWN CALLS has not left: the engine answers that
		// park at once, and the run carries straight on (internal/sandbox's
		// callwait.go).
		return out, false
	default:
		out.Left, out.Outcome = types.RunLeftParked, string(sandbox.StageOf(target))
	}
	out.Branch = target.Branch
	out.Pushed, out.Refs = e.pushedBy(ctx, target)
	return out, true
}

// loggedEnd is how turn of seat ended as the executor log records it, false
// where it records none this node can read.
func (e *Engine) loggedEnd(ctx context.Context, seat, turn string) (executorkeep.Ending, bool) {
	if e.router == nil {
		return executorkeep.Ending{}, false
	}
	read, err := e.router.ExecutorReads().Session(ctx, seat, turn, executordomain.RelaunchRead())
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return executorkeep.Ending{}, false
	case err != nil:
		log.WarnContext(ctx, "seat_run_wait_end_unread", "seat", seat, "turn_id", turn, "error", err.Error(),
			"detail", "how the run waited for ended could not be read, so its wait is answered as gone")
		return executorkeep.Ending{}, false
	case read.Session.Ending == nil:
		return executorkeep.Ending{}, false
	}
	return *read.Session.Ending, true
}

// waitReport is the first lines of what ending's reviewer said landed, within
// [runWaitReportBytes] on a line's end, for a wait on a run of the waiting
// run's own requester; "" for another requester's.
func waitReport(wait *sandbox.RunWait, ending executorkeep.Ending) string {
	if !wait.SameRequester {
		return ""
	}
	report := strings.TrimSpace(ending.CompletedWork)
	if len(report) <= runWaitReportBytes {
		return report
	}
	cut := report[:runWaitReportBytes]
	if at := strings.LastIndexByte(cut, '\n'); at > 0 {
		cut = cut[:at]
	}
	return strings.ToValidUTF8(cut, "") + "\n…"
}

// pushedBy is what run pushed to Gitlet, as its push record reads now.
func (e *Engine) pushedBy(ctx context.Context, run sandbox.PendingRun) (types.RunPushed, []string) {
	record, err := e.codeRunPushes().Pushed(ctx, run)
	if err != nil {
		return types.RunPushedUnknown, nil
	}
	if len(record.Refs) == 0 {
		return types.RunPushedNone, nil
	}
	refs := make([]string, 0, len(record.Refs))
	for _, ref := range record.Refs {
		refs = append(refs, ref.Repo+" "+ref.Ref)
	}
	return types.RunPushedSome, refs
}

// carryWaitOn wakes run, the run it waited for having left
// ([Engine.deliverWaitOver]): its seat's holder carries it on, told how that
// run went as it reads it then. Its conversation's line saying the work waits
// comes down.
func (e *Engine) carryWaitOn(ctx context.Context, run sandbox.PendingRun, waited types.RunWaited) {
	delivered, err := e.deliverWaitOver(ctx, run)
	if !delivered {
		return
	}
	outcome, failure := runResumed, error(nil)
	if err != nil {
		outcome, failure = runFailed, err
	}
	(&runsDesk{engine: e}).count(ctx, runToolAnswer, outcome, run.TurnID, waited.Turn, failure)
	if failure == nil {
		e.Status().Dequeue(context.WithoutCancel(ctx), run.AgentHandle, []string{runWaitWake(run.TurnID, run.LaunchID)})
	}
}

// carryCallWaitOn wakes run, its job parked on its own calls alone
// ([sandbox.PendingRun.WaitsOnOwnCalls]): its seat's holder carries it on in its
// own session, its brief telling each call and what it returned.
func (e *Engine) carryCallWaitOn(ctx context.Context, run sandbox.PendingRun) {
	if _, err := e.deliverWaitOver(ctx, run); err != nil {
		log.WarnContext(ctx, "seat_call_wait_unanswered", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"error", err.Error(),
			"detail", "the run parked on its own calls was not woken; woken again at the next read")
	}
}

// deliverWaitOver puts the wake carrying run on on its seat's control topic
// ([sandbox.AnswerDesk.DeliverWaitOver]), false where this node has no desk to
// deliver it through.
func (e *Engine) deliverWaitOver(ctx context.Context, run sandbox.PendingRun) (bool, error) {
	if deliver := e.runWaits.deliver; deliver != nil {
		return true, deliver(ctx, run)
	}
	desk := e.waitDesk()
	if desk == nil {
		return false, nil
	}
	return true, desk.DeliverWaitOver(ctx, run)
}

// waitDesk is the answer desk a wait's answer is delivered through, nil
// without a fleet store or a queue.
func (e *Engine) waitDesk() *sandbox.AnswerDesk {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return nil
	}
	return &sandbox.AnswerDesk{Pending: sandbox.NewCoordStore(e.backends.Fleet), Queue: e.backends.Queue,
		Seal: e.AnswerSeal()}
}

// runWaitWake is the wait a conversation's line holds for job of turn's run,
// parked by `wait_for_run`.
func runWaitWake(turn, job string) string { return "run-wait:" + turn + ":" + job }

// tellRunWaits tells the person caller's run works for why its work waits,
// once per wait, on the line of the conversation the turn holds ([Engine.tellWaiting]'s
// once-per-reason rule, [notify.Statuses.ExplainTurn]): the line stays up
// saying it, past the park that drops the turn's own hold, until the wait is
// answered.
func (e *Engine) tellRunWaits(ctx context.Context, caller sandbox.PendingRun, job string, wait *sandbox.RunWait) {
	why := "is waiting for another of its runs to finish"
	if wait.Item != "" {
		why = "is waiting for its other run on " + wait.Item + " to finish"
	}
	wake := runWaitWake(caller.TurnID, job)
	if !e.waitsTold.tell(caller.AgentHandle, wake, why) {
		return
	}
	if !e.Status().ExplainTurn(caller.AgentHandle, caller.TurnID, wake, why) {
		log.DebugContext(ctx, "seat_run_wait_untold", "turn_id", caller.TurnID,
			"detail", "no indicator of this node holds the turn's conversation, so nobody is told why it waits")
	}
}

// runStopped is [sandbox.CoordinatorOptions.Stopped]: the working indicator
// comes down ([Engine.releaseWorkingStatus]), and every wait is read again,
// since one may be on the run that stopped.
func (e *Engine) runStopped(ctx context.Context, handle, turnID string) {
	e.releaseWorkingStatus(ctx, handle, turnID)
	e.nudgeWaits()
}

// runParked is [sandbox.CoordinatorOptions.Parked]: a held call's delegates
// are woken ([Engine.wakeDelegates]), and a run parked by `wait_for_run`, or on
// its own calls, is watched until it is carried on.
func (e *Engine) runParked(ctx context.Context, run sandbox.PendingRun) {
	e.wakeDelegates(ctx, run)
	if run.EngineAnswers() {
		e.watchWaits(ctx, run.AgentHandle)
	}
}

// parkedRunRecovered is [sandbox.CoordinatorOptions.Unresumable], told every
// parked run of a seat this node recovers: judged as [Engine.parkedRunUnresumable]
// judges it, and a run parked by `wait_for_run`, or on its own calls, is watched
// here from now.
func (e *Engine) parkedRunRecovered(ctx context.Context, run sandbox.PendingRun) (*sandbox.Unresumable, error) {
	if run.EngineAnswers() {
		e.watchWaits(ctx, run.AgentHandle)
	}
	return e.parkedRunUnresumable(ctx, run)
}
