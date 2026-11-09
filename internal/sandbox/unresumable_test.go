package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// stateRetired is the judgement the engine gives a parked run whose state no
// live build reads.
func stateRetired() *Unresumable {
	return &Unresumable{Reason: types.ExecutorFailureStateRetired, Detail: "the parked turn's format was retired"}
}

// A PARKED RUN NO BUILD WILL RESUME IS SETTLED WHEN ITS SEAT IS RECOVERED: ended
// and announced once under the judgement's reason, its record gone, and its seat
// no longer counted as waiting on a person, so the seat takes work again at once.
func TestAParkedRunOnUnreadableStateIsSettledAtBoot(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.Stop()

	next := reaper(t.Context(), t, rig, rig.pending, 2, func(next *coordRig) {
		next.coordinator.unresumable = func(context.Context, PendingRun) (*Unresumable, error) {
			return stateRetired(), nil
		}
	})
	rig.finished("t1")
	got := announcedOnce(t, rig, types.ExecutorFailureStateRetired)
	if !strings.HasPrefix(got[0].failed.Detail, stateRetired().Detail) {
		t.Errorf("detail %q, want the judgement's own sentence", got[0].failed.Detail)
	}
	if held, awaits := next.coordinator.SeatRuns("swe"); held || awaits {
		t.Fatalf("SeatRuns = %v, %v after the settle, want the seat free of the run", held, awaits)
	}
}

// A PARKED RUN THE ENGINE CANNOT JUDGE, OR JUDGES RESUMABLE, WAITS FOR ITS
// ANSWER as before: counted as a question waiting on a person, nothing announced.
func TestAParkedRunNotJudgedUnresumableWaitsForItsAnswer(t *testing.T) {
	t.Parallel()
	for name, judge := range map[string]func(context.Context, PendingRun) (*Unresumable, error){
		"judged resumable": func(context.Context, PendingRun) (*Unresumable, error) { return nil, nil },
		"not judged": func(context.Context, PendingRun) (*Unresumable, error) {
			return nil, errors.New("the fleet could not be read")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			parkOnAQuestion(t, rig)
			rig.coordinator.Stop()
			next := reaper(t.Context(), t, rig, rig.pending, 2, func(next *coordRig) {
				next.coordinator.unresumable = judge
			})
			if got := rig.get("t1"); got.Status != StatusAwaiting || got.Ending != nil {
				t.Fatalf("run %q with ending %+v, want it still parked", got.Status, got.Ending)
			}
			if _, awaits := next.coordinator.SeatRuns("swe"); !awaits {
				t.Fatal("the parked run was not counted as a question waiting on a person")
			}
			if got := rig.failures(); len(got) != 0 {
				t.Fatalf("announced %+v, want nothing", got)
			}
		})
	}
}

// AN ANSWER TO A RUN NO BUILD WILL RESUME REACHES THE SEAT ONCE: the resume it
// drives is abandoned before its turn, the run is ended and announced under the
// judgement's reason saying the reply went back, and the reply is handed back to
// the seat exactly once. A later reply finds no run and is the seat's ordinary
// message, never requeued against it.
func TestAnAnswerToASettledRunReachesTheSeatOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(stateRetired())
	rig.resumer.beforeTurn = true
	asked := rig.get("t1").AskedAt

	r1 := replyAt("use main", asked.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded on the run", d)
	}
	rig.finished("t1")
	got := announcedOnce(t, rig, types.ExecutorFailureStateRetired)
	if got[0].failed.Detail != stateRetired().Detail+replyReturnedDetail {
		t.Errorf("detail %q, want it saying the reply went back to the seat", got[0].failed.Detail)
	}
	if back := rig.handedBack(); len(back) != 1 {
		t.Fatalf("handed back %v, want the reply once", back)
	}

	r2 := replyAt("use main, please", asked.Add(5*time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main, please", r2)); d != AnswerNotMine {
		t.Fatalf("R2 = %q, want it left to the seat as an ordinary message", d)
	}
	if back := rig.handedBack(); len(back) != 1 {
		t.Fatalf("handed back %v after a second reply, want only the first's copy", back)
	}
}

// A PARKED RUN HOLDS ITS SEAT FROM OTHER WORK ONLY WHILE ITS BOX WAITS PAUSED:
// past its pause it is reseeded and still takes its answer, but the seat is free.
// One no build will resume is ended there, and the seat is free too.
func TestASeatWaitingPastThePauseTTLIsHandedWork(t *testing.T) {
	t.Parallel()
	t.Run("within its pause, then past it", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		if pausing, err := rig.coordinator.SettleParked(t.Context(), "swe"); err != nil || !pausing {
			t.Fatalf("SettleParked = %v, %v within the pause, want the seat held", pausing, err)
		}
		if won, err := rig.pending.ExpirePause(t.Context(), "t1"); err != nil || !won {
			t.Fatalf("ExpirePause = %v, %v", won, err)
		}
		if pausing, err := rig.coordinator.SettleParked(t.Context(), "swe"); err != nil || pausing {
			t.Fatalf("SettleParked = %v, %v past the pause, want the seat free", pausing, err)
		}
		if got := rig.get("t1"); got.Status != StatusReseed {
			t.Fatalf("status %q, want the run still waiting for its answer", got.Status)
		}
	})
	t.Run("judged unresumable", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		rig.coordinator.unresumable = func(context.Context, PendingRun) (*Unresumable, error) {
			return &Unresumable{Reason: types.ExecutorFailureItemFinished, Detail: "the item was finished"}, nil
		}
		if pausing, err := rig.coordinator.SettleParked(t.Context(), "swe"); err != nil || pausing {
			t.Fatalf("SettleParked = %v, %v, want the run ended and the seat free", pausing, err)
		}
		rig.finished("t1")
		announcedOnce(t, rig, types.ExecutorFailureItemFinished)
		if _, awaits := rig.coordinator.SeatRuns("swe"); awaits {
			t.Fatal("the seat still counts a question waiting on a person")
		}
	})
	t.Run("a seat this node does not hold", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		rig.coordinator.lease = func(string) (Fence, bool) { return Fence{}, false }
		if _, err := rig.coordinator.SettleParked(t.Context(), "swe"); !errors.Is(err, ErrSeatNotHeld) {
			t.Fatalf("SettleParked = %v, want it refused for a seat not held", err)
		}
	})
}
