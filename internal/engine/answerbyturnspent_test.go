package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// AN ANSWER BY TURN IS SPENT ONCE, AND SAID ONCE, WHATEVER ITS RUN DID NEXT.
//
// The node that took the answer can stop before it acknowledges the delivery —
// mid-turn, its turn's reply perhaps already sent — and the original comes
// round to the seat's next holder. By then the run was revived and resumed by
// that holder, reaped with the answer a turn took, or resumed and relaunched.
// The coordinator spends the answer's delivery at each of those moments
// ([sandbox.CoordinatorOptions.Spent]); the dispatcher recorded nothing for it,
// because an answer by turn is not a type its ledger keeps for turns, and the
// answer-by-turn route never read the ledger. So the redelivery reached the
// coordinator, found nothing on the run that recognised it, and was announced
// `gone` or `not_awaiting` after it had resumed the run. Every case here
// delivers the answer on the seat's control topic, where it is settled, with
// the ledger read wired as the engine wires it
// ([sandbox.CoordinatorOptions.WasSpent]).

// answerBoard records every executor_answered a coordinator announces, and
// every copy of an answer by turn it hands back to the seat's control topic.
type answerBoard struct {
	mu       sync.Mutex
	answered []types.ExecutorAnswered
	inbox    []*events.Event
}

func (b *answerBoard) Publish(_ context.Context, topic string, ev *events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := ev.Data.(*types.ExecutorAnswerGiven); ok && topic == topics.AgentControl(fixtureSeat("swe")) {
		b.inbox = append(b.inbox, ev)
		return nil
	}
	if payload, ok := ev.Data.(*types.ExecutorAnswered); ok && topic == topics.Event(payload.EventType()) {
		b.answered = append(b.answered, *payload)
	}
	return nil
}

func (b *answerBoard) handedBack() []*events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*events.Event(nil), b.inbox...)
}

func (b *answerBoard) outcomes() []types.AnswerOutcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]types.AnswerOutcome, 0, len(b.answered))
	for _, a := range b.answered {
		out = append(out, a.Outcome)
	}
	return out
}

// spentWorld is one seat's executor over one store and the fleet's one
// completion ledger, held by whichever node a case gives it to.
type spentWorld struct {
	store   *sandbox.CoordStore
	ledger  *ledgerstore.MemoryCompletions
	board   *answerBoard
	manager *sandbox.Manager
	runner  *sandbox.FakeRunner
}

func newSpentWorld(t *testing.T) *spentWorld {
	t.Helper()
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &spentWorld{
		store: sandbox.NewCoordStore(coordmemory.NewFleet()), ledger: ledgerstore.NewMemoryCompletions(),
		board: &answerBoard{}, manager: manager, runner: runner,
	}
}

// spentHolder is a node holding the seat: its coordinator, and the dispatcher
// whose completion ledger it spends answers in and reads them back from, wired
// as the engine wires them.
type spentHolder struct {
	coordinator *sandbox.Coordinator
	dispatcher  *engine.Dispatcher
	retries     *handCranked
	ordinary    *turns
}

func (w *spentWorld) holder(t *testing.T, lease sandbox.Fence, resume sandbox.Resumer) *spentHolder {
	t.Helper()
	return w.leasedBy(t, func(string) (sandbox.Fence, bool) { return lease, true }, resume)
}

// leasedBy is [spentWorld.holder] for a node whose hold on the seat is lease's
// answer: a node that has let it go answers not held.
func (w *spentWorld) leasedBy(t *testing.T, lease func(string) (sandbox.Fence, bool),
	resume sandbox.Resumer,
) *spentHolder {
	t.Helper()
	h := &spentHolder{retries: &handCranked{}, ordinary: &turns{}}
	h.dispatcher = &engine.Dispatcher{
		Ledgered: inbox.Ledgered, Turn: h.ordinary.run, Completions: w.ledger,
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		NoteDeferred: func(string) {},
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers:  fleetAnswers(),
		Audience: noAudience{}, Queue: w.board, Pending: w.store, Manager: w.manager,
		Resume: resume, After: h.retries.after, Spent: h.dispatcher.SpendAnswer,
		WasSpent: h.dispatcher.AnswerWorked,
		Lease:    lease,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	h.coordinator = coordinator
	h.dispatcher.Answer = coordinator.TryResumeFromAnswer
	return h
}

// deliver hands ev to the holder as the seat's control topic does, and fails
// the case unless it was settled rather than handed back.
func (h *spentHolder) deliver(t *testing.T, ev *events.Event) {
	t.Helper()
	if err := h.coordinator.OnEvent(t.Context(), ev); err != nil {
		t.Fatalf("the answer on the control topic was handed back: %v", err)
	}
}

// parked launches t1 on holder's node and parks it on a question.
func (w *spentWorld) parked(t *testing.T, h *spentHolder) sandbox.PendingRun {
	t.Helper()
	ctx := t.Context()
	if _, err := h.coordinator.Launch(ctx, w.manager, sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: "t1", AgentID: fixtureSeat("swe").String(), AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := w.store.MarkSuspended(ctx, "t1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	sandboxtest.Ask(t, w.store, "t1", sandbox.Ask{Question: "which branch?", To: sandbox.AskToRequester})
	w.runner.Finish(sandbox.Result{})
	complete(t, h.coordinator, w.store, "t1")
	run := mustRun(t, w.store, "t1")
	if run.Status != sandbox.StatusAwaiting {
		t.Fatalf("the run is %q, want it parked on its question", run.Status)
	}
	return run
}

// claimedBy is the row a node leaves when it recorded the answer by turn given
// in, claimed the run for its resume under lease — and stopped, the delivery
// unacknowledged; took says its turn got as far as taking the answer.
func (w *spentWorld) claimedBy(t *testing.T, run sandbox.PendingRun, given *events.Event,
	lease sandbox.Fence, took bool,
) {
	t.Helper()
	ctx := t.Context()
	payload, _ := events.DataAs[*types.ExecutorAnswerGiven](given)
	raw, err := json.Marshal(given)
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	answer, err := fleetAnswers().Seal(run, sandbox.AnswerWords{
		Text: payload.Answer, Via: types.AnswerViaOperator, By: payload.AnsweredBy,
		ByKind: payload.AnsweredByKind, OperatorID: payload.OperatorID,
	}, sandbox.RecordedAnswer{
		EventIDs: []string{given.ID.String()},
		Events:   []json.RawMessage{raw}, PostedAt: given.Timestamp, RecordedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, ok, err := w.store.RecordAnswer(ctx, "t1", run.LaunchID, answer, lease); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := w.store.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(run.LaunchID), lease); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if took {
		if ok, err := w.store.TakeAnswer(ctx, "t1", run.LaunchID, lease); err != nil || !ok {
			t.Fatalf("TakeAnswer = %v, %v", ok, err)
		}
	}
}

// redeliveredSilently hands the original delivery to holder's control topic
// again and asserts it was acknowledged without reaching the run, without
// running a turn, and without any announcement but want.
func (w *spentWorld) redeliveredSilently(t *testing.T, h *spentHolder, original *events.Event,
	want ...types.AnswerOutcome,
) {
	t.Helper()
	before := len(w.board.outcomes())
	h.deliver(t, original)
	if outcomes := w.board.outcomes(); len(outcomes) != before {
		t.Fatalf("the redelivered answer was announced %v after %v", outcomes[before:], outcomes[:before])
	}
	if got := w.board.outcomes(); len(got) != len(want) || (len(want) > 0 && got[0] != want[0]) {
		t.Fatalf("the answer was announced %v, want %v", got, want)
	}
	if ran := h.ordinary.ran(); len(ran) != 0 {
		t.Fatalf("an answer by turn was run as a turn: %q", ran)
	}
}

// takingResumer resumes a run as a turn does: it takes the answer, and runs.
type takingResumer struct {
	mu    sync.Mutex
	calls int
}

func (r *takingResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	if req.Begin != nil {
		if err := req.Begin(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *takingResumer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// REVIVED AND RESUMED BY THE NEXT HOLDER. The node that recorded the answer
// claimed the run and stopped before its turn took it; the next holder revives
// the claim and resumes the run, spending the answer at its take. The original
// comes round to that holder afterwards, and is acknowledged — the run it
// answered was resumed with it, and is over.
func TestAnAnswerByTurnRevivedAndResumedIsNotAnnouncedAgain(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	old := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, &takingResumer{})
	run := w.parked(t, old)
	original := answerAgainst(run, "use main")
	w.claimedBy(t, run, original, sandbox.Fence{Owner: "node-x", Epoch: 1}, false)
	old.coordinator.Stop()

	resumer := &takingResumer{}
	next := w.holder(t, sandbox.Fence{Owner: "node-y", Epoch: 2}, resumer)
	if err := next.coordinator.RecoverSeat(t.Context(), "swe", "node-y", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	next.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the next holder resumed the revived run %d times, want once", resumer.count())
	}
	w.redeliveredSilently(t, next, original, types.AnswerResumed)
}

// TAKEN BY A TURN THAT DIED MID-TURN. The answer was used — the turn may already
// have answered the person — and the next holder reaps the run as spent,
// recording the answer's delivery for a node that stopped between its take and
// its spend. The original coming round is acknowledged, and the person is told
// nothing about an answer whose turn ran.
func TestAnAnswerByTurnATurnTookIsNotAnnouncedGone(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	old := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, &takingResumer{})
	run := w.parked(t, old)
	original := answerAgainst(run, "use main")
	w.claimedBy(t, run, original, sandbox.Fence{Owner: "node-x", Epoch: 1}, true)
	old.coordinator.Stop()

	next := w.holder(t, sandbox.Fence{Owner: "node-y", Epoch: 2}, &takingResumer{})
	if err := next.coordinator.RecoverSeat(t.Context(), "swe", "node-y", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if _, found, err := w.store.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("Get = %v, %v, want the run reaped", found, err)
	}
	w.redeliveredSilently(t, next, original)
}

// RESUMED, AND ITS TURN RELAUNCHED. The answer resumed the run, and the
// resumed turn started another job, so the run is running a launch the answer
// was never given against. The original coming round — the node stopped before
// it acknowledged it — is acknowledged, rather than announced not_awaiting
// about a run it resumed.
func TestAnAnswerByTurnWhoseTurnRelaunchedIsNotAnnouncedNotAwaiting(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	resumer := &relaunchingResumer{manager: w.manager, store: w.store}
	h := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, resumer)
	resumer.coordinator = h.coordinator
	run := w.parked(t, h)
	original := answerAgainst(run, "use main")
	h.deliver(t, original)
	h.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the run was resumed %d times, want once", resumer.count())
	}
	if now := mustRun(t, w.store, "t1"); now.LaunchID == run.LaunchID || now.Status != sandbox.StatusRunning {
		t.Fatalf("run %q on launch %s, want the resumed turn's new job running", now.Status, now.LaunchID)
	}
	w.redeliveredSilently(t, h, original, types.AnswerResumed)
}

// failingResumer is a node that cannot resume the run: every resume is handed
// back, so the answer's attempts are spent and it is let go of.
type failingResumer struct{}

func (failingResumer) Resume(context.Context, sandbox.ResumeRequest) error {
	return fmt.Errorf("%w: this node cannot read the suspended conversation", sandbox.ErrResumeUnavailable)
}

// A LET-GO'S COPY IS ANNOUNCED ONCE. Every attempt to resume the run with the
// answer failed, so it was let go of and its copy handed back to the seat's
// control topic, where it is spent and announced `declined`, and recorded, so the copy
// coming round again, its acknowledgement lost, says nothing a second time. The
// original, spent before the let-go, is acknowledged without a word too.
func TestALetGosCopyOfAnAnswerByTurnIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	h := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, failingResumer{})
	run := w.parked(t, h)
	original := answerAgainst(run, "use main")
	h.deliver(t, original)
	for range sandbox.MaxAnswerAttempts {
		h.retries.fire()
	}
	copies := w.board.handedBack()
	if len(copies) != 1 {
		t.Fatalf("handed back %d copies, want the let-go's one", len(copies))
	}
	for range 2 {
		h.deliver(t, copies[0])
	}
	if got := w.board.outcomes(); len(got) != 1 || got[0] != types.AnswerDeclined {
		t.Fatalf("announced %v, want the let-go announced once, as declined", got)
	}
	w.redeliveredSilently(t, h, original, types.AnswerDeclined)
}

// AN ANSWER REFUSED BY A NODE THAT LET THE SEAT GO SURVIVES TO BE RECORDED BY
// THE SEAT'S HOLDER. The stale node still consumes the seat's control topic
// for a moment after its lease is gone, and it refuses the answer for want of
// the seat. Naked, the answer came back to it on every backoff, one delivery
// each, until the broker dead-lettered it and the person's answer was gone.
// The refusal names the seat as lost, so the delivery is handed on, once,
// with nothing written on the run; the holder records it and resumes the run.
func TestAnAnswerRefusedForTheSeatSurvivesToItsHolder(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	first := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, &takingResumer{})
	run := w.parked(t, first)
	first.coordinator.Stop()

	given := answerAgainst(run, "use main")
	stale := w.leasedBy(t, func(string) (sandbox.Fence, bool) { return sandbox.Fence{}, false }, &takingResumer{})
	err := stale.coordinator.OnEvent(t.Context(), given)
	if !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Fatalf("the node that let the seat go answered %v, want it refused for the seat", err)
	}
	if got := engine.ControlOutcome(err).Outcome; got != queue.OutcomeDefer {
		t.Fatalf("the refused answer leaves the control topic as %s, want it handed on (defer)", got)
	}
	if now := mustRun(t, w.store, "t1"); now.Answer != nil || now.Status != sandbox.StatusAwaiting {
		t.Fatalf("the refusing node wrote %q with answer %+v, want the run left waiting", now.Status, now.Answer)
	}

	resumer := &takingResumer{}
	holder := w.holder(t, sandbox.Fence{Owner: "node-y", Epoch: 2}, resumer)
	if err := holder.coordinator.RecoverSeat(t.Context(), "swe", "node-y", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	holder.deliver(t, given)
	holder.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the holder resumed the run %d times with the handed-on answer, want once", resumer.count())
	}
}

// A REFUSAL ON THE STORE'S WORD ALONE IS NOT A LOST SEAT. The node still holds
// the seat at its lease, and the run's row carries a newer one: the two
// disagree, and the heartbeat settles which is right within a beat. Named a
// lost seat, the delivery would be deferred and this node's control attachment
// quiesced, and a seat it does hold would hear no answer or completion again,
// since nothing resumes the attachment. So it is handed back on the backoff,
// and the holder of the newer lease records it.
//
// Mutation: name every ErrSeatNotHeld a lost seat without asking the node's
// own lease, and the refusal is deferred.
func TestARefusalOnTheStoresWordAloneIsNotALostSeat(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	older := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, &takingResumer{})
	run := w.parked(t, older)
	resumer := &takingResumer{}
	newer := w.holder(t, sandbox.Fence{Owner: "node-y", Epoch: 2}, resumer)
	if err := newer.coordinator.RecoverSeat(t.Context(), "swe", "node-y", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}

	given := answerAgainst(run, "use main")
	err := older.coordinator.OnEvent(t.Context(), given)
	if !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Fatalf("the node whose lease the row outranks answered %v, want it refused for the seat", err)
	}
	var refused *sandbox.TurnRefused
	if errors.As(err, &refused) && refused.Condition == sandbox.SeatLost {
		t.Fatalf("a refusal on the store's word was named a lost seat while the node's lease stands: %v", err)
	}
	if got := engine.ControlOutcome(err).Outcome; got != queue.OutcomeNak {
		t.Fatalf("the refusal leaves the control topic as %s, want nak: a deferral quiesces "+
			"the attachment of a seat this node holds", got)
	}

	newer.deliver(t, given)
	newer.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the newer lease's holder resumed the run %d times, want once", resumer.count())
	}
}

// A COMPLETION REFUSED BY A NODE THAT LET THE SEAT GO SURVIVES TO THE HOLDER,
// as an answer does: the refusal names the seat as lost, so the delivery is
// handed on, once, with the run left running and unclaimed, and the holder
// settles it.
//
// Mutation: route the completion past the lost-seat naming in OnEvent, and it
// is handed back on the backoff to the node that keeps refusing it.
func TestACompletionRefusedForTheSeatSurvivesToItsHolder(t *testing.T) {
	t.Parallel()
	w := newSpentWorld(t)
	first := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, &takingResumer{})
	if _, err := first.coordinator.Launch(t.Context(), w.manager, sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: "t1", AgentID: fixtureSeat("swe").String(), AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := w.store.MarkSuspended(t.Context(), "t1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	first.coordinator.Stop()
	w.runner.Finish(sandbox.Result{Success: true, Text: "done"})
	run := mustRun(t, w.store, "t1")
	done := types.ExecutorFinished{Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
	completion := events.New(done, events.TraceContext{})

	stale := w.leasedBy(t, func(string) (sandbox.Fence, bool) { return sandbox.Fence{}, false }, &takingResumer{})
	err := stale.coordinator.OnEvent(t.Context(), completion)
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != sandbox.SeatLost {
		t.Fatalf("the node that let the seat go answered %v, want the completion refused as a lost seat", err)
	}
	if got := engine.ControlOutcome(err).Outcome; got != queue.OutcomeDefer {
		t.Fatalf("the refused completion leaves the control topic as %s, want it handed on (defer)", got)
	}
	if now := mustRun(t, w.store, "t1"); now.Status != sandbox.StatusRunning {
		t.Fatalf("the refusing node left the run %q, want it running and unclaimed", now.Status)
	}

	resumer := &takingResumer{}
	holder := w.holder(t, sandbox.Fence{Owner: "node-y", Epoch: 2}, resumer)
	if err := holder.coordinator.RecoverSeat(t.Context(), "swe", "node-y", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	holder.deliver(t, completion)
	holder.retries.fire()
	if resumer.count() != 1 {
		t.Fatalf("the holder resumed the completed run %d times, want once", resumer.count())
	}
}
