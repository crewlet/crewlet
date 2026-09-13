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
	err, relaunch := s.err, s.relaunch
	if err == nil {
		s.requests = append(s.requests, req)
	}
	s.mu.Unlock()
	// Before the error, as a real turn does: a resumed executor can call
	// run_sandbox again and break later in the same round.
	if relaunch != nil {
		relaunch(ctx, req.Run)
	}
	return err
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
	rig.finished("t1")
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
	rig.resumer.relaunch = func(ctx context.Context, r PendingRun) {
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
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "claimed"); err != nil || !won {
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
