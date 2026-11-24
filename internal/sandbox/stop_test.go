package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON'S STOP ENDS THE SEAT'S RUNNING WORK AT ONCE.
//
// A pause with `stop_running` used to reach a seat's running turn at its next
// round and nothing else: a run that turn had launched is its harness's own
// process in a box, with no rounds the engine sees, so it ran on to its end —
// minutes or hours — still acting as the seat through its tool bridge, and only
// the turn that came back to collect it was stopped. These cases hold the stop
// to what it promises: the run ends now, through the ending every run takes,
// and says who ended it without ever reading as a failure.

// stopper is who every case's pause names.
var stopper = RunStop{By: "founder", ByKind: "human", OperatorID: "pat:founder-token",
	Reason: "wrong repository"}

// stopSpies records what a stop does above this package: the bridge sessions
// it closes ([CoordinatorOptions.Ended]) and the turns it closes on the record
// ([CoordinatorOptions.CloseStopped]).
type stopSpies struct {
	mu       sync.Mutex
	ended    []string
	closed   []PendingRun
	closeErr error
}

func (s *stopSpies) endedRuns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ended)
}

func (s *stopSpies) closedTurns() []PendingRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.closed)
}

// watchStops installs the spies on the rig's coordinator.
func watchStops(rig *coordRig) *stopSpies {
	spies := &stopSpies{}
	rig.coordinator.ended = func(runID string) {
		spies.mu.Lock()
		defer spies.mu.Unlock()
		spies.ended = append(spies.ended, runID)
	}
	rig.coordinator.closeStopped = func(_ context.Context, run PendingRun, _ RunCalls) (executorkeep.Ending, error) {
		spies.mu.Lock()
		defer spies.mu.Unlock()
		if spies.closeErr != nil {
			return executorkeep.Ending{}, spies.closeErr
		}
		spies.closed = append(spies.closed, run)
		return executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, StoppedBy: run.Stop.By}, nil
	}
	return spies
}

// stopAnnouncements is every publish of a stop on the events topic, repeats
// included.
func (r *coordRig) stopAnnouncements() []announcedStop {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []announcedStop
	for _, p := range r.queue.published {
		payload, ok := p.event.Data.(*types.ExecutorStopped)
		if !ok || p.topic != topics.Event(payload.EventType()) {
			continue
		}
		out = append(out, announcedStop{id: p.event.ID.String(), stopped: *payload})
	}
	return out
}

type announcedStop struct {
	id      string
	stopped types.ExecutorStopped
}

// stop is the seat's pause reaching this node, which decides the stop of every
// run it ends. A job that started in a box is ended by its read, which waits
// for it ([Coordinator.endJob]), so the pause watch hands the rest of that
// stop to the ending's own retry: a real node runs it at once, and a case
// fires it ([coordRig.fireRetries]).
func (r *coordRig) stop(t *testing.T) {
	t.Helper()
	if err := r.coordinator.StopSeat(t.Context(), "swe", stopper); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
}

// A RUNNING RUN IS ENDED: its box reclaimed, its bridge session closed, its row
// gone and its seat free — and nothing is left for its completion to claim.
func TestAStopEndsTheSeatsRunningRunAtOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	running := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)

	rig.stop(t)
	rig.fireRetries()

	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stopped run's record is still there, so its box and its seat are still held")
	}
	if !slices.Contains(rig.provider.KilledIDs(), running.SandboxID) {
		t.Fatalf("killed %v, want the stopped run's box %s: its harness is still working", rig.provider.KilledIDs(),
			running.SandboxID)
	}
	if !slices.Contains(spies.endedRuns(), "t1") {
		t.Fatal("the run's bridge session was not closed, so its box can still act as the seat")
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat is still held by a run that was stopped")
	}
	if !slices.Contains(rig.stoppedTurns(), "swe/t1") {
		t.Fatal("the engine was not told the turn stopped, so whatever it held up is still up")
	}
	// AND THE JOB'S COMPLETION, WHEN IT COMES, FINDS NOTHING TO CLAIM.
	rig.runner.Finish(Result{Success: true, Text: "done anyway"})
	payload := types.ExecutorFinished{
		Agent: running.AgentID, AgentHandle: running.AgentHandle, RoleName: running.Role,
		TurnID: running.TurnID, LaunchID: running.LaunchID,
		SandboxID: running.SandboxID, Harness: running.Harness,
	}
	ev := events.New(payload, events.TraceContext{TraceID: running.TraceID})
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := len(rig.resumer.requests); got != 0 {
		t.Fatalf("a stopped run's turn was resumed %d times", got)
	}
}

// THE BRIDGE SESSION GOES BEFORE THE BOX IS READ, so the stop takes effect on the
// seat's surface at once rather than after a read of the box that can take as
// long as the box takes to answer.
func TestAStopClosesTheRunsSessionBeforeItReadsTheBox(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	rig.launch("t1")
	var endedFirst bool
	rig.runner.CollectFunc = func(context.Context) error {
		endedFirst = slices.Contains(spies.endedRuns(), "t1")
		return nil
	}

	rig.stop(t)
	rig.fireRetries()

	if !endedFirst {
		t.Fatal("the stopped run's box was read while its bridge session was still open")
	}
}

// A STOP IS ANNOUNCED ONCE, AS A STOP, NAMING WHO — and never as a loss, which is
// a failure by its very type and would count a person's stop with the runs that
// broke.
func TestAStopIsAnnouncedAsAStopAndNeverAsALoss(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	watchStops(rig)
	running := rig.launch("t1")

	rig.stop(t)
	rig.fireRetries()

	if failures := rig.failures(); len(failures) != 0 {
		t.Fatalf("a stop was announced as a loss: %+v", failures)
	}
	got := rig.stopAnnouncements()
	if len(got) != 1 {
		t.Fatalf("announced the stop %d times, want once: %+v", len(got), got)
	}
	stopped := got[0].stopped
	if stopped.StoppedBy != stopper.By || stopped.StoppedByKind != stopper.ByKind ||
		stopped.OperatorID != stopper.OperatorID || stopped.Reason != stopper.Reason {
		t.Fatalf("the stop names %+v, want the pause's author %+v", stopped, stopper)
	}
	if stopped.TurnID != "t1" || stopped.LaunchID != running.LaunchID || stopped.SandboxID != running.SandboxID ||
		stopped.WorkKey != rigWorkKey || stopped.Agent != sweID {
		t.Fatalf("the stop does not name the run it ended: %+v", stopped)
	}
}

// WHAT THE RUN SPENT BEFORE THE STOP IS CHARGED, ONCE, and its phase published —
// not as a failure: a run stopped halfway still spent what it spent, and once
// its box is reclaimed nothing can read it.
func TestAStoppedRunsSpendIsChargedAndPublishedBeforeItsBoxGoes(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	rig.launch("t1")
	// What the job had done by the stop. Finished here only so the fake has
	// something to read back: the stop's read ends a job still working, and
	// reads what it had done either way.
	rig.runner.Finish(Result{Text: "halfway through the migration",
		InputTokens: 9000, OutputTokens: 1000, CacheReadTokens: 4000})

	rig.stop(t)
	rig.fireRetries()

	if got := rig.accountant.asked(); got != 1 {
		t.Fatalf("charged the stopped run %d times, want once", got)
	}
	if rig.accountant.charged != 10_000 {
		t.Fatalf("charged %d tokens, want the 10000 the run had spent", rig.accountant.charged)
	}
	launches := rig.collected()
	if len(launches) != 1 {
		t.Fatalf("published %d launch records for the stopped run, want one", len(launches))
	}
	if rec := launches[0]; rec.Failed || rec.Notes != "stopped by founder before it finished" ||
		rec.InputTokens != 9000 || rec.OutputTokens != 1000 {
		t.Fatalf("the stopped run's launch record is %+v, want its tokens, noted as stopped and not failed", rec)
	}
	// AND THE FIGURES REACH THE TURN'S CLOSE, which charges the work item.
	closed := spies.closedTurns()
	if len(closed) != 1 {
		t.Fatalf("closed the turn %d times, want once", len(closed))
	}
	if stop := closed[0].Stop; stop == nil || !stop.Collected || stop.InputTokens != 9000 ||
		stop.OutputTokens != 1000 || stop.CacheReadTokens != 4000 {
		t.Fatalf("the turn was closed on %+v, want the run's collected spend", closed[0].Stop)
	}
}

// A BOX THAT CANNOT BE READ DOES NOT HOLD THE STOP UP: the run is ended all the
// same, with nothing charged, because a stop that waited on a dead box would
// never finish.
func TestAStopWhoseBoxCannotBeReadStillEndsTheRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	rig.launch("t1")
	rig.runner.CollectErr = errBoxGoneMidRead

	rig.stop(t)
	rig.fireRetries()

	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("a stop whose box could not be read left its run in place")
	}
	if rig.accountant.asked() != 0 || len(rig.collected()) != 0 {
		t.Fatal("charged or published a spend nobody read")
	}
	if len(rig.stopAnnouncements()) != 1 || len(spies.closedTurns()) != 1 {
		t.Fatal("a stop whose box could not be read was not announced, or its turn not closed")
	}
}

// EVERY STOP ENDS ITS TURN FROM THE RUN'S ENDING, parked or not, once: a run
// stopped while still launching has no parked turn on its row — that turn is
// still in the frame that launched it, which closes it on the record when its
// park is refused — but its end, and its colleague's answer, are the ending's,
// recorded after the stop's read; the engine closes a turn on the record only
// where it parked ([CoordinatorOptions.CloseStopped]).
//
// Mutation: hand the ending a stop only where the row holds a parked state, as
// it once did, and a launch stopped before its park ends nowhere.
func TestEveryStopEndsItsTurnFromTheRunsEnding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		place func(rig *coordRig)
	}{
		{"running", func(rig *coordRig) { rig.launch("t1") }},
		{"launching", func(rig *coordRig) { rig.launching("t1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spies := watchStops(rig)
			tc.place(rig)

			rig.stop(t)
			rig.fireRetries()

			if got := len(spies.closedTurns()); got != 1 {
				t.Fatalf("ended the stopped turn %d times, want once", got)
			}
			if len(rig.stopAnnouncements()) != 1 {
				t.Fatal("the stop was not announced")
			}
			if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
				t.Fatal("the stopped run's record was left behind its owed end")
			}
		})
	}
}

// PARKED WORK STAYS HELD BEHIND THE PAUSE, and a claimed tail is the turn's own:
// a stop ends only what is out in a box running. A run waiting on a person's
// answer does no work, and a resumed run is a turn the turn's own fence stops at
// its next round.
func TestAStopLeavesEveryRunItDoesNotEndAsItWas(t *testing.T) {
	t.Parallel()
	for _, status := range Active {
		if slices.Contains(Stoppable, status) {
			continue
		}
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			place(t, rig, status)
			before := rig.get("t1")

			rig.stop(t)

			after, found, err := rig.pending.Get(t.Context(), "t1")
			if err != nil || !found {
				t.Fatalf("a %s run was ended by the stop (found=%v, err=%v)", status, found, err)
			}
			if after.Status != before.Status || after.Ending != nil || after.Stop != nil {
				t.Fatalf("a %s run became %s (ending %+v): a stop ends only launching and running runs",
					status, after.Status, after.Ending)
			}
			if len(rig.stopAnnouncements()) != 0 {
				t.Fatalf("announced a stop of a %s run", status)
			}
		})
	}
}

// A COMPLETION CLAIMED FIRST WINS. The stop's license names the job in the
// statuses a stop ends, so a run whose tail a completion already claimed is the
// resumed turn's — which its own fence stops — and is not ended under it.
func TestAStopAfterTheCompletionWasClaimedLeavesTheClaim(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	// THE STOP LISTS THE RUN RUNNING, and the claim lands between its listing
	// and its decision: the store has to refuse the stop on its own, from the
	// license.
	refusing := &claimBeforeDecide{PendingStore: rig.pending, t: t}
	rig.coordinator.pending = refusing

	rig.stop(t)

	run := rig.get("t1")
	if run.Status != StatusResumed || run.Ending != nil {
		t.Fatalf("the claimed run became %s (ending %+v); the stop must leave the claim to its turn",
			run.Status, run.Ending)
	}
	if !refusing.claimed {
		t.Fatal("the claim never landed, so this case raced nothing")
	}
}

// A JOB ITS COLLECTION ALREADY HANDED TO ITS TURN IS THE TURN'S: back in
// running read and charged, its spend is the turn's resume's to charge, so the
// stop leaves it, no second read, reclaim, close or announcement, and the
// turn's fence ends the resume. A job kept for its hold comes round at once
// (TestAStopEndsTheWaitOfACollectedJobsResume), its re-fire being the turn's.
//
// Mutation: drop the store's refusal and StopSeat's skip, and the box is read a
// second time, reclaimed, and the turn closed on the stop.
func TestAStopLeavesAJobItsCollectionHandedToItsTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail func(rig *coordRig) error
	}{
		{"held by the budget", func(rig *coordRig) error { return budgetHeld(rig.now.Add(time.Hour)) }},
		{"handed back for a retry", func(*coordRig) error { return errors.New("the resume broke first") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spies := watchStops(rig)
			running := rig.launch("t1")
			rig.resumer.failWith(tc.fail(rig))
			rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 4000, OutputTokens: 500})
			payload, ev := rig.completion("t1")
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			back := rig.get("t1")
			if back.Status != StatusRunning || !back.Launch.Collected {
				t.Fatalf("the handed-back row reads %s collected=%v, want running and collected",
					back.Status, back.Launch.Collected)
			}
			charges := rig.accountant.asked()
			// THE COLLECTION ENDED THE JOB'S SESSION ITSELF, as every
			// collection does before it reads ([Coordinator.drainCalls]): what
			// the stop ends is counted from here.
			sessions := len(spies.endedRuns())
			// A READ IS THE STOP'S OR THE TURN'S, told by the row it is made
			// on: the turn's own collection reads under its claim, and a
			// stop's read under the ending it decided. The stop ends the wait
			// of a job this node keeps for its hold by re-firing it
			// ([Coordinator.endCollectedWait]), and that re-fire is the turn's
			// resume, which collects the job again under its claim, as every
			// resume of a handed-back claim does.
			var stopReads, turnReads atomic.Int32
			rig.runner.CollectFunc = func(ctx context.Context) error {
				row, found, err := rig.pending.Get(ctx, "t1")
				if err == nil && found && row.Status == StatusResumed && row.Ending == nil && row.Stop == nil {
					turnReads.Add(1)
				} else {
					stopReads.Add(1)
				}
				return nil
			}

			rig.stop(t)
			// NO STOP DECIDED, read before anything is waited for: a stop's
			// ending goes on retrying on the coordinator's own lifetime, so
			// a wait for it would never return.
			if decided := rig.get("t1"); decided.Ending != nil || decided.Stop != nil {
				t.Fatalf("the stop decided an ending of the collected job's run (ending %+v, stop %+v); "+
					"it is its turn's", decided.Ending, decided.Stop)
			}
			// WHATEVER ELSE THE STOP SET GOING HAS RUN, so what it did is
			// counted whole rather than by a race against the assertions.
			rig.coordinator.inflight.Wait()

			after := rig.get("t1")
			if after.Status != StatusRunning || after.Ending != nil || after.Stop != nil {
				t.Fatalf("the collected job's run became %s (ending %+v, stop %+v); it is its turn's",
					after.Status, after.Ending, after.Stop)
			}
			if got := stopReads.Load(); got != 0 {
				t.Fatalf("the stop read the collected job's box again, %d times", got)
			}
			if got := rig.accountant.asked(); got != charges {
				t.Fatalf("the counters were asked %d times, %d before the stop", got, charges)
			}
			if slices.Contains(rig.provider.KilledIDs(), running.SandboxID) {
				t.Fatal("the stop reclaimed the box its turn's resume continues in")
			}
			// EACH OF THE TURN'S COLLECTIONS ENDS THE JOB'S SESSION ITSELF
			// before it reads, as the first one did: any more is the stop's.
			turns := int(turnReads.Load())
			if len(spies.closedTurns()) != 0 || len(spies.endedRuns()) != sessions+turns ||
				len(rig.stopAnnouncements()) != 0 {
				t.Fatalf("the stop closed %d turns, ended %d sessions and announced %d stops of a job it "+
					"does not end", len(spies.closedTurns()), len(spies.endedRuns())-sessions-turns,
					len(rig.stopAnnouncements()))
			}
		})
	}
}

// answer stages what the box's host answers the next read with.
func (b *boxAsks) answer(result Result, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.result, b.err = result, err
}

// A STOP OF A RUN WHOSE BOX IS ON ANOTHER NODE'S HOST READS ITS SPEND THERE. The
// seat moved to this node while the job worked, and the job's direct box is a
// process tree on the node that launched it, which no other node can read:
// read here, the box is not found and the run's spend is lost. The frame that
// decides the stop closes the run's session and waits on no other node; this
// node's own retry asks the host, charges what it read, and leaves the box to
// its host, which it tells to look now.
//
// Mutation: read the stopped box here rather than asking its host, and the
// host is never asked; finish the stop on the deciding frame, and the pause
// watch waits on another node.
func TestAStopOfARunWhoseBoxIsOnAnotherHostIsReadThere(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	asks := &boxAsks{result: Result{Text: "halfway", InputTokens: 4000}}
	told := &nudges{}
	rig.onNode(t, "node-b", asks, told)
	local := readHere(rig)
	running := rig.launchedBy("t1", "node-a")

	rig.stop(t)

	if !slices.Contains(spies.endedRuns(), "t1") {
		t.Fatal("the stop did not close the run's session at once")
	}
	if got := asks.reads(); len(got) != 0 {
		t.Fatalf("the deciding frame asked the box's host %v; it waits on no other node", got)
	}
	if got := rig.get("t1"); got.Ending == nil || got.Stop == nil {
		t.Fatalf("the stop was not decided on the row: ending %+v stop %+v", got.Ending, got.Stop)
	}

	rig.fireRetries()

	if got := asks.reads(); !slices.Equal(got, []BoxRead{BoxReadStop}) {
		t.Fatalf("the box's host was asked %v; want one stop's read", got)
	}
	if *local != 0 {
		t.Fatalf("this node read %d box(es) itself", *local)
	}
	if got := rig.accountant.total(); got != 4000 {
		t.Fatalf("charged %d tokens, want the 4000 the host read", got)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v here; the box is its host's to reclaim", killed)
	}
	if sent := told.all(); len(sent) != 1 || sent[0].Why != NudgeReleased || sent[0].LaunchID != running.LaunchID {
		t.Fatalf("told the host %+v; want one release of the run's job", sent)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stopped run's record is still there")
	}
	if len(rig.stopAnnouncements()) != 1 {
		t.Fatalf("announced the stop %d times, want once", len(rig.stopAnnouncements()))
	}
	if closed := spies.closedTurns(); len(closed) != 1 || closed[0].Stop.InputTokens != 4000 {
		t.Fatalf("closed the turn on %+v, want once on the host's read", closed)
	}
}

// A SILENT HOST KEEPS THE STOP, AND NEVER WRITES ITS RUN'S SPEND OFF. The node
// whose host the box is on is draining or restarting: it is no verdict about the
// box, which only that node reclaims, and only once it is back. So the stop is
// kept — its session closed, nothing charged, nothing announced — and asked
// again until the host answers, which charges what it read; only a host silent
// for the host grace since this node first asked the host, the bound the
// launch-node watch takes a launch node for gone by, ends the run with nothing
// read.
//
// Mutation: read a silent host as nothing spent, and the stop finishes on the
// first retry with its spend lost; drop the bound, and a host that never comes
// back keeps the stop for good.
func TestAStopWaitsForASilentHostAndNeverWritesItsSpendOff(t *testing.T) {
	t.Parallel()
	silent := fmt.Errorf("asking node-a: %w", ErrBoxHostSilent)
	for _, tc := range []struct {
		name string
		// then is what happens while the stop waits, and the tokens the
		// stop ends up charging.
		then   func(rig *coordRig, asks *boxAsks)
		tokens int
	}{
		{"the host comes back", func(_ *coordRig, asks *boxAsks) {
			asks.answer(Result{Text: "halfway", InputTokens: 4000}, nil)
		}, 4000},
		{"the host never does", func(rig *coordRig, _ *boxAsks) {
			rig.now = rig.now.Add(DefaultHostGrace)
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spies := watchStops(rig)
			asks := &boxAsks{err: silent}
			rig.onNode(t, "node-b", asks, &nudges{})
			rig.launchedBy("t1", "node-a")

			rig.stop(t)
			rig.fireRetries()

			kept := rig.get("t1")
			if kept.Ending == nil || kept.Stop == nil || kept.Stop.Collected {
				t.Fatalf("the stop on a silent host reads ending %+v stop %+v; want it kept, unread",
					kept.Ending, kept.Stop)
			}
			if rig.accountant.asked() != 0 || len(rig.stopAnnouncements()) != 0 || len(spies.closedTurns()) != 0 {
				t.Fatal("the stop charged, announced or closed its turn while its box's host was silent")
			}
			if !slices.Contains(spies.endedRuns(), "t1") {
				t.Fatal("the kept stop left the run's session open")
			}

			tc.then(rig, asks)
			rig.fireRetries()

			if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
				t.Fatal("the stop was not finished")
			}
			if got := rig.accountant.total(); got != tc.tokens {
				t.Fatalf("charged %d tokens, want %d", got, tc.tokens)
			}
			if len(rig.stopAnnouncements()) != 1 || len(spies.closedTurns()) != 1 {
				t.Fatal("the finished stop was not announced once with its turn closed once")
			}
		})
	}
}

// A STOP WHOSE HOST ALONE HAS A SILENT STORE WAITS FOR IT AS FOR A SILENT HOST:
// kept, its session closed and nothing charged, until the host reads the box,
// or the host grace by this node's clock since it first asked, when it ends
// with nothing read. Unbounded, it would hold the seat's mail for the
// partition.
//
// Mutation: read the store's silence as the box's failure in readStopped, and
// the stop finishes on its first retry with its spend lost; drop the bound, and
// it is kept past the host grace.
func TestAStopWhoseHostAloneHasASilentStoreWaitsForItAsForASilentHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// then is what happens while the stop waits, and the tokens the
		// stop ends up charging.
		then   func(rig *coordRig, store *silentStore)
		tokens int
	}{
		{"the host's store answers", func(_ *coordRig, store *silentStore) { store.answer() }, 4000},
		{"it does not for the host grace", func(rig *coordRig, _ *silentStore) {
			rig.now = rig.now.Add(DefaultHostGrace)
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spies := watchStops(rig)
			store := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
			rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, &nudges{})
			rig.launchedBy("t1", "node-a")
			rig.runner.Finish(Result{Text: "halfway", InputTokens: 4000})

			rig.stop(t)
			rig.fireRetries()
			kept := rig.get("t1")
			if kept.Ending == nil || kept.Stop == nil || kept.Stop.Collected {
				t.Fatalf("the stop on a host whose store is silent reads ending %+v stop %+v; want it kept, "+
					"unread", kept.Ending, kept.Stop)
			}
			if rig.accountant.asked() != 0 || len(rig.stopAnnouncements()) != 0 || len(spies.closedTurns()) != 0 {
				t.Fatal("the stop charged, announced or closed its turn while the host's store was silent")
			}
			if !slices.Contains(spies.endedRuns(), "t1") {
				t.Fatal("the kept stop left the run's session open")
			}

			tc.then(rig, store)
			rig.fireRetries()
			if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
				t.Fatal("the stop was not finished")
			}
			if got := rig.accountant.total(); got != tc.tokens {
				t.Fatalf("charged %d tokens, want %d", got, tc.tokens)
			}
			if len(rig.stopAnnouncements()) != 1 || len(spies.closedTurns()) != 1 {
				t.Fatal("the finished stop was not announced once with its turn closed once")
			}
		})
	}
}

// silentFromDecision is this node's own view of the run store, which, once
// armed, falls silent as an ending's decision lands on it: that decision is
// answered, and no read of a row, decision or stop's record after it, until it
// answers again. A silence this node comes to share mid-attempt, after its
// retry read the row and decided the ending again, so the attempt's own read of
// the row is the first step it does not answer.
type silentFromDecision struct {
	PendingStore
	mu     sync.Mutex
	armed  bool
	silent bool
}

func (s *silentFromDecision) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *silentFromDecision) answer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.silent = false, false
}

func (s *silentFromDecision) isSilent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.silent
}

func (s *silentFromDecision) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.isSilent() {
		return PendingRun{}, false, fmt.Errorf("sandbox: read run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.Get(ctx, turnID)
}

func (s *silentFromDecision) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	if s.isSilent() {
		return PendingRun{}, false, fmt.Errorf("sandbox: decide the end of run %s: %w", turnID, errStoreUnanswered)
	}
	run, ok, err := s.PendingStore.DecideEnding(ctx, turnID, d)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = s.armed
	return run, ok, err
}

func (s *silentFromDecision) CollectStop(ctx context.Context, turnID string, c StopCollection) (PendingRun, bool,
	error) {
	if s.isSilent() {
		return PendingRun{}, false, fmt.Errorf("sandbox: record stopped run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.CollectStop(ctx, turnID, c)
}

// A STOP WHOSE NODE SHARES ITS HOST'S STORE SILENCE IS KEPT, AND NEVER SAID
// ENDED, past the host grace too, nor counted toward the host's silence. The
// staged attempt's own read of the row is the first step this node's store does
// not answer, after the retry decided the ending again.
//
// Mutation: bound the host's silence whatever the attempt's own row read met
// (readStopped), or hand readStopped a read that answered (finishEnding), and
// the stop is kept only by its record's write failing; count the shared silence
// (readStopped), and it is given up once this node's store alone answers.
// ([TestAStopIsSaidEndedUnreadOnlyOnceItsRecordSaysSo] holds the saying.)
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAStopWhoseNodeSharesItsHostsStoreSilenceIsKeptAndNeverSaidEnded(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	spies := watchStops(rig)
	hostStore := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
	rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", hostStore)}, &nudges{})
	ownStore := &silentFromDecision{PendingStore: rig.pending}
	rig.coordinator.pending = ownStore
	rig.launchedBy("t1", "node-a")
	rig.runner.Finish(Result{Text: "halfway", InputTokens: 4000})

	rig.stop(t)
	rig.now = rig.now.Add(DefaultHostGrace)
	ownStore.arm()
	for range 2 {
		rig.fireRetries()
	}
	kept := rig.get("t1")
	if kept.Ending == nil || kept.Stop == nil || kept.Stop.Collected {
		t.Fatalf("past the host grace, the stop on a silence both nodes share reads ending %+v stop %+v; "+
			"want it kept, unread", kept.Ending, kept.Stop)
	}
	if rig.accountant.asked() != 0 || len(rig.stopAnnouncements()) != 0 || len(spies.closedTurns()) != 0 {
		t.Fatal("the stop charged, announced or closed its turn while both stores were silent")
	}
	line := logs.String()
	if strings.Contains(line, "sandbox_stop_unread") {
		t.Fatalf("the stop kept on a shared silence was said ended all the same: %q", line)
	}
	if !strings.Contains(line, "sandbox_stop_read_waits") || !strings.Contains(line, "waits_for=store") ||
		!strings.Contains(line, "shared=true") {
		t.Fatalf("nothing said the stop waits on a store silent to both nodes: %q", line)
	}

	rig.now = rig.now.Add(DefaultHostGrace)
	ownStore.answer()
	rig.fireRetries()
	if kept := rig.get("t1"); kept.Stop == nil || kept.Stop.Collected || rig.accountant.asked() != 0 {
		t.Fatalf("a host grace into a silence both nodes shared, the stop whose host's store alone is "+
			"silent now reads stop %+v, charged %t; want it kept, its host's silence counted from now",
			kept.Stop, rig.accountant.asked() != 0)
	}

	hostStore.answer()
	rig.fireRetries()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stop was not finished once both stores answered")
	}
	if got := rig.accountant.total(); got != 4000 {
		t.Fatalf("charged %d tokens, want the 4000 the host read once the stores answered", got)
	}
	if len(rig.stopAnnouncements()) != 1 || len(spies.closedTurns()) != 1 {
		t.Fatal("the finished stop was not announced once with its turn closed once")
	}
}

// recordUnansweredOnce is this node's own view of the run store, which does not
// answer the first record of a stopped run's collection (CollectStop) and
// answers every other call: a silence that began after the attempt's own read
// of the run's row answered, and ended before the next attempt.
type recordUnansweredOnce struct {
	PendingStore
	missed atomic.Bool
}

func (s *recordUnansweredOnce) CollectStop(ctx context.Context, turnID string, c StopCollection) (PendingRun, bool,
	error) {
	if s.missed.CompareAndSwap(false, true) {
		return PendingRun{}, false, fmt.Errorf("sandbox: record stopped run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.CollectStop(ctx, turnID, c)
}

// A STOP IS SAID ENDED UNREAD ONLY ONCE ITS RECORD SAYS SO (CollectStop): a
// write of this node's store, which can fall silent after the attempt's own
// read; the stop is then kept for the retry. Here the read comes to nothing by
// the stop's age: the host grace and twice objstore.AssumedClockSkew.
//
// Mutation: say it before the record is written, and the kept stop is said
// ended; never say it, and a stop ended unread is never said to be.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAStopIsSaidEndedUnreadOnlyOnceItsRecordSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange launches t1 where its stop's read comes to nothing, and
		// says is how the line names what it came to.
		arrange func(t *testing.T, rig *coordRig)
		says    string
	}{
		{"the box gone", func(_ *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.runner.CollectErr = errBoxGoneMidRead
		}, "gone=true"},
		{"the box unreadable on this node's host", func(_ *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.runner.CollectErr = errBoxBlip
		}, errBoxBlip.Error()},
		{"its host silent for the host grace", func(t *testing.T, rig *coordRig) {
			rig.onNode(t, "node-b", &boxAsks{err: fmt.Errorf("asking node-a: %w", ErrBoxHostSilent)}, &nudges{})
			rig.launchedBy("t1", "node-a")
		}, "host_silent=true"},
		{"its host's store silent for the host grace", func(t *testing.T, rig *coordRig) {
			store := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
			rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, &nudges{})
			rig.launchedBy("t1", "node-a")
		}, "store_silent=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			spies := watchStops(rig)
			tc.arrange(t, rig)
			rig.runner.Finish(Result{Text: "halfway", InputTokens: 4000})
			rig.coordinator.pending = &recordUnansweredOnce{PendingStore: rig.coordinator.pending}

			rig.stop(t)
			// PAST THE STOP'S AGE BOUND, so a silent host is given up on the
			// retry's first ask, whose silence has only just begun.
			rig.now = rig.now.Add(DefaultHostGrace + 2*objstore.AssumedClockSkew)
			rig.fireRetries()
			kept := rig.get("t1")
			if kept.Ending == nil || kept.Stop == nil || kept.Stop.Collected {
				t.Fatalf("the stop whose record went unanswered reads ending %+v stop %+v; want it kept",
					kept.Ending, kept.Stop)
			}
			if said := logs.String(); strings.Contains(said, "sandbox_stop_unread") {
				t.Fatalf("the stop kept for its record was said ended all the same: %q", said)
			}

			rig.fireRetries()
			if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
				t.Fatal("the stop was not finished once its record was answered")
			}
			said := logs.String()
			if n := strings.Count(said, "sandbox_stop_unread"); n != 1 || !strings.Contains(said, tc.says) {
				t.Fatalf("the stop ended unread was said so %d times, in %q; want once, as its record landed, "+
					"saying %q", n, said, tc.says)
			}
			if rig.accountant.asked() != 0 || len(rig.stopAnnouncements()) != 1 || len(spies.closedTurns()) != 1 {
				t.Fatal("the stop ended unread charged, or was not announced once with its turn closed once")
			}
		})
	}
}

// A STOP OF A RELAUNCH BEFORE IT ATTACHES ITS BOX READS NO JOB AND LEAVES THE
// BOX TO ITS HOST. A relaunch resets the row on another node (a new job, not
// yet charged), but the row names the previous job's box, on that job's host,
// until the launch attaches its own. A stop in that window must not read the
// previous, collected and charged job back off the box (a second charge), nor,
// with the row naming this node as host, kill an id this node lacks and leave
// the real host untold.
//
// Mutation: keep the previous job's command on the reset row, and the stop
// asks the box's host to read the finished job; name the relaunch's node on the
// reset row, and the stop kills the box here and tells its host nothing.
func TestAStopOfARelaunchBeforeItsBoxReadsNoJobAndLeavesTheBoxToItsHost(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	asks := &boxAsks{result: Result{Text: "the previous job, done", InputTokens: 9000}}
	told := &nudges{}
	rig.onNode(t, "node-b", asks, told)
	prior := rig.launchedBy("t1", "node-a")
	// THE PREVIOUS JOB, COLLECTED AND CHARGED; its turn resumed here and is
	// relaunching.
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1",
		CompletionTail(prior.LaunchID), rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	relaunch, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: sweID, Role: "SWE", Harness: "claude-code",
		TraceID: "tr-1", CreatedAt: rig.now, LaunchNode: "node-b",
	}, rigLease)
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}

	rig.stop(t)
	rig.fireRetries()

	if got := asks.reads(); len(got) != 0 {
		t.Fatalf("the stop asked the box's host %v, for a job the relaunch has not started", got)
	}
	if rig.accountant.asked() != 0 || len(rig.collected()) != 0 {
		t.Fatal("the stop charged or published the previous job's spend again")
	}
	if slices.Contains(rig.provider.KilledIDs(), prior.SandboxID) {
		t.Fatalf("killed %s from a node that is not its host", prior.SandboxID)
	}
	if sent := told.all(); len(sent) != 1 || sent[0].Why != NudgeReleased || sent[0].LaunchID != relaunch.LaunchID {
		t.Fatalf("told the box's host %+v; want it told once its box is left to it", sent)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stopped relaunch's record is still there")
	}
	if len(rig.stopAnnouncements()) != 1 || len(spies.endedRuns()) == 0 {
		t.Fatal("the stop was not announced, or its session not closed")
	}
}

// A STOP OF A LAUNCH IS KEPT FOR THE FRAME LAUNCHING IT, AND ONLY FOR IT. The
// stop deletes the row in the time a box takes to die, and the person may
// resume the seat before the turn unwinds to its park, so the frame reads the
// stop's own decision here: once, by the turn it ended. A running run's stop
// is its parked turn's close and keeps nothing; a stop no frame took goes with
// the seat when it leaves this node; and the turn's next launch is a new job.
//
// Mutation: keep nothing for a stopped launch, and its frame finds no stop;
// keep a running run's stop, or keep one past its seat, and a later refused
// park of the same turn is read as a stop it was not.
func TestAStopOfALaunchIsKeptForTheFrameLaunchingIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	watchStops(rig)
	rig.launching("t1")
	rig.stop(t)
	rig.fireRetries()

	got, ok := rig.coordinator.LaunchStopped("t1")
	if !ok || got != stopper {
		t.Fatalf("the launch's frame read %+v (%v), want the stop that ended it", got, ok)
	}
	if _, again := rig.coordinator.LaunchStopped("t1"); again {
		t.Fatal("the stop was kept after its frame took it")
	}

	// A RUNNING RUN'S STOP closes its parked turn itself, and keeps nothing.
	rig.launch("t2")
	rig.stop(t)
	rig.fireRetries()
	if _, kept := rig.coordinator.LaunchStopped("t2"); kept {
		t.Fatal("a running run's stop was kept for a launching frame there is none of")
	}

	// A STOP NO FRAME TOOK goes with the seat.
	rig.launching("t3")
	rig.stop(t)
	rig.fireRetries()
	rig.coordinator.ReleaseSeat("swe")
	if _, kept := rig.coordinator.LaunchStopped("t3"); kept {
		t.Fatal("a launch's stop outlived its seat on this node")
	}

	// AND THE TURN'S NEXT LAUNCH IS A NEW JOB.
	rig.launching("t4")
	rig.stop(t)
	rig.fireRetries()
	if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t4")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, kept := rig.coordinator.LaunchStopped("t4"); kept {
		t.Fatal("the turn's last launch's stop was read as its next one's")
	}
}

// claimBeforeDecide claims a run's completion the moment a stop decides on it,
// as a completion processed on the seat's control topic can.
type claimBeforeDecide struct {
	PendingStore
	t       *testing.T
	claimed bool
}

func (s *claimBeforeDecide) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	if !s.claimed {
		run, _, err := s.PendingStore.Get(ctx, turnID)
		if err != nil {
			s.t.Fatalf("Get: %v", err)
		}
		if _, won, err := s.PendingStore.ClaimForResume(ctx, turnID, CompletionTail(run.LaunchID),
			rigLease); err != nil || !won {
			s.t.Fatalf("ClaimForResume = %v, %v", won, err)
		}
		s.claimed = true
	}
	return s.PendingStore.DecideEnding(ctx, turnID, d)
}

// A LAUNCH STOPPED BETWEEN ITS JOB AND ITS PARK IS TOLD SO. The turn that
// launched it is still unwinding; its park is refused with [ErrRunEnding] — not
// answered false, which the engine reads as a suspension that failed — so it
// closes itself as stopped.
func TestALaunchStoppedBeforeItsParkIsRefusedThePark(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	// THE ENDING STAYS DECIDED: its delete is refused, as a stop whose
	// last step the store did not take leaves it, and the park lands in
	// that window.
	rig.coordinator.pending = &refusingStore{inner: rig.pending, refuse: []string{"Finish"}}

	if err := rig.coordinator.StopSeat(t.Context(), "swe", stopper); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	_, err := rig.pending.MarkSuspended(t.Context(), "t1", Suspension{State: []byte(`{"v":3}`)})
	if !errors.Is(err, ErrRunEnding) {
		t.Fatalf("MarkSuspended on a stopped launch = %v, want ErrRunEnding", err)
	}
}

// A NODE THAT DOES NOT HOLD THE SEAT STOPS NOTHING: the node that holds it reads
// the same pause and stops the runs under its own lease.
func TestAStopOnANodeThatDoesNotHoldTheSeatEndsNothing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.lease = notLeased

	err := rig.coordinator.StopSeat(t.Context(), "swe", stopper)
	if !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("StopSeat on a seat this node lost = %v, want ErrSeatNotHeld", err)
	}
	if run := rig.get("t1"); run.Ending != nil || run.Status != StatusRunning {
		t.Fatalf("a node that lost the seat ended its run: %+v", run.Ending)
	}
}

// A STOP THIS NODE COULD NOT FINISH IS FINISHED BY THE SEAT'S NEXT HOLDER AS THE
// SAME STOP: the decision is on the row with who made it, so the successor
// announces the same event under the same identity and closes the turn — rather
// than the run being lost without a word, or announced as a loss.
func TestAStopTheNodeCouldNotFinishIsFinishedByTheSeatsNextHolder(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	rig.launch("t1")
	// The broker refuses the announcement, so the ending stays decided on
	// the row after this node's retry has read the box — and this node
	// "stops" before its next one.
	rig.failPublishes(errors.New("the broker went away"))
	if err := rig.coordinator.StopSeat(t.Context(), "swe", stopper); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	rig.fireRetries()
	decided := rig.get("t1")
	if decided.Ending == nil || decided.Stop == nil || decided.Stop.By != stopper.By {
		t.Fatalf("the row carries ending %+v and stop %+v, want the stop decided with who made it",
			decided.Ending, decided.Stop)
	}
	rig.failPublishes(nil)

	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}

	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the seat's next holder left the stopped run in place")
	}
	got := rig.stopAnnouncements()
	if len(got) == 0 {
		t.Fatal("the stop was never announced")
	}
	for _, a := range got {
		if a.id != endingEventID(decided).String() {
			t.Fatalf("announced the stop as %s, want the identity its ending recorded, %s",
				a.id, endingEventID(decided))
		}
	}
	if len(rig.failures()) != 0 {
		t.Fatal("the next holder announced the stop as a loss")
	}
	if len(spies.closedTurns()) == 0 {
		t.Fatal("the next holder did not close the stopped turn")
	}
}

// A CLOSE THAT FAILS KEEPS THE ENDING: the row is not deleted while the turn it
// would close is still open on the record, because nothing reads a deleted row
// again.
func TestAStopWhoseTurnCouldNotBeClosedKeepsItsRow(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spies := watchStops(rig)
	rig.launch("t1")
	spies.closeErr = errors.New("the event bus refused the turn's completion")

	if err := rig.coordinator.StopSeat(t.Context(), "swe", stopper); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	rig.fireRetries()

	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found || run.Ending == nil {
		t.Fatalf("the row went (found=%v, err=%v) while its stopped turn was still open", found, err)
	}
	// AND THE RETRY THAT CLOSES IT CHARGES NOTHING TWICE.
	spies.mu.Lock()
	spies.closeErr = nil
	spies.mu.Unlock()
	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stop was never finished")
	}
	if got := rig.accountant.asked(); got > 1 {
		t.Fatalf("charged the stopped run %d times across two attempts at one ending", got)
	}
}

// A STOP IS NOT THE SEAT FENCE. A launch whose turn's fence refuses after its row
// exists starts no job and leaves no row — whatever the fence refused for.
func TestALaunchWhoseFenceRefusesAfterItsRowStartsNothing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	stop := errors.New("a person paused swe and asked for its running turn to stop")
	req := launchReq("t1")
	req.Fence = func() error { return stop }

	_, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
	if !errors.Is(err, stop) {
		t.Fatalf("Launch = %v, want the fence's own refusal", err)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("a refused launch left its row behind")
	}
	if got := len(rig.runner.Started()); got != 0 {
		t.Fatalf("started %d jobs on a seat whose fence refused", got)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("a refused launch kept its seat held")
	}
}

// THE POLL LEAVES A RUN WHOSE ENDING IS DECIDED to that ending: a completion it
// announced would be refused its claim, and a keepalive would keep alive the
// very box the ending is reclaiming — a stop the node could not finish yet
// would hold its box open for as long as the poll kept reaching it.
func TestThePollLeavesARunWhoseEndingIsDecided(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	running := rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	stop := stopper
	if _, ok, err := rig.pending.DecideEnding(t.Context(), "t1", Decision{
		License: License{Fence: rigLease, WhileIn: Stoppable, Launch: running.LaunchID},
		Reclaim: true, Stop: &stop,
	}); err != nil || !ok {
		t.Fatalf("DecideEnding = %v, %v", ok, err)
	}
	box := rig.provider.Box(running.SandboxID)
	before := box.Keepalives()

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll announced %d completions of a run whose ending is decided", fired)
	}
	if after := box.Keepalives(); after != before {
		t.Fatalf("the poll kept alive the box of a run whose ending is decided (%d keepalives)", after-before)
	}
}

// A box's host gives no account of a stopped run's box in three ways, as a
// node's BoxReader reports them: it did not answer an ask that reached the
// broker, it answered that it was busy, or this node could not put the ask to
// it at all.
var (
	errHostSilent   = fmt.Errorf("asking node-a: %w", ErrBoxHostSilent)
	errHostBusy     = fmt.Errorf("%w: node node-a", ErrBoxHostBusy)
	errHostNotAsked = fmt.Errorf("%w: node-a: the broker is unreachable", ErrBoxNotAsked)
)

// stopIsKept reports whether the stop of t1 is still kept, unread: decided on
// the row, nothing charged through counters, nothing announced on queue.
func stopIsKept(t *testing.T, rig *coordRig, counters *ledgerSpy) bool {
	t.Helper()
	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("reading the stopped run: %v", err)
	}
	if !found {
		return false
	}
	if run.Ending == nil || run.Stop == nil || run.Stop.Collected {
		t.Fatalf("the stop reads ending %+v stop %+v; want it kept, unread", run.Ending, run.Stop)
	}
	if counters.asked() != 0 || len(rig.stopAnnouncements()) != 0 {
		t.Fatal("the kept stop charged or announced")
	}
	return true
}

// stopWrittenOff fails the case unless the stop of t1 ended with nothing read:
// its row gone, nothing charged through counters, announced once.
func stopWrittenOff(t *testing.T, rig *coordRig, counters *ledgerSpy) {
	t.Helper()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stop was not ended")
	}
	if got := counters.total(); got != 0 {
		t.Fatalf("charged %d tokens for a host that never answered, want none", got)
	}
	if got := len(rig.stopAnnouncements()); got != 1 {
		t.Fatalf("announced the stop %d times, want once", got)
	}
}

// nextHolder is the seat's next holder, node-c: a coordinator of its own over
// rig's store, broker and manager, whose clock reads *now and which asks the
// box's host through asks, having recovered the seat under a newer lease.
func nextHolder(t *testing.T, rig *coordRig, now *time.Time, asks BoxReader) *coordRig {
	t.Helper()
	next := newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.Pending, o.Queue, o.Manager = rig.pending, rig.queue, rig.manager
		o.Now = func() time.Time { return *now }
	})
	watchStops(next)
	next.onNode(t, "node-c", asks, &nudges{})
	if err := next.recoverSeat(t.Context(), "node-c:1", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	return next
}

// A STOP GIVES A SILENT HOST UP ON THE ASKING NODE'S OWN CLOCK. The stop's
// decision is dated by the deciding node's clock, which may stand hours from
// the asking node's; read as an age, it gave the host up at once on a node
// ahead and never on one behind. So the bound is this node's own silence: the
// host grace from its first ask that reached the broker and came back with
// nothing. An ask it could not make neither starts nor advances it; a busy
// answer means the host is there, and its silence starts again.
//
// Mutation: age the decision's instant, or count each ask alone ((a) never
// ends); count from the ask's end ((b) is kept); count an ask not made, or from
// the series' first attempt ((c) ends early); keep the silence over a busy
// answer ((d) ends early).
func TestAStopGivesASilentHostUpOnTheAskingNodesOwnClock(t *testing.T) {
	t.Parallel()
	timeout := DefaultHostGrace
	// step is one ask of the box's host: at, from the first ask, this node's
	// clock reads; the host answers err; the ask itself lasts lasts on this
	// node's clock; and the stop is ended with nothing read as it returns,
	// or kept.
	type step struct {
		at    time.Duration
		err   error
		lasts time.Duration
		ended bool
	}
	for _, tc := range []struct {
		name string
		// ahead is how far the deciding node's clock stands ahead of this
		// node's.
		ahead time.Duration
		steps []step
	}{
		{"(a) a decider ahead by the host grace and an hour", timeout + time.Hour, []step{
			{at: 0, err: errHostSilent},
			{at: timeout - time.Second, err: errHostSilent},
			{at: timeout, err: errHostSilent, ended: true},
		}},
		{"(b) one ask as long as the host grace", 0, []step{
			{at: 0, err: errHostSilent, lasts: timeout, ended: true},
		}},
		{"(c) asks this node could not make, then silence", 0, []step{
			{at: 0, err: errHostNotAsked},
			{at: timeout, err: errHostNotAsked},
			{at: timeout, err: errHostSilent},
			{at: 2*timeout - time.Second, err: errHostSilent},
			{at: 2 * timeout, err: errHostSilent, ended: true},
		}},
		{"(d) a busy answer between silences", 0, []step{
			{at: 0, err: errHostSilent},
			{at: timeout / 2, err: errHostBusy},
			{at: timeout, err: errHostSilent},
			{at: 2*timeout - time.Second, err: errHostSilent},
			{at: 2 * timeout, err: errHostSilent, ended: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			asks := &boxAsks{}
			rig.onNode(t, "node-b", asks, &nudges{})
			rig.launchedBy("t1", "node-a")
			// THE DECIDER'S CLOCK, the store handle's that dates the stop.
			rig.pending.WithClock(func() time.Time { return rig.now.Add(tc.ahead) })
			rig.stop(t)
			first := rig.now
			for i, s := range tc.steps {
				rig.now = first.Add(s.at)
				asks.answer(Result{}, s.err)
				asks.during = func() { rig.now = rig.now.Add(s.lasts) }
				rig.fireRetries()
				if s.ended {
					stopWrittenOff(t, rig, rig.accountant)
					if got := len(asks.reads()); got != i+1 {
						t.Fatalf("the host was asked %d times, want %d", got, i+1)
					}
					return
				}
				if !stopIsKept(t, rig, rig.accountant) {
					t.Fatalf("ask %d (at +%v, %v) wrote the stop off; want it kept", i, s.at, s.err)
				}
			}
			t.Fatal("no step ended the stop")
		})
	}
}

// A STOP'S RETRY ON A NODE AHEAD OF ITS DECIDER STILL ASKS ITS HOST. The seat
// moved after the stop was decided, to a node whose clock reads the decision
// more than the host grace old. That node has heard nothing from the host for
// no time at all: it asks, keeps the stop while the host is silent, and charges
// what the host reads once it answers — where it wrote the run's spend off on
// its very first ask.
//
// Mutation: give the host up by the decision's age, and the next holder writes
// the spend off at its first ask.
func TestAStopsRetryOnANodeAheadOfItsDeciderStillAsksItsHost(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{err: errHostSilent}
	rig.onNode(t, "node-b", asks, &nudges{})
	rig.launchedBy("t1", "node-a")
	rig.stop(t)
	rig.fireRetries()
	if !stopIsKept(t, rig, rig.accountant) {
		t.Fatal("the deciding node wrote the stop off at its first silent ask")
	}

	// THE SEAT MOVES, to a node whose clock stands ahead of the decider's.
	rig.coordinator.lease = notLeased
	later := rig.now.Add(DefaultHostGrace + time.Hour)
	nextAsks := &boxAsks{err: errHostSilent}
	next := nextHolder(t, rig, &later, nextAsks)
	next.fireRetries()

	if got := nextAsks.reads(); !slices.Equal(got, []BoxRead{BoxReadStop}) {
		t.Fatalf("the next holder asked the box's host %v; want one stop's read", got)
	}
	if !stopIsKept(t, rig, next.accountant) {
		t.Fatal("the next holder wrote the stop's spend off at its first ask of a silent host")
	}

	nextAsks.answer(Result{Text: "halfway", InputTokens: 4000}, nil)
	next.fireRetries()

	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stop was not finished once the host answered")
	}
	if got := next.accountant.total(); got != 4000 {
		t.Fatalf("charged %d tokens, want the 4000 the host read", got)
	}
	if got := len(rig.stopAnnouncements()); got != 1 {
		t.Fatalf("announced the stop %d times, want once", got)
	}
}

// A STOP IS GIVEN UP BY ITS ABSOLUTE BOUND WHERE NO FRAME COUNTS ITS SILENCE. A
// seat's retirement finishes its runs on its own frame, each tick asking the
// host for no longer than the tick's budget, and its release drops every series
// of the seat, so silence alone would never give a dead host up there: the run
// record and the mailbox would be kept for good. The stop the host grace and
// twice the assumed clock skew old by the retiring node's clock is given up —
// the one reading of another node's instant that never comes before the host
// grace in real time — and not a second before.
//
// Mutation: drop the absolute bound, and neither row ever ends; drop the skew
// from it, or read a frame with no series as silent since the decision, and the
// younger row ends at its first tick.
func TestAStopIsGivenUpByItsAbsoluteBoundWhereNoFrameCountsItsSilence(t *testing.T) {
	t.Parallel()
	bound := DefaultHostGrace + 2*objstore.AssumedClockSkew
	for _, tc := range []struct {
		name string
		old  time.Duration
		// ticks is how many retirement ticks, two seconds apart, end it.
		ticks int
	}{
		{"(a) a second past the bound", bound + time.Second, 1},
		{"(b) a second short of the bound", bound - time.Second, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			asks := &boxAsks{err: errHostSilent}
			rig.onNode(t, "node-b", asks, &nudges{})
			rig.launchedBy("t1", "node-a")
			rig.stop(t)
			// THE SEAT LEFT THE CHART, and its host released it: no series
			// of the stop's is left on any node.
			rig.coordinator.ReleaseSeat("swe")
			rig.now = rig.now.Add(tc.old)
			for tick := 1; ; tick++ {
				_, err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", "node-a:1", 3)
				if tick == tc.ticks {
					if err != nil {
						t.Fatalf("tick %d: RetireSeat = %v; want the stop ended", tick, err)
					}
					stopWrittenOff(t, rig, rig.accountant)
					return
				}
				if err == nil {
					t.Fatalf("tick %d ended the stop; want it kept until tick %d", tick, tc.ticks)
				}
				if !stopIsKept(t, rig, rig.accountant) {
					t.Fatalf("tick %d wrote the stop off and answered %v", tick, err)
				}
				rig.now = rig.now.Add(2 * time.Second)
			}
		})
	}
}

// AN ASK THAT OUTLIVES ITS SEAT WRITES NOTHING OFF SHORT OF THE HOST GRACE.
// The seat moved while this node's ask of the box's host was out, and its
// release dropped the stop's series: the ask came back silent with nothing on
// this node remembering when the host was first asked. What it measured is its
// own silence, and that was short of the host grace, so the stop is kept — for
// the next holder, which this node's next retry leaves it to, and which charges
// what the host reads.
//
// Mutation: read an ask with no series as a host given up, and the run's spend
// is written off as the ask returns.
func TestAnAskThatOutlivesItsSeatWritesNothingOffShortOfTheHostGrace(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{err: errHostSilent}
	rig.onNode(t, "node-b", asks, &nudges{})
	rig.launchedBy("t1", "node-a")
	rig.stop(t)
	asks.during = func() {
		rig.coordinator.ReleaseSeat("swe")
		rig.coordinator.lease = notLeased
		rig.now = rig.now.Add(DefaultHostGrace - time.Minute)
	}
	rig.fireRetries()
	if !stopIsKept(t, rig, rig.accountant) {
		t.Fatal("an ask whose seat moved under it wrote the stop's spend off")
	}

	asks.during = nil
	rig.fireRetries()
	if got := len(asks.reads()); got != 1 {
		t.Fatalf("this node asked the host %d times; once its seat moved it asks no more", got)
	}

	nextAsks := &boxAsks{result: Result{Text: "halfway", InputTokens: 4000}}
	now := rig.now
	next := nextHolder(t, rig, &now, nextAsks)
	next.fireRetries()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the next holder did not finish the stop")
	}
	if got := next.accountant.total(); got != 4000 {
		t.Fatalf("charged %d tokens, want the 4000 the host read", got)
	}
	if got := len(rig.stopAnnouncements()); got != 1 {
		t.Fatalf("announced the stop %d times, want once", got)
	}
}
