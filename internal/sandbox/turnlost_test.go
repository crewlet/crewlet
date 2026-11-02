package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A TURN LOST WITH ITS RUN IS HANDED TO THE ENGINE, as a step of the run's
// ending: after its announcement, before its delete, once per attempt — and only
// for a loss. A loss is the one terminal of a parked turn no frame of that turn
// ends, so the engine's answer to the colleague whose ask woke the turn has to
// come from here ([CoordinatorOptions.TurnLost]); a run whose turn came back,
// or was stopped, is answered by the frame that ended it.

// lostSpy records what an ending hands [CoordinatorOptions.TurnLost], and what
// stood when it did.
type lostSpy struct {
	rig *coordRig

	mu    sync.Mutex
	calls []lostCall
}

type lostCall struct {
	run PendingRun
	// recorded is whether the run's record still existed: a step made after
	// the delete is lost to a crash between the two.
	recorded bool
	// announced is how many announcements of the loss had been published: a
	// turn told about before its loss is on the record is told about a loss
	// that may yet be kept and decided again.
	announced int
}

// watchLosses installs the spy on the rig's coordinator.
func watchLosses(rig *coordRig) *lostSpy {
	spy := &lostSpy{rig: rig}
	rig.coordinator.turnLost = spy.lost
	return spy
}

func (s *lostSpy) lost(ctx context.Context, run PendingRun) {
	_, recorded, _ := s.rig.pending.Get(ctx, run.TurnID)
	announced := len(s.rig.announcements())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, lostCall{run: run, recorded: recorded, announced: announced})
}

func (s *lostSpy) seen() []lostCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// askedBy is the ask a colleague put to the turn every case here launches.
var askedBy = ColleagueAsk{ChannelID: "a2a-1", Question: "Which branch carries the fix?",
	Chain: []string{"ceo"}}

// launchAsked launches a running run for a turn a colleague's ask woke, its row
// carrying the ask as a real launch writes it ([TurnRef.ColleagueAsk]).
func (r *coordRig) launchAsked(turnID string) {
	r.t.Helper()
	r.launchingAs(turnID, rigLease, func(run *PendingRun) {
		asked := askedBy
		run.ColleagueAsk = &asked
	})
	r.suspend(turnID)
	r.coordinator.countRun("swe", StatusRunning)
}

// loseOnCollect delivers turnID's completion with its box gone, so the
// collection gives the run up at once ([Coordinator.collectFailed]).
func (r *coordRig) loseOnCollect(t *testing.T, turnID string) {
	t.Helper()
	r.runner.Finish(Result{Success: true})
	r.runner.CollectErr = errBoxGoneMidRead
	payload, ev := r.completion(turnID)
	if err := r.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
}

// A LOST RUN HANDS ITS TURN TO THE ENGINE ONCE, AS IT STANDS: the ask the turn
// carries and the ending that says why it was lost, after the loss is announced
// and while the record still exists.
//
// Mutation: drop the step from the ending, or make it after the delete, and
// this goes red.
func TestALostRunHandsItsTurnToTheEngineBeforeItsRecordGoes(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchLosses(rig)
	rig.launchAsked("t1")

	rig.loseOnCollect(t, "t1")

	calls := spy.seen()
	if len(calls) != 1 {
		t.Fatalf("the lost run was handed over %d times, want once", len(calls))
	}
	got := calls[0]
	if got.run.ColleagueAsk == nil || got.run.ColleagueAsk.ChannelID != askedBy.ChannelID ||
		got.run.ColleagueAsk.Question != askedBy.Question {
		t.Errorf("the engine was handed ask %+v, want the one the turn's row carries", got.run.ColleagueAsk)
	}
	if got.run.Ending == nil || got.run.Ending.Reason != types.SandboxFailureCollect ||
		got.run.Ending.Detail == "" {
		t.Errorf("the engine was handed ending %+v, want the loss with its reason and its sentence",
			got.run.Ending)
	}
	if !got.recorded {
		t.Error("the run was handed over after its record was deleted: a crash between the two " +
			"leaves the asker unanswered by every later attempt")
	}
	if got.announced != 1 {
		t.Errorf("the run was handed over with %d announcements of its loss out, want it after the one",
			got.announced)
	}
	rig.finished("t1")
}

// AN ENDING KEPT BEFORE ITS ANNOUNCEMENT HANDS NOTHING OVER UNTIL THE RETRY THAT
// ANNOUNCES IT: the broker refuses the announcement, the ending is kept, and the
// retry that publishes it hands the turn over then — once.
//
// Mutation: make the step before the announcement, and the turn is handed over
// by the attempt the broker refused, and again by its retry.
func TestALostRunIsHandedOverOnlyOnceItsLossIsAnnounced(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchLosses(rig)
	rig.launchAsked("t1")
	rig.failPublishes(errors.New("the broker is unreachable"))

	rig.loseOnCollect(t, "t1")
	if calls := spy.seen(); len(calls) != 0 {
		t.Fatalf("the turn was handed over %d times by an ending whose loss was never announced", len(calls))
	}

	rig.failPublishes(nil)
	if fired := rig.fireRetries(); fired == 0 {
		t.Fatal("the kept ending scheduled no retry")
	}
	calls := spy.seen()
	if len(calls) != 1 || calls[0].announced == 0 || !calls[0].recorded {
		t.Fatalf("the retry handed the turn over as %+v, want once, after the announcement, "+
			"before the delete", calls)
	}
	rig.finished("t1")
}

// ONLY A LOSS IS HANDED OVER. A run whose turn came back was answered by the
// turn's own resume, a stopped one by the stop's close, and an abandoned resume
// whose turn ran by that turn; handed over too, the asker would be told a turn
// that answered it was lost. A resume that broke before its turn began, and a
// suspension the engine could not record, are losses: no frame of the turn
// answers either.
//
// Mutation: hand over every ending that announces anything, or every ending,
// and the cases that are not losses go red; hand over none, and the losses do.
func TestOnlyALossHandsItsTurnToTheEngine(t *testing.T) {
	t.Parallel()
	abandoned := fmt.Errorf("%w: the reviewer's provider went away", ErrResumeAbandoned)
	for _, tc := range []struct {
		name  string
		drive func(*testing.T, *coordRig)
		lost  bool
	}{
		{"collected, its turn resumed and ended", func(t *testing.T, rig *coordRig) {
			rig.launchAsked("t1")
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, false},
		{"stopped by a person", func(t *testing.T, rig *coordRig) {
			rig.launchAsked("t1")
			rig.stop(t)
		}, false},
		{"its resume abandoned after its turn ran", func(t *testing.T, rig *coordRig) {
			rig.launchAsked("t1")
			rig.resumer.failWith(abandoned)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, false},
		{"its resume broken before its turn began", func(t *testing.T, rig *coordRig) {
			rig.launchAsked("t1")
			rig.resumer.mu.Lock()
			rig.resumer.beforeTurn = true
			rig.resumer.mu.Unlock()
			rig.resumer.failWith(abandoned)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, true},
		{"its box gone at its collection", func(t *testing.T, rig *coordRig) {
			rig.launchAsked("t1")
			rig.loseOnCollect(t, "t1")
		}, true},
		{"its suspension never recorded", func(t *testing.T, rig *coordRig) {
			rig.launchingAs("t1", rigLease, func(run *PendingRun) {
				asked := askedBy
				run.ColleagueAsk = &asked
			})
			if err := rig.coordinator.FailRun(t.Context(), "t1",
				types.SandboxFailureSuspensionUnrecorded, "the parked turn state could not be written"); err != nil {
				t.Fatalf("FailRun: %v", err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spy := watchLosses(rig)
			tc.drive(t, rig)
			rig.finished("t1")
			calls := spy.seen()
			switch {
			case tc.lost && (len(calls) != 1 || calls[0].run.ColleagueAsk == nil):
				t.Fatalf("a lost run was handed over as %+v, want once, carrying its ask", calls)
			case !tc.lost && len(calls) != 0:
				t.Fatalf("a run that was not lost was handed over %d times as one: its asker would "+
					"be told a turn that answered it was lost", len(calls))
			}
		})
	}
}
