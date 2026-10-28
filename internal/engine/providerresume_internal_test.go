package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RESUME NEVER RUNS INTO A LIMIT ITS MODELS ALREADY KNOW, and one that meets
// a limit it did not know waits for it: neither is a failure.
//
// What a running engine did: a coding run's completion reached a seat whose
// models were all at their usage limit, the resume ran a turn into the limit,
// the turn was reverted, and the control topic NAKed the completion, which the
// broker redelivered into the same limit 25 times in about ten minutes.

// spentEntry is a model whose CLI already reported its usage limit, as
// cliagent's entry knows it ([chain.Unready]), until the case lifts it.
type spentEntry struct {
	limited atomic.Bool
	retry   time.Time
}

func (*spentEntry) Model() string { return "opus" }

func (m *spentEntry) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	if err := m.Unready(ctx); err != nil {
		return nil, err
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

func (m *spentEntry) Unready(context.Context) error {
	if !m.limited.Load() {
		return nil
	}
	return &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: m.retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// THE GATE REFUSES A RESUME WHOSE MODELS ARE AT A LIMIT THEY KNOW, before the
// claim and with the instant they are tried again, and the control topic
// acknowledges that refusal; a resume that met the limit mid-turn is refused
// the same way, and a spent window as its own wait.
//
// Mutation: drop the seat's conditions from admitResume and the limited seat
// is admitted; drop the mapping from the resume's failure and a limit met
// mid-turn is a plain error, which the control topic NAKs.
func TestAResumeAtItsModelsLimitIsAWaitNotAFailure(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)
	retry := time.Now().Add(14 * time.Minute).UTC().Truncate(time.Second)
	entry := &spentEntry{retry: retry}
	entry.limited.Store(true)
	withModels(t, e, entry)
	turns := e.node.InFlightTurns()

	_, release, err := e.admitResume(t.Context(), "swe")
	if err == nil {
		// Admitted: the slot is given back before the case fails, so the
		// engine's stop does not wait on it.
		release()
	}
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != waitProvider || !refused.Until.Equal(retry) {
		t.Fatalf("admitResume on a limited seat = %v, want a refusal waiting on %q until %v",
			err, waitProvider, retry)
	}
	if got := e.node.InFlightTurns(); got != turns {
		t.Errorf("a refused resume kept the node's admission: %d turns in flight, want %d", got, turns)
	}
	if got := controlOutcome(err).Outcome; got != queue.OutcomeAck {
		t.Errorf("the control topic hands a limited completion back as %s, want ack", got)
	}

	limitedChain := &chain.Error{Attempted: []string{"default"},
		Failures: []error{entry.Unready(t.Context())}, Err: entry.Unready(t.Context())}
	if refusal, ok := resumeRefusal(limitedChain); !ok ||
		refusal.Condition != waitProvider || !refusal.Until.Equal(retry) {
		t.Errorf("a resume that met the limit = %+v (%v), want it waiting on %q until %v",
			refusal, ok, waitProvider, retry)
	}
	// A SPENT WINDOW IS NEVER THIS REFUSAL'S: the segment is held on its
	// window before the resume is judged ([Engine.heldByBudget]), and one
	// that reaches here has turned over since.
	if _, ok := resumeRefusal(toolloop.ErrBudgetExhausted); ok {
		t.Error("a spent window was read as the provider's wait")
	}
	if _, ok := resumeRefusal(errors.New("server error")); ok {
		t.Error("a failure that is not a wait was read as one")
	}

	entry.limited.Store(false)
	if _, release, err := e.admitResume(t.Context(), "swe"); err != nil {
		t.Fatalf("admitResume once the limit lifted: %v", err)
	} else {
		release()
	}
}

// A COMPLETION THAT ARRIVES DURING A LIMIT IS NEVER HANDED BACK, AND RESUMES
// ONCE WHEN IT LIFTS.
//
// Every announcement the waiter makes while the limit holds is refused at the
// gate and acknowledged: none comes back from the broker, the control topic is
// never quiesced, and the run stays unclaimed with its box uncollected. When
// the limit lifts, the signal the provider park's release sends re-fires the
// kept completion, and the run is collected once.
//
// Mutation: NAK a refusal in controlOutcome and the broker hands the refused
// completions back after the waiter has stopped announcing them.
func TestACompletionDuringALimitIsNeverHandedBackAndResumesOnce(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	var refusals atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle string) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle)
			var refused *sandbox.TurnRefused
			if errors.As(err, &refused) && refused.Condition == waitProvider {
				refusals.Add(1)
			}
			return ctx, release, err
		}
	}
	e := r.start(t)
	entry := &spentEntry{retry: time.Now().Add(time.Hour)}
	entry.limited.Store(true)
	withModels(t, e, entry)
	var announced atomic.Int32
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if ev.Type == (types.SandboxRunCompleted{}).EventType() {
			announced.Add(1)
		}
	})
	r.seedRun(t, e, "t1")
	handlers := e.backends.Queue.InFlightCount()

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	eventually(t, "the waiter's completions to be refused for the limit", func() bool {
		return refusals.Load() >= 2
	})
	stopWaiters(e)
	eventually(t, "the refused completions' handlers to return", func() bool {
		return e.backends.Queue.InFlightCount() == handlers
	})
	settled, sent := refusals.Load(), announced.Load()
	if settled > sent {
		t.Fatalf("%d refusals for %d announcements: the broker handed refused completions back",
			settled, sent)
	}
	// THE BROKER'S FAILURE BACKOFF IS A SECOND, doubling: a NAKed
	// completion would be back well inside this, with nothing announcing it.
	time.Sleep(2500 * time.Millisecond)
	if got := refusals.Load(); got != settled {
		t.Fatalf("refusals rose from %d to %d after the waiter stopped: a refused completion "+
			"was handed back", settled, got)
	}
	if deferred(t, e) {
		t.Fatal("a completion refused for a limit quiesced the seat's control topic")
	}
	if got := r.run(t, e, "t1"); got.Status != sandbox.StatusRunning || got.Collected {
		t.Fatalf("run = %q (collected %v) while the limit held, want it unclaimed", got.Status, got.Collected)
	}

	entry.limited.Store(false)
	e.readmitResumes(waitProvider)
	eventually(t, "the run to be collected once the limit lifted", func() bool {
		return r.collected.Load() > 0
	})
	eventually(t, "the run to leave running", func() bool {
		return r.run(t, e, "t1").Status != sandbox.StatusRunning
	})
	if got := r.collected.Load(); got != 1 {
		t.Fatalf("the run was collected %d times, want once", got)
	}
}

// midTurnLimit is a model whose entry has reported nothing yet, so the turn
// gate admits a resume, and whose call refuses for a usage limit while the case
// holds it: the limit a resumed turn meets a round in.
type midTurnLimit struct {
	limited atomic.Bool
	retry   time.Time
	calls   atomic.Int32
}

func (*midTurnLimit) Model() string { return "opus" }

func (m *midTurnLimit) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.calls.Add(1)
	if m.limited.Load() {
		return nil, m.limitedErr()
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

// limitedErr is the refusal the model makes while it is limited.
func (m *midTurnLimit) limitedErr() error {
	return &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: m.retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// limitedResume is a drain rig whose swe seat's coding run has finished and
// whose resume has met the model's limit mid-turn, with the waiter stopped so
// nothing but a signal can bring the kept completion back: the engine, the
// model, how many times the gate admitted a resume, and how many it had once
// the refusal settled.
type limitedResume struct {
	r        *drainRig
	e        *Engine
	model    *midTurnLimit
	admitted *atomic.Int32
	settled  int32
}

func newLimitedResume(t *testing.T) *limitedResume {
	t.Helper()
	r := newDrainRig(t)
	var admitted atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle string) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle)
			if err == nil {
				admitted.Add(1)
			}
			return ctx, release, err
		}
	}
	e := r.start(t)
	model := &midTurnLimit{retry: time.Now().Add(time.Hour)}
	model.limited.Store(true)
	withModels(t, e, model)
	r.seedRun(t, e, "t1")
	handlers := e.backends.Queue.InFlightCount()

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	// THE REAL RESUME, into the limit: the run is claimed, collected, and
	// its turn calls the model, which refuses.
	eventually(t, "the resumed turn to meet the model's limit", func() bool {
		return model.calls.Load() > 0
	})
	stopWaiters(e)
	eventually(t, "the refused resume's handlers to return", func() bool {
		return e.backends.Queue.InFlightCount() == handlers
	})
	eventually(t, "the run to be given back to running", func() bool {
		return r.run(t, e, "t1").Status == sandbox.StatusRunning
	})
	lr := &limitedResume{r: r, e: e, model: model, admitted: &admitted, settled: admitted.Load()}
	// NEVER HANDED BACK: a NAKed completion comes back on the broker's
	// backoff, a second doubling, with nothing announcing it.
	time.Sleep(2500 * time.Millisecond)
	if got := admitted.Load(); got != lr.settled {
		t.Fatalf("admissions rose from %d to %d with the waiter stopped: the refused "+
			"completion was handed back to the broker", lr.settled, got)
	}
	if deferred(t, e) {
		t.Fatal("a completion refused for a limit quiesced the seat's control topic")
	}
	return lr
}

// resumedOnce lifts the limit with lift, which must signal the wait itself,
// and holds the kept completion to one resume.
func (lr *limitedResume) resumedOnce(t *testing.T, lift func()) {
	t.Helper()
	lr.model.limited.Store(false)
	lift()
	eventually(t, "the kept completion to resume once the limit lifted", func() bool {
		return lr.admitted.Load() > lr.settled
	})
	eventually(t, "the run to leave running", func() bool {
		return lr.r.run(t, lr.e, "t1").Status != sandbox.StatusRunning
	})
	time.Sleep(time.Second)
	if got := lr.admitted.Load() - lr.settled; got != 1 {
		t.Fatalf("the kept completion resumed %d times after the limit lifted, want once", got)
	}
}

// A RESUMED TURN THAT MEETS THE LIMIT MID-TURN IS A WAIT, END TO END: the real
// resumer runs the real turn, the model refuses a round in, the claim is given
// back and the completion acknowledged and kept, and it is never handed back to
// the broker. When the seat's provider park probes and the model answers, the
// release signals the wait and the completion resumes, once.
//
// Mutation: drop the refusal mapping from resumeTurn and the completion is
// NAKed into the limit; drop the signal from the park's release and nothing
// brings the kept completion back.
func TestAResumeThatMeetsALimitMidTurnWaitsForTheParksRelease(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t)
	alarms := &fakeAlarms{}
	lr.e.providerParks.after = alarms.after
	refusal := lr.model.limitedErr()
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	if _, parked, err := lr.e.providerPark(t.Context(), "swe", limit); err != nil || !parked {
		t.Fatalf("providerPark = %v, %v", parked, err)
	}
	lr.resumedOnce(t, func() {
		live := alarms.live()
		if len(live) != 1 {
			t.Fatalf("%d probe alarms armed, want the swe park's", len(live))
		}
		live[0].fire()
	})
}

// A PUBLISHED COMPANY SIGNALS THE RESUMES WAITING ON A LIMIT: it builds every
// entry afresh, so a completion kept for its models' limit is re-checked under
// it at once rather than at the waiter's next poll.
//
// Mutation: drop the provider readmit from the converge, and nothing brings the
// kept completion back.
func TestAPublishedCompanyResumesACompletionKeptForALimit(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t)
	lr.resumedOnce(t, func() {
		next := *lr.e.Company()
		lr.e.epoch.current.Store(&next)
		lr.e.followCompany(t.Context(), &next)
	})
}
