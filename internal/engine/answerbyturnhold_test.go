package engine_test

import (
	"context"
	"slices"
	"strings"
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
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// busySeat is one seat wired end to end as a node wires it: its inbox routed
// through the dispatcher, its control topic through the coordinator, and the
// coordinator holding the inbox while one of the seat's runs holds the seat.
type busySeat struct {
	q           queue.EventQueue
	store       *sandbox.CoordStore
	manager     *sandbox.Manager
	runner      *sandbox.FakeRunner
	coordinator *sandbox.Coordinator
	retries     *handCranked
	ordinary    *turns
}

func newBusySeat(t *testing.T, resume func(*busySeat) sandbox.Resumer) *busySeat {
	t.Helper()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })

	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := &busySeat{q: q, store: sandbox.NewCoordStore(coordmemory.NewFleet()), manager: manager,
		runner: runner, retries: &handCranked{}, ordinary: &turns{}}
	ledger := ledgerstore.NewMemoryCompletions()
	d := &engine.Dispatcher{
		Ledgered: func(kind string) bool { return kind == notificationType },
		Turn:     s.ordinary.run, Completions: ledger, NoteDeferred: func(string) {},
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers:  fleetAnswers(),
		Audience: noAudience{}, Queue: q, Pending: s.store, Manager: manager,
		Resume: resume(s), Hold: inboxHold{q: q}, After: s.retries.after,
		Spent: d.SpendAnswer, WasSpent: d.AnswerWorked,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	s.coordinator = coordinator
	d.Answer = coordinator.TryResumeFromAnswer
	d.HoldSandbox = coordinator.HoldSeat
	d.Conditions = func(handle string) inbox.Conditions {
		held, awaits := coordinator.SeatRuns(handle)
		return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
			SeatHeldBySandbox: held, SandboxAwaitsAnswer: awaits}
	}
	if err := q.Subscribe(ctx, fixtureInbox("swe"), fixtureInboxGroup("swe"),
		func(ctx context.Context, ev *events.Event) queue.Result {
			return d.Dispatch(ctx, "swe", []*events.Event{ev})
		}); err != nil {
		t.Fatalf("Subscribe inbox: %v", err)
	}
	seat := fixtureSeat("swe")
	if err := q.Subscribe(ctx, topics.AgentControl(seat), topics.AgentControlGroup(seat),
		func(ctx context.Context, ev *events.Event) queue.Result {
			if err := coordinator.OnEvent(ctx, ev); err != nil {
				return queue.Nak(err)
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe control: %v", err)
	}
	return s
}

// launch starts a job for turnID on the seat, its turn suspended behind it.
func (s *busySeat) launch(t *testing.T, turnID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.coordinator.Launch(ctx, s.manager, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TurnID: turnID, AgentID: fixtureSeat("swe").String(),
			AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch %s: %v", turnID, err)
	}
	if _, err := s.store.MarkSuspended(ctx, turnID,
		sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended %s: %v", turnID, err)
	}
}

// parks finishes turnID's job on a question.
func (s *busySeat) parks(t *testing.T, turnID, question string) sandbox.PendingRun {
	t.Helper()
	sandboxtest.Ask(t, s.store, turnID, sandbox.Ask{Question: question, To: sandbox.AskToRequester})
	s.runner.Finish(sandbox.Result{})
	complete(t, s.coordinator, s.store, turnID)
	run := mustRun(t, s.store, turnID)
	if run.Status != sandbox.StatusAwaiting {
		t.Fatalf("run %s is %q, want it parked on %q", turnID, run.Status, question)
	}
	return run
}

// answer is a person answering run through answer_run's own desk.
func (s *busySeat) answer(t *testing.T, run sandbox.PendingRun, text string) {
	t.Helper()
	given := types.ExecutorAnswerGiven{
		TurnID: run.TurnID, AgentHandle: run.AgentHandle, Answer: text,
		AnsweredBy: "founder", AnsweredByKind: "human", OperatorID: "pat:founder-token",
		LaunchID: run.LaunchID,
	}
	if _, err := (sandbox.AnswerDesk{Pending: s.store, Queue: s.q}).Deliver(t.Context(), given); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// AN ANSWER BY TURN REACHES ITS RUN WHILE ANOTHER OF THE SEAT'S JOBS HOLDS THE
// SEAT, and resumes it as the seat's next turn, ahead of the seat's mail.
//
// What a person saw: they answered a parked run from the Inbox and were told
// `pending`, and nothing happened, twice, until the run's pause TTL took its
// box. The answer had gone to the seat's INBOX, and the seat was busy coding:
//
//  1. run t1 parks on a question;
//  2. the seat takes on another job, t2, which holds the seat, so its inbox is
//     held and fetches nothing, an older notice waiting on it;
//  3. the person answers t1, and the answer waits on the held inbox, behind the
//     notice; when t2 ended, the notice ran first, and its turn could launch
//     the next job and hold the seat again.
//
// The answer is addressed to a run, so it travels on the seat's control topic,
// which no hold stops: it is RECORDED at once (the run reads `answered`, which
// is what the person's Inbox settles on), its resume waits for t2 to stop
// holding the seat, and the seat's held mail waits behind that resume.
//
// Mutation: deliver to the inbox again and the run is never answered; drop the
// coordinator's seat-coding refusal and t1 resumes beside t2's job; drop the
// signal when the seat frees and t1 waits for its backstop.
func TestAnAnswerByTurnReachesItsRunWhileAnotherJobHoldsTheSeat(t *testing.T) {
	t.Parallel()
	order := &sequence{}
	s := newBusySeat(t, func(*busySeat) sandbox.Resumer { return &orderedResumer{order: order} })

	s.launch(t, "t1")
	t1 := s.parks(t, "t1", "which branch?")
	s.launch(t, "t2")
	if !s.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the premise: t2's job holds the seat")
	}
	if err := s.q.Publish(t.Context(), fixtureInbox("swe"), said("designer", "GP-23 moved to done", time.Now().UTC().Add(-time.Hour))); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	s.answer(t, t1, "stack it on gp-4")
	eventually(t, "the answer to be recorded on its run while the seat is busy", func() bool {
		return mustRun(t, s.store, "t1").Status == sandbox.StatusAnswered
	})
	s.retries.fire()
	settle()
	if got := order.list(); len(got) != 0 {
		t.Fatalf("the seat did %v while t2's job held it, want nothing", got)
	}

	// T2'S JOB ENDS, its turn resumes and finishes, and the seat is free.
	s.runner.Finish(sandbox.Result{Success: true})
	complete(t, s.coordinator, s.store, "t2")
	if s.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the premise: t2 settled and let go of the seat")
	}
	if s.retries.fireNow() == 0 {
		t.Fatal("nothing re-checked the owed answer when the seat came free: it waits " +
			"for its backstop")
	}
	eventually(t, "the notice to run once the answer's resume is done", func() bool {
		return len(s.ordinary.ran()) == 1
	})
	order.add("turn:" + s.ordinary.ran()[0])
	if got := order.list(); !slices.Equal(got[:2], []string{"resume:t2", "resume:t1"}) || len(got) != 3 {
		t.Fatalf("the seat did %v, want t2's resume, then t1 resumed with the answer, then its mail", got)
	}
	if answered := order.answerFor("t1"); answered == "" {
		t.Fatal("t1 was resumed without the person's answer")
	}
}

// AN ANSWER GIVEN AGAINST A QUESTION THE RUN HAS MOVED PAST ANSWERS NOTHING.
//
//  1. A run parks on question Q1, and two people answer Q1 by turn.
//  2. The first answer resumes the run, and the resumed turn relaunches its
//     executor: the run is running a new job.
//  3. The second answer arrives meanwhile, on the control topic, at once.
//
// Named only by its run, it claimed whatever the run waited on by then and
// resumed it a second time, with Q1's answer presented as Q2's. It names its
// question, so it is spent as not_awaiting, and the new job parks on Q2 with
// nobody's answer recorded against it.
func TestALateAnswerByTurnDoesNotAnswerTheRunsNextQuestion(t *testing.T) {
	t.Parallel()
	var resumer *relaunchingResumer
	s := newBusySeat(t, func(s *busySeat) sandbox.Resumer {
		resumer = &relaunchingResumer{manager: s.manager, store: s.store}
		return resumer
	})
	resumer.coordinator = s.coordinator

	s.launch(t, "t1")
	q1 := s.parks(t, "t1", "which branch?")
	s.answer(t, q1, "use the release branch")
	eventually(t, "the first answer to be recorded", func() bool {
		return mustRun(t, s.store, "t1").Status == sandbox.StatusAnswered
	})
	s.retries.fire()
	if resumer.count() != 1 || !s.coordinator.SeatHeldBySandbox("swe") {
		t.Fatalf("resumed %d times, want once, with the resumed turn's new job holding the seat",
			resumer.count())
	}

	s.answer(t, q1, "use main")
	settle()
	s.retries.fire()
	q2 := s.parks(t, "t1", "which test suite?")
	s.retries.fire()
	settle()
	if n := resumer.count(); n != 1 {
		t.Fatalf("the run was resumed %d times: the late answer to Q1 resumed it again", n)
	}
	if q2.LaunchID == q1.LaunchID || q2.Answer != nil {
		t.Fatalf("run on launch %s with answer %+v, want it waiting on its second question "+
			"with nothing recorded against it", q2.LaunchID, q2.Answer)
	}
	if ran := s.ordinary.ran(); len(ran) != 0 {
		t.Fatalf("an answer by turn was run as a turn: %q", ran)
	}
}

// sequence is the order a seat did things in, across goroutines.
type sequence struct {
	mu      sync.Mutex
	steps   []string
	answers map[string]string
	// boxes is the box each run was resumed in, empty for one re-seeded.
	boxes map[string]string
}

func (s *sequence) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

func (s *sequence) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.steps)
}

func (s *sequence) answerFor(turnID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answers[turnID]
}

// orderedResumer resumes a run as a turn that takes its answer and finishes,
// and records the resume in the seat's sequence.
type orderedResumer struct{ order *sequence }

func (r *orderedResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	if req.Begin != nil {
		if err := req.Begin(ctx); err != nil {
			return err
		}
	}
	r.order.add("resume:" + req.Run.TurnID)
	r.order.mu.Lock()
	if r.order.answers == nil {
		r.order.answers = map[string]string{}
	}
	// A PERSON'S ANSWER RIDES THE QUESTION IT ANSWERS ([sandbox.ResumeRequest.Asked]),
	// never the report: the resumed turn carries the run on with it.
	if req.Asked != nil {
		r.order.answers[req.Run.TurnID] = req.Asked.Answer
	}
	if r.order.boxes == nil {
		r.order.boxes = map[string]string{}
	}
	r.order.boxes[req.Run.TurnID] = req.Run.SandboxID
	r.order.mu.Unlock()
	return nil
}

// relaunchingResumer re-enters a parked run as a turn that relaunches its
// executor would: it takes the answer, and launches the next job.
type relaunchingResumer struct {
	coordinator *sandbox.Coordinator
	manager     *sandbox.Manager
	store       *sandbox.CoordStore

	mu    sync.Mutex
	calls int
}

func (r *relaunchingResumer) launch(turnID string) sandbox.LaunchRequest {
	return sandbox.LaunchRequest{
		Turn:  sandbox.TurnRef{TurnID: turnID, AgentID: fixtureSeat("swe").String(), AgentHandle: "swe", Role: "SWE"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}
}

func (r *relaunchingResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	if req.Begin != nil {
		if err := req.Begin(ctx); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if _, err := r.coordinator.Launch(ctx, r.manager, r.launch(req.Run.TurnID)); err != nil {
		return err
	}
	_, err := r.store.MarkSuspended(ctx, req.Run.TurnID, sandbox.Suspension{State: []byte(`{"messages":[]}`)})
	return err
}

func (r *relaunchingResumer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// answerAgainst is an operator's answer to the question run is waiting on, as
// answer_run puts it on the seat's control topic.
func answerAgainst(run sandbox.PendingRun, answer string) *events.Event {
	ev := events.New(types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: run.AgentID, AgentHandle: run.AgentHandle,
		Answer:     answer,
		AnsweredBy: "founder", AnsweredByKind: "human", OperatorID: "pat:founder-token",
		LaunchID: run.LaunchID,
	}, events.TraceContext{})
	ev.Source = types.OperatorSource
	return ev
}

// complete hands the coordinator the completion of the job the run holds now.
func complete(t *testing.T, c *sandbox.Coordinator, store *sandbox.CoordStore, turnID string) {
	t.Helper()
	run := mustRun(t, store, turnID)
	done := types.ExecutorFinished{Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: turnID, LaunchID: run.LaunchID, SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
	if err := c.OnCompleted(t.Context(), done, events.New(done, events.TraceContext{})); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
}

// mustRun is one run's row.
func mustRun(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.PendingRun {
	t.Helper()
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	return run
}

// AN ANSWER RECORDED WHILE ITS SEAT IS BUSY OUTLIVES ITS RUN'S PAUSE TTL. The
// box is reclaimed when the pause runs out, as a parked run's is, but the run
// stays answered with the answer on it: once the seat is free it resumes with
// that answer, re-seeded from its branch in a new box, rather than being
// treated as a question nobody answered.
//
// Mutation: let the reaper reset an answered run to an unanswered reseed, and
// the run resumes with no answer, or not at all.
func TestAnAnswerWaitingOnABusySeatOutlivesThePauseTTL(t *testing.T) {
	t.Parallel()
	order := &sequence{}
	s := newBusySeat(t, func(*busySeat) sandbox.Resumer { return &orderedResumer{order: order} })
	s.launch(t, "t1")
	t1 := s.parks(t, "t1", "which branch?")
	s.launch(t, "t2")
	s.answer(t, t1, "stack it on gp-4")
	eventually(t, "the answer to be recorded", func() bool {
		return mustRun(t, s.store, "t1").Status == sandbox.StatusAnswered
	})
	s.retries.fire()

	// THE PAUSE TTL RUNS OUT while the seat is busy, and the reaper takes the box.
	if won, err := s.store.ExpirePause(t.Context(), "t1"); err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v, want the reaper to take the answered run's box", won, err)
	}
	if got := mustRun(t, s.store, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
		got.SandboxID != "" {
		t.Fatalf("run = %q with answer %v in box %q after the reap, want it still answered, "+
			"with its answer and no box", got.Status, got.Answer != nil, got.SandboxID)
	}

	s.runner.Finish(sandbox.Result{Success: true})
	complete(t, s.coordinator, s.store, "t2")
	s.retries.fireNow()
	if !strings.Contains(order.answerFor("t1"), "stack it on gp-4") {
		t.Fatalf("t1 was resumed with %q, want the answer recorded before its box was reaped",
			order.answerFor("t1"))
	}
	order.mu.Lock()
	box := order.boxes["t1"]
	order.mu.Unlock()
	if box != "" {
		t.Fatalf("t1 resumed into box %q, which the reaper destroyed; want it re-seeded", box)
	}
}

// AN ANSWER TO A RUN WHOSE PAUSE RAN OUT REACHES IT, WHILE ANOTHER JOB HOLDS
// THE SEAT. A parked run whose pause TTL lapses loses its box and reads
// `reseed`, which is still a question waiting on a person, on the same launch.
// The person's answer to it is recorded at once, and once the seat is free the
// run resumes with that answer, re-seeded from its branch in a new box.
//
// This is the run a person found stuck: parked, reaped to `reseed`, its seat
// busy coding, and every answer they gave accepted as `pending` and never seen.
//
// Mutation: deliver to the inbox again and the run is never answered; refuse an
// answer to a reseed run and it is never recorded; resume it into its old box
// and it resumes into a box the reaper destroyed.
func TestAnAnswerToARunWhosePauseRanOutReachesItWhileTheSeatIsBusy(t *testing.T) {
	t.Parallel()
	order := &sequence{}
	s := newBusySeat(t, func(*busySeat) sandbox.Resumer { return &orderedResumer{order: order} })
	s.launch(t, "t1")
	s.parks(t, "t1", "which branch?")
	if won, err := s.store.ExpirePause(t.Context(), "t1"); err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v, want the reaper to take the parked run's box", won, err)
	}
	t1 := mustRun(t, s.store, "t1")
	if t1.Status != sandbox.StatusReseed || t1.SandboxID != "" {
		t.Fatalf("run = %q in box %q after the reap, want it waiting to be re-seeded",
			t1.Status, t1.SandboxID)
	}

	s.launch(t, "t2")
	s.answer(t, t1, "stack it on gp-4")
	eventually(t, "the answer to the reseed run to be recorded", func() bool {
		got := mustRun(t, s.store, "t1")
		return got.Status == sandbox.StatusAnswered && got.Answer != nil
	})
	s.retries.fire()
	if got := order.answerFor("t1"); got != "" {
		t.Fatalf("t1 resumed with %q while t2 holds the seat, want it to wait for the seat", got)
	}

	s.runner.Finish(sandbox.Result{Success: true})
	complete(t, s.coordinator, s.store, "t2")
	s.retries.fireNow()
	if !strings.Contains(order.answerFor("t1"), "stack it on gp-4") {
		t.Fatalf("t1 was resumed with %q, want the answer given after its pause ran out",
			order.answerFor("t1"))
	}
	order.mu.Lock()
	box := order.boxes["t1"]
	order.mu.Unlock()
	if box != "" {
		t.Fatalf("t1 resumed into box %q, which the reaper destroyed; want it re-seeded", box)
	}
}
