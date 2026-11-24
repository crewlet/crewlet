package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SEAT WHOSE MODELS REFUSE THEIR SIGN-IN STOPS RETRYING THE REVIEW OF ITS
// FINISHED JOB, AND SAYS WHY, rather than NAK the resume into a redelivery
// about every second until the resume give-up, each attempt a failed turn
// published as the turn's end.

// signedOut is a model whose harness refuses its sign-in while the case holds
// it out. calls counts what reached it.
type signedOut struct {
	out   atomic.Bool
	calls atomic.Int32
}

func (*signedOut) Model() string { return "opus" }

func (m *signedOut) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.calls.Add(1)
	if m.out.Load() {
		return nil, &llm.Error{Kind: llm.KindAuth, Key: "default", Err: errors.New("Not logged in · Please run /login")}
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

// THE SECOND REFUSED REVIEW PAUSES THE SEAT AS THE ENGINE; from then the
// job's announcements are refused on the pause before their claim, so the
// model is asked nothing more, and no attempt is published as the turn's end.
// Signing the model in and resuming the seat reviews the kept job once.
//
// Mutation: drop the failure hold from resumeTurn's failure (never paused);
// hand the paused attempt back as its failure, not a wait on the pause (a
// second failed resume); drop failurePauseRefusal from seatCannotRun (the
// review runs again into the refusal); publish a reverted segment as it came
// back, res not published (the failed attempts read as ended turns).
func TestAResumeWhoseModelsRefuseTheirSignInPausesTheSeatAndWaits(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	// WHAT THE GATE DID WITH EACH ANNOUNCEMENT: admitted it, or refused it on
	// the seat's pause, the witness that the waiter announced the job again.
	var admitted, refusedOnPause atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle, conversation string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle, conversation, route)
			var refused *sandbox.TurnRefused
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.As(err, &refused) && refused.Condition == waitPause:
				refusedOnPause.Add(1)
			}
			return ctx, release, err
		}
	}
	e := r.start(t)
	model := &signedOut{}
	model.out.Store(true)
	withModels(t, e, model)
	var mu sync.Mutex
	var completions []types.AgentTurnCompleted
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if done, ok := events.DataAs[*types.AgentTurnCompleted](ev); ok && done != nil && done.TurnID == "t1" {
			mu.Lock()
			completions = append(completions, *done)
			mu.Unlock()
		}
	})
	published := func() []types.AgentTurnCompleted {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(completions)
	}
	seat := seatIn(t, e, "swe")
	r.seedRun(t, e, "t1")

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	eventually(t, "the seat to be paused for its models' refusal", func() bool {
		p, paused, _ := e.pauseOf(seat)
		return paused && p.Failure != nil && p.Failure.Kind == string(failureModelsRefused)
	})
	p, _, err := e.backends.Fleet.SeatPause(t.Context(), seat)
	if err != nil || p.By != engineActor || !strings.HasPrefix(p.Reason, "Every model it can use refused its credential") {
		t.Fatalf("the pause is %+v (%v), want the engine's, saying why in a sentence of its own", p, err)
	}
	eventually(t, "the refused resume to give its claim back", func() bool {
		return r.run(t, e, "t1").Status == sandbox.StatusRunning
	})
	// THE ATTEMPT THAT PAUSED THE SEAT IS A WAIT, NOT A FAILED RESUME: only the
	// first refusal counts toward the give-up; the second was handed back on
	// the pause, not NAKed.
	if got := r.run(t, e, "t1").Launch.ResumeFailures; got != 1 {
		t.Errorf("the run counts %d failed resumes, want the first refusal alone", got)
	}

	// HELD WHILE THE PAUSE STANDS: the waiter re-announces the job (polling
	// every 50 ms) and the gate refuses each on the pause before its claim,
	// so none reaches the model or starts a turn.
	calls, settled := model.calls.Load(), admitted.Load()
	if calls != 2 {
		t.Errorf("the model was asked %d times before the pause, want the failure and its one repeat", calls)
	}
	seen := refusedOnPause.Load()
	eventually(t, "the waiter's next two announcements to be refused on the pause", func() bool {
		return refusedOnPause.Load() >= seen+2
	})
	if got := model.calls.Load(); got != calls {
		t.Fatalf("the model was asked %d more times while the seat was paused on its refusal", got-calls)
	}
	if got := admitted.Load(); got != settled {
		t.Fatalf("%d resumes were admitted while the seat was paused on its refusal, want none", got-settled)
	}
	for _, c := range published() {
		if !c.Suspended || !c.Failed {
			t.Fatalf("a refused attempt was published as %+v, want a failed segment that did not end the turn", c)
		}
	}

	// THE PERSON SIGNS THE MODEL IN AND RESUMES THE SEAT: the kept completion
	// resumes on the lift and its review reaches the model once. The waiters
	// are stopped first, so only the lift can bring the job back.
	stopWaiter(e)
	model.out.Store(false)
	settled = admitted.Load()
	if lifted, err := e.backends.Fleet.DeleteSeatPause(t.Context(), seat, p.Version); err != nil || !lifted {
		t.Fatalf("DeleteSeatPause = (%v, %v)", lifted, err)
	}
	eventually(t, "the kept job's review to reach the model once the seat was resumed", func() bool {
		return model.calls.Load() > calls
	})
	if got := admitted.Load() - settled; got != 1 {
		t.Errorf("the kept completion was admitted %d times on the lift, want once", got)
	}
}
