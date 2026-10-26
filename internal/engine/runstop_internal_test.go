package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A PERSON'S STOP ENDS THE SEAT'S RUNNING WORK, ITS CODING RUN INCLUDED.
//
// The seat's turn hears a stop at its next round; the run that turn launched is
// its coding CLI's own process, with no rounds the engine sees, and it used to
// run on to its end. These cases hold the engine to the rest of the promise:
// every way a stop reaches a seat this node holds ends its running run, and
// the turn that was waiting on the run is closed as stopped — the same records
// a turn stopped at its next round leaves — wherever it was when the stop came.

// janeStops is the pause every case puts on the seat: Jane, through her machine
// token, asking for the running work to stop.
func janeStops() coord.SeatPause {
	return coord.SeatPause{
		Seat: seatOf("swe"), By: "jane", ByKind: iam.ActorHuman, OperatorID: "pat:jane-token",
		Reason: "wrong repository", StopRunning: true, At: time.Now().UTC(),
	}
}

// halfwayPlay is a run that had done some of its work, and spent on it, when
// it was read back.
func halfwayPlay(context.Context, sandbox.RunRequest) sandbox.Result {
	return sandbox.Result{Text: "halfway through the migration", InputTokens: 4000, OutputTokens: 600}
}

// parkOnRun runs swe's turn on a task to its park on its executor's run.
func parkOnRun(t *testing.T, e *Engine) {
	t.Helper()
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
}

// stopRecords is every record of one turn's end the queue carried, in order:
// its completions, its turn_completed and its stop.
type stopRecords struct {
	completions []*events.Event
	ends        []*events.Event
	stops       []*events.Event
	runStops    []*events.Event
	runLosses   []*events.Event
}

func recordsOf(q *memory.Queue, turnID string) stopRecords {
	var out stopRecords
	for _, ev := range q.History() {
		switch data := ev.Data.(type) {
		case *types.AgentTurnCompleted:
			if data.TurnID == turnID {
				out.completions = append(out.completions, ev)
			}
		case *types.TurnCompleted:
			if data.TurnID == turnID {
				out.ends = append(out.ends, ev)
			}
		case *types.AgentTurnStopped:
			if data.TurnID == turnID {
				out.stops = append(out.stops, ev)
			}
		case *types.SandboxRunStopped:
			if data.TurnID == turnID {
				out.runStops = append(out.runStops, ev)
			}
		case *types.SandboxRunFailed:
			if data.TurnID == turnID {
				out.runLosses = append(out.runLosses, ev)
			}
		}
	}
	return out
}

// closedAsStopped asserts a turn ended on the record as stopped by Jane — its
// last completion `stopped` and never `failed`, its last turn_completed an end
// rather than a park, its stop naming her — and that its run was announced as
// stopped, never lost.
func closedAsStopped(t *testing.T, q *memory.Queue, turnID string) stopRecords {
	t.Helper()
	got := recordsOf(q, turnID)
	if len(got.runLosses) != 0 {
		t.Fatalf("the stopped run was announced as a loss: %d sandbox_run_failed", len(got.runLosses))
	}
	if len(got.runStops) == 0 {
		t.Fatal("the run's stop was never announced")
	}
	if len(got.completions) == 0 {
		t.Fatal("the turn has no completion at all")
	}
	last := got.completions[len(got.completions)-1].Data.(*types.AgentTurnCompleted)
	if !last.Stopped || last.Failed || last.Suspended {
		t.Fatalf("the turn's last completion is stopped=%v failed=%v suspended=%v; want it stopped",
			last.Stopped, last.Failed, last.Suspended)
	}
	if len(got.ends) == 0 {
		t.Fatal("the turn has no turn_completed")
	}
	if end := got.ends[len(got.ends)-1].Data.(*types.TurnCompleted); end.Suspended {
		t.Fatal("the turn's last turn_completed is still a park: the turn reads parked for good")
	}
	return got
}

// A STOP ENDS A PARKED TURN'S RUNNING RUN AND CLOSES THE TURN AS STOPPED, the
// moment the pause reaches the node that holds the seat: the run ended and its
// row gone, its spend charged to the turn's task as the turn's last, stopped,
// segment, and the turn's end on the record naming who stopped it.
func TestAStopEndsTheSeatsParkedRunAndClosesItsTurnAsStopped(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)

	pause := janeStops()
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})

	if _, found, _ := e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("the stopped run is still on the record")
	}
	got := closedAsStopped(t, q, "run-1")
	if len(got.stops) != 1 {
		t.Fatalf("published %d agent_turn_stopped, want one", len(got.stops))
	}
	if stop := got.stops[0].Data.(*types.AgentTurnStopped); stop.StoppedBy != "jane" ||
		stop.OperatorID != "pat:jane-token" || stop.Reason != "wrong repository" {
		t.Fatalf("the turn's stop names %+v, want Jane's pause", stop)
	}
	turns, _ := served.recorded()
	if len(turns) != 2 {
		t.Fatalf("the task was charged %d segments, want the park and the stopped close", len(turns))
	}
	if closing := turns[1]; closing.Outcome != segmentStopped || closing.Spend.Input < 4000 ||
		closing.Spend.Output < 600 || closing.Spend.Turns != 0 {
		t.Fatalf("the closing segment charged %+v; want it stopped, paying the run's 4000/600 "+
			"tokens and counting no second turn", closing)
	}
}

// A CLOSE MADE AGAIN IS THE SAME CLOSE: every record it publishes carries the
// identity its ending recorded, so the attempt that repeats it after a crash —
// this node's retry, or the seat's next holder — is the same event to every
// store that keeps it, and the task's charge the same operation.
func TestAStoppedTurnsCloseIsTheSameRecordsEveryTime(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	row := rig.row(t, "run-1")
	row.Ending = &sandbox.RecordedEnding{ID: "ending-1", At: time.Now().UTC().Add(-time.Minute), Reclaim: true}
	row.Stop = &sandbox.RunStop{By: "jane", ByKind: "human", Collected: true, InputTokens: 4000, OutputTokens: 600}
	before := len(q.History())

	for range 2 {
		if err := e.closeStoppedTurn(t.Context(), row); err != nil {
			t.Fatalf("closeStoppedTurn: %v", err)
		}
	}

	byType := map[string][]*events.Event{}
	for _, ev := range q.History()[before:] {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}
	for _, typ := range []string{
		types.AgentTurnCompleted{}.EventType(), types.TurnCompleted{}.EventType(),
		types.AgentTurnStopped{}.EventType(),
	} {
		got := byType[typ]
		if len(got) != 2 {
			t.Fatalf("%s published %d times by two closes, want twice", typ, len(got))
		}
		if got[0].ID != got[1].ID || !got[0].Timestamp.Equal(got[1].Timestamp) {
			t.Fatalf("two closes published %s as two events (%s@%s, %s@%s), want one", typ,
				got[0].ID, got[0].Timestamp, got[1].ID, got[1].Timestamp)
		}
		if !got[0].Timestamp.Equal(row.Ending.At.UTC()) {
			t.Fatalf("%s is dated %s, want the instant the ending was decided, %s", typ,
				got[0].Timestamp, row.Ending.At)
		}
	}
	turns, ops := served.recorded()
	if len(turns) != 3 || ops[1] != ops[2] {
		t.Fatalf("two closes charged the task as %v, want the one segment twice under one operation", ops)
	}
}

// parkStopsFirst is a run store whose park lets a person's stop land first —
// the window between a launch and its park, which a stop reaches whenever the
// pause lands while the turn is unwinding.
type parkStopsFirst struct {
	sandbox.PendingStore

	mu sync.Mutex
	// before runs once, at the first park, before the park is written.
	before func()
	done   bool
}

func (s *parkStopsFirst) MarkSuspended(ctx context.Context, turnID string, sus sandbox.Suspension) (bool, error) {
	s.mu.Lock()
	before, done := s.before, s.done
	s.done = true
	s.mu.Unlock()
	if before != nil && !done {
		before()
	}
	return s.PendingStore.MarkSuspended(ctx, turnID, sus)
}

// A LAUNCH A PERSON STOPPED BEFORE ITS TURN COULD PARK CLOSES ITS OWN TURN AS
// STOPPED — not as a suspension that failed, and not as a loss — and its park
// record comes first, so the turn's end is the newer of the two on every reader
// that takes the newest.
func TestALaunchStoppedBeforeItsParkClosesItsOwnTurnAsStopped(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	store := &parkStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rigRunsOn(t, e, store, halfwayPlay)
	parkedFirst := false
	store.before = func() {
		// THE PARK RECORD IS OUT BEFORE THE PARK IS WRITTEN: what can end
		// the parked turn is only possible from the write on.
		for _, ev := range recordsOf(q, "run-1").completions {
			if ev.Data.(*types.AgentTurnCompleted).Suspended {
				parkedFirst = true
			}
		}
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	}

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})

	if !turn.Stopped(err) || res.Suspended {
		t.Fatalf("runTurn = suspended %v, %v; want the turn stopped", res.Suspended, err)
	}
	if !parkedFirst {
		t.Fatal("the turn's park was published after its park was written, so a stop that " +
			"landed in between is older than the park it closes")
	}
	closedAsStopped(t, q, "run-1")
	// AND THE TASK SAYS SO: the close is a segment of its own, never one
	// the tracker's ledger collapses into the park.
	turns, ops := served.recorded()
	if len(turns) != 2 || ops[0] == ops[1] || turns[1].Outcome != segmentStopped || turns[1].Spend.Turns != 0 {
		t.Fatalf("the task was charged %+v under %v; want the park and a stopped close of its own "+
			"counting no second turn", turns, ops)
	}
	// CHARGED ONCE: the park paid for the segment, and the close, which ran
	// nothing, pays nothing of it again — no tokens, no auxiliary calls, no
	// wall time. Built from the parking segment's telemetry, it charged that
	// segment's calls and span a second time.
	if spent := turns[1].Spend; spent.Tokens() != 0 || spent.WallMs != 0 || spent.Rounds != 0 {
		t.Fatalf("the stopped close charged %+v; the park already paid for the segment", spent)
	}
}

// A SEAT TAKEN WHILE A PERSON'S STOP IS ON IT ENDS THE RUNS ITS LAST HOLDER LEFT
// RUNNING, before its mailbox opens: the stop was the seat's, and a run the new
// holder recovered would otherwise go on to its end.
func TestASeatAcquiredUnderAStopEndsTheRunsItsLastHolderLeft(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	rig.row(t, "run-1")
	// THE PAUSE IS ON THE SEAT AS THIS NODE TAKES IT: in its copy, with no
	// watch delivery left to act on it.
	pause := janeStops()
	e.pauses.mu.Lock()
	e.pauses.pauses = map[uuid.UUID]coord.SeatPause{pause.Seat: pause}
	e.pauses.known = true
	e.pauses.mu.Unlock()
	e.markSandboxSeat("swe", false)

	if err := e.prepareSeatSandbox(t.Context(), e.sandbox.Load(),
		placement.Seat{Handle: "swe", ID: pause.Seat}, 7, "node-b:1"); err != nil {
		t.Fatalf("prepareSeatSandbox: %v", err)
	}

	if _, found, _ := e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("the seat was taken with its stopped run still running")
	}
	closedAsStopped(t, q, "run-1")
}

// A COMPLETE READ OF THE PAUSES ENDS THE RUNS A STOP ASKED FOR, which is the only
// word a node whose watch was down while the pause landed ever gets of it.
func TestACompleteReadOfThePausesEndsTheRunsAStopAskedFor(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	pause := janeStops()

	e.adoptSeatPauses(t.Context(), map[uuid.UUID]coord.SeatPause{pause.Seat: pause})

	if _, found, _ := e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("a complete read of a stop left the seat's run running")
	}
	closedAsStopped(t, q, "run-1")
}

// A PAUSE THAT DOES NOT ASK TO STOP LEAVES THE RUN RUNNING: it holds the seat's
// new work, and the run in flight is the work the person let finish.
func TestAPauseWithoutAStopLeavesTheRunRunning(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	pause := janeStops()
	pause.StopRunning = false

	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})

	if row := rig.row(t, "run-1"); row.Status != sandbox.StatusRunning || row.Ending != nil {
		t.Fatalf("a pause without a stop moved the run to %s (ending %+v)", row.Status, row.Ending)
	}
	if got := recordsOf(q, "run-1"); len(got.runStops) != 0 {
		t.Fatal("a pause without a stop announced the run stopped")
	}
}

// attachStopsFirst is a run store whose box attach lets a person's stop land
// first — a stop that reaches the seat while its executor's launch is between
// its row and its job.
type attachStopsFirst struct {
	sandbox.PendingStore

	mu     sync.Mutex
	before func()
	done   bool
}

func (s *attachStopsFirst) AttachSandbox(ctx context.Context, turnID string, box sandbox.BoxRef,
	fence sandbox.Fence) error {
	s.mu.Lock()
	before, done := s.before, s.done
	s.done = true
	s.mu.Unlock()
	if before != nil && !done {
		before()
	}
	return s.PendingStore.AttachSandbox(ctx, turnID, box, fence)
}

// A LAUNCH A STOP ENDED UNDER IT IS THE STOP: the turn reads the person's stop,
// not a launch that broke, and no job is left running in a box.
func TestALaunchAStopEndedMidwayIsReportedAsTheStop(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	store := &attachStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rig := rigRunsOn(t, e, store, halfwayPlay)
	store.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	}

	_, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})

	if !turn.Stopped(err) {
		t.Fatalf("runTurn = %v, want the turn stopped by the person who stopped its launch", err)
	}
	if got := len(rig.started()); got != 0 {
		t.Fatalf("started %d jobs for a launch a person stopped", got)
	}
	if got := recordsOf(q, "run-1"); len(got.runLosses) != 0 {
		t.Fatal("a launch a person stopped was announced as a lost run")
	}
	completions := recordsOf(q, "run-1").completions
	if len(completions) == 0 {
		t.Fatal("the stopped turn published no completion")
	}
	if last := completions[len(completions)-1].Data.(*types.AgentTurnCompleted); !last.Stopped || last.Failed {
		t.Fatalf("the turn's completion is stopped=%v failed=%v, want stopped", last.Stopped, last.Failed)
	}
}

// A STOP THAT FINISHED BEFORE ITS TURN COULD PARK IS STILL THE TURN'S END ONCE
// THE SEAT IS RESUMED. The stop ends a launching run in the time its box takes
// to die and deletes the row; the person can resume the seat before the turn
// unwinds to its park. The frame used to read the stop off the row, then off the
// pause — both gone by then — so it took the refused park for a suspension with
// nowhere to go, and the turn's last record was its park: parked for good,
// beside a run announced stopped. The frame now asks the coordinator, which
// kept the stop's decision for it.
//
// Mutation: answer the refused park from the row and the pause alone, and the
// turn is never closed.
func TestALaunchStoppedBeforeItsParkIsClosedAsStoppedOnceTheSeatIsResumed(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	store := &parkStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rigRunsOn(t, e, store, halfwayPlay)
	store.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat})
	}

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})

	if _, found, _ := e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("the stop did not finish before the park, so this case raced nothing")
	}
	if !turn.Stopped(err) || res.Suspended {
		t.Fatalf("runTurn = suspended %v, %v; want the turn stopped", res.Suspended, err)
	}
	if pause, ok := stopOf(err); !ok || pause.By != "jane" || pause.OperatorID != "pat:jane-token" {
		t.Fatalf("the turn's stop names %+v, want Jane's", pause)
	}
	closedAsStopped(t, q, "run-1")
}

// A LAUNCH A STOP ENDED UNDER IT IS THE STOP ONCE THE SEAT IS RESUMED TOO: the
// turn reads the decision the coordinator kept for it, never the pause, which
// the person may have lifted before the launch unwound — read off the pause,
// the stop was reported as a launch that broke.
//
// Mutation: ask the seat's fence rather than the coordinator, and the turn
// fails rather than stops.
func TestALaunchAStopEndedMidwayIsTheStopOnceTheSeatIsResumed(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	store := &attachStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rigRunsOn(t, e, store, halfwayPlay)
	store.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat})
	}

	_, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})

	if pause, ok := stopOf(err); !turn.Stopped(err) || !ok || pause.By != "jane" {
		t.Fatalf("runTurn = %v, want the turn stopped by Jane, who stopped its launch", err)
	}
	if _, kept := e.sandbox.Load().coordinator.LaunchStopped("run-1"); kept {
		t.Fatal("the launch's stop was left for nobody to take")
	}
}
