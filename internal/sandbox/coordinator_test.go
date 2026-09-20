package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// captureLogs points the engine logger at a buffer for the length of one test
// and puts it back afterwards.
//
// The writer is mutex-guarded because the root logger is process-wide: this
// package's TestMain sends every line to io.Discard, and a swap that handed
// out an unguarded buffer would be a data race the moment anything else in
// the process logged.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	return &out
}

// resumeSpy records re-entries and can be made to fail.
type resumeSpy struct {
	mu       sync.Mutex
	requests []ResumeRequest
	err      error

	// owned counts the re-entries after which the TURN's own frame is
	// answerable for whatever was held up while it worked: one that returned
	// nil ran to its end, and one that returned [ErrResumeAbandoned] broke after
	// acting — both took their hold down on the way out.
	//
	// NOT EVERY RE-ENTRY, which is the distinction the coordinator's contract
	// turns on: any OTHER error leaves the hold standing, because the engine
	// keeps it on this package's promise to give the claim back so the
	// completion comes round again. A promise the coordinator then breaks —
	// a revert it cannot write — is exactly when it still owes somebody a
	// word, which is why [entryDrive.run] counts this rather than the calls.
	owned int

	// during runs inside the resumed Execute, before any error is returned:
	// the turn calling run_sandbox again (the box-reuse branch), or an event
	// redelivered to the seat while the turn runs.
	during func(ctx context.Context, run PendingRun)
}

func (s *resumeSpy) Resume(ctx context.Context, req ResumeRequest) error {
	s.mu.Lock()
	err, during := s.err, s.during
	if err == nil || errors.Is(err, ErrResumeAbandoned) {
		s.owned++
	}
	if err == nil {
		s.requests = append(s.requests, req)
	}
	s.mu.Unlock()
	// Before the error, as a real turn does: a resumed executor can call
	// run_sandbox again and break later in the same round.
	if during != nil {
		during(ctx, req.Run)
	}
	return err
}

// ownedHolds is how many re-entries ended their own hold. See owned.
func (s *resumeSpy) ownedHolds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owned
}

func (s *resumeSpy) calls() []ResumeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResumeRequest(nil), s.requests...)
}

// ledgerSpy records post-charges.
//
// IT RECORDS AN OVER-CAP CHARGE, which is what the engine's accountant does:
// it is coord.Budgets.PostCharge underneath, which moves both counters whatever
// the caps say, and answers whether the run took one PAST its cap. A spy that
// dropped those tokens would certify a coordinator that offers an over-cap run
// again on every retry.
type ledgerSpy struct {
	mu      sync.Mutex
	charged int
	calls   int
	over    bool
	err     error
}

func (l *ledgerSpy) Charge(_ context.Context, _, _ string, tokens int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return false, l.err
	}
	l.charged += tokens
	return l.over, nil
}

func (l *ledgerSpy) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.charged
}

func (l *ledgerSpy) set(over bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.over, l.err = over, err
}

func (l *ledgerSpy) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// failWith sets the error every later Resume returns, nil to let them through.
func (s *resumeSpy) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

type coordRig struct {
	*waiterRig
	coordinator *Coordinator
	resumer     *resumeSpy
	accountant  *ledgerSpy

	mu      sync.Mutex
	stopped []string
}

// stoppedTurns is every turn the coordinator reported as stopped, in order,
// however it stopped: parked on a question, destroyed, or ended.
func (r *coordRig) stoppedTurns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stopped...)
}

func newCoordRig(t *testing.T) *coordRig {
	t.Helper()
	base := newWaiterRig(t)
	rig := &coordRig{
		waiterRig:  base,
		resumer:    &resumeSpy{},
		accountant: &ledgerSpy{},
	}
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: base.queue, Pending: base.pending, Manager: base.manager,
		Resume: rig.resumer, Account: rig.accountant,
		Stopped: func(_ context.Context, handle, turnID string) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.stopped = append(rig.stopped, handle+"/"+turnID)
		},
		Now: func() time.Time { return base.now },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	rig.coordinator = coordinator
	return rig
}

// failures is every SandboxRunFailed the coordinator announced, deduped
// across the two topics each one goes to.
func (r *coordRig) failures() []types.SandboxRunFailed {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []types.SandboxRunFailed
	seen := map[string]bool{}
	for _, p := range r.queue.published {
		payload, ok := p.event.Data.(*types.SandboxRunFailed)
		if !ok || seen[p.event.ID.String()] {
			continue
		}
		seen[p.event.ID.String()] = true
		out = append(out, *payload)
	}
	return out
}

// completion is the signal the waiter raises for the job the row holds now.
func (r *coordRig) completion(turnID string) (types.SandboxRunCompleted, *events.Event) {
	run := r.get(turnID)
	payload := types.SandboxRunCompleted{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, CodingAgent: run.CodingAgent,
	}
	return payload, events.New(payload, events.TraceContext{TraceID: run.TraceID})
}

// ---------------------------------------------------------------------
// the busy gate
// ---------------------------------------------------------------------

// A coding job can run for hours, far past any broker ack window, so its
// seat's mail is parked rather than consumed and held.
func TestALaunchedRunParksTheSeatsMail(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")

	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat was busy before the start event arrived")
	}
	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		AgentHandle: "swe", TurnID: "t1",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if !rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat is not parked on its detached run")
	}
	// AND NO QUESTION IS OPEN ON IT. The two answers are disjoint by
	// construction, and a running job is the half that holds.
	if _, awaits := rig.coordinator.SeatRuns("swe"); awaits {
		t.Fatal("a running job counted as a run waiting for somebody's answer")
	}
}

// At-least-once means the start event can arrive twice, and a double
// increment would leave the seat parked forever after the run settled.
func TestARedeliveredStartDoesNotDoubleParkTheSeat(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")

	started := types.SandboxRunStarted{AgentHandle: "swe", TurnID: "t1"}
	for range 3 {
		if err := rig.coordinator.OnStarted(t.Context(), started); err != nil {
			t.Fatalf("OnStarted: %v", err)
		}
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked after its only run settled")
	}
}

// A person can take days to answer, and the answer arrives on the seat's own
// inbox — which a parked seat would never read.
func TestAParkedClarificationFreesTheSeat(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("a seat waiting on a person's answer cannot receive it while parked")
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want %q", got.Status, StatusAwaiting)
	}
	if got.Question != "which branch?" || got.Branch != "wip/t1" {
		t.Fatalf("the question and its branch were not recorded: %+v", got)
	}
	// AND THE QUESTION IS OPEN ON THE SEAT, which is the other half of the
	// same fact and was answered by nothing. Freeing the seat is what lets
	// the reply arrive; knowing a question is open is what makes the engine
	// recognise it as one instead of running it as an unrelated turn.
	held, awaits := rig.coordinator.SeatRuns("swe")
	if held || !awaits {
		t.Fatalf("SeatRuns = held %v / awaiting %v, want a free seat with an "+
			"open question", held, awaits)
	}
}

// AND THE PARK IS REPORTED TO THE ENGINE, which is the only way anything above
// this package can learn that an agent STOPPED.
//
// A parked run is neither finished nor working: the turn that suspended into
// it does not return, so nothing on the Ended path fires, and a person who may
// take days is now the only thing that can move it. Whatever the engine holds
// up "while the agent works" — the working indicator first of all — has
// nothing else to come down on.
//
// AFTER THE QUESTION IS ON THE ROW, and NOT BEFORE — which is the order the
// report's whole value depends on. Until that write lands the completion can
// still be retried, and a retry that resumed the turn would find the hold
// already dropped; what comes down here is a claim on somebody's screen.
//
// The ANNOUNCEMENT goes first, ahead of both, and is not what this asserts:
// see [Coordinator.park] for why a question recorded but never announced is
// the worse of the two orders.
func TestAParkedRunIsReportedOnlyOnceItsQuestionIsOnTheRow(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("asks")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
	})
	// The publish tells the order from the other end: it happens FIRST, so
	// a report made before the row was written is visible here as a stop
	// with no question recorded.
	rig.queue.before = func() {
		if got := rig.stoppedTurns(); len(got) != 0 {
			t.Errorf("%v was reported stopped before the question was recorded, "+
				"so a completion retried from here resumes a turn whose hold is "+
				"already down", got)
		}
	}
	payload, ev := rig.completion("asks")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.stoppedTurns(); len(got) != 1 || got[0] != "swe/asks" {
		t.Fatalf("the park reported %v, want the seat and turn that stopped", got)
	}
	if run := rig.get("asks"); run.Status != StatusAwaiting || run.Question == "" {
		t.Fatalf("the stop was reported with the row in %q / question %q, want "+
			"it recorded first", run.Status, run.Question)
	}
}

// ---------------------------------------------------------------------
// the at-most-once claim
// ---------------------------------------------------------------------

func TestACompletionResumesTheSuspendedLoop(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{
		Success: true, Text: "fixed the flake", DeliveredRefs: []string{"pr/42"},
		InputTokens: 900, OutputTokens: 200,
	})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want 1", len(calls))
	}
	if !calls[0].Success {
		t.Fatal("a successful run was reported as failed to the resumed phase")
	}
	if !strings.Contains(calls[0].Answer, "fixed the flake") {
		t.Fatalf("the answer does not carry the findings: %q", calls[0].Answer)
	}
	if !strings.Contains(calls[0].Answer, "pr/42") {
		t.Fatalf("the answer does not carry what was delivered: %q", calls[0].Answer)
	}
	if !strings.Contains(calls[0].Answer, "do NOT redo it") {
		t.Fatalf("the answer does not stop the executor redoing the work: %q", calls[0].Answer)
	}
	rig.finished("t1")
}

// A START EVENT REDELIVERED WHILE THE RESUME RUNS must not park the seat for
// good. At-least-once delivery can hand the seat its run's start again at any
// moment, and the busy count it recomputes from the store counts the run as
// holding the seat, because a claimed run is. The count was freed before the
// resume and nothing touched it after the settle, so the seat stayed parked on
// a run that no longer existed until the seat changed hands.
func TestARedeliveredStartDuringAResumeDoesNotParkTheSeatForGood(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		if err := rig.coordinator.OnStarted(ctx, types.SandboxRunStarted{
			AgentHandle: r.AgentHandle, TurnID: r.TurnID,
		}); err != nil {
			t.Errorf("OnStarted: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a run that settled while its start was redelivered")
	}
}

// Successive poll ticks can both fire before the first claim lands, and queue
// delivery is at-least-once.
func TestADuplicateCompletionResumesOnlyOnce(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	for range 4 {
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times for one run — the suspended conversation ran more than once", got)
	}
}

func TestConcurrentCompletionsResumeOnlyOnce(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	payload, ev := rig.completion("t1")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			if err := rig.coordinator.OnCompleted(context.Background(), payload, ev); err != nil {
				t.Errorf("OnCompleted: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times under contention, want 1", got)
	}
}

// Without the revert the NAK'd completion redelivers, the claim refuses, and
// the suspended conversation is permanently lost with the row stuck in resumed.
func TestAFailedResumeUnclaimsSoTheRetryCanWin(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = errors.New("the node lost the seat mid-resume")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was reported as success — the completion would be acked")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want it reverted to %q so the retry can re-claim", got.Status, StatusRunning)
	}
	// Running again, the run holds its seat again: the resume freed it,
	// and a turn slipped in now would run beside a job still owed a tail.
	if !rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat took new turns while its run waited for the retry")
	}

	rig.resumer.mu.Lock()
	rig.resumer.err = nil
	rig.resumer.mu.Unlock()
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times, want the retry to succeed exactly once", got)
	}
}

// AND A REVERT THAT CANNOT BE WRITTEN IS NOT A RETRY AT ALL.
//
// The un-claim above is the whole reason a failed resume is harmless, so the
// write that makes it is load-bearing: with it refused the row stays in
// [StatusResumed], where the completion poll does not look, a redelivery is
// refused by the very claim it would retake, no answer matches and no reaper
// expires it. "Leave it for next time" is then a turn destroyed in silence,
// with its box paused and billed until the seat happens to change hands — and
// the indicator the suspended turn left up heart-beating over it for the life
// of the process, which is the same hole a park that could not be written had
// and the fifth instance of it found in this file.
//
// So the run is ENDED instead, under the same reason a stranded park takes:
// nothing about the two failures differs once the claim is stuck.
func TestAResumeWhoseClaimCannotBeGivenBackEndsTheRun(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = errors.New("the node lost the seat mid-resume")
	rig.coordinator.pending = &refusingStore{
		inner: rig.pending, refuse: []string{"ReleaseClaim"},
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted = %v, want nil: the run is settled, so a redelivery would "+
			"find nothing to claim", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Errorf("killed %v, want the paused box of a turn nothing can resume reclaimed", killed)
	}
	if got := rig.stoppedTurns(); len(got) != 1 || got[0] != "swe/t1" {
		t.Errorf("reported %v as stopped, want the turn whose claim is stuck", got)
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.SandboxFailureClaimStranded {
		t.Fatalf("announced %+v, want one %q", failed, types.SandboxFailureClaimStranded)
	}
	if held, awaits := rig.coordinator.SeatRuns("swe"); held || awaits {
		t.Errorf("SeatRuns = held %v / awaiting %v, want neither: the seat's only run is over",
			held, awaits)
	}
}

// A RESUME THAT BROKE AFTER WRITING OUTSIDE THE ENGINE KEEPS ITS CLAIM.
//
// The counterpart of the un-claim above and the reason that one needs a
// condition. Reverting hands the completion back to a retry, and the retry
// re-enters the SAME suspended conversation — so every branch pushed and every
// issue commented on since the box came back happens a second time, up to the
// broker's whole delivery budget. The suspended turn is lost either way once
// the phase breaks; only one of the two outcomes also repeats the writes.
func TestAResumeThatBrokeAfterActingKeepsItsClaim(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeAbandoned)
	rig.coordinator.countRun("swe", StatusRunning)

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the completion was NAKed, so it will be redelivered into a "+
			"conversation whose writes already landed: %v", err)
	}
	if got, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || (found && got.Status == StatusRunning) {
		t.Fatalf("Get = %+v, found %v, %v; want the claim left taken so no retry wins the flip", got, found, err)
	}

	// AND THE LOST TURN IS SETTLED, like every other one. Nothing else will
	// ever act on it: no completion can claim it again, and recovery runs only
	// when the seat changes hands. Left claimed and busy, the seat parked
	// every message it received for as long as this node kept it, and the
	// paused box and its record stayed for good.
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a run whose resumed turn is over")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the lost turn's box %q reclaimed", killed, run.SandboxID)
	}
	rig.finished("t1")
	// The resumed turn published its own failed completion; this is not a
	// run that never resumed, so it is not announced as one.
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a turn that did resume", failed)
	}

	// AND THE RETRY DOES NOT WIN. This is the invariant, not the status
	// string: a second delivery of the same completion must not resume.
	rig.resumer.mu.Lock()
	rig.resumer.err = nil
	rig.resumer.mu.Unlock()
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the redelivery raised: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 0 {
		t.Fatalf("resumed %d times after abandoning, want none", got)
	}
}

// A resumed turn that called run_sandbox again and then broke after acting
// leaves a relaunched job no turn will ever suspend into: the conversation that
// would have been written is gone with the turn. So the settle reads the record
// as it is NOW and reclaims the box the relaunch is running in, rather than
// reading it as a reuse to leave alone.
func TestAResumeThatRelaunchedAndThenBrokeReclaimsTheRelaunchedBox(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
		// The relaunch's start event reaches the seat before the turn
		// breaks, so the seat is counted busy on the relaunched job.
		if err := rig.coordinator.OnStarted(ctx, types.SandboxRunStarted{
			AgentHandle: r.AgentHandle, TurnID: r.TurnID,
		}); err != nil {
			t.Errorf("OnStarted: %v", err)
		}
	}
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeAbandoned)
	rig.runner.Finish(Result{Success: true, Text: "first pass"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Fatalf("killed %v, want the relaunched job's box %q reclaimed", killed, run.SandboxID)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a relaunch nothing will ever resume")
	}
}

// AN ABANDONED RESUME IS OVER, AND SETTLED LIKE ONE.
//
// Keeping the claim is what stops the completion re-entering the conversation;
// it is not a reason to keep the run. The abandon path re-marked the seat busy
// and returned before the settle, so the row stayed in resumed, which holds
// the seat, and only a seat acquisition ever reaps a resumed row: on a live
// node the seat requeued every delivery it was sent, and its box stayed up and
// billed, until the process restarted or the seat moved.
//
// BOTH CAUSES, because they reach this path from opposite directions and only
// the sentinel is shared: a turn that wrote outside the engine, and one that
// panicked.
func TestAnAbandonedResumeFreesTheSeatAndReclaimsTheBox(t *testing.T) {
	for name, cause := range map[string]string{
		"after acting":    "the reviewer's provider went away",
		"after panicking": "panic: assignment to entry in nil map",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCoordRig(t)
			run := rig.launch("t1")
			if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
				AgentHandle: "swe", TurnID: "t1",
			}); err != nil {
				t.Fatalf("OnStarted: %v", err)
			}
			rig.runner.Finish(Result{Success: true, Text: "done"})
			rig.resumer.err = fmt.Errorf("%w: %s", ErrResumeAbandoned, cause)

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			if rig.coordinator.SeatHeldBySandbox("swe") {
				t.Fatal("the seat stayed parked on a run whose turn is over, so it " +
					"requeues every delivery until the process restarts")
			}
			rig.finished("t1")
			if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
				t.Fatalf("killed %v, want the finished run's box reclaimed", killed)
			}
		})
	}
}

// AND NEITHER SETTLE IS SILENT WHEN THE RECORD CANNOT BE READ.
//
// Both tails above read the row back before settling it, to learn which box to
// reclaim — and answered an unreadable read by returning, which leaves the run
// exactly where the two cases above spend their whole argument not leaving it:
// [StatusResumed], where no poll looks, no redelivery can re-claim, no answer
// matches and no reaper expires. The box stays paused and billed until the
// seat happens to change hands, and on a node that keeps its seat that is for
// ever.
//
// Driven for BOTH tails from one table, because the defect was one shape
// written twice and a case covering one of them is how the second survived.
func TestASettleWhoseRecordCannotBeReadEndsTheRunAnyway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume error
	}{
		{"a resumed turn that finished", nil},
		{"a resumed turn that broke after acting", fmt.Errorf(
			"%w: the reviewer's provider went away", ErrResumeAbandoned)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			run := rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			rig.resumer.err = tc.resume
			// The read that says which box to settle, and nothing else:
			// the claim, the collect and the resume all land.
			rig.coordinator.pending = &refusingStore{
				inner: rig.pending, refuse: []string{"Get"},
			}

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted = %v, want nil: the turn was re-entered, so the "+
					"completion must not come round again", err)
			}
			rig.finished("t1")
			if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
				t.Errorf("killed %v, want the paused box of a run nothing else will "+
					"ever settle reclaimed", killed)
			}
			if held, awaits := rig.coordinator.SeatRuns("swe"); held || awaits {
				t.Errorf("SeatRuns = held %v / awaiting %v, want neither", held, awaits)
			}
			if got := rig.stoppedTurns(); len(got) != 1 || got[0] != "swe/t1" {
				t.Errorf("reported %v as stopped, want the turn this settle ended", got)
			}
		})
	}
}

// AND IT STILL DOES NOT KILL A RELAUNCHED JOB, which is the one hazard the
// read ever guarded against and the reason the old answer was to do nothing.
//
// The condition moves to the store instead: the ending is licensed for the
// claim alone, and a resumed executor that called run_sandbox again has taken
// the row back through launching — so the delete declines, the box the new job
// is running in survives, and the row is left to the paths that end a
// launching run. Doing nothing bought the same protection at the price of
// stranding every run that had NOT relaunched, which is nearly all of them.
func TestAnUnreadableSettleLeavesARelaunchedJobAlone(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	}
	rig.coordinator.pending = &refusingStore{
		inner: rig.pending, refuse: []string{"Get"},
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v out from under the job the resumed turn relaunched", killed)
	}
	got := rig.get("t1")
	if got.Status != StatusLaunching {
		t.Fatalf("status = %q, want the relaunch left where its own turn will take it",
			got.Status)
	}
	if got.SandboxID != run.SandboxID {
		t.Fatalf("sandbox_id = %q, want the reused box still named by the row", got.SandboxID)
	}
}

// The claim snapshots the EXACT prior status, so a run answered out of a
// clarification reverts there rather than to running.
func TestAFailedResumeRevertsToWhereTheClaimFoundIt(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	rig.resumer.err = errors.New("no seat here")

	disposition, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", nil)
	if err == nil {
		t.Fatal("a failed resume reported success")
	}
	if disposition != AnswerDeferred {
		t.Fatalf("disposition = %q, want %q: the run is awaiting this same "+
			"answer again, so the delivery has to come back", disposition, AnswerDeferred)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want it back at %q", got.Status, StatusAwaiting)
	}
}

// A node with no seat to resume into must send the completion back rather than
// settle the run and tear the box down with the turn inside it, AND give the
// claim back, so the node that does hold the seat can win it.
//
// The resumer is what says so: only the engine knows which seats it holds.
// This used to be a nil resumer instead, for a node that could resume nothing
// at all, and that path returned before the revert below it, stranding the row
// in resumed where no retry could claim it.
func TestANodeThatCannotResumeSaysSoRatherThanSettling(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = fmt.Errorf("%w: seat %q is not held on this node",
		ErrResumeUnavailable, "swe")

	payload, ev := rig.completion("t1")
	err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
	if !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("OnCompleted = %v, want ErrResumeUnavailable", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim reverted to %q: the NAK'd completion comes back "+
			"to a claim that refuses it, and the suspended conversation is stranded", got.Status, StatusRunning)
	}
	// And the completion, redelivered to a node that DOES hold the seat, wins.
	rig.resumer.err = nil
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the redelivery failed: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times after the redelivery, want 1", got)
	}
}

// A row with nothing to re-enter cannot continue its turn, and the seat must
// not stay parked on it.
//
// No LIVE path produces one any more — a run holds [StatusLaunching] until its
// conversation is written and a launching run is not claimable — so this
// reaches the state the only way left: a row written by a build that predates
// that state, read by this one across a rolling upgrade.
func TestARunWithNoSuspendedConversationIsFailedAndFreed(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launching("t1")
	if err := rig.pending.SetStatus(t.Context(), "t1", StatusRunning, Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a run that can never continue")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the orphaned box reclaimed", killed)
	}
}

// ---------------------------------------------------------------------
// the box across a resume
// ---------------------------------------------------------------------

// The resumed Execute may call run_sandbox again, and re-provisioning would
// throw away the working tree.
func TestCollectPausesTheBoxRatherThanTearingItDown(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	box := rig.provider.Box(run.SandboxID)
	// The resumed executor relaunches, so the settle path must leave the box.
	//
	// THROUGH THE REAL LAUNCH, not a hand-written status flip. This test
	// used to set the row back to running itself, which no production path
	// did — so it certified a branch the engine could never reach, while
	// the real relaunch left the row in `resumed` and the settle below tore
	// down the box the second job was running in.
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if box.Closed() {
		t.Fatal("the box was torn down while a second run was using its checkout")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v mid-turn", killed)
	}
	if got := rig.get("t1"); got.Status != StatusLaunching {
		t.Fatalf("status = %q, want the relaunch to own the row", got.Status)
	}
}

// THE LAUNCH WINDOW. A coding job can finish between the launch starting it
// and the turn writing the conversation a resume re-enters — a few
// milliseconds on an idle host, hundreds on a loaded one, and every trivial
// run is a candidate. Claiming there hands the coordinator a run it cannot
// resume, and its only honest answer to that is to fail the turn: the agent's
// whole in-progress turn destroyed by a job that was too quick.
func TestACompletionInTheLaunchWindowLeavesTheTurnAlone(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launching("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done before the turn unwound"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %d times from a run with no conversation to resume", len(calls))
	}
	got := rig.get("t1")
	if got.Status != StatusLaunching {
		t.Fatalf("status = %q, want the launch left untouched", got.Status)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v out from under a turn that is still suspending", killed)
	}
	if !rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat was freed while its run was still launching")
	}

	// And the turn then suspends normally: the next completion resumes it,
	// which is the whole point of leaving it alone.
	rig.suspend("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted after the suspend: %v", err)
	}
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("resumed %d times after the suspend landed, want 1", len(calls))
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the box reclaimed once the turn was done", killed)
	}
}

// A destroyed turn must be as loud as a question. settleFailed marked the row,
// killed the box and freed the seat — and published nothing, so all three ways
// of reaching it presented to the seat, the dashboard and the requester as an
// identical silence. The first symptom was a wait that never ended.
func TestALostRunIsAnnouncedWithTheReasonItWasLost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		derail func(*coordRig)
		want   string
	}{
		{"the box cannot be read back", func(r *coordRig) {
			r.runner.CollectErr = errors.New("the box died mid-read")
		}, types.SandboxFailureCollect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			rig.launch("t1")
			rig.runner.Finish(Result{Success: true})
			tc.derail(rig)
			before := rig.queue.count()

			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}

			failed := rig.failures()
			if len(failed) != 1 {
				t.Fatalf("published %d failure events for a destroyed turn (queue grew by %d)",
					len(failed), rig.queue.count()-before)
			}
			if failed[0].Reason != tc.want {
				t.Fatalf("reason = %q, want %q", failed[0].Reason, tc.want)
			}
			if failed[0].TurnID != "t1" || failed[0].AgentHandle != "swe" {
				t.Fatalf("the announcement does not name the run: %+v", failed[0])
			}
			if failed[0].Detail == "" {
				t.Fatal("the announcement says what failed but not what it means")
			}
			// Both copies: the board reads the events topic, and the node
			// running the turn reads the seat's control topic.
			topicsSeen := rig.queue.topics()
			for _, want := range []string{
				topics.Event(types.SandboxRunFailed{}.EventType()),
				topics.AgentControl("swe"),
			} {
				if !slices.Contains(topicsSeen, want) {
					t.Fatalf("the failure did not reach %q; published to %v", want, topicsSeen)
				}
			}
		})
	}
}

// AND SO IS EVERY OTHER WAY A RUN STOPS, which is the property this file has
// missed one path of in each of three rounds.
//
// A settle ends a turn that is still SUSPENDED into its run: the record is
// deleted, the box reclaimed and the seat freed, and the frame that started the
// turn returned the moment it suspended. So nothing above this package learns
// that the agent stopped unless [CoordinatorOptions.Stopped] says so, and the
// working indicator was the first victim each time it did not — a run whose
// collect failed kept it up for the life of the process.
//
// EVERY ENDING, NOT EVERY ENDING SOMEBODY ENUMERATED. The report is made by
// [Coordinator.endRecord], the one place a run's record is deleted, so the
// cases below are evidence rather than the rule: a path added later reports
// because it deletes, not because anybody remembered. That is what makes a
// retirement — which ends runs without going through [Coordinator.finish] at
// all — one of them rather than a third site somebody has to remember. That includes a run whose turn
// came back and ended its own hold — the sentence "this run is over, stop
// holding anything up for its turn" is simply TRUE there, the resume frame has
// already acted on it, and a gate would be a second opinion about a question
// the store has answered.
func TestEveryWayARunStopsReportsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(*testing.T, *coordRig)
		want  []string
	}{
		{"a collect that could not read the box back", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true})
			rig.runner.CollectErr = errors.New("the box died mid-read")
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, []string{"swe/t1"}},
		{"a row carrying no suspended conversation", func(t *testing.T, rig *coordRig) {
			rig.launching("t1")
			if err := rig.pending.SetStatus(t.Context(), "t1", StatusRunning, Fence{}); err != nil {
				t.Fatalf("SetStatus: %v", err)
			}
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, []string{"swe/t1"}},
		{"a suspension the engine could not record", func(t *testing.T, rig *coordRig) {
			rig.launching("t1")
			if err := rig.coordinator.FailRun(t.Context(), "t1",
				types.SandboxFailureSuspensionUnrecorded,
				"the suspended conversation could not be written"); err != nil {
				t.Fatalf("FailRun: %v", err)
			}
			// REPORTED HERE TOO, although the suspending frame is still
			// on the stack and clears its own indicator off this same
			// settle. One rule for every settle rather than a branch for
			// the one caller that happens to have another way of
			// knowing: the release is idempotent, and a gate here would
			// be a second answer to "did this turn stop".
		}, []string{"swe/t1"}},
		{"a park whose question could not be written", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			// Neither write lands: the question, and then the claim the
			// completion would have been retried from. Nothing will ever
			// pick the run up, so the stop is real and this is the one
			// path that reports it through a settle it chose itself.
			rig.coordinator.pending = &refusingStore{
				inner: rig.pending, refuse: []string{"MarkAwaiting", "ReleaseClaim"},
			}
			rig.runner.Finish(Result{
				NeedsInput: true, Question: "which branch?", AskTo: "requester",
			})
			payload, ev := rig.completion("t1")
			// WHAT IT RETURNS IS ASSERTED WHERE IT MEANS SOMETHING —
			// [TestAParkThatCouldNotBeWrittenIsRetriedOrEnded] — because
			// here the only question is whether the stop was reported,
			// and failing on the error would mask that answer.
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
		}, []string{"swe/t1"}},
		{"a resume whose claim could not be given back", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.coordinator.pending = &refusingStore{
				inner: rig.pending, refuse: []string{"ReleaseClaim"},
			}
			rig.resumer.err = errors.New("the node lost the seat mid-resume")
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			// As above: the return is this path's own test's business.
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
		}, []string{"swe/t1"}},
		{"a run that parked on a question", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{
				NeedsInput: true, Question: "which branch?", AskTo: "requester",
			})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, []string{"swe/t1"}},
		{"a completion that resumed its turn and ended it", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
		}, []string{"swe/t1"}},
		{"a run ended because its seat left the company", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			// The retirement does not go through the settle every other
			// path uses — it keeps the record of a box it could not
			// reclaim — so it is the ending most likely to be forgotten,
			// and it was: the contract's own doc named two reporting
			// sites and this was the third.
			if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12); err != nil {
				t.Fatalf("RetireSeat: %v", err)
			}
		}, []string{"swe/t1"}},
		{"an ending whose record could not be deleted", func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.coordinator.pending = &refusingStore{
				inner: rig.pending, refuse: []string{"Finish"},
			}
			// A delete that errored MAY have landed, and nothing here
			// can tell. The retirement retries on its next tick, where a
			// second report finds nothing left to drop — while staying
			// quiet is the indicator standing over an agent that stopped.
			_ = rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12)
		}, []string{"swe/t1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			tc.drive(t, rig)
			if got := rig.stoppedTurns(); !slices.Equal(got, tc.want) {
				t.Errorf("reported %v as stopped, want %v", got, tc.want)
			}
		})
	}
}

// A PARK THAT COULD NOT BE WRITTEN IS A RETRY OR AN ENDING, NEVER A SILENCE.
//
// Its two branches are opposites and the row is what decides which: the run
// arrives here claimed, and a claim is the promise that a tail will run.
//
//   - THE CLAIM GOES BACK, so the completion comes round again — the poll reads
//     running rows and the box is still there to collect a second time. Nothing
//     stopped, so nothing is reported and the indicator a suspended turn left up
//     is CORRECT to still be up. The error goes back to the caller so the
//     delivery does too.
//   - THE CLAIM CANNOT GO BACK, and then nothing retries at all: a row left in
//     [StatusResumed] is polled by nothing, refused by the claim a redelivery
//     would retake, matched by no answer and expired by no reaper. So the run is
//     ENDED — box reclaimed, record deleted, loss announced under its own reason
//     — and the stop reported, because that turn is never coming back.
//
// AND THE SEAT'S COUNTS FOLLOW THE ROW, which is the third thing this branch
// got wrong: the park's two counts moved before the write, so a question the
// store never recorded left the seat reading as free with an answer pending —
// the exact state in which a person's reply is run as an unrelated turn.
func TestAParkThatCouldNotBeWrittenIsRetriedOrEnded(t *testing.T) {
	asks := Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"}

	t.Run("the claim goes back", func(t *testing.T) {
		rig := newCoordRig(t)
		run := rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.coordinator.pending = &refusingStore{
			inner: rig.pending, refuse: []string{"MarkAwaiting"},
		}
		rig.runner.Finish(asks)

		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
			t.Fatal("OnCompleted = nil, want the park's error so the completion comes back")
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Errorf("the row is %q, want %q: nothing polls or re-claims a row left in the claim",
				got.Status, StatusRunning)
		}
		if got := rig.stoppedTurns(); len(got) != 0 {
			t.Errorf("reported %v as stopped, want nothing: the box is still there and the "+
				"completion is retried, so the suspended turn's indicator is honest", got)
		}
		// COUNTED AS IT IS FILED. The row went back to running, which is a
		// seat HELD — where a delivery is offered to the answer match and
		// then requeued — and never a free seat with a question open on it,
		// which is a delivery consumed as an unrelated turn.
		held, awaits := rig.coordinator.SeatRuns("swe")
		if !held || awaits {
			t.Errorf("SeatRuns = held %v / awaiting %v, want the seat still held on a "+
				"run that is not parked", held, awaits)
		}
		// AND THE RETRY REALLY DOES COME ROUND, which is the whole
		// justification for keeping the indicator up: the poll fires the
		// completion again, the paused box is collected a second time and
		// the park lands.
		rig.coordinator.pending = rig.pending
		if fired := rig.tick(); fired != 1 {
			t.Fatalf("the poll fired %d completions for the reverted run, want 1", fired)
		}
		payload, ev = rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("the retried completion: %v", err)
		}
		if got := rig.get("t1"); got.Status != StatusAwaiting || got.Question != asks.Question {
			t.Errorf("after the retry the row is %q / %q, want the question parked",
				got.Status, got.Question)
		}
		if got := rig.stoppedTurns(); len(got) != 1 || got[0] != "swe/t1" {
			t.Errorf("the retried park reported %v, want the stop it did not report first "+
				"time round", got)
		}
		if killed := rig.provider.KilledIDs(); len(killed) != 0 {
			t.Errorf("killed %v; a run that is coming back keeps its box %q",
				killed, run.SandboxID)
		}
	})

	t.Run("the claim cannot go back", func(t *testing.T) {
		rig := newCoordRig(t)
		run := rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.coordinator.pending = &refusingStore{
			inner: rig.pending, refuse: []string{"MarkAwaiting", "ReleaseClaim"},
		}
		rig.runner.Finish(asks)

		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted = %v, want nil: the run is settled, so there is nothing "+
				"for a redelivery to claim", err)
		}
		rig.finished("t1")
		if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
			t.Errorf("killed %v, want the box of the run that can never resume reclaimed", killed)
		}
		if got := rig.stoppedTurns(); len(got) != 1 || got[0] != "swe/t1" {
			t.Errorf("reported %v as stopped, want the turn nothing will ever resume", got)
		}
		failed := rig.failures()
		if len(failed) != 1 || failed[0].Reason != types.SandboxFailureClaimStranded {
			t.Fatalf("announced %+v, want one %q: a lost turn cannot be quieter than the "+
				"question it was about to ask", failed, types.SandboxFailureClaimStranded)
		}
		if held, awaits := rig.coordinator.SeatRuns("swe"); held || awaits {
			t.Errorf("SeatRuns = held %v / awaiting %v, want neither on a seat whose only "+
				"run is over", held, awaits)
		}
	})
}

// AND A RUN THIS CALL DID NOT END REPORTS NOTHING, on the gate the failure
// announcement takes.
//
// A run a newer lease owns, or one somebody else settled between the claim and
// this settle, is that party's to end and to explain — and its holds are that
// party's to drop. Reporting it here would tell a node that is still running
// the turn that its turn is over.
func TestASettleSomebodyElseEndedReportsNoStop(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	var stopped []string
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: endedFirst{rig.pending},
		Manager: rig.manager, Resume: rig.resumer,
		Stopped: func(_ context.Context, handle, turnID string) {
			stopped = append(stopped, handle+"/"+turnID)
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errors.New("the box died mid-read")

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
	if len(stopped) != 0 {
		t.Errorf("reported %v as stopped for a run somebody else had already ended", stopped)
	}
}

// The recovery pass reaps a tail its previous owner abandoned, and that is a
// turn lost too — the seat's new owner is about to open its mailbox.
func TestAReapedAbandonedTailIsAnnounced(t *testing.T) {
	rig := newCoordRig(t)
	rig.launching("t1")

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-2", 7); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.SandboxFailureAbandoned {
		t.Fatalf("failures = %+v, want one %q", failed, types.SandboxFailureAbandoned)
	}
}

// A launch that never got to write its conversation is an unresumable tail
// holding a box, exactly like a claimed one whose engine died mid-resume.
// Nothing else will ever look at that row, and taking the seat's lease is what
// proves no live process is still driving it.
func TestSeatRecoveryReapsALaunchNobodyFinished(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launching("t1")

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-2", 7); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the abandoned launch's box reclaimed", killed)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the new owner took the seat parked on a run nothing can finish")
	}
}

func TestAFinishedTurnTearsTheBoxDown(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want [%s]", killed, run.SandboxID)
	}
	rig.finished("t1")
}

// A collect that fails means the job is over regardless: the seat must not be
// left parked on a run that finished.
func TestAFailedCollectStillFreesTheSeat(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errors.New("the box died mid-read")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked after a failed collect")
	}
	rig.finished("t1")
}

// A run somebody else ended between the claim and the settle (a new owner's
// recovery, a retirement) was announced by that party, for the reason it was
// really lost. The seat is still freed here, and the loss is not announced a
// second time under a reason it did not end for.
func TestASettleSomebodyElseEndedIsNotAnnouncedTwice(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: endedFirst{rig.pending}, Manager: rig.manager, Resume: rig.resumer,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errors.New("the box died mid-read")

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a run that is over")
	}
	rig.finished("t1")
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run somebody else had already ended", failed)
	}
}

// endedFirst is a store where another party always ends a run a moment
// before this caller does.
type endedFirst struct{ PendingStore }

func (s endedFirst) Finish(ctx context.Context, turnID string, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	if _, _, err := s.PendingStore.Finish(ctx, turnID, fence, whileIn); err != nil {
		return PendingRun{}, false, err
	}
	return s.PendingStore.Finish(ctx, turnID, fence, whileIn)
}

// ---------------------------------------------------------------------
// a claim names the job it is for
// ---------------------------------------------------------------------

// questions is every clarification the coordinator announced.
func (r *coordRig) questions() []types.SandboxClarificationRequested {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []types.SandboxClarificationRequested
	for _, p := range r.queue.published {
		if ask, ok := p.event.Data.(*types.SandboxClarificationRequested); ok {
			out = append(out, *ask)
		}
	}
	return out
}

// failPublishes makes every later publish fail, nil to let them through.
func (r *coordRig) failPublishes(err error) {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	r.queue.err = err
}

// A COMPLETION THAT OUTLIVED ITS JOB. A failed resume leaves two completions in
// flight for one job, the NAK'd delivery and the one the next poll tick
// publishes, and the retry that wins resumes the turn, whose executor may call
// run_sandbox again. The other then arrived at a row holding the NEXT job,
// still running, and claimed it: it collected a half-written result, resumed
// the turn on it and tore the new job's box down.
func TestAStaleCompletionDoesNotClaimTheNextRun(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 500})
	stale, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), stale, ev); err != nil {
		t.Fatalf("the first run's completion: %v", err)
	}
	rig.suspend("t1")
	next := rig.get("t1")

	if err := rig.coordinator.OnCompleted(t.Context(), stale, ev); err != nil {
		t.Fatalf("the stale completion: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times, want only the first run's completion to resume", got)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.LaunchID != next.LaunchID {
		t.Fatalf("row = %s/%s, want the second run still running", got.Status, got.LaunchID)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v under a job that was still running", killed)
	}
	if got := rig.accountant.total(); got != 500 {
		t.Fatalf("charged %d, want only the first run's 500", got)
	}
}

// THE SETTLE IS THE CLAIM'S TOO. A resumed turn that called run_sandbox again
// returns only once its frame has unwound, and the new job can finish, be
// claimed by its own completion and park on a question of its own inside that
// window. The settle read the row back, saw a status that was neither running
// nor launching, and tore down the paused box holding that question's
// checkout, then marked the run done under the question.
func TestASettleLeavesTheNextJobItsOwnTail(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
		if suspended, err := rig.pending.MarkSuspended(ctx, r.TurnID, map[string]any{
			"pending_tool_name": "run_sandbox",
		}); err != nil || !suspended {
			t.Errorf("the relaunch's suspension: suspended=%v err=%v", suspended, err)
		}
		rig.runner.Finish(Result{NeedsInput: true, Question: "which file?", AskTo: "requester"})
		next, _, err := rig.pending.Get(ctx, r.TurnID)
		if err != nil {
			t.Errorf("Get: %v", err)
		}
		completion := types.SandboxRunCompleted{
			AgentHandle: next.AgentHandle, TurnID: next.TurnID, LaunchID: next.LaunchID,
			SandboxID: next.SandboxID, CodingAgent: next.CodingAgent,
		}
		if err := rig.coordinator.OnCompleted(ctx, completion, events.New(completion, events.TraceContext{})); err != nil {
			t.Errorf("the next job's completion: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the first job's completion: %v", err)
	}

	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Question != "which file?" {
		t.Fatalf("row = %s %q, want the next job waiting on its question", got.Status, got.Question)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v under a question still waiting for its answer", killed)
	}
}

// A RELAUNCH THAT NEVER STARTED IS SETTLED WITH THE TURN. It could get no box,
// so the row still names the previous job's, paused, and nothing else will
// reclaim it: a paused box has no provider-side expiry, and the pause reaper
// only looks at runs waiting on a person.
func TestAFinishedTurnTearsDownTheBoxAFailedRelaunchLeftBehind(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		rig.provider.CreateErr = errors.New("no capacity")
		defer func() { rig.provider.CreateErr = nil }()
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, launchReq(r.TurnID)); err == nil {
			t.Error("the relaunch got a box the fixture refuses")
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Fatalf("killed %v, want the paused box %q the failed relaunch left named", killed, run.SandboxID)
	}
	rig.finished("t1")
}

// THE WAITER'S OWN SIGNAL IS ONE THE COORDINATOR CLAIMS. Every other case
// here hands the coordinator a completion built from the row; this one takes
// the one the poll published, so the two cannot disagree about what names a
// job without a test noticing that every run would then wait forever.
func TestTheWaitersCompletionIsClaimed(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d completions, want 1", fired)
	}

	rig.queue.mu.Lock()
	var control *events.Event
	for _, p := range rig.queue.published {
		if p.topic == topics.AgentControl("swe") {
			control = p.event
		}
	}
	rig.queue.mu.Unlock()
	if control == nil {
		t.Fatal("the completion never reached the seat's control topic")
	}
	if err := rig.coordinator.OnEvent(t.Context(), control); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times from the waiter's completion, want 1", got)
	}
}

// A DUPLICATE COMPLETION OF A PARKED RUN. The row is waiting on a person, and
// only their answer may take it; a completion that could claim it too
// reconnected to the paused box, collected the same result and asked the same
// question a second time.
func TestADuplicateCompletionDoesNotAskAParkedQuestionAgain(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

	payload, ev := rig.completion("t1")
	for range 2 {
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
	}
	if got := len(rig.questions()); got != 1 {
		t.Fatalf("the question was asked %d times, want once", got)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run still waiting on its answer", got.Status)
	}
}

// staleFind answers the parked-run lookup from a snapshot taken before the
// row moved on, which is what the lookup is by the time the claim lands.
type staleFind struct {
	PendingStore
	snapshot PendingRun
}

func (s staleFind) FindAwaitingByConversation(context.Context, string, ConversationRef) (PendingRun, bool, error) {
	return s.snapshot, true, nil
}

// AN ANSWER BELONGS TO THE JOB THAT ASKED. The lookup that matched it is a
// snapshot, and a claim that took whatever the row held by then handed the
// answer to the job that had replaced the one asking: it resumed the turn with
// a reply to a question nothing had asked, on top of a job still running.
func TestAnAnswerDoesNotClaimTheJobThatReplacedTheAsker(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.park("t1")
	asked := rig.get("t1")
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	rig.suspend("t1")

	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: staleFind{PendingStore: rig.pending, snapshot: asked},
		Manager: rig.manager, Resume: rig.resumer, Account: rig.accountant,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	handled, err := coordinator.TryResumeFromAnswer(t.Context(), "swe", answerOnTheDM, "use main", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if handled != AnswerConsumed {
		t.Fatalf("disposition = %q, want %q: an answer to a question already "+
			"superseded was run as an unrelated message", handled, AnswerConsumed)
	}
	if got := len(rig.resumer.calls()); got != 0 {
		t.Fatalf("resumed %d times into a job that asked nothing", got)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the replacing job left running", got.Status)
	}
}

// parkFails is a store whose MarkAwaiting fails a set number of times.
type parkFails struct {
	PendingStore
	mu   sync.Mutex
	left int
}

func (p *parkFails) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	p.mu.Lock()
	if p.left > 0 {
		p.left--
		p.mu.Unlock()
		return errors.New("the coordination store did not answer")
	}
	p.mu.Unlock()
	return p.PendingStore.MarkAwaiting(ctx, turnID, q)
}

// A QUESTION THAT COULD NOT BE RECORDED IS ASKED AGAIN. The failed park left
// the row resumed, which the completion's retry can never claim, so the run
// sat stranded with its box paused and its seat freed until the seat changed
// hands and recovery reaped it as abandoned.
func TestAQuestionThatCouldNotBeRecordedIsAskedAgain(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: &parkFails{PendingStore: rig.pending, left: 1},
		Manager: rig.manager, Resume: rig.resumer, Account: rig.accountant,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	coordinator.countRun("swe", StatusRunning)
	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a question that was never recorded was acked")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim handed back for the retry", got.Status)
	}
	if !coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat was freed before its question was on the row")
	}

	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Question != "which branch?" {
		t.Fatalf("row = %s %q, want the retry to park the question", got.Status, got.Question)
	}
	if coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked once its question was recorded")
	}
}

// statusAtAsk records the run's status at the instant its question is
// announced.
type statusAtAsk struct {
	*recorder
	rig    *coordRig
	status []string
}

func (s *statusAtAsk) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if ask, ok := ev.Data.(*types.SandboxClarificationRequested); ok {
		run, _, err := s.rig.pending.Get(ctx, ask.TurnID)
		if err != nil {
			return err
		}
		s.status = append(s.status, run.Status)
	}
	return s.recorder.Publish(ctx, topic, ev)
}

// A RUN IS PARKED ONLY ONCE ITS QUESTION IS OUT. From the moment the row says
// awaiting, a completion can no longer reach it and an answer can, so a
// question that failed to go out after that point could neither be asked again
// nor handed back without stomping on the answer's claim. Zero pause TTL
// included: tearing the box down before the question is out would leave a
// retry nothing to collect.
func TestAQuestionIsAskedBeforeTheRunIsParked(t *testing.T) {
	for _, ttl := range []float64{DefaultPauseTTL.Seconds(), 0} {
		t.Run(fmt.Sprintf("pause ttl %gs", ttl), func(t *testing.T) {
			rig := newCoordRig(t)
			rig.launch("t1")
			if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
				SandboxID: rig.get("t1").SandboxID, CommandID: "cmd-1",
				CodingAgent: "claude-code", PauseTTLSec: ttl,
			}, Fence{}); err != nil {
				t.Fatalf("AttachSandbox: %v", err)
			}
			rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

			spy := &statusAtAsk{recorder: rig.queue, rig: rig}
			coordinator, err := NewCoordinator(CoordinatorOptions{
				Queue: spy, Pending: rig.pending, Manager: rig.manager,
				Resume: rig.resumer, Account: rig.accountant,
			})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			payload, ev := rig.completion("t1")
			if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			if len(spy.status) != 1 || spy.status[0] != StatusResumed {
				t.Fatalf("status when the question went out = %v, want it still claimed", spy.status)
			}
			if got := rig.get("t1"); !slices.Contains(Awaiting, got.Status) {
				t.Fatalf("status = %q, want the run parked once the question was out", got.Status)
			}
		})
	}
}

// A QUESTION THAT COULD NOT BE ANNOUNCED IS ASKED AGAIN. It goes out before the
// row says awaiting, because once it does no redelivery of the completion can
// reach it: a question recorded but never asked would wait for an answer
// nobody knew to give.
func TestAQuestionThatCouldNotBeAnnouncedIsAskedAgain(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

	rig.failPublishes(errors.New("the stream did not answer"))
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a question that was never asked was acked")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim handed back for the retry", got.Status)
	}

	rig.failPublishes(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := len(rig.questions()); got != 1 {
		t.Fatalf("the question was announced %d times, want once", got)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run waiting on its answer", got.Status)
	}
}

// ---------------------------------------------------------------------
// a claim is handed back only while it stands
// ---------------------------------------------------------------------

// relaunchThenBreak is a resumed executor that calls run_sandbox again and
// then breaks before its turn can suspend on the new job, having written
// nothing outside the engine: a failed relaunch is a failed call, and a
// failed call proves no outward write.
type relaunchThenBreak struct {
	relaunch func(ctx context.Context, run PendingRun)
}

func (r relaunchThenBreak) Resume(ctx context.Context, req ResumeRequest) error {
	r.relaunch(ctx, req.Run)
	return errors.New("the model provider did not answer")
}

// withResumer is the rig's coordinator over the same store and box, resuming
// through r.
func (r *coordRig) withResumer(t *testing.T, resume Resumer) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: r.queue, Pending: r.pending, Manager: r.manager,
		Resume: resume, Account: r.accountant,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return coordinator
}

// A RELAUNCH IS NOT THE CLAIM'S TO HAND BACK. The resumed executor called
// run_sandbox again, so the row holds a new launch with the previous job's
// conversation cleared, and that launch failed to start and was settled
// failed. Reverting it to running left a row with no box and no conversation
// that no poll would ever complete, and a seat re-marked busy on it, its mail
// parked until the seat changed hands (where recovery re-marked it again).
func TestAFailedResumeLeavesARelaunchItsOwnOutcome(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 1000})
	coordinator := rig.withResumer(t, relaunchThenBreak{relaunch: func(ctx context.Context, r PendingRun) {
		rig.runner.StartErr = errors.New("the coding agent would not start")
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err == nil {
			t.Error("the relaunch started a job the fixture refuses")
		}
	}})
	coordinator.countRun("swe", StatusRunning)

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	// The relaunch ENDED the run when it could not start, and the hand-back
	// of the PREVIOUS job's claim must not bring it back: a release names the
	// launch it took, and that launch went with the record.
	rig.finished("t1")
	if coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat was parked on a run nothing will ever complete")
	}
	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll fired %d completions for a run that has none", fired)
	}
}

// AND NOT A SECOND CHARGE OF THE FIRST JOB. A relaunch that could get no box at
// all leaves the row naming the previous one, finished and paused. Reverted to
// running under the new launch's name, the poll found that job finished, and
// its completion claimed the relaunch, collected the first job's result a
// second time and charged it again, the relaunch having cleared the record of
// the first charge, before failing the turn for having nothing to resume.
func TestAFailedRelaunchDoesNotChargeThePreviousJobAgain(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 1000})
	coordinator := rig.withResumer(t, relaunchThenBreak{relaunch: func(ctx context.Context, r PendingRun) {
		// A reattach that failed falls through to a fresh box, and none
		// could be had either.
		rig.provider.CreateErr = errors.New("no capacity")
		defer func() { rig.provider.CreateErr = nil }()
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, launchReq(r.TurnID)); err == nil {
			t.Error("the relaunch got a box the fixture refuses")
		}
	}})

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	if fired := rig.tick(); fired != 0 {
		t.Fatalf("the poll fired %d completions for the previous job", fired)
	}
	rig.deliverControl(t)
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens for one job of 1000", got)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %d failures for a turn nothing tried to resume again", len(failed))
	}
}

// NOR A RUN THE SEAT'S NEXT OWNER HAS REAPED. A lease that moved mid-resume
// hands the seat to a node whose recovery finds the row resumed, reaps it as
// abandoned, tears its box down and announces the loss. Reverted after that,
// the run came back as running with no box, a turn announced lost and never
// completed, holding its seat on every node that recovered it afterwards.
func TestAFailedResumeDoesNotReviveARunTheSeatsNextOwnerReaped(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 1000})
	successor := rig.withResumer(t, &resumeSpy{})
	coordinator := rig.withResumer(t, relaunchThenBreak{relaunch: func(ctx context.Context, _ PendingRun) {
		if err := successor.RecoverSeat(ctx, "swe", "node-b:1", 2); err != nil {
			t.Errorf("the successor's recovery: %v", err)
		}
	}})

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	// The reap ended the run, and the hand-back leaves it ended.
	rig.finished("t1")
	if err := successor.RecoverSeat(t.Context(), "swe", "node-c:1", 3); err != nil {
		t.Fatalf("a later recovery: %v", err)
	}
	if successor.SeatHeldBySandbox("swe") {
		t.Fatal("a later owner parked the seat on a run already announced lost")
	}
}

// honoursCancel refuses a release on a cancelled context, the way the fleet's
// store does over the wire and the in-memory twin does not.
type honoursCancel struct{ PendingStore }

func (s honoursCancel) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.PendingStore.ReleaseClaim(ctx, turnID, release)
}

// drained is a resume a drain breaks: the delivery's context is cancelled
// under it, and it fails with that cancellation.
type drained struct{ cancel context.CancelFunc }

func (d drained) Resume(ctx context.Context, _ ResumeRequest) error {
	d.cancel()
	return ctx.Err()
}

// A DRAIN THAT BREAKS A RESUME STILL HANDS THE CLAIM BACK. The release is a
// rollback of the claim, and the failure it undoes is the drain's own
// cancellation: inheriting it, the release wrote nothing, the run stayed
// resumed, and the node the drain handed the seat to reaped it as abandoned
// rather than resuming it.
func TestADrainThatBreaksAResumeStillHandsTheClaimBack(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	delivery, cancel := context.WithCancel(t.Context())
	defer cancel()
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: honoursCancel{rig.pending}, Manager: rig.manager,
		Resume: drained{cancel: cancel},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(delivery, payload, ev); !errors.Is(err, context.Canceled) {
		t.Fatalf("OnCompleted = %v, want the drain's cancellation sent back", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim handed back for the seat's next owner", got.Status)
	}
	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-b:1", 2); err != nil {
		t.Fatalf("the next owner's recovery: %v", err)
	}
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the redelivery to the next owner: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("the next owner resumed %d times, want the turn continued once", got)
	}
}

// deliverControl hands the coordinator every completion published to the
// seat's control topic, as the broker would.
func (r *coordRig) deliverControl(t *testing.T) {
	t.Helper()
	r.queue.mu.Lock()
	var completions []*events.Event
	for _, p := range r.queue.published {
		if _, ok := p.event.Data.(*types.SandboxRunCompleted); ok && p.topic == topics.AgentControl("swe") {
			completions = append(completions, p.event)
		}
	}
	r.queue.mu.Unlock()
	for _, ev := range completions {
		if err := r.coordinator.OnEvent(t.Context(), ev); err != nil {
			t.Fatalf("OnEvent: %v", err)
		}
	}
}

// A FAILED ANSWER DOES NOT PARK THE SEAT. The claim goes back to waiting on a
// person, which does not hold the seat, and marking it busy regardless parked
// every later delivery until something recounted the seat. Nothing did: the
// answer's retry resumes and settles the run, and neither step takes back a
// mark the failure left, so the seat stayed parked after its run was done.
func TestAFailedAnswerLeavesTheSeatFree(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.park("t1")
	rig.resumer.failWith(errors.New("the model provider did not answer"))

	handled, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", answerOnTheDM, "use main", nil)
	if err == nil || handled != AnswerDeferred {
		t.Fatalf("TryResumeFromAnswer = %v, %v, want the answer still owed to "+
			"the run and sent back", handled, err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run waiting on its answer again", got.Status)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("a seat whose run went back to waiting on a person was parked")
	}

	rig.resumer.failWith(nil)
	if _, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", answerOnTheDM, "use main", nil); err != nil {
		t.Fatalf("the answer's retry: %v", err)
	}
	rig.finished("t1")
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked after its only run settled")
	}
}

// ---------------------------------------------------------------------
// accounting
// ---------------------------------------------------------------------

// A cap cannot un-spend a collected run, so a charge it refuses does not stop
// the turn: the tokens are spent and the work they bought continues. The
// refusal itself moves no counter, which is the shared counter's answer to
// every charge it refuses; this case used to assert the opposite of a spy
// that recorded refused charges, which the real accountant never did.
func TestAnOverBudgetChargeDoesNotStopTheResume(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.accountant.set(true, nil)
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 4000, OutputTokens: 1000})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.accountant.asked(); got != 1 {
		t.Fatalf("offered the run's spend %d times, want once", got)
	}
	if len(rig.resumer.calls()) != 1 {
		t.Fatal("an over-budget charge stopped the turn from continuing")
	}
}

// THE RETRY IS NOT A SECOND RUN. A resume that fails reverts the claim and the
// completion comes back, and the retry collects the same finished job again.
// Charging on every pass billed the seat and the company once per retry, for
// as long as the resume kept failing: every poll tick, on a node that had lost
// the seat.
func TestARetriedResumeChargesTheRunOnce(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
	rig.resumer.failWith(fmt.Errorf("%w: the seat moved", ErrResumeUnavailable))

	payload, ev := rig.completion("t1")
	for attempt := range 3 {
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
			t.Fatalf("attempt %d: a failed resume was acked", attempt+1)
		}
	}
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry that resumes: %v", err)
	}

	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times, want the last retry to resume once", got)
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens over four deliveries of one run, want its 1000 once", got)
	}
}

// THE RECORD IS THE FLEET'S, NOT THE COORDINATOR'S. A failed resume's retry
// goes wherever the seat is, which after a lease move or a restart is a
// coordinator that never saw the first charge. Only the run's own row can tell
// it the spend is already counted.
func TestARetryOnAnotherNodeChargesTheRunOnce(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
	rig.resumer.failWith(fmt.Errorf("%w: the seat moved", ErrResumeUnavailable))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}

	// The seat's next owner: a coordinator of its own over the same store,
	// charging the same fleet counter.
	successor := &ledgerSpy{}
	next, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Account: successor,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := next.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the successor's retry: %v", err)
	}
	if got := rig.accountant.total() + successor.total(); got != 1000 {
		t.Fatalf("charged %d tokens across two nodes, want the run's 1000 once", got)
	}
}

// onlyTheClaim is a coordination store that answers the claim and its release
// and refuses every other write a tail makes, the shape of a store under a
// blip that lets some compare-and-swaps through and not others.
type onlyTheClaim struct{ PendingStore }

var errUnanswered = errors.New("the coordination store did not answer")

func (onlyTheClaim) MarkBoxPaused(context.Context, string, time.Time) error { return errUnanswered }
func (onlyTheClaim) MarkAwaiting(context.Context, string, Clarification) error {
	return errUnanswered
}
func (onlyTheClaim) SetStatus(context.Context, string, string, Fence) error { return errUnanswered }
func (onlyTheClaim) ReleaseBox(context.Context, string) error               { return errUnanswered }

// THE RECORD NEEDS NO WRITE OF ITS OWN. Kept in one, a store that refused it
// and then accepted the release reopened the run with no record, and the
// retry charged the same job again. Riding on the release, the run is handed
// back with its record or not at all.
func TestAChargeIsRecordedByTheWriteThatHandsTheClaimBack(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
	resumer := &resumeSpy{err: fmt.Errorf("%w: the seat moved", ErrResumeUnavailable)}
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: onlyTheClaim{rig.pending}, Manager: rig.manager,
		Resume: resumer, Account: rig.accountant,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	payload, ev := rig.completion("t1")
	for attempt := range 2 {
		if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
			t.Fatalf("attempt %d: a failed resume was acked", attempt+1)
		}
	}
	resumer.failWith(nil)
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry that resumes: %v", err)
	}
	if got := len(resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times, want the last retry to resume once", got)
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens over three deliveries of one run, want its 1000 once", got)
	}
}

// A duplicate completion of a run that parked on a question is the other way
// one job reaches the charge twice.
func TestADuplicateCompletionOfAParkedRunChargesItOnce(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		InputTokens: 700, OutputTokens: 300,
	})

	payload, ev := rig.completion("t1")
	for range 2 {
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens for one parked run, want its 1000 once", got)
	}
}

// A SECOND RUN IN ONE TURN IS A SECOND SPEND. The record is the launch's, so a
// resumed executor that calls run_sandbox again is charged for that job too,
// even when the first job's completion was retried and its row carries the
// first job's record into the relaunch.
func TestASecondRunInOneTurnIsChargedToo(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 400, OutputTokens: 100})
	rig.resumer.failWith(fmt.Errorf("%w: the seat moved", ErrResumeUnavailable))
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the first run's completion: %v", err)
	}

	rig.resumer.mu.Lock()
	rig.resumer.during = nil
	rig.resumer.mu.Unlock()
	rig.suspend("t1")
	rig.runner.Finish(Result{Success: true, Text: "second pass", InputTokens: 200, OutputTokens: 300})
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the second run's completion: %v", err)
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens for two runs of 500, want both", got)
	}
}

// ONLY A CHARGE THAT MOVED THE COUNTER IS RECORDED. A counter that never
// answered left it where it was, so the retry offers the spend again rather
// than inheriting an answer about a counter that may be reachable by then.
func TestAnUnrecordedChargeIsOfferedAgainOnTheRetry(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
	rig.accountant.set(false, errors.New("counter unreachable"))
	rig.resumer.failWith(fmt.Errorf("%w: the seat moved", ErrResumeUnavailable))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	rig.accountant.set(false, nil)
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d, want the retry to count the spend the first pass did not", got)
	}
}

// A RUN THAT WENT OVER A CAP IS STILL CHARGED ONCE.
//
// The post-charge records the spend whatever the caps say, so the boolean it
// answers with says the run took a counter PAST its cap — not that nothing was
// recorded. Read the second way, an over-cap run is charged again on every
// completion retry, which is the double-charge the run's own record exists to
// stop, reintroduced for exactly the companies a cap is binding on.
func TestARunThatWentOverItsCapIsChargedOnceAcrossARetry(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
	rig.accountant.set(true, nil)
	rig.resumer.failWith(fmt.Errorf("%w: the seat moved", ErrResumeUnavailable))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := rig.accountant.total(); got != 1000 {
		t.Fatalf("charged %d tokens for one run of 1000: an over-cap charge was "+
			"read as unrecorded and offered again", got)
	}
}

// Accounting is a store call that can fail on its own, and its failing is not
// a reason to lose the turn.
func TestAFailedChargeDoesNotAbortTheResume(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.accountant.err = errors.New("counter unreachable")
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 100})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if len(rig.resumer.calls()) != 1 {
		t.Fatal("an unreachable counter cost the turn")
	}
}

// ---------------------------------------------------------------------
// the clarification round trip
// ---------------------------------------------------------------------

// answerOnTheDM is the person's reply to the question this rig's run parked
// on: the same DM line, in the thread the run's own trigger arrived in.
//
// THE TWO VALUES DIFFER, because the rig's row is a direct message's — so a
// case that hands this to the coordinator is exercising a real pair rather
// than one string written twice.
var answerOnTheDM = ConversationRef{Identity: "chat:D1", Partition: "chat:D1:root-1"}

func TestTheAnswerToAParkedQuestionResumesTheSameTurn(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	disposition, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if disposition != AnswerConsumed {
		t.Fatalf("disposition = %q, want %q: the answer was not matched to the "+
			"run that asked", disposition, AnswerConsumed)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want 1", len(calls))
	}
	if calls[0].Run.TurnID != "t1" {
		t.Fatalf("resumed turn %q, want the one that asked", calls[0].Run.TurnID)
	}
	if !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("the answer did not reach the loop: %q", calls[0].Answer)
	}
	if !strings.Contains(calls[0].Answer, "which branch?") {
		t.Fatalf("the loop was not reminded what it asked: %q", calls[0].Answer)
	}
}

// AND THE ANSWER NEED NOT ARRIVE IN THE BATCH THE QUESTION WAS ASKED IN.
//
// The engine's own chat prompt tells a seat to reply to a top-level direct
// message AS A THREAD, so the two halves of a DM's clarification routinely sit
// in different partitions: the question parked under one, the person's answer
// arriving in another. Matched on the partition this resume never fires at
// all — the box waits out its pause TTL while the answer sits in the seat's
// inbox — and a DM is one conversation however it is threaded, which is what
// the match runs on now.
func TestAnAnswerOutsideTheQuestionsPartitionStillResumesTheRun(t *testing.T) {
	rig := newCoordRig(t)
	// Parked from a thread on the DM line: conversation chat:D1, partition
	// chat:D1:root-1.
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	// A TOP-LEVEL reply on the same DM line: same conversation, different
	// batch — which is exactly the shape the row's own partition cannot
	// match.
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		ConversationRef{Identity: "chat:D1", Partition: "chat:D1"}, "use main", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if disposition != AnswerConsumed {
		t.Fatalf("an answer arriving outside the question's own partition reached "+
			"nobody (disposition %q): the parked run waits for a reply it can "+
			"never be given", disposition)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 || calls[0].Run.TurnID != "t1" {
		t.Fatalf("resumed %+v, want the one run that asked", calls)
	}
}

// AND THE LINE THAT RECORDS IT NAMES BOTH ENDS OF THE MATCH.
//
// The match has two keys on each side, and which one decided is the whole
// diagnosis: a row whose conversation equals the delivery's was admitted on
// the identity, a row with no conversation was matched on the partition
// fallback because it predates the split, and two questions parked on one
// direct-message line are told apart by the partitions alone. Logging the
// identity by itself — which is what this line did once the match moved onto
// it — left all three indistinguishable, on the exact path where the two
// values differ and an operator is asking why THIS run woke.
func TestTheAnsweredLineNamesBothKeysOfBothEnds(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	// Parked from a thread on the DM line: conversation chat:D1, partition
	// chat:D1:root-1.
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	// A TOP-LEVEL reply on the same line: same conversation, different
	// batch, so the identity is what admitted this row.
	if _, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		ConversationRef{Identity: "chat:D1", Partition: "chat:D1"},
		"use main", nil); err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}

	line := logs.String()
	if !strings.Contains(line, "sandbox_clarification_answered") {
		t.Fatalf("nothing recorded the resume: %q", line)
	}
	for _, want := range []string{
		`conversation=chat:D1`,
		`partition=chat:D1`,
		`run_conversation=chat:D1`,
		`run_partition=chat:D1:root-1`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not carry %s, so a reader cannot tell "+
				"which row won or why: %q", want, line)
		}
	}
}

// blindStore is a PendingStore whose conversation lookup cannot be reached.
//
// Everything else is the real one: the failure this covers is a read that
// could not be made, not a store that is absent.
type blindStore struct {
	PendingStore
	err error
}

func (s blindStore) FindAwaitingByConversation(context.Context, string, ConversationRef) (PendingRun, bool, error) {
	return PendingRun{}, false, s.err
}

// AND SO DOES THE LINE FOR A LOOKUP THAT COULD NOT BE MADE.
//
// This one FAILS OPEN — an unreadable store must not swallow an ordinary
// message — so the delivery goes on to be handled as a normal inbound and
// the only trace that a parked run may have just missed its answer is this
// line. With one key on it an operator cannot tell which read was attempted
// against what, which on a direct message is two different values.
func TestTheLookupFailureNamesBothKeysOfTheDelivery(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	rig.coordinator.pending = blindStore{
		PendingStore: rig.pending,
		err:          errors.New("the coordination store is unreachable"),
	}

	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		ConversationRef{Identity: "chat:D1", Partition: "chat:D1:root-1"},
		"use main", nil)
	if err != nil {
		t.Fatalf("a lookup failure must not fail the delivery: %v", err)
	}
	if disposition != AnswerNotMine {
		t.Fatalf("disposition = %q, want %q: nothing was matched, so nothing is "+
			"owed this delivery and it must be handled as the ordinary message "+
			"it looks like", disposition, AnswerNotMine)
	}

	line := logs.String()
	if !strings.Contains(line, "sandbox_answer_lookup_failed") {
		t.Fatalf("nothing recorded the failed lookup: %q", line)
	}
	for _, want := range []string{`conversation=chat:D1`, `partition=chat:D1:root-1`} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not carry %s: %q", want, line)
		}
	}
}

// A RESUME THAT BROKE LEAVES THE QUESTION OPEN, not the seat held.
//
// The claim takes a parked run out of the waiting set and gives its seat back
// to the resume; a resume that fails puts the ROW back where it was —
// [StatusAwaiting], still waiting for a person — so the counts have to go back
// there too. Counted onto the seat instead, the run waited for an answer that
// every delivery from then on was parked behind, for as long as this node kept
// the seat: the person answered twice and neither reply reached anything.
func TestAFailedResumeLeavesTheQuestionOpenRatherThanTheSeatHeld(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	rig.resumer.err = errors.New("the model never answered")
	if _, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", nil); err == nil {
		t.Fatal("TryResumeFromAnswer reported a resume that never happened")
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the row back where the claim found it", got.Status)
	}
	held, awaits := rig.coordinator.SeatRuns("swe")
	if held || !awaits {
		t.Fatalf("SeatRuns = held %v / awaiting %v, want the question open again "+
			"on a free seat — the person's next answer is the only thing that "+
			"moves this run", held, awaits)
	}
}

// ---------------------------------------------------------------------
// what the offer tells the dispatcher to do with the delivery
// ---------------------------------------------------------------------

// parkOnAQuestion drives one run to the state every case below starts from:
// the job asked a person something, the run is [StatusAwaiting], and the seat
// is free with a question open on it.
func parkOnAQuestion(t *testing.T, rig *coordRig) {
	t.Helper()
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run parked on its question", got.Status)
	}
}

// answerFrom is one delivery carrying a person's reply, with an id of its own
// so the requeue budget can tell two messages apart.
func answerFrom(text string) *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: text,
	}, events.TraceContext{})
}

// answerBudgetsFor is how many deliveries of one run this node holds a budget
// for.
func (c *Coordinator) answerBudgetsFor(handle, turnID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.attempts[answerKey{handle: handle, turnID: turnID}])
}

// answerAttemptsFor is the failed-handoff count this node holds across every
// delivery of one run.
func (c *Coordinator) answerAttemptsFor(handle, turnID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, budget := range c.attempts[answerKey{handle: handle, turnID: turnID}] {
		total += budget.failures
	}
	return total
}

// A DELIVERY ANOTHER INBOUND ALREADY CLAIMED IS SPENT, NOT RUN.
//
// Two replies arriving together on one conversation both match the one parked
// run, and only one can claim it. The loser must not be handed back to the
// ordinary route: the run IS being resumed, by the winner, and a turn on this
// copy would answer a question that is already being answered.
func TestAnAnswerAnotherInboundAlreadyClaimedIsSpent(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	// THE RACE ITSELF: the lookup finds the run awaiting and the claim
	// then loses the flip, which is what the winner taking it between the
	// two reads looks like from here.
	rig.coordinator.pending = lostClaimStore{PendingStore: rig.pending}

	disposition, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", answerFrom("use main"))
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if disposition != AnswerConsumed {
		t.Fatalf("disposition = %q, want %q: the run this delivery answers is "+
			"already being resumed by the inbound that won the claim",
			disposition, AnswerConsumed)
	}
}

// lostClaimStore answers every claim with "somebody else won it", while its
// lookup still matches: the race the at-most-once gate exists for.
type lostClaimStore struct{ PendingStore }

func (lostClaimStore) ClaimForResume(context.Context, string, Tail) (PendingRun, bool, error) {
	return PendingRun{}, false, nil
}

// A CLAIM THE STORE COULD NOT WRITE REQUEUES THE ANSWER.
//
// The ambiguous one, and the reason the ambiguity is resolved towards the run:
// a store that could not say whether the flip landed is not a store that said
// no. Reported as "not the answer" — which is what an error used to mean at
// the dispatcher — the person's reply went on to be consumed as an unrelated
// turn while the run it was written for waited for a further message.
func TestAClaimTheStoreCouldNotWriteRequeuesTheAnswer(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.pending = &refusingStore{
		inner: rig.pending, refuse: []string{"ClaimForResume"},
	}

	disposition, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", answerFrom("use main"))
	if err == nil {
		t.Fatal("a claim that could not be written reported success")
	}
	if disposition != AnswerDeferred {
		t.Fatalf("disposition = %q, want %q: the claim may or may not have "+
			"landed, and an answer that arrives twice is recoverable where one "+
			"that is spent is not", disposition, AnswerDeferred)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run still waiting for this answer", got.Status)
	}
}

// A RUN THAT IS TERMINALLY GONE LETS ITS ANSWER BE AN ORDINARY MESSAGE.
//
// The other half of the classification, and the half a requeue would turn into
// a loop with no end: these runs have been settled, announced and deleted, so
// nothing is coming back for the delivery and nothing is owed it. Acking it
// would be worse still — the person's message would be swallowed on behalf of
// a turn that no longer exists.
func TestAnAnswerForATerminallyGoneRunBecomesAnOrdinaryMessage(t *testing.T) {
	for name, arrange := range map[string]func(*coordRig){
		// The claim was taken and could NOT be given back, so the run was
		// settled in its place rather than stranded in the claim.
		"a claim that could not be given back": func(rig *coordRig) {
			rig.resumer.err = errors.New("the model never answered")
			rig.coordinator.pending = &refusingStore{
				inner: rig.pending, refuse: []string{"ReleaseClaim"},
			}
		},
		// A row from a build that predates the launching state: claimable,
		// with nothing to resume into, so the run is failed.
		"a row with no suspended conversation": func(rig *coordRig) {
			rig.coordinator.pending = statelessStore{PendingStore: rig.pending}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCoordRig(t)
			parkOnAQuestion(t, rig)
			arrange(rig)

			disposition, err := rig.coordinator.TryResumeFromAnswer(
				t.Context(), "swe", answerOnTheDM, "use main", answerFrom("use main"))
			if err != nil {
				t.Fatalf("TryResumeFromAnswer = %v, want no error: there is "+
					"nothing left to come back to", err)
			}
			if disposition != AnswerNotMine {
				t.Fatalf("disposition = %q, want %q: the run has been ended and "+
					"announced, so this is an ordinary message now",
					disposition, AnswerNotMine)
			}
			if failed := rig.failures(); len(failed) != 1 {
				t.Fatalf("announced %+v, want the one lost turn", failed)
			}
		})
	}
}

// statelessStore hands back a claimed run with no suspended conversation on
// it, which is what a row written before [StatusLaunching] existed looks like
// to this build.
type statelessStore struct{ PendingStore }

func (s statelessStore) ClaimForResume(ctx context.Context, turnID string, tail Tail) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail)
	run.ExecuteState = nil
	return run, won, err
}

// THE HAND-BACK IS BOUNDED, and then the message is let go.
//
// Nothing outside this package bounds the loop: a run parked on a question
// stays matchable for ever, since the pause reaper only moves it to reseed, so
// a resume that fails the same way every time would circle the seat's inbox
// for the life of the process. The budget is [MaxAnswerAttempts], and what
// happens at the end of it is the ordinary route: the run is left parked
// rather than destroyed, because every failure that gets here is a statement
// about THIS node.
func TestTheRequeueOfAnAnswerIsBounded(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")
	delivery := answerFrom("use main")

	// Every attempt but the LAST asks for the delivery back.
	for attempt := 1; attempt < MaxAnswerAttempts; attempt++ {
		disposition, err := rig.coordinator.TryResumeFromAnswer(
			t.Context(), "swe", answerOnTheDM, "use main", delivery)
		if err == nil {
			t.Fatalf("attempt %d: the resume reported success", attempt)
		}
		if disposition != AnswerDeferred {
			t.Fatalf("attempt %d of %d: disposition = %q, want %q — the run is "+
				"still owed this answer", attempt, MaxAnswerAttempts,
				disposition, AnswerDeferred)
		}
	}

	disposition, _ := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", delivery)
	if disposition != AnswerNotMine {
		t.Fatalf("disposition = %q on attempt %d, want %q: a resume that fails "+
			"for ever must not requeue for ever",
			disposition, MaxAnswerAttempts, AnswerNotMine)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run still parked: a spent budget is this "+
			"node giving up on one message, not the turn being destroyed", got.Status)
	}
}

// AND EVERY MESSAGE GETS ITS OWN BUDGET.
//
// The count is per DELIVERY, not per run, because a person sending a second
// reply is a fresh attempt at the same question rather than another go at the
// same answer. Carried over, the last message's failures would spend the next
// one's chances — and a run whose node was briefly unable to resume it would
// answer nobody ever again.
func TestASecondMessageGetsItsOwnRequeueBudget(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")

	first := answerFrom("use main")
	var last AnswerDisposition
	for attempt := 1; attempt <= MaxAnswerAttempts; attempt++ {
		last, _ = rig.coordinator.TryResumeFromAnswer(
			t.Context(), "swe", answerOnTheDM, "use main", first)
	}
	if last != AnswerNotMine {
		t.Fatalf("the first message's budget did not run out (%q)", last)
	}
	// AND STAYS SPENT: a second copy of the same message does not buy the
	// same delivery a second budget.
	if disposition, _ := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", first); disposition != AnswerNotMine {
		t.Fatalf("a further copy of a spent message was requeued again (%q)", disposition)
	}

	disposition, _ := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "or the release branch",
		answerFrom("or the release branch"))
	if disposition != AnswerDeferred {
		t.Fatalf("disposition = %q, want %q: a different message is a new attempt "+
			"at the question and starts its own budget", disposition, AnswerDeferred)
	}
}

// AND TWO MESSAGES CIRCLING ONE RUN DO NOT RESET EACH OTHER.
//
// THE CASE THAT PROVES THE BOUND EXISTS. A budget per RUN — one slot holding
// the delivery it was counting — reset itself the moment a different message
// arrived, so two replies alternating on one parked run replaced each other's
// slot on every pass and neither ever reached [MaxAnswerAttempts]: both kept
// answering deferred, both kept coming back, and the loop the bound exists to
// end was reachable with two messages. A budget per (run, delivery) is the
// only shape that counts what the doc always claimed it counted.
func TestTwoMessagesCirclingOneRunBothRunOutOfBudget(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")

	first, second := answerFrom("use main"), answerFrom("or the release branch")
	offer := func(delivery *events.Event) AnswerDisposition {
		t.Helper()
		disposition, _ := rig.coordinator.TryResumeFromAnswer(
			t.Context(), "swe", answerOnTheDM, "use main", delivery)
		return disposition
	}

	// INTERLEAVED, which is how they arrive: each message is offered, fails,
	// is handed back and meets the other one on its way round.
	for attempt := 1; attempt < MaxAnswerAttempts; attempt++ {
		for name, delivery := range map[string]*events.Event{
			"first": first, "second": second,
		} {
			if got := offer(delivery); got != AnswerDeferred {
				t.Fatalf("the %s message on attempt %d of %d: disposition = %q, "+
					"want %q — its own budget is not spent yet",
					name, attempt, MaxAnswerAttempts, got, AnswerDeferred)
			}
		}
	}
	for name, delivery := range map[string]*events.Event{
		"first": first, "second": second,
	} {
		if got := offer(delivery); got != AnswerNotMine {
			t.Fatalf("the %s message on attempt %d: disposition = %q, want %q — "+
				"a second message must not buy the first one a fresh budget",
				name, MaxAnswerAttempts, got, AnswerNotMine)
		}
	}
}

// AND ONE RUN HOLDS ONLY SO MANY BUDGETS AT ONCE.
//
// The other half of "per (run, delivery)": a budget each means a table that
// grows with the messages a conversation carries, for as long as the run
// lives. [maxAnswerDeliveries] bounds it — and bounds it by REFUSING a new
// budget rather than by evicting a live one, because budgets evicting each
// other is the same unreachable bound the per-run slot had, at N messages
// instead of two. A SPENT budget is different and does make room: its delivery
// has already been let go to the ordinary route.
func TestOneRunHoldsABoundedNumberOfAnswerBudgets(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")
	offer := func(delivery *events.Event) AnswerDisposition {
		t.Helper()
		disposition, _ := rig.coordinator.TryResumeFromAnswer(
			t.Context(), "swe", answerOnTheDM, "use main", delivery)
		return disposition
	}

	live := make([]*events.Event, maxAnswerDeliveries)
	for i := range live {
		live[i] = answerFrom(fmt.Sprintf("reply %d", i))
		if got := offer(live[i]); got != AnswerDeferred {
			t.Fatalf("message %d of the run's %d budgets: disposition = %q, want %q",
				i, maxAnswerDeliveries, got, AnswerDeferred)
		}
	}

	// EVERY SLOT HOLDS A LIVE BUDGET, so this one is handled as the ordinary
	// message it looks like rather than given a budget that would have to
	// come out of somebody else's.
	crowd := answerFrom("and another thing")
	if got := offer(crowd); got != AnswerNotMine {
		t.Fatalf("disposition = %q, want %q: a run with %d messages already in "+
			"hand-back must not open an unbounded number more",
			got, AnswerNotMine, maxAnswerDeliveries)
	}
	if held := rig.coordinator.answerBudgetsFor("swe", "t1"); held != maxAnswerDeliveries {
		t.Fatalf("the run holds %d budgets, want at most %d", held, maxAnswerDeliveries)
	}

	// AND A SPENT ONE MAKES ROOM: the first message runs out its attempts,
	// which frees the slot the next one takes.
	for attempt := 1; attempt < MaxAnswerAttempts; attempt++ {
		offer(live[0])
	}
	if got := offer(live[0]); got != AnswerNotMine {
		t.Fatalf("the first message's own budget did not run out (%q)", got)
	}
	if got := offer(crowd); got != AnswerDeferred {
		t.Fatalf("disposition = %q, want %q: a spent budget leaves a slot for a "+
			"message that has not had one", got, AnswerDeferred)
	}
}

// AND THE BOUND IS A TOLERANCE, NOT ONLY A COUNT.
//
// The second clause: this node hands a message back for at most the run's own
// awaiting window — pause_ttl_seconds, the same number the pause reaper
// enforces on its box. A count on its own said nothing about how long the
// attempts covered, which is exactly what was wrong with spending 25 of them
// in milliseconds. Here the attempts are spread by the queue's own backoff
// (see [Dispatcher.answered]) and the window is what ends them: the ceiling is
// nowhere near reached and the message is let go all the same, because the
// engine has stopped being willing to wait for it.
func TestTheHandBackOfAnAnswerStopsAtTheRunsAwaitingWindow(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")
	delivery := answerFrom("use main")

	if disposition, _ := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", delivery,
	); disposition != AnswerDeferred {
		t.Fatalf("the first attempt answered %q, want %q", disposition, AnswerDeferred)
	}

	// The attempts are spaced, so time passes between them — and this run's
	// window is DefaultPauseTTL, which is what the rig's box was attached
	// with.
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)

	disposition, _ := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", delivery)
	if disposition != AnswerNotMine {
		t.Fatalf("disposition = %q after the run's whole awaiting window, want "+
			"%q: %d of its %d attempts were still unspent, and a count is not "+
			"a tolerance", disposition, AnswerNotMine, MaxAnswerAttempts-2,
			MaxAnswerAttempts)
	}
}

// AND NOTHING IS CHARGED WHILE ANOTHER RUN HOLDS THE SEAT.
//
// There the delivery is requeued for the SEAT's sake whatever the offer says —
// an immediate republish, at a rate nothing bounds, for as long as that job
// runs — so a charge there would spend the whole budget inside a held run's
// park loop within milliseconds and leave nothing for the attempts that are
// actually spaced: the ones made once the seat is free, which is the state a
// parked run leaves it in.
func TestAHeldSeatsParkSpendsNoAnswerBudget(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	// A SECOND RUN OF THE SAME SEAT, holding it while the first waits for a
	// person: the one shape where both counts are non-zero at once.
	rig.launch("t2")
	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		Agent: "a-1", AgentHandle: "swe", TurnID: "t2",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if held, awaits := rig.coordinator.SeatRuns("swe"); !held || !awaits {
		t.Fatalf("SeatRuns = %v, %v, want a seat both held and awaiting", held, awaits)
	}
	rig.resumer.err = fmt.Errorf("%w: seat %q has no resumer on this node",
		ErrResumeUnavailable, "swe")

	delivery := answerFrom("use main")
	for attempt := 1; attempt <= MaxAnswerAttempts*3; attempt++ {
		disposition, _ := rig.coordinator.TryResumeFromAnswer(
			t.Context(), "swe", answerOnTheDM, "use main", delivery)
		if disposition != AnswerDeferred {
			t.Fatalf("pass %d: disposition = %q, want %q — the park this "+
				"delivery lands on is the held seat's, and it spaces nothing",
				attempt, disposition, AnswerDeferred)
		}
	}
	if counted := rig.coordinator.answerAttemptsFor("swe", "t1"); counted != 0 {
		t.Fatalf("%d attempts were charged to a delivery the seat's own park "+
			"was requeuing anyway", counted)
	}
}

// AND A COUNT NEVER OUTLIVES THE RUN IT IS ABOUT.
//
// The count is this process's own memory, so the one thing it must not do is
// outlive its run: an entry per parked run this node ever failed to resume,
// kept for the life of the process, is a leak. It goes at the two moments no
// further delivery can be offered to that run here — the record being deleted,
// which is the one place a run ends, and the seat being handed on, after which
// the successor gets a clean set of attempts. Neither moment goes back through
// the offer, so neither is covered by what the offer itself clears.
func TestAFinishedRunDropsItsRequeueCount(t *testing.T) {
	for name, finish := range map[string]func(*testing.T, *coordRig){
		// THE RUN ENDS somewhere else entirely: the seat leaves the
		// company while its run is still parked on a question, and the
		// retirement deletes every record it had — which is the one
		// place a run ends and the one place the count can be dropped
		// with it.
		"a run whose record is deleted": func(t *testing.T, rig *coordRig) {
			t.Helper()
			if err := rig.coordinator.RetireSeat(t.Context(), "swe", "node-a:1", 3); err != nil {
				t.Fatalf("RetireSeat: %v", err)
			}
		},
		"a seat handed on": func(_ *testing.T, rig *coordRig) {
			rig.coordinator.ReleaseSeat("swe")
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCoordRig(t)
			parkOnAQuestion(t, rig)
			rig.resumer.err = errors.New("the model never answered")
			if _, err := rig.coordinator.TryResumeFromAnswer(
				t.Context(), "swe", answerOnTheDM, "use main",
				answerFrom("use main")); err == nil {
				t.Fatal("the resume reported success")
			}
			if counted := rig.coordinator.answerAttemptsFor("swe", "t1"); counted != 1 {
				t.Fatalf("the failed handoff was counted %d times, want once", counted)
			}

			finish(t, rig)

			if counted := rig.coordinator.answerAttemptsFor("swe", "t1"); counted != 0 {
				t.Fatalf("%d counted failures were left behind for a run nothing "+
					"will ever offer a delivery to again", counted)
			}
		})
	}
}

// A run whose box was reclaimed must not be told to continue in a working tree
// that is gone — git is the durable state and the brief has to say so.
func TestAnAnswerAfterTheBoxWasReclaimedSaysToReseedFromGit(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	// The reaper expires the pause while the question waits.
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	rig.tick()

	if _, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", answerOnTheDM, "use main", nil); err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Answer, "FRESH machine") {
		t.Fatalf("the brief promises a box that no longer exists: %q", calls[0].Answer)
	}
	if !strings.Contains(calls[0].Answer, "wip/t1") {
		t.Fatalf("the brief does not name the branch holding the work: %q", calls[0].Answer)
	}
}

// A zero TTL means "never hold a blocked box": tear it down the moment the run
// blocks, and re-seed from the branch when the answer comes.
func TestAZeroPauseTtlTearsTheBoxDownTheMomentTheRunBlocks(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{
		SandboxID: run.SandboxID, CodingAgent: "claude-code", PauseTTLSec: 0,
	}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 {
		t.Fatalf("killed %v, want the box reclaimed immediately", killed)
	}
	if got := rig.get("t1"); got.Status != StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, StatusReseed)
	}
}

// The question reaches the engine's audited announcement path; the sandbox
// never posts anything itself.
func TestAParkedQuestionIsAnnounced(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "team"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	var found *types.SandboxClarificationRequested
	for _, p := range rig.queue.published {
		if ask, ok := p.event.Data.(*types.SandboxClarificationRequested); ok {
			found = ask
		}
	}
	if found == nil {
		t.Fatal("no clarification was announced")
	}
	if found.Question != "which branch?" || found.Audience != "team" {
		t.Fatalf("announcement = %+v", found)
	}
	// THE ANNOUNCEMENT NAMES THE CONVERSATION — the same value the answer
	// is matched on, and the same one the resume reports back through.
	// This event is read for display, and the durable thread is what a
	// person reading the feed means by the run's conversation; the
	// partition it was launched from is the row's business, not theirs.
	if found.ConversationKey != "chat:D1" {
		t.Fatalf("the announcement names %q, want the conversation the run reports to",
			found.ConversationKey)
	}
}

// A question with a credential in it becomes an announcement anyone can read.
func TestAnAnnouncedQuestionIsRedacted(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	secret := "glpat-" + strings.Repeat("e", 20)
	rig.runner.Finish(Result{
		NeedsInput: true, AskTo: "requester",
		Question: "is " + secret + " the right token?",
	})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	for _, p := range rig.queue.published {
		if ask, ok := p.event.Data.(*types.SandboxClarificationRequested); ok {
			if strings.Contains(ask.Question, secret) {
				t.Fatalf("a credential was announced: %q", ask.Question)
			}
			return
		}
	}
	t.Fatal("no clarification was announced")
}

// The findings become a tool message published as a phase record.
func TestTheResumeAnswerIsRedacted(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	secret := "ghp_" + strings.Repeat("b", 36)
	rig.runner.Finish(Result{Success: true, Text: "cloned with " + secret})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times", len(calls))
	}
	if strings.Contains(calls[0].Answer, secret) {
		t.Fatalf("a credential reached the resumed conversation: %q", calls[0].Answer)
	}
}

// An unreadable store must not swallow an ordinary message.
func TestAnUnreadableAnswerLookupFallsThroughToNormalHandling(t *testing.T) {
	rig := newCoordRig(t)
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: brokenStore{}, Manager: rig.manager, Resume: rig.resumer,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	disposition, err := coordinator.TryResumeFromAnswer(t.Context(), "swe", answerOnTheDM, "hello", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if disposition != AnswerNotMine {
		t.Fatalf("an unreadable store answered %q rather than %q, and swallowed "+
			"an ordinary message", disposition, AnswerNotMine)
	}
}

// brokenStore fails every read. Embedded so it satisfies the interface while
// overriding only what the test exercises.
type brokenStore struct{ PendingStore }

func (brokenStore) FindAwaitingByConversation(context.Context, string, ConversationRef) (PendingRun, bool, error) {
	return PendingRun{}, false, fmt.Errorf("store unreachable")
}

// ---------------------------------------------------------------------
// restart recovery
// ---------------------------------------------------------------------

// The waiter then drives the recovered job to completion.
func TestClaimingASeatReParksItsRunningJobs(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-a:1", 7); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if !rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("a running job did not re-park its seat")
	}
	if got := rig.get("t1"); got.Owner != "node-a:1" || got.OwnerEpoch != 7 {
		t.Fatalf("ownership = %q/%d, want the claiming node's", got.Owner, got.OwnerEpoch)
	}
}

// Nothing will ever pick up a resumed row — the at-most-once claim already
// flipped, so a redelivered completion is refused — and its box sits paused.
func TestClaimingASeatReapsATailTheDeadOwnerAbandoned(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", CompletionTail(run.LaunchID)); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the abandoned box reclaimed", killed)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat was parked on a tail nothing will ever finish")
	}
}

// A person's answer can arrive days later; the run waits for it.
func TestClaimingASeatLeavesAParkedRunForItsAnswer(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	if err := rig.pending.MarkAwaiting(t.Context(), "t1", Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-a:1", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want it left waiting", got.Status)
	}
	held, awaits := rig.coordinator.SeatRuns("swe")
	if held {
		t.Fatal("a seat waiting on a person cannot receive their answer while parked")
	}
	// AND THE NEW OWNER INHERITS THE OPEN QUESTION. The count went with the
	// node that parked the run; this is the only place the successor can
	// learn of it, and without it the answer lands on a seat that believes
	// nothing is waiting and is run as an unrelated turn.
	if !awaits {
		t.Fatal("the seat's new owner did not inherit the question the run is waiting on")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("recovery killed %v out from under a waiting run", killed)
	}
}

// A detached run belongs to its row, not to this process. Reaping the box on
// release would destroy work the successor is about to resume.
func TestReleasingASeatTearsNothingDown(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)

	rig.coordinator.ReleaseSeat("swe")

	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("a released seat is still tracked here")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("releasing a seat killed %v", killed)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.SandboxID != run.SandboxID {
		t.Fatalf("the row was disturbed: %+v", got)
	}
}

// EVERY COLLABORATOR IS REQUIRED, and a missing one is refused by name. The
// resumer is among them: every node that builds a coordinator runs the engine a
// completion resumes into, and "not on this node" is the resumer's answer.
func TestACoordinatorNeedsItsCollaborators(t *testing.T) {
	_, err := NewCoordinator(CoordinatorOptions{})
	if err == nil {
		t.Fatal("a coordinator with no queue, store, manager or resumer was accepted")
	}
	for _, field := range []string{"Queue", "Pending", "Manager", "Resume"} {
		if !strings.Contains(err.Error(), "CoordinatorOptions."+field) {
			t.Errorf("the refusal does not name CoordinatorOptions.%s: %v", field, err)
		}
	}

	rig := newCoordRig(t)
	if _, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
	}); err == nil || !strings.Contains(err.Error(), "CoordinatorOptions.Resume") {
		t.Fatalf("a coordinator with no resumer = %v, want it refused naming Resume", err)
	}
}

// A BOX IS RECLAIMED EVEN WHEN THE CALLER'S CONTEXT IS ALREADY DEAD.
//
// teardown is reached from settleFailed and resumeAndSettle — right after a
// failure — and from the queue handler a drain cancels, so the context that
// got here is very often the cancellation being undone. Inheriting it makes
// both calls no-ops in two different bad ways: on a remote provider the box
// runs out its TTL, billed, with the row's record of it already cleared so
// nothing will ever collect it; on the local one Kill's wait for the process
// group is what stops removeBox racing the dying wrapper's writes.
//
// The two siblings — abandon() and Manager.discard() — already detach, with
// comments saying why. This one did not.
func TestAFailedRunsBoxIsReclaimedOnADeadContext(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	if run.SandboxID == "" {
		t.Fatal("the fixture launched no box to reclaim")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rig.coordinator.settleFailed(ctx, run, types.SandboxFailureCollect,
		"the box could not be read back")

	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Fatalf("killed %v, want the failed run's box %q: it will run to its "+
			"TTL with nothing left to collect it", killed, run.SandboxID)
	}
	// And the run's record is gone, so nothing later thinks there is still a
	// box to talk to. The delete ran on the same detached context as the
	// kill: on the dead one it would have been a no-op.
	rig.finished("t1")
}

// ---------------------------------------------------------------------
// ending a run
// ---------------------------------------------------------------------

// A SUSPENSION WITH NOWHERE TO GO IS SETTLED, NOT MARKED. The job is already
// executing in its box, and a record merely marked failed is read by no
// recovery pass and polled by no waiter: the box ran to its provider's TTL,
// billed, with nothing left to reclaim it.
func TestFailingARunThatCouldNotSuspendReclaimsItsBox(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launching("t1")
	rig.coordinator.countRun("swe", StatusRunning)

	if err := rig.coordinator.FailRun(t.Context(), "t1",
		types.SandboxFailureSuspensionUnrecorded, "the conversation could not be written"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the running job's box %q reclaimed", killed, run.SandboxID)
	}
	rig.finished("t1")
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat stayed parked on a run that never suspended")
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.SandboxFailureSuspensionUnrecorded {
		t.Fatalf("failures = %+v, want one %q", failed, types.SandboxFailureSuspensionUnrecorded)
	}
}

// A SUSPENSION REPORTED AS UNWRITTEN CAN HAVE LANDED. A store write that
// times out after it committed leaves the run running with its conversation on
// the record, which is an ordinary suspended run the completion poll resumes.
// Only a run still launching is one nothing else will act on, so only that one
// is settled; settling this one would destroy a turn that is fine.
func TestFailingARunWhoseSuspensionLandedLeavesItAlone(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")

	if err := rig.coordinator.FailRun(t.Context(), "t1",
		types.SandboxFailureSuspensionUnrecorded, "the write timed out"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.SandboxID != run.SandboxID {
		t.Fatalf("the suspended run was disturbed: %+v", got)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v: a suspended run's box was reclaimed", killed)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run that is still resumable", failed)
	}
}

// A run that is already gone was settled by somebody else, who reclaimed its
// box and said so.
func TestFailingARunThatIsAlreadyGoneDoesNothing(t *testing.T) {
	rig := newCoordRig(t)
	if err := rig.coordinator.FailRun(t.Context(), "never-launched",
		types.SandboxFailureSuspensionUnrecorded, "gone"); err != nil {
		t.Fatalf("FailRun of a missing run: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v for a run that does not exist", killed)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run that does not exist", failed)
	}
}

// THE REMOVED SEAT'S RUNS END WITH ITS MAILBOX. Every one of them, whatever
// it was doing: a resume needs the seat in the company, an answer arrives on
// an inbox that is being deleted, and a completion is routed to a control
// topic that goes with it. Left alone they stayed in the ageless bucket for
// good, a running job's box kept alive by the waiter on every tick.
func TestRetiringASeatEndsEveryRunItHeld(t *testing.T) {
	rig := newCoordRig(t)
	running := rig.launch("running")
	launching := rig.launching("launching")
	parked := rig.launch("parked")
	if err := rig.pending.MarkAwaiting(t.Context(), "parked", Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if err := rig.pending.MarkBoxPaused(t.Context(), "parked", rig.now); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}
	claimed := rig.launch("claimed")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "claimed",
		CompletionTail(claimed.LaunchID)); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	rig.launch("reseed")
	if err := rig.pending.MarkAwaiting(t.Context(), "reseed", Clarification{Question: "still?"}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if won, err := rig.pending.ExpirePause(t.Context(), "reseed"); err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v", won, err)
	}
	// A colleague's run on another seat is none of this retirement's.
	otherBox, err := rig.provider.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "colleague", AgentHandle: "pm", CreatedAt: rig.now,
	}, Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rig.pending.AttachSandbox(t.Context(), "colleague",
		BoxRef{SandboxID: otherBox.ID()}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.coordinator.countRun("swe", StatusRunning)

	if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12); err != nil {
		t.Fatalf("RetireSeat: %v", err)
	}
	for _, id := range []string{"running", "launching", "parked", "claimed", "reseed"} {
		rig.finished(id)
	}
	killed := rig.provider.KilledIDs()
	for _, box := range []string{running.SandboxID, launching.SandboxID, parked.SandboxID, claimed.SandboxID} {
		if !slices.Contains(killed, box) {
			t.Errorf("killed %v, missing box %q", killed, box)
		}
	}
	if slices.Contains(killed, otherBox.ID()) {
		t.Fatalf("retiring one seat killed another seat's box %q", otherBox.ID())
	}
	if got := rig.get("colleague"); got.AgentHandle != "pm" {
		t.Fatalf("another seat's run was disturbed: %+v", got)
	}
	failed := rig.failures()
	if len(failed) != 5 {
		t.Fatalf("announced %d lost runs, want one for each of the seat's five", len(failed))
	}
	for _, f := range failed {
		if f.Reason != types.SandboxFailureSeatRemoved || f.AgentHandle != "swe" {
			t.Errorf("announcement %+v, want reason %q for seat swe", f, types.SandboxFailureSeatRemoved)
		}
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the retired seat is still tracked as busy")
	}
}

// The retirement holds the seat's lease for a budget, and the lease is the
// only thing keeping a returning seat's owner off these records. Work its
// context no longer covers is reported rather than done, so the retirement is
// retried instead of deleting records outside the lease that protects them.
func TestRetiringASeatStopsWhenItsBudgetEnds(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := rig.coordinator.RetireSeat(ctx, "swe", "retirement:1", 12); err == nil {
		t.Fatal("a retirement past its budget reported every run ended")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q: a run was ended outside the retirement's lease", got.Status)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("killed %v outside the retirement's lease", killed)
	}
}

// A run a newer lease has claimed belongs to that lease's holder, and neither
// a recovery nor a retirement under an older one may kill its box.
func TestEndingARunNeverReachesANewerLeasesBox(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launching("t1")
	if won, err := rig.pending.ClaimOwnership(t.Context(), "t1", "node-c:1", 20); err != nil || !won {
		t.Fatalf("ClaimOwnership = %v, %v", won, err)
	}

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12); err == nil {
		t.Fatal("a retirement under an older lease reported the newer lease's run ended")
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, run.SandboxID) {
		t.Fatalf("killed %v: an older lease reclaimed the box of a run a newer one owns", killed)
	}
	if got := rig.get("t1"); got.Owner != "node-c:1" {
		t.Fatalf("the newer lease's run was disturbed: %+v", got)
	}
	// Nor is it announced as lost: the newer owner is continuing it, and a
	// sandbox_run_failed naming an abandoned tail would tell the board and
	// the seat a turn died that did not.
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run a newer lease owns", failed)
	}
}

// A BOX THAT COULD NOT BE RECLAIMED KEEPS ITS RECORD. The retirement is
// retried on the next tick, and only the record tells that retry the box
// exists: deleted anyway, the box would be billed with nothing naming it. The
// retry that can reach the provider then ends the run once.
func TestRetiringASeatKeepsARunWhoseBoxCouldNotBeReclaimed(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.provider.KillErr = errors.New("the provider is unreachable")

	if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12); err == nil {
		t.Fatal("a retirement that could not reclaim a box reported the run ended")
	}
	if got := rig.get("t1"); got.SandboxID != run.SandboxID {
		t.Fatalf("the record no longer names box %q: %+v", run.SandboxID, got)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run that was not ended", failed)
	}

	rig.provider.KillErr = nil
	if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 12); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	rig.finished("t1")
	if failed := rig.failures(); len(failed) != 1 || failed[0].Reason != types.SandboxFailureSeatRemoved {
		t.Fatalf("failures = %+v, want the run announced once as %q", failed, types.SandboxFailureSeatRemoved)
	}
}

// THE BOX GOES BEFORE THE RECORD. A record that outlives its box is reaped by
// the seat's next recovery, and a kill of a box that is gone costs nothing; a
// box that outlives its record is named by nothing and billed until its
// provider's TTL. So every ending kills first and deletes second, which only
// the store can observe at the moment of the delete.
func TestARunsBoxIsReclaimedBeforeItsRecordIsDeleted(t *testing.T) {
	rig := newCoordRig(t)
	run := rig.launch("t1")
	witness := &finishWitness{PendingStore: rig.pending, provider: rig.provider, box: run.SandboxID}
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: witness, Manager: rig.manager, Resume: rig.resumer,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if !witness.finished {
		t.Fatal("the settled run was never finished")
	}
	if !witness.killedFirst {
		t.Fatal("the run's record was deleted while its box was still running: a crash there " +
			"leaves a box nothing names")
	}
}

// finishWitness records whether a run's box had been killed by the time its
// record was deleted.
type finishWitness struct {
	PendingStore
	provider              *FakeProvider
	box                   string
	finished, killedFirst bool
}

func (w *finishWitness) Finish(ctx context.Context, turnID string, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	w.finished = true
	w.killedFirst = slices.Contains(w.provider.KilledIDs(), w.box)
	return w.PendingStore.Finish(ctx, turnID, fence, whileIn)
}

// EVERY ANNOUNCEMENT CARRIES THE UNIT OF WORK, and a run parked before
// ADR-0017 carries it in its turn id.
//
// A coding run is detached: the row is written by one build and read, minutes
// or days later and possibly on another node, by whatever is running then.
// Nothing rewrites a parked row, so a run suspended before the split has no
// WorkKey field at all and its TurnID IS the work key — which is why
// [PendingRun.UnitOfWork] exists and why no publisher may reach for the raw
// field. Reaching for it announced an empty unit of work for exactly the runs
// that outlived the upgrade, and the completion, the question and the failure
// are the three gestures a resumed turn's identity travels on.
func TestAnAnnouncementCarriesTheUnitOfWorkOfAPreSplitRun(t *testing.T) {
	// A DERIVED-SHAPED TURN ID: 32 lowercase hex, which is what
	// workkey.Derive produces and what a pre-split build put in TurnID.
	const preSplit = "0123456789abcdef0123456789abcdef"

	t.Run("clarification", func(t *testing.T) {
		rig := newCoordRig(t)
		rig.launch(preSplit)
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{
			NeedsInput: true, Question: "which branch?", AskTo: "requester",
		})
		payload, ev := rig.completion(preSplit)
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		asked := rig.questions()
		if len(asked) != 1 {
			t.Fatalf("%d questions announced, want one", len(asked))
		}
		if asked[0].WorkKey != preSplit {
			t.Errorf("WorkKey = %q, want the pre-split run's unit of work %q",
				asked[0].WorkKey, preSplit)
		}
	})

	t.Run("failure", func(t *testing.T) {
		rig := newCoordRig(t)
		rig.launching(preSplit)
		if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-2", 7); err != nil {
			t.Fatalf("RecoverSeat: %v", err)
		}
		failed := rig.failures()
		if len(failed) != 1 {
			t.Fatalf("%d failures announced, want one", len(failed))
		}
		if failed[0].WorkKey != preSplit {
			t.Errorf("WorkKey = %q, want the pre-split run's unit of work %q",
				failed[0].WorkKey, preSplit)
		}
	})
}
