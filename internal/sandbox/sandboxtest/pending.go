// Package sandboxtest is the pending-run store's contract suite.
//
// THE PROPERTIES THAT MATTER HERE ARE THE STORE'S. The at-most-once tail claim,
// the scoped release and the charge record it carries, the epoch fence, the
// box record's two halves moving together: each is a conditional write, not
// code around one, so a suite that ran only against a fake would assert the
// author's intent and nothing about the store. The one implementation is
// [sandbox.CoordStore]. The record operations it is built on are certified on
// both coordination backends by coordtest, and this suite certifies every
// conditional flip built on top of them.
package sandboxtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var base = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// Run drives every case against one store.
//
// newStore hands back the store AND the raw records it is built on, because
// one property is about bytes no [sandbox.PendingRun] can express: what a
// flip does to a key this build does not know. A case that could reach the
// row only through the store would be asking the codec under test to describe
// its own output.
func Run(t *testing.T, newStore func(t *testing.T) (sandbox.PendingStore, coord.SandboxRuns)) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, sandbox.PendingStore)
	}{
		{"WorkItemSurvivesParkAndResume", testWorkItemSurvivesParkAndResume},
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
		{"AFinishedRunIsGoneForEveryReader", testAFinishedRunIsGoneForEveryReader},
		{"AFinishedRunIsNotRecreatedByALateWrite", testAFinishedRunIsNotRecreatedByALateWrite},
		{"ARunIsFinishedExactlyOnce", testARunIsFinishedExactlyOnce},
		{"AnEndingIsRefusedOutsideItsLicense", testAnEndingIsRefusedOutsideItsLicense},
		{"AnEndingIsNotAStatus", testAnEndingIsNotAStatus},
		{"EveryLaunchIsNamedAnew", testEveryLaunchIsNamedAnew},
		{"AClaimForAnotherLaunchIsRefused", testAClaimForAnotherLaunchIsRefused},
		{"ACompletionDoesNotClaimAParkedRun", testACompletionDoesNotClaimAParkedRun},
		{"AnAnswerDoesNotClaimARunningRun", testAnAnswerDoesNotClaimARunningRun},
		{"AReleaseHandsTheClaimBackWhereItFoundIt", testAReleaseHandsTheClaimBackWhereItFoundIt},
		{"AReleaseOfAnotherLaunchIsRefused", testAReleaseOfAnotherLaunchIsRefused},
		{"AReleaseOfARunNoLongerClaimedIsRefused", testAReleaseOfARunNoLongerClaimedIsRefused},
		{"AStaleFenceCannotRelease", testAStaleFenceCannotRelease},
		{"AReleaseGoesBackOnlyToAClaimableStatus", testAReleaseGoesBackOnlyToAClaimableStatus},
		{"AReleaseOfAMissingRunIsNotAnError", testAReleaseOfAMissingRunIsNotAnError},
		{"AReleaseRecordsTheClaimsCharge", testAReleaseRecordsTheClaimsCharge},
		{"AReleaseNeverClearsAChargeRecord", testAReleaseNeverClearsAChargeRecord},
		{"ARefusedReleaseRecordsNoCharge", testARefusedReleaseRecordsNoCharge},
		{"OnlyALaunchClearsAChargeRecord", testOnlyALaunchClearsAChargeRecord},
		{"ParkingCarriesTheBranch", testParkingCarriesTheBranch},
		{"OwnershipIsNotStolenByAnOlderLease", testOwnershipIsNotStolenByAnOlderLease},
		{"AStaleFenceCannotWrite", testAStaleFenceCannotWrite},
		{"ReleasingABoxClearsBothHalves", testReleasingABoxClearsBothHalves},
		{"ExecuteStateRoundTrips", testExecuteStateRoundTrips},
		{"ActiveIncludesResumed", testActiveIncludesResumed},
		{"AnAnswerFindsTheRunThatAsked", testAnAnswerFindsTheRunThatAsked},
		{"ARunParkedOnATopLevelDMIsAnsweredInItsThread", testARunParkedOnATopLevelDMIsAnsweredInItsThread},
		{"TwoQuestionsOnOneDMAreToldApartByTheirThreads", testTwoQuestionsOnOneDMAreToldApartByTheirThreads},
		{"AnAnswerOnAnotherConversationMatchesNothing", testAnAnswerOnAnotherConversationMatchesNothing},
		{"AnAnswerWithNoConversationMatchesNothing", testAnAnswerWithNoConversationMatchesNothing},
		{"ARowWithNoIdentityReportsBackToItsPartition", testARowWithNoIdentityReportsBackToItsPartition},
		{"APreSplitRowIsStillAnswerable", testAPreSplitRowIsStillAnswerable},
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
			store, _ := newStore(t)
			tc.fn(t, store)
		})
	}
	raw := []struct {
		name string
		fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
	}{
		{"AudienceFieldsSurviveAStatusFlipByABuildThatDoesNotKnowThem",
			testAudienceFieldsSurviveAStatusFlipByABuildThatDoesNotKnowThem},
	}
	for _, tc := range raw {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, runs := newStore(t)
			tc.fn(t, store, runs)
		})
	}
}

func run(turnID string) sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		CodingAgent: "claude-code", TaskDescription: "fix the flake",
		// THE TWO CONVERSATION VALUES OF A DIRECT MESSAGE, which is the
		// one shape where they differ — a reply in the thread this run's
		// question was asked in partitions on the thread while the
		// conversation is the whole DM line. A fixture that made them
		// equal would let every backend certify the split by accident.
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
		Reply:   "tool",
		TraceID: "tr-1", CreatedAt: base,
	}
}

// answerOnTheDM is the reply to the question [run] parked on: the same DM
// line, in the same thread. BOTH VALUES, because a store is free to read
// either and a fixture that stated one would let it read that one alone.
var answerOnTheDM = sandbox.ConversationRef{
	Identity: "chat:D1", Partition: "chat:D1:root-1",
}

// mustBeginLaunch opens a launch and leaves the run where a launch leaves it:
// [sandbox.StatusLaunching], its job started and its conversation not yet
// written.
func mustBeginLaunch(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) {
	t.Helper()
	if err := s.BeginLaunch(t.Context(), r, sandbox.Fence{}); err != nil {
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
	suspended, err := s.MarkSuspended(t.Context(), r.TurnID, suspension())
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
	got, ok, err := s.Get(t.Context(), turnID)
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
	if err := s.AttachSandbox(t.Context(), "t1",
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
	if err := s.BeginLaunch(t.Context(), run(""), sandbox.Fence{}); err == nil {
		t.Error("a run with no turn id was persisted")
	}
}

func testALaunchingRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	// THE WINDOW THIS STATE EXISTS TO CLOSE. The job is started and can
	// finish at any moment, but the turn has not yet written the
	// conversation a resume re-enters. A completion claimed here would find
	// nothing to resume into and fail the whole turn.
	mustBeginLaunch(t, s, run("t1"))

	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1")); err != nil || won {
		t.Fatalf("a launching run was claimed: won=%v err=%v", won, err)
	}
	// Nor by a tail that names the status outright: the closed set is the
	// store's to keep, not the caller's to widen.
	widened := sandbox.Tail{
		Launch: mustGet(t, s, "t1").LaunchID, From: []string{sandbox.StatusLaunching},
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", widened); err != nil || won {
		t.Fatalf("a tail naming launching claimed it: won=%v err=%v", won, err)
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
	suspended, err := s.MarkSuspended(t.Context(), "t1", suspension())
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
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1")); err != nil || !won {
		t.Errorf("a suspended run was not claimable: won=%v err=%v", won, err)
	}
}

func testOnlyALaunchingRunCanSuspend(t *testing.T, s sandbox.PendingStore) {
	// A run whose tail has already been claimed has nowhere to put a
	// suspension, and re-arming it would hand a redelivered completion a
	// second resume of a turn that is over. Reported rather than written,
	// so the caller fails the run instead of stranding it.
	mustLaunched(t, s, run("t1"))
	mustClaim(t, s, "t1")

	suspended, err := s.MarkSuspended(t.Context(), "t1", suspension())
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
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.AttachSandbox(t.Context(), "t1",
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
	tail := completionOf(t, s, "t1")
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail); err != nil || won {
		t.Errorf("a second claim won: won=%v err=%v", won, err)
	}
}

func testAClaimIsExclusiveUnderContention(t *testing.T, s sandbox.PendingStore) {
	// The property a fake cannot have. Ten goroutines racing one tail:
	// exactly one may win.
	mustLaunched(t, s, run("t1"))
	tail := completionOf(t, s, "t1")
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 10 {
		wg.Go(func() {
			if _, won, err := s.ClaimForResume(t.Context(), "t1", tail); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
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
	if err := s.MarkAwaiting(t.Context(), "t1",
		sandbox.Clarification{Question: "which branch?", Audience: "requester"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"))
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
	if err := s.SetStatus(t.Context(), "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("set reseed: %v", err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1")); err != nil || !won {
		t.Errorf("a reseeded run could not be claimed: won=%v err=%v", won, err)
	}
}

// A FINISHED RUN HAS NO RECORD. The bucket is ageless and every completion
// poll and seat recovery reads all of it, so a settled run that stayed behind
// would be read on every one of those passes for the life of the deployment.
// Nothing may still find it: not the tail claim, not the busy check, not an
// answer matching a question it once asked.
func testAFinishedRunIsGoneForEveryReader(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	// Read before the record goes: a claim names the launch it is for, and
	// after the delete there is nothing left to read one from.
	tail := answerTo(t, s, "t1")

	settled, finished, err := s.Finish(ctx, "t1", sandbox.Fence{}, sandbox.Active)
	if err != nil || !finished {
		t.Fatalf("Finish = %v, %v; want the record deleted", finished, err)
	}
	// THE RECORD IT DELETED, not the caller's snapshot of it: a settle that
	// could not read the row first reclaims the box this names, so a store
	// handing back a zero value there would leave a live box named by
	// nothing.
	if settled.TurnID != "t1" || settled.SandboxID != "box-t1" ||
		settled.Status != sandbox.StatusAwaiting {
		t.Errorf("Finish handed back %+v; want the record as the store held it", settled)
	}
	if got, found, err := s.Get(ctx, "t1"); err != nil || found {
		t.Fatalf("Get after Finish = %+v, found %v, %v; want no record", got, found, err)
	}
	if active, err := s.ListActive(ctx); err != nil || len(active) != 0 {
		t.Errorf("ListActive after Finish = %+v, %v; want none", active, err)
	}
	if seat, err := s.ListActiveForSeat(ctx, "swe"); err != nil || len(seat) != 0 {
		t.Errorf("the seat's busy read still sees a finished run: %+v, %v", seat, err)
	}
	if _, found, err := s.FindAwaitingByConversation(ctx, "swe", answerOnTheDM); err != nil || found {
		t.Errorf("an answer matched the question of a finished run: found %v, %v", found, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", tail); err != nil || won {
		t.Errorf("a finished run was claimed: won=%v err=%v", won, err)
	}
	// Two parties reaching the end of one run is ordinary, not an error.
	if _, again, err := s.Finish(ctx, "t1", sandbox.Fence{}, sandbox.Active); err != nil || again {
		t.Errorf("a second Finish = %v, %v; want false and no error", again, err)
	}
}

// A write that arrives after the run ended is the ordinary shape of a box
// shutting down or a peer a moment behind. Each is a conditional flip on an
// existing record, so none of them may bring the record back: a resurrected
// row is a run that nothing will ever settle again.
func testAFinishedRunIsNotRecreatedByALateWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if _, _, err := s.Finish(ctx, "t1", sandbox.Fence{}, sandbox.Active); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	late := map[string]func() error{
		"SetStatus": func() error { return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{}) },
		"AttachSandbox": func() error {
			return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-1"}, sandbox.Fence{})
		},
		"MarkBoxPaused": func() error { return s.MarkBoxPaused(ctx, "t1", base) },
		"ReleaseBox":    func() error { return s.ReleaseBox(ctx, "t1") },
		"MarkAwaiting": func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "still there?"})
		},
		"MarkSuspended": func() error {
			_, err := s.MarkSuspended(ctx, "t1", suspension())
			return err
		},
		"ClaimOwnership": func() error {
			_, err := s.ClaimOwnership(ctx, "t1", "node-b:2", 9)
			return err
		},
		"ExpirePause": func() error {
			_, err := s.ExpirePause(ctx, "t1")
			return err
		},
		"AppendBridgeCall": func() error {
			_, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeCall{Name: "read_page"})
			return err
		},
	}
	for name, write := range late {
		if err := write(); err != nil {
			t.Errorf("%s on a finished run: %v; want the ordinary no-op", name, err)
		}
		if _, found, err := s.Get(ctx, "t1"); err != nil || found {
			t.Fatalf("%s recreated a finished run's record (found %v, %v)", name, found, err)
		}
	}
}

// Every party that ends a run deletes a record it read. Racing ends must
// agree that exactly one of them did it, so that whatever each does on the
// strength of that answer, such as announcing a lost turn, happens once.
func testARunIsFinishedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	const racers = 10
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Go(func() {
			<-start
			_, finished, err := s.Finish(t.Context(), "t1", sandbox.Fence{}, sandbox.Active)
			if err != nil {
				t.Errorf("Finish: %v", err)
				return
			}
			if finished {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent Finish calls reported deleting the record, want exactly 1", got, racers)
	}
}

// A settle that could NOT read the row hands the decision to the store: it
// asks for the run to be ended only while it is still the status its claim
// left it in. So a row that moved on under it — a relaunch the resumed turn
// made, which takes the run back through launching and reuses the very box
// this settle would kill — has to survive the call, and a row still in the
// claim has to be deleted by it.
func testAnEndingIsRefusedOutsideItsLicense(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	claimed := []string{sandbox.StatusResumed}

	if _, ended, err := s.Finish(ctx, "t1", sandbox.Fence{}, claimed); err != nil || ended {
		t.Errorf("a running run was ended under a claimed-only licence: %v, %v", ended, err)
	}
	if got, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a refused ending (found %v, %v)", found, err)
	} else if got.Status != sandbox.StatusRunning {
		t.Errorf("a refused ending left the record at %q", got.Status)
	}
	// An empty licence ends nothing at all, which is the safe reading of a
	// caller that stated none.
	if _, ended, err := s.Finish(ctx, "t1", sandbox.Fence{}, nil); err != nil || ended {
		t.Errorf("an ending with no licence deleted the record: %v, %v", ended, err)
	}

	if _, _, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, ended, err := s.Finish(ctx, "t1", sandbox.Fence{}, claimed); err != nil || !ended {
		t.Errorf("the run the claim left behind was not ended: %v, %v", ended, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || found {
		t.Errorf("the claimed run still has a record (found %v, %v)", found, err)
	}
}

// Done and failed are not states a record can be written in, because a run
// that reached either has no record. Refused at the write, so a caller that
// reaches for the old terminal flip fails loudly instead of leaving a record
// every listing skips and nothing ever deletes.
func testAnEndingIsNotAStatus(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	for _, status := range []string{"done", "failed", ""} {
		if err := s.SetStatus(t.Context(), "t1", status, sandbox.Fence{}); err == nil {
			t.Errorf("SetStatus(%q) was accepted", status)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a refused status changed the record to %q", got.Status)
	}
}

func testEveryLaunchIsNamedAnew(t *testing.T, s sandbox.PendingStore) {
	// The name is what tells a completion's job from the one that replaced
	// it, so it has to change on every launch under one turn id, and it is
	// the store's to mint: a caller that could choose it could reuse one.
	chosen := run("t1")
	chosen.LaunchID = "chosen-by-the-caller"
	mustBeginLaunch(t, s, chosen)
	first := mustGet(t, s, "t1").LaunchID
	if first == "" || first == chosen.LaunchID {
		t.Fatalf("launch id = %q, want one the store minted", first)
	}
	mustBeginLaunch(t, s, run("t1"))
	if second := mustGet(t, s, "t1").LaunchID; second == "" || second == first {
		t.Errorf("a second launch under one turn id kept the name %q", second)
	}
}

func testAClaimForAnotherLaunchIsRefused(t *testing.T, s sandbox.PendingStore) {
	// A completion that outlived its job arrives at a row holding the next
	// one. The status alone cannot tell them apart, since both are running;
	// only the name can.
	mustLaunched(t, s, run("t1"))
	stale := completionOf(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	if suspended, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}

	if _, won, err := s.ClaimForResume(t.Context(), "t1", stale); err != nil || won {
		t.Fatalf("the previous job's completion claimed the next job: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("a refused claim moved the row to %q", got.Status)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1")); err != nil || !won {
		t.Errorf("the next job's own completion could not claim it: won=%v err=%v", won, err)
	}
}

func testACompletionDoesNotClaimAParkedRun(t *testing.T, s sandbox.PendingStore) {
	// A parked run is waiting on a person, and only their answer may take
	// it. A completion that finds it there is a duplicate of the one that
	// parked it.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1")); err != nil || won {
		t.Fatalf("a completion claimed a parked run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting {
		t.Errorf("status = %q, want the run still waiting on its answer", got.Status)
	}
}

func testAnAnswerDoesNotClaimARunningRun(t *testing.T, s sandbox.PendingStore) {
	// And the other way round: a running job asked nothing, so an answer
	// that finds one has nothing to answer.
	mustLaunched(t, s, run("t1"))
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1")); err != nil || won {
		t.Fatalf("an answer claimed a running job: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want the job still running", got.Status)
	}
}

// releaseOf is the release that hands a claimed run back where it was taken
// from, under the lease it was taken under.
func releaseOf(claimed sandbox.PendingRun) sandbox.Release {
	return sandbox.Release{
		Launch: claimed.LaunchID, To: claimed.ClaimedFrom,
		Fence: sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
	}
}

// mustRelease hands a claimed run back.
func mustRelease(t *testing.T, s sandbox.PendingStore, claimed sandbox.PendingRun) {
	t.Helper()
	released, err := s.ReleaseClaim(t.Context(), claimed.TurnID, releaseOf(claimed))
	if err != nil || !released {
		t.Fatalf("release %s: released=%v err=%v", claimed.TurnID, released, err)
	}
}

func testAReleaseHandsTheClaimBackWhereItFoundIt(t *testing.T, s sandbox.PendingStore) {
	// The retry a failed resume opens has to find the run exactly where the
	// signal it is retrying can take it: a completion's job running, an
	// answer's question waiting.
	mustLaunched(t, s, run("t1"))
	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("status = %q, want the completion's job running again", got.Status)
	}
	mustClaim(t, s, "t1")

	mustLaunched(t, s, run("t2"))
	if err := s.MarkAwaiting(t.Context(), "t2", sandbox.Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	answered, won, err := s.ClaimForResume(t.Context(), "t2", answerTo(t, s, "t2"))
	if err != nil || !won {
		t.Fatalf("claim the answer: won=%v err=%v", won, err)
	}
	mustRelease(t, s, answered)
	if got := mustGet(t, s, "t2"); got.Status != sandbox.StatusAwaiting || got.Question != "which branch?" {
		t.Fatalf("row = %s %q, want the question waiting on its answer again", got.Status, got.Question)
	}
}

func testAReleaseOfAnotherLaunchIsRefused(t *testing.T, s sandbox.PendingStore) {
	// The resumed executor called run_sandbox again, so the row holds a new
	// launch with the claimed job's conversation cleared. Handed back, that
	// launch would read as a running job with nothing to resume into, and
	// its completion would collect whatever the box still held.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	relaunched := mustGet(t, s, "t1")

	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("the claim handed back a launch it never took: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusLaunching || got.LaunchID != relaunched.LaunchID {
		t.Fatalf("row = %s under %q, want the relaunch left as it was", got.Status, got.LaunchID)
	}
	// Nor once the new job is claimed in its turn: the status is the same,
	// and only the launch tells the two claims apart.
	if suspended, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}
	next := mustClaim(t, s, "t1")
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("the claim handed back the next job's claim: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Fatalf("status = %q, want the next job still claimed", got.Status)
	}
	mustRelease(t, s, next)
}

func testAReleaseOfARunNoLongerClaimedIsRefused(t *testing.T, s sandbox.PendingStore) {
	// The seat's next owner reaps a claim its previous owner abandoned, and
	// announces the run lost. A release landing after that would revive it
	// with its box torn down. And a run nobody claimed has no claim to hand
	// back at all.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	if _, finished, err := s.Finish(t.Context(), "t1", sandbox.Fence{},
		sandbox.Active); err != nil || !finished {
		t.Fatalf("reap: finished=%v err=%v", finished, err)
	}
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("a reaped run was handed back: released=%v err=%v", released, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("a release recreated the record of a run already ended: found=%v err=%v", found, err)
	}

	mustLaunched(t, s, run("t2"))
	unclaimed := sandbox.Release{Launch: mustGet(t, s, "t2").LaunchID, To: sandbox.StatusRunning}
	if released, err := s.ReleaseClaim(t.Context(), "t2", unclaimed); err != nil || released {
		t.Errorf("a run nobody claimed reported a release: released=%v err=%v", released, err)
	}
}

func testAStaleFenceCannotRelease(t *testing.T, s sandbox.PendingStore) {
	// A claim taken under a lease that has since moved hands nothing back:
	// the seat's new owner decides what becomes of the run.
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-a:1", 3); err != nil {
		t.Fatalf("own: %v", err)
	}
	claimed := mustClaim(t, s, "t1")
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 5); err != nil {
		t.Fatalf("take over: %v", err)
	}
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("a stale fence handed the claim back: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Fatalf("status = %q, want the claim left where the new owner finds it", got.Status)
	}
	current := releaseOf(claimed)
	current.Fence = sandbox.Fence{Owner: "node-b:2", Epoch: 5}
	if released, err := s.ReleaseClaim(t.Context(), "t1", current); err != nil || !released {
		t.Errorf("the current lease could not hand the claim back: released=%v err=%v", released, err)
	}
}

func testAReleaseGoesBackOnlyToAClaimableStatus(t *testing.T, s sandbox.PendingStore) {
	// No claim takes a run out of any other status, so a release naming one
	// is a caller's mistake, and writing it would strand the run where no
	// signal can take it.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	for _, to := range []string{sandbox.StatusLaunching, sandbox.StatusResumed, "done", ""} {
		release := releaseOf(claimed)
		release.To = to
		if released, err := s.ReleaseClaim(t.Context(), "t1", release); err == nil || released {
			t.Errorf("a release to %q was accepted: released=%v err=%v", to, released, err)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Errorf("a refused release moved the run to %q", got.Status)
	}
}

func testAReleaseOfAMissingRunIsNotAnError(t *testing.T, s sandbox.PendingStore) {
	released, err := s.ReleaseClaim(t.Context(), "never-launched",
		sandbox.Release{To: sandbox.StatusRunning})
	if err != nil || released {
		t.Errorf("a missing run reported a release: released=%v err=%v", released, err)
	}
}

// completionOf is the tail a completion of the run's current job claims.
func completionOf(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.Tail {
	t.Helper()
	return sandbox.CompletionTail(mustGet(t, s, turnID).LaunchID)
}

// answerTo is the tail an answer to the run's current question claims.
func answerTo(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.Tail {
	t.Helper()
	return sandbox.AnswerTail(mustGet(t, s, turnID).LaunchID)
}

// mustClaim takes a running run's tail, as its job's completion would.
func mustClaim(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	got, won, err := s.ClaimForResume(t.Context(), turnID, completionOf(t, s, turnID))
	if err != nil || !won {
		t.Fatalf("claim %s: won=%v err=%v", turnID, won, err)
	}
	return got
}

// mustReleaseCharged hands a claimed run back with its charge recorded.
func mustReleaseCharged(t *testing.T, s sandbox.PendingStore, claimed sandbox.PendingRun) {
	t.Helper()
	release := releaseOf(claimed)
	release.Charged = true
	released, err := s.ReleaseClaim(t.Context(), claimed.TurnID, release)
	if err != nil || !released {
		t.Fatalf("release %s charged: released=%v err=%v", claimed.TurnID, released, err)
	}
}

func testAReleaseRecordsTheClaimsCharge(t *testing.T, s sandbox.PendingStore) {
	// THE DURABLE HALF OF CHARGING A RUN ONCE, in the one write that reopens
	// the run to a retry. The retry reads nothing but the row its own claim
	// returns, on this node or the seat's next owner, so the record has to
	// come back on it.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	if claimed.Charged {
		t.Fatal("a run nothing has charged reads as charged")
	}
	mustReleaseCharged(t, s, claimed)
	if got := mustClaim(t, s, "t1"); !got.Charged {
		t.Error("the retry's claim came back without the charge the first attempt made")
	}
}

func testAReleaseNeverClearsAChargeRecord(t *testing.T, s sandbox.PendingStore) {
	// A charge that landed stays landed. A later release that says nothing
	// about it (a retry whose resume failed too, having charged nothing
	// because the record said not to) must not erase what the first
	// attempt recorded, or the retry after it charges the run again.
	mustLaunched(t, s, run("t1"))
	mustReleaseCharged(t, s, mustClaim(t, s, "t1"))
	retry := mustClaim(t, s, "t1")
	retry.Charged = false
	mustRelease(t, s, retry)
	if got := mustGet(t, s, "t1"); !got.Charged {
		t.Error("a release that carried no charge erased the record of one")
	}
}

func testARefusedReleaseRecordsNoCharge(t *testing.T, s sandbox.PendingStore) {
	// A release that hands nothing back writes nothing, the record included:
	// on a row a second launch has opened it would let that job's own spend
	// go uncounted.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	release := releaseOf(claimed)
	release.Charged = true
	if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || released {
		t.Fatalf("the claim handed back a launch it never took: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Charged {
		t.Error("a refused release recorded its charge on the next launch")
	}
}

func testOnlyALaunchClearsAChargeRecord(t *testing.T, s sandbox.PendingStore) {
	// The record is launch-scoped, and every write to the row other than a
	// launch is about the same launch. One of them dropping it would let the
	// retry that write opens charge the run again, so each is walked here;
	// the launch that follows is a new job, and must not inherit it.
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	mustReleaseCharged(t, s, mustClaim(t, s, "t1"))
	tail := completionOf(t, s, "t1")
	var claimed sandbox.PendingRun
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"pause the box", func() error { return s.MarkBoxPaused(ctx, "t1", base) }},
		{"take ownership", func() error {
			_, err := s.ClaimOwnership(ctx, "t1", "node-b:2", 3)
			return err
		}},
		{"claim the tail", func() error {
			var err error
			claimed, _, err = s.ClaimForResume(ctx, "t1", tail)
			return err
		}},
		{"hand the claim back", func() error {
			_, err := s.ReleaseClaim(ctx, "t1", releaseOf(claimed))
			return err
		}},
		{"change status", func() error {
			return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{})
		}},
		{"park on a question", func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?"})
		}},
		{"expire the pause", func() error {
			_, err := s.ExpirePause(ctx, "t1")
			return err
		}},
		{"attach a box", func() error {
			return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-2"}, sandbox.Fence{})
		}},
		{"release the box", func() error { return s.ReleaseBox(ctx, "t1") }},
		{"append a bridged call", func() error {
			_, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeCall{Name: "read_page", At: base})
			return err
		}},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := mustGet(t, s, "t1"); !got.Charged {
			t.Fatalf("%s dropped the charge record", step.name)
		}
	}

	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); got.Charged {
		t.Error("a second launch inherited the first job's charge, so its own spend would go uncounted")
	}
}

func testParkingCarriesTheBranch(t *testing.T, s sandbox.PendingStore) {
	// The WIP is pushed BEFORE the question is asked, so a snapshot reaped
	// days later loses nothing a re-seed cannot recover. A question parked
	// over unpushed work is a question whose answer arrives to an empty box.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
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
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 5); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-a:1", 3); err != nil || won {
		t.Errorf("an older lease stole the run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-b:2" || got.OwnerEpoch != 5 {
		t.Errorf("owner = %+v", got)
	}
	// Equal epochs pass: a node re-claiming its OWN run after a restart
	// within one lease must not be locked out of it.
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-b:3", 5); err != nil || !won {
		t.Errorf("a node could not re-claim its own run: won=%v err=%v", won, err)
	}
}

func testAStaleFenceCannotWrite(t *testing.T, s sandbox.PendingStore) {
	// THE FENCE IS THE GUARANTEE, the ownership check only an optimisation:
	// a node whose lease moved cannot write even if it has not noticed yet.
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 7); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stale := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if err := s.SetStatus(t.Context(), "t1", sandbox.StatusReseed, stale); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a stale fence wrote %q", got.Status)
	}
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "ghost"}, stale); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a stale fence attached a box: %q", got.SandboxID)
	}
	// Nor may it END the run: a node whose lease moved deleting the record
	// its successor recovered strands the successor's box.
	if _, finished, err := s.Finish(t.Context(), "t1", stale, sandbox.Active); err != nil || finished {
		t.Errorf("a stale fence finished the run: %v, %v", finished, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a stale Finish (found %v, %v)", found, err)
	}
	if _, finished, err := s.Finish(t.Context(), "t1",
		sandbox.Fence{Owner: "node-b:2", Epoch: 7}, sandbox.Active); err != nil || !finished {
		t.Errorf("the owning lease could not finish its own run: %v, %v", finished, err)
	}
}

func testReleasingABoxClearsBothHalves(t *testing.T, s sandbox.PendingStore) {
	// A paused_at pointing at no box is a snapshot the reaper looks for
	// every tick and never finds — a warning per tick, for ever.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.Paused() || !got.HasBox() {
		t.Fatalf("run = %+v", got)
	}
	if err := s.ReleaseBox(t.Context(), "t1"); err != nil {
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

	got, err := s.ListActive(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusLaunching {
		t.Errorf("active = %+v, want the launching run", got)
	}
	seat, err := s.ListActiveForSeat(t.Context(), "swe")
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
	mustClaim(t, s, "t1")
	got, err := s.ListActive(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusResumed {
		t.Errorf("active = %+v; a resumed tail must stay visible to recovery", got)
	}

	// A finished run does not.
	if _, _, err := s.Finish(t.Context(), "t1", sandbox.Fence{}, sandbox.Active); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got, _ := s.ListActive(t.Context()); len(got) != 0 {
		t.Errorf("a finished run is still active: %+v", got)
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
		if err := s.MarkAwaiting(t.Context(), id,
			sandbox.Clarification{Question: "?" + id}); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", answerOnTheDM)
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's conversation is not this seat's.
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "other", answerOnTheDM); ok {
		t.Error("another seat's answer matched this seat's run")
	}
	// THE MATCH IS ON THE CONVERSATION, NOT ON THE BATCH BESIDE IT: a direct
	// message is one conversation however it is threaded, so a TOP-LEVEL
	// reply on the same DM line answers a question asked in a thread on it.
	// Compared on the partition this would miss, which is the same miss that
	// strands a question asked the other way round — see
	// testARunParkedOnATopLevelDMIsAnsweredInItsThread.
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1",
	}); !ok {
		t.Error("a top-level reply on the DM line did not answer the question asked on it")
	}
	// And the identity IS carried, so the resume knows where to report.
	if got.ConversationKey != "chat:D1" {
		t.Errorf("the run reports back to %q, want the DM line it was launched from",
			got.ConversationKey)
	}
	if got.Conversation() != "chat:D1" {
		t.Errorf("Conversation() = %q", got.Conversation())
	}
}

// A ROW FROM BEFORE THE SPLIT carries only the partition key, and a resume
// must still know where to report: nothing rewrites a parked run, and one
// waits for a person, so this row shape outlives any upgrade window.
func testARowWithNoIdentityReportsBackToItsPartition(t *testing.T, s sandbox.PendingStore) {
	old := run("t1")
	old.ConversationKey = ""
	mustLaunched(t, s, old)
	got, found, err := s.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.Conversation() != "chat:D1:root-1" {
		t.Errorf("a pre-split row reports back to %q, want the one key it carries — "+
			"an empty answer records no ledger entry at all", got.Conversation())
	}
}

// THE ENGINE'S OWN PROMPT SENDS THE ANSWER WHERE THE PARTITION CANNOT REACH.
//
// A run launched from a TOP-LEVEL direct message parks under the bare DM
// channel, because that is the partition a top-level burst coalesces on — and
// the chat prompt then tells the seat to reply AS A THREAD, so the person's
// answer arrives keyed on that thread. Compared on the partition the two
// strings never meet: the clarification the box is parked waiting for is
// silently never delivered, and the run sits until its pause TTL reaps it.
// Compared on the conversation it arrives, because a direct message is ONE
// conversation however it is threaded.
func testARunParkedOnATopLevelDMIsAnsweredInItsThread(t *testing.T, s sandbox.PendingStore) {
	top := run("t1")
	// What a top-level DM turn writes: the partition IS the bare channel,
	// and so is the conversation.
	top.PartitionKey, top.ConversationKey = "chat:D1", "chat:D1"
	mustLaunched(t, s, top)
	park(t, s, "t1")

	// The person's reply, in the thread the seat was told to open.
	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-1",
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok || got.TurnID != "t1" {
		t.Fatalf("the threaded reply matched %q (ok=%v); the answer to a question "+
			"asked from a top-level DM never reaches the run that asked it",
			got.TurnID, ok)
	}
}

// TWO QUESTIONS PARKED IN TWO THREADS OF ONE DIRECT MESSAGE ARE TOLD APART BY
// THE THREAD, not by which was asked last.
//
// A direct message is one conversation however it is threaded, which is what
// makes an answer reach the run that asked at all — and it means every run
// parked on that channel shares one identity. So a reply in thread root-1 is
// admitted by both runs, and picking the NEWEST resumes the one waiting in
// root-2: its question spliced with the answer to somebody else's, and the run
// that was actually answered still waiting. Both the arriving reference and
// each row carry the thread; preferring the row whose partition matches is the
// whole fix, and recency is what decides only between runs that agree on it.
func testTwoQuestionsOnOneDMAreToldApartByTheirThreads(t *testing.T, s sandbox.PendingStore) {
	first, second := run("t1"), run("t2")
	// The same DM line, two threads on it — and the one asked EARLIER is
	// the one the reply belongs to, so recency alone gets this wrong.
	first.PartitionKey = "chat:D1:root-1"
	second.PartitionKey = "chat:D1:root-2"
	second.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, first)
	mustLaunched(t, s, second)
	park(t, s, "t1")
	park(t, s, "t2")

	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-1",
	})
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t1" {
		t.Errorf("a reply in thread root-1 answered %s, the question parked in "+
			"root-2: the answer to one question resumed another run", got.TurnID)
	}
	// AND THE OTHER THREAD'S REPLY REACHES THE OTHER RUN, so what is under
	// test is the pairing rather than a preference for the older row.
	got, ok, err = s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-2",
	})
	if err != nil || !ok || got.TurnID != "t2" {
		t.Errorf("a reply in thread root-2 answered %q (ok=%v err=%v)", got.TurnID, ok, err)
	}
	// AND RECENCY IS STILL THE LAST WORD where the thread cannot decide: a
	// TOP-LEVEL reply on the DM line matches neither thread, and the person
	// is answering what they were just asked.
	got, ok, err = s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1",
	})
	if err != nil || !ok || got.TurnID != "t2" {
		t.Errorf("a top-level reply answered %q (ok=%v err=%v), want the most "+
			"recently parked question", got.TurnID, ok, err)
	}
}

// A run is answered by ITS conversation and no other. Matching on the seat
// alone would hand an unrelated message to whichever run happened to be
// waiting — and that run would treat it as the answer to its question.
func testAnAnswerOnAnotherConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D2", Partition: "chat:D2:root-9",
	}); ok {
		t.Error("a message on another conversation answered this run's question")
	}
}

// A PRE-SPLIT ROW IS STILL ANSWERABLE, in both readings of the one value it
// carries — and it has to be: nothing rewrites a parked run, one waits for a
// person, so this row shape outlives any upgrade window.
//
// Its value is the PARTITION its build derived, so comparing the arriving
// partition against it reproduces that build's own match exactly. It is
// compared against the identity as well, which is not a second spelling of
// the same rule: such a row parked from a top-level DM holds the bare
// channel, which is precisely what this build calls the identity, so reading
// it that way is what repairs the rows the defect already stranded.
func testAPreSplitRowIsStillAnswerable(t *testing.T, s sandbox.PendingStore) {
	// Parked from a DM thread by a build that had no identity to write.
	threaded := run("t1")
	threaded.ConversationKey = ""
	mustLaunched(t, s, threaded)
	park(t, s, "t1")
	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", answerOnTheDM)
	if err != nil || !ok || got.TurnID != "t1" {
		t.Fatalf("find = %q ok=%v err=%v; a row parked before the split stopped "+
			"being answerable at all", got.TurnID, ok, err)
	}

	// And one parked from a top-level DM, whose one value is the channel.
	toplevel := run("t2")
	toplevel.PartitionKey, toplevel.ConversationKey = "chat:D9", ""
	toplevel.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, toplevel)
	park(t, s, "t2")
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "swe", sandbox.ConversationRef{
		Identity: "chat:D9", Partition: "chat:D9:root-2",
	}); !ok {
		t.Error("a pre-split row parked from a top-level DM is still unanswerable")
	}
}

func testAnAnswerWithNoConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	// Matching by seat alone would hand an unrelated message to whichever
	// run happened to be waiting — and that run would treat it as the
	// answer to its question.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1",
		sandbox.Clarification{Question: "?"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "swe",
		sandbox.ConversationRef{}); ok {
		t.Error("a message with no conversation matched a parked run")
	}
	// AND NEITHER DOES A RUN WITH NONE, which is what a schedule tick or an
	// A2A wake launches: it stored no conversation any message could ever
	// reproduce, so every reply on every surface would otherwise be its
	// answer.
	keyless := run("t2")
	keyless.PartitionKey, keyless.ConversationKey = "", ""
	keyless.CreatedAt = base.Add(time.Minute)
	mustLaunched(t, s, keyless)
	park(t, s, "t2")
	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", answerOnTheDM)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if ok && got.TurnID == "t2" {
		t.Error("a run launched with no conversation was answered by a chat message")
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
		got, err := s.ListActive(t.Context())
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
		wg.Go(func() {
			<-start
			won, err := s.ExpirePause(t.Context(), "t1")
			if err != nil {
				t.Errorf("ExpirePause: %v", err)
				return
			}
			if won {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d reapers won the flip, want exactly 1 — each winner kills a box", got, racers)
	}
	got, _, err := s.Get(t.Context(), "t1")
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
		sandbox.StatusRunning, sandbox.StatusResumed, sandbox.StatusReseed,
	} {
		mustLaunched(t, s, run(status))
		if err := s.SetStatus(t.Context(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		won, err := s.ExpirePause(t.Context(), status)
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

	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1")); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	won, err := s.ExpirePause(t.Context(), "t1")
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
	ctx := t.Context()
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
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}

	won, err := s.ExpirePause(t.Context(), "t1")
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

// item is the work item a launching turn was charged to.
var item = types.WorkItem{Backend: types.WorkNative, ID: "task-7", Key: "ENG-7", Project: "ENG"}

func testWorkItemSurvivesParkAndResume(t *testing.T, s sandbox.PendingStore) {
	// THE RESUMED TURN READS ITS ITEM OFF THE ROW, because nothing else can
	// name it: the dispatch that resolved it is gone, and the answer that
	// resumes a parked run is a chat message naming no item. So the item has
	// to survive every write between the launch and the resume — the
	// suspension, the claim, the park, the answer's claim — or the second
	// half of the turn is charged to nothing.
	r := run("t1")
	r.WorkItem = &item
	mustLaunched(t, s, r)
	claimed := mustClaim(t, s, "t1")
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	resumed, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"))
	if err != nil || !won {
		t.Fatalf("claim the answer: won=%v err=%v", won, err)
	}
	for name, got := range map[string]sandbox.PendingRun{
		"claimed": claimed, "resumed": resumed, "read back": mustGet(t, s, "t1"),
	} {
		if got.WorkItem == nil || *got.WorkItem != item {
			t.Errorf("%s: work item = %+v, want %+v", name, got.WorkItem, item)
		}
	}
}

// audienceQuorum is a key no build has declared: the stand-in for whatever a
// newer build adds to the row next.
const audienceQuorum = "audience_quorum"

func testAudienceFieldsSurviveAStatusFlipByABuildThatDoesNotKnowThem(
	t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns,
) {
	// EVERY WRITE HERE IS A READ-MODIFY-WRITE OF THE WHOLE ROW, and a fleet
	// mid-upgrade has an older build doing them. The row is written the way
	// a NEWER build would write it — the audience fields this build knows,
	// plus one it has never heard of — and then this build, playing the
	// older half, drives every flip a parked run goes through. Each has to
	// hand the row back with all of it, or the first claim an older node
	// makes deletes what the newer one wrote.
	r := run("t1")
	r.WorkItem = &item
	r.AudienceHandles = []string{"ada", "grace"}
	r.AudienceFallback = true
	r.Status = sandbox.StatusLaunching
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields[audienceQuorum] = json.RawMessage(`{"min":2,"of":["ada","grace"]}`)
	body, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := runs.CreateSandboxRun(t.Context(), "t1", body); err != nil || !created {
		t.Fatalf("seed the newer build's row: created=%v err=%v", created, err)
	}

	check := func(step string) {
		t.Helper()
		record, found, err := runs.SandboxRun(t.Context(), "t1")
		if err != nil || !found {
			t.Fatalf("%s: read the raw row: found=%v err=%v", step, found, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &got); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if string(got[audienceQuorum]) != `{"min":2,"of":["ada","grace"]}` {
			t.Errorf("%s dropped a key this build does not know: %s = %s",
				step, audienceQuorum, got[audienceQuorum])
		}
		read := mustGet(t, s, "t1")
		if len(read.AudienceHandles) != 2 || !read.AudienceFallback ||
			read.WorkItem == nil || *read.WorkItem != item {
			t.Errorf("%s dropped the audience or the item: %+v %v %+v", step,
				read.AudienceHandles, read.AudienceFallback, read.WorkItem)
		}
	}

	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	check("the suspension")
	claimed := mustClaim(t, s, "t1")
	check("the claim")
	mustRelease(t, s, claimed)
	check("the release")
	mustClaim(t, s, "t1")
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{Question: "q"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	check("the park")
	if ok, err := s.ClaimOwnership(t.Context(), "t1", "node-b", 3); err != nil || !ok {
		t.Fatalf("move: %v %v", ok, err)
	}
	check("the ownership move")
}
