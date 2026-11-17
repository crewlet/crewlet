package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// A PERSON'S ANSWER ARRIVING MID-TURN IS RECORDED AT ONCE AND RESUMED AS THE
// SEAT'S NEXT TURN. It arrives on the seat's control topic, which no mailbox
// turn waits for; a seat runs one turn at a time ([node.ErrSeatBusy]), so the
// resume waits for the running turn and runs once it is released
// ([node.Config.SeatTurnEnded]), not at the five-minute backstop. Drives the
// engine's own coordinator, gate, control-topic subscription and node hook.
//
// Mutation: map the node's busy refusal to no condition, or drop the hook from
// the node's config, and the resume waits for the backstop; drop the per-seat
// rule from the node and it starts beside the running turn.
func TestAnAnswerArrivingMidTurnIsResumedWhenTheTurnEnds(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)
	// A company with models: the rig's has none, and a seat with no model
	// runs no turn, so the answer would wait on that instead.
	withModels := *e.Company()
	withModels.Models = refusingModels(t)
	e.epoch.current.Store(&withModels)
	if refusal, refused := e.mayResumeAnswer(t.Context(), "swe"); refused {
		t.Fatalf("the premise: the seat's own conditions refuse a resume (%s)", refusal.Reason)
	}
	r.seedRun(t, e, "t1")
	sandboxtest.Ask(t, sandbox.NewCoordStore(e.backends.Fleet), "t1",
		sandboxtest.Question("which branch?", sandbox.AskToRequester))
	r.runner.Finish(sandbox.Result{})
	eventually(t, "the run to park on its question", func() bool {
		return slices.Contains(sandbox.Awaiting, r.run(t, e, "t1").Status)
	})
	parked := r.run(t, e, "t1")

	// THE SEAT'S NEXT TURN, once the park's own has let go of it.
	var release func()
	eventually(t, "a turn to be admitted on the seat", func() bool {
		_, admitted, err := e.node.AdmitTurn(t.Context(), "swe")
		release = admitted
		return err == nil
	})
	t.Cleanup(release)
	_, next, err := e.admitResume(t.Context(), "swe", sandbox.ResumeAnswer)
	var refused *sandbox.TurnRefused
	if err == nil {
		next()
	}
	if !errors.As(err, &refused) || refused.Condition != waitSeatBusy || !errors.Is(err, node.ErrSeatBusy) {
		t.Fatalf("a resume on a seat mid-turn = %v, want it waiting on %q with the node's "+
			"busy refusal as its cause", err, waitSeatBusy)
	}

	desk := sandbox.AnswerDesk{Pending: sandbox.NewCoordStore(e.backends.Fleet), Queue: e.backends.Queue}
	if _, err := desk.Deliver(t.Context(), types.ExecutorAnswerGiven{
		TurnID: "t1", AgentHandle: "swe", Answer: "stack it on gp-4",
		AnsweredBy: "founder", AnsweredByKind: "human", LaunchID: parked.LaunchID,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	eventually(t, "the answer to be recorded on its run while the seat is mid-turn", func() bool {
		return r.run(t, e, "t1").Status == sandbox.StatusAnswered
	})
	recorded := r.run(t, e, "t1")
	time.Sleep(300 * time.Millisecond)
	if got := r.run(t, e, "t1"); got.Status != sandbox.StatusAnswered || !got.UpdatedAt.Equal(recorded.UpdatedAt) {
		t.Fatalf("run = %q, written at %s after the record at %s, while the seat's turn was still "+
			"running: want it answered and untouched, waiting for that turn",
			got.Status, got.UpdatedAt, recorded.UpdatedAt)
	}

	// The claim's row write is the resume starting: this seat's model has no
	// in-box runner, so the relaunch is refused and the turn goes no further.
	release()
	eventually(t, "the answer's resume to start once the seat's turn ended", func() bool {
		run, found, err := sandbox.NewCoordStore(e.backends.Fleet).Get(t.Context(), "t1")
		return err == nil && (!found || run.UpdatedAt.After(recorded.UpdatedAt))
	})
}
