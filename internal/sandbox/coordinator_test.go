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
	rig.coordinator.markBusy("swe")
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
	if !rig.coordinator.AwaitingSandbox("swe") {
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
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeActed)

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the completion was NAKed, so it will be redelivered into a "+
			"conversation whose writes already landed: %v", err)
	}
	if got := rig.get("t1"); got.Status == StatusRunning {
		t.Fatalf("status = %q, want the claim left taken so no retry wins the flip", got.Status)
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	if got := rig.get("t1"); got.Status != StatusDone || got.SandboxID != "" {
		t.Fatalf("row = %s naming %q, want the turn's run settled with no box", got.Status, got.SandboxID)
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
	if got := rig.get("t1"); got.Status != StatusFailed || got.LaunchID == payload.LaunchID {
		t.Fatalf("row = %s under launch %q, want the relaunch's own failure left standing",
			got.Status, got.LaunchID)
	}
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
	if got := rig.get("t1"); got.Status != StatusFailed {
		t.Fatalf("status = %q, want the reaped run left failed", got.Status)
	}
	if err := successor.RecoverSeat(t.Context(), "swe", "node-c:1", 3); err != nil {
		t.Fatalf("a later recovery: %v", err)
	}
	if successor.AwaitingSandbox("swe") {
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

	handled, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", "chat:C1", "use main", nil)
	if err == nil || !handled {
		t.Fatalf("TryResumeFromAnswer = %v, %v, want the answer handled and sent back", handled, err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("status = %q, want the run waiting on its answer again", got.Status)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
		t.Fatal("a seat whose run went back to waiting on a person was parked")
	}

	rig.resumer.failWith(nil)
	if _, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", "chat:C1", "use main", nil); err != nil {
		t.Fatalf("the answer's retry: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusDone {
		t.Fatalf("status = %q, want the answered run settled", got.Status)
	}
	if rig.coordinator.AwaitingSandbox("swe") {
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	rig.resumer.relaunch = nil
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
	// And the row's record of it is cleared, so nothing later thinks there
	// is still a box to talk to.
	if after := rig.get("t1"); after.SandboxID != "" {
		t.Errorf("the row still names sandbox %q after teardown", after.SandboxID)
	}
}
