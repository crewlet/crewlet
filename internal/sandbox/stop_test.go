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

// A JOB ITS COLLECTION ALREADY HANDED TO ITS TURN IS THE TURN'S. A completion
// whose resume the seat's budget held, or whose resume was handed back for a
// retry, goes back to running with its job read and charged; the turn's resume
// has charged the work item the job's spend under its own segment, or will.
// Ended by the stop, its turn's close charged that spend to the item again
// under the stop's segment. So the stop leaves it — no second read of the box,
// no reclaim, no close, no announcement — and the turn's own fence ends the
// resume when it comes round, as it ends a claimed one.
//
// Mutation: drop the store's refusal and StopSeat's skip, and the box is read
// a second time, reclaimed, and the turn closed on the stop.
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
			var reads atomic.Int32
			rig.runner.CollectFunc = func(context.Context) error {
				reads.Add(1)
				return nil
			}

			rig.stop(t)

			after := rig.get("t1")
			if after.Status != StatusRunning || after.Ending != nil || after.Stop != nil {
				t.Fatalf("the collected job's run became %s (ending %+v, stop %+v); it is its turn's",
					after.Status, after.Ending, after.Stop)
			}
			if got := reads.Load(); got != 0 {
				t.Fatalf("the stop read the collected job's box again, %d times", got)
			}
			if got := rig.accountant.asked(); got != charges {
				t.Fatalf("the counters were asked %d times, %d before the stop", got, charges)
			}
			if slices.Contains(rig.provider.KilledIDs(), running.SandboxID) {
				t.Fatal("the stop reclaimed the box its turn's resume continues in")
			}
			if len(spies.closedTurns()) != 0 || len(spies.endedRuns()) != sessions || len(rig.stopAnnouncements()) != 0 {
				t.Fatalf("the stop closed %d turns, ended %d sessions and announced %d stops of a job it "+
					"does not end", len(spies.closedTurns()), len(spies.endedRuns())-sessions,
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
// for the box timeout since the stop, the bound the fleet's waiter takes a
// launch node for gone by, ends the run with nothing read.
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
			rig.now = rig.now.Add(DefaultBoxTimeout)
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

// A STOP WHOSE HOST ALONE HAS A SILENT STORE WAITS FOR IT AS FOR A SILENT HOST.
// The host is up, and so is the box: only the host's read of the run's row,
// which licenses the read, goes unanswered, while this node's own store answers
// every step of the stop's ending, as in a partition that cuts the host off from
// the store's leader and not this node. That is no verdict about the box, and
// its spend is not written off for it at once: the stop is kept, its session
// closed, nothing charged, announced or closed, and asked again, and once the
// store answers the host, it reads the box and the stop charges what it read.
// But only the host can read the box, and a host that cannot for the box
// timeout since the stop is as good as away: past it the stop is ended with
// nothing read, as a silent host's is. Unbounded, it held the seat's mail for as
// long as the partition lasted, while every write of the ending's would land.
//
// Mutation: read the store's silence as the box's failure in readStopped, and
// the stop finishes on its first retry with its spend lost; drop the bound, and
// it is kept past the box timeout.
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
		{"it does not for the box timeout", func(rig *coordRig, _ *silentStore) {
			rig.now = rig.now.Add(DefaultBoxTimeout)
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
// ENDED. Where this node's own store does not answer either, nothing the stop's
// ending writes would land, so the bound a host that alone cannot read the box
// is held to never ends it: past the box timeout too, the stop is kept, its
// spend still on the box and charged once the stores answer. Nor does its log
// say it was ended all the same on any attempt, which the record's write,
// failing, would then belie. The attempt staged here is the one that meets it:
// the retry read the row and decided the ending again, and the silence began
// as that decision landed, so the attempt's own read of the row is the first
// step this node's store does not answer, and the next attempt's read of the
// row keeps the stop before any read of the box.
//
// Mutation: bound the host's store's silence whatever the attempt's own read of
// the row met (readStopped), or hand readStopped a read that answered whatever
// it met (finishEnding), and nothing says the stop waits on a silence both
// nodes share: its read is taken for a host given up, and the stop is kept
// only by its record's write failing. (Said ended it never is, which is the
// record's to say: [TestAStopIsSaidEndedUnreadOnlyOnceItsRecordSaysSo].)
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
	rig.now = rig.now.Add(DefaultBoxTimeout)
	ownStore.arm()
	for range 2 {
		rig.fireRetries()
	}
	kept := rig.get("t1")
	if kept.Ending == nil || kept.Stop == nil || kept.Stop.Collected {
		t.Fatalf("past the box timeout, the stop on a silence both nodes share reads ending %+v stop %+v; "+
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

	hostStore.answer()
	ownStore.answer()
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

// A STOP IS SAID ENDED UNREAD ONLY ONCE ITS RECORD SAYS SO. A stop whose read of
// its box comes to nothing (the box gone, or unreadable on this node's own
// host, or its host or its host's store silent for the box timeout since the
// stop) is ended with nothing charged by its record of that (CollectStop): a
// write of this node's store, which can fall silent after the attempt's own
// read of the run's row answered. The stop is then kept for this node's retry,
// which reads the box again and charges whatever that read finds. Said before
// the write, the log told an operator that a stop still kept had ended with
// its spend written off, whatever its read had come to, and a stop kept on a
// silence both nodes share was said ended all the same. So it is said once
// the record lands, and only then, with what the read came to.
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
		{"its host silent for the box timeout", func(t *testing.T, rig *coordRig) {
			rig.onNode(t, "node-b", &boxAsks{err: fmt.Errorf("asking node-a: %w", ErrBoxHostSilent)}, &nudges{})
			rig.launchedBy("t1", "node-a")
		}, "host_silent=true"},
		{"its host's store silent for the box timeout", func(t *testing.T, rig *coordRig) {
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
			rig.now = rig.now.Add(DefaultBoxTimeout)
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
// BOX TO ITS HOST. The resumed turn's relaunch has reset the row on another node
// — a new job, its charge not yet made — and the row still names the previous
// job's box, on the host of the node that launched that job, until the launch
// attaches a box of its own. A stop decided in that window found the previous
// job's command still on the row and read that finished, collected and charged
// job back off the box, charging it a second time as this one's; and with the
// row naming this node as the box's host, it killed an id this node does not
// have and never told the host that has it.
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
	keepalives := func() int { return len(box.Timeouts()) }
	before := keepalives()

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll announced %d completions of a run whose ending is decided", fired)
	}
	if after := keepalives(); after != before {
		t.Fatalf("the poll kept alive the box of a run whose ending is decided (%d keepalives)", after-before)
	}
}
