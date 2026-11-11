package sandbox

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// How an ending of a run waits for the calls its log holds in flight, and what
// it says of one it stops waiting for (drain.go).

// landsOnWatch is a run store that lands a call's update the moment a wait
// opens its watch of the run's log — as the node still running the call makes
// it, another node for all the collecting one knows — so the update is on the
// log for a reader that waited for the call, and for no reader that did not.
type landsOnWatch struct {
	PendingStore
	land func()

	mu      sync.Mutex
	started bool
	landed  chan struct{}
}

func (s *landsOnWatch) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	views, err := s.PendingStore.WatchLog(ctx, turnID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.started = true
		go func() {
			defer close(s.landed)
			s.land()
		}()
	}
	return views, nil
}

// updatedOnWatch puts a store on rig's coordinator that lands call's update —
// finished, with what it answered and committed — through rig's own store once
// a wait watches run's log.
func updatedOnWatch(t *testing.T, rig *coordRig, run PendingRun, call BridgeCall) {
	t.Helper()
	store := &landsOnWatch{PendingStore: rig.coordinator.pending, landed: make(chan struct{})}
	store.land = func() {
		if ok, err := rig.pending.AppendBridgeCall(context.Background(), run.TurnID, BridgeAppend{
			Launch: run.LaunchID, Round: run.Round, Call: call}); err != nil || !ok {
			t.Errorf("the call's update = %v, %v", ok, err)
		}
	}
	rig.coordinator.pending = store
	t.Cleanup(func() {
		store.mu.Lock()
		started := store.started
		store.mu.Unlock()
		if started {
			<-store.landed
		}
	})
}

// endingCalls records the run's calls as each ending handed them to the engine
// to decide the turn's end on ([CoordinatorOptions.CloseStopped],
// [CoordinatorOptions.TurnLost]).
type endingCalls struct {
	mu    sync.Mutex
	calls []RunCalls
}

func (e *endingCalls) stopped(_ context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error) {
	e.add(calls)
	return executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, StoppedBy: run.Stop.By}, nil
}

func (e *endingCalls) lost(_ context.Context, _ PendingRun, calls RunCalls) executorkeep.Ending {
	e.add(calls)
	return executorkeep.Ending{Outcome: executorkeep.OutcomeLost}
}

func (e *endingCalls) add(calls RunCalls) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, calls)
}

func (e *endingCalls) seen() []RunCalls {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// EVERY ENDING WAITS FOR A CALL IN FLIGHT BEFORE IT DECIDES THE TURN'S END. A
// person's stop and a loss whose box is reclaimed each decide the turn's end on
// the run's log — the engine's account of whether the turn replied, and of what
// it changed ([CoordinatorOptions.CloseStopped], [CoordinatorOptions.TurnLost])
// — before the ending deletes the run's record. A call the job's harness left
// running is on the log by its entry, and its update lands once it returns, on
// whichever node runs it: so the ending, made by this node's own retry
// ([errAskAway]), waits on the run's log until the entry is gone, and only then
// reads — and the turn's end is handed the call finished, with the work item it
// filed. The update is made through the store the moment the wait watches the
// log, as another node makes it, so only an ending that waited holds it.
//
// Mutation: decide the turn's end without waiting for the calls in flight
// (ending-decides-without-the-drain), and the engine is handed the call still
// in flight, its work item in no account of the turn.
func TestEveryEndingWaitsForACallInFlightBeforeItDecidesTheTurnsEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// end brings the run to its ending, which this node's retry finishes.
		end func(t *testing.T, rig *coordRig)
	}{
		{"a person's stop", func(t *testing.T, rig *coordRig) { rig.stop(t) }},
		{"a loss whose box is reclaimed", func(t *testing.T, rig *coordRig) { rig.loseOnCollect(t, "t1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			read := &endingCalls{}
			rig.coordinator.closeStopped, rig.coordinator.turnLost = read.stopped, read.lost
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			call := BridgeCall{Name: "create_work_item", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
			run := beginInFlight(t, rig, "t1", call)
			updatedOnWatch(t, rig, run, finished(call, "filed", types.CommittedItem))

			tc.end(t, rig)
			rig.fireRetries()

			handed := read.seen()
			if len(handed) != 1 {
				t.Fatalf("the turn's end was decided %d times, want once", len(handed))
			}
			logged := handed[0].Log
			if handed[0].Unread != nil || len(logged.InFlight) != 0 ||
				!slices.ContainsFunc(logged.Calls, func(c BridgeCall) bool {
					return c.Name == "create_work_item" && c.Output == "filed"
				}) {
				t.Errorf("the turn's end was decided on %+v with %+v in flight (unread: %v), want the call the "+
					"ending waited for, finished", logged.Calls, logged.InFlight, handed[0].Unread)
			}
			if !slices.Contains(logged.Changes(), string(types.CommittedItem)) {
				t.Errorf("the turn's end reads the run as changing %v, missing the work item the call filed",
					logged.Changes())
			}
			if !rig.gone("t1") {
				t.Error("the run's record outlived its ending")
			}
		})
	}
}

// A CALL STILL IN FLIGHT AT THE TURN'S CAP IS NAMED, NEVER DROPPED UNSEEN. A
// run whose turn's wall-clock cap passed is collected and ended at once
// ([Coordinator.completeAtDeadline]): the cap bounds every wait for its calls,
// so a call its harness left running is waited for no longer — named at
// warning as the collection stops waiting (`sandbox_calls_unfinished`, why: the
// turn's cap passed), handed to the turn's end as a call still in flight, its
// outcome unknown, and named again as the ending deletes the run's record
// (`sandbox_run_deleted_with_calls_in_flight`), whose update then finds no
// record. Not parallel: the lines are read off the process's own logger.
//
// Mutation: delete the record without naming what it still holds in flight
// (delete-names-nothing), and the call that ran leaves no line as its record
// goes.
func TestACappedRunNamesACallInFlightAtOnceAndAsItsRecordGoes(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	read := &endingCalls{}
	rig.coordinator.turnLost = read.lost
	// A SEAT THAT TAKES NO TURN NOW, its model at its limit: past the cap its
	// completion is collected and ended rather than kept for the limit.
	rig.coordinator.gate = func(context.Context, string, ResumeRoute) (context.Context, func(), error) {
		return nil, nil, weeklyLimit(rig.now.Add(time.Hour))
	}
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	call := BridgeCall{Name: "create_work_item", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
	beginInFlight(t, rig, "t1", call)
	rig.runner.Finish(Result{Success: true, Text: "filed the item"})

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(ctx, payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
		t.Fatalf("the run past its cap was not ended at once: failures %+v", rig.failures())
	}
	handed := read.seen()
	if len(handed) != 1 || len(handed[0].Log.InFlight) != 1 || handed[0].Log.InFlight[0].Name != "create_work_item" {
		t.Errorf("the turn's end was handed %+v, want the call still in flight, its outcome unknown", handed)
	}
	lines := strings.Split(logs.String(), "\n")
	for _, msg := range []string{"sandbox_calls_unfinished", "sandbox_run_deleted_with_calls_in_flight"} {
		if !slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, msg) && strings.Contains(line, "create_work_item at place 1")
		}) {
			t.Errorf("no %s line names the call still in flight:\n%s", msg, logs.String())
		}
	}
	if !slices.ContainsFunc(lines, func(line string) bool {
		return strings.Contains(line, "sandbox_calls_unfinished") && strings.Contains(line, "the turn's cap passed")
	}) {
		t.Errorf("the call was not said to be given up on at the turn's cap:\n%s", logs.String())
	}
}
