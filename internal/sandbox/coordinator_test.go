package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// resumeSpy records re-entries and can be made to fail.
type resumeSpy struct {
	mu       sync.Mutex
	requests []ResumeRequest
	err      error

	// during runs inside the resumed Execute, before any error is returned:
	// the turn calling run_sandbox again (the box-reuse branch), or an event
	// redelivered to the seat while the turn runs.
	during func(ctx context.Context, run PendingRun)
}

func (s *resumeSpy) Resume(ctx context.Context, req ResumeRequest) error {
	s.mu.Lock()
	err, during := s.err, s.during
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

func (s *resumeSpy) calls() []ResumeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResumeRequest(nil), s.requests...)
}

// ledgerSpy records post-charges.
//
// A refusal moves nothing, which is what the engine's accountant does: it is
// coord.Budgets.Charge underneath, and a charge a cap refuses leaves both
// counters where they were.
type ledgerSpy struct {
	mu      sync.Mutex
	charged int
	calls   int
	refuse  bool
	err     error
}

func (l *ledgerSpy) Charge(_ context.Context, _, _ string, tokens int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return false, l.err
	}
	if l.refuse {
		return true, nil
	}
	l.charged += tokens
	return false, nil
}

func (l *ledgerSpy) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.charged
}

func (l *ledgerSpy) set(refuse bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refuse, l.err = refuse, err
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
	rig.coordinator.markBusy("swe")
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
	if rig.coordinator.AwaitingSandbox("swe") {
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
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeActed)
	rig.coordinator.markBusy("swe")

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
	if rig.coordinator.AwaitingSandbox("swe") {
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
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeActed)
	rig.runner.Finish(Result{Success: true, Text: "first pass"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, run.SandboxID) {
		t.Fatalf("killed %v, want the relaunched job's box %q reclaimed", killed, run.SandboxID)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat stayed parked on a relaunch nothing will ever resume")
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
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim reverted to %q: the NAK'd completion comes back "+
			"to a claim that refuses it, and the suspended conversation is stranded", got.Status, StatusRunning)
	}
	// And the completion, redelivered to a node that can resume, wins.
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
	rig.coordinator.markBusy("swe")
	rig.runner.Finish(Result{Success: true, Text: "done"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.finished("t1")
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
	rig.finished("t1")
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
	coordinator.markBusy("swe")
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errors.New("the box died mid-read")

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if coordinator.AwaitingSandbox("swe") {
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

func (s endedFirst) Finish(ctx context.Context, turnID string, fence Fence) (bool, error) {
	if _, err := s.PendingStore.Finish(ctx, turnID, fence); err != nil {
		return false, err
	}
	return s.PendingStore.Finish(ctx, turnID, fence)
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

func (s staleFind) FindAwaitingByConversation(context.Context, string, string) (PendingRun, bool, error) {
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
	handled, err := coordinator.TryResumeFromAnswer(t.Context(), "swe", "chat:C1", "use main", nil)
	if err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	if !handled {
		t.Fatal("an answer to a question already superseded was run as an unrelated message")
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
	rig.coordinator.markBusy("swe")
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})

	coordinator, err := NewCoordinator(CoordinatorOptions{
		Queue: rig.queue, Pending: &parkFails{PendingStore: rig.pending, left: 1},
		Manager: rig.manager, Resume: rig.resumer, Account: rig.accountant,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	coordinator.markBusy("swe")
	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a question that was never recorded was acked")
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("status = %q, want the claim handed back for the retry", got.Status)
	}
	if !coordinator.AwaitingSandbox("swe") {
		t.Fatal("the seat was freed before its question was on the row")
	}

	if err := coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Question != "which branch?" {
		t.Fatalf("row = %s %q, want the retry to park the question", got.Status, got.Question)
	}
	if coordinator.AwaitingSandbox("swe") {
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
	coordinator.markBusy("swe")

	payload, ev := rig.completion("t1")
	if err := coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	// The relaunch ENDED the run when it could not start, and the hand-back
	// of the PREVIOUS job's claim must not bring it back: a release names the
	// launch it took, and that launch went with the record.
	rig.finished("t1")
	if coordinator.AwaitingSandbox("swe") {
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
	if successor.AwaitingSandbox("swe") {
		t.Fatal("a later owner parked the seat on a run already announced lost")
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
// resumed executor that calls run_sandbox again is charged for that job too.
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
	payload, ev := rig.completion("t1")
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

// ONLY A CHARGE THAT MOVED THE COUNTER IS RECORDED. A refusal and an
// unanswered counter both left it where it was, so the retry offers the spend
// again rather than inheriting an answer about a counter that may have room,
// or be reachable, by then.
func TestAnUnrecordedChargeIsOfferedAgainOnTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool
		err     error
	}{
		{"refused by a cap", true, nil},
		{"the counter did not answer", false, errors.New("counter unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			rig.launch("t1")
			rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 900, OutputTokens: 100})
			rig.accountant.set(tc.refused, tc.err)
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
		})
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
	rig.coordinator.markBusy("swe")

	if err := rig.coordinator.FailRun(t.Context(), "t1",
		types.SandboxFailureSuspensionUnrecorded, "the conversation could not be written"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Fatalf("killed %v, want the running job's box %q reclaimed", killed, run.SandboxID)
	}
	rig.finished("t1")
	if rig.coordinator.AwaitingSandbox("swe") {
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
	rig.coordinator.markBusy("swe")

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
	if rig.coordinator.AwaitingSandbox("swe") {
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

func (w *finishWitness) Finish(ctx context.Context, turnID string, fence Fence) (bool, error) {
	w.finished = true
	w.killedFirst = slices.Contains(w.provider.KilledIDs(), w.box)
	return w.PendingStore.Finish(ctx, turnID, fence)
}
