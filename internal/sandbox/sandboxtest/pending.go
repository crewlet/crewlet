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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
		{"AFinishedRunIsGoneForEveryReader", testAFinishedRunIsGoneForEveryReader},
		{"AFinishedRunIsNotRecreatedByALateWrite", testAFinishedRunIsNotRecreatedByALateWrite},
		{"ARunIsFinishedExactlyOnce", testARunIsFinishedExactlyOnce},
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
		{"AnAnswerWithNoConversationMatchesNothing", testAnAnswerWithNoConversationMatchesNothing},
		{"ListingsAreStable", testListingsAreStable},
		{"APauseExpiresExactlyOnce", testAPauseExpiresExactlyOnce},
		{"OnlyAParkedRunCanExpire", testOnlyAParkedRunCanExpire},
		{"AnAnsweredRunCannotBeExpiredUnderTheResume", testAnAnsweredRunCannotBeExpiredUnderTheResume},
		{"ExpiringAPauseClearsTheBoxInTheSameWrite", testExpiringAPauseClearsTheBoxInTheSameWrite},
		{"BridgeCallsAreAppendedInOrder", testBridgeCallsAreAppendedInOrder},
		{"BridgeCallsSurviveWithoutAFence", testBridgeCallsSurviveWithoutAFence},
		{"BridgeCallsForAMissingRunAreDropped", testBridgeCallsForAMissingRunAreDropped},
		{"EveryBridgedCallIsReadBackPastTheRowsBound", testEveryBridgedCallIsReadBackPastTheRowsBound},
		{"ABridgedCallPastTheRecordCeilingIsReadBackWhole", testABridgedCallPastTheRecordCeilingIsReadBackWhole},
		{"ABridgedCallsArgumentsAreKeptWholeOrMarkedWhereTheyAre", testABridgedCallsArgumentsAreKeptWholeOrMarkedWhereTheyAre},
		{"ABridgedCallsArgumentsOutrankItsOutput", testABridgedCallsArgumentsOutrankItsOutput},
		{"AWholeKeptInPartsIsNeverPagedOrCounted", testAWholeKeptInPartsIsNeverPagedOrCounted},
		{"BridgedCallsPageWithACursor", testBridgedCallsPageWithACursor},
		{"AFirstPageCarriesTheEndOfTheLog", testAFirstPageCarriesTheEndOfTheLog},
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
// suspension beside it gets, for the same reason — in the log this build
// reads, and in the row's list older builds read.
func testASecondLaunchDropsTheFirstRunsBridgedCalls(t *testing.T, s sandbox.PendingStore) {
	first := run("t-relaunch-bridge")
	mustBeginLaunch(t, s, first)
	if _, err := s.AppendBridgeCall(context.Background(), first.TurnID, sandbox.BridgeCall{
		Name: "submit_work", Args: `{"outcome":"delivered"}`, At: base,
	}); err != nil {
		t.Fatalf("AppendBridgeCall: %v", err)
	}
	if got := mustCalls(t, s, first.TurnID); len(got) != 1 {
		t.Fatalf("the first round recorded %d calls, want 1", len(got))
	}

	mustBeginLaunch(t, s, run("t-relaunch-bridge"))
	if got := mustCalls(t, s, first.TurnID); len(got) != 0 {
		t.Errorf("the second round inherited %d calls from the first: %+v", len(got), got)
	}
	got := mustGet(t, s, first.TurnID)
	if len(got.BridgeCalls) != 0 {
		t.Errorf("the row's list, which older builds read, inherited %d calls from the first round",
			len(got.BridgeCalls))
	}
	if got.BridgeCallsElided != 0 {
		t.Errorf("the elision count survived the relaunch: %d", got.BridgeCallsElided)
	}
}

// mustCalls reads a run's whole bridged-call log, the way a resume does.
//
// Every run this suite launches is one this build records, so its log drops
// nothing — and a log that reported a drop here would be reporting a gap the
// per-call records cannot have.
func mustCalls(t *testing.T, s sandbox.PendingStore, turnID string) []sandbox.BridgeCall {
	t.Helper()
	log, err := s.BridgeCalls(t.Context(), mustGet(t, s, turnID))
	if err != nil {
		t.Fatalf("BridgeCalls(%s): %v", turnID, err)
	}
	if log.Dropped != 0 || log.DroppedAfter != 0 {
		t.Errorf("BridgeCalls(%s) reports %d calls dropped after %d, from a log that drops nothing",
			turnID, log.Dropped, log.DroppedAfter)
	}
	return log.Calls
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

	finished, err := s.Finish(ctx, "t1", sandbox.Fence{})
	if err != nil || !finished {
		t.Fatalf("Finish = %v, %v; want the record deleted", finished, err)
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
	if _, found, err := s.FindAwaitingByConversation(ctx, "swe", "slack:C1"); err != nil || found {
		t.Errorf("an answer matched the question of a finished run: found %v, %v", found, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", tail); err != nil || won {
		t.Errorf("a finished run was claimed: won=%v err=%v", won, err)
	}
	// Two parties reaching the end of one run is ordinary, not an error.
	if again, err := s.Finish(ctx, "t1", sandbox.Fence{}); err != nil || again {
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
	if _, err := s.Finish(ctx, "t1", sandbox.Fence{}); err != nil {
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
			finished, err := s.Finish(t.Context(), "t1", sandbox.Fence{})
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
	if finished, err := s.Finish(t.Context(), "t1", sandbox.Fence{}); err != nil || !finished {
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
	if finished, err := s.Finish(t.Context(), "t1", stale); err != nil || finished {
		t.Errorf("a stale fence finished the run: %v, %v", finished, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a stale Finish (found %v, %v)", found, err)
	}
	if finished, err := s.Finish(t.Context(), "t1", sandbox.Fence{Owner: "node-b:2", Epoch: 7}); err != nil || !finished {
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
	if _, err := s.Finish(t.Context(), "t1", sandbox.Fence{}); err != nil {
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
	got, ok, err := s.FindAwaitingByConversation(t.Context(), "swe", "slack:C1")
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's thread is not this seat's.
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "other", "slack:C1"); ok {
		t.Error("another seat's answer matched this seat's run")
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
	if _, ok, _ := s.FindAwaitingByConversation(t.Context(), "swe", ""); ok {
		t.Error("a message with no conversation matched a parked run")
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

	got := mustCalls(t, s, r.TurnID)
	if len(got) != 3 {
		t.Fatalf("%d calls recorded: %+v", len(got), got)
	}
	want := []string{"read_page", "post_message", "read_page"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("call %d = %q, want %q", i, got[i].Name, name)
		}
		// NUMBERED, which is what a page's cursor is.
		if got[i].Seq != uint64(i+1) {
			t.Errorf("call %d is numbered %d, want %d", i, got[i].Seq, i+1)
		}
	}
	// The ARGUMENTS and the OUTCOME ride along, because a log of bare
	// names does not tell a reviewer whether the turn delivered anything.
	if got[0].Args != `{"id":1}` || got[0].Output != "read_page ok" {
		t.Errorf("the call does not carry what it did: %+v", got[0])
	}
	// STAMPED, so a reader can see the shape of a run that stalled.
	if got[0].At.IsZero() {
		t.Error("the call has no timestamp")
	}
	// And the row's list, which is what an older build on the same store
	// reads, carries the same calls.
	if row := mustGet(t, s, r.TurnID); len(row.BridgeCalls) != 3 {
		t.Errorf("the row's list for older builds holds %d calls, want 3", len(row.BridgeCalls))
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
	if got := mustCalls(t, s, r.TurnID); len(got) != 1 {
		t.Errorf("%d calls recorded", len(got))
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

// EVERY CALL IS READ BACK, however many the run made — and in particular a
// delivery in the middle of a long run.
//
// The run's row keeps a bounded list, for older builds, that drops its middle
// past [sandbox.MaxBridgeCalls]. A resume that read that list could not see a
// delivery made in the middle of a long run: its submission's citation of
// that delivery was refused, its delivery check counted nobody reached, and
// the turn could be sent round to post to a person a second time. The log a
// resume reads drops nothing, so the delivery is there.
func testEveryBridgedCallIsReadBackPastTheRowsBound(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-long")
	mustLaunched(t, s, r)

	total := sandbox.MaxBridgeCalls + 50
	delivery := total / 2
	for i := range total {
		call := sandbox.BridgeCall{Name: fmt.Sprintf("call-%03d", i)}
		if i == delivery {
			call = sandbox.BridgeCall{Name: "slack_post", Args: `{"channel":"C1","text":"done"}`,
				Output: `{"ts":"1718000000.000100"}`}
		}
		if ok, err := s.AppendBridgeCall(ctx, r.TurnID, call); err != nil || !ok {
			t.Fatalf("append %d = %v, %v", i, ok, err)
		}
	}

	got := mustCalls(t, s, r.TurnID)
	if len(got) != total {
		t.Fatalf("the log read back %d calls, want all %d", len(got), total)
	}
	if got[delivery].Name != "slack_post" || got[delivery].Output != `{"ts":"1718000000.000100"}` {
		t.Errorf("call %d = %+v, want the delivery the run made there", delivery, got[delivery])
	}
	for i, call := range got {
		if call.Seq != uint64(i+1) {
			t.Fatalf("call %d is numbered %d: the log is out of order or has a hole", i, call.Seq)
		}
	}

	// THE ROW'S LIST IS STILL OLDER BUILDS' BOUNDED VIEW. A peer on an
	// older build reads only this, so it keeps the shape it always had —
	// the first and last halves, and a count of the middle it dropped.
	row := mustGet(t, s, r.TurnID)
	if len(row.BridgeCalls) != sandbox.MaxBridgeCalls {
		t.Errorf("the row's list keeps %d calls, want %d", len(row.BridgeCalls), sandbox.MaxBridgeCalls)
	}
	if row.BridgeCallsElided != total-sandbox.MaxBridgeCalls {
		t.Errorf("the row counts %d dropped calls, want %d", row.BridgeCallsElided, total-sandbox.MaxBridgeCalls)
	}
}

// recorded is the run's calls as their RECORDS hold them: one page of the log,
// which reads each call's record and never the parts filed under it.
func recorded(t *testing.T, s sandbox.PendingStore, turnID string) []sandbox.BridgeCall {
	t.Helper()
	page, err := s.BridgeCallPage(t.Context(), mustGet(t, s, turnID), 0, 100)
	if err != nil {
		t.Fatalf("BridgeCallPage(%s): %v", turnID, err)
	}
	return append(page.Calls, page.End...)
}

// sameCall reports how a call read back differs from the call that was made,
// or "" when it is that call, every text byte for byte.
func sameCall(got, sent sandbox.BridgeCall) string {
	switch {
	case got.Name != sent.Name:
		return fmt.Sprintf("name %q, want %q", got.Name, sent.Name)
	case got.Args != sent.Args:
		return fmt.Sprintf("arguments of %d bytes, want the %d sent", len(got.Args), len(sent.Args))
	case got.Output != sent.Output:
		return fmt.Sprintf("output of %d bytes, want the %d returned", len(got.Output), len(sent.Output))
	case got.Failed != sent.Failed || !got.At.Equal(sent.At):
		return fmt.Sprintf("outcome %v at %s, want %v at %s", got.Failed, got.At, sent.Failed, sent.At)
	case got.WholeBytes != 0 || got.WholeParts != 0:
		return fmt.Sprintf("a reference to %d bytes in %d parts, on what should be the whole itself",
			got.WholeBytes, got.WholeParts)
	}
	return ""
}

// A CALL TOO LARGE FOR ONE RECORD IS READ BACK WHOLE, BYTE FOR BYTE.
//
// The coding agent in the box was handed the whole output, and the log a
// resume reads is what the resumed phase's record is built from: a text cut
// there is a fact nothing downstream ever holds again. So the whole is kept in
// parts under the call's record and the resume's read reassembles it — while
// the record, one message on the transport, holds the call FITTED and marked,
// with a reference to its whole, for the readers of the record alone.
func testABridgedCallPastTheRecordCeilingIsReadBackWhole(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-huge")
	mustLaunched(t, s, r)

	// Three-byte characters, so a byte cut through one would show; and two
	// records' worth of them, so the whole takes more than one part.
	sent := sandbox.BridgeCall{
		Name: "read_file", Args: `{"path":"big.txt"}`, Failed: true, At: base,
		Output: strings.Repeat("あ", sandbox.MaxBridgeCallBytes*2/3+1000),
	}
	if ok, err := s.AppendBridgeCall(ctx, r.TurnID, sent); err != nil || !ok {
		t.Fatalf("a call too large for one record was not recorded: %v, %v", ok, err)
	}
	got := mustCalls(t, s, r.TurnID)
	if len(got) != 1 {
		t.Fatalf("%d calls read back, want the one", len(got))
	}
	if diff := sameCall(got[0], sent); diff != "" {
		t.Errorf("the call read back is not the call made: %s", diff)
	}
	if got[0].Seq != 1 {
		t.Errorf("the call read back is numbered %d, want 1", got[0].Seq)
	}

	record := recorded(t, s, r.TurnID)[0]
	if !strings.HasSuffix(record.Output, "…") || !strings.HasPrefix(sent.Output, strings.TrimSuffix(record.Output, "…")) {
		t.Error("the record's output is not a marked head of the whole")
	}
	if len(record.Output) < sandbox.MaxBridgeCallBytes-1024 {
		t.Errorf("the record kept %d bytes of output, far short of the %d-byte record it had room in",
			len(record.Output), sandbox.MaxBridgeCallBytes)
	}
	if !utf8.ValidString(record.Output) {
		t.Error("the record's cut went through a character")
	}
	if record.Args != sent.Args {
		t.Errorf("the record's arguments were touched while the output alone could make room: %q", record.Args)
	}
	if record.WholeParts < 2 || record.WholeBytes <= sandbox.MaxBridgeCallBytes {
		t.Errorf("the record's reference = %d bytes in %d parts, want the whole's length and every part",
			record.WholeBytes, record.WholeParts)
	}
	assertRecordFits(t, record)

	// And a call that fits is left exactly as it was, with nothing to refer to.
	small := sandbox.BridgeCall{Name: "read_file", Output: "small…", At: base}
	if ok, err := s.AppendBridgeCall(ctx, r.TurnID, small); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	if diff := sameCall(mustCalls(t, s, r.TurnID)[1], small); diff != "" {
		t.Errorf("a call that fits was changed: %s", diff)
	}
	if diff := sameCall(recorded(t, s, r.TurnID)[1], small); diff != "" {
		t.Errorf("a call that fits was recorded changed: %s", diff)
	}
}

// assertRecordFits holds a call as its record holds it to the ceiling the
// record had.
func assertRecordFits(t *testing.T, call sandbox.BridgeCall) {
	t.Helper()
	raw, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	// The call as read carries its Seq, which the stored record does not
	// (the record's key does); the rest is what the record holds. HTML
	// escaping, which this encoder does and the store's does not, is not
	// something these calls carry.
	if len(raw) > sandbox.MaxBridgeCallBytes+len(`,"seq":1`) {
		t.Errorf("the recorded call encodes to %d bytes, past the %d a record may hold",
			len(raw), sandbox.MaxBridgeCallBytes)
	}
}

// ARGUMENTS ARE JSON, and a cut through JSON is text nothing can parse — so
// when they alone overfill the record, the record holds a marker in their
// place, in the field every reader of it shows, saying they are kept whole in
// the call's parts; and the resume reads them whole from there.
//
// AND THE OUTPUT KEEPS THE ROOM THEY GAVE BACK. The arguments are decided
// before the output is cut, so a small output beside them — "posted", after a
// nine-megabyte message body — is kept whole rather than cut for room the
// arguments were about to give up.
func testABridgedCallsArgumentsAreKeptWholeOrMarkedWhereTheyAre(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-huge-args")
	mustLaunched(t, s, r)

	sent := sandbox.BridgeCall{
		Name: "slack_post", Args: `{"text":"` + strings.Repeat("x", sandbox.MaxBridgeCallBytes) + `"}`,
		Output: "posted", At: base,
	}
	if ok, err := s.AppendBridgeCall(ctx, r.TurnID, sent); err != nil || !ok {
		t.Fatalf("a call whose arguments alone overfill a record was not recorded: %v, %v", ok, err)
	}
	record := recorded(t, s, r.TurnID)[0]
	if record.Args != sandbox.ArgsInParts(len(sent.Args)) {
		t.Errorf("the record's arguments = %.80q…, want the marker saying the %d bytes are in its parts",
			record.Args, len(sent.Args))
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(record.Args), &marker); err != nil || len(marker) != 1 {
		t.Errorf("the marker is not the one-member JSON object a reader decodes: %q (%v)", record.Args, err)
	}
	if record.Output != "posted" {
		t.Errorf("the record's output = %q, want %q kept whole: the record had room for it once the "+
			"arguments were set aside", record.Output, "posted")
	}
	if record.Name != "slack_post" {
		t.Errorf("the record lost the call's name: %+v", record)
	}
	assertRecordFits(t, record)

	if diff := sameCall(mustCalls(t, s, r.TurnID)[0], sent); diff != "" {
		t.Errorf("the call read back is not the call made: %s", diff)
	}
}

// ARGUMENTS THAT FIT ONCE THE OUTPUT IS CUT ARE KEPT in the record. Only a
// record over the ceiling with an output of nothing but its mark sets them
// aside: arguments are what the call DID, and an output is what it was told
// back. Read whole, the call has both.
func testABridgedCallsArgumentsOutrankItsOutput(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-both-large")
	mustLaunched(t, s, r)

	sent := sandbox.BridgeCall{
		Name:   "create_page",
		Args:   `{"body":"` + strings.Repeat("a", sandbox.MaxBridgeCallBytes/2) + `"}`,
		Output: strings.Repeat("b", sandbox.MaxBridgeCallBytes/2+1000), At: base,
	}
	if ok, err := s.AppendBridgeCall(ctx, r.TurnID, sent); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	record := recorded(t, s, r.TurnID)[0]
	if record.Args != sent.Args {
		t.Errorf("the record's arguments were not kept whole (%d bytes of %d) although the output "+
			"could make room for them", len(record.Args), len(sent.Args))
	}
	if !strings.HasSuffix(record.Output, "…") || !strings.HasPrefix(sent.Output, strings.TrimSuffix(record.Output, "…")) {
		t.Error("the record's output is not a marked head of the whole")
	}
	assertRecordFits(t, record)
	if diff := sameCall(mustCalls(t, s, r.TurnID)[0], sent); diff != "" {
		t.Errorf("the call read back is not the call made: %s", diff)
	}
}

// A WHOLE KEPT IN PARTS IS NEVER A CALL. Its parts are records in the same
// bucket, under the call's own; a log that paged or counted them would show a
// run making calls it never made, and a board's cursor would step through
// pieces of one output as though they were the run's work.
func testAWholeKeptInPartsIsNeverPagedOrCounted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-parts-paged")
	mustLaunched(t, s, r)
	for _, call := range []sandbox.BridgeCall{
		{Name: "c0"},
		{Name: "c1", Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes*2)},
		{Name: "c2"},
	} {
		if ok, err := s.AppendBridgeCall(ctx, r.TurnID, call); err != nil || !ok {
			t.Fatalf("append %s: %v, %v", call.Name, ok, err)
		}
	}
	var seen []string
	after := uint64(0)
	for range 5 {
		page, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), after, 1)
		if err != nil {
			t.Fatalf("BridgeCallPage: %v", err)
		}
		if page.Total != 3 || page.Between+len(page.Calls)+len(page.End) > 3 {
			t.Errorf("a page counts %d calls (%d between), want the 3 the run made", page.Total, page.Between)
		}
		seen = append(seen, names(page.Calls)...)
		if page.Next == 0 {
			seen = append(seen, names(page.End)...)
			break
		}
		after = page.Next
	}
	if want := []string{"c0", "c1", "c2"}; !slices.Equal(seen, want) {
		t.Errorf("paged %q, want %q", seen, want)
	}
	if got := names(mustCalls(t, s, r.TurnID)); !slices.Equal(got, []string{"c0", "c1", "c2"}) {
		t.Errorf("the log read whole = %q, want the three calls", got)
	}
}

// THE CURSOR. A dashboard pages a long log rather than asking for all of it in
// one answer, and every call has to be reachable exactly once.
func testBridgedCallsPageWithACursor(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-pages")
	mustLaunched(t, s, r)
	for i := range 5 {
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeCall{Name: fmt.Sprintf("c%d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	var seen []string
	after := uint64(0)
	for range 5 {
		page, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), after, 2)
		if err != nil {
			t.Fatalf("BridgeCallPage: %v", err)
		}
		if page.Total != 5 || page.Dropped != 0 {
			t.Errorf("total = %d, dropped = %d; want 5 and none", page.Total, page.Dropped)
		}
		if after != 0 && (len(page.End) != 0 || page.Between != 0) {
			t.Errorf("a later page carried the log's end: %d calls, %d between", len(page.End), page.Between)
		}
		for _, call := range page.Calls {
			seen = append(seen, call.Name)
		}
		if page.Next == 0 {
			break
		}
		after = page.Next
	}
	if want := []string{"c0", "c1", "c2", "c3", "c4"}; !slices.Equal(seen, want) {
		t.Errorf("paged %q, want %q", seen, want)
	}
	if _, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), 0, 0); err == nil {
		t.Error("a page with no room for a call was answered")
	}
}

// A FIRST PAGE SHOWS HOW THE LOG ENDS. A bridged run finishes by submitting,
// so a reader shown only a long run's first page would see neither its
// submission nor its last delivery, and could not tell that from a run that
// made neither. The first page carries the newest calls too, and counts the
// middle it leaves for paging to.
func testAFirstPageCarriesTheEndOfTheLog(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-ends")
	mustLaunched(t, s, r)
	for i := range 7 {
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeCall{Name: fmt.Sprintf("c%d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	page, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), 0, 2)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if got := names(page.Calls); !slices.Equal(got, []string{"c0", "c1"}) {
		t.Errorf("the first page = %q, want c0 and c1", got)
	}
	if got := names(page.End); !slices.Equal(got, []string{"c5", "c6"}) {
		t.Errorf("the log's end = %q, want c5 and c6, in the order they were made", got)
	}
	if page.Between != 3 || page.Next != page.Calls[1].Seq {
		t.Errorf("between = %d, next = %d; want the 3 calls in the middle, reached from %d",
			page.Between, page.Next, page.Calls[1].Seq)
	}

	// A first page that reaches the end carries nothing beside it.
	whole, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), 0, 7)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if len(whole.Calls) != 7 || len(whole.End) != 0 || whole.Between != 0 || whole.Next != 0 {
		t.Errorf("a first page holding the whole log = %d calls, %d at the end, %d between, next %d; "+
			"want all 7 and nothing else", len(whole.Calls), len(whole.End), whole.Between, whole.Next)
	}

	// A page whose end would overlap it carries only what it does not — and,
	// with the two together holding the whole log, no cursor: paging on from
	// one would fetch the end's calls a second time.
	near, err := s.BridgeCallPage(ctx, mustGet(t, s, r.TurnID), 0, 4)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if got := names(near.End); !slices.Equal(got, []string{"c4", "c5", "c6"}) || near.Between != 0 {
		t.Errorf("the end beside a 4-call first page = %q with %d between, want c4 to c6 and none",
			got, near.Between)
	}
	if near.Next != 0 {
		t.Errorf("a first page that, with its end, holds the whole log hands out cursor %d", near.Next)
	}
}

func names(calls []sandbox.BridgeCall) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Name)
	}
	return out
}
