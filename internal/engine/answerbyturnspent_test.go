package engine_test

import (
	"context"
	"encoding/json"
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
// `gone` or `not_awaiting` after it had resumed the run. Every case here runs
// the dispatcher with the engine's own ledgered set.

// answerBoard records every sandbox_run_answered a coordinator announces, and
// every copy it hands back to the seat's inbox.
type answerBoard struct {
	mu       sync.Mutex
	answered []types.SandboxRunAnswered
	inbox    []*events.Event
}

func (b *answerBoard) Publish(_ context.Context, topic string, ev *events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if topic == topics.AgentInbox("swe") {
		b.inbox = append(b.inbox, ev)
		return nil
	}
	if payload, ok := ev.Data.(*types.SandboxRunAnswered); ok && topic == topics.Event(payload.EventType()) {
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

// spentWorld is one seat's coding run over one store and the fleet's one
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
// its inbox is routed through, wired as the engine wires them — the
// coordinator's spend is the dispatcher's own record, and the dispatcher reads
// the engine's ledgered set.
type spentHolder struct {
	coordinator *sandbox.Coordinator
	dispatcher  *engine.Dispatcher
	retries     *handCranked
	ordinary    *turns
}

func (w *spentWorld) holder(t *testing.T, lease sandbox.Fence, resume sandbox.Resumer) *spentHolder {
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
		Audience: noAudience{}, Queue: w.board, Pending: w.store, Manager: w.manager,
		Resume: resume, After: h.retries.after, Spent: h.dispatcher.SpendAnswer,
		Lease: func(string) (sandbox.Fence, bool) { return lease, true },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	h.coordinator = coordinator
	h.dispatcher.Answer = coordinator.TryResumeFromAnswer
	h.dispatcher.AnswerByTurn = coordinator.AnswerByTurn
	return h
}

// parked launches t1 on holder's node and parks it on a question.
func (w *spentWorld) parked(t *testing.T, h *spentHolder) sandbox.PendingRun {
	t.Helper()
	ctx := t.Context()
	if _, err := h.coordinator.Launch(ctx, w.manager, sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: "t1", AgentID: "a-1", AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := w.store.MarkSuspended(ctx, "t1", sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	w.runner.Finish(sandbox.Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})
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
	payload, _ := events.DataAs[*types.SandboxAnswerGiven](given)
	raw, err := json.Marshal(given)
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	if _, ok, err := w.store.RecordAnswer(ctx, "t1", run.LaunchID, sandbox.RecordedAnswer{
		Text: payload.Answer, Via: types.AnswerViaOperator, By: payload.AnsweredBy,
		BySeat: payload.AnsweredBySeat, EventIDs: []string{given.ID.String()},
		Events: []json.RawMessage{raw}, PostedAt: given.Timestamp, RecordedAt: time.Now().UTC(),
	}, lease); err != nil || !ok {
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

// redeliveredSilently hands the original delivery to holder's dispatcher again
// and asserts it was acknowledged without reaching the run, without running a
// turn, and without any announcement but want.
func (w *spentWorld) redeliveredSilently(t *testing.T, h *spentHolder, original *events.Event,
	want ...types.AnswerOutcome,
) {
	t.Helper()
	before := len(w.board.outcomes())
	if got := h.dispatcher.Dispatch(t.Context(), "swe", []*events.Event{original}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the redelivered answer = %v, want it acknowledged", got.Outcome)
	}
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

// RESUMED, AND ITS TURN RELAUNCHED. The answer resumed the run inline, and the
// resumed turn started another job, so the run is running a launch the answer
// was never given against. The original coming round — the node stopped before
// it acknowledged it — is acknowledged, rather than announced not_awaiting
// about a run it resumed.
func TestAnAnswerByTurnWhoseTurnRelaunchedIsNotAnnouncedNotAwaiting(t *testing.T) {
	w := newSpentWorld(t)
	resumer := &relaunchingResumer{manager: w.manager, store: w.store}
	h := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, resumer)
	resumer.coordinator = h.coordinator
	run := w.parked(t, h)
	original := answerAgainst(run, "use main")
	if got := h.dispatcher.Dispatch(t.Context(), "swe", []*events.Event{original}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the answer = %v, want it spent on its run", got.Outcome)
	}
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
// inbox, where it is spent and announced `declined` — and recorded, so the copy
// coming round again, its acknowledgement lost, says nothing a second time. The
// original, spent before the let-go, is acknowledged without a word too.
func TestALetGosCopyOfAnAnswerByTurnIsAnnouncedOnce(t *testing.T) {
	w := newSpentWorld(t)
	h := w.holder(t, sandbox.Fence{Owner: "node-x", Epoch: 1}, failingResumer{})
	run := w.parked(t, h)
	original := answerAgainst(run, "use main")
	if got := h.dispatcher.Dispatch(t.Context(), "swe", []*events.Event{original}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the answer = %v, want it recorded on its run", got.Outcome)
	}
	for range sandbox.MaxAnswerAttempts {
		h.retries.fire()
	}
	copies := w.board.handedBack()
	if len(copies) != 1 {
		t.Fatalf("handed back %d copies, want the let-go's one", len(copies))
	}
	for range 2 {
		if got := h.dispatcher.Dispatch(t.Context(), "swe", copies); got.Outcome != queue.OutcomeAck {
			t.Fatalf("the copy = %v, want it spent", got.Outcome)
		}
	}
	if got := w.board.outcomes(); len(got) != 1 || got[0] != types.AnswerDeclined {
		t.Fatalf("announced %v, want the let-go announced once, as declined", got)
	}
	w.redeliveredSilently(t, h, original, types.AnswerDeclined)
}
