// Package sandboxtest is the code sandbox's two contract suites: the
// pending-run store's ([Run]) and a box's file reads ([Box]).
//
// THE PROPERTIES THAT MATTER IN [Run] ARE THE STORE'S. The at-most-once tail
// claim, the scoped release and the charge record it carries, the epoch fence
// and the launch that stamps it, an answer taken by a turn or let go of but
// never both, the box record's two halves moving together: each is a
// conditional write, not
// code around one, so a suite that ran only against a fake would assert the
// author's intent and nothing about the store. So are a person's notes on a
// run's record (notes.go): filed, taken, pinned and bounded by the same writes. So is
// which of two reads of one job keeps its session (standby.go): the place given
// to one, the other standing by, and the next place taken only after a capture
// that did not land. The one implementation is
// [sandbox.CoordStore]. The record operations it is built on are certified on
// both coordination backends by coordtest, and this suite certifies every
// conditional flip built on top of them.
//
// # A later build's keys
//
// The pending-run record is rewritten whole by every node that touches it, so
// a field a newer build adds to the notes section, to one note, to where a
// note was read, to a hook's serve of a note, to the call that serve answered
// or to the job's own record is gone the moment this build writes the record
// back, unless every level that can grow keeps what it does not know. The log
// beside the record ([sandbox.BridgeLog]) is rewritten whole by every append,
// so the same holds at its top, in each bridged call it keeps and in each
// identity of a call it dropped: nine levels in all. One loss is meant: a
// relaunch's reset replaces the job's record whole, so the key a later build
// added to it goes with the job it was about. Every level is held by
// testAKeyALaterBuildAddsInsideASectionSurvivesEveryWrite (notes.go).
package sandboxtest

import (
	"context"
	"encoding/json"
	"slices"
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
//
// over builds the store under test on the records it is handed, for the cases
// whose subject is the gap between two of the store's own writes
// ([stagedLogCases]): they hand it records that stage that gap.
func Run(t *testing.T, newStore func(t *testing.T) (sandbox.PendingStore, coord.SandboxRuns),
	over func(coord.SandboxRuns) sandbox.PendingStore,
) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, sandbox.PendingStore)
	}{
		{"WorkItemSurvivesParkAndResume", testWorkItemSurvivesParkAndResume},
		{"AParkedRunRecordsWhoItsAudienceIs", testAParkedRunRecordsWhoItsAudienceIs},
		{"ARequesterSurvivesTheLaunchThatRecordsIt", testARequesterSurvivesTheLaunchThatRecordsIt},
		{"AColleaguesAskSurvivesEveryWriteOfItsTurn", testAColleaguesAskSurvivesEveryWriteOfItsTurn},
		{"ASecondLaunchKeepsTheBoxItWillReattachTo", testASecondLaunchKeepsTheBoxItWillReattachTo},
		{"ARelaunchsBoxMovesTheRowToItsCell", testARelaunchsBoxMovesTheRowToItsCell},
		{"EveryLaunchRecordsTheNodeThatLaunchedIt", testEveryLaunchRecordsTheNodeThatLaunchedIt},
		{"ALossIsRecordedOnlyOnTheJobItWasJudgedOn", testALossIsRecordedOnlyOnTheJobItWasJudgedOn},
		{"AClaimsCollectionRecordsItsJobLostOnItsClaimAlone", testAClaimsCollectionRecordsItsJobLostOnItsClaimAlone},
		{"ARoundCountsItsLostJobsAcrossTheRelaunchesThatCarryItOn", testARoundCountsItsLostJobsAcrossTheRelaunchesThatCarryItOn},
		{"AHeldReleaseHoldsTheResumeUntilItsConditionLifts", testAHeldReleaseHoldsTheResumeUntilItsConditionLifts},
		{"AHeldRunIsClaimedByNoAnnouncementUntilItsHoldLifts", testAHeldRunIsClaimedByNoAnnouncementUntilItsHoldLifts},
		{"ALiftClearsOnlyTheHoldItRead", testALiftClearsOnlyTheHoldItRead},
		{"AHeldReleaseOwesItsCompletionOnTheJobsRecord", testAHeldReleaseOwesItsCompletionOnTheJobsRecord},
		{"ASecondLaunchDropsTheFirstSuspension", testASecondLaunchDropsTheFirstSuspension},
		{"ASecondLaunchDropsTheFirstRunsBridgedCalls", testASecondLaunchDropsTheFirstRunsBridgedCalls},
		{"ALaunchNeedsATurnID", testALaunchNeedsATurnID},
		{"ALaunchAnswersTheJobItOpened", testALaunchAnswersTheJobItOpened},
		{"ABoxsSessionStartIsRecordedOnceForItsOwnJob", testABoxsSessionStartIsRecordedOnceForItsOwnJob},
		{"AHarnessConnectingItsBridgeIsRecordedOnceForItsOwnJob", testAHarnessConnectingItsBridgeIsRecordedOnceForItsOwnJob},
		{"AMintedSessionIsBoundOnceToItsOwnJob", testAMintedSessionIsBoundOnceToItsOwnJob},
		{"AMintedSessionIsOneLiveRuns", testAMintedSessionIsOneLiveRuns},
		{"AJobItsCapEndedIsRecordedOnceForItsOwnJob", testAJobItsCapEndedIsRecordedOnceForItsOwnJob},
		{"AJobsHarnessEndIsRecordedForItsOwnJobAndCutOffStands", testAJobsHarnessEndIsRecordedForItsOwnJobAndCutOffStands},
		{"AJobsHarnessEndRidesEveryWriteOfItsRecord", testAJobsHarnessEndRidesEveryWriteOfItsRecord},
		{"AReleaseRecordsTheHarnessEndByTheRecordsRule", testAReleaseRecordsTheHarnessEndByTheRecordsRule},
		{"AJobsMeasureIsItsOwnOrCarriedAlongItsCopy", testAJobsMeasureIsItsOwnOrCarriedAlongItsCopy},
		{"AMeasureLandsWhereTheEndIsAlreadyRecorded", testAMeasureLandsWhereTheEndIsAlreadyRecorded},
		{"AJobsCountIsItsOwnReadsAndNeverCarried", testAJobsCountIsItsOwnReadsAndNeverCarried},
		{"ACountLandsWhereTheEndIsAlreadyRecorded", testACountLandsWhereTheEndIsAlreadyRecorded},
		{"TheNextRequestIsNeverCarried", testTheNextRequestIsNeverCarried},
		{"AHarnessEndWritesTheNextRequest", testAHarnessEndWritesTheNextRequest},
		{"AHarnessEndWritesTheCountsProvider", testAHarnessEndWritesTheCountsProvider},
		{"TheCopysWriterIsHandedToTheRelaunchAndNeverCarried", testTheCopysWriterIsHandedToTheRelaunchAndNeverCarried},
		{"ALaunchRecordsWhatTheBriefItHandedLeftOut", testALaunchRecordsWhatTheBriefItHandedLeftOut},
		{"TheTurnsLeadsAreKeptBesideItsRowAndOutliveEveryRelaunch", testTheTurnsLeadsAreKeptBesideItsRowAndOutliveEveryRelaunch},
		{"AResumedTurnsCarryIsItsClaimsOwn", testAResumedTurnsCarryIsItsClaimsOwn},
		{"ALaunchARowsNewerLeaseOutranksIsRefused", testALaunchARowsNewerLeaseOutranksIsRefused},
		{"ALaunchingRunIsNotClaimable", testALaunchingRunIsNotClaimable},
		{"SuspendingOpensTheRunToTheTail", testSuspendingOpensTheRunToTheTail},
		{"OnlyALaunchingRunCanSuspend", testOnlyALaunchingRunCanSuspend},
		{"AttachingABoxClearsThePauseStamp", testAttachingABoxClearsThePauseStamp},
		{"ALaunchingRunIsActive", testALaunchingRunIsActive},
		{"ASeatsRunsAreFoundByItsID", testASeatsRunsAreFoundByItsID},
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
		{"AStopIsRecordedWithTheDecisionThatMakesIt", testAStopIsRecordedWithTheDecisionThatMakesIt},
		{"AStopsCollectionIsRecordedOnceAsAStepOfItsEnding", testAStopsCollectionIsRecordedOnceAsAStepOfItsEnding},
		{"AStopOfAJobACollectionReadIsRefused", testAStopOfAJobACollectionReadIsRefused},
		{"AJobKeepsTheInstantItWasFirstCollected", testAJobKeepsTheInstantItWasFirstCollected},
		{"AReleaseCountsAFailedResumeOntoTheJob", testAReleaseCountsAFailedResumeOntoTheJob},
		{"ARefusedRelaunchsWindowStaysOnItsJobUntilTheNextLaunch", testARefusedRelaunchsWindowStaysOnItsJobUntilTheNextLaunch},
		{"ARefusedRelaunchIsAnError", testARefusedRelaunchIsAnError},
		{"ANewerLeasesEndingIsStillASeatNotHeld", testANewerLeasesEndingIsStillASeatNotHeld},
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
		{"AReleaseWhoseReadAnsweredEndsTheRunOfFailures", testAReleaseWhoseReadAnsweredEndsTheRunOfFailures},
		{"AReleaseRecordsWhatTheHostsStoreLastSaid", testAReleaseRecordsWhatTheHostsStoreLastSaid},
		{"AnOwedRefusalIsRecordedOnceAndSettledByThePark", testAnOwedRefusalIsRecordedOnceAndSettledByThePark},
		{"AnOwedRefusalIsRecordedOnAClaimOfItsCompletion", testAnOwedRefusalIsRecordedOnAClaimOfItsCompletion},
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
		{"AnAnswerIsRecordedOnlySealed", testAnAnswerIsRecordedOnlySealed},
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
		{"TheEnginesCarryingOnIsAClaimOfItsOwnNeverAnAnswer", testTheEnginesCarryingOnIsAClaimOfItsOwnNeverAnAnswer},
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
		{"ManyBridgedWritersAllLand", testManyBridgedWritersAllLand},
		{"BridgeCallsForAMissingRunAreDropped", testBridgeCallsForAMissingRunAreDropped},
		{"BridgeCallsDropTheMiddleNotTheStart", testBridgeCallsDropTheMiddleNotTheStart},
		{"ABridgedRunsSpendIsTheNewestTotal", testABridgedRunsSpendIsTheNewestTotal},
		{"ABridgedCallNamingNoJobIsNotRecorded", testABridgedCallNamingNoJobIsNotRecorded},
		{"ABridgedCallAfterItsJobsClaimStaysOnItsJob", testABridgedCallAfterItsJobsClaimStaysOnItsJob},
		{"ARowALaterJobMovedOnReadsNoneOfItsCalls", testARowALaterJobMovedOnReadsNoneOfItsCalls},
		{"AParkedJobsCondensationIsKeptForItsAnswer", testAParkedJobsCondensationIsKeptForItsAnswer},
		{"BeginLaunchUnionsCodeHolds", testBeginLaunchUnionsCodeHolds},
		{"AddCodeHoldsAddsToALiveRowOnly", testAddCodeHoldsAddsToALiveRowOnly},
		{"RaiseCodeFloorRaisesTheCurrentLaunchOnly", testRaiseCodeFloorRaisesTheCurrentLaunchOnly},
		{"ACheckoutIsGrantedToTheRunningJobOnly", testACheckoutIsGrantedToTheRunningJobOnly},
		{"ARunWorksForOneRequester", testARunWorksForOneRequester},
	}
	cases = append(cases, noteCases...)
	cases = append(cases, askCases...)
	cases = append(cases, heldCases...)
	cases = append(cases, delegatedCases...)
	cases = append(cases, bytesCases...)
	cases = append(cases, refusalCases...)
	cases = append(cases, standbyCases...)
	cases = append(cases, lateCases...)
	cases = append(cases, lateItemCases...)
	cases = append(cases, wakeItemCases...)
	cases = append(cases, foldInCases...)
	cases = append(cases, tellingCases...)
	cases = append(cases, briefReadCases...)
	cases = append(cases, inFlightCases...)
	cases = append(cases, savingCases...)
	cases = append(cases, writtenCases...)
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
		{"ListRunsListsEveryStatus", testListRunsListsEveryStatus},
		{"ARunRowCarriesItsRepositoriesAndFloor", testARunRowCarriesItsRepositoriesAndFloor},
		{"AnEarlierRunsLeadsAreNoneOfTheTurnsNextRuns", testAnEarlierRunsLeadsAreNoneOfTheTurnsNextRuns},
		{"AClaimWhoseRunHasNoRecordBindsNothing", testAClaimWhoseRunHasNoRecordBindsNothing},
		{"AClaimACrashLeftNeverBlocksItsOwnJob", testAClaimACrashLeftNeverBlocksItsOwnJob},
	}
	raw = append(raw, noteRawCases...)
	raw = append(raw, bytesRawCases...)
	raw = append(raw, savingRawCases...)
	for _, tc := range raw {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, runs := newStore(t)
			tc.fn(t, store, runs)
		})
	}
	// TWO NODES' STORES OVER ONE BACKEND, for the cases whose subject is what
	// one node's write leaves the other.
	for _, tc := range slices.Concat(savingFleetCases, writtenFleetCases) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, runs := newStore(t)
			tc.fn(t, store, over(runs))
		})
	}
	for _, tc := range slices.Concat(stagedLogCases, lateStagedCases, lateItemStagedCases) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, runs := newStore(t)
			staged := &stagedRuns{records: runs}
			tc.fn(t, staged, over(staged))
		})
	}
	for _, tc := range noteStagedCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, runs := newStore(t)
			overtaking := &overtakingRuns{records: runs}
			tc.fn(t, overtaking, over(overtaking))
		})
	}
}

func run(turnID string) sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		Harness: "claude-code", TaskDescription: "fix the flake", Requester: requester,
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
// now — after every question the case parked — by the runs' requester, whom
// every question is put to. The rule is a value's
// ([sandbox.ConversationRef.Best], [sandbox.Reply.Best]); what the store is
// certified on is handing over every candidate, and nothing else.
func findAwaiting(ctx context.Context, s sandbox.PendingStore, handle string,
	conv sandbox.ConversationRef,
) (sandbox.PendingRun, bool, error) {
	runs, err := s.ListActiveForSeat(ctx, handle)
	if err != nil {
		return sandbox.PendingRun{}, false, err
	}
	reply := events.New(types.ExternalNotification{Body: "use main"}, events.TraceContext{})
	got, ok := sandbox.Reply{Conv: conv, Events: []*events.Event{reply}, Writer: requester, Says: true}.Best(runs)
	return got, ok, nil
}

// requester is the seat whose wake started every run this suite parks
// ([run]), and so whom each of its questions is put to.
const requester = "founder"

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

// suspendedState is a stand-in for a parked turn's serialized state. The
// nineteen-digit argument is the value a lossy store gets wrong: it is past
// 2^53, so any decode of it into a float64 on the way through comes back as a
// different number.
const suspendedState = `{"version":4,"iteration_history":[{"Calls":[{"Name":"tracker_update",` +
	`"Args":{"row":1234567890123456789}}]}],` +
	`"active_tool_names":["tracker_update","activate_tool"],"iteration":2}`

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
	return mustGetIn(t.Context(), t, s, turnID)
}

// mustGetIn is mustGet under ctx, for a closure that holds its case's context:
// the read goes under the context the case threads to every other call it
// makes, never one it makes of its own.
func mustGetIn(ctx context.Context, t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	got, ok, err := s.Get(ctx, turnID)
	if err != nil {
		t.Fatalf("get %s: %v", turnID, err)
	}
	if !ok {
		t.Fatalf("no run %s", turnID)
	}
	return got
}

// loggedRun is a run's row with the bridged tool log beside it, as a reader of
// both reads them: the row first, then its job's log
// ([sandbox.PendingStore.BridgeLog]).
type loggedRun struct {
	sandbox.PendingRun
	Log sandbox.BridgeLog
}

func mustLogged(t *testing.T, s sandbox.PendingStore, turnID string) loggedRun {
	t.Helper()
	return mustLoggedIn(t.Context(), t, s, turnID)
}

// mustLoggedIn is mustLogged under ctx, as mustGetIn is mustGet.
func mustLoggedIn(ctx context.Context, t *testing.T, s sandbox.PendingStore, turnID string) loggedRun {
	t.Helper()
	run := mustGetIn(ctx, t, s, turnID)
	log, err := s.BridgeLog(ctx, run)
	if err != nil {
		t.Fatalf("read the log of %s: %v", turnID, err)
	}
	return loggedRun{PendingRun: run, Log: log}
}
