package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
)

// sandboxWithModelDoc is [sandboxCompanyDoc] with a model, so a seat on it has
// a turn engine and only the condition a case sets refuses its work. The key
// is a `${K}` the case sets, as a company's credentials always are.
const sandboxWithModelDoc = `
name: Nimbus
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
  sandbox:
    fake: true
roles:
  - name: SWE
    handle: swe
    llm: zulu
    sandbox:
      enabled: true
`

// pauseHolds is what a queue reports holding a seat's inbox under.
type pauseHolds interface {
	PauseHolds(topic, group string) []string
}

// inboxHolds is every reason the engine's queue holds a seat's inbox under.
func inboxHolds(t *testing.T, e *Engine, handle string) []string {
	t.Helper()
	q, ok := e.backends.Queue.(pauseHolds)
	if !ok {
		t.Fatalf("the engine's queue (%T) does not report its holds", e.backends.Queue)
	}
	return q.PauseHolds(topics.AgentInbox(handle), topics.AgentInboxGroup(handle))
}

// A SEAT ITS RUNNING JOB HOLDS HAS ITS INBOX HELD, by the runtime the engine
// builds — the wiring between the coordinator that decides the hold and the
// queue it is taken on, which a coordinator test with a spy for a hold cannot
// see. Recovered with the seat, and taken before its mailbox opens, so the
// first message waiting on it is not fetched beside the job.
func TestTheEngineHoldsTheInboxOfASeatItsJobHolds(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	seedRunningRun(t, e, "wk-held", "box-1")
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	eventually(t, "the seat's running job to hold its inbox", func() bool {
		return slices.Contains(inboxHolds(t, e, "swe"), string(inbox.HoldSandbox))
	})
	if !e.SeatHeldBySandbox("swe") {
		t.Fatal("the inbox is held for a seat the screening does not call held")
	}
}

// A DELIVERY THAT REACHES A HELD SEAT RETRIES THE HOLD the queue refused, through
// the dispatcher the engine builds: the coordinator counts the seat held by its
// running job, but its hold did not land, so the seat's mail reaches the
// screening — which defers it and asks the coordinator, current when the
// delivery arrives, for the hold again.
func TestADeliveryThatReachesAHeldSeatTakesTheHoldTheQueueRefused(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	rt := e.sandbox.Load()
	hold := &refusedOnce{inner: seatHold{engine: e}}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: e.backends.Queue, Pending: rt.pending,
		Manager: rt.coordinator.Manager(), Resume: &resumeSpy{}, Hold: hold,
		Lease: e.launchFence,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.sandbox.Store(&sandboxRuntime{pending: rt.pending, coordinator: coordinator})

	seedRunningRun(t, e, "t-raced", "box-raced")
	recoverHeld(t, e, coordinator, "swe")
	if !e.SeatHeldBySandbox("swe") {
		t.Fatal("the running job does not hold its seat")
	}
	if slices.Contains(inboxHolds(t, e, "swe"), string(inbox.HoldSandbox)) {
		t.Fatal("the premise: the queue refused the hold")
	}

	got := e.Dispatch(t.Context(), "swe", []*events.Event{{ID: uuid.New(), Type: "task_assigned"}})
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want the delivery deferred at the head", got.Outcome)
	}
	if !slices.Contains(inboxHolds(t, e, "swe"), string(inbox.HoldSandbox)) {
		t.Fatal("the delivery that reached the held seat did not take the hold the queue refused, " +
			"so every later message reaches the screening too")
	}
}

// refusedOnce is a [sandbox.SeatHold] the queue refuses the first time.
type refusedOnce struct {
	mu      sync.Mutex
	inner   sandbox.SeatHold
	refused bool
}

func (h *refusedOnce) Hold(ctx context.Context, handle string) error {
	h.mu.Lock()
	first := !h.refused
	h.refused = true
	h.mu.Unlock()
	if first {
		return fmt.Errorf("the queue refused the hold")
	}
	return h.inner.Hold(ctx, handle)
}

func (h *refusedOnce) Release(ctx context.Context, handle string) error {
	return h.inner.Release(ctx, handle)
}

// EACH REFUSAL NAMES WHAT IT WAITS ON, which is what lets a retried resume wait
// for that rather than re-check on a timer. Two are clocks: a lease is re-proved
// by the renew the host makes once a heartbeat, so ownership is re-checked one
// heartbeat on; the rest are events the engine passes on.
func TestMayResumeAnswerNamesWhatEachRefusalWaitsOn(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	before := time.Now()
	refusal, refused := e.mayResumeAnswer(t.Context(), "swe")
	if !refused || refusal.Condition != waitOwnership {
		t.Fatalf("an unheld seat = %+v (%v), want refused on ownership", refusal, refused)
	}
	if beat := e.node.Host().HeartbeatInterval(); refusal.Until.Before(before.Add(beat)) ||
		refusal.Until.After(time.Now().Add(beat)) {
		t.Errorf("ownership waits until %v, want one heartbeat (%v) on", refusal.Until, beat)
	}

	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	if refusal, refused := e.mayResumeAnswer(t.Context(), "swe"); refused {
		t.Fatalf("a held, unpaused seat with a model was refused: %+v", refusal)
	}

	e.pauses.mu.Lock()
	e.pauses.pauses["swe"] = coord.SeatPause{Handle: "swe", By: "ana"}
	e.pauses.mu.Unlock()
	if refusal, refused := e.mayResumeAnswer(t.Context(), "swe"); !refused ||
		refusal.Condition != waitPause || !refusal.Until.IsZero() {
		t.Errorf("a paused seat = %+v (%v), want refused on the pause, which no clock lifts",
			refusal, refused)
	}
	e.pauses.mu.Lock()
	delete(e.pauses.pauses, "swe")
	e.pauses.mu.Unlock()

	e.notify.mu.Lock()
	e.notify.admits = func() bool { return false }
	e.notify.mu.Unlock()
	if refusal, refused := e.mayResumeAnswer(t.Context(), "swe"); !refused ||
		refusal.Condition != waitPosture {
		t.Errorf("a node refusing new work = %+v (%v), want refused on the posture", refusal, refused)
	}
}

// A BUDGET REFUSAL NAMES WHEN ITS WINDOW TURNS OVER, which is the instant a
// retried resume waits for: Berlin's midnight, an hour after the day was spent.
func TestABudgetRefusalNamesTheEndOfItsWindow(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)
	reason, resets, refusing := r.e.budgetRefusing(t.Context(), "lead")
	if !refusing || reason == "" {
		t.Fatalf("budgetRefusing = %q, %v, want the spent day refusing", reason, refusing)
	}
	if !resets.Equal(berlinMidnight) {
		t.Errorf("the refusal lifts at %v, want the day's end %v", resets, berlinMidnight)
	}
}

// resumeSpy is a [sandbox.Resumer] that records what it resumed, failing the
// first `failing` of them the way a node that cannot resume the run does.
type resumeSpy struct {
	mu      sync.Mutex
	turns   []string
	failing int
}

func (r *resumeSpy) Resume(_ context.Context, req sandbox.ResumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, req.Run.TurnID)
	if r.failing > 0 {
		r.failing--
		return fmt.Errorf("%w: the runner is still being built", sandbox.ErrResumeUnavailable)
	}
	return nil
}

func (r *resumeSpy) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

// AN ANSWER A PERSON'S PAUSE HELD BACK RESUMES THE MOMENT THEY RESUME THE SEAT —
// not at the next re-check, which is minutes away, and not before.
//
// The engine's own admission refuses the retry while the seat is paused; the
// pause watch hearing the resume is the signal, and the engine passes it on.
func TestAPausedAnswerResumesWhenThePersonResumesTheSeat(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	coordinator, resumer := spyRuntime(t, e)

	// A RUN WHOSE ANSWER IS RECORDED AND OWED, on a seat a person paused.
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Handle: "swe",
		Pause: &coord.SeatPause{Handle: "swe", By: "ana"}})
	answeredRun(t, e.sandbox.Load().pending, "t-paused")
	recoverHeld(t, e, coordinator, "swe")
	time.Sleep(200 * time.Millisecond)
	if n := resumer.count(); n != 0 {
		t.Fatalf("%d resumes ran on a paused seat", n)
	}

	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Handle: "swe"})
	eventually(t, "the held-back answer to resume once the seat is resumed", func() bool {
		return resumer.count() == 1
	})
}

// recoverHeld recovers a seat's runs on coordinator under the lease the
// engine's seat host holds the seat by, as the seat's acquisition does — the
// lease the coordinator then writes on the seat's behalf under.
func recoverHeld(t *testing.T, e *Engine, coordinator *sandbox.Coordinator, handle string) {
	t.Helper()
	fence, held := e.launchFence(handle)
	if !held {
		t.Fatalf("the premise: this node holds %s", handle)
	}
	if err := coordinator.RecoverSeat(t.Context(), handle, fence.Owner, fence.Epoch); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
}

// spyRuntime swaps the engine's sandbox runtime for one whose coordinator
// resumes into a recorder, keeping everything else — the store, the manager,
// the seat hold and the engine's own admission — so what a case observes is
// the engine's half: the refusal it names and the signal it sends.
func spyRuntime(t *testing.T, e *Engine) (*sandbox.Coordinator, *resumeSpy) {
	t.Helper()
	rt := e.sandbox.Load()
	if rt == nil {
		t.Fatal("the node runs no sandbox")
	}
	resumer := &resumeSpy{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: e.backends.Queue, Pending: rt.pending,
		Manager: rt.coordinator.Manager(), Resume: resumer,
		Hold: seatHold{engine: e}, Admit: e.mayResumeAnswer, Lease: e.launchFence,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.sandbox.Store(&sandboxRuntime{pending: rt.pending, coordinator: coordinator})
	return coordinator, resumer
}

// AN ANSWER WAITING ON A MODEL RESUMES WITH THE APPLY THAT BRINGS ONE: the
// apply is the event, and the engine passes it on.
func TestAnAnswerWaitingOnAModelResumesWithTheApplyThatBringsOne(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	coordinator, resumer := spyRuntime(t, e)
	answeredRun(t, e.sandbox.Load().pending, "t-model")
	recoverHeld(t, e, coordinator, "swe")
	time.Sleep(200 * time.Millisecond)
	if n := resumer.count(); n != 0 {
		t.Fatalf("%d resumes ran in a company with no model", n)
	}

	applyOK(t, e, sandboxWithModelDoc)
	eventually(t, "the answer to resume with the apply that brought a model", func() bool {
		return resumer.count() == 1
	})
}

// AN ANSWER THE POSTURE HELD BACK RESUMES ON THE RECONCILE TICK THAT FINDS THE
// NODE ADMITTING WORK AGAIN — the only place the posture moves.
func TestAnAnswerThePostureHeldBackResumesOnTheTickThatAdmitsWork(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	var admitting atomic.Bool
	e.notify.mu.Lock()
	e.notify.admits = admitting.Load
	e.notify.mu.Unlock()
	coordinator, resumer := spyRuntime(t, e)
	answeredRun(t, e.sandbox.Load().pending, "t-posture")
	recoverHeld(t, e, coordinator, "swe")
	time.Sleep(200 * time.Millisecond)
	if n := resumer.count(); n != 0 {
		t.Fatalf("%d resumes ran on a node refusing new work", n)
	}

	r, err := e.NewReconciler(ReconcilerOptions{Store: e.backends.Store, Fleet: e.backends.Fleet,
		Queue: e.backends.Queue, NodeID: "node-posture"})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	admitting.Store(true)
	r.nudged <- struct{}{}
	eventually(t, "the answer to resume on the tick that admits work", func() bool {
		return resumer.count() == 1
	})
}

// AN ANSWER A SEAT INHERITS RESUMES AS THE SEAT IS ESTABLISHED, not a heartbeat
// later.
//
// The seat's preparation recovers a run whose answer a previous holder
// recorded and never resumed with, and retries it — from inside the
// acquisition, while the seat is still establishing and admits no turn. The
// refusal names ownership, and nothing used to say when that lifted: an
// establishment is not a renew's admission edge, so the retry slept until the
// clock said a heartbeat had passed (15 s at the default TTL). The seat host
// now reports the establishment, and the engine passes it on.
//
// DETERMINISTIC rather than raced: the coordinator's retries are hand-cranked,
// and the one owed to the first inherited answer is fired from INSIDE the
// acquisition — from the store call the recovery makes for the second — so it
// is refused for establishing every time.
func TestAnInheritedAnswerResumesAsItsSeatIsEstablished(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	host := e.node.Host()
	heartbeat := host.HeartbeatInterval()

	rt := e.sandbox.Load()
	retries := &capturedRetries{}
	store := &claimWitness{PendingStore: rt.pending, during: func() { retries.fire() }}
	resumer := &resumeSpy{}
	var refusedForOwnership atomic.Int32
	admit := func(ctx context.Context, handle string) (sandbox.Refusal, bool) {
		refusal, refused := e.mayResumeAnswer(ctx, handle)
		if refused && refusal.Condition == waitOwnership {
			refusedForOwnership.Add(1)
		}
		return refusal, refused
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: e.backends.Queue, Pending: store,
		Manager: rt.coordinator.Manager(), Resume: resumer,
		Hold: seatHold{engine: e}, Admit: admit, After: retries.after, Lease: e.launchFence,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.sandbox.Store(&sandboxRuntime{pending: store, coordinator: coordinator})

	// TWO ANSWERS THE SEAT'S LAST HOLDER RECORDED, and the seat changes hands.
	answeredRun(t, rt.pending, "t-first")
	answeredRun(t, rt.pending, "t-second")
	if !host.Release(t.Context(), "swe", seat.ReasonPlacement) {
		t.Fatal("the seat was not released")
	}
	host.Sweep(t.Context())
	waitHeld(t, e, "swe")
	if refusedForOwnership.Load() == 0 {
		t.Fatal("the premise: no inherited answer was refused while its seat was establishing")
	}

	deadline := time.Now().Add(heartbeat / 3)
	for slices.ContainsFunc(retries.delays(), func(d time.Duration) bool { return d > 0 }) {
		if time.Now().After(deadline) {
			t.Fatalf("the inherited answers wait %v after their seat was established, want them "+
				"re-checked at once rather than a %v heartbeat later", retries.delays(), heartbeat)
		}
		time.Sleep(10 * time.Millisecond)
	}
	retries.fire()
	if n := resumer.count(); n != 2 {
		t.Fatalf("%d resumes, want both inherited answers resumed", n)
	}
}

// claimWitness is a store that runs during() inside every ownership claim — a
// call a seat's recovery makes from inside its acquisition.
type claimWitness struct {
	sandbox.PendingStore
	during func()
}

func (s *claimWitness) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	s.during()
	return s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
}

// capturedRetries is a coordinator scheduler a case fires by hand, keeping the
// delay each call was armed with.
type capturedRetries struct {
	mu  sync.Mutex
	due []*capturedRetry
}

type capturedRetry struct {
	delay time.Duration
	f     func()
	done  bool
}

func (c *capturedRetries) after(d time.Duration, f func()) func() bool {
	call := &capturedRetry{delay: d, f: f}
	c.mu.Lock()
	c.due = append(c.due, call)
	c.mu.Unlock()
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if call.done {
			return false
		}
		call.done = true
		return true
	}
}

// fire runs every call armed and not cancelled.
func (c *capturedRetries) fire() {
	c.mu.Lock()
	var due []*capturedRetry
	for _, call := range c.due {
		if !call.done {
			call.done = true
			due = append(due, call)
		}
	}
	c.due = nil
	c.mu.Unlock()
	for _, call := range due {
		call.f()
	}
}

// delays are the delays of the calls armed and not yet run or cancelled.
func (c *capturedRetries) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, call := range c.due {
		if !call.done {
			out = append(out, call.delay)
		}
	}
	return out
}

// A NODE THAT STOPS ITS SANDBOX STOPS THE ANSWERS IT WAS RETRYING. The resume
// of a recorded answer failed and its retry is a second away; the node is
// shutting down, and the retry must not run against a node on its way out —
// the answer is on the run, and the seat's next holder drives it.
func TestStoppingTheSandboxStopsTheAnswersItWasRetrying(t *testing.T) {
	t.Setenv("K", "sk-ant-test")
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	coordinator, resumer := spyRuntime(t, e)
	resumer.mu.Lock()
	resumer.failing = 1
	resumer.mu.Unlock()
	answeredRun(t, e.sandbox.Load().pending, "t-stop")
	recoverHeld(t, e, coordinator, "swe")
	eventually(t, "the first attempt to fail", func() bool { return resumer.count() == 1 })

	e.stopSandbox()
	time.Sleep(answerRetryFirst + 500*time.Millisecond)
	if n := resumer.count(); n != 1 {
		t.Fatalf("%d resumes, want only the one before the stop: a retry ran on a stopped node", n)
	}
}

// answerRetryFirst is how long the first retry of a failed resume waits —
// the coordinator's own one second.
const answerRetryFirst = time.Second

// answeredRun records a run on the swe seat that asked a question and whose
// answer is recorded and owed its resume.
func answeredRun(t *testing.T, store sandbox.PendingStore, turnID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", Role: "SWE", CodingAgent: "claude-code",
		ConversationKey: "slack:C1:1.0", PartitionKey: "slack:C1:1.0", CreatedAt: time.Now().UTC(),
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if _, err := store.MarkSuspended(ctx, turnID, sandbox.Suspension{
		State: json.RawMessage(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	asked := time.Now().UTC()
	if err := store.MarkAwaiting(ctx, turnID, sandbox.Clarification{
		Question: "which branch?", Audience: "requester", AskedAt: asked}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	run, _, err := store.Get(ctx, turnID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reply := events.New(types.ExternalNotification{NotificationSource: "slack", Body: "use main"},
		events.TraceContext{})
	reply.ID, reply.Timestamp = uuid.New(), asked.Add(time.Second)
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, won, err := store.RecordAnswer(ctx, turnID, run.LaunchID, sandbox.RecordedAnswer{
		Text: "use main", Via: types.AnswerViaChat, EventIDs: []string{reply.ID.String()},
		Events: []json.RawMessage{raw}, PostedAt: reply.Timestamp, RecordedAt: time.Now().UTC(),
	}, sandbox.Fence{}); err != nil || !won {
		t.Fatalf("RecordAnswer = %v, %v", won, err)
	}
}
