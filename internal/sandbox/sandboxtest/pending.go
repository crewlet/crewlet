// Package sandboxtest is the code sandbox's two contract suites: the
// pending-run store's ([Run]) and a box's file reads ([Box]).
//
// THE PROPERTIES THAT MATTER IN [Run] ARE THE STORE'S. The at-most-once tail
// claim, the scoped release and the charge record it carries, the epoch fence
// and the launch that stamps it, an answer taken by a turn or let go of but
// never both, the box record's two halves moving together: each is a
// conditional write, not
// code around one, so a suite that ran only against a fake would assert the
// author's intent and nothing about the store. The one implementation is
// [sandbox.CoordStore]. The record operations it is built on are certified on
// both coordination backends by coordtest, and this suite certifies every
// conditional flip built on top of them.
package sandboxtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
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
		{"AParkedRunRecordsWhoItsAudienceIs", testAParkedRunRecordsWhoItsAudienceIs},
		{"ARequesterSurvivesTheLaunchThatRecordsIt", testARequesterSurvivesTheLaunchThatRecordsIt},
		{"ASecondLaunchKeepsTheBoxItWillReattachTo", testASecondLaunchKeepsTheBoxItWillReattachTo},
		{"ASecondLaunchDropsTheFirstSuspension", testASecondLaunchDropsTheFirstSuspension},
		{"ASecondLaunchDropsTheFirstRunsBridgedCalls", testASecondLaunchDropsTheFirstRunsBridgedCalls},
		{"ALaunchNeedsATurnID", testALaunchNeedsATurnID},
		{"ALaunchAnswersTheJobItOpened", testALaunchAnswersTheJobItOpened},
		{"ALaunchARowsNewerLeaseOutranksIsRefused", testALaunchARowsNewerLeaseOutranksIsRefused},
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
		{"AnEndingIsDecidedOnce", testAnEndingIsDecidedOnce},
		{"ADecidedRunTakesNoOtherWrite", testADecidedRunTakesNoOtherWrite},
		{"AnEndingSaysWhetherItHandsAnAnswerBack", testAnEndingSaysWhetherItHandsAnAnswerBack},
		{"ARefusedRelaunchIsAnError", testARefusedRelaunchIsAnError},
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
		{"AReleaseCountsAFailedCollectionOntoTheJob", testAReleaseCountsAFailedCollectionOntoTheJob},
		{"AReleaseNeverClearsAChargeRecord", testAReleaseNeverClearsAChargeRecord},
		{"ARefusedReleaseRecordsNoCharge", testARefusedReleaseRecordsNoCharge},
		{"OnlyALaunchClearsAChargeRecord", testOnlyALaunchClearsAChargeRecord},
		{"CollectPublishesThePhase", testCollectPublishesThePhase},
		{"LiveLaunchSaysWhetherAJobIsRunning", testLiveLaunchSaysWhetherAJobIsRunning},
		{"ParkingCarriesTheBranch", testParkingCarriesTheBranch},
		{"AQuestionIsParkedWithItsAnchor", testAQuestionIsParkedWithItsAnchor},
		{"OwnershipIsNotStolenByAnOlderLease", testOwnershipIsNotStolenByAnOlderLease},
		{"AStaleFenceCannotWrite", testAStaleFenceCannotWrite},
		{"ALaunchStampsTheLeaseThatLaunchedIt", testALaunchStampsTheLeaseThatLaunchedIt},
		{"ReleasingABoxClearsBothHalves", testReleasingABoxClearsBothHalves},
		{"ExecuteStateRoundTrips", testExecuteStateRoundTrips},
		{"ActiveIncludesResumed", testActiveIncludesResumed},
		{"AnAnswerFindsTheRunThatAsked", testAnAnswerFindsTheRunThatAsked},
		{"ARunParkedOnATopLevelDMIsAnsweredInItsThread", testARunParkedOnATopLevelDMIsAnsweredInItsThread},
		{"TwoQuestionsOnOneDMAreToldApartByTheirThreads", testTwoQuestionsOnOneDMAreToldApartByTheirThreads},
		{"AnAnswerOnAnotherConversationMatchesNothing", testAnAnswerOnAnotherConversationMatchesNothing},
		{"AnAnswerWithNoConversationMatchesNothing", testAnAnswerWithNoConversationMatchesNothing},
		{"ListingsAreStable", testListingsAreStable},
		{"TheFirstReplyRecordedIsTheAnswer", testTheFirstReplyRecordedIsTheAnswer},
		{"AnAnswerIsRecordedWithItsRoute", testAnAnswerIsRecordedWithItsRoute},
		{"AnAnswerIsRecordedOnlyOnTheQuestionThatAsked", testAnAnswerIsRecordedOnlyOnTheQuestionThatAsked},
		{"AnAnswerIsResumedFromItsRecord", testAnAnswerIsResumedFromItsRecord},
		{"ADeclinedAnswerReopensTheQuestionAndIsNeverRecordedAgain", testADeclinedAnswerReopensTheQuestionAndIsNeverRecordedAgain},
		{"ADeclineOwesItsCopiesInTheSameWrite", testADeclineOwesItsCopiesInTheSameWrite},
		{"WhatARowOwesTheSeatIsBoundedAndOutlivesNothing", testWhatARowOwesTheSeatIsBoundedAndOutlivesNothing},
		{"AnEndingRecordsTheReplyItLetsGoOnItsClaim", testAnEndingRecordsTheReplyItLetsGoOnItsClaim},
		{"ALetGoWaitsForTheCopiesAlreadyOwed", testALetGoWaitsForTheCopiesAlreadyOwed},
		{"AnAnswerThatCarriesNoCopyIsStillLetGo", testAnAnswerThatCarriesNoCopyIsStillLetGo},
		{"AnAnsweredRunsReplyIsLetGoOnlyUnderItsLicense", testAnAnsweredRunsReplyIsLetGoOnlyUnderItsLicense},
		{"ATakenAnswerIsNotLetGo", testATakenAnswerIsNotLetGo},
		{"ARowHoldingAnUntakenReplyIsNotDeleted", testARowHoldingAnUntakenReplyIsNotDeleted},
		{"AnEndingLicensedForAJobLeavesAnother", testAnEndingLicensedForAJobLeavesAnother},
		{"AClaimIsTakenUnderTheClaimantsLease", testAClaimIsTakenUnderTheClaimantsLease},
		{"ATurnTakesTheAnswerItsClaimDrives", testATurnTakesTheAnswerItsClaimDrives},
		{"ADeadClaimsAnswerIsRevivedOnceFencedAndCounted", testADeadClaimsAnswerIsRevivedOnceFencedAndCounted},
		{"AClaimItsOwnNodeGivesBackIsNotCounted", testAClaimItsOwnNodeGivesBackIsNotCounted},
		{"AnAnswerIsRecordedOnlyUnderTheSeatsLease", testAnAnswerIsRecordedOnlyUnderTheSeatsLease},
		{"ARevivalIsRefusedAnAnswerATurnTook", testARevivalIsRefusedAnAnswerATurnTook},
		{"ANewQuestionForgetsTheLastOnesAnswer", testANewQuestionForgetsTheLastOnesAnswer},
		{"APauseExpiresExactlyOnce", testAPauseExpiresExactlyOnce},
		{"OnlyAParkedRunCanExpire", testOnlyAParkedRunCanExpire},
		{"AnAnsweredRunCannotBeExpiredUnderTheResume", testAnAnsweredRunCannotBeExpiredUnderTheResume},
		{"AnAnswerWaitingOnItsResumeStillExpiresTheBox", testAnAnswerWaitingOnItsResumeStillExpiresTheBox},
		{"AClaimedAnswerIsNotExpired", testAClaimedAnswerIsNotExpired},
		{"ExpiringAPauseClearsTheBoxInTheSameWrite", testExpiringAPauseClearsTheBoxInTheSameWrite},
		{"BridgeCallsAreAppendedInOrder", testBridgeCallsAreAppendedInOrder},
		{"BridgeCallsSurviveWithoutAFence", testBridgeCallsSurviveWithoutAFence},
		{"BridgeCallsForAMissingRunAreDropped", testBridgeCallsForAMissingRunAreDropped},
		{"BridgeCallsDropTheMiddleNotTheStart", testBridgeCallsDropTheMiddleNotTheStart},
		{"ABridgedRunsSpendIsTheNewestTotal", testABridgedRunsSpendIsTheNewestTotal},
		{"ABridgedCallOutlivingItsJobIsNotTheNextJobs", testABridgedCallOutlivingItsJobIsNotTheNextJobs},
		{"ABridgedCallNamingNoJobIsNotRecorded", testABridgedCallNamingNoJobIsNotRecorded},
		{"ABridgedCallAfterItsJobsClaimStaysOnItsJob", testABridgedCallAfterItsJobsClaimStaysOnItsJob},
		{"AParkedJobsCondensationIsKeptForItsAnswer", testAParkedJobsCondensationIsKeptForItsAnswer},
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

// findAwaiting is the answer match as the coordinator makes it: the seat's runs
// as this store lists them, judged by [sandbox.Reply.Best] for a reply posted
// now — after every question the case parked. The rule is a value's
// ([sandbox.ConversationRef.Best]); what the store is certified on is handing
// over every candidate, and nothing else.
func findAwaiting(ctx context.Context, s sandbox.PendingStore, handle string,
	conv sandbox.ConversationRef,
) (sandbox.PendingRun, bool, error) {
	runs, err := s.ListActiveForSeat(ctx, handle)
	if err != nil {
		return sandbox.PendingRun{}, false, err
	}
	reply := events.New(types.ExternalNotification{Body: "use main"}, events.TraceContext{})
	got, ok := sandbox.Reply{Conv: conv, Events: []*events.Event{reply}}.Best(runs)
	return got, ok, nil
}

// answerOnTheDM is the reply to the question [run] parked on: the same DM
// line, in the same thread. BOTH VALUES, because a store is free to read
// either and a fixture that stated one would let it read that one alone.
var answerOnTheDM = sandbox.ConversationRef{
	Identity: "chat:D1", Partition: "chat:D1:root-1",
}

// mustBeginLaunch opens a launch and leaves the run where a launch leaves it:
// [sandbox.StatusLaunching], its job started and its conversation not yet
// written. It answers the job's name, as the store answered it.
func mustBeginLaunch(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) string {
	t.Helper()
	launch, err := s.BeginLaunch(t.Context(), r, sandbox.Fence{})
	if err != nil {
		t.Fatalf("begin launch %s: %v", r.TurnID, err)
	}
	return launch.LaunchID
}

// mustLaunched carries a run all the way through its launch: the row, then the
// suspension that opens it to the completion poll. It answers the job's name.
//
// BOTH HALVES, because a run that has only had the first is not one any tail
// acts on — the poll skips it and a claim refuses it — so a case that reached
// for the store's `create` alone would be asserting about a state the rest of
// the engine deliberately ignores.
func mustLaunched(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) string {
	t.Helper()
	launch := mustBeginLaunch(t, s, r)
	suspended, err := s.MarkSuspended(t.Context(), r.TurnID, suspension())
	if err != nil {
		t.Fatalf("mark suspended %s: %v", r.TurnID, err)
	}
	if !suspended {
		t.Fatalf("mark suspended %s: the launch did not open to the poll", r.TurnID)
	}
	return launch
}

// suspendedState is a stand-in for the serialized Execute conversation. The
// nineteen-digit argument is the value a lossy store gets wrong: it is past
// 2^53, so any decode of it into a float64 on the way through comes back as a
// different number.
const suspendedState = `{"messages":[{"Role":"assistant","ToolCalls":[{"ID":"call_1",` +
	`"Name":"run_sandbox","Arguments":{"row":1234567890123456789}}]}],` +
	`"pending_tool_call_id":"call_1","pending_tool_name":"run_sandbox",` +
	`"active_tool_names":["run_sandbox","activate_tool"],"iteration":2}`

// suspension is the write that parks [suspendedState].
func suspension() sandbox.Suspension {
	return sandbox.Suspension{State: json.RawMessage(suspendedState), Iteration: 2}
}

// everyJob is an ending's license for the given statuses under fence, on
// whatever job the run holds — what an ending that has already reclaimed the
// run's box takes ([sandbox.EveryLaunch]).
func everyJob(fence sandbox.Fence, whileIn []string) sandbox.License {
	return sandbox.License{Fence: fence, WhileIn: whileIn, Launch: sandbox.EveryLaunch}
}

// end decides an ending under license, announcing nothing, and deletes the
// record — the whole of an ending whose run owes the seat nothing. Its answer is
// the delete's: a decided ending whose run still owes the seat something is
// refused by Finish, with the row.
func end(ctx context.Context, s sandbox.PendingStore, turnID string,
	license sandbox.License,
) (sandbox.PendingRun, bool, error) {
	decided, ok, err := s.DecideEnding(ctx, turnID, sandbox.Decision{License: license})
	if err != nil || !ok {
		return sandbox.PendingRun{}, false, err
	}
	return s.Finish(ctx, turnID, decided.Ending.ID)
}

// claimedAnswerOn parks turnID on a question, records answer as its answer and
// claims it for the answer's resume — the resumed turn taking the answer where
// take says so — and hands back the launch it is on.
func claimedAnswerOn(t *testing.T, s sandbox.PendingStore, turnID string, answer sandbox.RecordedAnswer,
	take bool,
) string {
	t.Helper()
	ctx := t.Context()
	mustLaunched(t, s, run(turnID))
	park(t, s, turnID)
	launch := mustGet(t, s, turnID).LaunchID
	if _, ok, err := s.RecordAnswer(ctx, turnID, launch, answer, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer %s = %v, %v", turnID, ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume %s = %v, %v", turnID, ok, err)
	}
	if take {
		if ok, err := s.TakeAnswer(ctx, turnID, launch, sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("TakeAnswer %s = %v, %v", turnID, ok, err)
		}
	}
	return launch
}

// decide decides an ending under license and hands back the row it is recorded
// on, failing the case where none is.
func decide(t *testing.T, s sandbox.PendingStore, turnID string, license sandbox.License,
) sandbox.PendingRun {
	t.Helper()
	decided, ok, err := s.DecideEnding(t.Context(), turnID, sandbox.Decision{License: license})
	if err != nil || !ok || decided.Ending == nil {
		t.Fatalf("DecideEnding = %+v, %v, %v, want the ending recorded", decided.Ending, ok, err)
	}
	return decided
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
	launch := mustBeginLaunch(t, s, first)
	if _, err := s.AppendBridgeCall(context.Background(), first.TurnID, sandbox.BridgeAppend{
		Launch: launch,
		Call:   sandbox.BridgeCall{Name: "submit_work", Args: `{"outcome":"delivered"}`, At: base},
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
	if _, err := s.BeginLaunch(t.Context(), run(""), sandbox.Fence{}); err == nil {
		t.Error("a run with no turn id was persisted")
	}
}

// A LAUNCH ANSWERS THE JOB IT OPENED, on the first launch and on a relaunch
// alike: its name and the instant it began, exactly as the row records them.
// The answer is what an agent-mode run's bridge session is bound to before its
// box exists, so an answer that differed from the row — a stale name, or the
// previous job's — would file every call the box makes under a job that is not
// its own.
func testALaunchAnswersTheJobItOpened(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	var names []string
	for i := range 2 {
		opened, err := s.BeginLaunch(ctx, run("t-answered"), sandbox.Fence{})
		if err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
		row := mustGet(t, s, "t-answered")
		if opened.LaunchID == "" || opened.LaunchID != row.LaunchID {
			t.Fatalf("launch %d answered job %q, and the row holds %q", i+1, opened.LaunchID, row.LaunchID)
		}
		if facts := row.Launch; !facts.StartedAt.Equal(opened.Launch.StartedAt) || opened.Launch.StartedAt.IsZero() {
			t.Errorf("launch %d answered a start of %v, and the row records %v",
				i+1, opened.Launch.StartedAt, facts.StartedAt)
		}
		names = append(names, opened.LaunchID)
	}
	if names[0] == names[1] {
		t.Error("a relaunch answered the first job's name — the case exercises nothing")
	}
}

// A LAUNCH ON A ROW A NEWER LEASE HOLDS IS REFUSED, not reported open. The
// launch that went on from a silent no-op started a job on a row that never
// named it, in a box nothing would ever reclaim.
func testALaunchARowsNewerLeaseOutranksIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := mustLaunched(t, s, run("t-outranked"))
	if ok, err := s.ClaimOwnership(ctx, "t-outranked", "node-b:2", 7); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	opened, err := s.BeginLaunch(ctx, run("t-outranked"), sandbox.Fence{Owner: "node-a:1", Epoch: 3})
	if err == nil {
		t.Fatalf("a launch under a lease a newer one outranks was opened as job %q", opened.LaunchID)
	}
	if got := mustGet(t, s, "t-outranked"); got.LaunchID != first || got.Status != sandbox.StatusRunning {
		t.Errorf("the refused launch moved the row: job %q status %q, want %q %q",
			got.LaunchID, got.Status, first, sandbox.StatusRunning)
	}
}

func testALaunchingRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	// THE WINDOW THIS STATE EXISTS TO CLOSE. The job is started and can
	// finish at any moment, but the turn has not yet written the
	// conversation a resume re-enters. A completion claimed here would find
	// nothing to resume into and fail the whole turn.
	mustBeginLaunch(t, s, run("t1"))

	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("a launching run was claimed: won=%v err=%v", won, err)
	}
	// Nor by a tail that names the status outright: the closed set is the
	// store's to keep, not the caller's to widen.
	widened := sandbox.Tail{
		Launch: mustGet(t, s, "t1").LaunchID, From: []string{sandbox.StatusLaunching},
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", widened, sandbox.Fence{}); err != nil || won {
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
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
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
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err != nil || won {
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
			if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err == nil && won {
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
		sandbox.Clarification{Question: "which branch?", Audience: "requester", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{})
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
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
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

	settled, finished, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
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
	if _, found, err := findAwaiting(ctx, s, "swe", answerOnTheDM); err != nil || found {
		t.Errorf("an answer matched the question of a finished run: found %v, %v", found, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", tail, sandbox.Fence{}); err != nil || won {
		t.Errorf("a finished run was claimed: won=%v err=%v", won, err)
	}
	// Two parties reaching the end of one run is ordinary, not an error.
	if _, again, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || again {
		t.Errorf("a second Finish = %v, %v; want false and no error", again, err)
	}
}

// A write that arrives after the run ended is the ordinary shape of a box
// shutting down or a peer a moment behind. Each is a conditional flip on an
// existing record, so none of them may bring the record back: a resurrected
// row is a run that nothing will ever settle again.
func testAFinishedRunIsNotRecreatedByALateWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	if _, _, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil {
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
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "still there?", AskedAt: time.Now().UTC()})
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
			_, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
				Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}})
			return err
		},
	}
	for name, write := range late {
		// AN ATTACH IS THE ONE THAT SAYS SO: its caller is a launch about to
		// start a job in the box, and a row that is gone names no box.
		if err := write(); (err != nil) != (name == "AttachSandbox") {
			t.Errorf("%s on a finished run: %v; want the ordinary no-op, or the attach refused",
				name, err)
		}
		if _, found, err := s.Get(ctx, "t1"); err != nil || found {
			t.Fatalf("%s recreated a finished run's record (found %v, %v)", name, found, err)
		}
	}
}

// Every party that ends a run decides its ending and deletes the record it
// read. Racing ends must agree on ONE ending — every decider handed the same
// one, whatever it asked to announce — and exactly one of them deletes the
// record, so that the announcement is the same event whoever makes it.
func testARunIsFinishedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	const racers = 10
	var wins atomic.Int32
	var mu sync.Mutex
	endings := map[string]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			<-start
			decided, ok, err := s.DecideEnding(t.Context(), "t1", sandbox.Decision{
				License: everyJob(sandbox.Fence{}, sandbox.Active),
				Reason:  fmt.Sprintf("reason-%d", i),
			})
			if err != nil {
				t.Errorf("DecideEnding: %v", err)
				return
			}
			if !ok {
				return
			}
			mu.Lock()
			endings[decided.Ending.ID] = decided.Ending.Reason
			mu.Unlock()
			_, finished, err := s.Finish(t.Context(), "t1", decided.Ending.ID)
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
		t.Fatalf("%d of %d concurrent endings reported deleting the record, want exactly 1", got, racers)
	}
	if len(endings) != 1 {
		t.Fatalf("the racers were handed %d endings %v, want one: two decisions are two "+
			"announcements of one lost run", len(endings), endings)
	}
}

// A settle that could NOT read the row hands the decision to the store: it
// asks for the run to be ended only while it is still the status its claim
// left it in. So a row that moved on under it — a relaunch the resumed turn
// made, which takes the run back through launching and reuses the very box
// this settle would kill — has to survive the call, and a row still in the
// claim has to be ended by it.
func testAnEndingIsRefusedOutsideItsLicense(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	claimed := []string{sandbox.StatusResumed}

	if got, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, claimed)}); err != nil || ok {
		t.Errorf("a running run was ended under a claimed-only licence: %+v, %v, %v", got.Ending, ok, err)
	}
	if got, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a refused ending (found %v, %v)", found, err)
	} else if got.Status != sandbox.StatusRunning || got.Ending != nil {
		t.Errorf("a refused ending left the record at %q, ending %+v", got.Status, got.Ending)
	}
	// An empty licence ends nothing at all, which is the safe reading of a
	// caller that stated none.
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, nil)}); err != nil || ok {
		t.Errorf("an ending with no licence was decided: %v, %v", ok, err)
	}

	if _, _, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, ended, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, claimed)); err != nil || !ended {
		t.Errorf("the run the claim left behind was not ended: %v, %v", ended, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || found {
		t.Errorf("the claimed run still has a record (found %v, %v)", found, err)
	}
}

// AN ENDING IS DECIDED ONCE, and a second decider is handed the first: the same
// identity and the same announcement, which is what every attempt that finishes
// it publishes — so a run whose ending was decided by one party and finished by
// another is announced once, for the reason it was decided. And the record goes
// only for the ending named.
func testAnEndingIsDecidedOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	license := everyJob(sandbox.Fence{}, sandbox.Active)
	first, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: license, Reason: "collect_unreachable", Detail: "the box died", Reclaim: true,
	})
	if err != nil || !ok || first.Ending == nil || first.Ending.ID == "" || first.Ending.At.IsZero() {
		t.Fatalf("DecideEnding = %+v, %v, %v, want an ending with an identity", first.Ending, ok, err)
	}
	if !first.Ending.Reclaim || first.Ending.Reason != "collect_unreachable" {
		t.Fatalf("recorded %+v, want the decision's terms", first.Ending)
	}
	second, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: license, Reason: "abandoned_tail", Detail: "the node stopped",
	})
	if err != nil || !ok || second.Ending == nil || *second.Ending != *first.Ending {
		t.Fatalf("a second decision = %+v, %v, %v, want the first one handed back: %+v",
			second.Ending, ok, err, first.Ending)
	}
	if got := mustGet(t, s, "t1"); got.Ending == nil || *got.Ending != *first.Ending {
		t.Fatalf("the row records %+v, want the first decision", got.Ending)
	}
	if _, ended, err := s.Finish(ctx, "t1", "another-ending"); err != nil || ended {
		t.Fatalf("Finish for another ending = %v, %v, want refused", ended, err)
	}
	if _, ended, err := s.Finish(ctx, "t1", first.Ending.ID); err != nil || !ended {
		t.Fatalf("Finish for the decided ending = %v, %v", ended, err)
	}
}

// A RUN WHOSE ENDING IS DECIDED TAKES NO OTHER WRITE. The ending is announced
// before its record is deleted, so nothing may move the run off it in between:
// no claim, no take, no release, no relaunch, no answer, no park — a run
// announced lost and then resumed is a person told their work is gone while it
// carries on. A write whose caller acts on it landing is refused ALOUD
// ([sandbox.ErrRunEnding]); the rest are refused as their ordinary no. What
// every node may still write is what is true whoever finishes the ending — a
// lease stamped on the row, a call the box already made, a copy published.
func testADecidedRunTakesNoOtherWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	decided := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))

	refusedQuietly := map[string]func() (bool, error){
		"TakeAnswer": func() (bool, error) { return s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}) },
		"ReleaseClaim": func() (bool, error) {
			return s.ReleaseClaim(ctx, "t1", sandbox.Release{Launch: launch, To: sandbox.StatusAnswered})
		},
		"ClaimForResume": func() (bool, error) {
			_, ok, err := s.ClaimForResume(ctx, "t1", sandbox.Tail{Launch: launch, From: sandbox.Active}, sandbox.Fence{})
			return ok, err
		},
		"RecordAnswer": func() (bool, error) {
			_, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{})
			return ok, err
		},
		"DeclineAnswer": func() (bool, error) {
			_, ok, err := s.DeclineAnswer(ctx, "t1", launch, []string{"r1"}, nil, sandbox.Fence{})
			return ok, err
		},
		"ExpirePause":   func() (bool, error) { return s.ExpirePause(ctx, "t1") },
		"MarkSuspended": func() (bool, error) { return s.MarkSuspended(ctx, "t1", suspension()) },
	}
	for name, write := range refusedQuietly {
		if ok, err := write(); err != nil || ok {
			t.Errorf("%s on a run whose ending is decided = %v, %v, want refused", name, ok, err)
		}
	}
	refusedAloud := map[string]func() error{
		"BeginLaunch": func() error {
			_, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{})
			return err
		},
		"AttachSandbox": func() error { return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-2"}, sandbox.Fence{}) },
		"MarkAwaiting": func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "still there?", AskedAt: time.Now().UTC()})
		},
		"SetStatus": func() error { return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{}) },
	}
	for name, write := range refusedAloud {
		if err := write(); !errors.Is(err, sandbox.ErrRunEnding) {
			t.Errorf("%s on a run whose ending is decided = %v, want it refused with ErrRunEnding", name, err)
		}
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusResumed || got.LaunchID != launch || got.Answer == nil ||
		got.Answer.Taken() || got.SandboxID != "box-t1" || *got.Ending != *decided.Ending {
		t.Fatalf("the decided run moved: status %q launch %q answer %+v box %q ending %+v",
			got.Status, got.LaunchID, got.Answer, got.SandboxID, got.Ending)
	}
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 5); err != nil || !ok {
		t.Errorf("a lease stamped on a decided run = %v, %v, want it taken", ok, err)
	}
	if ok, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"},
	}); err != nil || !ok {
		t.Errorf("a bridged call recorded on a decided run = %v, %v, want it taken", ok, err)
	}
}

// AN ENDING SAYS WITH ITS DECISION WHETHER IT HANDS A PERSON'S ANSWER BACK, so
// its announcement says so whichever attempt makes it: an answer no turn took
// goes back, one a turn took does not — unless the ending says it went unused —
// and copies the row already owes go back too.
func testAnEndingSaysWhetherItHandsAnAnswerBack(t *testing.T, s sandbox.PendingStore) {
	carried := func(id string) sandbox.RecordedAnswer {
		a := answerOf(id, "use main")
		a.Events = []json.RawMessage{json.RawMessage(`{"id":"` + id + `"}`)}
		return a
	}
	claimedAnswerOn(t, s, "untaken", carried("untaken-r1"), false)
	if got := decide(t, s, "untaken", everyJob(sandbox.Fence{}, sandbox.Active)); !got.Ending.Returned {
		t.Errorf("an ending over an answer no turn took = %+v, want it handing the answer back", got.Ending)
	}
	claimedAnswerOn(t, s, "taken", carried("taken-r1"), true)
	if got := decide(t, s, "taken", everyJob(sandbox.Fence{}, sandbox.Active)); got.Ending.Returned {
		t.Errorf("an ending over an answer a turn took = %+v, want nothing handed back", got.Ending)
	}
	claimedAnswerOn(t, s, "unused", carried("unused-r1"), true)
	unused := everyJob(sandbox.Fence{}, sandbox.Active)
	unused.Unused = true
	if got := decide(t, s, "unused", unused); !got.Ending.Returned || !got.Ending.Unused {
		t.Errorf("an ending whose answer went unused = %+v, want it handing the answer back", got.Ending)
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

// A TAIL REQUEST ASKS ABOUT ONE JOB, and the store's record is what says
// whether that job is running: while it runs it is, a later launch on the same
// turn is a different job, a parked run is not running, and a run whose
// record is gone — settled and reclaimed — says so rather than answering with
// the empty output of a box nobody drives any more.
func testLiveLaunchSaysWhetherAJobIsRunning(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	first := mustGet(t, s, "t1").LaunchID

	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		!running || status != "" {
		t.Fatalf("a running launch read running=%v status=%q err=%v; want running", running, status, err)
	}
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", "some-other-job"); err != nil ||
		running || status != sandbox.StatusReplaced {
		t.Errorf("another job's launch id read running=%v status=%q err=%v; want %q",
			running, status, err, sandbox.StatusReplaced)
	}

	park(t, s, "t1")
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		running || status != sandbox.StatusAwaiting {
		t.Errorf("a parked launch read running=%v status=%q err=%v; want %q",
			running, status, err, sandbox.StatusAwaiting)
	}

	if _, finished, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !finished {
		t.Fatalf("Finish = %v, %v", finished, err)
	}
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		running || status != "" {
		t.Errorf("a finished run read running=%v status=%q err=%v; want not running, no record",
			running, status, err)
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

	if _, won, err := s.ClaimForResume(t.Context(), "t1", stale, sandbox.Fence{}); err != nil || won {
		t.Fatalf("the previous job's completion claimed the next job: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("a refused claim moved the row to %q", got.Status)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
		t.Errorf("the next job's own completion could not claim it: won=%v err=%v", won, err)
	}
}

func testACompletionDoesNotClaimAParkedRun(t *testing.T, s sandbox.PendingStore) {
	// A parked run is waiting on a person, and only their answer may take
	// it. A completion that finds it there is a duplicate of the one that
	// parked it.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{Question: "which branch?", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
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
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || won {
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
	if err := s.MarkAwaiting(t.Context(), "t2", sandbox.Clarification{Question: "which branch?", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	answered, won, err := s.ClaimForResume(t.Context(), "t2", answerTo(t, s, "t2"), sandbox.Fence{})
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
	if _, finished, err := end(t.Context(), s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !finished {
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
	return sandbox.Tail{Launch: mustGet(t, s, turnID).LaunchID, From: sandbox.Awaiting}
}

// mustClaim takes a running run's tail, as its job's completion would on the
// node holding the seat: under the lease the row carries.
func mustClaim(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	row := mustGet(t, s, turnID)
	got, won, err := s.ClaimForResume(t.Context(), turnID, completionOf(t, s, turnID),
		sandbox.Fence{Owner: row.Owner, Epoch: row.OwnerEpoch})
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

func testAReleaseCountsAFailedCollectionOntoTheJob(t *testing.T, s sandbox.PendingStore) {
	// THE BOUND ON A COLLECTION'S RETRIES LIVES ON THE JOB'S RECORD, in the
	// write that reopens the run to the retry: a count kept in memory would
	// grant every node, and every restart, a fresh allowance.
	mustLaunched(t, s, run("t1"))
	first := base.Add(3 * time.Second)
	for i, at := range []time.Time{first, first.Add(40 * time.Second)} {
		release := releaseOf(mustClaim(t, s, "t1"))
		release.CollectFailedAt = at
		if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || !released {
			t.Fatalf("release %d: released=%v err=%v", i, released, err)
		}
	}
	facts := mustGet(t, s, "t1").Launch
	if facts.CollectFailures != 2 || !facts.CollectFailingSince.Equal(first) {
		t.Errorf("the job's record = %d failures since %v; want 2 since the first, %v",
			facts.CollectFailures, facts.CollectFailingSince, first)
	}

	// A release that is not a failed collection counts nothing.
	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1").Launch.CollectFailures; got != 2 {
		t.Errorf("an ordinary hand-back moved the count to %d", got)
	}

	// A COLLECTION THAT READ THE BOX ENDS THE RUN: the bound is on
	// consecutive failures, and a later failure starts a run of its own.
	collected := releaseOf(mustClaim(t, s, "t1"))
	collected.Collected = true
	if released, err := s.ReleaseClaim(t.Context(), "t1", collected); err != nil || !released {
		t.Fatalf("release after a collection: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Errorf("a release after a collection that read the box left %+v", got)
	}
	later := first.Add(2 * time.Minute)
	failed := releaseOf(mustClaim(t, s, "t1"))
	failed.CollectFailedAt = later
	if released, err := s.ReleaseClaim(t.Context(), "t1", failed); err != nil || !released {
		t.Fatalf("release after the run ended: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 1 || !got.CollectFailingSince.Equal(later) {
		t.Errorf("the job's record = %d failures since %v; want 1 since %v",
			got.CollectFailures, got.CollectFailingSince, later)
	}

	// A release cannot say a collection both read the box and failed to.
	claimed := mustClaim(t, s, "t1")
	both := releaseOf(claimed)
	both.Collected, both.CollectFailedAt = true, later
	if _, err := s.ReleaseClaim(t.Context(), "t1", both); err == nil {
		t.Error("a release claiming a collection both read and failed was accepted")
	}
	mustRelease(t, s, claimed)

	// And the next launch is a new job, with an allowance of its own.
	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Errorf("a new launch inherited the last job's failed collections: %+v", got)
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
	launch := mustLaunched(t, s, run("t1"))
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
			claimed, _, err = s.ClaimForResume(ctx, "t1", tail, sandbox.Fence{Owner: "node-b:2", Epoch: 3})
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
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?", AskedAt: time.Now().UTC()})
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
			recorded, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
				Launch: launch, Call: sandbox.BridgeCall{Name: "read_page", At: base}})
			if err == nil && !recorded {
				return errors.New("the append was not recorded, so it exercised nothing")
			}
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
		AskedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Branch != "wip/swe/t1" ||
		got.Question != "which base branch?" || got.Audience != "requester" {
		t.Errorf("parked run = %+v", got)
	}
}

// A QUESTION IS PARKED WITH ITS ANCHOR, or not at all: the instant it was asked
// is what every answer is measured against, and a question parked without one
// is one no reply could be shown to answer. The refused park leaves the run as
// it was.
func testAQuestionIsParkedWithItsAnchor(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?"}); err == nil {
		t.Fatal("a park with no instant the question was asked at was accepted")
	}
	if got := mustGet(t, s, "t1"); got.Status == sandbox.StatusAwaiting || got.Question != "" {
		t.Fatalf("run %q asking %q, want the refused park to have written nothing", got.Status, got.Question)
	}
	asked := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?", AskedAt: asked}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting || !got.AskedAt.Equal(asked) {
		t.Fatalf("run %q asked at %v, want it parked at %v", got.Status, got.AskedAt, asked)
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

// A RUN IS OWNED BY THE LEASE THAT LAUNCHED IT: a fenced launch stamps its
// fence on the row, so the launching node's own writes — which carry that fence
// back off the row — are refused once the seat's next holder fences the row to
// a newer lease. Unstamped, the row sat at the zero epoch, and the zero fence
// constrains nothing: the node that lost the seat could release a claim the
// next holder had reaped. A relaunch restamps; an unfenced launch stamps
// nothing.
func testALaunchStampsTheLeaseThatLaunchedIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	lease := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, err := s.BeginLaunch(ctx, run("t1"), lease); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != lease.Owner || got.OwnerEpoch != lease.Epoch {
		t.Fatalf("owner %q epoch %d, want the launching lease %+v", got.Owner, got.OwnerEpoch, lease)
	}
	if ok, err := s.MarkSuspended(ctx, "t1", suspension()); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.CompletionTail(launch), lease)
	if err != nil || !ok || claimed.OwnerEpoch != lease.Epoch {
		t.Fatalf("claim = %v (epoch %d), %v, want it carrying the launching lease", ok,
			claimed.OwnerEpoch, err)
	}
	// THE NEXT HOLDER FENCES IT, and the launching node's release is refused.
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 4); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusRunning,
		Fence: sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
	}); err != nil || released {
		t.Fatalf("the launching node's release after the next holder fenced the row = %v, %v, "+
			"want refused", released, err)
	}
	// A RELAUNCH RESTAMPS, under the lease that relaunched.
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-c:1", Epoch: 6}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-c:1" || got.OwnerEpoch != 6 {
		t.Fatalf("owner %q epoch %d after the relaunch, want node-c:1 at 6", got.Owner, got.OwnerEpoch)
	}
	// AN UNFENCED LAUNCH STAMPS NOTHING.
	if _, err := s.BeginLaunch(ctx, run("t2"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch t2: %v", err)
	}
	if got := mustGet(t, s, "t2"); got.Owner != "" || got.OwnerEpoch != 0 {
		t.Fatalf("an unfenced launch stamped %q at %d", got.Owner, got.OwnerEpoch)
	}
}

// A RELAUNCH THE ROW REFUSES IS AN ERROR, never a launch that went ahead: a run a
// newer lease owns, or one whose ending is decided, is not reset — and a caller
// told nil would go on to start a job in a box the row does not record, killed
// under it by the ending or billed and named by nothing.
func testARefusedRelaunchIsAnError(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 5); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}
	before := mustGet(t, s, "t1")
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-a:1", Epoch: 3}); err == nil {
		t.Fatal("a relaunch under a lease the run's outranks answered as if it landed")
	}
	if got := mustGet(t, s, "t1"); got.LaunchID != before.LaunchID || got.Status != sandbox.StatusRunning {
		t.Fatalf("the refused relaunch moved the run: %q under %q", got.Status, got.LaunchID)
	}
	decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-b:1", Epoch: 5}); !errors.Is(err, sandbox.ErrRunEnding) {
		t.Fatalf("a relaunch of a run whose ending is decided = %v, want ErrRunEnding", err)
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
	// A STALE ATTACH IS REFUSED ALOUD: its caller is a launch about to start
	// a job in that box, which must abandon the box rather than run a job
	// the row does not name.
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "ghost"}, stale); err == nil {
		t.Fatal("a stale fence's attach answered as if it landed")
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a stale fence attached a box: %q", got.SandboxID)
	}
	// Nor may it END the run: a node whose lease moved deleting the record
	// its successor recovered strands the successor's box.
	if _, finished, err := end(t.Context(), s, "t1", everyJob(stale, sandbox.Active)); err != nil || finished {
		t.Errorf("a stale fence finished the run: %v, %v", finished, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a stale Finish (found %v, %v)", found, err)
	}
	if _, finished, err := end(t.Context(), s, "t1",
		everyJob(sandbox.Fence{Owner: "node-b:2", Epoch: 7}, sandbox.Active)); err != nil || !finished {
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
	//
	// BYTE FOR BYTE: the store carries the conversation and never reads it,
	// so anything it gives back other than what it was given — a number
	// re-read as a float64, a key re-sorted — is the store editing a turn.
	mustLaunched(t, s, run("t1"))
	got := mustGet(t, s, "t1")
	if string(got.ExecuteState) != suspendedState {
		t.Errorf("execute state = %s\nwant exactly %s", got.ExecuteState, suspendedState)
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
	if _, _, err := end(t.Context(), s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil {
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
			sandbox.Clarification{Question: "?" + id, AskedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	got, ok, err := findAwaiting(t.Context(), s, "swe", answerOnTheDM)
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's conversation is not this seat's.
	if _, ok, _ := findAwaiting(t.Context(), s, "other", answerOnTheDM); ok {
		t.Error("another seat's answer matched this seat's run")
	}
	// THE MATCH IS ON THE CONVERSATION, NOT ON THE BATCH BESIDE IT: a direct
	// message is one conversation however it is threaded, so a TOP-LEVEL
	// reply on the same DM line answers a question asked in a thread on it.
	// Compared on the partition this would miss, which is the same miss that
	// strands a question asked the other way round — see
	// testARunParkedOnATopLevelDMIsAnsweredInItsThread.
	if _, ok, _ := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1",
	}); !ok {
		t.Error("a top-level reply on the DM line did not answer the question asked on it")
	}
	// And the identity IS carried, so the resume knows where to report.
	if got.ConversationKey != "chat:D1" {
		t.Errorf("the run reports back to %q, want the DM line it was launched from",
			got.ConversationKey)
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
	got, ok, err := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
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

	got, ok, err := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
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
	got, ok, err = findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D1", Partition: "chat:D1:root-2",
	})
	if err != nil || !ok || got.TurnID != "t2" {
		t.Errorf("a reply in thread root-2 answered %q (ok=%v err=%v)", got.TurnID, ok, err)
	}
	// AND RECENCY IS STILL THE LAST WORD where the thread cannot decide: a
	// TOP-LEVEL reply on the DM line matches neither thread, and the person
	// is answering what they were just asked.
	got, ok, err = findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
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
	if _, ok, _ := findAwaiting(t.Context(), s, "swe", sandbox.ConversationRef{
		Identity: "chat:D2", Partition: "chat:D2:root-9",
	}); ok {
		t.Error("a message on another conversation answered this run's question")
	}
}

func testAnAnswerWithNoConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	// Matching by seat alone would hand an unrelated message to whichever
	// run happened to be waiting — and that run would treat it as the
	// answer to its question.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1",
		sandbox.Clarification{Question: "?", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, ok, _ := findAwaiting(t.Context(), s, "swe",
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
	got, ok, err := findAwaiting(t.Context(), s, "swe", answerOnTheDM)
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

	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
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

// A recorded answer waits on its resume, which waits on the seat's holder and
// its conditions — on a seat no node holds, for as long as nobody takes it.
// Its box is held for that whole wait, so the reaper expires it exactly as it
// would a parked run's: the answer and its debt survive, the box does not, and
// letting the answer go reopens the question on a run with nothing to
// reconnect to.
func testAnAnswerWaitingOnItsResumeStillExpiresTheBox(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if err := s.MarkBoxPaused(ctx, "t1", base); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if held, ok := mustGet(t, s, "t1").HeldSince(); !ok || !held.Equal(base) {
		t.Fatalf("HeldSince = %v, %v: an answered run's box is still being held", held, ok)
	}

	won, err := s.ExpirePause(ctx, "t1")
	if err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v: the box of an answer waiting on its resume was never reclaimed", won, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil || got.Answer.Text != "use main" {
		t.Fatalf("run = %q answer %+v, want the answer still recorded and owed", got.Status, got.Answer)
	}
	if got.SandboxID != "" || got.CommandID != "" || !got.PausedAt.IsZero() {
		t.Fatalf("the row still names the box being destroyed: %+v", got)
	}
	if _, held := got.HeldSince(); held {
		t.Fatal("the row still reads as holding a box")
	}

	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusReseed {
		t.Fatalf("a let-go answer reopened the question as %q, want %q — the box it was parked in is gone",
			got.Status, sandbox.StatusReseed)
	}
}

// An answered run whose resume already claimed it is not the reaper's.
func testAClaimedAnswerIsNotExpired(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if won, err := s.ExpirePause(ctx, "t1"); err != nil || won {
		t.Fatalf("ExpirePause = %v, %v: the reaper took a box the answer's resume is reconnecting to", won, err)
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
		AskedAt: time.Now().UTC(),
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
	launch := mustLaunched(t, s, r)

	for _, name := range []string{"read_page", "post_message", "read_page"} {
		recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch,
			Call:   sandbox.BridgeCall{Name: name, Args: `{"id":1}`, Output: name + " ok"},
		})
		if err != nil || !recorded {
			t.Fatalf("AppendBridgeCall(%s) = %v, %v", name, recorded, err)
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
	launch := mustLaunched(t, s, r)
	// Move the run under a NEWER owner, so the caller's own view of the
	// lease is stale by any measure.
	if ok, err := s.ClaimOwnership(ctx, r.TurnID, "node-b", 99); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}

	recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}})
	if err != nil || !recorded {
		t.Fatalf("a log append was refused by ownership: %v, %v", recorded, err)
	}
	if got := mustGet(t, s, r.TurnID); len(got.BridgeCalls) != 1 {
		t.Errorf("%d calls recorded", len(got.BridgeCalls))
	}
}

// A LATE CALL FROM A BOX THAT IS SHUTTING DOWN is the ordinary shape here, and
// it must not be an error: the caller cannot fail the box's call over a log
// row, so an answer of "not recorded" has to be as safe to ignore as one.
func testBridgeCallsForAMissingRunAreDropped(t *testing.T, s sandbox.PendingStore) {
	recorded, err := s.AppendBridgeCall(t.Context(), "never-existed", sandbox.BridgeAppend{
		Launch: "a-job-that-was", Call: sandbox.BridgeCall{Name: "read_page"}})
	if err != nil {
		t.Fatalf("a missing run was an error: %v", err)
	}
	if recorded {
		t.Error("an append onto no row reported being recorded")
	}
}

// THE MIDDLE IS WHAT GETS DROPPED. How a run began and how it ended are what
// explain it, and a log truncated to its last N loses the former entirely.
func testBridgeCallsDropTheMiddleNotTheStart(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-cap")
	launch := mustLaunched(t, s, r)

	total := sandbox.MaxBridgeCalls + 10
	for i := range total {
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch, Call: sandbox.BridgeCall{Name: fmt.Sprintf("call-%03d", i)},
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

// bridged is a session's running total after n calls, every figure distinct.
func bridged(n int) sandbox.EngineSpend {
	return sandbox.EngineSpend{
		Aux:     sandbox.AuxTokens{Input: 100 * n, Output: 10 * n, CacheRead: 40 * n, CacheWrite: 4 * n},
		Workers: n, WorkerInput: 1000 * n, WorkerOutput: 70 * n,
	}
}

// WHAT A BRIDGED RUN'S CALLS COST THE ENGINE RIDES ITS CALLS, AS THE NEWEST
// TOTAL. Each append carries the session's running total, and two calls that
// finish together race to the row — so the total an earlier call read can land
// after a later one's. The job's record keeps the largest of each figure, which
// within one session is the latest, and no append moves it backwards. The
// segment that resumes from the job pays it; without it on the row, a resume on
// another node or after a restart would have nothing to pay from.
func testABridgedRunsSpendIsTheNewestTotal(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-spend")
	launch := mustLaunched(t, s, r)

	// The third call's total lands before the second's.
	for _, n := range []int{1, 3, 2} {
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch, Call: sandbox.BridgeCall{Name: "refresh_memory"}, Spent: bridged(n),
		}); err != nil || !recorded {
			t.Fatalf("append %d = %v, %v; want it recorded on job %q", n, recorded, err, launch)
		}
	}
	if got := mustGet(t, s, r.TurnID).Launch.Bridged; got != bridged(3) {
		t.Fatalf("the job's bridged spend = %+v, want the newest total %+v", got, bridged(3))
	}
}

// A CALL FROM A JOB THAT IS OVER IS NOT THE NEXT JOB'S. Every append names the
// job its session was opened for, and once the row has moved on — a second
// launch on the same turn — a call still in flight from the first is not
// recorded, neither its log entry nor its spend, because the second job's
// resume would pay for it as its own. The second job starts with nothing
// bridged.
//
// BOTH SHAPES, and the second is the one a session that learned its job from
// the row got wrong: a session whose earlier call landed, and a session whose
// FIRST call is the late one — a delegated worker the CLI opened with, still
// running when the reviewer relaunched. Neither may land.
func testABridgedCallOutlivingItsJobIsNotTheNextJobs(t *testing.T, s sandbox.PendingStore) {
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{"after a call of its own landed", true},
		{"as its session's first call", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r := run("t-bridge-pin-" + fmt.Sprint(tc.landed))
			first := mustLaunched(t, s, r)
			if tc.landed {
				if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
					Launch: first, Call: sandbox.BridgeCall{Name: "read_page"}, Spent: bridged(1),
				}); err != nil || !recorded {
					t.Fatalf("the first job's own call = %v, %v", recorded, err)
				}
			}

			second := mustBeginLaunch(t, s, r)
			if second == first {
				t.Fatal("the second launch kept the first job's name — the case exercises nothing")
			}
			if got := mustGet(t, s, r.TurnID).Launch.Bridged; got != (sandbox.EngineSpend{}) {
				t.Fatalf("a fresh job starts with the last one's bridged spend: %+v", got)
			}

			recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
				Launch: first, Call: sandbox.BridgeCall{Name: "delegate"}, Spent: bridged(4)})
			if err != nil {
				t.Fatalf("a late call was an error: %v", err)
			}
			if recorded {
				t.Error("a call from a job that is over was recorded on the next one")
			}
			got := mustGet(t, s, r.TurnID)
			if len(got.BridgeCalls) != 0 || got.Launch.Bridged != (sandbox.EngineSpend{}) {
				t.Fatalf("the next job took a call from the last one: calls %+v, spend %+v",
					got.BridgeCalls, got.Launch.Bridged)
			}
		})
	}
}

// AN APPEND NAMING NO JOB IS RECORDED UNDER NONE. The store used to read an
// empty job as "whichever the row holds", which was how a late first call
// reached the next job; nothing in this build sends one, so an empty name is
// a caller's mistake and lands nowhere rather than somewhere.
func testABridgedCallNamingNoJobIsNotRecorded(t *testing.T, s sandbox.PendingStore) {
	r := run("t-bridge-unnamed")
	mustLaunched(t, s, r)
	recorded, err := s.AppendBridgeCall(t.Context(), r.TurnID, sandbox.BridgeAppend{
		Call: sandbox.BridgeCall{Name: "read_page"}, Spent: bridged(1)})
	if err != nil {
		t.Fatalf("an unnamed append was an error: %v", err)
	}
	got := mustGet(t, s, r.TurnID)
	if recorded || len(got.BridgeCalls) != 0 || got.Launch.Bridged != (sandbox.EngineSpend{}) {
		t.Fatalf("an append naming no job was recorded (%v): calls %+v, spend %+v",
			recorded, got.BridgeCalls, got.Launch.Bridged)
	}
}

// A CALL THAT FINISHES AFTER ITS JOB'S RESUME CLAIMED IT, and before any
// relaunch, stays on its own job: the row still holds that job, and the call is
// evidence — a resume that fails and hands its claim back is collected again,
// and that claim reads it. What the claim that already won read is what its
// segment pays, so this call's spend is not in it: a late call leaves the task
// short of the turn's cost, never past it.
func testABridgedCallAfterItsJobsClaimStaysOnItsJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-after-claim")
	launch := mustLaunched(t, s, r)
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}, Spent: bridged(1),
	}); err != nil || !recorded {
		t.Fatalf("the job's own call = %v, %v", recorded, err)
	}
	claimed := mustClaim(t, s, r.TurnID)

	recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "delegate"}, Spent: bridged(2)})
	if err != nil || !recorded {
		t.Fatalf("a call after the claim, before any relaunch = %v, %v; want it on its own job",
			recorded, err)
	}
	if got := claimed.Launch.Bridged; got != bridged(1) {
		t.Errorf("the claim read %+v, want what the job held when it was claimed: %+v", got, bridged(1))
	}
	got := mustGet(t, s, r.TurnID)
	if len(got.BridgeCalls) != 2 || got.Launch.Bridged != bridged(2) {
		t.Fatalf("the job's record after the late call: calls %d, spend %+v; want 2 and %+v",
			len(got.BridgeCalls), got.Launch.Bridged, bridged(2))
	}
}

// WHAT CONDENSING A PARKED JOB'S COLLECTION COST IS PARKED WITH ITS QUESTION,
// on the job's own record, for the resume the answer drives — which may run
// days later on another node, with nothing collected — exactly as the job's own
// tokens are. A second launch on the turn is a new job, and carries none of it.
func testAParkedJobsCondensationIsKeptForItsAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-park-condensed")
	mustLaunched(t, s, r)
	cost := sandbox.AuxTokens{Input: 900, Output: 60, CacheRead: 300}
	if err := s.MarkAwaiting(ctx, r.TurnID, sandbox.Clarification{
		Question: "which branch?", AskedAt: base, InputTokens: 5000, OutputTokens: 400, Condensed: cost,
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, r.TurnID).Launch.Condensed; got != cost {
		t.Fatalf("the parked job's condensation = %+v, want %+v", got, cost)
	}
	mustBeginLaunch(t, s, run("t-park-condensed"))
	if got := mustGet(t, s, r.TurnID).Launch.Condensed; got != (sandbox.AuxTokens{}) {
		t.Fatalf("a new job carries the last one's condensation: %+v", got)
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
		AskedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	resumed, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{})
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

func testAParkedRunRecordsWhoItsAudienceIs(t *testing.T, s sandbox.PendingStore) {
	// THE PARK WRITES WHOM THE QUESTION IS PUT TO, in the same write as the
	// question — the label alone ("manager") answered nobody's "what is
	// waiting on me" — and a new job opened on the row takes it away with
	// the question, because a question that is gone waits on nobody.
	mustBeginLaunch(t, s, run("t1"))
	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	mustClaim(t, s, "t1")
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Question: "which base branch?", Audience: "manager",
		Answerers: sandbox.Audience{Handles: []string{"founder", "cto"}, Fallback: true},
		AskedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Audience != "manager" || len(got.AudienceHandles) != 2 ||
		got.AudienceHandles[0] != "founder" || got.AudienceHandles[1] != "cto" ||
		!got.AudienceFallback {
		t.Fatalf("the park recorded audience %q handles %v fallback %v, want "+
			"manager, [founder cto], true", got.Audience, got.AudienceHandles, got.AudienceFallback)
	}

	mustBeginLaunch(t, s, run("t1"))
	again := mustGet(t, s, "t1")
	if len(again.AudienceHandles) != 0 || again.AudienceFallback {
		t.Errorf("a new job kept the last question's audience %v (fallback %v) — "+
			"the run would read as waiting on people nobody is asking",
			again.AudienceHandles, again.AudienceFallback)
	}
}

func testARequesterSurvivesTheLaunchThatRecordsIt(t *testing.T, s sandbox.PendingStore) {
	// WHO WOKE THE TURN is written by the launch and read by the park,
	// which runs when the job finishes — so every write between the two
	// has to hand it on.
	r := run("t1")
	r.Requester = "ada"
	mustBeginLaunch(t, s, r)
	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	claimed := mustClaim(t, s, "t1")
	if claimed.Requester != "ada" {
		t.Fatalf("the claim read requester %q, want ada", claimed.Requester)
	}
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{Question: "q", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Requester != "ada" {
		t.Errorf("the park left requester %q, want ada", got.Requester)
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
	// THE PARK IS THE WRITE THAT OWNS THE AUDIENCE, so it states the same
	// resolution the newer build parked with; what it must not drop is the
	// key it has never heard of, and the item.
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Question:  "q",
		Answerers: sandbox.Audience{Handles: []string{"ada", "grace"}, Fallback: true},
		AskedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	check("the park")
	if ok, err := s.ClaimOwnership(t.Context(), "t1", "node-b", 3); err != nil || !ok {
		t.Fatalf("move: %v %v", ok, err)
	}
	check("the ownership move")
}

func testCollectPublishesThePhase(t *testing.T, s sandbox.PendingStore) {
	// THE DURABLE HALF OF PUBLISHING A COLLECTED RUN'S PHASE ONCE, and of
	// what that record states. The launch dates the job on the store's own
	// clock and names its model; the suspension files it under the
	// executor's iteration; the release that reopens the run to a retry
	// carries whether its record went out, and a later release that says
	// nothing never takes that back. A second launch is a second job with a
	// record of its own.
	before := time.Now().UTC()
	r := run("t1")
	r.Launch = sandbox.LaunchRecord{Model: "claude-sonnet-5",
		// Not the caller's to choose: the store dates the job.
		StartedAt: base, Published: true, Iteration: 9}
	mustLaunched(t, s, r)
	got := mustGet(t, s, "t1")
	facts := got.Launch
	if facts.StartedAt.Before(before) || facts.StartedAt.After(time.Now().UTC()) {
		t.Errorf("the job is dated %s, not the instant it launched", facts.StartedAt)
	}
	if facts.Model != "claude-sonnet-5" || facts.Iteration != suspension().Iteration || facts.Published {
		t.Errorf("the launch record = %+v, want the model, iteration %d and nothing published",
			facts, suspension().Iteration)
	}

	claimed := mustClaim(t, s, "t1")
	release := releaseOf(claimed)
	release.Published = true
	if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}
	retry := mustClaim(t, s, "t1")
	if !retry.Launch.Published {
		t.Fatal("the retry's claim came back without the publish the first attempt made")
	}
	mustRelease(t, s, retry)
	if !mustGet(t, s, "t1").Launch.Published {
		t.Error("a release that carried no publish erased the record of one")
	}

	mustBeginLaunch(t, s, run("t1"))
	next := mustGet(t, s, "t1")
	if f := next.Launch; f.Published || f.Iteration != 0 || f.StartedAt.Before(facts.StartedAt) {
		t.Errorf("the second job inherited the first one's record: %+v", f)
	}
}

// answerOf is a recorded answer made of one delivery.
func answerOf(id, text string) sandbox.RecordedAnswer {
	return sandbox.RecordedAnswer{Text: text, Via: types.AnswerViaChat, EventIDs: []string{id}, RecordedAt: base}
}

// copyOf is the hand-back a decline of delivery id owes the seat: one copy,
// under id + "-copy".
func copyOf(id string) sandbox.HandedBack {
	return sandbox.HandedBack{ID: id + "-copy", Original: id,
		Event: json.RawMessage(`{"id":"` + id + `-copy"}`)}
}

// decline lets go of the one-delivery answer id on t1, owing its copy.
func decline(t *testing.T, s sandbox.PendingStore, launch, id string) (bool, error) {
	t.Helper()
	_, ok, err := s.DeclineAnswer(t.Context(), "t1", launch, []string{id},
		[]sandbox.HandedBack{copyOf(id)}, sandbox.Fence{})
	return ok, err
}

// THE FIRST REPLY IS THE ANSWER, and the store is where that is decided: two
// replies racing for one question — on two nodes across a handoff — both read
// it waiting, and exactly one record lands.
func testTheFirstReplyRecordedIsTheAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range racers {
		wg.Go(func() {
			_, ok, err := s.RecordAnswer(ctx, "t1", launch,
				answerOf(fmt.Sprintf("reply-%d", i), fmt.Sprintf("answer %d", i)), sandbox.Fence{})
			if err != nil {
				t.Errorf("RecordAnswer: %v", err)
				return
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d replies were recorded as the answer to one question, want 1", won)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q with answer %+v, want it answered", got.Status, got.Answer)
	}
	if got.Answer.From != sandbox.StatusAwaiting {
		t.Errorf("the record took the run out of %q, want %q", got.Answer.From, sandbox.StatusAwaiting)
	}
	// AND A LATER ONE FINDS IT ANSWERED, whoever asks.
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("late", "late"), sandbox.Fence{}); err != nil || ok {
		t.Errorf("a reply after the answer = %v, %v, want refused", ok, err)
	}
	// AN ANSWERED QUESTION IS NOT WAITING, so no listing offers it a reply.
	if _, found, _ := findAwaiting(ctx, s, "swe", answerOnTheDM); found {
		t.Error("a question that has its answer was matched to another reply")
	}
}

// AN ANSWER IS RECORDED WITH THE ROUTE IT CAME BY, on either route, and one that
// names none is refused: the route is what tells the resumed turn who answered,
// what the answer's record says, and what a copy of it becomes when it is let
// go of — an ordinary message, or an answer by turn with no ordinary form.
func testAnAnswerIsRecordedWithItsRoute(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	routeless := answerOf("r1", "use main")
	routeless.Via = ""
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, routeless, sandbox.Fence{}); err == nil || ok {
		t.Fatalf("an answer naming no route = %v, %v, want refused", ok, err)
	}
	byTurn := answerOf("r1", "use main")
	byTurn.Via, byTurn.By, byTurn.BySeat = types.AnswerViaOperator, "founder-token", "founder"
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, byTurn, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("an answer by turn = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1").Answer
	if got == nil || got.Via != types.AnswerViaOperator || got.By != "founder-token" || got.BySeat != "founder" {
		t.Fatalf("recorded %+v, want the answer by turn with the credential and the person it names", got)
	}
}

// A RECORD NAMES ITS QUESTION: the launch that asked, while it waits. One for
// a job that replaced the asker, or for a run that is not waiting, is refused.
func testAnAnswerIsRecordedOnlyOnTheQuestionThatAsked(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer to a RUNNING run = %v, %v, want refused", ok, err)
	}
	park(t, s, "t1")
	if _, ok, err := s.RecordAnswer(ctx, "t1", "another-launch", answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer naming another launch = %v, %v, want refused", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "gone", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer to a missing run = %v, %v, want false and no error", ok, err)
	}
	if _, _, err := s.RecordAnswer(ctx, "t1", launch, sandbox.RecordedAnswer{Text: "x"}, sandbox.Fence{}); err == nil {
		t.Fatal("an answer naming no delivery was recorded")
	}
	// A RESEED IS STILL WAITING: the box was reaped, the question was not.
	if err := s.SetStatus(ctx, "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if got, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok ||
		got.Answer.From != sandbox.StatusReseed {
		t.Fatalf("an answer to a reseeded run = %+v, %v, %v, want it recorded from reseed",
			got.Answer, ok, err)
	}
}

// THE RESUME CLAIMS THE RECORD, not the question: an answered run is claimed
// out of answered, and a claim that is handed back puts it there again — the
// answer still on it, owed the same resume.
func testAnAnswerIsResumedFromItsRecord(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	// A CLAIM OUT OF THE OPEN QUESTION finds it answered, and takes nothing.
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.Tail{Launch: launch, From: sandbox.Awaiting}, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a claim out of the open question took one that already has its answer: %v, %v", ok, err)
	}
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok || claimed.ClaimedFrom != sandbox.StatusAnswered {
		t.Fatalf("claim = %v from %q, %v, want it claimed from answered", ok, claimed.ClaimedFrom, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusAnswered,
	}); err != nil || !released {
		t.Fatalf("ReleaseClaim = %v, %v", released, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil || got.Answer.Text != "use main" {
		t.Fatalf("run = %q with answer %+v, want the answer still owed", got.Status, got.Answer)
	}
}

// A DECLINED ANSWER REOPENS THE QUESTION, and the reply it was is declined for
// good — with its copy — so it can never be recorded against it again.
func testADeclinedAnswerReopensTheQuestionAndIsNeverRecordedAgain(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	// Only the answer it holds is let go of.
	if ok, err := decline(t, s, launch, "other"); err != nil || ok {
		t.Fatalf("declining an answer the run does not hold = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 0 {
		t.Fatalf("a refused decline still recorded copies as owed: %+v", got.HandBack)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Answer != nil ||
		!slices.Equal(got.DeclinedAnswers, []string{"r1", "r1-copy"}) {
		t.Fatalf("run = %q answer %+v declined %v, want the question open again with r1 "+
			"and its copy declined", got.Status, got.Answer, got.DeclinedAnswers)
	}
	for _, id := range []string{"r1", "r1-copy"} {
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf(id, "use main"), sandbox.Fence{}); err != nil || ok {
			t.Errorf("a declined reply %s was recorded again: %v, %v", id, ok, err)
		}
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Errorf("a new reply to the reopened question = %v, %v, want it recorded", ok, err)
	}
	published(t, s, "r1-copy")
	if ok, err := decline(t, s, launch, "r2"); err != nil || !ok {
		t.Fatalf("DeclineAnswer: %v, %v", ok, err)
	}
	// BOUNDED, newest kept.
	for i := range 20 {
		id := fmt.Sprintf("r%d", i+3)
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf(id, "x"), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("RecordAnswer %s = %v, %v", id, ok, err)
		}
		published(t, s, fmt.Sprintf("r%d-copy", i+2))
		if ok, err := decline(t, s, launch, id); err != nil || !ok {
			t.Fatalf("DeclineAnswer %s = %v, %v", id, ok, err)
		}
	}
	got = mustGet(t, s, "t1")
	if len(got.DeclinedAnswers) != 16 || got.DeclinedAnswers[15] != "r22-copy" {
		t.Errorf("declined = %v, want the newest 16", got.DeclinedAnswers)
	}

	// AND THE NEWEST DECLINE WHOLE, however many deliveries its answer was:
	// a copy of it whose id fell off would answer the question that let it
	// go, and circle the run it already failed to reach.
	published(t, s, "r22-copy")
	var batch []string
	var copies []sandbox.HandedBack
	for i := range 12 {
		id := fmt.Sprintf("b%d", i)
		batch = append(batch, id)
		copies = append(copies, copyOf(id))
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, sandbox.RecordedAnswer{
		Text: "a long batch", Via: types.AnswerViaChat, EventIDs: batch, RecordedAt: base}, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer of a batch = %v, %v", ok, err)
	}
	if _, ok, err := s.DeclineAnswer(ctx, "t1", launch, batch, copies, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("DeclineAnswer of a batch = %v, %v", ok, err)
	}
	got = mustGet(t, s, "t1")
	for _, copied := range copies {
		if !slices.Contains(got.DeclinedAnswers, copied.ID) || !slices.Contains(got.DeclinedAnswers, copied.Original) {
			t.Fatalf("declined = %v: a decline of %d deliveries lost %s or its copy",
				got.DeclinedAnswers, len(batch), copied.Original)
		}
	}
}

// published clears one copy a decline owed t1, the way a publisher does once
// the copy is out.
func published(t *testing.T, s sandbox.PendingStore, id string) {
	t.Helper()
	if ok, err := s.ClearHandBack(t.Context(), "t1", []string{id}); err != nil || !ok {
		t.Fatalf("ClearHandBack(%s) = %v, %v", id, ok, err)
	}
}

// WHAT A ROW OWES THE SEAT IS BOUNDED BY ONE DECLINE, and outlives nothing it
// has not handed back. A second decline, and an ending's let-go, are refused
// while earlier copies are owed — the answer stays recorded and owed, and nothing
// grows — and a run that owes copies is not deleted until they are cleared,
// because nothing reads a deleted row again.
func testWhatARowOwesTheSeatIsBoundedAndOutlivesNothing(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r1 = %v, %v", ok, err)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer r1 = %v, %v", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r2 = %v, %v", ok, err)
	}

	// A SECOND DECLINE WAITS for the first one's copies.
	if ok, err := decline(t, s, launch, "r2"); !errors.Is(err, sandbox.ErrHandBackOwed) || ok {
		t.Fatalf("a decline over owed copies = %v, %v, want refused with ErrHandBackOwed", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
		!slices.Equal(got.Answer.EventIDs, []string{"r2"}) || len(got.HandBack) != 1 {
		t.Fatalf("run = %q answer %+v hand-back %+v, want r2 still recorded and only r1's copy "+
			"owed", got.Status, got.Answer, got.HandBack)
	}

	// AN ENDING OUTSIDE ITS LICENSE is refused as one, not as a debt.
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, nil)}); err != nil || ok {
		t.Fatalf("an unlicensed ending = %v, %v, want a plain refusal", ok, err)
	}
	decided := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	ending := decided.Ending.ID

	// NOR IS THE RUN DELETED while it owes them, and it says what it owes.
	owing, ended, err := s.Finish(ctx, "t1", ending)
	if !errors.Is(err, sandbox.ErrHandBackOwed) || ended ||
		len(owing.HandBack) != 1 || owing.HandBack[0].ID != "r1-copy" {
		t.Fatalf("Finish over owed copies = %v, %+v, %v, want refused with the copies it owes",
			ended, owing.HandBack, err)
	}
	// NOR LET GO OF ANOTHER until they are out.
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); !errors.Is(err, sandbox.ErrHandBackOwed) || ok {
		t.Fatalf("a let-go over owed copies = %v, %v, want refused with ErrHandBackOwed", ok, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the run owing its seat a reply was deleted: %v, %v", found, err)
	}

	// ONCE HANDED BACK, the reply r2 is still owed, and then let go of.
	published(t, s, "r1-copy")
	if _, ended, err := s.Finish(ctx, "t1", ending); !errors.Is(err, sandbox.ErrAnswerOwed) || ended {
		t.Fatalf("Finish over the reply the run still holds = %v, %v, want refused with ErrAnswerOwed", ended, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); err != nil || !ok {
		t.Fatalf("the let-go of r2 once r1's copy was out = %v, %v", ok, err)
	}
	published(t, s, "r2-copy")
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish once nothing is owed = %v, %v", ended, err)
	}
}

// letGoOf is the let-go, by the decided ending named, of the answer made of
// ids, owing one copy of each.
func letGoOf(ending string, ids ...string) sandbox.LetGo {
	var copies []sandbox.HandedBack
	for _, id := range ids {
		copies = append(copies, copyOf(id))
	}
	return sandbox.LetGo{Ending: ending, Answer: ids, HandBack: copies}
}

// claimOnly is the license a claim's own ending takes.
var claimOnly = []string{sandbox.StatusResumed}

// A DECIDED ENDING LETS THE REPLY GO, in one write: only as a step of that
// ending, of the answer it holds, and only one decline's worth. The answer leaves
// the row in the same write, so a let-go repeated after a crash finds nothing
// left to let go of; and no turn can take it afterwards, nor any release give
// it to a resume — the run's ending is decided.
func testAnEndingRecordsTheReplyItLetsGoOnItsClaim(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	lease := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), lease); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	owe := func(letGo sandbox.LetGo) (sandbox.PendingRun, bool, error) {
		return s.OweHandBack(ctx, "t1", letGo)
	}
	// NOT BEFORE AN ENDING IS DECIDED: a let-go is a step of one.
	if _, ok, err := owe(letGoOf("no-ending", "r1")); err != nil || ok {
		t.Fatalf("OweHandBack on a run with no ending decided = %v, %v, want refused", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{Fence: lease, WhileIn: claimOnly, Launch: launch}).Ending.ID
	if _, ok, err := owe(letGoOf("another-ending", "r1")); err != nil || ok {
		t.Fatalf("OweHandBack for another ending = %v, %v, want refused", ok, err)
	}
	if _, ok, err := owe(letGoOf(ending, "other")); err != nil || ok {
		t.Fatalf("OweHandBack of an answer the run does not hold = %v, %v, want refused", ok, err)
	}
	if _, _, err := owe(letGoOf(ending)); err == nil {
		t.Error("a let-go naming no delivery was accepted")
	}
	written, ok, err := owe(letGoOf(ending, "r1"))
	if err != nil || !ok {
		t.Fatalf("OweHandBack = %v, %v", ok, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": written, "stored": mustGet(t, s, "t1")} {
		if got.Status != sandbox.StatusResumed || got.Answer != nil || got.Ending == nil ||
			len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" {
			t.Fatalf("%s row = %q answer %+v hand-back %+v ending %+v, want the ending kept, the "+
				"answer gone and r1's copy owed, from the one write", name, got.Status, got.Answer,
				got.HandBack, got.Ending)
		}
	}
	// LET GO ONCE: a repeat finds no answer left to let go of.
	if _, ok, err := owe(letGoOf(ending, "r1")); err != nil || ok {
		t.Fatalf("a second let-go of the same answer = %v, %v, want refused", ok, err)
	}
	// AND NEVER TAKEN, NOR GIVEN BACK TO A RESUME.
	if ok, err := s.TakeAnswer(ctx, "t1", launch, lease); err != nil || ok {
		t.Fatalf("TakeAnswer after the answer was let go = %v, %v, want refused", ok, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusAnswered, Fence: lease,
	}); err != nil || released {
		t.Fatalf("a claim whose answer was let go of was released to answered: %v, %v", released, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "gone", letGoOf(ending, "r1")); err != nil || ok {
		t.Errorf("OweHandBack on a run that does not exist = %v, %v", ok, err)
	}
}

// AN ANSWER NONE OF WHOSE DELIVERIES COULD BE CARRIED IS LET GO OF WITH NOTHING
// TO HAND BACK, as its decline is: refused, the ending could never finish, and
// the store would never delete the row that holds it.
func testAnAnswerThatCarriesNoCopyIsStillLetGo(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	letGo := letGoOf(ending, "r1")
	letGo.HandBack = nil
	if _, ok, err := s.OweHandBack(ctx, "t1", letGo); err != nil || !ok {
		t.Fatalf("OweHandBack with no copy to owe = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer != nil || len(got.HandBack) != 0 {
		t.Fatalf("answer %+v hand-back %+v, want the answer gone and nothing owed", got.Answer, got.HandBack)
	}
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish after the let-go = %v, %v", ended, err)
	}
}

// AN ANSWERED RUN IS ENDED ONLY BY AN ENDING LICENSED FOR IT. A release that
// landed although it reported a failure, or an old holder's release under a reap
// whose fence did not land, puts a run an ending decided on as a claim back to
// answered — owed its resume, not an ending. A claim's own ending is licensed
// for the claim alone, so it decides nothing about that run; an ending licensed
// for it does, and from then on no resume can claim it.
func testAnAnsweredRunsReplyIsLetGoOnlyUnderItsLicense(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly, Launch: launch},
	}); err != nil || ok {
		t.Fatalf("a claim's own ending of an answered run = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil || got.Ending != nil {
		t.Fatalf("run = %q answer %+v ending %+v, want it still answered and owed its resume",
			got.Status, got.Answer, got.Ending)
	}
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a resume claimed a run whose ending is decided: %v, %v", ok, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r1")); err != nil || !ok {
		t.Fatalf("the ending's let-go of an answered run = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer != nil ||
		len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" {
		t.Fatalf("row = %q answer %+v hand-back %+v, want the answer gone and r1's copy owed",
			got.Status, got.Answer, got.HandBack)
	}
}

// THE TAKE AND THE LET-GO ARE EXCLUSIVE IN THE STORE, whichever lands first and
// whatever any fence says: a take that lands before the ending is decided is
// seen by the decision, and the ending's let-go of the answer is refused as
// taken — the reply was used, and goes with the run — unless the ending says it
// went unused; and a take that comes after the decision is refused.
func testATakenAnswerIsNotLetGo(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), true)
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	row, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r1"))
	if !errors.Is(err, sandbox.ErrAnswerTaken) || ok || row.Answer == nil || !row.Answer.Taken() {
		t.Fatalf("a let-go of a taken answer = %v, %+v, %v, want refused as taken with the row",
			ok, row.Answer, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer == nil || !got.Answer.Taken() || len(got.HandBack) != 0 {
		t.Fatalf("answer %+v hand-back %+v, want the taken answer left alone", got.Answer, got.HandBack)
	}
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish of a run whose turn took the reply = %v, %v, want it ended with it", ended, err)
	}

	claimedAnswerOn(t, s, "t2", answerOf("r1", "use main"), true)
	unused := everyJob(sandbox.Fence{}, sandbox.Active)
	unused.Unused = true
	ending = decide(t, s, "t2", unused).Ending.ID
	if _, ended, err := s.Finish(ctx, "t2", ending); !errors.Is(err, sandbox.ErrAnswerOwed) || ended {
		t.Fatalf("Finish of a run whose taken reply went unused = %v, %v, want refused with ErrAnswerOwed",
			ended, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t2", letGoOf(ending, "r1")); err != nil || !ok {
		t.Fatalf("the let-go of an answer its turn gave back = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t2"); got.Answer != nil || len(got.HandBack) != 1 {
		t.Fatalf("answer %+v hand-back %+v, want the reply let go and its copy owed", got.Answer, got.HandBack)
	}
}

// A ROW HOLDING A PERSON'S REPLY NO TURN TOOK IS NOT DELETED, whatever ending it
// is decided on: its delivery was spent when it was recorded, so the row is the
// only thing that still carries it. The ending is told so, with the row, and
// lets the reply go first — and no turn can take it from a row whose ending is
// decided. One a turn took before the decision has been used, and goes with
// the run.
func testARowHoldingAnUntakenReplyIsNotDeleted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{WhileIn: claimOnly, Launch: claimed.LaunchID}).Ending.ID
	row, ended, err := s.Finish(ctx, "t1", ending)
	if !errors.Is(err, sandbox.ErrAnswerOwed) || ended || row.Answer == nil {
		t.Fatalf("Finish of a claim holding an untaken reply = %v, %+v, %v, want refused with the row",
			ended, row.Answer, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("TakeAnswer after the ending was decided = %v, %v, want refused", ok, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the row holding the reply is gone (found %v, %v)", found, err)
	}

	mustLaunched(t, s, run("t2"))
	park(t, s, "t2")
	launch = mustGet(t, s, "t2").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t2", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer t2 = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t2", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume t2 = %v, %v", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t2", launch, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("TakeAnswer t2 = %v, %v", ok, err)
	}
	if _, ended, err := end(ctx, s, "t2", sandbox.License{WhileIn: claimOnly, Launch: launch}); err != nil || !ended {
		t.Fatalf("the ending of a claim whose turn took the reply = %v, %v", ended, err)
	}
}

// A CLAIM'S OWN ENDING NAMES ITS JOB, and a run that holds another is not its
// to end: the resumed turn's relaunch is claimed by its own completion in the
// very status the first claim held, and an ending licensed on the status alone
// ended it, box and all.
func testAnEndingLicensedForAJobLeavesAnother(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	first := mustClaim(t, s, "t1")
	mustLaunched(t, s, run("t1"))
	mustClaim(t, s, "t1")
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly, Launch: first.LaunchID},
	}); err != nil || ok {
		t.Fatalf("an ending licensed for the first job ended the second: %v, %v", ok, err)
	}
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly},
	}); err != nil || ok {
		t.Fatalf("an ending naming no job ended a run that holds one: %v, %v", ok, err)
	}
	if _, ended, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, claimOnly)); err != nil || !ended {
		t.Fatalf("an ending licensed for every job = %v, %v", ended, err)
	}
}

// A CLAIM IS TAKEN UNDER THE CLAIMANT'S LEASE: refused where a newer lease owns
// the run, and stamped on the row where it lands, so the take, the release and
// the ending that follow it carry the CLAIMANT's lease rather than whatever the
// row was stamped with before — which, for a run nobody re-stamped, fenced out
// nobody. And an UNFENCED claim is refused on a row any lease owns: a node that
// noticed it lost the seat, holding no lease, claimed past its successor's
// fence. One claims only a row no lease has owned, and leaves its owner as it
// stands.
func testAClaimIsTakenUnderTheClaimantsLease(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 4); err != nil {
		t.Fatalf("own: %v", err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"),
		sandbox.Fence{Owner: "node-a:1", Epoch: 3}); err != nil || won {
		t.Fatalf("a claim under a lease the run's outranks = %v, %v, want refused", won, err)
	}
	claimed, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"),
		sandbox.Fence{Owner: "node-c:1", Epoch: 6})
	if err != nil || !won {
		t.Fatalf("a claim under the newest lease = %v, %v", won, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": claimed, "stored": mustGet(t, s, "t1")} {
		if got.Owner != "node-c:1" || got.OwnerEpoch != 6 {
			t.Fatalf("%s row owned by %q at %d, want the claimant's lease", name, got.Owner, got.OwnerEpoch)
		}
	}
	mustRelease(t, s, claimed)
	if _, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("an unfenced claim of a row a lease owns = %v, %v, want refused", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning || got.Owner != "node-c:1" ||
		got.OwnerEpoch != 6 {
		t.Fatalf("run %q owned by %q at %d, want it left running under its owner",
			got.Status, got.Owner, got.OwnerEpoch)
	}

	mustLaunched(t, s, run("t2"))
	if _, won, err := s.ClaimForResume(ctx, "t2", completionOf(t, s, "t2"), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("an unfenced claim of a row no lease owns = %v, %v", won, err)
	}
	if got := mustGet(t, s, "t2"); got.Owner != "" || got.OwnerEpoch != 0 {
		t.Fatalf("an unfenced claim moved the owner to %q at %d", got.Owner, got.OwnerEpoch)
	}
}

// A LET-GO WAITS FOR THE COPIES ALREADY OWED, and says what they are: an ending
// lets a claimed answer go only once an earlier decline's copies are out, so the
// row never owes more than one decline's worth.
func testALetGoWaitsForTheCopiesAlreadyOwed(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r1 = %v, %v", ok, err)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer r1 = %v, %v", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r2 = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{WhileIn: claimOnly, Launch: launch}).Ending.ID
	owing, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2"))
	if !errors.Is(err, sandbox.ErrHandBackOwed) || ok ||
		len(owing.HandBack) != 1 || owing.HandBack[0].ID != "r1-copy" {
		t.Fatalf("a let-go over owed copies = %v, %+v, %v, want refused with r1's copy", ok,
			owing.HandBack, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer == nil || len(got.HandBack) != 1 {
		t.Fatalf("answer %+v hand-back %+v, want r2 still on the claim and only r1's copy owed",
			got.Answer, got.HandBack)
	}
	published(t, s, "r1-copy")
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); err != nil || !ok {
		t.Fatalf("a let-go once r1's copy was out = %v, %v", ok, err)
	}
}

// A TURN TAKES ITS ANSWER ONCE, on the claim that drives it, and a release
// gives it back to the run for the retry: what the seat's next holder reads to
// tell a reply a turn used from one nobody ever got to.
func testATurnTakesTheAnswerItsClaimDrives(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	// NOT BEFORE THE CLAIM: an answer still owed its resume is nobody's.
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("TakeAnswer before the claim = %v, %v, want refused", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("answer %+v, want it claimed and NOT taken: a claim is not a turn", got.Answer)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", "another-launch", sandbox.Fence{}); err != nil || ok {
		t.Fatalf("TakeAnswer for another launch = %v, %v, want refused", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("TakeAnswer = %v, %v", ok, err)
	}
	taken := mustGet(t, s, "t1").Answer
	if taken == nil || !taken.Taken() {
		t.Fatalf("answer %+v, want it taken", taken)
	}
	// IDEMPOTENT, and the instant is the first one's.
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("a repeated TakeAnswer = %v, %v, want true", ok, err)
	}
	if got := mustGet(t, s, "t1").Answer; !got.TakenAt.Equal(taken.TakenAt) {
		t.Errorf("a repeated take moved the instant from %v to %v", taken.TakenAt, got.TakenAt)
	}
	// A RELEASE GIVES IT BACK: the retry's turn takes it again.
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusAnswered,
	}); err != nil || !released {
		t.Fatalf("ReleaseClaim = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
		got.Answer.Taken() {
		t.Fatalf("run = %q answer %+v, want it owed again and not taken", got.Status, got.Answer)
	}
	// A NEWER LEASE FENCES A TAKE OUT: the seat's next holder decides on a
	// row nobody who lost the seat can still write.
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if ok, err := s.ClaimOwnership(ctx, "t1", "next", 5); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{Owner: "old", Epoch: 1}); err != nil || ok {
		t.Fatalf("TakeAnswer under an outranked fence = %v, %v, want refused", ok, err)
	}
	// NOR UNDER NO LEASE AT ALL, which every other write here lets
	// through: a stalled claimant that held none took the answer on a row
	// its successor had fenced, and the person was answered twice.
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("TakeAnswer under no lease on a fenced row = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1").Answer; got == nil || got.Taken() {
		t.Fatalf("answer %+v, want it untaken after the outranked take", got)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{Owner: "next", Epoch: 5}); err != nil || !ok {
		t.Fatalf("TakeAnswer under the row's own lease = %v, %v", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "gone", launch, sandbox.Fence{}); err != nil || ok {
		t.Errorf("TakeAnswer on a run that does not exist = %v, %v", ok, err)
	}
}

// A DEAD CLAIM'S ANSWER IS REVIVED ONCE, FENCED AND COUNTED. The seat's next
// holder gives a claim whose node stopped before its turn took the answer back
// to that answer: the run answered again, owed its resume, stamped with the
// holder's lease and the loss counted on the answer — all in one write, because
// a revival without its fence is one the stalled claimant could still take, and
// one without its count is a resume that kills its node revived for ever. Only
// the claim it names is revived — that job, that answer, no newer lease — and
// only once: a revived run is no longer the claim, so a take under the dead
// claim's lease finds nothing to take.
func testADeadClaimsAnswerIsRevivedOnceFencedAndCounted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), false)
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	for name, revival := range map[string]sandbox.Revival{
		"another launch": {Launch: "another-launch", Answer: []string{"r1"}, Fence: next, Lost: true},
		"another answer": {Launch: launch, Answer: []string{"r2"}, Fence: next, Lost: true},
	} {
		if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
			t.Fatalf("a revival naming %s = %v, %v, want refused", name, ok, err)
		}
	}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Fence: next, Lost: true}); err == nil || ok {
		t.Fatalf("a revival naming no answer = %v, %v, want an error", ok, err)
	}
	revival := sandbox.Revival{Launch: launch, Answer: []string{"r1"}, Fence: next, Lost: true}
	revived, ok, err := s.ReviveAnswer(ctx, "t1", revival)
	if err != nil || !ok {
		t.Fatalf("ReviveAnswer = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Owner != "next" || got.OwnerEpoch != 2 {
		t.Fatalf("run %q owned by %q at %d, want it answered again under the reviving lease",
			got.Status, got.Owner, got.OwnerEpoch)
	}
	if a := got.Answer; a == nil || a.Text != "use main" || a.Taken() || a.LostClaims != 1 ||
		a.FirstLostAt.IsZero() {
		t.Fatalf("answer %+v, want the same answer, untaken, its lost claim counted", a)
	}
	if revived.Status != got.Status || revived.OwnerEpoch != got.OwnerEpoch ||
		revived.Answer == nil || revived.Answer.LostClaims != 1 {
		t.Fatalf("ReviveAnswer reported %q at %d with %+v, want the row as written",
			revived.Status, revived.OwnerEpoch, revived.Answer)
	}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
		t.Fatalf("a second revival of the same claim = %v, %v, want refused: it is no longer a claim", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a take under the dead claim = %v, %v, want refused: the answer is the run's again", ok, err)
	}

	// THE NEXT CLAIM DYING TOO is counted on the same answer, from the same
	// first instant: what the count bounds is a series no one node sees.
	first := got.Answer.FirstLostAt
	later := sandbox.Fence{Owner: "later", Epoch: 3}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), next); err != nil || !ok {
		t.Fatalf("the revived run's claim = %v, %v", ok, err)
	}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
		Fence: later, Lost: true}); err != nil || !ok {
		t.Fatalf("the second revival = %v, %v", ok, err)
	}
	if a := mustGet(t, s, "t1").Answer; a.LostClaims != 2 || !a.FirstLostAt.Equal(first) {
		t.Fatalf("answer lost %d claims since %v, want 2 since %v", a.LostClaims, a.FirstLostAt, first)
	}

	// NOT UNDER AN OUTRANKED LEASE: a holder that lost the seat revives
	// nothing its successor holds.
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), later); err != nil || !ok {
		t.Fatalf("the third claim = %v, %v", ok, err)
	}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
		t.Fatalf("a revival under an outranked lease = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed || got.Answer.LostClaims != 2 {
		t.Fatalf("run %q with %d lost claims, want the claim left as it was", got.Status, got.Answer.LostClaims)
	}
}

// AN ANSWER IS RECORDED ONLY UNDER THE SEAT'S LEASE. A row a newer lease owns
// refuses a record under an older lease, and under none, with
// [sandbox.ErrSeatNotHeld] — the answer is the seat holder's to record and
// drive, and one recorded by a node that lost the seat landed on a run its
// successor had recovered as waiting, where nothing drove it. Under the row's
// own lease or a newer one it records, and stamps nothing: a record is not a
// claim of the run.
func testAnAnswerIsRecordedOnlyUnderTheSeatsLease(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-y", 6); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	for name, fence := range map[string]sandbox.Fence{
		"an outranked lease": {Owner: "node-x", Epoch: 5},
		"no lease":           {},
	} {
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), fence); ok ||
			!errors.Is(err, sandbox.ErrSeatNotHeld) {
			t.Fatalf("a record under %s = %v, %v, want it refused as a seat not held", name, ok, err)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q with answer %+v, want it still waiting", got.Status, got.Answer)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"),
		sandbox.Fence{Owner: "node-y", Epoch: 6}); err != nil || !ok {
		t.Fatalf("a record under the row's own lease = %v, %v", ok, err)
	}

	mustLaunched(t, s, run("t2"))
	park(t, s, "t2")
	if ok, err := s.ClaimOwnership(ctx, "t2", "node-y", 6); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t2", mustGet(t, s, "t2").LaunchID, answerOf("r2", "use dev"),
		sandbox.Fence{Owner: "node-z", Epoch: 7}); err != nil || !ok {
		t.Fatalf("a record under a newer lease = %v, %v", ok, err)
	}
	for _, turn := range []string{"t1", "t2"} {
		if got := mustGet(t, s, turn); got.Status != sandbox.StatusAnswered || got.Owner != "node-y" ||
			got.OwnerEpoch != 6 {
			t.Fatalf("%s %q owned by %q at %d, want it answered and its owner left as it stood",
				turn, got.Status, got.Owner, got.OwnerEpoch)
		}
	}
}

// A CLAIM ITS OWN NODE GIVES BACK IS NOT COUNTED. A claim whose write reported a
// failure and landed is given back to its answer by the node that made it,
// under the lease it was taken under: the run answered again, owed its resume —
// and nothing counted, because no node stopped. Counted, a coordination store
// that answered a few of a healthy node's claims with errors spent the answer's
// revivals and ended the run as an abandoned tail. A claim that IS lost after it
// is counted as the first.
func testAClaimItsOwnNodeGivesBackIsNotCounted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), false)
	own := sandbox.Fence{Owner: "own", Epoch: 1}
	for i := range 3 {
		if i > 0 {
			if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), own); err != nil || !ok {
				t.Fatalf("claim %d = %v, %v", i+1, ok, err)
			}
		}
		given, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
			Fence: own})
		if err != nil || !ok {
			t.Fatalf("give-back %d = %v, %v", i+1, ok, err)
		}
		got := mustGet(t, s, "t1")
		if got.Status != sandbox.StatusAnswered || got.Owner != "own" || got.OwnerEpoch != 1 {
			t.Fatalf("run %q owned by %q at %d, want it answered again under the claim's own lease",
				got.Status, got.Owner, got.OwnerEpoch)
		}
		if a := got.Answer; a == nil || a.Taken() || a.LostClaims != 0 || !a.FirstLostAt.IsZero() {
			t.Fatalf("answer after give-back %d: %+v, want the same answer, untaken, nothing counted",
				i+1, a)
		}
		if given.Answer == nil || given.Answer.LostClaims != 0 || given.Status != sandbox.StatusAnswered {
			t.Fatalf("ReviveAnswer reported %q with %+v, want the row as written", given.Status, given.Answer)
		}
	}

	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), own); err != nil || !ok {
		t.Fatalf("the claim that dies = %v, %v", ok, err)
	}
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
		Fence: next, Lost: true}); err != nil || !ok {
		t.Fatalf("the lost claim's revival = %v, %v", ok, err)
	}
	if a := mustGet(t, s, "t1").Answer; a.LostClaims != 1 || a.FirstLostAt.IsZero() {
		t.Fatalf("answer lost %d claims since %v, want the one lost claim counted", a.LostClaims, a.FirstLostAt)
	}
}

// A REVIVAL IS REFUSED AN ANSWER A TURN TOOK, with the row — the take and the
// revival are exclusive, as the take and an ending's let-go are, so whichever
// lands first decides and the person is answered once: by the turn, or by the
// revived run's resume. Nor does it revive a run whose ending is decided —
// that ending owns what becomes of the answer — or one that is gone.
func testARevivalIsRefusedAnAnswerATurnTook(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), true)
	row, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"}, Fence: next,
		Lost: true})
	if !errors.Is(err, sandbox.ErrAnswerTaken) || ok || row.Answer == nil || !row.Answer.Taken() {
		t.Fatalf("a revival of a taken answer = %v, %+v, %v, want refused as taken with the row",
			ok, row.Answer, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed || got.Answer.LostClaims != 0 {
		t.Fatalf("run %q with %d lost claims, want the turn's claim left alone", got.Status, got.Answer.LostClaims)
	}

	launch = claimedAnswerOn(t, s, "t2", answerOf("r2", "use dev"), false)
	decide(t, s, "t2", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, ok, err := s.ReviveAnswer(ctx, "t2", sandbox.Revival{Launch: launch, Answer: []string{"r2"},
		Fence: next, Lost: true}); err != nil || ok {
		t.Fatalf("a revival of a run whose ending is decided = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t2"); got.Status != sandbox.StatusResumed {
		t.Fatalf("run %q, want the decided run left as its ending found it", got.Status)
	}
	if _, ok, err := s.ReviveAnswer(ctx, "gone", sandbox.Revival{Launch: launch, Answer: []string{"r2"},
		Fence: next, Lost: true}); err != nil || ok {
		t.Fatalf("a revival of a run that does not exist = %v, %v", ok, err)
	}
}

// A DECLINE OWES ITS COPIES IN THE SAME WRITE, and they are owed until a
// publisher clears them — whatever the run does in between. The write is the
// decision to hand the reply back, and the copies on the row are what make
// that decision survive the process that took it: a decline that cleared the
// answer and recorded nothing would lose the reply to a crash before its
// publish, and one that recorded the copies anywhere but this write could be
// seen half done.
func testADeclineOwesItsCopiesInTheSameWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	written, ok, err := s.DeclineAnswer(ctx, "t1", launch, []string{"r1"},
		[]sandbox.HandedBack{copyOf("r1")}, sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": written, "stored": mustGet(t, s, "t1")} {
		if got.Status != sandbox.StatusAwaiting || got.Answer != nil ||
			len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" ||
			string(got.HandBack[0].Event) != `{"id":"r1-copy"}` {
			t.Fatalf("%s row = %q answer %+v hand-back %+v, want the question open and r1's "+
				"copy owed, from the one write", name, got.Status, got.Answer, got.HandBack)
		}
	}
	// A DECLINE NEEDS THE ANSWER IT LETS GO OF.
	if _, _, err := s.DeclineAnswer(ctx, "t1", launch, nil, nil, sandbox.Fence{}); err == nil {
		t.Error("a decline naming no delivery was accepted")
	}

	// OWED THROUGH WHATEVER THE RUN DOES NEXT: a new answer, and a new job.
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r2 = %v, %v", ok, err)
	}
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 1 {
		t.Fatalf("hand-back = %+v after a new answer and a relaunch, want r1's copy still owed",
			got.HandBack)
	}

	// CLEARED BY ID, and only what is named.
	if ok, err := s.ClearHandBack(ctx, "t1", []string{"someone-else"}); err != nil || ok {
		t.Errorf("clearing a copy the run does not owe = %v, %v, want nothing removed", ok, err)
	}
	if ok, err := s.ClearHandBack(ctx, "t1", []string{"r1-copy"}); err != nil || !ok {
		t.Fatalf("ClearHandBack = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 0 {
		t.Fatalf("hand-back = %+v after it was cleared", got.HandBack)
	}
	if ok, err := s.ClearHandBack(ctx, "gone", []string{"r1-copy"}); err != nil || ok {
		t.Errorf("clearing a run that does not exist = %v, %v", ok, err)
	}
}

// A NEW QUESTION IS MEASURED FROM ITS OWN ASKING, with nothing recorded or
// declined against it: a reply one question let go of may be exactly what the
// next is waiting for, and an answer belongs to the question it answered.
func testANewQuestionForgetsTheLastOnesAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	asked := base.Add(time.Hour)
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "?", AskedAt: asked}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.AskedAt.Equal(asked) {
		t.Fatalf("asked at %v, want %v", got.AskedAt, asked)
	}
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "x"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	got := mustGet(t, s, "t1")
	if !got.AskedAt.IsZero() || got.Answer != nil || len(got.DeclinedAnswers) != 0 {
		t.Fatalf("a new launch kept asked_at %v, answer %+v, declined %v",
			got.AskedAt, got.Answer, got.DeclinedAnswers)
	}
}
