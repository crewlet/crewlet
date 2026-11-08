# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`, `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`, `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`, `internal/executordomain.TestALateCaptureNeverRollsASessionBack`, `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`, `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`, `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`, `internal/executordomain.TestARemovedSeatStaysRemoved`, `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`, `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`, `internal/engine.TestTheExecutorLogFitsBesideTheOtherLogsAtTheOperatorsSmallestVolumes`, `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`, `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`, `internal/estate.TestEveryExecutorWriteIsTheEngines`, `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`, `internal/sandbox.TestEachLaunchOfATurnIsNumberedPastTheOneBefore`, `internal/sandbox.TestACapturesPlaceOnlyGrowsOnTheRow`, `internal/sandbox.TestACollectionTakesTheSessionBeforeThePauseAndKeepsItAfter`, `internal/sandbox.TestEveryCollectionTakesWhatItsJobLeft`, `internal/sandbox.TestAJobTakenOnceItHadEndedIsNotTakenAgain`, `internal/sandbox.TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd`, `internal/sandbox.TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd`, `internal/sandbox.TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim`, `internal/sandbox.TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt`, `internal/sandbox.TestALostRunsBoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAnEndWaitsForItsTurnsCaptureToLand`, `internal/sandbox.TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded`, `internal/sandbox.TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused`, `internal/sandbox.TestTheScrubReadsTheLoginAgainWhereItIsRefreshed`, `internal/sandbox.TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse`, `internal/sandbox.TestWhatABoxWasHandedIsKeptBesideItsRowForItsJobAlone`, `internal/executordomain.TestAnEndWhoseNewestCaptureWasNotKeptReleasesTheOlderOne`, `internal/executordomain.TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted`, `internal/sandbox.TestTheSwitchOffKeepsNothingAndStillEndsTheTurn`, `internal/sandbox/executor.TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox`, `internal/sandbox/executor.TestAPackOnDemandKeepsItsDigestInTheBox`, `internal/sandbox/executor.TestATranscriptLineStaysJSONAndLosesEverySecret`, `internal/sandbox/executor.TestTheDigestIsPOSIXCksums`, `internal/sandbox/executor.TestTheScrubHoldsNothingPastItsBounds`, `internal/sandbox/executor.TestATarLeavesTheBoxWithOnlyItsSessionsFiles`, `internal/sandbox/executor.TestAnExportIsOneDocumentKeptScrubbed`, `internal/sandbox/executor.TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed`, `internal/sandbox/executor.TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession`, `internal/sandbox/executor.TestATakeIsBoundedByTheArchiveItKeeps`, `internal/engine.TestAResumedTurnsEndIsRecordedWithItsOwnDecision`, `internal/engine.TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog`, `internal/engine.TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestALostTurnEndsAsLostOnTheExecutorLog`, `internal/engine.TestTheEndCarriesTheTurnsOwnDecisionAndBreach`, `internal/engine.TestTheEndSaysWhatTheEngineAnsweredAColleague`, `internal/engine.TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid`, `internal/engine.TestAStopsAccountReadsEveryRoundsCalls`, `internal/engine.TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds`, `internal/engine.TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped`, `internal/engine.TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn`, `internal/engine.TestAResumedTurnThatPanicsOnceBegunEndsFailed`, `internal/engine.TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose`, `internal/engine.TestAnMCPServersURLSecretIsHandedToTheScrub`, `internal/engine.TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives`, `internal/sandbox.TestARetiredSeatsRunIsFinishedWithItsTurnsEnd`, `internal/sandbox.TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded`, `internal/sandbox.TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt`, `internal/sandbox.TestTheSwitchOffAsksNoHostForALostRunsSession`, `internal/sandbox.TestASpoolACrashLeftIsSweptByTheTakesAfterItAges`, `internal/sandbox.TestASpoolNeverOutlivesItsCapture`, `internal/sandbox.TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded`, `internal/sandbox.TestTwoAttemptsAtOneEndingOnANodeFinishItOnce`, `internal/sandbox.TestALoginTheBoxGrewPastItsBoundRefusesTheCapture`, `internal/sandbox.TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt`, `internal/sandbox.TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt`, `internal/sandbox.TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail`, `internal/sandbox.TestAnEndingDecidedBeforeTheNextHoldersFenceIsFinishedByIt`, `internal/sandbox.TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer`, `internal/sandbox.TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled`, `internal/sandbox/executor.TestAHandedKeysArmourStaysForTheShapePass`, `internal/sandbox/executor.TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack`, `internal/redact.TestALineIsCutAtTheArmourAKeyIsReadBy`, `internal/procgroup.TestASignalReachesOnlyALivingMemberOfTheGroup`, `internal/sandbox.TestADirectBoxEndsOnlyAProcessOfItsJob`, `internal/sandbox.TestADirectBoxKillsEveryProcessOfItsJobAndKeepsTheBox`, `internal/sandbox.TestAContainerBoxEndsAndKillsItsJobInsideTheContainer`, `internal/sandbox.TestAContainerJobThatRecordedNoGroupIsKilledByTheGroupItsExecLeads`, `internal/sandbox.TestAContainerJobWhoseGroupNoExecLeadsIsSignalledNothing`, `internal/sandbox.TestAContainerJobWithNoGroupRecordedIsReadOverOnlyOnceItIs`, `internal/sandbox.TestAnE2BBoxEndsAndKillsItsJobAsItsUser`, `internal/sandbox/executor.TestAnEndedClaudeCodeRunPacksAsItsOwnEndDoes`, `internal/sandbox/executor.TestAnEndedOpenCodeRunExportsItsSessionBeforeItsServerStops`, `internal/sandbox/executor.TestAnEndIsAskedOfThePIDTheScriptRecorded`, `internal/sandbox/executor.TestACollectionSaysItsJobWasEnded`, `internal/sandbox.TestAStopEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestALossEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestAJobThatOutlivesItsEndIsKilledBeforeItsTake`, `internal/sandbox.TestAJobAskedNothingIsKilledNotWaitedFor`, `internal/sandbox.TestAStopOfABoxOnAnotherHostEndsItsJobThere`, `internal/sandbox.TestAStopWithTheSwitchOffEndsItsJobOffTheFrame`, `internal/sandbox.TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake`, `internal/engine.TestABoxReadWaitsAsLongAsItsKindTakes`, `internal/sandbox.TestACappedJobIsEndedBeforeItsSessionIsTaken`, `internal/sandbox.TestACappedRunStillSaysItWasCutOffAtItsCap`, `internal/sandbox/executor.TestAnEndedOpenCodeJobsSessionIsTakenAsItsScriptExportedIt`, `internal/sandbox.TestACapsVerdictOutlivesAReadCutShortAsItEndsTheJob`, `internal/executordomain.TestABeginHoldsTheBaseItsReadNamed`, `internal/executordomain.TestABeginAndAnEndOfItsBaseAreOrderedByTheLog`, `internal/executordomain.TestNoReleaseTakesAHeldBase`, `internal/executordomain.TestTheHoldLiftsAtTheHoldersFirstCapture`, `internal/executordomain.TestTheHoldLiftsWhenTheHolderEnds`, `internal/executordomain.TestATurnThatNeverCapturedAbsorbsNothing`, `internal/executordomain.TestTheRetentionListsNoHeldRow`, `internal/executordomain.TestAPageNamesEachConversationsHead`, `internal/executordomain.TestABeginTheRowsCannotTakeIsRefusedAtTheDecide`, `internal/executordomain.TestABeginSaysWhetherTheLogHoldsItsBase`, `internal/executordomain.TestACapturesContextAndCutOffReachTheRow`, `internal/executordomain.TestTheExecutorStatementsArePlanned`, `internal/sandbox.TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff`, `internal/executordomain.TestABeginHoldsOnlyTheSessionItsReadSaw`, `internal/executordomain.TestALateBeginRecordsTheReadAndHoldsNothing`, `internal/executordomain.TestABeginOfATurnThatEndedWritesNothing`, `internal/executordomain.TestABeginNoReadHandsOutIsRefusedByItsWriter`, `internal/executordomain.TestABeginThatHoldsNothingSaysWhy`, `internal/executordomain.TestAHolderWhoseEndWasLostIsEndedLostAndItsHoldLifts`, `internal/sandbox.TestATurnsFirstLaunchContinuesItsConversationsHead`, `internal/sandbox.TestTheFreezeIsWrittenAtTheFirstLaunchOnly`, `internal/sandbox.TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase`, `internal/sandbox.TestWhetherABaseIsContinuedIsOneDecision`, `internal/sandbox.TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone`, `internal/sandbox.TestABaseIsContinuedOnlyInABoxOfItsOwn`, `internal/engine.TestAFrozenReadFitsItsBound`, `internal/engine.TestATurnReadsItsConversationOnce`, `internal/engine.TestTheFrozenBaseOutlivesASiblingsEnd`, `internal/engine.TestAnEndingLeftOutOfTheBriefIsNotAbsorbed`, `internal/engine.TestATurnThatNeverLaunchedTellsTheNextTurnHowItEnded`, `internal/engine.TestAnAbandonedFirstLaunchEndsItsTurnOnTheLog`, `internal/engine.TestTheSwitchOffCarriesNothingBetweenTurns`, `internal/agent/prompts.TestTheEndingsKeepTheNewestWholeAndCountTheRest`, `internal/sandbox.TestWhatAReaderIsToldOfAnEndedTurnIsTheNextTurnsOwnDecision`, `internal/sandbox.TestARelaunchOfATurnThatKeptNothingIsSaidToContinueItsBase`, `internal/api/queries.TestARowSaysWhetherTheNextTurnContinuesIt`, `internal/api/queries.TestARowReadAgainIsDecidedOnTheHeadItsOwnReadNamed`, `internal/engine.TestAnUnreadableConversationIsLessContextNotAFailedTurn`, `internal/schedule.TestEachFireOfAScheduleIsOneConversationOfItsRunner`, `internal/schedule.TestTheLongestScheduleNameFitsTheLogsKey`, `internal/org.TestAScheduleNameIsBoundedAsItsKeySpellsIt`, `internal/notify.TestAScheduleConversationIsKnownByItsPrefixAlone`, `internal/engine.TestAScheduledFireContinuesThePreviousFiresSession`, `internal/engine.TestFiresOfOneScheduleThatWaitedTogetherRunOnce`, `internal/engine.TestAFireIsNeverTakenAsTheAnswerToTheLastFire`, `internal/api/queries.TestARunNoChatCanAnswerSaysSo`, `internal/sandbox.TestAScheduleRowIsToldItsNextFire`, `internal/api/queries.TestAScheduleRowSaysWhetherItsNextFireContinuesIt`, `internal/executordomain.TestAConversationsHeadIsTheTurnThatReadItLast`, `internal/executordomain.TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued`, `internal/executordomain.TestAHeadReleasedBeforeAStaleSiblingHandsTheNextTurnTheSibling`, `internal/engine.TestTheNextFireContinuesTheSchedulesLineNotTheFireThatEndedLast`, `internal/executordomain.TestAnEndThatRanOutOfContextLeavesTheConversationNoHead`, `internal/executordomain.TestARetiredSessionGoesFromTheBaseWhereNoCaptureLanded`, `internal/executordomain.TestARetiredSessionGoesFromARowALiveTurnHolds`, `internal/executordomain.TestASessionStartedAfterRunningOutIsTheNextHead`, `internal/executordomain.TestAnEndThatFailedAnyOtherWayKeepsItsSession`, `internal/executordomain.TestARetirementReachesNoOtherSessionOrConversationOrSeat`, `internal/executorkeep.TestAnEndingsExhaustedSessionsAreBounded`, `internal/sandbox.TestAJobThatRanOutOfContextIsRecordedAndKeepsNoCapture`, `internal/sandbox.TestACappedRunThatAskedAndRanOutOfContextKeepsNoCapture`, `internal/sandbox.TestARelaunchContinuesNoKeptCaptureOfASessionThatRanOut`, `internal/sandbox.TestARelaunchContinuesNoBaseThatRanOut`, `internal/sandbox.TestARelaunchContinuesNoCopyInItsBoxOfASessionThatRanOut`, `internal/sandbox.TestAFreshSessionAfterRunningOutIsContinued`, `internal/sandbox.TestAReaderIsToldASessionThatRanOutIsNotContinued`, `internal/sandbox.TestATurnsEndNamesTheSessionsItRanOutOfContextIn`, `internal/engine.TestATurnAfterASessionRanOutOfContextStartsItsOwnSayingWhy`, `internal/executordomain.TestAnEndWhoseCaptureGoesWithItAbsorbsNothing`, `internal/executordomain.TestARetiredRowSaysSoWhereItsNewestAttemptWasNotKept`, `internal/executordomain.TestASeatsSearchFindsEverySessionHoldingEveryWord`, `internal/executordomain.TestASearchMatchesWhatAReadWouldShow`, `internal/executordomain.TestASearchThatStopsAtItsBoundSaysWhereAndHowFar`, `internal/estate.TestEveryExecutorOperationCrossesWhole`, `internal/estate.TestTheExecutorFacadesAskExactlyTheExecutorOperations`, `internal/executorread.TestASessionReleasedWhileItIsReadIsSaidAsReleased`, `internal/executordomain.TestASeatReadsNoOtherSeatsSession`, `internal/sandbox/executor.TestAMatchIsDecidedOnWhatItsReaderIsShown`, `internal/sandbox/executor.TestTwoRecordsThatRedactAlikeAreReadAlike`, `internal/sandbox/executor.TestAFilterHandsOutEveryPartItCouldNotRead`, `internal/agent/builtin.TestASeatReadsOnlyItsOwnSessions`, `internal/agent/builtin.TestTheSwitchStopsEveryModelsReadsAtItsNextCall`, `internal/agent/builtin.TestAPersonIsNeverOfferedASeatsSessions`, `internal/agent/builtin.TestReadingASessionWritesNothing`, `internal/agent/builtin.TestASessionListedThenReleasedIsReadAsReleased`, `internal/agent/runner.TestTheReviewerIsOfferedNoSessionTool`, `internal/engine.TestTheSessionToolsAreOmittedWhereNoSessionIsKept`, `internal/engine.TestEveryBarrierDeclaresItsReadModel`, `internal/e2e.TestAnExecutorReadsItsSeatsSessionOverTheBridge`, `internal/sandbox.TestARelaunchIsToldOfABaseItRanOutInWhileItsRowHoldsIt`, `internal/sandbox.TestACaptureWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow`, `internal/sandbox.TestAnEndDoesNotWaitOutACaptureWhoseTakersLeaseLapsed`, `internal/sandbox.TestADrainingTakersCaptureIsNotSettledUnderIt`, `internal/sandbox.TestATakerLeaseHeldAtAnotherEpochIsGone`, `internal/sandbox.TestAStoreThatCannotAnswerNeverReadsATakerAsGone`, `internal/sandbox.TestAGoneTakersCaptureIsSettledOnlyByAnAnswerFromTheLog`, `internal/sandbox.TestARelaunchSettlesACaptureWhoseTakerIsGoneBeforeItDecides`, `internal/sandbox.TestAnUnresolvedCaptureIsAskedAgainBeforeARelaunchDecides`, `internal/sandbox.TestACaptureARelaunchsAskLandsIsTheTurnsOwnToRead`, `internal/sandbox.TestARelaunchsAskOfAnUnansweredRecordEndsWithinItsWait`, `internal/sandbox.TestAFencedKeepUploadsAndRecordsNothing`, `internal/sandbox.TestATakerLeaseOutlastsTheDrainAndIsReleasedAfterTheLastKeep`, `internal/sandbox.TestATakeWithNoTakerLeaseBeginsNoCapture`, `internal/sandbox.TestAStampWithNoTakerIsSettledByAge`, `internal/sandbox.TestAKeepIsFencedOnceItsLastRenewalNoLongerCoversIt`, `internal/sandbox.TestATakeStampsTheTenureItReservesItsPlaceUnder`, `internal/sandbox.TestAnAttemptAtAKeepsRecordEndsWithTheWindowItWasSentIn`, `internal/sandbox.TestASettleLeavesWhatItsTakerStampedSince`, `internal/sandbox.TestATakerLeaseIsNeverAskedForLongerThanTheStoreKeepsOne`, `internal/sandbox.TestAStopUnderARenewalSaysNothingOfTheStore`, `internal/sandbox.TestACoordinatorNeedsItsCollaborators`, `internal/sandbox.TestACaptureRecordedAfterItsSettleIsKeptThroughTheTurnsEnd`, `internal/sandbox.TestARelaunchStartsWithoutACaptureItsRecordSettledRefused`, `internal/sandbox.TestARelaunchOfATurnThatFrozeNoBaseReadsItsRowForACaptureItsRecordRefused`, `internal/sandbox.TestARelaunchThatWillNotWaitOutAKeepContinuesTheBaseTheTurnStillHolds`, `internal/executordomain.TestAnEndsUnkeptCaptureStandsOnlyWhereItIsNewerThanTheRowsPlace`, `internal/api/queries.TestAnEndingSendsAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`, `internal/engine.TestAnEndingTellsTheNextTurnAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`, `internal/executordomain.TestARowIsShownWithNoUnkeptCaptureItKeeps`, `internal/coord/kv.TestALapsedTakerLeasesKeyIsRemoved`, the `internal/coord/coordtest` contract case `every_taker_lease_mints_from_its_class_counter`, which every coordination backend runs, `internal/engine.TestTheEnginesCoordinatorHoldsThisProcesssTakerLease`, `internal/engine.TestTheSandboxIsStoppedOnlyByTheTeardownAfterTheDrain`, `internal/engine.TestASandboxsTakerLeaseIsGivenBackOnTheStopsAllowance`, `internal/engine.TestATokenThePollRenewsIsRecordedBeforeItIsWritten`, `internal/engine.TestARenewalTheStoreDidNotTakeIsNotWritten`, `internal/sandbox.TestACaptureTakenAfterARenewalHoldsNoRenewedValue`, `internal/sandbox.TestATokenIsRecordedOnlyInTheSetOfTheJobItsRowHoldsRunning`, `internal/sandbox.TestATokenTheLaunchWritesInAKeptBoxIsSealedWithItsJob`, `internal/sandbox.TestAnIssuedDocumentIsScrubbedByEveryStringItHolds`, `internal/sandbox.TestAServiceAccountKeysPublishedConstantsStayInItsSessions`, `internal/sandbox.TestARenewedValueIsDroppedAnHourPastItsExpiry`, `internal/sandbox.TestAnOlderBuildsScrubTakesTheRenewedValues`, `internal/sandbox.TestARenewalPastTheSetsBoundIsWrittenAndRefusesEveryLaterCapture`, `internal/sandbox.TestARenewalTheStoreDidNotAnswerIsKeptOutOfTheBox`, `internal/sandbox.TestALaterBuildsKeysInTheHandedSetSurviveARenewal`, `internal/sandbox.TestAWaiterThatRefreshesIsRefusedWithoutARecorder`, `internal/engine.TestBothWaitersKeepARunsTokenFileCurrent`, `internal/executordomain.TestAnEndWhoseTurnWentOnPastItsSessionKeepsIt`, `internal/executordomain.TestAnEndStampedBelowFourIsAppliedByTheRuleItWasWrittenUnder`, `internal/executordomain.TestARowKeptUpToASessionItsTurnWentPastSaysSo`, `internal/sandbox.TestAnEndOfATurnThatEndedOnASaveSaysItWentOn`, `internal/sandbox.TestAnEndWithoutWentOnNeverCarriesAnUnkeptCaptureNewerThanItsRow`, `internal/engine.TestAHeadItsTurnWentOnPastIsHandedTheNextTurnAsOne`, `internal/agent/runner.TestARunContinuingASessionItsTurnWentPastIsLedSo`, `internal/agent/prompts.TestANextTurnOnASessionItsTurnWentPastIsToldSo`, `internal/agent/prompts.TestACutOffSessionIsSaidTrueOfASaveAndOfAStop`, `internal/sandbox.TestAnEndOfATurnWhoseLaterLaunchKeptNothingSaysItWentOn`, `internal/sandbox.TestACaptureTheLogTookAfterARelaunchSettledItStaysTheTurnsSession`, `internal/executordomain.TestARowThatNamesNoSessionSaysWhichOfThreeReasons`, `internal/sandbox.TestALastWordTheLogTookAfterItsSettleIsReadAsTheTurnsLastWord`, `internal/executorkeep.TestAnEndingsLastWordIsPlacedFromTheFirstCapture`, `internal/executordomain.TestALiveRowNamingASessionReadsAsInProgress`, `internal/api/queries.TestARowKeptUpToASessionItsTurnWentPastCarriesItsNote`, `internal/api/queries.TestTheSessionsScreenReadsWhatTheAnswersSend`, `internal/agent/builtin.TestASeatIsToldOfASessionInProgressOrOneItsTurnWentPastAsAPersonIs`, `internal/executorread.TestALiveRowWithNoSessionYetIsSaidAsSuch`, `internal/executorread.TestASessionKeptAgainSinceItsCursorIsReadAgainInItsRowsWords`, `internal/sandbox/executor.TestARestoreCutsATornLastLineOfEverySubagentThread`, `internal/sandbox/executor.TestTheFileEditSnapshotsAreNamedSo`
- **Measured:** a harness's session of a day's work compresses to one or two mebibytes and is bounded here at 32 MiB; at the reference company's 3 000 turns a day the log that names the sessions grows about 32 MB a day, about a kibibyte of it each turn's first read of its conversation (its `begin`), while the sessions themselves would be gigabytes; crewlet-operator's smallest data volumes leave 448 MiB (12Gi with the objects in S3, and 16Gi beside the nats bucket) above the other logs at their floors, and this log, scaled to its 64 MiB floor there, still fits both — the minimum classes do not move, but the least a fresh node boots in rises by 64 MiB of logs (12348030975 bytes of broker on nats, up 85.3 MiB with the bucket's quarter beside it, and 64 MiB on S3), which is what the operator's own floors follow. At that floor the log holds about two days at the reference company's rate, under the seven-day `min_age` a healthy trim keeps, so a company above about thirty seats on those volumes sets `stream.executor_log_max_bytes` or a larger volume, and `log_ceiling_short` says so
- **Tag-status:** unreleased

## The decision

A seat's executor session — the harness's own transcript, captured so a later
launch of the same turn, in any box on any node, continues it, and so a later
turn on the same conversation can — is stored as **one sealed object per capture in the fleet's object store**
(ADR-0026), under a key the store mints, and is named by **one row per
(seat handle, turn)** of the `executor` state-log domain on
`CREWLET_EXECUTOR_LOG`, the tenth domain of the register. Every record about a
turn is **arbitrated on its conversation** (the seat's handle and a digest of
the conversation's key, or of the turn's own id for a turn with none) and
declares the conversation, or the whole seat, as its scope, so every rule over
a conversation is decided from one snapshot of it in log order. The object is
sealed under the fleet keyring in a streaming format with associated data
binding it to `executor/<seat>/<turn>/<object key>`; a capture naming an
object older than `objstore.RecordWithin` is refused, a capture whose
(launch, capture) ordinals are not newer than the row's writes nothing, and an
ended turn is terminal.

What happens to a row is the domain's and nothing else's: a later turn's `end`
absorbs exactly the ended turns its first launch's read handed out — the head
it was handed, the endings it was told and the siblings it was offered, carried
on its begin or its captures — so what a conversation still holds is what no
ended turn was shown, and the next read shows all of it; a seat's byte budget
releases the oldest ended sessions; the company's conversation retention deletes
a row past its last instant, ended or not but never one whose run is live, and
first ends as `lost` a turn whose end never came, a day past its last record
(its newest capture, or its begin) with no run live, so a lost end holds the
seat's budget, or a base, for a day rather than the whole retention; and the
mailbox retirement deletes a removed seat's rows with one record, always
published, whose position every capture and end is held to, so one decided
before the removal never recreates a row for a seat that is gone.

Two departures from the plan this piece was cut from are deliberate. A
retention delete takes the turn's tombstone with its row rather than leaving
one, because a tombstone refuses a record decided before its row went and by
the retention none can still land: every capture is past
`objstore.RecordWithin`, and the duty never nominates a turn whose run is live;
a late `end` writes an ending-only row the retention takes in its turn. And an
end absorbs only what its turn was handed rather than every ended row at or
below the position its launch read at, because a read bounded at sixteen
endings and eight offered sessions would otherwise have its end delete turns
nobody was ever shown.

A person's removal does not reach these rows yet; a later record amends this
one with the erasure.

## Why the obvious alternatives are wrong

**A volume per node, or a PVC.** A session must reach the next turn's box,
which may be on another engine pod or on e2b; a volume would need every engine
pod to mount every other's, could not reach e2b at all, and would be a second,
Kubernetes-only store the object collector, its audit and the backup cannot
see — the store per node ADR-0026 already refused.

**The memory changelog.** A seat's memory is a compacted subject per row with
no ordering across subjects, and the rules here are over a whole conversation
in log order: which ended turn is the head, which rows a turn's end absorbs,
which attempt at one unit of work survives. A compacted log cannot decide
those, and it declares no barrier, so a launch could not read the head at
`linearizable`.

**The coordination store.** A value of mebibytes per turn in a bucket every
lease, ledger and counter of the company shares, held for the conversation
retention: the bucket is sized for kilobytes, and a full one stops the fleet
claiming seats.

**One row per conversation that each turn's capture takes over.** It reads as
simpler — the conversation's session is the row's object — and it is an object
carry: a row that stops naming one turn's object and starts naming another's,
which ADR-0027 (as ADR-0036 amends it) allows only inside one artifact, because
the collector judges an object by whether any row names it. Two turns running
on one conversation at once would also overwrite each other's session. A row
per turn names only its own object, and the conversation's head is a read.

## Amendment: every node reaches the log through the estate router, and only the engine writes it

A seat's box runs wherever its seat is held, a node without `data` among them,
and only a data node publishes, so the log is reached the way every other
domain of the estate is: through the estate router
([ADR-0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md)),
floored on this log. Each write is an idempotent operation under the id the
domain derives for its execution, so one a data node never answered is asked of
the next under that same id; each read is answered at the level its caller asks,
and a launch reads its conversation at `linearizable`, since a seat that moved
holds no floor for the end its last turn published through another node.

Its writes are a class of their own: ENGINE-ORIGINATED. A turn's first read
(its `begin`), a capture, an end, a retention
delete and a removed seat's are the engine's record of what its own runs did,
which no tool, no person and no operator surface asks for. Declaring the log to
the person's write surfaces, as the domains a surface writes are declared, would
have meant a refusal class and an object argument for writes nobody can make of
them, met only by borrowing an argument some operator tool takes for something
else. So the router declares who originates each write, the surfaces are held
to the domains a surface writes, and two gates hold the engine's class: every
write on this log is declared the engine's and nothing else is, and no package
that serves a person or a model names the writer the router hands out, the
domain's own writer or an engine hand-out of either. The second is a walk of
the source and sees names, not values, so a writer smuggled inside another type
is review's to catch. The reads are ordinary reads, which a surface may hold.
`internal/estate` states the class.

## Amendment: every launch's session is captured where its box can be read, and every turn's end is recorded

The sandbox runtime now writes the log (internal/sandbox's `session.go`).

**Captured wherever the box can still be read, on the node whose host it is
on** (ADR-0065), before the box is paused or reclaimed: a completion's
collection takes the archive the run's script packed when its harness exited,
or packs one now for a job that never got to — one its turn's cap ended while
it ran, one that failed before it packed, one lost with its engine whose box is
left; a person's stop packs one in the read that charges the stopped job; and a
loss whose box is still on this node's host packs one before its ending
reclaims the box. A box that is gone keeps nothing. The pack made on demand is
the runner's own (internal/sandbox/executor), the same pack the run's script
makes, so every pack keeps its archive's digest in the box.

**Scrubbed on its way out**, file by file, each read and written as what it is:
every secret the engine itself put in the box, by value, raw and as a JSON
string escapes it, then the redaction every text leaving a box passes — a
transcript line string by string, a document a token at a time, anything else
as text as the redaction settles it.
The scrub changes the archive's bytes, so the box records the kept archive's
digest too, and a session carried back into the box it came from is never
restored over the copy that box went on with.

**Ordered by the launch's ordinal**, which the write opening each launch mints
on the run's record (1, 2, …) beside its id, so every capture carries a
(launch, capture) pair that only grows. Each capture reserves its place on the
run's record before a byte moves, with the bridged call and the note the job
had been answered and told through, and is stamped there as landed, refused
(why) or unresolved — uploaded and its record unanswered, which the turn's end
asks again under the same operation, and which is refused once its object is
past `objstore.RecordWithin`. A record is asked through the estate router's
writes, the engine's own class, under the operation its turn, launch and place
derive, again under it for thirty seconds while the log does not answer.

**Every turn that launched its executor records its end** — at its resumed
turn's end, a person's stop of a resumed turn, an abandoned resume or one that
panicked once it began, and the run's own ending for every stop of a run —
parked, or before its turn could park — and for a loss, which no frame of the
turn reaches — with the turn's own decision and breach, whether its reply
reached the party waiting, and the engine's own answer to a colleague's ask.
How an end is owed, and when it is recorded, is the next amendment's.
`turn_engine.conversation_session.enabled: false` captures nothing and still
records every end.

A cross-node box read waits for the capture's take inside it, and for nothing
after it (the next amendment's). Restoring a capture is a later amendment's.

## Amendment: a session is taken before its box goes and kept off the read, and a turn's end is owed until the log takes it

The first capture amendment held a read, a stop and an ending for a whole
upload, recorded ends from frames that raced the stop's capture, and let a
refused capture leave the log naming an older session. Each is now decided as
follows (internal/sandbox's `session.go` and `handed.go`, internal/engine's
`executorsession.go`).

**A capture has two halves.** The TAKE reads the box and is all that happens
before the box is paused or reclaimed: the archive, packed where it must be,
streamed through the scrub and sealed under the fleet keyring into a spool on
the node's own disk, bounded by the pack's and the scrub's waits
(`SessionTakeBudget`). The KEEP — the upload at the object store's pace and the
record — runs after the read has answered, off its frame, on the coordinator's
own lifetime (`SessionKeepBudget`). A cross-node read waits for the take alone,
sized so the host's three-quarter share of the asker's wait holds it
(`BoxReadBudget`). A stop's read and a loss's capture are handed to the
ending's own retry rather than spent on the frame that decided them, and a
loss whose box is on another node's host is read there (`BoxReadLoss`).

**A job still working is not final.** A collection past its turn's cap packs
the job as it stands and marks the capture not final; a stop's capture is final
only where the job had ended. A pack that fails is an error the capture is
stamped refused with, never "no session": only a harness that wrote no
transcript, or an OpenCode that answers it holds no such session, has nothing
to take — an OpenCode that could not be asked is a pack that failed.

**What the scrub takes out is what the launch handed the box from a
credential position**, collected from the launch request itself — every value
a reference resolved to (in the run's environment, a setup file that is one
reference whole, an MCP server's url), every variable whose name says it
carries a credential and the entry's sign-in, its MCP servers' env and
headers, the run's telemetry token and its Gitlet credential, the seeded
login — and nothing else: the
seat's identity facts, a setting, a setup step's own script are no secrets,
and taken out by value they were lost from every kept transcript. Only the
engine knows where a value came from, so it says which are secrets
(`LaunchRequest.Secrets`). The set is sealed under the fleet keyring and
bound to the turn, and kept beside the run's row, named for the job, never on
it: the row is read whole by every git request of a coding box and every
listing, and only a capture and the next launch read the set. A job that
continues in the turn's box carries the previous job's set forward, since the
box and the session it resumes still hold those values; the login is read
again in the box and on the host at the capture. A job that has none beside
its row, a continued box whose row says nothing of what it was handed, or a
seal the node cannot open, refuses the capture, and so does
a login read again past what a launch may hand, or a set past four times that.
A key's PEM armour is never a value: every key of its kind carries it, and the
shapes read a key by it. Keys of JSON objects, members' names (by value and by
shape) and the tar's metadata are scrubbed too, and the scrub is bounded on
what it reads and holds, refusing a session past either.

**A turn's end is owed on the run's row**, written there by whatever decides
it, and recorded by the run's ending only after every capture of the turn has
settled — the end tombstones the turn — before the row's delete, which refuses
while an end is owed (`ErrEndOwed`); the seat's mail waits behind it, and the
end is asked again until the log takes it. It is DECIDED ONCE, and stays so
once recorded (`EndRecorded`): an attempt after the clear decides it again and
answers no colleague twice; and two attempts at one ending never run on a node
at once. A stop of a run — parked, launching, or before its turn parked — is
ended from the run's own ending, never from the frame that launched it, which
answers the colleague nothing. A relaunch that fails in its launch leaves the
turn's end to the resumed frame still running on the node, and the row waits
for it to owe it. A seat's retirement finishes its runs' endings itself, under
its own lease, rather than handing them to a seat host that no longer holds the
seat. An end a newer build owed, holding what this build cannot read, is left
owed for a node that reads it. Only a frame that finds no row left — a
dispatch's launch its own fence refused, a row the seat's next holder finished
— records its end at once, and owes it on the row instead where one is there
after all.

**The end carries the turn's newest capture where it was not kept**
(`executorkeep.Unkept`): applied, it releases the object an older capture's row
named (an end stamped below version 4; a later amendment's keeps that session
and says the turn went on past it), so the log never names an older session as
the turn's last — the end being recorded, on every path but a frame's that
found no row, by the run's ending that read the row's stamps. And where the
run's ending decides the end from the row and the run's bridged log alone — a
loss, a stop — it says whether the turn's reply is known to have reached its
party read off every round's successful calls, the earlier rounds' folded into
the log by the opening that starts a new round (`DeliveryUnknown` where the log
elided calls or could not be read); a turn its resumed frame ends says what
that frame's loop established.

## Amendment: a stopped or lost job is ended before its session is taken

The capture amendments took a stopped job's session packed as it stood, final
only where the job had ended, and a lost job's packed now with its job maybe
still working — a step in flight half recorded, a harness appending to the file
being packed. Now the read that takes a stopped or lost job's session first
**ends the job** (internal/sandbox's `endjob.go`, internal/sandbox/executor's
`end.go`), on the box's host (ADR-0065), and so replaces "a stop's capture is
final only where the job had ended": a stop's and a loss's capture is final
wherever the read ended the job.

**One request, to one process.** The engine sends `SIGUSR1` to the run's script
alone, by the pid the script recorded in its box (`.crewlet/job-script`, `1
<pid>`) — recorded only once the script traps the signal, so a request through
the file always meets its handler. The script ends its harness (`SIGTERM`, then
`SIGKILL` past `JobStopGrace`, five seconds; an OpenCode run first interrupts its
session through its own server, which settles the step in flight so the export
holds it), packs the session by the very code path its own exit takes, records
that the job was ended and how (`.crewlet/job-ended`), and exits with its
harness's own status; the collection never reads such a run as a success. The
read waits for the job to be over, at most `JobEndWait` (two minutes for the
pack and three graces, 135 seconds), then kills what is left of it (`KillJob`:
`SIGTERM`, a grace, `SIGKILL`, a grace) — a sweep after a clean end, the
escalation past the wait, and at once for a script that answers no request (an
older build's, one not yet at its first line). Only then is the session taken:
the script's archive where it left one, packed now where it did not, final.
Only a job the kill could not reach — a container job whose box can name no
process group of the job's own, which it signals nothing of — is taken as it
stands, not final, and a job its turn's cap ended is, as before, packed as it
stands and paused.

**`SIGUSR1`, not `SIGTERM`.** `SIGTERM` is the teardown's, reaching every
process of the job at once — an OpenCode run's server and its login exporter
with it — and an OpenCode script answers it by exporting its login beside a
server that is going away. An end needs the server alive to export the session
through, so it is a signal only the engine sends and only to the script, which
a harness never receives: an exec resets the trap.

**Two backend methods, not one helper over `Exec`** (`Sandbox.EndJob`,
`Sandbox.KillJob`). A `direct` job's pid is a host pid the job itself wrote
down, and the job runs as the engine's user, so the request is held to the job's
recorded process group, through a pidfd on Linux
(`procgroup.Leader.SignalMember`). A `container` job's group is known only to
the exec that started it, which reads it off `/proc` and records it on the
engine's side; its kill never sends `kill -1`, which would take the container's
main process from under the read. A job an older build started recorded none,
and its kill reads the group off the job's first process, where an exec's own
session leads it, as runc's does; a job whose group neither names is signalled
nothing at all. Its first process alone is no kill of it — that is the runner's
wrapper, whose children the run's script and its harness are — and is the
process the box reads the job's state by, so a kill of it alone had a job still
working read as ended, and its session taken final. An E2B job runs in envd's
own process group, which no signal may reach, so its kill is every process of
the box's user — envd runs as root, and the box is reclaimed right after.

**Off every frame, and with the session closed first.** Every stop of a job that
started in a box is read in the ending's own retry, whatever
`conversation_session.enabled` says — the switch governs what is kept of a
session, not how a stop ends a job — and a loss closes the run's bridge session
before its read, as a stop does, so a lost job still working stops acting as the
seat before its take.

**The read's budget holds the end.** `BoxReadBudget` is per read: a collection's
stays 460 seconds; a stop's and a loss's add `JobEndBudget` (the wait and the
kill, 145 seconds) at the host's three-quarter share, 653 seconds, so a host's
take after an end at its bound is still given its whole `SessionTakeBudget`. No
wire changes: a host's reply carries `Result.JobEnd` additively, an older
engine never sends the request, and a script of an older build is asked nothing
and killed.

## Amendment: a turn's first read holds the session it was handed

A conversation's next turn is to continue its head, and a turn whose first job
was lost before it kept a session of its own is to be relaunched into the same
base its first launch froze, on any node (crewlet#519). That base is an ENDED
row, and every release of an ended row could take it from under the turn:
another turn's end that was handed the same head absorbs it, an end of the same
unit of work dedupes it, a capture of any conversation of the seat releases it
past the budget, and the retention deletes it. Once no row names the object,
the collector deletes it at its first pass with the object's upload and its
key's minting both more than a day old — not "a day after the row stops naming
it", as the domain's first migration says, which is history and so is corrected
in the package doc instead — so an old base is gone within the hour.

**The hold is a record on the log: the turn's own row.** The run record that
knows the base lives in the coordination store, which the applier, a pure
function of the record and the rows, cannot read; and a release decided against
that store races the launch: the ending writer lists the seat's runs before the
launch has frozen the base, the launch reads and freezes it, and the end applies
and absorbs it. No node-local ordering closes that, since captures are kept off
the frame, endings finish wherever the seat is held next and the reaper runs
where its lease is. So a turn's first launch records its read in a `begin`, the
one new op, on the conversation's subject, under an operation its turn derives:
where it read the head, the turns the read handed out, and the base — the head's
turn, object and digest. Its apply repeats every guard of its decide in log
order: the record's shape, the seat's removal and the turn's tombstone, the
turn's row (a turn's first read stands, whichever record carried it), and the
base, held only where its row is still there, ended at or before the read, in
the conversation and naming the very session the read saw — otherwise the begin
records the read and holds nothing. The writer reads the row back and says
whether the base is held, and why not.

**While the turn's row is live and has landed no capture, it holds the base**,
through a partial index over exactly such rows (`executor_sessions_held_idx`,
migration 0077). An end's absorption; its dedupe, which still merges what a
held attempt delivered; a lost end, as any end; and the retention's delete each
ask, of each row they are about to take, whether a live turn holds it, and skip
it. The budget's release, on a capture of any conversation unless the capturing
turn is the holder, reads the seat's holds once before it walks the seat's
ended sessions, and skips each held one. All of it is decided in the apply,
never through a subquery the planner could rewrite into a read of the whole
table. The retention's listing reads every hold once and leaves held rows out,
so they never fill its page. The unkept release (only the ending turn's own
object) and a removed seat's delete (the holders go with it) are unchanged. A
begin and every end, lost end and retention delete of the conversation are
arbitrated on one subject, so the log orders them; a capture of another
conversation is not arbitrated against the begin, but its apply reads the holds
in log order, and its seat-wide scope contains the begin's, so a node that
defers one defers the other.

**The hold lasts until the holder's first landed capture, or its end.** From
that capture on, the turn's launches continue its own session, which holds
everything the base did, so that capture may release the base: what keeps
"releasing the head then loses nothing" true at the 33 MiB floor, where a hold
until the end would need a live session and its base to fit together. A holder
whose run is lost with no end is ended `lost` within a day, so a hold outlives
its holder by at most a day and a pass of the retention duty. A skipped row
stays an ordinary ended row, which the holder's own end absorbs in its turn, as
does the next end whose read showed it; while the hold lasts a later read can
show it a second time, which takes a second live turn of the conversation beside
a sibling that ended while the first still held — rare, and harmless.

**Only a turn that landed a capture absorbs and dedupes.** A turn whose row is
its begin's alone holds no session that could stand in for what its read handed
out, as a turn with no row at all does not, so the gate on an end's absorption
is a landed capture rather than a row.

**The siblings a read offers are not held, and opening one never holds it.**
A hold on a session a run opens would be a write the model's read triggers,
which this record bars, and it would contend with the reader's own capture.
A read of a sibling released meanwhile reads its row again and says that it
was released, rather than holding it.

**Record version 2.** The `begin`, and a capture's context and cut-off — the
whole input of its session's last request and whether its harness was cut off,
which only the run record held, deleted with the turn, and which a launch
continuing a session after its turn ended needs for its budget floor and its
brief — are rows of the domain's versioned fields at version 2, though no tag
has shipped any of them: a build reading version 1 would fault on the unknown
op and apply a capture without the two fields, and with the rows it retains both
until it can apply them. A capture recording neither stays at version 1. The
capture fills both from the run's stamp of it.

**A read names each conversation's head**: a page of a seat's rows names the
head of every conversation its rows name, and one row's read its
conversation's head, each from the snapshot the rows were read in — what a read
surface decides a row's standing by.

A turn's first launch publishes its `begin` (the next amendment), so the log
grows by one record of about a kibibyte per turn that read its conversation:
about 3 MB a day at the reference company's rate, the Measured line's 32 MB a
day in all.

## Amendment: a conversation's next turn continues its head, held by its first launch's record

A conversation's next turn now continues the session its last turn left
(internal/engine's `conversation.go`, internal/sandbox's `conversationbase.go`).

**The head is read once, at the turn's dispatch**, through the estate router at
`linearizable`, and only for a turn that serves a conversation of a company that
keeps sessions; a read that fails is logged, and the turn runs with no account
of its conversation and a session of its own. From that one read the turn's
brief is told how the conversation's earlier turns ended — the head always, then
the others newest first within twenty-four thousand bytes, the rest counted —
and what the brief told, and nothing it left out, is what the turn's end
absorbs: the head it was handed, whether or not a launch could continue it, and
the endings and siblings it was told.

**The turn's first launch freezes the read and publishes it.** The run's record
the first launch creates keeps the read (`PendingRun.Base`, written by the
create alone and kept by every reset), and the launch publishes the turn's
`begin` right after that record exists and its fence has passed, under the
operation the turn derives, asked for fifteen seconds on the launching frame: a
begin the log has not held by then leaves the launch continuing no base, and one
that lands later holds no longer than the turn's first capture or its end. Every
capture of the turn carries the read its first launch froze, so a capture made after a sibling
of the conversation ended still names the read the turn was handed.

**The base is continued only while the turn's row holds it, and only in a box of
its own.** The first launch, and a relaunch of a turn that has kept no session
of its own — its first job lost with its box, its captures refused or never
answered — on whichever node holds the seat, restore the frozen base into a new
box: never a head read again, and never once the turn has kept a capture, which
holds the base and everything after it. The launch reads the turn's own row,
where its own begin was not read back holding the base, and the base's row, at
`linearizable`, and takes the base up only while that row still names the object
the read froze; the restore carrier is the base turn's own object, opened
against that row's associated data. A box the turn's earlier job ran in holds
that job's tree and session, which are the turn's to continue in place. The
seat's budget pre-check counts the base of every live turn that holds one,
since the log cannot release it while the hold lasts.

**A turn of a conversation that ends in its own frame leaves its ending too.**
One that never launched and ended without an error — refused at the delegation
cap — and a first launch abandoned once its record was written record their ends
from the frame that dispatched them, so the conversation's next turn is told how
they ended, and an abandoned launch's hold lifts at once. A turn whose frame
failed before it launched records none: its delivery is handed back, to be parked
or run again.

**A seat's sessions say which launch continues each one, as that launch
decides it.** The read surface (internal/api/queries' `executor_sessions`)
decides a live turn's row by its relaunch's decision, and an ended turn's by
its conversation's next turn's: only the conversation's head is continued,
decided on the head the read that answered the row named from the same
snapshot, by the decision that turn's first launch takes of it
(internal/sandbox's `BaseContinuityOf`), for the member the node holding the
seat would launch that turn on — asked by no turn, since it is admitted under
the revision live when it comes. A row an end absorbed leaves the listing with
the turn that was handed it; one that names no session says why of three
reasons — nothing captured, its newest capture not kept, the seat's budget —
an unkept capture's own reason redacted; and a live turn that has kept nothing
says a relaunch continues the base it was handed, where its row still holds it.

**A hundred and fifty linearizable reads per seat-day**: the dispatch's head
read, the base's row at a first launch that continues it, two reads of the
turn's row at each relaunch, and one of the executor's own reads of the seat's
sessions (a later amendment's), at the reference company's thirty turns a seat
a day, a mean of one relaunch a turn and one session call a turn — the barrier
term the log's ceiling carries and `census_drift` holds it to.

## Amendment: a schedule is a conversation of each seat that runs it

A schedule's next fire now continues the session its last fire left
(internal/schedule's `FirePartition`, internal/notify's `ScheduleConversation`).
The scheduler stamps every fire with its schedule's conversation — `schedule/`,
then the schedule's scope, the scope's id and its name, each escaped as a URL
path segment — as both the fire's inbox partition and its conversation
identity, so this log files one seat's fires of one schedule as one
conversation of that seat, and a fire's dispatch reads, freezes, holds and
absorbs the schedule's head exactly as a conversation's next turn does. Nothing
here tells a schedule's conversation from any other: the log keys a seat's rows
on the conversation, whatever spells it, and the fires that waited in a seat's
inbox together run once, on that one key.

**The key is the schedule's identity and nothing else.** Not the runner, which
this log's rows already carry as their seat, so a unit schedule fanned out to
two seats is two conversations; and not the task, the cron, the zone or the
timeout, which an operator edits without meaning a new line of work. A rename,
a move between a role and a unit, and a role schedule on a company whose name
changed (the seat's agent id derives from it) each start afresh.

**The key fits this log's bound**, which refuses a longer key rather than cut
one, since a cut key would merge two conversations: a schedule's name is
bounded at 400 bytes as the key spells it (internal/org's
`ScheduleNameMaxBytes`), and the key spends at most 79 before the name.

**Nobody can reply into it.** Every fire of the schedule reproduces the key and
nothing a person writes does, so a delivery on it is never offered to a parked
run as its answer (internal/notify's `Derived`), nor is a fire's run answerable
in chat: the next fire would otherwise be recorded as the answer to the
question the last fire's run parked on.

**A conversation's head is the turn that read it last**, no longer simply its
newest end: of its ended turns whose rows still name a session, the one that
read the conversation last, the newest end among turns that read it at one
position, and a turn that read nothing (its read failed) after every turn that
read. A fire is never an answer, so a schedule's next fire runs beside a fire
of it still parked on a person's question, which frees its seat, every time one
parks: each later fire reads the schedule's head, continues it and ends, the
next continuing that one, and the parked fire, answered or stopped hours later,
ends after them all. As the newest end it became the head, so the fire after it
continued the session the schedule had left before it parked, and the sessions
the fires beside it had left, each continuing the one before, were told and
then absorbed without ever being continued. Every first launch continues the
head its read named, so a conversation's sessions are a line that every later
read built on, and a turn that read early and ended late continued none of what
the turns that read after it left: it is told and offered beside the head, as
every other ended turn is, and the next end absorbs it. Replicated migration
0149 adds the index the head's order seeks. The case was there for any
conversation whose turn ran beside a parked one, and a conversation's later
message is offered to the parked run as its answer first, so it was rare until
a schedule made it routine. Holding a schedule's later fires back while a fire
of it waits on a question was rejected: a schedule fires whatever its last run
is doing, and a question nobody answers for days would hold every fire of it as
long, to run once and days late.

**The seat budget still releases the oldest end first**, so under budget
pressure it can release a conversation's head before a stale sibling, a turn
that read the conversation earlier and ended later, and the next turn then
continues, of the sessions the conversation still keeps, the one that read it
last, and is told the released head's ending as it is told every other.
Releasing in the head's order was rejected: which sessions a capture releases
is decided by the applier on every
data node, so in a rolling upgrade a node releasing by end beside one releasing
by read would release different sessions for one capture, and the copies of
the replicated estate would diverge. The head's order is a read's alone: two
builds that answer two heads hand two turns two bases, each recorded on the log
as that turn's read, and diverge nothing. A released head costs the next turn
that session alone, and how its turn ended is still told.

**The read surface says it in the schedule's words** (`continued_by:
next_fire`), from the same decision a fire's first launch takes; internal/sandbox
is told by its caller which launch an ended row's is, because the rule that
knows a schedule's key is internal/notify's alone and the sandbox cannot import
it.

**The retention is a schedule's as it is any conversation's**: a schedule whose
fires come further apart than `retention_days` never continues, its last
session gone before its next fire. Sparing the newest session of a schedule the
fleet's revision still declares would make the reaper read the company's
schedules, and give one kind of conversation a memory the operator's retention
does not state. There is no per-schedule opt-out either: the company's switch,
its ceilings and the launch floor already bound what a continued fire spends.

**The read model already counts it.** A fire's dispatch now reads its
schedule's head, which the log's hundred and fifty linearizable reads per
seat-day assumed of every one of a seat's thirty turns a day, fires included.

## Amendment: a session that ran out of context is retired

A run whose conversation no longer fits its model's context fails on every later
launch of that session, so its failure is a fact about the session rather than
about the run. Nothing consumed it: the launch's record labelled it, the session
stayed the conversation's head, and every next turn continued it and failed the
same way, for as long as the thread lasted.

**It is recorded where each reader of it already looks.** The run's record holds
the sessions a job of the turn ran out in (`PendingRun.ExhaustedSessions`), written
as the job is collected and before its turn resumes, which every decision a
relaunch asks reads: its kept capture (`ContinuityOf`), its frozen base
(`BaseContinuityOf`) and the box's own copy (`continueSession`, `inParkedBox`). The
turn's end carries them to this log (`Ending.Exhausted`) as it carries a capture
that was not kept, which is record version 3, since a build applying that end
without reading them would leave its copy of the rows naming sessions no other
node's names.

**The log retires the session, not the row, and by its id.** The apply takes it off
every ended row of the conversation that names it, its own included: a turn that
continued its base and whose capture never landed leaves that base as the head
otherwise, and the next turn fails on it. Taking the object off a row is what every
reader already refuses, so no read learns a new rule. `retired_by` keeps the row's
account truthful, where it would otherwise read as a budget's release.

**The object goes, and the last capture is never made.** No launch would continue
it, the seat's budget would carry it at the expense of sessions that can be
continued, and the transcript's only other reader is an operator who has the run's
own record of what it did. So the job's collection takes no capture of it, and any
earlier capture goes at the turn's end. The collector deletes what no row names.

**A hold does not protect it, and its turn absorbs nothing.** A hold keeps a session
for its holder's relaunch to continue, which here would meet the same end. And
absorbing says a turn's session holds what its read handed out, which is not true
of one no later turn reads: those turns stay, and are told to the next, which is
what carries the conversation on. The same holds of an end whose newest capture was
not kept, whose row loses its older one the same way, and which until now absorbed
them with nothing left to stand in for them.

**The alternatives.** Compacting or truncating the session and continuing it is the
harness's own move, already tried by the time it reports this: Claude Code's
`rapid_refill_breaker` is its autocompact giving up. Retiring only the turn's own
row, or only the head, leaves the same session named on the row beneath it. A
setting for how many failures retire a session would make the second failure
something a company chooses to pay for, with nothing to gain: nothing changes
between launches of a session that no longer fits.

**Other sessions of the conversation stay.** A sibling that never ran out is
continued where it is the head, and if it runs out it is retired in turn, so a
conversation's run of failures is bounded by the sessions it holds. An end the log
never hears of this from, its run's record gone before its end was recorded, costs
one more failed launch, whose own end then retires it.

## Amendment: a job its turn's cap stopped is ended before its session is taken

The end-job amendment left one pack made beside a live harness: a collection
past its turn's wall-clock cap took the job's session packed as it stood — a
tar beside a live Claude Code, and OpenCode's own runtime (`--standalone`)
against the database the run's live server held, which could find it busy and
refuse the pack — and it was the pause after the read that stopped the job. The
cap is an end: the run is ended as if it had finished. So the collection's read
now ends a job still working past its cap as a stop's read ends one
(internal/sandbox's `readBox`, `endJob`), its bridge session closed first as
before, and its session is the script's own final pack, an OpenCode run's
exported through the run's own server, final wherever the job ended. This
replaces "a job its turn's cap ended is, as before, packed as it stands and
paused": only a job the kill could not reach is taken as it stands, not final,
and no other pack is made beside a live harness.

**Asked first whether it finished.** The read polls the job before it ends
anything, so a job that finished by itself before its cap's read is collected
as any finished job is: not swept, and not said to be cut off. A poll that
cannot say is no reason to leave the job working past its cap, and it is asked
to end all the same. And whatever `conversation_session.enabled` says, as a
stop's: the switch governs what is kept of a session, not how the cap ends a
job.

**Still cut off at its cap, recorded so before the end.** The run's result says
the wall-clock cap ended it, and the run is never parked on a question it asked
on its way out, wherever the read SAW the job working past the cap: a poll that
answered that it still ran, or its script hearing the request to end. The read
records that on the job's record the moment it sees it, before it asks or
kills anything (`LaunchRecord.EndedAtDeadline`, through
`PendingStore.MarkEndedAtDeadline`, under a context of its own as the kill is),
and every later collection of the job reads it there. A read cut short as it
ends the job — its node draining, the seat moving — kills what is left of the
job as a stop's does, and the kill can take the script before the script
records the request; a collection made again after it, or after a read whose
box could not be read back, would otherwise find a job over with nothing to say
why, read it as one that ended by itself and park it on its question. The job's
own record that a request ended it (`Result.JobEnd`) is read as the same sight
where the read's record did not land — only the cap asks a collected job to
end, since a stop's and a loss's ends are endings, which no completion
collects. Read off whether the job was over once its end was done, every capped
run would read as one that finished; and a poll that did not answer, with no
script that heard, has shown nothing, so a job that finished by itself is
never said to be cut off.

**Judged as its collection records it.** Whether the job ran out of its model's
context is decided on the run as its collection records it — no question, no
success — so a capped run that asked on its way out and ran out is refused its
capture untaken, as the amendment above says of every such job, rather than
taken and kept by the read and retired by the collection right after.

**No wire changes, and one key on the job's record.** The read is the
collection it was, and a host of an older build makes it as before. The job's
record gains `ended_at_deadline`, which an older build carries through its
writes (`LaunchRecord.Extra`) and does not read: a capped run such a build
collects again, while builds are mixed, reads there as a run ended on request,
naming no cause. Nothing after the cap runs a job in the box its collection
paused — the turn starts no round past its cap — so an E2B kill of every
process of the box's user takes nothing a later job needs.

**The read's budget holds the end.** `BoxReadBudget` takes the run as well as
the read: every collection of a turn with a cap waits as a stop's and a loss's
read do, 653 seconds, whether or not the cap has passed on the asker's clock,
since the host decides on its own as it reads; a collection of a turn with none
waits 460, as it did. While builds are mixed, an asker of an older build still
waits 460 seconds on every collection, so a newer host's take after an end at
its bound is cut short of its budget there, and that capture is stamped
refused; this lasts only for the upgrade's window.

## Amendment: a seat reads its own sessions, and a read holds nothing

A seat's executor finds and reads its seat's other sessions itself, through two
built-in tools on the seat's registry (internal/agent/builtin's `sessions.go`):
`list_sessions`, the seat's rows newest begun first, or a search of their words,
and `read_session`, a page of one session's transcript.

**The seat's own, by the turn.** Neither tool takes a seat: each reads the seat
of the turn its call is bound to, which over the tool bridge is the run's own
seat, set as the call's principal by the node holding it whatever the request
carried. Another seat's turn id reads the calling seat's row of that turn, which
does not exist, and is answered as a turn id nobody has. The reviewer's surface
offers neither, no person's surface registers them, and their authority rows are
an agent seat's own verbs of the self class. They read whoever the reading turn
acts for, as a seat's diary and its past turns are read: a transcript keeps no
record of which entry needed whose permission.

**Rows at the seat's level, through the router; the search where the rows are.**
Each call is one read of this log at `linearizable`, through the estate router's
reads, which this log's read model counts. A search runs on the node holding the
rows (`executor.search`, `Reader.Search`): a page of the seat's rows examined,
at most a thousand, matched whole as the engine's lexical search matches a word
against the row's own words as its one redaction leaves them — never a word of a
marker the redaction wrote (`Session.Redacted`, which every reader of a row
shows it through) — and only the hits sent back; a page says how far it
searched, and, where the copy that answered could not read every record, that
sessions may be missing from it. A data node of an older build answers the
operation unknown and unserved, and the router asks the next.

**The object opened on the holder, within the process's slots, one for
seats.** A page of a transcript is read by the one reader every surface reads a
kept session by (internal/executorread), on the node holding the seat, held to a
seat's bounds and inside the process's two transcript slots, of which seats hold
at most one between them, within two minutes a call. A read for words decides
each hit on what the entry's whole text redacts to, never on the text as the
harness recorded it, and every entry states the size of that redaction, never
its size as recorded to the byte, so neither whether an entry is a hit, where a
page full of hits stops, nor the size an entry states confirms anything the
redaction took out; and a part of the record the read could not read is handed
out whatever the filter asks, never passed over as if searched.

**Fenced as a harness's words**, under `executor_session` (ADR-0040's
amendment).

**Omitted where the switch keeps nothing.** An epoch whose company keeps no
session registers neither tool, the sessions kept before the switch went off
included, which people still read as the seat's trail; nor does a node with no
object store or no estate router. And each call asks the switch of the revision
its node runs, so a launch already running when its node applies a revision
that turns it off is refused at its next call: the switch stops every model's
reads, not only the next launch's.

**A read holds nothing.** Opening a session writes no record and changes nothing
a release reads: a hold taken on a model's read would be a write a model's
surface originates, which the first amendment bars, and held sessions would keep
the budget from releasing for the reader's own capture. A session released
while it is listed or read — absorbed by a later turn's end, released by the
seat's budget, retired by the end of a turn that ran out of context in it, past
the retention, captured again — is answered from its row as released: a read
whose object fails reads the row again at `linearizable`, and says what the row
says now, and only where the row still names the object is the failure the
store's or the archive's own. What a read handed out stands. The first launch's
base hold (above) is this log's only hold.

**A session that ran out of context is read until an end retires it.** Until
then the rows that named it still do — the base the turn continued, or an
earlier capture of the turn's own — so a relaunch of the turn it ran out in,
which starts a session of its own, is told it may read it. An end that retires
it (the amendment above) takes it from every ended row that names it, a held
base included, and a read of such a row then says so; the seat's later turns
are told how that turn ended.

## Amendment: a capture names its taker's lease, and one whose taker's lease lapsed is settled from the log

A take dies with its process. Its place stays stamped begun on the run's row,
its spool goes with that process's disk, and a stamp read by its age alone held
the turn's end for as long as a capture can take (`SessionCaptureBudget`, about
twenty-one minutes), and sent the relaunch that followed it to a session of its
own, beside a record the log may well have taken. Each is now decided as
follows (internal/sandbox's `session.go`, `taker.go` and `restore.go`).

**Every process holds a taker lease of its own** in the coordination store, a
class of its own (`coord.ClassTaker`): the whole company, now, since any node
may wait on a capture another process began. Its resource names the process
incarnation, `taker:<node>:<uuid>`, so a restarted process claims its own at
once rather than waiting out its predecessor's. It is claimed as the sandbox
coordinator is built, for the seat lease TTL the deployment configures — so a
stopped taker is read as gone as fast as its seats move — and never past
`coord.MaxDutyTTL`, the longest lease the store keeps outside seats and
presence, which refused every claim asked for longer. It is renewed four times
a TTL (`coord.KeptRenewals`, the count the engine's kept duties renew by): a
keep goes on only inside the window its process's last confirmed renewal
covers, and renewed a third of a TTL apart that window held two intervals, so
the renewal after one the store did not answer was asked as the window closed,
and one blip refused every take and fenced every keep under a lease no peer
could yet read as lapsed. It is held through a drain until the coordinator's
stop has seen its last keep end, and released then, through the engine's lease
store for give-backs no stop's context reaches (`holdLeases`), so its release
is a step of the stop's one allowance; a process that dies leaves it to lapse
on its TTL. Its key is kept in the duty bucket, whose age removes it three
hours after its last write, and its tokens come from one counter for the whole
class (`coord.CounterResource`), so nothing a process start leaves in the store
accumulates: a counter per taker would be a record per process start in the
ageless epochs bucket.

**Every stamp of a take names that lease** (`SessionCapture.Taker`, its holder,
and `SessionCapture.TakerEpoch`, its fencing token), written by the take and
kept by every later stamp of it, and read again right before the take reserves
its place: the pack and the scrub take minutes at the bounds, and a lease
claimed afresh meanwhile is the tenure the keep goes on under. The holder is
held to 128 bytes, the bound the row's budget counts its owner at; the token is
an integer, as the owner's is. Both are wire-additive, carried in the stamp's
`Extra` by a build that does not know them, and live on the run's row alone:
the executor log's record does not carry them, since only a wait for a capture
asks them. A process holding no taker lease whose last renewal still covers it
— its claim, or its renewals, unanswered — begins no capture at all.

**A taker is gone where that lease has lapsed or is held at another epoch, and
nothing else.** One coordination read decides it, three-valued: a store that
cannot answer is never a taker gone. The end's wait and the relaunch's ask it
of a begun capture at every pass, and settle one whose taker is gone from the
turn's row at `RelaunchRead`, linearizable, so the read has seen every record
the log took before it: landed from the row where the row names the capture's
place, refused ("its taker `<node>` stopped before it was kept") where it does
not. The settle is written only over the begun stamp it was decided from
(`PendingStore.SettleBegun`, decided on the row as it stands): a stamp the
taker wrote meanwhile stands — an unresolved one among them, whose record was
sent and may yet land, and which a wait asks again under its own operation
rather than refuses — and the taker's own stamp coming after the settle is
refused by the row's rule, a settled place never taken back. A relaunch also
asks an unresolved capture again under its own operation before it decides, as
the turn's end asks it: asked by the end alone, it left a relaunch reading a
record the log had taken as one never answered, and starting a session of its
own beside it. The `launch_node_gone` verdict comes fifteen minutes or more
after a loss, long after a lapse, and is no second definition.

**The fence is mandatory.** A keep checks its own lease before its upload and
again before every attempt at its record, from its own renewals rather than a
store read: held at the token its stamp names, and inside the exclusive window
its last confirmed renewal covers (`coord.ExclusiveThrough`, one renewal
interval its margin) — and every attempt at the record is cut at that window's
end, since one bounded by the record's thirty-second budget alone could land
long after the window closed. A keep its lease no longer covers uploads and
records nothing more, stamps the capture refused — or unresolved where a
record of it was already sent and not answered, which a wait asks again under
its own operation rather than settles — and says so (`executor_session_fenced`,
beside the `executor_session_not_kept` every capture not kept logs). So no
attempt the taker makes outlives the window, which closes a renewal interval
before any peer can read the lease lapsed.

**What the fence cannot reach** is a record the taker had already handed on
before its window closed — to a data node the router asked, or to its broker
connection's buffer while the broker was unreachable — and that the log takes
only after a peer has read the turn's row. On the turn's end the log orders the
two itself: a record it took before the end is the row's place and kept, one
after it is refused by the end's tombstone, and the log never contradicts
itself. **That is decided, and no record of the settle's is added to the
log:** a refusal ordered there, as the end's tombstone is, would have the
applier refuse the late capture and lose a session, and no session history is
given up for a stamp's sake. So a late capture the log takes before the turn's
end is kept and read as any other — the turn's row names it, a session read and
the conversation's next turn take it — and one decision alone goes by the stamp
once the log names the capture: the relaunch that follows the job whose
capture was settled. A relaunch continues only what the run's record keeps,
and the record never keeps a place it holds refused, so every attempt at that
relaunch — the one whose wait settled it, and one asked again after the record
landed — starts without the capture, and the read surface's account of it says
the log keeps the capture rather than that it was not kept, as the relaunch's
own reason does, which reads the turn's row for a capture its record refused;
the relaunch's run, starting without it, is named the turn's own as a session
it may read (`ContinuedSession.KeptTurn`), the capture being there. Nothing else
treats the stamp as authoritative once the log names the capture: the end
names it unkept at the row's own place, which releases nothing, and an account
of how the turn ended tells a capture as not kept only where it is newer than
the row's own place (`executordomain.Session.Unkept`, which the one form every
reader is shown a row in, `Session.Redacted`, reads it through — the read
surface's ending, a seat's own `list_sessions` and `read_session` and their
search, and the account the conversation's next turn is told: each had told
the end's unkept capture as the end named it, and so called a session the row
keeps lost). That relaunch starts without it as a relaunch does that will not
wait out a slow keep (`relaunchCaptureWait`): in the base the turn's first
launch froze, where the turn has kept nothing and its row still held that base
when the relaunch read it — the late record landing only after — and otherwise
in a session of its own, or the one its job's box holds where it carries the
run on in that box. A keep still under way past that wait goes the same way:
it is no kept capture, so its relaunch continues the frozen base where the
turn has kept nothing, rather than starting fresh beside a base its row still
holds. Should the capture land after the relaunch read the
row, the relaunch's first landed capture supersedes it, and what that job did
is lost to the relaunch whichever session it starts in.

**A stamp with no taker**, an older build's, is waited for by its age alone.

**The alternatives.** Settling on the taker's node holding no presence, or
holding it under another incarnation — the rule this amendment was first
written to — was withdrawn before it shipped: a draining node gives its
presence up at once while its turns finish and their captures upload, so an
end that settled such a capture as refused tombstoned the turn before the
draining node's record landed, the record was refused, the turn's last session
was lost, and the end's unkept capture let the older one go too. Presence says
whether a node may take new work, not whether a process still keeps what it
began, and narrowed to another incarnation it left a node that never returns —
a scale-down, a lost VM — and a dead `e2b` waiter duty's holder on the
twenty-one-minute wait. Letting the run's loss verdict alone settle the stamp
fails the `e2b` box, whose captures are taken by whichever process held the
waiter duty, which the verdict cannot know, and only a reader of the log can
tell landed from not. One taker lease per node rather than per process would
have a restarted process wait out its predecessor's before it could keep
anything. Renewing on presence's cadence, a third of a TTL, is the blip above;
giving the lease back on a bound of its own spent that bound past the stop's
one allowance on a node whose store had gone.

## Amendment: a token the poll renews in a running box is scrubbed from its run's captures

The scrub's values were collected at the launch and the login read again at the
capture. A token the engine issues the seat — an AWS session, renewed every ten
minutes; a GitHub or Google Cloud token, about once an hour — is rewritten in
the box's files by the completion poll while the run works, and was in neither:
read again at a capture it would be another issue, never the token the box
holds, and the shapes do not name an AWS session's secret or its token printed
bare. So every capture of a run past its first renewal could keep a live token.

**A value is recorded before it is written** (internal/engine's `issued.go`,
internal/sandbox's `handed.go`). The poll's refresh hands each renewed token,
with the expiry its issuer gave it, to a recorder before it writes the first
file with it, and writes no token the recorder refuses: the store did not
answer, and the next poll asks again; or the row no longer holds the job
running, and nothing more is written for it. On the poll the token is added to
the job's sealed set beside its row (`PendingStore.AddHanded`, a read of the
row that finds the job running with its ending undecided, then one
compare-and-swap of the part); at the launch of a job that continues in its
turn's kept box, whose refresh runs before the job's own set is written, it is
sealed with the job. So the set holds every value before the box does, and a
capture taken once its job has ended is scrubbed of every one; a capture of a
job its box would not end, taken as it stands, reads the set before its pack,
so a token the poll renews while that pack is made goes only by its shape.

**The set is no longer read by a capture and the next launch alone.** Every
node that polls the box reads the part, opens the set, grows it and writes
both back, so each carries the keys a later build wrote into it (`Extra`, as
every section of the run's record a later build may add to does); the next
job's attach still replaces the part whole.

**In `Values` itself**, which an older build's scrub decodes alone, so its
captures take the renewed values too. When each lapses rides beside them in
`Expires`, which only this build reads, and a value is dropped an hour past its
expiry: a lapsed token opens nothing, and a day of an AWS session's renewals
would otherwise outgrow the set's bound. A value is taken out whole and, where
it is a JSON document, by every string it holds, the launch's own values among
them: an AWS session reaches its box as the document its `credential_process`
prints, whose token a transcript prints alone. Every string, a JSON-valued
secret's settings among them — a Google Cloud service-account key's project ID
and client email, which a kept transcript then loses wherever it names them —
since the engine tells a document's credential from its settings by nothing
but its members' names. But not a format's own published constants, which
cannot be secrets: of a Google service-account key (a document whose `type` is
exactly `service_account`), the values of `type`, `auth_uri`, `token_uri`,
`auth_provider_x509_cert_url` and `universe_domain` are fixed by Google's
format, the same in every key, and are kept, so a seat handed one keeps every
googleapis.com URL its tools print. One closed list, for one format the engine
has a first-class integration for, decided on the key's declared `type`; every
other document gives up every string, and every other member of the key goes.

**A set the renewals take past `MaxHandedBytes`** once its lapsed values are
dropped, or one the node cannot open or seal again, is MARKED: an empty seal,
which an older build reads as a job that keeps none, beside a reason this build
states in its refusal. Every later capture of the job is refused, as one of a
launch past the bound is, and the token is written all the same, since the run
needs a good token more than a kept session. The job's own write of its set,
landing late, never replaces the mark, and a job with no set at all is marked
when its first renewal finds none.

**The alternatives.** Reading the issued tokens again at the capture would issue
new ones, never the ones the box holds. A part of its own for the renewals would
be read by no older build's scrub, which scrubs by `Values` alone. Splitting a
document by the strings under a credential's key alone would keep its settings,
and keep too, printed bare, a secret under a name the rule does not read.
Refusing to
write a token the set cannot hold would leave a working run with an expiring
credential halfway through its work, for the sake of a session it can live
without.

## Amendment: an end says its turn went on past the session its row names, and the row keeps it

A turn's row could name a session that was not the turn's last word and say so
nowhere. An end took its own session off the row for any unkept attempt newer
than the row's place, so whether a later attempt had been made, not what the
session held, decided it: a turn whose last launch's final capture was refused
after an earlier capture of it landed lost the session that held what it did up
to then, and the conversation's next turn continued the previous head, or
nothing. And a turn that ended on such a capture with no newer attempt — a stop
or a loss whose box was already gone takes nothing — kept it on its row, and
the next turn was told the session ran "through" a turn that went on long after
it. Each is now decided as follows (internal/sandbox's `session.go`,
internal/executordomain's `apply_session.go` and `read.go`,
internal/agent/prompts' `continued.go`).

**The end says whether the turn went on past the session its row names**
(`executorkeep.Ending.WentOn`, written only where true): wherever the turn took
a capture and the run's record does not hold its last word — the final capture
of its last launch that ran a job — kept. What it kept was taken while its job
still worked, or is an earlier launch's, or a newer capture beside it was not
kept, or it kept none. Only the run's record knows it, and that record is
deleted with the turn, so the end carries it, as it carries the unkept capture.
A launch whose job never started worked no session past the one before it, so
it is not counted: a relaunch whose harness would not start leaves the session
it followed the turn's last word.

**Decided on what the record did not keep, not only on what it kept.** A
capture the record holds refused can be on the log all the same: a record its
taker had handed on before a wait's settle read the turn's row and found it not
there, which the log takes after (the amendment naming a capture's taker lease:
"what the fence cannot reach"). So a turn whose record kept nothing may have
its row naming a session older than the newest capture its end names unkept,
which the older rule would take off beside an end saying nothing of going on.
Every unkept capture an end names therefore rides an end that says the turn
went on: an end this build writes without `went_on` names no unkept capture at
all, and the older rule releases nothing of this build's.

**And the end names the turn's last word, which a reader holds the row's place
to** (`executorkeep.Ending.LastWord`). Decided off the run's record, `went_on`
alone claims too much where the capture the log took late is the turn's last
word itself: the row names it and holds all the turn did, yet the end says the
turn went on past it, and the conversation's next turn would be told that what
the turn did after it is not above. So beside `went_on`, wherever the turn's
newest capture is its last word — refused, never answered, or begun and never
finished on the record — the end names that capture's place (`launch_seq`,
`capture_seq`), and every reader decides on one rule
(`executordomain.Session.endWentPast`): the turn went on past the row's session
only where the end names no last word, or the row's place is older than the one
it names. A row at that place reads as its turn's last word, as one whose final
capture landed does: no note that the turn went on past it, no `went_on` shown,
and the next turn led through the turn. Held to the row's place at read time, it
is right whichever side of the end the late record landed on: one the log took
before the end, whether before the end was decided or after, is the row's place,
and one it took after is refused by the end's tombstone, which leaves the row
older than the last word. An end that read the turn's row before it decided
would only narrow the race, since the late record can land between that read
and the end, and an apply that decided it would need the last word all the
same. The field rides only beside `went_on`, so it stamps no end at a version
`went_on` had not; it is a row of record version 4 (`End.Ending.LastWord`) all
the same, since a field's version is its own.

**An end that says so keeps the row's session.** It never takes it off for an
unkept attempt, and it absorbs and dedupes as any turn whose row keeps a
session does. This record's reason for the release, that the log "never names
an older session as the turn's last", is kept by saying so instead: a prefix of
what the turn did, told as one, holds more than the conversation's previous
head or a fresh session would, and is never told as the whole. A session that
ran out of context in the turn is retired all the same. And a session its end
kept is one only the seat's budget releases: a row released so says the budget
released it, beside the newer capture that was not kept (`Session.NotKept`),
never that the capture its end named took it.

**It is record version 4** (`End.Ending.WentOn`, and `End.Ending.LastWord`
beside it), **and the apply branches on the end's stamp** (`wentOnVersion`): an end stamped 4 or above keeps the row's
session whatever unkept attempt it names; one stamped below keeps the rule it
was written under, the session released and nothing absorbed. Without the
branch, an end a build at version 3 wrote during an upgrade would be applied by
one rule on that build's copies and by another on this build's, and the copies
would diverge. A record's meaning is fixed by its stamp: this reads nothing of
an older build's data, and keeps the estate a pure function of the log.

**What the row says, and to whom.** `executordomain.Session.WentOn` is true of
a row naming an object whose ending says its turn went on past that session
(`went_on`, held to its `last_word`), or whose turn was lost — which no frame of
it saw end, so its session may not hold the last of what it did — and
`Session.WentOnNote` is that in a reader's words, naming the newer capture not
kept, and why, redacted, where its end names one that stands, asked of the one
form every reader is shown a row in (`Session.Redacted`), which shows the
ending's `went_on` as it stands on the row, as it shows the unkept capture: the
sessions read and the seat's own `list_sessions` and `read_session`, which show
none of the row's place to hold the end's word to, send `went_on` only where
the turn went on past the row's session, and `last_word` as the end named it.
The conversation's next turn is told in words: the row's one view
(`Session.Kept`) carries the end's `went_on`, so held, and the lost outcome
apart (`TurnRow.WentOn`, `TurnRow.Lost`), with when the
session was kept (`TurnRow.CapturedAt`), frozen on the turn's base, and that
turn's run is told the session was kept while that turn still worked, at the
instant it was kept, that what the turn did after it is not above, and that how
the turn ended is told below — saying too where the turn was then lost; and, of
a lost turn whose end said nothing of going on, that the session runs through
it as far as the engine knows and may not hold the last of what it did
(`prompts.BuildNextTurn`). A cut-off session is said to have been saved while
its harness was still working, or as or after it was stopped
(`prompts.InterruptedSession`), which is true of both.

**What an upgrade costs.** A node at version 3 holds back an end that carries
`went_on`, and with it every later record whose scope meets it: every capture
is scoped to its whole seat, so every later record of that seat. Writes whose
scope it covers are answered `Unavailable` there and served by another data
node; its backup refuses while one is in its tail; and its collector's pass
stops judging, so objects no row names wait for the upgrade to finish. Such
ends are those of turns whose last word was not kept — a final capture refused,
whether or not an earlier one was kept; a stop or a loss whose box was already
gone after a capture taken while its job worked, or after an earlier launch's;
a relaunch that ran and kept nothing — which are rarer than losses, and the
stalls they cause last until every node runs this build, seat by seat. And an
end that a node of this build without `data` routes to a data node still at
version 3 loses `went_on` and `last_word` on the way: the estate router's server
decodes a write's arguments into its own build's types, which drop a key it does
not know, so that node writes the end stamped below 4, and every node applies it
by the older rule, as before this amendment: a session beside a newer capture
not kept is released, and one with none beside it is kept without a word that
the turn went on past it — for as long as such a data node serves the seat's
ends.

**The alternative**, releasing only where the turn's last launch ran a session
other than the one the row names and absorbing nothing after a release, keeps
the release for that case and needs the end to say which session the last
launch ran; what it would leave the next turn is the previous head or nothing,
where a prefix of the turn's own work, told as one, holds more.

## Amendment: a kept session is read as in progress, or as one its turn went past, on every surface

A live turn's row can name a session — a job's final capture as its collection
took it, while the turn goes on to its review or waits on a person's answer, or
one a loss left its box to take — and every reader read it as it reads an ended
turn's: nothing said the turn's run might have gone on past it, nor that its
last call might have no result yet. And the note a row its turn went on past is
read in (`Session.WentOnNote`, the amendment above) reached no surface. Each is
now said as follows (internal/executordomain's `read.go`, internal/executorread's
`read.go`, internal/api/queries' `executorsessions.go`, internal/agent/builtin's
`sessions.go`, and the dashboard's Sessions view).

**A row naming the session of a turn that has not ended is in progress**
(`executordomain.Session.InProgress`: not ended, and naming an object), read
off the row alone, since nothing on the log says what kind of capture landed a
row and no reader of it reads the turn's run record. It is read as the turn's
session and never as an ended turn's; whether a relaunch continues it is the
relaunch's own decision (`sandbox.ContinuityOf`, which a person's read of the
row asks as `continuable`), since a relaunch continues only the newest kept
capture of the job it follows and a live turn's row often names an earlier
job's. Its sentence says only what the row says (`Session.InProgressNote`):
"This turn has not ended: this is the newest session its runs have kept,
captured at `<captured_at>`; its run may have gone on past it, and its last
call may have no result yet." Never that the session is saved as its run
works, which would be false of a harness or a seat whose runs keep nothing
while they work.

**Two notes, each in its own field, on every surface.** What a reader is told
of the session a row keeps is the domain's one sentence of it, asked of the
shown form (`Session.Redacted`): the in-progress sentence of a live row, and
the went-on note of an ended one (`Session.WentOnNote`), whose unkept capture's
reason is redacted. A person's read surface (`in_progress`, `in_progress_note`
and `kept_note` on the row `executor_sessions` and `executor_session` send) and
the seat's own `list_sessions` and `read_session` (the same three) carry them,
and the dashboard draws each where it reads — the first beside "not ended",
the second under when the session was captured — and words neither itself. A
copy of either sentence in the dashboard was rejected: two copies of one
sentence are two surfaces that tell a person and a model different things about
one session. And so was one field for both, though they never meet: the
went-on note quotes what refused a capture, an error a box or a store wrote, so
its field is declared somebody's words and a seat reads it fenced naming
nobody — and the in-progress sentence, carried in that field, reached the model
fenced as words of unknown authorship, the one sentence meant to keep a seat
from taking its earlier launch's session for an ended turn's labelled as
nobody's. Each in its own field, the engine's sentence is never fenced and the
note that quotes a refusal always is.

**A later page of a session in progress is refused in its runs' words**
(internal/executorread's `recaptured`): its turn has not ended, and its runs
have kept its session again since the cursor was handed out — true whichever
capture moved the row, a later launch's or one taken as its run works — and of
an ended row, as before, that its session was captured again. The dashboard's
banners take the same words, spelled once (`executorread.LiveKept`, which the
dashboard's contract declares as `LIVE_KEPT` and
`internal/api/queries.TestTheSessionsScreenReadsWhatTheAnswersSend` holds to
it), so a rewording reaches both or fails there; and of a transcript that read
no capture, the screen says its runs have kept its session since, never
"again".

## Amendment: a resume cuts every torn line of a session

A resume cut back a transcript whose last line its harness was killed while
writing, and nothing else of the session. A subagent's thread is torn the same
way — by a harness killed while the subagent appended, or by a pack made beside
a harness still working, which reads the thread as it grows — and Claude Code
2.1.310's resume, measured against a scripted model, writes the next record to
a torn thread onto its torn line, losing that record too, while it continues a
thread cut back to its last whole line through its last whole call. So every
resume of a Claude Code session cuts back the transcript and every `.jsonl`
under its sidecar directory, at any depth (internal/sandbox/executor's
`claudeSessionGate`); a file with no whole line is left as the transcript
always was. Nothing on the log, the row or the archive changes: an archive
holds what it held, and the cut is the box's, made before its harness starts.
