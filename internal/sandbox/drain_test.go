package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// beginInFlight writes call's entry on turnID's log, as the bridge of the node
// running it writes it before the call runs.
func beginInFlight(t *testing.T, rig *coordRig, turnID string, call BridgeCall) PendingRun {
	t.Helper()
	run, found, err := rig.pending.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("reading run %s: %v %v", turnID, found, err)
	}
	if ok, err := rig.pending.AppendBridgeCall(t.Context(), turnID, BridgeAppend{Step: AppendBegin,
		Launch: run.LaunchID, Round: run.Round, Call: call}); err != nil || !ok {
		t.Fatalf("the entry of %s = %v, %v", call.Name, ok, err)
	}
	return run
}

// finished is call as its update records it: its bound gone, answered output,
// and what it committed.
func finished(call BridgeCall, output string, committed ...types.CommittedChange) BridgeCall {
	call.Until, call.Output = time.Time{}, output
	for _, c := range committed {
		call.Committed = append(call.Committed, string(c))
	}
	return call
}

// endedTurns records every turn whose job's session the coordinator ended
// ([CoordinatorOptions.Ended]).
type endedTurns struct {
	mu    sync.Mutex
	turns []string
}

func (e *endedTurns) end(turnID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turnID)
}

func (e *endedTurns) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.turns)
}

// finishOnEnd makes call's update once the coordinator ends its job's session
// ([CoordinatorOptions.Ended]) — as the node still running the call makes it,
// another node's for all this one knows, through the store — and answers a
// channel closed once the update has landed, and the sessions ended.
func finishOnEnd(t *testing.T, rig *coordRig, run PendingRun, call BridgeCall) (<-chan struct{}, *endedTurns) {
	t.Helper()
	landed := make(chan struct{})
	ended := &endedTurns{}
	var start sync.Once
	rig.coordinator.ended = func(turnID string) {
		ended.end(turnID)
		start.Do(func() {
			go func() {
				defer close(landed)
				if _, err := rig.pending.AppendBridgeCall(context.Background(), run.TurnID, BridgeAppend{
					Launch: run.LaunchID, Round: run.Round, Call: call}); err != nil {
					t.Errorf("the call's update: %v", err)
				}
			}()
		})
	}
	return landed, ended
}

// EVERY COLLECTION WAITS FOR A CALL IN FLIGHT BEFORE IT READS THE LOG. A call
// its harness left running — the harness ended its own run around it, or the
// job was lost with its host — is on the run's log by its entry, written before
// it ran, and its update lands once it returns, made by whichever node runs it:
// so a collection ends the job's session to new calls, waits on the run's log
// until the entry is gone, and only then reads it, and the resume judges the
// round on the call and settles the turn's outcome by what it committed — here
// the work item it filed. The update is made through the store, as a node other
// than the one collecting makes it, so nothing of the bridge's own memory is
// read.
//
// Mutation: read the log without waiting for the calls in flight
// (collection-reads-without-the-drain), and the resume reads the work item's
// call as still in flight, its commit in no outcome.
func TestEveryCollectionWaitsForACallInFlightBeforeItReadsTheLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// collect brings the run to its collection: its job over, its
		// completion announced.
		collect func(t *testing.T, rig *coordRig)
		lost    bool
	}{
		{"a completion", func(_ *testing.T, rig *coordRig) {
			rig.runner.Finish(Result{Success: true, Text: "filed the item"})
		}, false},
		{"a job lost with its host, its round carried on", func(t *testing.T, rig *coordRig) {
			rig.runner.Lose(Result{Text: "the host went"})
			if fired := rig.tick(); fired != 1 {
				t.Fatalf("announced %d completions for a lost job, want 1", fired)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			call := BridgeCall{Name: "create_work_item", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
			run := beginInFlight(t, rig, "t1", call)
			landed, ended := finishOnEnd(t, rig, run, finished(call, "filed", types.CommittedItem))

			tc.collect(t, rig)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(ctx, payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			select {
			case <-landed:
			case <-ctx.Done():
				t.Fatal("the call's update never landed: nothing ended the job's session")
			}
			if !slices.Contains(ended.all(), "t1") {
				t.Error("the collection read the run without ending its job's session to new calls")
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 || (calls[0].Lost != nil) != tc.lost {
				t.Fatalf("resumed %+v, want the round read once (lost: %v)", calls, tc.lost)
			}
			logged := calls[0].Bridged
			if len(logged.InFlight) != 0 || !slices.ContainsFunc(logged.Calls, func(c BridgeCall) bool {
				return c.Name == "create_work_item" && c.Output == "filed"
			}) {
				t.Errorf("the resume read %+v and %+v in flight, want the call the collection waited for, "+
					"finished", logged.Calls, logged.InFlight)
			}
			if !slices.Contains(logged.Changes(), string(types.CommittedItem)) {
				t.Errorf("the turn's outcome is settled by %v, missing the work item the call filed",
					logged.Changes())
			}
		})
	}
}

// A WAIT STOPS AT THE TURN'S CAP, AT A CALL'S OWN BOUND, AND AT ITS PROCESS GONE,
// AND NAMES THE CALL. A collection holds the run's seat while it waits, so the
// wait is bounded: by the turn's wall-clock cap where it has one, by the call's
// own bound, which its entry carries since most turns have no cap, and by its
// runner's taker lease held at no epoch — a crash's calls are never recorded,
// and are given up within a lease's lifetime rather than at their bound, read
// as the runner first appears rather than a tick later. A call given up on is
// handed back, with why, for the read that follows to read as one whose outcome
// is unknown; never dropped unseen. And a runner whose lease is held is waited
// for.
//
// Mutation: ignore the turn's cap (drain-ignores-the-turns-cap), or a call's own
// bound (drain-ignores-a-calls-own-bound), and the wait for that case runs to
// the case's own deadline.
func TestAWaitStopsAtTheCapAtACallsBoundAndAtItsProcessGone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		until    time.Duration
		runner   string
		why      string
	}{
		{"the turn's cap", -time.Second, time.Hour, "node-a:alive", "the turn's cap passed"},
		{"its own bound", 0, -time.Second, "node-a:alive", "its own bound passed"},
		{"its process gone", 0, time.Hour, "node-a:crashed", "its process stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newTakerStore()
			rig := newCoordRigWith(t, taking("node-b:collector", store))
			claimTaker(t, store, "node-a:alive")
			rig.launch("t1")
			call := BridgeCall{Name: "delegate", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(tc.until),
				Runner: tc.runner, Digest: strings.Repeat("d", 64)}
			run := beginInFlight(t, rig, "t1", call)
			if tc.deadline != 0 {
				run.Deadline = rig.now.Add(tc.deadline)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			left, held, err := rig.coordinator.awaitCalls(ctx, run)
			if err != nil {
				t.Fatalf("the wait ended %v, want it stopped by %s", err, tc.why)
			}
			if held != 1 || len(left) != 1 || left[0].why != tc.why || left[0].call.Name != "delegate" {
				t.Errorf("the wait saw %d in flight and stopped waiting for %+v, want the call named, %q",
					held, left, tc.why)
			}
		})
	}
	// THE CONTROL: a call within its bound whose runner holds its lease is
	// still waited for, so each stop above is its case's and nothing else's.
	t.Run("a live runner within the bound", func(t *testing.T) {
		t.Parallel()
		store := newTakerStore()
		rig := newCoordRigWith(t, taking("node-b:collector", store))
		claimTaker(t, store, "node-a:alive")
		rig.launch("t1")
		call := BridgeCall{Name: "delegate", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour),
			Runner: "node-a:alive"}
		run := beginInFlight(t, rig, "t1", call)
		running, left, err := rig.coordinator.callsStillRunning(t.Context(), run)
		if err != nil || !running || len(left) != 0 {
			t.Errorf("a live runner's call within its bound = running %v, given up %+v, %v; want it waited for",
				running, left, err)
		}
	})
}

// brokenWatch is a run store whose first watch of a log stops hearing at once,
// having delivered nothing, and whose second lands an update once it is open.
type brokenWatch struct {
	PendingStore
	land    func()
	mu      sync.Mutex
	watches int
}

func (s *brokenWatch) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	s.mu.Lock()
	s.watches++
	n := s.watches
	s.mu.Unlock()
	if n == 1 {
		closed := make(chan LogView)
		close(closed)
		return closed, nil
	}
	views, err := s.PendingStore.WatchLog(ctx, turnID)
	if err == nil && n == 2 {
		go s.land()
	}
	return views, err
}

func (s *brokenWatch) opened() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches
}

// A WAIT WHOSE WATCH STOPS HEARING WATCHES AGAIN, AND MISSES NOTHING. A closed
// watch says nothing of the log — the store stopped telling, nothing was
// removed — so the wait watches again, and the new watch starts from the log
// as the store holds it now: an update that lands meanwhile, or after, ends
// the wait, and the wait never ends on the broken watch alone.
//
// Mutation: end the wait when its watch stops hearing, and it returns with the
// call still in flight.
func TestAWaitWhoseWatchStopsHearingWatchesAgainAndMissesNothing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	call := BridgeCall{Name: "post_message", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
	run := beginInFlight(t, rig, "t1", call)
	store := &brokenWatch{PendingStore: rig.coordinator.pending, land: func() {
		if _, err := rig.pending.AppendBridgeCall(context.Background(), "t1", BridgeAppend{Launch: run.LaunchID,
			Round: run.Round, Call: finished(call, "posted")}); err != nil {
			t.Errorf("the call's update: %v", err)
		}
	}}
	rig.coordinator.pending = store
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	left, _, err := rig.coordinator.awaitCalls(ctx, run)
	if err != nil || len(left) != 0 {
		t.Fatalf("the wait = %+v, %v; want it ended by the call's update", left, err)
	}
	if n := store.opened(); n != 2 {
		t.Errorf("the wait watched the log %d times, want once more after the watch that broke", n)
	}
	logged, err := rig.pending.BridgeLog(t.Context(), run)
	if err != nil || len(logged.InFlight) != 0 || len(logged.Calls) != 1 {
		t.Errorf("as the wait ended the log held %+v in flight and %+v (%v), want the call finished",
			logged.InFlight, logged.Calls, err)
	}
}

// A PARKED JOB'S SESSION IS ENDED AT ITS COLLECTION. A run that asked a person
// something is collected and parked on the question; its collection, like every
// collection, ends the job's session on the bridge before it reads the log, so
// a call the asking box makes after it is refused rather than landing — the
// answer's relaunch opens a session of its own, under a job of its own.
//
// Mutation: collect a park without the drain (collection-reads-without-the-drain),
// and the session the asking box holds stays open.
func TestAParkedJobsSessionIsEndedAtItsCollection(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	ended := &endedTurns{}
	rig.coordinator.ended = ended.end
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if run := rig.get("t1"); run.Status != StatusAwaiting {
		t.Fatalf("the run is %s, want it parked on its question", run.Status)
	}
	if !slices.Contains(ended.all(), "t1") {
		t.Errorf("the park's collection ended the sessions of %v, want the asking job's", ended.all())
	}
}

// AN ENDING THAT MAY NOT WAIT HANDS A CALL STILL WAITED FOR TO ITS RETRY, which
// waits for it. An ending reached on a frame somebody waits on — a completion's
// handler, a seat's acquisition — decides the turn's end on the run's log only
// where the log holds no call a collection would still wait for, and hands the
// ending to this node's retry where it does; the retry waits for the call, and
// then reads. A call past its own bound is waited for by nobody, so it hands
// nothing on.
//
// Mutation: report no call still running (calls-still-running-says-none), and
// the turn's end is decided on a frame without the call.
func TestAnEndingThatMayNotWaitHandsACallStillWaitedForToItsRetry(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("reading the run: %v %v", found, err)
	}
	if err := rig.coordinator.drainEnding(t.Context(), run, false); err != nil {
		t.Fatalf("an ending of a run with no call in flight = %v, want it decided here", err)
	}
	past := BridgeCall{Name: "read_page", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(-time.Second)}
	beginInFlight(t, rig, "t1", past)
	if err := rig.coordinator.drainEnding(t.Context(), run, false); err != nil {
		t.Fatalf("an ending beside a call past its own bound = %v, want it decided here", err)
	}
	call := BridgeCall{Name: "post_message", Seq: 2, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
	run = beginInFlight(t, rig, "t1", call)
	if err := rig.coordinator.drainEnding(t.Context(), run, false); !errors.Is(err, errAskAway) {
		t.Fatalf("an ending that may not wait, with a call in flight = %v, want it handed to its retry", err)
	}
	landed, _ := finishOnEnd(t, rig, run, finished(call, "posted"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := rig.coordinator.drainEnding(ctx, run, true); err != nil {
		t.Fatalf("the retry's wait = %v", err)
	}
	select {
	case <-landed:
	case <-ctx.Done():
		t.Fatal("the call's update never landed")
	}
	logged, err := rig.pending.BridgeLog(t.Context(), run)
	if err != nil || len(logged.InFlight) != 1 || logged.InFlight[0].Name != "read_page" ||
		len(logged.Calls) != 1 || logged.Calls[0].Name != "post_message" {
		t.Errorf("after the retry's wait the log holds %+v in flight and %+v (%v), want the call it "+
			"waited for finished and the one past its bound still in flight", logged.InFlight, logged.Calls, err)
	}
	// AND THE CALL NOBODY WAITED FOR, its outcome unknown to every read so far,
	// is listed with its outcome once its update lands while the record stands.
	if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: run.LaunchID,
		Round: run.Round, Call: finished(past, "read")}); err != nil || !ok {
		t.Fatalf("the late update of the call past its bound = %v, %v", ok, err)
	}
	logged, err = rig.pending.BridgeLog(t.Context(), run)
	if err != nil || len(logged.InFlight) != 0 || !slices.ContainsFunc(logged.Calls, func(c BridgeCall) bool {
		return c.Name == "read_page" && c.Output == "read"
	}) {
		t.Errorf("after its update the log holds %+v in flight and %+v (%v), want the call past its bound "+
			"listed with its outcome", logged.InFlight, logged.Calls, err)
	}
}

// A SEAT'S RETIREMENT LEAVES A RUN WHOSE CALL STILL RUNS TO ITS NEXT TICK. A
// retirement ends every run of its seat in one loop, under the seat's lease and
// its own budget, and gives the seat up after it — this node's retry of an
// ending with it — so a run whose log holds a call a collection would still
// wait for is neither waited for in the loop, which would hold every later run
// of the seat behind it, nor handed to that retry, which would end it never:
// its session is ended to new calls and it is answered as not ended, its
// ending undecided, which keeps the retirement for its next tick; and that tick
// ends it once the call is recorded.
//
// Mutation: end the run whatever its log holds (calls-still-running-says-none),
// and the retirement decides its ending with the call still running.
func TestASeatsRetirementLeavesARunWhoseCallStillRunsToItsNextTick(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	call := BridgeCall{Name: "delegate", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
	run := beginInFlight(t, rig, "t1", call)
	ended := &endedTurns{}
	rig.coordinator.ended = ended.end

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := rig.coordinator.RetireSeat(ctx, sweSeat(), "swe", "retirement:1", 12); !errors.Is(err,
		errCallsStillRunning) {
		t.Fatalf("a retirement beside a call still running = %v, want the run left for its next tick", err)
	}
	if row, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || !found || row.Ending != nil {
		t.Fatalf("the run is %+v (%v, %v) after the retirement, want it on the record, its ending undecided",
			row.Ending, found, err)
	}
	if !slices.Contains(ended.all(), "t1") {
		t.Errorf("the retirement left the sessions of %v open, want the run's ended to new calls", ended.all())
	}

	if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: run.LaunchID,
		Round: run.Round, Call: finished(call, "delegated")}); err != nil || !ok {
		t.Fatalf("the call's update = %v, %v", ok, err)
	}
	if err := rig.coordinator.RetireSeat(ctx, sweSeat(), "swe", "retirement:1", 12); err != nil {
		t.Fatalf("the retirement's next tick = %v, want the run ended", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Errorf("the run is on the record after the next tick (%v, %v), want it ended", found, err)
	}
}
