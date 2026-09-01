package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// resumeSpy records re-entries and can be made to fail.
type resumeSpy struct {
	mu       sync.Mutex
	requests []ResumeRequest
	err      error

	// relaunch makes the resumed Execute call run_sandbox again, which is
	// the box-reuse branch.
	relaunch func(ctx context.Context, run PendingRun)
}

func (s *resumeSpy) Resume(ctx context.Context, req ResumeRequest) error {
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.requests = append(s.requests, req)
	relaunch := s.relaunch
	s.mu.Unlock()
	if relaunch != nil {
		relaunch(ctx, req.Run)
	}
	return nil
}

func (s *resumeSpy) calls() []ResumeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResumeRequest(nil), s.requests...)
}

// ledgerSpy records post-charges.
type ledgerSpy struct {
	mu      sync.Mutex
	charged int
	over    bool
	err     error
}

func (l *ledgerSpy) Charge(_ context.Context, _, _ string, tokens int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

type coordRig struct {
	*waiterRig
	coordinator *Coordinator
	resumer     *resumeSpy
	accountant  *ledgerSpy
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
		Now: func() time.Time { return base.now },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	rig.coordinator = coordinator
	return rig
}

func (r *coordRig) completion(turnID string) (types.SandboxRunCompleted, *events.Event) {
	run := r.get(turnID)
	payload := types.SandboxRunCompleted{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, SandboxID: run.SandboxID, CodingAgent: run.CodingAgent,
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

	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat was busy before the start event arrived")
	}
	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		AgentHandle: "swe", TurnID: "t1",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if !rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat is not parked on its detached run")
	}
	if got := rig.coordinator.Busy(); len(got) != 1 || got[0] != "swe" {
		t.Fatalf("Busy = %v, want [swe]", got)
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
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat stayed parked after its only run settled")
	}
}

// A person can take days to answer, and the answer arrives on the seat's own
// inbox — which a parked seat would never read.
func TestAParkedClarificationFreesTheSeat(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.markBusy("swe")
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		DeliveredRefs: []string{"wip/t1"},
	})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("a seat waiting on a person's answer cannot receive it while parked")
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want %q", got.Status, StatusAwaiting)
	}
	if got.Question != "which branch?" || got.Branch != "wip/t1" {
		t.Fatalf("the question and its branch were not recorded: %+v", got)
	}
}

// ---------------------------------------------------------------------
// the at-most-once claim
// ---------------------------------------------------------------------

func TestACompletionResumesTheSuspendedLoop(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.markBusy("swe")
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
	if got := rig.get("t1"); got.Status != StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, StatusDone)
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := rig.coordinator.OnCompleted(context.Background(), payload, ev); err != nil {
				t.Errorf("OnCompleted: %v", err)
			}
		}()
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
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = errors.New("the node lost the seat mid-resume")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was reported as success — the completion would be acked")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want it reverted to %q so the retry can re-claim", got.Status, StatusRunning)
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

	handled, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", "chat:C1", "use main", nil)
	if err == nil {
		t.Fatal("a failed resume reported success")
	}
	if !handled {
		t.Fatal("the answer was not reported as handled")
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want it back at %q", got.Status, StatusAwaiting)
	}
}

// A node with no seat to resume into must send the completion back rather than
// settle the run and tear the box down with the turn inside it.
func TestANodeThatCannotResumeSaysSoRatherThanSettling(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})

	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	payload, ev := rig.completion("t1")
	err = coordinator.OnCompleted(t.Context(), payload, ev)
	if !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("OnCompleted = %v, want ErrResumeUnavailable", err)
	}
	if got := rig.get("t1"); got.Status == StatusDone {
		t.Fatal("the run was settled done by a node that never resumed it")
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
	rig.coordinator.markBusy("swe")
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, StatusFailed)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	rig.coordinator.markBusy("swe")
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
	if !rig.coordinator.AwaitingSandbox("swe") {
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
	if got := rig.get("t1"); got.Status != StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, StatusDone)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the box reclaimed once the turn was done", killed)
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
	if got := rig.get("t1"); got.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, StatusFailed)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the abandoned launch's box reclaimed", killed)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
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
	if got := rig.get("t1"); got.SandboxID != "" {
		t.Fatalf("the row still names a box that is gone: %q", got.SandboxID)
	}
}

// A collect that fails means the job is over regardless: the seat must not be
// left parked on a run that finished.
func TestAFailedCollectStillFreesTheSeat(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.markBusy("swe")
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errors.New("the box died mid-read")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat stayed parked after a failed collect")
	}
	if got := rig.get("t1"); got.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, StatusFailed)
	}
}

// ---------------------------------------------------------------------
// accounting
// ---------------------------------------------------------------------

// A refusal cannot un-spend a collected run, so the charge is recorded whatever
// the cap says — that is the only way the meter stays true when it is binding.
func TestACollectedRunIsChargedEvenOverBudget(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.accountant.over = true
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 4000, OutputTokens: 1000})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.accountant.total(); got != 5000 {
		t.Fatalf("charged %d, want 5000", got)
	}
	if len(rig.resumer.calls()) != 1 {
		t.Fatal("an over-budget charge stopped the turn from continuing")
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

	handled, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", "chat:C1", "use main", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if !handled {
		t.Fatal("the answer was not matched to the run that asked")
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
		t.Context(), "swe", "chat:C1", "use main", nil); err != nil {
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
	if found.ConversationKey != "chat:C1" {
		t.Fatalf("the answer's conversation was not carried: %q", found.ConversationKey)
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
	handled, err := coordinator.TryResumeFromAnswer(t.Context(), "swe", "chat:C1", "hello", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if handled {
		t.Fatal("an unreadable store swallowed an ordinary message")
	}
}

// brokenStore fails every read. Embedded so it satisfies the interface while
// overriding only what the test exercises.
type brokenStore struct{ PendingStore }

func (brokenStore) FindAwaitingByConversation(context.Context, string, string) (PendingRun, bool, error) {
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
	if !rig.coordinator.AwaitingSandbox("swe") {
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
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1"); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}

	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusFailed {
		t.Fatalf("status = %q, want the abandoned tail failed", got.Status)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the abandoned box reclaimed", killed)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
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
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("a seat waiting on a person cannot receive their answer while parked")
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
	rig.coordinator.markBusy("swe")

	rig.coordinator.ReleaseSeat("swe")

	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("a released seat is still tracked here")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Fatalf("releasing a seat killed %v", killed)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.SandboxID != run.SandboxID {
		t.Fatalf("the row was disturbed: %+v", got)
	}
}

func TestACoordinatorNeedsItsCollaborators(t *testing.T) {
	if _, err := NewCoordinator(CoordinatorOptions{}); err == nil {
		t.Fatal("a coordinator with no queue, store or manager was accepted")
	}
}
