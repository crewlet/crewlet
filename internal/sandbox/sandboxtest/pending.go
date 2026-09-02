// Package sandboxtest is the pending-run store's contract suite.
//
// ONE SUITE, BOTH IMPLEMENTATIONS. The properties that matter here — the
// at-most-once tail claim, the epoch fence, the box record's two halves moving
// together — are properties of the STATEMENTS, not of the code around them, so
// a suite that ran only against a fake would assert the author's intent and
// nothing about the store. And a memory twin nobody holds to the contract is a
// twin that models the store wrongly and certifies the bug.
package sandboxtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

var base = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// Run drives every case against one store.
func Run(t *testing.T, newStore func(t *testing.T) sandbox.PendingStore) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, sandbox.PendingStore)
	}{
		{"ASecondLaunchKeepsTheBoxItWillReattachTo", testASecondLaunchKeepsTheBoxItWillReattachTo},
		{"ASecondLaunchDropsTheFirstSuspension", testASecondLaunchDropsTheFirstSuspension},
		{"ASecondLaunchDropsTheFirstRunsBridgedCalls", testASecondLaunchDropsTheFirstRunsBridgedCalls},
		{"ALaunchNeedsATurnID", testALaunchNeedsATurnID},
		{"ALaunchingRunIsNotClaimable", testALaunchingRunIsNotClaimable},
		{"SuspendingOpensTheRunToTheTail", testSuspendingOpensTheRunToTheTail},
		{"OnlyALaunchingRunCanSuspend", testOnlyALaunchingRunCanSuspend},
		{"AttachingABoxClearsTheSnapshotStamp", testAttachingABoxClearsTheSnapshotStamp},
		{"ALaunchingRunIsActive", testALaunchingRunIsActive},
		{"TheTailIsClaimedExactlyOnce", testTheTailIsClaimedExactlyOnce},
		{"AClaimIsExclusiveUnderContention", testAClaimIsExclusiveUnderContention},
		{"AClaimReportsWhereItCameFrom", testAClaimReportsWhereItCameFrom},
		{"AReseedIsStillClaimable", testAReseedIsStillClaimable},
		{"ATerminalRunIsNotClaimable", testATerminalRunIsNotClaimable},
		{"ParkingCarriesTheBranch", testParkingCarriesTheBranch},
		{"OwnershipIsNotStolenByAnOlderLease", testOwnershipIsNotStolenByAnOlderLease},
		{"AStaleFenceCannotWrite", testAStaleFenceCannotWrite},
		{"ReleasingABoxClearsBothHalves", testReleasingABoxClearsBothHalves},
		{"ExecuteStateRoundTrips", testExecuteStateRoundTrips},
		{"ActiveIncludesResumed", testActiveIncludesResumed},
		{"AnAnswerFindsTheRunThatAsked", testAnAnswerFindsTheRunThatAsked},
		{"AnAnswerWithNoConversationMatchesNothing", testAnAnswerWithNoConversationMatchesNothing},
		{"TheReaperSeesOnlyOldSnapshots", testTheReaperSeesOnlyOldSnapshots},
		{"ListingsAreStable", testListingsAreStable},
		{"APauseExpiresExactlyOnce", testAPauseExpiresExactlyOnce},
		{"OnlyAParkedRunCanExpire", testOnlyAParkedRunCanExpire},
		{"AnAnsweredRunCannotBeExpiredUnderTheResume", testAnAnsweredRunCannotBeExpiredUnderTheResume},
		{"ExpiringAPauseClearsTheBoxInTheSameWrite", testExpiringAPauseClearsTheBoxInTheSameWrite},
		{"BridgeCallsAreAppendedInOrder", testBridgeCallsAreAppendedInOrder},
		{"BridgeCallsSurviveWithoutAFence", testBridgeCallsSurviveWithoutAFence},
		{"BridgeCallsForAMissingRunAreDropped", testBridgeCallsForAMissingRunAreDropped},
		{"BridgeCallsDropTheMiddleNotTheStart", testBridgeCallsDropTheMiddleNotTheStart},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
	}
}

func run(turnID string) sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		CodingAgent: "claude-code", TaskDescription: "fix the flake",
		ConversationKey: "slack:C1", Reply: "tool",
		TraceID: "tr-1", CreatedAt: base,
	}
}

// mustBeginLaunch opens a launch and leaves the run where a launch leaves it:
// [sandbox.StatusLaunching], its job started and its conversation not yet
// written.
func mustBeginLaunch(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) {
	t.Helper()
	if err := s.BeginLaunch(context.Background(), r, sandbox.Fence{}); err != nil {
		t.Fatalf("begin launch %s: %v", r.TurnID, err)
	}
}

// mustLaunched carries a run all the way through its launch: the row, then the
// suspension that opens it to the completion poll.
//
// BOTH HALVES, because a run that has only had the first is not one any tail
// acts on — the poll skips it and a claim refuses it — so a case that reached
// for the store's `create` alone would be asserting about a state the rest of
// the engine deliberately ignores.
func mustLaunched(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) {
	t.Helper()
	mustBeginLaunch(t, s, r)
	suspended, err := s.MarkSuspended(context.Background(), r.TurnID, suspension())
	if err != nil {
		t.Fatalf("mark suspended %s: %v", r.TurnID, err)
	}
	if !suspended {
		t.Fatalf("mark suspended %s: the launch did not open to the poll", r.TurnID)
	}
}

// suspension is a stand-in for the serialized Execute conversation.
func suspension() map[string]any {
	return map[string]any{
		"messages":             []any{map[string]any{"role": "assistant", "content": "working"}},
		"pending_tool_call_id": "call_1",
		"pending_tool_name":    "run_sandbox",
		"active_tool_names":    []any{"run_sandbox", "activate_tool"},
		"iteration":            float64(2),
	}
}

func mustGet(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	got, ok, err := s.Get(context.Background(), turnID)
	if err != nil {
		t.Fatalf("get %s: %v", turnID, err)
	}
	if !ok {
		t.Fatalf("no run %s", turnID)
	}
	return got
}

func testASecondLaunchKeepsTheBoxItWillReattachTo(t *testing.T, s sandbox.PendingStore) {
	// A second run_sandbox call in one turn presents the same turn id, and
	// the box on that row is the paused checkout it is about to reattach
	// to. A launch that erased it would re-clone the work instead.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-9", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	mustBeginLaunch(t, s, run("t1"))

	if got := mustGet(t, s, "t1"); got.SandboxID != "box-9" {
		t.Errorf("a second launch erased the attached box: %+v", got)
	}
}

func testASecondLaunchDropsTheFirstSuspension(t *testing.T, s sandbox.PendingStore) {
	// THE RELAUNCH RESET. The previous call's suspended conversation is not
	// this job's, and leaving it in place is worse than absent: a
	// completion claimed before the new suspension lands would splice this
	// run's findings into the loop the LAST call suspended, which has
	// already moved on. The status is what says so — back to launching, so
	// the poll leaves it alone until the new conversation is written.
	mustLaunched(t, s, run("t1"))
	mustBeginLaunch(t, s, run("t1"))

	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusLaunching {
		t.Errorf("status = %q, want %q", got.Status, sandbox.StatusLaunching)
	}
	if len(got.ExecuteState) != 0 {
		t.Errorf("the first call's suspension survived the relaunch: %+v", got.ExecuteState)
	}
}

// A SECOND LAUNCH UNDER ONE TURN ID IS A SECOND ROUND, and its tool log starts
// empty.
//
// The bridged log is the whole record an agent-mode resume rebuilds its phase
// from. A reviewer's self_iterate launches another executor run under the same
// turn id, and the first round's log left in place is not merely stale: a
// round that in fact submitted nothing would report the PREVIOUS round's
// outcome instead of being rescued as incomplete, and the previous round's
// deliveries would satisfy this round's delivery check. The same reset the
// suspension beside it gets, for the same reason.
func testASecondLaunchDropsTheFirstRunsBridgedCalls(t *testing.T, s sandbox.PendingStore) {
	first := run("t-relaunch-bridge")
	mustBeginLaunch(t, s, first)
	if _, err := s.AppendBridgeCall(context.Background(), first.TurnID, sandbox.BridgeCall{
		Name: "submit_work", Args: `{"outcome":"delivered"}`, At: base,
	}); err != nil {
		t.Fatalf("AppendBridgeCall: %v", err)
	}
	if got := mustGet(t, s, first.TurnID); len(got.BridgeCalls) != 1 {
		t.Fatalf("the first round recorded %d calls, want 1", len(got.BridgeCalls))
	}

	mustBeginLaunch(t, s, run("t-relaunch-bridge"))
	got := mustGet(t, s, first.TurnID)
	if len(got.BridgeCalls) != 0 {
		t.Errorf("the second round inherited %d calls from the first: %+v",
			len(got.BridgeCalls), got.BridgeCalls)
	}
	if got.BridgeCallsElided != 0 {
		t.Errorf("the elision count survived the relaunch: %d", got.BridgeCallsElided)
	}
}

func testALaunchNeedsATurnID(t *testing.T, s sandbox.PendingStore) {
	// The turn id is the identity. A row without one collides with every
	// other row that forgot the same field, and nothing could ever find it.
	if err := s.BeginLaunch(context.Background(), run(""), sandbox.Fence{}); err == nil {
		t.Error("a run with no turn id was persisted")
	}
}

func testALaunchingRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	// THE WINDOW THIS STATE EXISTS TO CLOSE. The job is started and can
	// finish at any moment, but the turn has not yet written the
	// conversation a resume re-enters. A completion claimed here would find
	// nothing to resume into and fail the whole turn.
	mustBeginLaunch(t, s, run("t1"))

	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || won {
		t.Fatalf("a launching run was claimed: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusLaunching {
		t.Errorf("a refused claim moved the row to %q", got.Status)
	}
}

func testSuspendingOpensTheRunToTheTail(t *testing.T, s sandbox.PendingStore) {
	// One write, both facts: the conversation lands and the run becomes
	// pollable at the same instant. Two writes would leave a running row
	// with nothing to resume into, which is the state the poll fires on.
	mustBeginLaunch(t, s, run("t1"))
	suspended, err := s.MarkSuspended(context.Background(), "t1", suspension())
	if err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}

	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want %q", got.Status, sandbox.StatusRunning)
	}
	if len(got.ExecuteState) == 0 {
		t.Error("the run opened to the poll with no conversation on it")
	}
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Errorf("a suspended run was not claimable: won=%v err=%v", won, err)
	}
}

func testOnlyALaunchingRunCanSuspend(t *testing.T, s sandbox.PendingStore) {
	// A run whose tail has already been claimed has nowhere to put a
	// suspension, and re-arming it would hand a redelivered completion a
	// second resume of a turn that is over. Reported rather than written,
	// so the caller fails the run instead of stranding it.
	mustLaunched(t, s, run("t1"))
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	suspended, err := s.MarkSuspended(context.Background(), "t1", suspension())
	if err != nil {
		t.Fatalf("mark suspended: %v", err)
	}
	if suspended {
		t.Error("a claimed run was re-armed by a late suspension")
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Errorf("status = %q, want the claim to stand", got.Status)
	}
}

func testAttachingABoxClearsTheSnapshotStamp(t *testing.T, s sandbox.PendingStore) {
	// paused_at is half of what an operator board draws a HELD box from, so
	// it has to move with the box. A reused box is attached while the row
	// still carries the stamp from the collect that snapshotted it — and a
	// live second run then rendered as a paused one, billing, for the rest
	// of the turn.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(context.Background(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-2"}, sandbox.Fence{}); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	if got := mustGet(t, s, "t1"); got.Paused() {
		t.Errorf("a reattached box still reads as a held snapshot: paused_at=%s", got.PausedAt)
	}
}

func testTheTailIsClaimedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	// THE AT-MOST-ONCE GATE. Two nodes both splicing a result into one
	// suspended loop produce two turns from one job — which the seat sees
	// as its own work arriving twice.
	mustLaunched(t, s, run("t1"))
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || won {
		t.Errorf("a second claim won: won=%v err=%v", won, err)
	}
}

func testAClaimIsExclusiveUnderContention(t *testing.T, s sandbox.PendingStore) {
	// The property a fake cannot have. Ten goroutines racing one tail:
	// exactly one may win.
	mustLaunched(t, s, run("t1"))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, won, err := s.ClaimForResume(context.Background(), "t1"); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d claims won; exactly one may", wins)
	}
}

func testAClaimReportsWhereItCameFrom(t *testing.T, s sandbox.PendingStore) {
	// A failed resume dispatch reverts to EXACTLY the prior status so the
	// NAK'd trigger can re-claim on redelivery. Inferring it afterwards is
	// unsound: a reused run keeps its old question, so "has a question"
	// does not mean "was parked".
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1",
		sandbox.Clarification{Question: "which branch?", Audience: "requester"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, won, err := s.ClaimForResume(context.Background(), "t1")
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if got.ClaimedFrom != sandbox.StatusAwaiting {
		t.Errorf("claimed_from = %q, want the parked status", got.ClaimedFrom)
	}
	if got.Status != sandbox.StatusResumed {
		t.Errorf("the returned row carries %q, want the POST-flip status", got.Status)
	}
}

func testAReseedIsStillClaimable(t *testing.T, s sandbox.PendingStore) {
	// Reaping the box does NOT end the run — the answer can still arrive,
	// and the work re-seeds from the pushed branch. That is the whole
	// reason reseed is a state rather than a deletion.
	mustLaunched(t, s, run("t1"))
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("set reseed: %v", err)
	}
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Errorf("a reseeded run could not be claimed: won=%v err=%v", won, err)
	}
}

func testATerminalRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	for _, status := range []string{sandbox.StatusDone, sandbox.StatusFailed} {
		mustLaunched(t, s, run(status))
		if err := s.SetStatus(context.Background(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("set %s: %v", status, err)
		}
		if _, won, _ := s.ClaimForResume(context.Background(), status); won {
			t.Errorf("a %s run was claimed", status)
		}
	}
}

func testParkingCarriesTheBranch(t *testing.T, s sandbox.PendingStore) {
	// The WIP is pushed BEFORE the question is asked, so a snapshot reaped
	// days later loses nothing a re-seed cannot recover. A question parked
	// over unpushed work is a question whose answer arrives to an empty box.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1", sandbox.Clarification{
		Question: "which base branch?", Audience: "requester",
		Branch: "wip/swe/t1", SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Branch != "wip/swe/t1" ||
		got.Question != "which base branch?" || got.Audience != "requester" {
		t.Errorf("parked run = %+v", got)
	}
}

func testOwnershipIsNotStolenByAnOlderLease(t *testing.T, s sandbox.PendingStore) {
	// A run whose epoch is HIGHER belongs to a newer lease. Taking it would
	// put two engines on one box, both collecting, both resuming.
	mustLaunched(t, s, run("t1"))
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-b:2", 5); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-a:1", 3); err != nil || won {
		t.Errorf("an older lease stole the run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-b:2" || got.OwnerEpoch != 5 {
		t.Errorf("owner = %+v", got)
	}
	// Equal epochs pass: a node re-claiming its OWN run after a restart
	// within one lease must not be locked out of it.
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-b:3", 5); err != nil || !won {
		t.Errorf("a node could not re-claim its own run: won=%v err=%v", won, err)
	}
}

func testAStaleFenceCannotWrite(t *testing.T, s sandbox.PendingStore) {
	// THE FENCE IS THE GUARANTEE, the ownership check only an optimisation:
	// a node whose lease moved cannot write even if it has not noticed yet.
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(context.Background(), "t1", "node-b:2", 7); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stale := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusFailed, stale); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a stale fence wrote %q", got.Status)
	}
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "ghost"}, stale); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a stale fence attached a box: %q", got.SandboxID)
	}
}

func testReleasingABoxClearsBothHalves(t *testing.T, s sandbox.PendingStore) {
	// A paused_at pointing at no box is a snapshot the reaper looks for
	// every tick and never finds — a warning per tick, for ever.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(context.Background(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.Paused() || !got.HasBox() {
		t.Fatalf("run = %+v", got)
	}
	if err := s.ReleaseBox(context.Background(), "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Paused() || got.HasBox() || got.CommandID != "" {
		t.Errorf("release left %+v", got)
	}
}

func testALaunchingRunIsActive(t *testing.T, s sandbox.PendingStore) {
	// A launching run is never polled, and is listed anyway: a node that
	// died mid-launch left a box behind, and a row nobody lists is a box
	// nobody reclaims.
	mustBeginLaunch(t, s, run("t1"))

	got, err := s.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusLaunching {
		t.Errorf("active = %+v, want the launching run", got)
	}
	seat, err := s.ListActiveForSeat(context.Background(), "swe")
	if err != nil {
		t.Fatalf("list for seat: %v", err)
	}
	if len(seat) != 1 {
		t.Errorf("the seat's own listing missed its launching run: %+v", seat)
	}
}

func testExecuteStateRoundTrips(t *testing.T, s sandbox.PendingStore) {
	// THE SUSPENDED CONVERSATION. Everything the tool loop needs to re-enter
	// where it stopped; a lossy round trip here resumes into a conversation
	// that is not the one that was suspended.
	mustLaunched(t, s, run("t1"))
	got := mustGet(t, s, "t1")
	if got.ExecuteState["pending_tool_call_id"] != "call_1" ||
		got.ExecuteState["pending_tool_name"] != "run_sandbox" {
		t.Errorf("execute state = %+v", got.ExecuteState)
	}
	msgs, ok := got.ExecuteState["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("the suspended conversation did not survive: %+v", got.ExecuteState["messages"])
	}
}

func testActiveIncludesResumed(t *testing.T, s sandbox.PendingStore) {
	// Boot recovery has to SEE a tail that died mid-flight with the previous
	// engine. Nothing else would ever look at that row again, and its paused
	// box would leak for ever.
	mustLaunched(t, s, run("t1"))
	if _, _, err := s.ClaimForResume(context.Background(), "t1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := s.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusResumed {
		t.Errorf("active = %+v; a resumed tail must stay visible to recovery", got)
	}

	// A terminal run does not.
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusDone, sandbox.Fence{}); err != nil {
		t.Fatalf("set done: %v", err)
	}
	if got, _ := s.ListActive(context.Background()); len(got) != 0 {
		t.Errorf("a done run is still active: %+v", got)
	}
}

func testAnAnswerFindsTheRunThatAsked(t *testing.T, s sandbox.PendingStore) {
	// A seat can have parked more than one question on one thread, and the
	// answer belongs to the most recent — the person is replying to what
	// they were just asked.
	first, second := run("t1"), run("t2")
	second.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, first)
	mustLaunched(t, s, second)
	for _, id := range []string{"t1", "t2"} {
		if err := s.MarkAwaiting(context.Background(), id,
			sandbox.Clarification{Question: "?" + id}); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	got, ok, err := s.FindAwaitingByConversation(context.Background(), "swe", "slack:C1")
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's thread is not this seat's.
	if _, ok, _ := s.FindAwaitingByConversation(context.Background(), "other", "slack:C1"); ok {
		t.Error("another seat's answer matched this seat's run")
	}
}

func testAnAnswerWithNoConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	// Matching by seat alone would hand an unrelated message to whichever
	// run happened to be waiting — and that run would treat it as the
	// answer to its question.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1",
		sandbox.Clarification{Question: "?"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, ok, _ := s.FindAwaitingByConversation(context.Background(), "swe", ""); ok {
		t.Error("a message with no conversation matched a parked run")
	}
}

func testTheReaperSeesOnlyOldSnapshots(t *testing.T, s sandbox.PendingStore) {
	// A provider holds a paused box indefinitely and bills for the snapshot,
	// so nothing else would ever reclaim it. What the reaper must NOT see is
	// a fresh pause or a run with no box at all.
	for _, tc := range []struct {
		id     string
		paused time.Time
		hasBox bool
	}{
		{"old", base.Add(-2 * time.Hour), true},
		{"fresh", base.Add(-time.Minute), true},
		{"boxless", base.Add(-2 * time.Hour), false},
	} {
		mustLaunched(t, s, run(tc.id))
		if tc.hasBox {
			if err := s.AttachSandbox(context.Background(), tc.id,
				sandbox.BoxRef{SandboxID: "box-" + tc.id}, sandbox.Fence{}); err != nil {
				t.Fatalf("attach %s: %v", tc.id, err)
			}
		}
		if err := s.MarkBoxPaused(context.Background(), tc.id, tc.paused); err != nil {
			t.Fatalf("pause %s: %v", tc.id, err)
		}
	}
	got, err := s.ListPausedBefore(context.Background(), base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list paused: %v", err)
	}
	if len(got) != 1 || got[0].TurnID != "old" {
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		t.Errorf("reapable = %v, want only the old snapshot with a box", ids)
	}
}

func testListingsAreStable(t *testing.T, s sandbox.PendingStore) {
	// A recovery pass that reordered its work every boot would make two runs
	// of the same failure look like different failures.
	for i, id := range []string{"c", "a", "b"} {
		r := run(id)
		r.CreatedAt = base.Add(time.Duration(i) * time.Second)
		mustLaunched(t, s, r)
	}
	var first []string
	for range 5 {
		got, err := s.ListActive(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		if first == nil {
			first = ids
			continue
		}
		if len(ids) != len(first) {
			t.Fatalf("listing length changed: %v then %v", first, ids)
		}
		for i := range ids {
			if ids[i] != first[i] {
				t.Fatalf("listing order changed: %v then %v", first, ids)
			}
		}
	}
}

// The reaper's flip is the AUTHORITY for the whole reap — it decides whether
// the box gets destroyed — so two reapers racing must produce exactly one
// destruction.
func testAPauseExpiresExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")

	const racers = 10
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			won, err := s.ExpirePause(context.Background(), "t1")
			if err != nil {
				t.Errorf("ExpirePause: %v", err)
				return
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d reapers won the flip, want exactly 1 — each winner kills a box", got, racers)
	}
	got, _, err := s.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != sandbox.StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, sandbox.StatusReseed)
	}
}

// Every other paused box belongs to a tail that is actively being driven, and
// expiring one from the reaper would kill it out from under live work.
func testOnlyAParkedRunCanExpire(t *testing.T, s sandbox.PendingStore) {
	for _, status := range []string{
		sandbox.StatusRunning, sandbox.StatusResumed,
		sandbox.StatusDone, sandbox.StatusFailed, sandbox.StatusReseed,
	} {
		mustLaunched(t, s, run(status))
		if err := s.SetStatus(context.Background(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		won, err := s.ExpirePause(context.Background(), status)
		if err != nil {
			t.Fatalf("ExpirePause(%s): %v", status, err)
		}
		if won {
			t.Fatalf("a run in %q was expired by the pause reaper", status)
		}
	}
}

// The answer that un-parks a run and the reaper that expires it are the same
// race, from the two sides: whichever lands first, the other must lose.
func testAnAnsweredRunCannotBeExpiredUnderTheResume(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")

	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	won, err := s.ExpirePause(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ExpirePause: %v", err)
	}
	if won {
		t.Fatal("the reaper expired a run whose answer had already claimed it — it would destroy the box the resume is reconnecting to")
	}
}

// park moves a seeded run to awaiting_clarification with a box attached.
func park(t *testing.T, s sandbox.PendingStore, turnID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: "box-" + turnID, CodingAgent: "claude-code", PauseTTLSec: 1800,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{
		// The branch is what the answer re-seeds from once the box is
		// reclaimed, so a parked run without one is not a realistic
		// starting point for anything the reaper does.
		Question: "which branch?", Audience: "requester", Branch: "wip/" + turnID,
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
}

// The gap between two writes is a state a reader can SEE: a run reading as
// `reseed` while it still names its box tells an arriving answer that the
// checkout is live, moments before the reaper destroys it. One statement, no
// window.
func testExpiringAPauseClearsTheBoxInTheSameWrite(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if err := s.MarkBoxPaused(context.Background(), "t1", base); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}

	won, err := s.ExpirePause(context.Background(), "t1")
	if err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v", won, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, sandbox.StatusReseed)
	}
	if got.SandboxID != "" || got.CommandID != "" {
		t.Fatalf("the row still names a box that is about to be destroyed: %+v", got)
	}
	if !got.PausedAt.IsZero() {
		t.Fatal("the row still claims a snapshot is held")
	}
	// The QUESTION survives: the run is not over, and the answer still has
	// to find it.
	if got.Question == "" || got.Branch == "" {
		t.Fatalf("the reseed lost what the answer needs: question=%q branch=%q",
			got.Question, got.Branch)
	}
}

// --- the bridged run's durable tool log ------------------------------------

// A BRIDGED RUN'S CALLS HAVE NOWHERE ELSE TO LIVE. A native tool loop keeps
// them on a surface in memory and the turn writes them when it ends; a bridged
// run's are made by a process outside the engine and can outlive the node. A
// reviewer of a resumed run with no log judges a turn that reads as having
// acted on nothing.
func testBridgeCallsAreAppendedInOrder(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge")
	mustLaunched(t, s, r)

	for _, name := range []string{"read_page", "post_message", "read_page"} {
		ok, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeCall{
			Name: name, Args: `{"id":1}`, Output: name + " ok",
		})
		if err != nil || !ok {
			t.Fatalf("AppendBridgeCall(%s) = %v, %v", name, ok, err)
		}
	}

	got := mustGet(t, s, r.TurnID)
	if len(got.BridgeCalls) != 3 {
		t.Fatalf("%d calls recorded: %+v", len(got.BridgeCalls), got.BridgeCalls)
	}
	want := []string{"read_page", "post_message", "read_page"}
	for i, name := range want {
		if got.BridgeCalls[i].Name != name {
			t.Errorf("call %d = %q, want %q", i, got.BridgeCalls[i].Name, name)
		}
	}
	// The ARGUMENTS and the OUTCOME ride along, because a log of bare
	// names does not tell a reviewer whether the turn delivered anything.
	if got.BridgeCalls[0].Args != `{"id":1}` || got.BridgeCalls[0].Output != "read_page ok" {
		t.Errorf("the call does not carry what it did: %+v", got.BridgeCalls[0])
	}
	// STAMPED, so a reader can see the shape of a run that stalled.
	if got.BridgeCalls[0].At.IsZero() {
		t.Error("the call has no timestamp")
	}
}

// NO FENCE, unlike every other mutation on this row. The call already ran and
// its effect already happened; refusing to record it because the seat's lease
// moved mid-run would lose evidence of something that is true either way.
func testBridgeCallsSurviveWithoutAFence(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-fence")
	mustLaunched(t, s, r)
	// Move the run under a NEWER owner, so the caller's own view of the
	// lease is stale by any measure.
	if ok, err := s.ClaimOwnership(ctx, r.TurnID, "node-b", 99); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}

	ok, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeCall{Name: "read_page"})
	if err != nil || !ok {
		t.Fatalf("a log append was refused by ownership: %v, %v", ok, err)
	}
	if got := mustGet(t, s, r.TurnID); len(got.BridgeCalls) != 1 {
		t.Errorf("%d calls recorded", len(got.BridgeCalls))
	}
}

// A LATE CALL FROM A BOX THAT IS SHUTTING DOWN is the ordinary shape here, and
// it must not be an error: the caller cannot fail the box's call over a log
// row, so false and true have to be equally safe to ignore.
func testBridgeCallsForAMissingRunAreDropped(t *testing.T, s sandbox.PendingStore) {
	ok, err := s.AppendBridgeCall(t.Context(), "never-existed",
		sandbox.BridgeCall{Name: "read_page"})
	if err != nil {
		t.Fatalf("a missing run was an error: %v", err)
	}
	if ok {
		t.Error("an append onto no row reported success")
	}
}

// THE MIDDLE IS WHAT GETS DROPPED. How a run began and how it ended are what
// explain it, and a log truncated to its last N loses the former entirely.
func testBridgeCallsDropTheMiddleNotTheStart(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-cap")
	mustLaunched(t, s, r)

	total := sandbox.MaxBridgeCalls + 10
	for i := range total {
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeCall{
			Name: fmt.Sprintf("call-%03d", i),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got := mustGet(t, s, r.TurnID)
	if len(got.BridgeCalls) != sandbox.MaxBridgeCalls {
		t.Fatalf("%d calls kept, want %d", len(got.BridgeCalls), sandbox.MaxBridgeCalls)
	}
	if first := got.BridgeCalls[0].Name; first != "call-000" {
		t.Errorf("the first call was dropped: %q — a log cut to its tail loses how the run began", first)
	}
	last := got.BridgeCalls[len(got.BridgeCalls)-1].Name
	if want := fmt.Sprintf("call-%03d", total-1); last != want {
		t.Errorf("the last call = %q, want %q", last, want)
	}
	// AND THE GAP IS REPORTED: a log that silently skips is a log that
	// lies about what the run did.
	if got.BridgeCallsElided != 10 {
		t.Errorf("elided = %d, want 10", got.BridgeCallsElided)
	}
}
