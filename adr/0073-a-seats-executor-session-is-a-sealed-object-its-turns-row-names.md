# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:**
  - `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`
  - `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`
  - `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`
  - `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`
  - `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`
  - `internal/executordomain.TestALateCaptureNeverRollsASessionBack`
  - `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`
  - `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`
  - `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`
  - `internal/executordomain.TestARemovedSeatStaysRemoved`
  - `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`
  - `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`
  - `internal/engine.TestTheExecutorLogFitsBesideTheOtherLogsAtTheOperatorsSmallestVolumes`
  - `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`
  - `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`
  - `internal/estate.TestEveryExecutorWriteIsTheEngines`
  - `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`
  - `internal/sandbox.TestEachLaunchOfATurnIsNumberedPastTheOneBefore`
  - `internal/sandbox.TestACapturesPlaceOnlyGrowsOnTheRow`
  - `internal/sandbox.TestACollectionTakesTheSessionBeforeThePauseAndKeepsItOffTheRead`
  - `internal/sandbox.TestACollectionsKeepStartsOnceItsTakeIsSpooled`
  - `internal/sandbox.TestACollectionsPauseRecordIsHeldToItsRoom`
  - `internal/sandbox.TestEveryCollectionTakesWhatItsJobLeft`
  - `internal/sandbox.TestAJobTakenOnceItHadEndedIsNotTakenAgain`
  - `internal/sandbox.TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd`
  - `internal/sandbox.TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd`
  - `internal/sandbox.TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim`
  - `internal/sandbox.TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt`
  - `internal/sandbox.TestALostRunsBoxOnAnotherHostIsAskedOfThatHost`
  - `internal/sandbox.TestAnEndWaitsForItsTurnsCaptureToLand`
  - `internal/sandbox.TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded`
  - `internal/sandbox.TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused`
  - `internal/sandbox.TestTheScrubReadsTheLoginAgainWhereItIsRefreshed`
  - `internal/sandbox.TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse`
  - `internal/sandbox.TestWhatABoxWasHandedIsKeptBesideItsRowForItsJobAlone`
  - `internal/executordomain.TestAnEndWhoseNewestCaptureWasNotKeptReleasesTheOlderOne`
  - `internal/executordomain.TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted`
  - `internal/sandbox.TestTheSwitchOffKeepsNothingAndStillEndsTheTurn`
  - `internal/sandbox/executor.TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox`
  - `internal/sandbox/executor.TestAPackOnDemandKeepsItsDigestInTheBox`
  - `internal/sandbox/executor.TestATranscriptLineStaysJSONAndLosesEverySecret`
  - `internal/sandbox/executor.TestTheDigestIsPOSIXCksums`
  - `internal/sandbox/executor.TestTheScrubReadsNothingPastItsBoundsAndRefusesNothingForItsLength`
  - `internal/sandbox/executor.TestATarLeavesTheBoxWithOnlyItsSessionsFiles`
  - `internal/sandbox/executor.TestAnExportIsOneDocumentKeptScrubbed`
  - `internal/sandbox/executor.TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed`
  - `internal/sandbox/executor.TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession`
  - `internal/sandbox/executor.TestATakeIsBoundedByTheArchiveItKeeps`
  - `internal/engine.TestAResumedTurnsEndIsRecordedWithItsOwnDecision`
  - `internal/engine.TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog`
  - `internal/engine.TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`
  - `internal/engine.TestALostTurnEndsAsLostOnTheExecutorLog`
  - `internal/engine.TestTheEndCarriesTheTurnsOwnDecisionAndBreach`
  - `internal/engine.TestTheEndSaysWhatTheEngineAnsweredAColleague`
  - `internal/engine.TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid`
  - `internal/engine.TestAStopsAccountReadsEveryRoundsCalls`
  - `internal/engine.TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds`
  - `internal/engine.TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped`
  - `internal/engine.TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`
  - `internal/engine.TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn`
  - `internal/engine.TestAResumedTurnThatPanicsOnceBegunEndsFailed`
  - `internal/engine.TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose`
  - `internal/engine.TestAnMCPServersURLSecretIsHandedToTheScrub`
  - `internal/engine.TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives`
  - `internal/sandbox.TestARetiredSeatsRunIsFinishedWithItsTurnsEnd`
  - `internal/sandbox.TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded`
  - `internal/sandbox.TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt`
  - `internal/sandbox.TestTheSwitchOffAsksNoHostForALostRunsSession`
  - `internal/sandbox.TestASpoolDirectoryIsSweptOnlyOnceItsProcessHasLetGo`
  - `internal/sandbox.TestASaveLeavesRoomForOneFinalSessionAtTheBound`
  - `internal/sandbox.TestEverySpoolGivesItsRoomBackWithItsFile`
  - `internal/sandbox.TestAKeptSessionReadAgainOtherwiseFailsTheLaunch`
  - `internal/sandbox.TestASpoolNeverOutlivesItsCapture`
  - `internal/sandbox.TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded`
  - `internal/sandbox.TestTwoAttemptsAtOneEndingOnANodeFinishItOnce`
  - `internal/sandbox.TestALoginTheBoxGrewPastItsBoundRefusesTheCapture`
  - `internal/sandbox.TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt`
  - `internal/sandbox.TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt`
  - `internal/sandbox.TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail`
  - `internal/sandbox.TestAnEndingDecidedBeforeTheNextHoldersFenceIsFinishedByIt`
  - `internal/sandbox.TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer`
  - `internal/sandbox.TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled`
  - `internal/sandbox/executor.TestAHandedKeysArmourStaysForTheShapePass`
  - `internal/sandbox/executor.TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack`
  - `internal/redact.TestALineIsCutAtTheArmourAKeyIsReadBy`
  - `internal/procgroup.TestASignalReachesOnlyALivingMemberOfTheGroup`
  - `internal/sandbox.TestADirectBoxEndsOnlyAProcessOfItsJob`
  - `internal/sandbox.TestADirectBoxKillsEveryProcessOfItsJobAndKeepsTheBox`
  - `internal/sandbox.TestAContainerBoxEndsAndKillsItsJobInsideTheContainer`
  - `internal/sandbox.TestAContainerJobThatRecordedNoGroupIsKilledByTheGroupItsExecLeads`
  - `internal/sandbox.TestAContainerJobWhoseGroupNoExecLeadsIsSignalledNothing`
  - `internal/sandbox.TestAContainerJobWithNoGroupRecordedIsReadOverOnlyOnceItIs`
  - `internal/sandbox/executor.TestAnEndedClaudeCodeRunPacksAsItsOwnEndDoes`
  - `internal/sandbox/executor.TestAnEndedOpenCodeRunExportsItsSessionBeforeItsServerStops`
  - `internal/sandbox/executor.TestAnEndIsAskedOfThePIDTheScriptRecorded`
  - `internal/sandbox/executor.TestACollectionSaysItsJobWasEnded`
  - `internal/sandbox.TestAStopEndsItsJobBeforeItTakesItsSession`
  - `internal/sandbox.TestALossEndsItsJobBeforeItTakesItsSession`
  - `internal/sandbox.TestAJobThatOutlivesItsEndIsKilledBeforeItsTake`
  - `internal/sandbox.TestAJobAskedNothingIsKilledNotWaitedFor`
  - `internal/sandbox.TestAStopOfABoxOnAnotherHostEndsItsJobThere`
  - `internal/sandbox.TestAStopWithTheSwitchOffEndsItsJobOffTheFrame`
  - `internal/sandbox.TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake`
  - `internal/engine.TestABoxReadWaitsAsLongAsItsKindTakes`
  - `internal/sandbox.TestACappedJobIsEndedBeforeItsSessionIsTaken`
  - `internal/sandbox.TestACappedRunStillSaysItWasCutOffAtItsCap`
  - `internal/sandbox/executor.TestAnEndedOpenCodeJobsSessionIsTakenAsItsScriptExportedIt`
  - `internal/sandbox.TestACapsVerdictOutlivesAReadCutShortAsItEndsTheJob`
  - `internal/executordomain.TestABeginHoldsTheBaseItsReadNamed`
  - `internal/executordomain.TestABeginAndAnEndOfItsBaseAreOrderedByTheLog`
  - `internal/executordomain.TestNoReleaseTakesAHeldBase`
  - `internal/executordomain.TestTheHoldLiftsAtTheHoldersFirstCapture`
  - `internal/executordomain.TestTheHoldLiftsWhenTheHolderEnds`
  - `internal/executordomain.TestATurnThatNeverCapturedAbsorbsNothing`
  - `internal/executordomain.TestTheRetentionListsNoHeldRow`
  - `internal/executordomain.TestAPageNamesEachConversationsHead`
  - `internal/executordomain.TestABeginTheRowsCannotTakeIsRefusedAtTheDecide`
  - `internal/executordomain.TestABeginSaysWhetherTheLogHoldsItsBase`
  - `internal/executordomain.TestACapturesContextAndCutOffReachTheRow`
  - `internal/executordomain.TestTheExecutorStatementsArePlanned`
  - `internal/sandbox.TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff`
  - `internal/executordomain.TestABeginHoldsOnlyTheSessionItsReadSaw`
  - `internal/executordomain.TestALateBeginRecordsTheReadAndHoldsNothing`
  - `internal/executordomain.TestABeginOfATurnThatEndedWritesNothing`
  - `internal/executordomain.TestABeginNoReadHandsOutIsRefusedByItsWriter`
  - `internal/executordomain.TestABeginThatHoldsNothingSaysWhy`
  - `internal/executordomain.TestAHolderWhoseEndWasLostIsEndedLostAndItsHoldLifts`
  - `internal/sandbox.TestATurnsFirstLaunchContinuesItsConversationsHead`
  - `internal/sandbox.TestTheFreezeIsWrittenAtTheFirstLaunchOnly`
  - `internal/sandbox.TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase`
  - `internal/sandbox.TestWhetherABaseIsContinuedIsOneDecision`
  - `internal/sandbox.TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone`
  - `internal/sandbox.TestABaseIsContinuedOnlyInABoxOfItsOwn`
  - `internal/engine.TestAFrozenReadFitsItsBound`
  - `internal/engine.TestATurnReadsItsConversationOnce`
  - `internal/engine.TestTheFrozenBaseOutlivesASiblingsEnd`
  - `internal/engine.TestAnEndingLeftOutOfTheBriefIsNotAbsorbed`
  - `internal/engine.TestATurnThatNeverLaunchedTellsTheNextTurnHowItEnded`
  - `internal/engine.TestAnAbandonedFirstLaunchEndsItsTurnOnTheLog`
  - `internal/engine.TestTheSwitchOffCarriesNothingBetweenTurns`
  - `internal/agent/prompts.TestTheEndingsKeepTheNewestWholeAndCountTheRest`
  - `internal/sandbox.TestWhatAReaderIsToldOfAnEndedTurnIsTheNextTurnsOwnDecision`
  - `internal/sandbox.TestARelaunchOfATurnThatKeptNothingIsSaidToContinueItsBase`
  - `internal/api/queries.TestARowSaysWhetherTheNextTurnContinuesIt`
  - `internal/api/queries.TestARowReadAgainIsDecidedOnTheHeadItsOwnReadNamed`
  - `internal/engine.TestAnUnreadableConversationIsLessContextNotAFailedTurn`
  - `internal/schedule.TestEachFireOfAScheduleIsOneConversationOfItsRunner`
  - `internal/schedule.TestTheLongestScheduleNameFitsTheLogsKey`
  - `internal/org.TestAScheduleNameIsBoundedAsItsKeySpellsIt`
  - `internal/notify.TestAScheduleConversationIsKnownByItsPrefixAlone`
  - `internal/engine.TestAScheduledFireContinuesThePreviousFiresSession`
  - `internal/engine.TestFiresOfOneScheduleThatWaitedTogetherRunOnce`
  - `internal/engine.TestAFireIsNeverTakenAsTheAnswerToTheLastFire`
  - `internal/api/queries.TestARunNoChatCanAnswerSaysSo`
  - `internal/sandbox.TestAScheduleRowIsToldItsNextFire`
  - `internal/api/queries.TestAScheduleRowSaysWhetherItsNextFireContinuesIt`
  - `internal/executordomain.TestAConversationsHeadIsTheTurnThatReadItLast`
  - `internal/executordomain.TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued`
  - `internal/executordomain.TestAHeadReleasedBeforeAStaleSiblingHandsTheNextTurnTheSibling`
  - `internal/engine.TestTheNextFireContinuesTheSchedulesLineNotTheFireThatEndedLast`
  - `internal/executordomain.TestAnEndThatRanOutOfContextLeavesTheConversationNoHead`
  - `internal/executordomain.TestARetiredSessionGoesFromTheBaseWhereNoCaptureLanded`
  - `internal/executordomain.TestARetiredSessionGoesFromARowALiveTurnHolds`
  - `internal/executordomain.TestASessionStartedAfterRunningOutIsTheNextHead`
  - `internal/executordomain.TestAnEndThatFailedAnyOtherWayKeepsItsSession`
  - `internal/executordomain.TestARetirementReachesNoOtherSessionOrConversationOrSeat`
  - `internal/executorkeep.TestAnEndingsSpentSessionsAreBounded`
  - `internal/sandbox.TestAJobThatRanOutOfContextIsRecordedAndKeepsNoCapture`
  - `internal/sandbox.TestACappedRunThatAskedAndRanOutOfContextKeepsNoCapture`
  - `internal/sandbox.TestARelaunchContinuesNoKeptCaptureOfASessionThatRanOut`
  - `internal/sandbox.TestARelaunchContinuesNoBaseThatRanOut`
  - `internal/sandbox.TestARelaunchContinuesNoCopyInItsBoxOfASessionThatRanOut`
  - `internal/sandbox.TestAFreshSessionAfterRunningOutIsContinued`
  - `internal/sandbox.TestAReaderIsToldASessionThatRanOutIsNotContinued`
  - `internal/sandbox.TestATurnsEndNamesTheSessionsItSpentAndHow`
  - `internal/engine.TestATurnAfterASessionWasSpentStartsItsOwnSayingHow`
  - `internal/executordomain.TestAnEndWhoseCaptureGoesWithItAbsorbsNothing`
  - `internal/executordomain.TestARetiredRowSaysHowItsSessionWasSpent`
  - `internal/executordomain.TestASeatsSearchFindsEverySessionHoldingEveryWord`
  - `internal/executordomain.TestASearchMatchesWhatAReadWouldShow`
  - `internal/executordomain.TestASearchThatStopsAtItsBoundSaysWhereAndHowFar`
  - `internal/estate.TestEveryExecutorOperationCrossesWhole`
  - `internal/estate.TestTheExecutorFacadesAskExactlyTheExecutorOperations`
  - `internal/executorread.TestASessionReleasedWhileItIsReadIsSaidAsReleased`
  - `internal/executordomain.TestASeatReadsNoOtherSeatsSession`
  - `internal/sandbox/executor.TestAMatchIsDecidedOnWhatItsReaderIsShown`
  - `internal/sandbox/executor.TestTwoRecordsThatRedactAlikeAreReadAlike`
  - `internal/sandbox/executor.TestAFilterHandsOutEveryPartItCouldNotRead`
  - `internal/agent/builtin.TestASeatReadsOnlyItsOwnSessions`
  - `internal/agent/builtin.TestTheSwitchStopsEveryModelsReadsAtItsNextCall`
  - `internal/agent/builtin.TestAPersonIsNeverOfferedASeatsSessions`
  - `internal/agent/builtin.TestReadingASessionWritesNothing`
  - `internal/agent/builtin.TestASessionListedThenReleasedIsReadAsReleased`
  - `internal/agent/runner.TestTheReviewerIsOfferedNoSessionTool`
  - `internal/engine.TestTheSessionToolsAreOmittedWhereNoSessionIsKept`
  - `internal/engine.TestEveryBarrierDeclaresItsReadModel`
  - `internal/e2e.TestAnExecutorReadsItsSeatsSessionOverTheBridge`
  - `internal/sandbox.TestARelaunchIsToldOfABaseItRanOutInWhileItsRowHoldsIt`
  - `internal/sandbox.TestACaptureWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow`
  - `internal/sandbox.TestAnEndDoesNotWaitOutACaptureWhoseTakersLeaseLapsed`
  - `internal/sandbox.TestADrainingTakersCaptureIsNotSettledUnderIt`
  - `internal/sandbox.TestATakerLeaseHeldAtAnotherEpochIsGone`
  - `internal/sandbox.TestAStoreThatCannotAnswerNeverReadsATakerAsGone`
  - `internal/sandbox.TestAGoneTakersCaptureIsSettledOnlyByAnAnswerFromTheLog`
  - `internal/sandbox.TestARelaunchSettlesACaptureWhoseTakerIsGoneBeforeItDecides`
  - `internal/sandbox.TestAnUnresolvedCaptureIsAskedAgainBeforeARelaunchDecides`
  - `internal/sandbox.TestACaptureARelaunchsAskLandsIsTheTurnsOwnToRead`
  - `internal/sandbox.TestARelaunchsAskOfAnUnansweredRecordEndsWithinItsWait`
  - `internal/sandbox.TestAFencedKeepUploadsAndRecordsNothing`
  - `internal/sandbox.TestATakerLeaseOutlastsTheDrainAndIsReleasedAfterTheLastKeep`
  - `internal/sandbox.TestATakeWithNoTakerLeaseBeginsNoCapture`
  - `internal/sandbox.TestAStampWithNoTakerIsSettledByAge`
  - `internal/sandbox.TestAKeepIsFencedOnceItsLastRenewalNoLongerCoversIt`
  - `internal/sandbox.TestATakeStampsTheTenureItReservesItsPlaceUnder`
  - `internal/sandbox.TestAnAttemptAtAKeepsRecordEndsWithTheWindowItWasSentIn`
  - `internal/sandbox.TestASettleLeavesWhatItsTakerStampedSince`
  - `internal/sandbox.TestATakerLeaseIsNeverAskedForLongerThanTheStoreKeepsOne`
  - `internal/sandbox.TestAStopUnderARenewalSaysNothingOfTheStore`
  - `internal/sandbox.TestACoordinatorNeedsItsCollaborators`
  - `internal/sandbox.TestACaptureRecordedAfterItsSettleIsKeptThroughTheTurnsEnd`
  - `internal/sandbox.TestARelaunchStartsWithoutACaptureItsRecordSettledRefused`
  - `internal/sandbox.TestARelaunchOfATurnThatFrozeNoBaseReadsItsRowForACaptureItsRecordRefused`
  - `internal/sandbox.TestARelaunchThatWillNotWaitOutAKeepContinuesTheBaseTheTurnStillHolds`
  - `internal/executordomain.TestAnEndsUnkeptCaptureStandsOnlyWhereItIsNewerThanTheRowsPlace`
  - `internal/api/queries.TestAnEndingSendsAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`
  - `internal/engine.TestAnEndingTellsTheNextTurnAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`
  - `internal/executordomain.TestARowIsShownWithNoUnkeptCaptureItKeeps`
  - `internal/coord/kv.TestALapsedTakerLeasesKeyIsRemoved`
  - `internal/coord/coordtest`
  - `every_taker_lease_mints_from_its_class_counter`
  - `internal/engine.TestTheEnginesCoordinatorHoldsThisProcesssTakerLease`
  - `internal/engine.TestTheSandboxIsStoppedOnlyByTheTeardownAfterTheDrain`
  - `internal/engine.TestASandboxsTakerLeaseIsGivenBackOnTheStopsAllowance`
  - `internal/engine.TestATokenThePollRenewsIsRecordedBeforeItIsWritten`
  - `internal/engine.TestARenewalTheStoreDidNotTakeIsNotWritten`
  - `internal/sandbox.TestACaptureTakenAfterARenewalHoldsNoRenewedValue`
  - `internal/sandbox.TestATokenIsRecordedOnlyInTheSetOfTheJobItsRowHoldsRunning`
  - `internal/sandbox.TestATokenTheLaunchWritesInAKeptBoxIsSealedWithItsJob`
  - `internal/sandbox.TestAnIssuedDocumentIsScrubbedByEveryStringItHolds`
  - `internal/sandbox.TestAServiceAccountKeysPublishedConstantsStayInItsSessions`
  - `internal/sandbox.TestARenewedValueIsDroppedAnHourPastItsExpiry`
  - `internal/sandbox.TestAnOlderBuildsScrubTakesTheRenewedValues`
  - `internal/sandbox.TestARenewalPastTheSetsBoundIsWrittenAndRefusesEveryLaterCapture`
  - `internal/sandbox.TestARenewalTheStoreDidNotAnswerIsKeptOutOfTheBox`
  - `internal/sandbox.TestALaterBuildsKeysInTheHandedSetSurviveARenewal`
  - `internal/sandbox.TestAWaiterThatRefreshesIsRefusedWithoutARecorder`
  - `internal/engine.TestTheWaiterKeepsARunsTokenFileCurrent`
  - `internal/executordomain.TestAnEndWhoseTurnWentOnPastItsSessionKeepsIt`
  - `internal/executordomain.TestAnEndStampedBelowFourIsAppliedByTheRuleItWasWrittenUnder`
  - `internal/executordomain.TestARowKeptUpToASessionItsTurnWentPastSaysSo`
  - `internal/sandbox.TestAnEndOfATurnThatEndedOnASaveSaysItWentOn`
  - `internal/sandbox.TestAnEndWithoutWentOnNeverCarriesAnUnkeptCaptureNewerThanItsRow`
  - `internal/engine.TestAHeadItsTurnWentOnPastIsHandedTheNextTurnAsOne`
  - `internal/agent/runner.TestARunContinuingASessionItsTurnWentPastIsLedSo`
  - `internal/agent/prompts.TestANextTurnOnASessionItsTurnWentPastIsToldSo`
  - `internal/agent/prompts.TestACutOffSessionIsSaidTrueOfASaveAndOfAStop`
  - `internal/sandbox.TestAnEndOfATurnWhoseLaterLaunchKeptNothingSaysItWentOn`
  - `internal/sandbox.TestACaptureTheLogTookAfterARelaunchSettledItStaysTheTurnsSession`
  - `internal/executordomain.TestARowThatNamesNoSessionSaysWhichOfThreeReasons`
  - `internal/sandbox.TestALastWordTheLogTookAfterItsSettleIsReadAsTheTurnsLastWord`
  - `internal/executorkeep.TestAnEndingsLastWordIsPlacedFromTheFirstCapture`
  - `internal/executordomain.TestALiveRowNamingASessionReadsAsInProgress`
  - `internal/api/queries.TestARowKeptUpToASessionItsTurnWentPastCarriesItsNote`
  - `internal/api/queries.TestTheSessionsScreenReadsWhatTheAnswersSend`
  - `internal/agent/builtin.TestASeatIsToldOfASessionInProgressOrOneItsTurnWentPastAsAPersonIs`
  - `internal/executorread.TestALiveRowWithNoSessionYetIsSaidAsSuch`
  - `internal/executorread.TestASessionKeptAgainSinceItsCursorIsReadAgainInItsRowsWords`
  - `internal/sandbox/executor.TestARestoreCutsATornLastLineOfEverySubagentThread`
  - `internal/sandbox/executor.TestABoxWithoutFindResumesItsSessionAndSaysItCutNoThread`
  - `internal/sandbox.TestACapturePointRisesOnlyWhereItsTakeNamesTheHeldCapturesNewestCompaction`
  - `internal/sandbox.TestACaptureIsPlacedByItsLogReadOnlyWhereItsHarnessEndedItsOwnRun`
  - `internal/sandbox.TestOnlyACaptureItsLogReadPlacesReadsTheLog`
  - `internal/agent/prompts.TestACutOffSessionIsToldItsCallsAsOnesItMayAlreadyShow`
  - `internal/agent/runner.TestACarriedOnSessionIsToldWhatCameAfterItWasSaved`
  - `internal/sandbox/executor.TestASaveIsPackedToItsTakersOwnPathAndNeverTakenAsTheJobsFinal`
  - `internal/sandbox/executor.TestASaveOfAMemberRewrittenWhileItWasPackedFails`
  - `internal/sandbox/executor.TestASaveOfASessionThatOnlyGrewAsItWasPackedHolds`
  - `internal/sandbox/executor.TestAPackWhoseBoxClockNeverMovesIsRefused`
  - `internal/sandbox/executor.TestAFailedSaveLeavesTheLastOneStanding`
  - `internal/sandbox/executor.TestASaveIsNotPackedPastWhatTheScrubWouldRead`
  - `internal/sandbox/executor.TestTheClearBeforeEveryRunRemovesTheSavesFiles`
  - `internal/sandbox/executor.TestAPackMadeOnceItsHarnessIsGoneKeepsTheEndsRule`
  - `internal/sandbox/executor.TestAPackOnABoxWhoseSleepTakesNoTenthIsRefused`
  - `internal/sandbox/executor.TestAPackBesideAWorkingHarnessNamesAToolItsBoxLacks`
  - `internal/sandbox/executor.TestAPackProvesTheSessionsOwnTranscriptWhateverItsFindReads`
  - `internal/sandbox/executor.TestAPackSetsEveryFileOfItsSessionFromItsBoxsFutureToItsStartAndHolds`
  - `internal/sandbox/executor.TestAFileFromItsBoxsFutureRewrittenAsItIsPackedIsStillRefused`
  - `internal/sandbox/executor.TestTwoPacksBesideOneWorkingHarnessHideNoWriteFromEachOther`
  - `internal/sandbox/executor.TestAHeldPackWhoseChecksCannotBeMadeIsRefused`
  - `internal/sandbox/executor.TestTwoPacksOnDemandOfOneBoxAtOnceBothHold`
  - `internal/sandbox/executor.TestTwoSavesOfOneTakerAtOnceBothHold`
  - `internal/sandbox/executor.TestATakeOfAJobStillWorkingNeverMovesItsArchiveOverTheJobsCapture`
  - `internal/sandbox.TestEveryCaptureNoReadPlacesIsReservedAtZero`
  - `internal/sandbox.TestACollectionRecordsHowItsJobsHarnessEnded`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesAJobsHarnessEnd`
  - `internal/sandbox.TestPendingStoreContract`
  - `internal/engine.TestAResumeReadsHowTheJobsHarnessEnded`
  - `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldTheCallsItsCopyMayLack`
  - `internal/agent/runner.TestARunCarriedOnInItsBoxArmsItsJobsLoadsOnlyWhereItsHarnessEndedItsOwnRun`
  - `internal/agent/runner.TestARunCarriedOnInItsBoxLaunchesNothingWhenTheCallsItsCopyMayLackFillTheWindow`
  - `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldAgainEveryNoteTheTurnRead`
  - `internal/agent/prompts.TestACopyCutOffInItsBoxIsToldSoWithoutASave`
  - `internal/agent/prompts.TestNotesToldAgainAreOnesTheSessionMayAlreadyShow`
  - `internal/agent/runner.TestAnApprovedCallCarriedOnInItsBoxIsToldFirstThenWhatItsCopyMayLack`
  - `internal/sandbox.TestAReadIsNeverRecordedOverAWordThisBuildDoesNotKnow`
  - `internal/sandbox.TestAJobsHarnessEndIsRecordedAfterItsTakeWithinTheReadsRoom`
  - `internal/sandbox.TestEachOfATakesRecordsIsGivenItsOwnGrace`
  - `internal/sandbox.TestACapturesBudgetsCountEveryStepAtTheSlowestCrossing`
  - `internal/sandbox.TestACapturesAgeIsReadAsAnyTwoNodesClocksAllow`
  - `internal/sandbox.TestAKeepsReadsLeaveItsUploadItsWholePace`
  - `internal/sandbox.TestEveryCapturesOutcomeLandsWithinItsLife`
  - `internal/sandbox.TestAnEndWaitsForEachCaptureItSeesItsWholeLifeFromFirstSight`
  - `internal/sandbox.TestAReadStandingByTakesTheNextPlaceOnceItsOwnWaitOutlastsTheCapture`
  - `internal/sandbox.TestAnUnansweredRecordIsAskedWhateverItsKeysAgeReadsHere`
  - `internal/sandbox.TestAWaitsSettleIsWrittenInItsOwnRoomPastTheWait`
  - `internal/sandbox.TestAnAskThatLeavesARecordUnansweredWritesNothing`
  - `internal/agent/runner.TestALaunchRecordsTheSkillsItArmedNeverALoadItsRunMadeAsItStarted`
  - `internal/sandbox.TestACaptureStatesTheNotesItHoldsOnlyWhereItsHarnessEndedItsOwnRun`
  - `internal/agent/runner.TestAKeptSessionCutOffIsToldAgainEveryNoteTheTurnRead`
  - `internal/sandbox.TestEveryStampOfACaptureStatesTheNotesItsSessionHolds`
  - `internal/sandbox/executor.TestTwoReadsOfAJobThatIsOverEachStreamTheirOwnPacksArchive`
  - `internal/sandbox/executor.TestTheJobsOwnEndPackLandingAsAReadStreamsChangesNothingItReads`
  - `internal/sandbox.TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceFails`
  - `internal/sandbox.TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceStopped`
  - `internal/sandbox.TestAReadWhoseReservationAnotherReadBeatStandsByForItsCapture`
  - `internal/sandbox.TestAReadStandingByKeepsNothingOnceTheCaptureItStoodByForLands`
  - `internal/sandbox.TestAReadStandingByThatCannotKeepWhatItTookIsWithdrawn`
  - `internal/sandbox.TestAReadStandingByWhoseTakerIsGoneIsWithdrawnNotWaitedFor`
  - `internal/sandbox/executor.TestAPackReportsItsArchiveMadeOnlyOnceItsDigestIsRecorded`
  - `internal/sandbox/executor.TestARestoreIsContinuedOnlyOnceItsDigestIsRecorded`
  - `internal/sandbox/executor.TestATakeOfASaveStreamsItsOwnWhateverAnotherSaveOfItsTakerLands`
  - `internal/sandbox/executor.TestAPacksReleaseIsHeldToItsTakesOwnDeadline`
  - `internal/sandbox.TestATurnsEndWaitsForAStandbysTakeoverAndKeep`
  - `internal/sandbox.TestALossReadStandsByForACaptureOfTheJobStillBeingKept`
  - `internal/sandbox.TestAReadWhoseReservationWentUnansweredBesideAnotherStandsByForIt`
  - `internal/sandbox.TestAFinalReadBesideACaptureTakenAsTheJobStoodTakesTheNextPlace`
  - `internal/sandbox.TestAReadStandingByTakesOverAnUnansweredCaptureOnlyOnceTheLogRefusesIt`
  - `internal/sandbox.TestAStandbyTheStoreNeverAnsweredKeepsTheSessionWhereItStands`
  - `internal/sandbox.TestATakeoverTheStoreNeverAnsweredIsKeptUnderWhereItWasMade`
  - `internal/sandbox.TestATakeoverLeftUnansweredReadsItsRowWithinARead`
  - `internal/sandbox.TestAReadStandingByThatTookNothingLeavesTheCaptureItStoodByFor`
  - `internal/sandbox.TestAWaitLeavesAReadStandingByWhoseTakerHolds`
  - `internal/sandbox.TestAReadStandingByTakesOverACaptureBegunPastItsBudget`
  - `internal/agent/runner.TestEveryContinuationOfTheRoundIsToldALateCall`
  - `internal/agent/runner.TestNoGuardCountsALoadTheLogTookLate`
  - `internal/agent/runner.TestALateSubmissionIsNeverTheRounds`
  - `internal/sandbox.TestALaunchNamesItsJobsRound`
  - `internal/sandbox.TestALongRunsLogKeepsALateCallWhileAnOrdinaryOneIsLeft`
  - `internal/sandbox.TestALateCallsMarkSurvivesABuildThatDoesNotKnowIt`
  - `internal/engine.TestALateCallReachesItsRoundsLogThroughItsSessionsLedger`
  - `internal/agent/runner.TestALateCallIsToldApartUnderWordsOfItsOwn`
  - `internal/agent/runner.TestALateSubmissionIsToldAsNoneOfTheRounds`
  - `internal/agent/prompts.TestALateCallIsToldUnderWordsOfItsOwnWhateverTheSessionsEnd`
  - `internal/agent/runner.TestADroppedLateCallIsToldPastEverySavePoint`
  - `internal/sandbox.TestALateCallTheMiddleDropsIsCountedLate`
  - `internal/sandbox.TestALogsCountOfItsDroppedLateCallsSurvivesABuildThatDoesNotKnowIt`
  - `internal/engine.TestALaunchedSessionsCallOutlivingItsJobIsListedInItsRound`
  - `internal/engine.TestAReopenedSessionsLateCallIsListedInItsRound`
  - `internal/engine.TestALateCallsAppendIsAskedOnceAndAnswersNoError`
  - `internal/engine.TestACallTheLogTookIsNeverSaidToHaveLostItsOutcomeWhateverBecameOfItsSpend`
  - `internal/engine.TestAResumeHandsTheRunnerHowManyLateCallsItsLogDropped`
  - `internal/sandbox.TestAnOpeningCountsLateTheCallsTakenAfterItsReadThatTheMiddleDropped`
  - `internal/sandbox.TestALaterAttemptAtAnAnswerIsHandedTheCallTheFirstMade`
  - `internal/engine.TestAnOpeningMarksLateWhatTheLogTookAfterItsBriefsRead`
  - `internal/engine.TestAResumeHandsTheRunnerTheCountItsReadSaw`
  - `internal/agent/runner.TestARelaunchHandsItsLaunchTheReadItsBriefsWereBuiltFrom`
  - `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldTheCallsTheLogTookLate`
  - `internal/agent/prompts.TestALateCallTheRecordDroppedIsCountedUnderTheLateCallsWords`
  - `internal/engine.TestACallLandingWhileARelaunchIsReadiedIsToldToTheNextContinuation`
  - `internal/engine.TestAResumesReadReachesItsRelaunchsOpening`
  - `internal/agent/runner.TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnce`
  - `internal/engine.TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnceOffTheRoundsLog`
  - `internal/engine.TestWhatTheLogKeptOfACallIsWhatTheResumedTurnReads`
  - `internal/agent/runner.TestAMadeCallWhoseResultCannotBeReadHereIsToldAsMade`
  - `internal/engine.TestALaterAttemptAnswersAHeldCallFromTheMarkItsClaimRead`
  - `internal/toolsession.TestACallItsHarnessGaveUpOnReachesTheDispatcherAsAbandoned`
  - `internal/toolsession.TestACallWhoseResponseIsGoneReachesTheDispatcherAsAbandoned`
  - `internal/toolsession.TestACallItsHarnessCancelsReachesTheDispatcherAsAbandonedWhileItsAnswerIsKeptGoing`
  - `internal/api/mcpbridge.TestACallNobodyWaitedForIsRecordedSo`
  - `internal/api/mcpbridge.TestEveryEndingOfAnOpeningReturnsWhileACallIsHeld`
  - `internal/api/mcpbridge.TestACallWhoseToolReturnedBeforeItsOpeningEndedIsAnsweredWithItsOwnResult`
  - `internal/sandbox.TestALongRunsLogKeepsACallNobodyWaitedForAndCountsOneItDrops`
  - `internal/agent/runner.TestACallNobodyWaitedForIsToldPastEverySavePoint`
  - `internal/agent/runner.TestNoGuardCountsALoadNobodyWaitedFor`
  - `internal/engine.TestACallNobodyWaitedForIsToldToTheCopyInItsBox`
  - `internal/engine.TestACallNobodyWaitedForThatLandsOnNoRecordIsNamedAtWarning`
  - `internal/api/mcpbridge.TestACallIsOnItsRunsRecordBeforeItsToolRuns`
  - `internal/api/mcpbridge.TestACallWhoseEntryCannotBeWrittenIsNotRunAndIsRefused`
  - `internal/api/mcpbridge.TestACallWhoseEntryIsStillBeingWrittenIsNeverRunWhereItsOpeningEnds`
  - `internal/api/mcpbridge.TestAHeldCallIsRecordedWithoutItsArgumentsOrTheirDigest`
  - `internal/engine.TestAnUpdateTheStoreRefusesLeavesTheCallsIdentityOnRecord`
  - `internal/engine.TestABridgedCallsWritesOutlastALeadersReElection`
  - `internal/engine.TestACallWhoseEntryTheLogDoesNotTakeIsNamedAndRefused`
  - `internal/agent/runner.TestACallStillInFlightIsCountedByItsIdentityOnEveryPath`
  - `internal/agent/runner.TestACollectedRoundListsACallWhoseOutcomeIsUnknown`
  - `internal/sandbox.TestACallInFlightIsHeldToItsShare`
  - `internal/sandbox.TestTheWholeLogFitsItsBudget`
  - `internal/engine.TestACallsRecordIsUpdatedOnceByItsIdentity`
  - `internal/engine.TestAResumedTurnsDeliveryIsUnknownBesideACallWhoseOutcomeIsUnknown`
  - `internal/agent/prompts.TestACallWhoseOutcomeIsUnknownIsNeverToldItFinished`
  - `internal/agent/runner.TestAnUnfinishedCallIsToldItMayHaveTakenEffect`
  - `internal/agent/prompts.TestACallWhoseOutcomeIsUnknownIsNeverToldItDidNotTakeEffect`
  - `internal/agent/runner.TestEveryReaderOfARoundIsToldACallWhoseOutcomeIsUnknownMayHaveTakenEffect`
  - `internal/sandbox.TestEveryCollectionWaitsForACallInFlightBeforeItReadsTheLog`
  - `internal/sandbox.TestAWaitStopsAtTheCapAtACallsBoundAndAtItsProcessGone`
  - `internal/sandbox.TestAWaitWhoseWatchStopsHearingWatchesAgainAndMissesNothing`
  - `internal/sandbox.TestAParkedJobsSessionIsEndedAtItsCollection`
  - `internal/sandbox.TestAnEndingThatMayNotWaitHandsACallStillWaitedForToItsRetry`
  - `internal/sandbox.TestASeatsRetirementLeavesARunWhoseCallStillRunsToItsNextTick`
  - `internal/engine.TestACallStillRunningAsItsHarnessEndsItsRunCountsInTheTurnsOutcome`
  - `internal/engine.TestAnAnsweredParkContinuesOnAFreshJobAndSession`
  - `internal/api/mcpbridge.TestTheRunsCallLogCountsOnlyTheCallsItsRecordHolds`
  - `internal/api/mcpbridge.TestSideBySideCallsAreAdmittedInTheOrderTheyArrived`
  - `internal/api/mcpbridge.TestAnUpdateTheLedgerCouldNotLandIsAskedAgainOnceItsAnswerIsOut`
  - `internal/engine.TestASubmissionWhoseUpdateTheStoreRefusedLandsOnceTheStoreIsBack`
  - `internal/sandbox.TestEveryEndingWaitsForACallInFlightBeforeItDecidesTheTurnsEnd`
  - `internal/sandbox.TestACappedRunNamesACallInFlightAtOnceAndAsItsRecordGoes`
  - `internal/api/mcpbridge.TestEveryRecordOfABoxsCallCarriesTheIDItsHarnessGaveIt`
  - `internal/api/mcpbridge.TestACallIDIsReadOnlyUnderItsRunnersKeyAndOnlyWhole`
  - `internal/sandbox.TestACallsHarnessIDSurvivesABuildThatDoesNotKnowIt`
  - `internal/sandbox.TestABridgedCallIsHeldToItsShare`
  - `internal/sandbox/executor.TestEachBoxRunnerDeclaresTheCallIDKeyItsHarnessProfileReads`
  - `internal/engine.TestABridgedCallCarriesItsHarnessIDFromItsEntryToItsRecord`
  - `internal/engine.TestARecoveredRunsCallsCarryTheIDsTheirHarnessGaveThem`
  - `internal/e2e.TestAnExecutorThatAsksAQuestionParksAndResumesOnTheAnswer`
  - `internal/agent/runner.TestACopyHoldingItsEarlierSubmitWorkIsToldItDoesNotStand`
  - `internal/agent/ledger.TestTheShownGroupsAreWhatTheRenderPrints`
  - `internal/agent/prompts.TestAFreshRunIsToldTheRoundsCallsInItsOwnWords`
  - `internal/agent/prompts.TestReadsAnEarlierMessageCountedAreToldAsSuch`
  - `internal/agent/prompts.TestEveryCountThatSaysTheTallyIsBelowHasItBelow`
  - `internal/agent/runner.TestAFreshRunCarryingALostRoundIsToldEveryCallOfIt`
  - `internal/agent/runner.TestAFreshRunCarryingARotatedRoundIsToldEveryCallOfIt`
  - `internal/agent/runner.TestAFreshRunCarryingARefusedRoundIsToldEveryCallOfIt`
  - `internal/agent/runner.TestAFreshRunCarryingAnAnsweredRoundIsToldEveryCallOfIt`
  - `internal/agent/runner.TestEveryAskedFreshRunIsToldWhatTheRunBeforeItDidHappened`
  - `internal/agent/runner.TestARelaunchContinuingTheBaseIsToldEveryCallOfTheRound`
  - `internal/agent/runner.TestAFreshRunIsToldWhatTheBudgetsLeaveOutOfTheRound`
  - `internal/agent/runner.TestAReseedOverBudgetContinuedInPlaceIsToldExactlyTheReadsItLeftOut`
  - `internal/agent/runner.TestACopyContinuedInPlaceSeveralTimesIsToldEveryOwedReadOnce`
  - `internal/agent/runner.TestAnOwedReadTheLogDroppedIsCounted`
  - `internal/agent/runner.TestAKeptCaptureWhoseBriefLeftReadsOutIsToldThem`
  - `internal/agent/runner.TestTheFreshBriefKeepsItsOutline`
  - `internal/agent/runner.TestAFreshRunsToldCallsAreDefusedUnderItsToken`
  - `internal/agent/runner.TestAHeldSubmissionIsOneTheSessionShows`
  - `internal/agent/runner.TestTheCallsABriefLeftOutAreTheOnesItsRenderCounted`
  - `internal/agent/runner.TestAFreshRunCarryingARoundOnLaunchesNothingWhenItsCallsFillTheWindow`
  - `internal/agent/runner.TestEveryBriefRecordsTheReadsItsCapLeftOut`
  - `internal/agent/runner.TestAReadWithNoPlaceIsNeverRecordedLeftOut`
  - `internal/sandbox.TestALaunchRecordsTheOmittedCallsOfEachBrief`
  - `internal/sandbox.TestANewRoundRecordsNoOmittedCall`
  - `internal/sandbox.TestAFreshLaunchReadsNoContinuationItWasNotGiven`
  - `internal/sandbox.TestALaunchWhoseBriefsLeftOutMoreThanALogListsIsRefused`
  - `internal/sandbox.TestARecordReadsTheOmittedCallsOfTheBriefItsRunRead`
  - `internal/sandbox.TestAnOwnRunCaptureStatesTheReadsItsBriefLeftOut`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesAJobsOmittedCalls`
  - `internal/engine.TestARunLostWithItsHostCarriesItsRoundOnInAFreshRunToldItsCalls`
  - `internal/engine.TestAFreshJobsLeftOutReadsReachItsCopysNextBrief`
  - `internal/engine.TestALaunchCarriesWhatEachBriefLeftOutToTheJobsRecord`
  - `internal/engine.TestWhatEveryBriefLeftOutReachesTheSandboxLayer`
  - `internal/agent/runner.TestACutOffCopyIsToldTheLeadsItsJobsBriefTold`
  - `internal/agent/runner.TestAnOwnRunCopyIsToldNoLeadAgain`
  - `internal/agent/runner.TestACutOffKeptSessionIsToldTheLeadsItsJobsBriefTold`
  - `internal/agent/runner.TestOnlyTheLeadsTheJobsBriefToldAreToldAgain`
  - `internal/agent/runner.TestAContinuationOfAJobCutOffAfterTellingALeadAgainIsStillToldIt`
  - `internal/agent/runner.TestACarryInTheEnginesOwnWordsKeepsNoLead`
  - `internal/agent/runner.TestALeadACopyCouldNotReadIsHandedOnByItsNumber`
  - `internal/agent/runner.TestAToldAgainAnswerIsInNeutralWords`
  - `internal/agent/runner.TestABaseContinuationsLeadIsToldAgainWithTheRoundsItsBriefRendered`
  - `internal/agent/runner.TestATaskToldAgainSaysNothingOfANewBox`
  - `internal/agent/runner.TestASendBacksLeadIsToldAgainFromItsRound`
  - `internal/agent/runner.TestALeadOfAnotherSessionIsToldInWordsTrueOfAnySession`
  - `internal/agent/runner.TestAHeldLeadIsToldAgainWithWhatItsCallReturned`
  - `internal/agent/runner.TestAnApprovedCallItsLeadTellsAgainIsListedNowhereElse`
  - `internal/agent/runner.TestAToldAgainAnswerIsFencedUnderTheRelaunchsToken`
  - `internal/agent/runner.TestAnAnswerPastAPageIsCutAndSaidWhenToldAgain`
  - `internal/agent/runner.TestALeadThatDoesNotOpenIsSaid`
  - `internal/agent/runner.TestAHeldCallsLeadKeepsWhatItReturnedAsItsRowSealedIt`
  - `internal/agent/runner.TestEveryFormOfAnAnswerIsToldAgainInItsOwnWords`
  - `internal/agent/runner.TestALeadToldAgainKeepsEveryWordItRecomposes`
  - `internal/agent/runner.TestATaskToldAgainCarriesTheReviewItsBriefCarried`
  - `internal/agent/runner.TestABriefContinuingAKeptCaptureHandsOnTheLeadsItTold`
  - `internal/agent/runner.TestABriefContinuingTheBaseHandsOnTheTaskAsItRenderedIt`
  - `internal/agent/runner.TestATaskLeadKeepsTheLastRoundItsBriefRenderedByItsNumber`
  - `internal/agent/runner.TestATaskToldAgainRendersTheRoundsItsBriefRenderedByTheirNumbers`
  - `internal/agent/runner.TestAReviewWhoseRoundAParkDroppedIsSaidAsNotKept`
  - `internal/agent/prompts.TestLeadsToldAgainAreOnesTheSessionMayAlreadyShow`
  - `internal/agent/prompts.TestATurnToldAgainKeepsItsBoundaryAndNotItsBox`
  - `internal/agent/prompts.TestASendBackToldAgainSaysNothingOfTheSessionAbove`
  - `internal/agent/prompts.TestNestingAPromptMovesOnlyTheHeadingsItsBuilderWrote`
  - `internal/sandbox.TestALaunchMarksTheLeadsTheBriefItHandedTold`
  - `internal/sandbox.TestATakeKeepsEachLeadOnceNumberedAsItWasTaken`
  - `internal/sandbox.TestALeadABriefCouldNotReadIsMarkedByItsNumber`
  - `internal/sandbox.TestTheLeadsTheRunKeepsAreHeldToTheirShareWithTheCutSaid`
  - `internal/sandbox.TestTheCountOfKeptLeadsNeverDropsALeadTheirBytesWouldKeep`
  - `internal/sandbox.TestALeadThePartCannotWriteIsDroppedAndCounted`
  - `internal/sandbox.TestARecordWhoseBoxStartedItsOwnSessionTellsNoLead`
  - `internal/sandbox.TestAHeldResultKeptForALeadOpensWithItsCallsSeal`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesAJobsLeadMarks`
  - `internal/sandbox.TestALaterBuildsKeysInTheLeadsPartSurviveATake`
  - `internal/sandbox.TestTheRunIsNamedForItsLeadsByItsCreateAndKeptByEveryReset`
  - `internal/sandbox.TestAResumeReadsEveryLeadItsRowCountsOrSaysItIsNotKept`
  - `internal/sandbox.TestALeadsPartOfAnotherRunIsReplacedOnlyByTheRunItsRowNames`
  - `internal/sandbox.TestAnAttachMarksLeadsOnlyOnTheJobTheyWereKeptFor`
  - `internal/sandbox.TestAnAttachMarksTheLeadsItsBriefCouldNotRead`
  - `internal/sandbox.TestATakeTheStoreCannotWriteRefusesTheAttach`
  - `internal/sandbox.TestALeadsTakeThatNamesNoRunOrNoJobIsRefused`
  - `internal/sandbox.TestAResumeIsHandedTheLeadsBesideItsRowAndEveryOneItCannotRead`
  - `internal/sandbox.TestTheWholeLeadsPartFitsItsBudget`
  - `internal/sandbox.TestTheWorstLeadsPartSetsEveryField`
  - `internal/sandbox.TestEveryLeadKindAndOutcomeIsValid`
  - `internal/engine.TestAnAnswerCarriedOnInACutOffBoxReachesTheNextBriefAgain`
  - `internal/engine.TestAChainOfCutOffSessionsOutlivesAReadOfItsLeadsThatFailed`
  - `internal/engine.TestABaseContinuationRefusedBeforeItsHarnessWroteIsToldItsTurnAgain`
  - `internal/engine.TestAResumeOfAJobWhoseSessionHoldsItsBriefTellsNoLeadOfItAgain`
  - `internal/engine.TestAResumeThatCouldNotReadItsLeadsHandsOnTheOnesItsJobsBriefTold`
  - `internal/engine.TestAResumesLeadsReachItsRelaunchsBrief`
  - `internal/agent/runner.TestAFreshRunIsToldTheAnswersThatCarriedItsTurnOn`
  - `internal/agent/runner.TestAFreshRunIsToldEveryAnswerOfItsTurnNamingItsRound`
  - `internal/agent/runner.TestASentBackFreshRunIsToldTheTurnsAnswers`
  - `internal/agent/runner.TestABaseContinuationCarriesTheTurnsAnswers`
  - `internal/agent/runner.TestATaskToldAgainCarriesTheAnswersItsBriefTold`
  - `internal/agent/runner.TestEveryAnswerIsToldOnceUnderTheOldestTaskThatCarriesIt`
  - `internal/agent/runner.TestAnAnswerBeforeATaskThatDoesNotOpenIsToldOnItsOwn`
  - `internal/agent/runner.TestAnAnswerToldInsideATaskIsHandedOnTold`
  - `internal/agent/runner.TestATaskToldAgainPointsOnlyToTheRoundsItsLedgerLists`
  - `internal/agent/runner.TestAnAnswerPointsOnlyToARoundItsBriefListsCallByCall`
  - `internal/agent/runner.TestAFreshRunsEarlierHeldAnswerListsItsCallNowhereElse`
  - `internal/agent/runner.TestAnEarlierRoundsHeldCallIsToldOnceInAFreshBrief`
  - `internal/agent/runner.TestAFreshRunsEarlierAnswersAreFencedUnderItsToken`
  - `internal/agent/runner.TestWhatAFreshRunCannotBeToldOfTheTurnsAnswersIsSaid`
  - `internal/agent/prompts.TestEarlierAnswersAreToldInTheOrderTheyCame`
  - `internal/agent/prompts.TestAnAnswerNamesTheRoundItCameIn`
  - `internal/agent/prompts.TestACountedHeldCallIsOneOfItsEntrysCalls`
  - `internal/agent/prompts.TestAReviewNotKeptIsSaidAsGoneNeverAsACopyTheEngineHolds`
  - `internal/sandbox/sandboxtest`
  - `ALateCallOfTheRoundIsListedMarkedAndPaidByNoJob`
  - `ALateCallAskedAgainIsListedOnce`
  - `ALateCallKeepsItsMarkThroughEveryWriteOfItsRound`
  - `ALateCallWritesNoJobsSpend`
  - `ALateSubmissionNeverTakesTheRoundsWholeShare`
  - `OfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole`
  - `TheCallAPersonApprovedIsNeverLate`
  - `ACallTakenAfterTheBriefsReadIsMarkedLateByTheOpening`
  - `ACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob`
  - `ACallsEntryIsUpdatedOnceByItsIdentity`
  - `ALogAtItsCallsInFlightRefusesTheNextBegin`
  - `ALaterCallIsNumberedAfterACallStillInFlight`
  - `WhatALateCallOrOneNobodyWaitedForCommittedIsTheRounds`
  - `ARecordOfAnEarlierRoundIsListedLateInTheRoundAfterIt`
  - `ABeginOrWithdrawalOfAnEarlierRoundIsNeverTaken`
  - `ARecordOfAnEarlierRoundAskedAgainIsListedOnce`
  - `AnEarlierRoundsCallMovesNoPlaceOfTheRound`
  - `WhatAnEarlierRoundsCallCommittedIsTheNextRounds`
  - `ANewRoundCarriesTheCallsItsCollectionDidNotRead`
  - `ANewRoundCarriesTheAbsorbedCallsItsCollectionDidNotRead`
  - `ANewRoundStartsWithTheTallyOfTheRoundBefore`
  - `ACallCarriesItsHarnessIDWholeAndIsNeverFoundByIt`
  - `TheCallAPersonApprovedIsACallOfItsRound`
  - `ALaunchRecordsWhatTheBriefItHandedLeftOut`
  - `TheTurnsLeadsAreKeptBesideItsRowAndOutliveEveryRelaunch`
  - `AnEarlierRunsLeadsAreNoneOfTheTurnsNextRuns`
  - `internal/sandbox.TestACollectionRecordsWhatTheJobsLastRequestCarried`
  - `internal/sandbox.TestAClaimIsToldWhatTheReadMeasured`
  - `internal/sandbox.TestALaunchCarriesTheMeasureOfTheSessionItHandsItsRun`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesAJobsMeasure`
  - `internal/sandbox/executor.TestAClaudeRunsLastMainThreadRequestIsReadOffItsStream`
  - `internal/sandbox/executor.TestAMainThreadUsageThatDoesNotReadGivesNoFigure`
  - `internal/sandbox/executor.TestOpenCodesFigureIsItsNewestSettledMeasuredMessage`
  - `internal/sandbox/executor.TestTheRulesForAnExportAndForTheAPIAreOne`
  - `internal/sandbox/executor.TestTheScriptAsksTheSessionsNewestAssistantMessagesAPageAtATime`
  - `internal/sandbox/executor.TestTheScriptAsksNothingOnAnEndRequest`
  - `internal/sandbox/executor.TestTheMeasureRidesOutOfTheCollection`
  - `internal/sandbox/executor.TestALineCutShortGivesNoFigure`
  - `internal/sandbox/executor.TestAMessageThatDoesNotReadEndsTheReadingWithNone`
  - `internal/sandbox/executor.TestThePagesBudgetIsTheirsInAll`
  - `internal/sandbox/executor.TestTheScriptAsksNoMorePagesThanItsBound`
  - `internal/sandbox/executor.TestARequestWhileThePagesAreAskedStopsTheAsking`
  - `internal/sandbox.TestACollectionMadeAgainKeepsALaterBuildsWordOnItsMeasure`
  - `internal/sandbox/executor.TestTheCountIsTheAnchorWithItsOutputAndEverythingAfterIt`
  - `internal/sandbox/executor.TestTheAnchorSitsAtTheFirstRecordOfItsMessage`
  - `internal/sandbox/executor.TestARunStartsAtTheFirstRecordOfItsIdWithOrWithoutUsage`
  - `internal/sandbox/executor.TestACompactBoundaryRestartsTheCount`
  - `internal/sandbox/executor.TestABoundaryWithNoCountedReplyAfterItMarksTheCount`
  - `internal/sandbox/executor.TestOnlyAMainSystemBoundaryRestartsTheCount`
  - `internal/sandbox/executor.TestASyntheticOrUncountedReplyIsNoAnchor`
  - `internal/sandbox/executor.TestAnUnmeteredReplyIsNoAnchor`
  - `internal/sandbox/executor.TestTheTailIsChargedAtTheHarnessesLowestRate`
  - `internal/sandbox/executor.TestEveryChainedLineAfterTheAnchorIsCharged`
  - `internal/sandbox/executor.TestAnImageOrDocumentAfterTheAnchorMarksTheCount`
  - `internal/sandbox/executor.TestAMediaMarkNeedsABlocksType`
  - `internal/sandbox/executor.TestAPDFReadAfterTheAnchorMarksTheCount`
  - `internal/sandbox/executor.TestAClearingSinceTheLastBoundaryMarksTheCount`
  - `internal/sandbox/executor.TestAUsageThatDoesNotReadMarksTheCount`
  - `internal/sandbox/executor.TestALineThatIsNoRecordLeavesNoCount`
  - `internal/sandbox/executor.TestATornLastPieceIsNoLine`
  - `internal/sandbox/executor.TestAThreadPastItsBoundLeavesNoCount`
  - `internal/sandbox/executor.TestTheMarkSaidIsTheFirstOfTheirOrder`
  - `internal/sandbox/executor.TestOneWalkHandsEveryReadingEveryLine`
  - `internal/sandbox/executor.TestTheMetersFloorFigureIsWhatTheLayerReadsOfAReply`
  - `internal/sandbox/executor.TestTheCaptureCountsTheSessionItKeeps`
  - `internal/sandbox/executor.TestAPrecompactMemberMarksTheCapturesCount`
  - `internal/sandbox/executor.TestAnArchiveTheLayerGivesNoReadingLeavesNoCount`
  - `internal/sandbox/executor.TestAWriterBelowTheCountFloorLeavesNoCount`
  - `internal/sandbox/executor.TestTheCountIsReadOnlyWhereThePinIsInForce`
  - `internal/sandbox/executor.TestTheMainThreadIsReadOnceThroughTheBox`
  - `internal/sandbox/executor.TestAPrecompactInTheBoxMarksTheCount`
  - `internal/sandbox/executor.TestAReleaseBelowTheFloorIsMarkedBeforeAnythingIsRead`
  - `internal/sandbox/executor.TestAReadCutShortBetweenLinesLeavesNoCount`
  - `internal/sandbox/executor.TestEachRunnerReadsItsOwnHarnesssSession`
  - `internal/sandbox/executor.TestTheReportedWindowIsTheLeastItsRunReported`
  - `internal/sandbox/executor.TestTheCountNamesItsThreadsNewestRestart`
  - `internal/sandbox/executor.TestANewestRestartWhoseUUIDDoesNotReadLeavesNoCount`
  - `internal/sandbox/executor.TestAThreadWithNoChainedLineLeavesNoCount`
  - `internal/sandbox/executor.TestACountMarkedForWhatItsBytesCannotBoundNamesItsRestart`
  - `internal/sandbox/executor.TestAMarkedCaptureStillSaysWhetherAJobRestartedItsSession`
  - `internal/sandbox/executor.TestTheStandardWindowIsTheHarnessesOwn`
  - `internal/sandbox/executor.TestSendsWholeIsTheHarnessesThreshold`
  - `internal/sandbox/executor.TestNoBoundOrAnUnknownWindowMayCompact`
  - `internal/sandbox/executor.TestAReportedWindowBelowTheStandardDecides`
  - `internal/sandbox/executor.TestAReportedWindowNeverRaisesTheStandard`
  - `internal/sandbox.TestEveryCountMarkIsValid`
  - `internal/sandbox/executor.TestEveryClaudeRouteIsValid`
  - `internal/sandbox.TestTheWorseOfTwoCountsIsTheOneThatTellsMore`
  - `internal/sandbox.TestAJobThatRestartedItsSessionIsKnownByItsNewestRestart`
  - `internal/sandbox.TestACollectionRecordsTheCountOfTheCopyItsBoxHolds`
  - `internal/sandbox.TestACountOfAJobNotOverIsUnread`
  - `internal/sandbox.TestTheMainThreadIsReadBeforeTheTakeWithinItsWait`
  - `internal/sandbox.TestTheBoxReadBudgetHoldsTheMainThreadRead`
  - `internal/sandbox.TestAClaimIsToldTheCountTheReadMade`
  - `internal/sandbox.TestTheCaptureStampsItsCount`
  - `internal/sandbox.TestACapturesCountIsHeldToWhatARowCarries`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesTheCount`
  - `internal/sandbox.TestALaunchRecordsTheRestartOfTheSessionItHandsItsRun`
  - `internal/sandbox.TestAnUnboundedCountIsLoggedWithWhyByItsMark`
  - `internal/agent/runner.TestAnOwnRunCopyThatMayCompactIsToldAsACutOffOne`
  - `internal/agent/runner.TestAnOwnRunCopyThatSendsWholeIsToldAsHeld`
  - `internal/agent/runner.TestAKeptSessionThatMayCompactIsToldEveryCallFromTheRoundsStart`
  - `internal/agent/runner.TestTheKeptPathIsJudgedByTheWorseOfItsCaptureAndItsJobsCopy`
  - `internal/agent/runner.TestTheVerdictIsAskedOfTheBriefTheHeldTellingHands`
  - `internal/agent/runner.TestAMarkNoCountOrUnknownWindowIsDecidedBeforeAnyBrief`
  - `internal/agent/runner.TestASessionThatMayCompactArmsNoSkill`
  - `internal/agent/runner.TestACutOffCopyFarFromItsThresholdKeepsItsLaunchArmedSkills`
  - `internal/agent/runner.TestTheToldRecordFollowsTheTellingHanded`
  - `internal/agent/runner.TestASendBacksKeptContinuationIsJudgedToo`
  - `internal/agent/runner.TestACutOffCopyThatMayCompactKeepsItsCutOffWords`
  - `internal/agent/runner.TestAKeptCaptureCutOffSaysWhetherItsJobRestarted`
  - `internal/agent/runner.TestTheNoteCursorIsNeverReadWhereTheSessionMayCompact`
  - `internal/agent/runner.TestEveryTellingOfAPathReadsItsOneValue`
  - `internal/agent/runner.TestASessionItsJobRestartedIsToldAsOneThatMayBeASummary`
  - `internal/agent/prompts.TestAMayCompactSessionIsToldWhyAndNeverThatItWasCutOff`
  - `internal/engine.TestTheProbeJudgesUnderTheChainsSmallestStandardWindow`
  - `internal/sandbox/executor.TestATellingJudgesAnEntryOnItsCoarseRoute`
  - `internal/engine.TestEveryBasisTheProbeAnswersCarriesTheWindow`
  - `internal/engine.TestAProbeOfACapturePassesItsCount`
  - `internal/engine.TestRunnerResumeCarriesTheJobsCount`
  - `internal/engine.TestAnInBoxContinuationIsJudgedByItsCollectionsCount`
  - `internal/engine.TestACapturedSessionIsJudgedByItsCapturesCount`
  - `internal/sandbox/executor.TestEveryCallToTheRunsServerIsBounded`
  - `internal/sandbox/executor.TestACallItsServerNeverAnswersGoesAsACallToAGoneServerGoes`
  - `internal/sandbox/executor.TestAnEndedRunsPathFitsTheEnginesWait`
  - `internal/sandbox/executor.TestAHungInterruptKeepsNoEndFromEnding`
  - `internal/sandbox/executor.TestAnExportPastItsBoundLeavesTheSessionToThePackOnDemand`
  - `internal/sandbox.TestACallIsHeldOnlyThroughItsLoggedResult`
  - `internal/sandbox.TestHoldingsAreReadOnlyOfTheCaptureItsRelaunchContinues`
  - `internal/sandbox/executor.TestAReadingIsOnlyForAResumerWhoseResumeTheModelWasReadFrom`
  - `internal/sandbox/executor.TestParallelResultsAfterTheLastRestartAreLive`
  - `internal/sandbox/executor.TestAResultBeforeTheLastRestartIsNotLive`
  - `internal/sandbox/executor.TestASubagentIsReportedOnlyWhereItsOneConversationAnsweredAndFinishedEveryLeg`
  - `internal/sandbox/executor.TestARefusedReplyLeavesItsSubagentNotWhole`
  - `internal/sandbox/executor.TestARetractionListInASubagentsThreadLeavesItNotWhole`
  - `internal/sandbox/executor.TestALegFinishedWithoutTextIsUnfinished`
  - `internal/sandbox/executor.TestATornSubagentThreadIsNotWhole`
  - `internal/sandbox/executor.TestASubagentPastTheChainsBoundIsCountedStill`
  - `internal/sandbox/executor.TestAnAnswerCarryingAHandbackAnswersNoLeg`
  - `internal/sandbox/executor.TestAReadingDecidesOnlyOnceItsStreamEnded`
  - `internal/sandbox/executor.TestWhatMembersHandOutIsHeldToTheArchivesBudget`
  - `internal/engine.TestTheRestoreOpensTheObjectItsRowNamesAsItStreams`
  - `internal/sandbox/executor.TestAResultItsHarnessReplacedIsNotLive`
  - `internal/sandbox/executor.TestNoIDTheCallIDRuleRefusesIsKept`
  - `internal/sandbox/executor.TestAMainThreadTheResumeModelCannotPlaceIsUnread`
  - `internal/sandbox/executor.TestACallIsHeldOnlyWhereItsResumedRequestPairsItsUseWithItsResult`
  - `internal/sandbox/executor.TestACallIsHeldOnlyWhereItsUseNamesAToolOfTheBridges`
  - `internal/sandbox/executor.TestACallsIDOnAnyRecordOfItsFileIsAnotherCarrier`
  - `internal/sandbox/executor.TestACallWhoseUseCarriesNoReplyIDIsTold`
  - `internal/sandbox/executor.TestACallWhoseRecordARetractionNamesIsTold`
  - `internal/sandbox/executor.TestANoticeBeforeTheLastRestartAnswersNoLeg`
  - `internal/sandbox/executor.TestTheChainModelsTheResumeOfTheReleasesItWasReadFromAlone`
  - `internal/sandbox/executor.TestTheLeafSetIsCjtsOnTheRelinkedParents`
  - `internal/sandbox/executor.TestARelinkHandsTheKeptRecordsUnderItsAnchor`
  - `internal/sandbox/executor.TestARetractionTakesBackWhatTheRecordsItHandsName`
  - `internal/sandbox/executor.TestARecoveredRecordIsPlacedByItsPlaceInTheFile`
  - `internal/sandbox/executor.TestASubagentsConversationIsTheOneItsOwnLoadHands`
  - `internal/sandbox/executor.TestAThreadPastFiveMebibytesIsReadAsBothItsLargeFileReadersReadIt`
  - `internal/sandbox/executor.TestARecordSaysWhichRecordsItsHarnessTookBack`
  - `internal/sandbox/executor.TestACompactionsBoundarySaysWhatItKept`
  - `internal/sandbox/executor.TestARecordWrittenBelowTheFloorEndsTheReading`
  - `internal/sandbox/executor.TestASubagentRecordWrittenBelowTheFloorLeavesItUnreported`
  - `internal/sandbox/executor.TestALineHandsTheReleaseThatWroteIt`
  - `internal/agent/ledger.TestAValueIsShownWholeExactlyWhereItsLineShowsIt`
  - `internal/agent/runner.TestAHeldCallAToldCallCouldBeTakenForIsListedAgain`
  - `internal/agent/runner.TestAWriteWhoseArgumentsReadAlikeIsListedAgain`
  - `internal/agent/runner.TestACallWhoseShownArgumentsDifferIsNotListedAgain`
  - `internal/agent/runner.TestACallWhoseArgumentsAreNotKnownIsListedBesideEveryCallOfItsTool`
  - `internal/agent/runner.TestAHeldCallLikeALateCallIsListedAgainForEveryHarness`
  - `internal/agent/runner.TestACallTheWordsSayTheSessionLacksBringsNoListing`
  - `internal/agent/runner.TestTheCallsListedAgainTakeTheReadCapLastAndAreNeverOwed`
  - `internal/agent/runner.TestAnOpenCodeRelaunchWithNoCallsAlikeIsToldAsMainTellsIt`
  - `internal/agent/prompts.TestTheCallsListedAgainAreOnesAToldCallCouldBeTakenFor`
  - `internal/agent/runner.TestEveryReasonACallIsListedAgainIsHeadedByTheSameWords`
  - `internal/agent/runner.TestTheApprovedCallTheLeadTellsIsNeverListedAgain`
  - `internal/agent/runner.TestAValueReadsAsTheValueItIsNeverAsItsSpelling`
  - `internal/agent/runner.TestTheCallsListedAgainAreDefusedUnderTheTurnsToken`
  - `internal/agent/runner.TestAClosedRoundsCallAToldCallCouldBeTakenForIsListedAgain`
  - `internal/agent/runner.TestTheRoundASendBackCarriesIsListedAgainOnce`
  - `internal/agent/runner.TestAClosedRoundATaskToldAgainRendersIsNeverListedAgain`
  - `internal/agent/runner.TestAClosedSubmissionIsNeverListedAgain`
  - `internal/agent/runner.TestAClosedHeldCallAnAnswerHereTellsIsNeverListedAgain`
  - `internal/agent/runner.TestAFreshRunListsNothingAgain`
  - `internal/agent/runner.TestAClosedCallIsComparedAsAParkKeepsIt`
  - `internal/agent/runner.TestTheSummarizedTellingIsNeverTheShorterWithClosedCallsListedAgain`
  - `internal/agent/runner.TestTwoToldCallsThatCouldBeTakenForEachOtherAreEachMarked`
  - `internal/agent/runner.TestAToldCallLikeAHeldCallALeadTellsIsMarked`
  - `internal/agent/runner.TestALateCallLikeAWorkAlreadyDoneIsMarked`
  - `internal/agent/prompts.TestAnEarlierRoundsCallIsSaidToBeOfAnEarlierRoundOfTheTurnOrFire`
  - `internal/agent/ledger.TestANoteEndsTheLineOfTheCallItNames`
  - `internal/agent/ledger.TestShownIndicesIsTheWalkShownCallGroupsDraws`
  - `internal/agent/prompts.TestOtherCallsAreSaidToBeNoneOfTheListedOnes`
  - `internal/agent/runner.TestASessionThatMayHoldAnEarlierTurnIsToldItsCallsAreNoneListed`
  - `internal/agent/runner.TestCallsTheRecordNoLongerListsAreNamedByTheToolsTold`
  - `internal/agent/runner.TestASessionThatMayHoldItsEarlierRoundsAsASummaryIsToldSo`
  - `internal/agent/runner.TestCallsWorkersMadeAreNamedByTheToolsTold`
  - `internal/agent/runner.TestTheOtherCallsAreToldOnlyBesideWordsThatSayACallMayBeShownTwice`
  - `internal/agent/runner.TestAFreshRunIsToldNoOtherCalls`
  - `internal/agent/runner.TestAValueEitherLineShowsAsAPieceReadsAlikeAnything`
  - `internal/agent/runner.TestAValueARecordLeftOutReadsAlikeAnythingAndTheRestIsCompared`
  - `internal/kept.TestAValueARecordLeftOutIsKnownAsLeftOut`
  - `internal/agent/ledger.TestACallOfUnknownOutcomeSaysNoRecordKeepsItsArguments`
  - `internal/agent/ledger.TestACallWhoseArgumentsDidNotReadBackSaysSo`
  - `internal/engine.TestACallWhoseRecordedArgumentsDoNotReadBackIsMarkedSo`
  - `internal/agent/runner.TestACallWhoseArgumentsDidNotReadBackIsSeededAsOneNoLaterCallRepeats`
  - `internal/agent/execstate.TestAClosedRoundsCallKeepsWhatItsRecordSaysOfItsArguments`
  - `internal/api/mcpbridge.TestARecordedCallCarriesItsTimingAndWhoAnswered`
  - `internal/engine.TestABridgedCallsRowCarriesItsTimingAndWhoAnswered`
  - `internal/sandbox.TestTheWorstRowSetsEveryField`
  - `internal/agent/runner.TestAPartBesideTheTallyOfItsCallsIsNeverSaidToBeNamedByNoList`
  - `internal/agent/runner.TestEveryReasonADroppedCallMayBeShownNamesTheDroppedCalls`
  - `internal/agent/runner.TestASendBacksOwnRoundIsNoEarlierRoundOfItsOtherCalls`
  - `internal/agent/runner.TestAClosedTallyUnderASummaryIsNamedAmongTheEarlierRounds`
  - `internal/agent/runner.TestAHeldCallListedAgainLeavesTheClosedCallsOfItsToolNamed`
  - `internal/agent/runner.TestTheCopyInItsBoxIsToldItsClosedRoundsAsAKeptSessionIs`
  - `internal/agent/runner.TestAHeldCallBesideAnEarlierRoundsAnswerOfItsToolIsMarked`
  - `internal/agent/runner.TestAClosedWithheldCallOfAnotherRoundThanALeadsIsStillListedAgain`
  - `internal/agent/runner.TestASpanHoldingTheToldRoundIsNoPartnerOfItsCalls`
  - `internal/engine.TestACallStillInFlightCarriesItsHarnessID`
  - `internal/sandbox.TestABriefToldByAReadingIsHandedOnlyToABoxThatAnswersItsRelease`
  - `internal/sandbox.TestAnEmptyBriefToldByAReadingFallsBackToTheBriefWithout`
  - `internal/sandbox.TestAKeptSessionTheBoxStillHoldsRecordsWhatItsBriefLeftOut`
  - `internal/sandbox.TestAJobRecordsWhatTheBriefItWasHandedLeftOut`
  - `internal/sandbox.TestALaunchSaysWhetherItHandedTheBriefToldByAReading`
  - `internal/sandbox.TestALaunchRefusesABriefToldByAReadingThatLeftOutMoreThanALog`
  - `internal/engine.TestARelaunchReadsItsSessionThroughTheMemberThatWroteIt`
  - `internal/engine.TestAReadingThatFailsIsLoggedAndHoldsNothing`
  - `internal/engine.TestAReadingIsLoggedWithWhatItHeldAndTold`
  - `internal/engine.TestAProbedBasisCarriesTheReleaseThatWroteTheSession`
  - `internal/engine.TestTheBriefToldByAReadingReachesTheSandboxBesideTheOtherTwo`
  - `internal/engine.TestABriefToldByAReadingTheFloorCannotCarryIsDroppedAlone`
  - `internal/engine.TestTheLaunchsDecisionSaysWhetherItsBriefWasToldByAReading`
  - `internal/agent/runner.TestACutOffKeptSessionIsToldOnlyTheCallsItsReadingDoesNotHold`
  - `internal/agent/runner.TestAReadingIsAskedOnlyOfTheCallsTheTellingWouldList`
  - `internal/agent/runner.TestAnIDACallInFlightCarriesIsNeverAskedAbout`
  - `internal/agent/runner.TestAReadingIsAskedOncePerRelaunch`
  - `internal/agent/runner.TestNoReadingIsAskedOfACaptureWhoseHarnessEndedItsOwnRun`
  - `internal/agent/runner.TestAnOpenCodeRelaunchWithNoReadingIsToldAsBefore`
  - `internal/agent/runner.TestNoReadingIsAskedWhereTheFirstRequestCompactsWhatever`
  - `internal/agent/runner.TestOnlyAFirstRequestThisBuildJudgesCarriesABriefToldByAReading`
  - `internal/agent/runner.TestABriefToldByAReadingIsOfferedOnlyWhereItsOwnFirstRequestSendsWhole`
  - `internal/agent/runner.TestACallTheReadingTellsUnderASubagentIsListedUnderThatSubagent`
  - `internal/agent/runner.TestASubagentsSubmissionIsSaidNotToStandUnderItsName`
  - `internal/agent/runner.TestASubagentNamedInABriefIsDefusedAndCut`
  - `internal/agent/runner.TestASubagentsLeftOutReadsAreOwedByTheNextBrief`
  - `internal/agent/runner.TestACallTheReadingHoldsIsListedAgainBesideALikeCall`
  - `internal/agent/runner.TestAGuardArmsTheLoadsTheHandedReadingHolds`
  - `internal/agent/runner.TestASubmissionTheReadingHoldsIsSaidNotToStand`
  - `internal/agent/runner.TestACallTheReadingHoldsIsListedAgainBesideALikeCallUnderASubagent`
  - `internal/agent/runner.TestASubagentsCallAndALikeCallOfTheMainListAreEachMarked`
  - `internal/agent/runner.TestASubagentsToolsNameNoCallTheSessionMayShowThatNoListHolds`
  - `internal/agent/runner.TestTheRunsOpeningFrameIsTheBriefToldByTheReadingWhereItWasHanded`
  - `internal/agent/runner.TestAReadingThinsTheCallsOfASessionItsJobRestarted`
  - `internal/agent/runner.TestACallNoRecordNamesIsToldWhetherOrNotAnEarlierBriefToldIt`
  - `internal/agent/prompts.TestAListAReadingThinnedSaysTheSessionHoldsSomeLeftOut`
  - `internal/agent/prompts.TestTheReadHeaderIsNoLongerThanTheHeaderItReplaces`
  - `internal/agent/prompts.TestASubagentsCallsAreToldUnderItsName`
  - `internal/e2e.TestACutOffSessionsRelaunchIsToldOnlyTheCallsItsSessionLacks`
  - `internal/e2e.TestARelaunchInABoxOfAnotherReleaseIsToldEveryCallAfterTheSavePoint`
  - `internal/engine.TestALateCallReachesTheWorkingJobAtItsNextBatch`
  - `internal/engine.TestNoLateCallIsHandedAtAClaudeCodeFinish`
  - `internal/engine.TestAPersonsNoteIsNeverLeftWaitingForItems`
  - `internal/engine.TestALateCallReachesAnOpenCodeJobAtItsNextPollAndIsReadOnItsConfirmation`
  - `internal/engine.TestAnOpenCodeJobConfirmsTwelveNotesAndItemsInOneRead`
  - `internal/engine.TestALateCallOnAnotherNodeReachesTheJobThere`
  - `internal/sandbox.TestAnItemsCallIsHeldToItsShare`
  - `internal/sandbox.TestALateCallKeepsALaterBuildsKeysThroughThisBuildsWrite`
  - `internal/agent/prompts.TestALateCallArrivedStatesTheLateWordsFactsAsStatements`
  - `internal/agent/ledger.TestACallLinesHalvesAreTheLineTheLedgerDraws`
  - `internal/sandbox.TestACallTellingRidesAnOlderBuildsWrite`
  - `internal/agent/runner.TestASessionThatTookALateCallIsNotToldItAgain`
  - `internal/agent/runner.TestACaptureReadBeforeItsTellingIsToldTheCall`
  - `internal/agent/runner.TestASessionThatMayLackItsBriefCreditsNoTelling`
  - `internal/agent/runner.TestAFreshRunIsToldEveryLateCall`
  - `internal/agent/runner.TestAJobsOwnCopyCreditsEveryTellingOfTheRow`
  - `internal/agent/runner.TestALateCallABriefLeftOutIsRecordedApartAndNeverCredited`
  - `internal/agent/runner.TestATellingNeverCountsALoad`
  - `internal/agent/runner.TestAnOwedReadAndALateCallTheLogDroppedAreEachCountedOnce`
  - `internal/engine.TestTheResumeHandsOnlyTheTellingsOfItsLogsRound`
  - `internal/engine.TestAnOpenCodeJobThatConfirmedALateCallIsNotToldItByItsCopy`
  - `internal/sandbox/sandboxtest`
  - `ALateCallIsHandedToTheJobItsRoundRunsOn`
  - `ACallItsOwnJobGaveUpOnIsHandedToThatJob`
  - `EveryCallTheOpeningWalksIsHandedToTheJobItOpens`
  - `ALateCallAskedAgainAfterItsAnswerWasLostIsHandedOnce`
  - `NoItemIsFiledForAJobThatCannotTakeIt`
  - `NoItemIsFiledForAJobTheRowNoLongerNames`
  - `AJobsLateItemsStopAtMaxJobItems`
  - `AnItemKeepsTheOrdinaryShareOfItsCall`
  - `AnEarlierRoundsCallIsHandedToTheJobTheNextRoundRunsOn`
  - `AnItemTheStoreDidNotTakeLeavesTheAppendAsItWas`
  - `AConfirmationOfAnItemRecordsItsTellingAtItsOwnPlace`
  - `AHooksTakeRecordsNoTelling`
  - `AHeldVerdictRecordsATellingAtItsOwnPlaceAndARestartedOneNone`
  - `ATellingOutlivesItsItem`
  - `ANewRoundClearsTheTellings`
  - `TellingsAreOnePerCallAndBounded`
  - `internal/sandbox/executor.TestAnOpenCodeExportIsCountedAsOpenCodeCountsIt`
  - `internal/sandbox/executor.TestAnOpenCodeCountRestartsAtACompletedCompaction`
  - `internal/sandbox/executor.TestAnOpenCodeRestartIsAnyCompaction`
  - `internal/sandbox/executor.TestAnOpenCodeSessionWithNoReplySinceItsCompactionIsUnanchored`
  - `internal/sandbox/executor.TestAFileAfterTheReplyMarksTheCount`
  - `internal/sandbox/executor.TestAnOpenCodeCountNamesItsReplysProvider`
  - `internal/sandbox/executor.TestAReleaseOutsideTheSetIsNoCount`
  - `internal/sandbox/executor.TestTheOpenCodeSetIsTheReleasesProbed`
  - `internal/sandbox/executor.TestTheOpenCodeCountHoldsNoStringOfAnExport`
  - `internal/sandbox/executor.TestAnOpenCodeCopyIsReadOffTheExportItsJobLeft`
  - `internal/sandbox.TestTheCollectionReadsAnOpenCodeCopyItsJobEndedItself`
  - `internal/sandbox.TestTheProviderRidesBesideTheCount`
  - `internal/sandbox.TestOpenCodeSendsWholeStrictlyBelowThePoint`
  - `internal/sandbox.TestAnotherProvidersCountIsNoBound`
  - `internal/sandbox.TestAnOpenCodeContinuationThatMayCompactIsHandedTheSummarizedBrief`
  - `internal/sandbox.TestTheOwnCopyIsReadToo`
  - `internal/sandbox.TestAFailedReadHandsTheSummarizedBrief`
  - `internal/sandbox.TestAnOpenCodeTellingByAReadingIsJudgedOnItsOwnBrief`
  - `internal/sandbox/executor.TestThePointIsReadInTheContinuationsBox`
  - `internal/sandbox/executor.TestADefaultedLimitIsNoPoint`
  - `internal/sandbox/executor.TestAZeroLimitIsNoPointWhereCompactionIsOn`
  - `internal/sandbox/executor.TestTheHighestDocumentsBufferMovesThePoint`
  - `internal/sandbox/executor.TestAPluginOtherThanTheEnginesIsNoPoint`
  - `internal/sandbox/executor.TestTheReadKeepsNoValueItWasHanded`
  - `internal/agent/runner.TestByBoxBuildsBothTellings`
  - `internal/agent/runner.TestAnUnboundedOpenCodeCountIsToldAsASummaryWithNoRead`
  - `internal/agent/runner.TestAClaudeCodeContinuationIsToldAsOnMain`
  - `internal/engine.TestTheFloorCountsTheSummarizedBrief`
  - `internal/sandbox.TestWhatTheBoxHandsOverBesideTheBriefIsCharged`
  - `internal/sandbox.TestASummarizedTellIsBounded`
  - `internal/sandbox/executor.TestTheReadSeesTheBoxsDocumentsNotTheImages`
  - `internal/sandbox/executor.TestCompactionOffSendsWholeWhateverTheCount`
  - `internal/sandbox/executor.TestTheShareHoldsBackFifteenHundredths`
  - `internal/sandbox/executor.TestTheLimitIsTheSmallerOfInputAndContext`
  - `internal/sandbox/executor.TestANativeCompactionOrContextManagementIsNoPoint`
  - `internal/sandbox/executor.TestABoxOfAnotherReleaseThanTheWritersIsNoPoint`
  - `internal/agent/runner.TestTheGuardOfASummarizedHandoffArmsNothing`
  - `internal/agent/runner.TestAnOpenCodeRestartInTheJobVoidsWhatItsBriefToldHeld`
  - `internal/sandbox.TestAFreshLaunchKeepsItsTaskLead`
  - `internal/sandbox.TestAFailedRestoreKeepsTheFreshBriefsTask`
  - `internal/agent/runner.TestEveryBriefThatMayStartASessionHandsItsTask`
  - `internal/agent/runner.TestAFreshTaskIsToldAgainWhereTheSessionMayBeASummary`
  - `internal/agent/runner.TestACutOffFreshSessionIsNotToldItsTask`
  - `internal/agent/runner.TestTheTurnsAskIsToldOnceWhereSeveralLeadsKeepIt`
  - `internal/sandbox.TestAFinalTakeNeverLosesItsPlaceToASave`
  - `internal/sandbox.TestEveryReaderOfASaveUnderWayReadsItThroughOneRule`
  - `internal/sandbox.TestAnOlderBuildsWriteCarriesASaveUnderWay`
  - `internal/sandbox.TestAnEndWaitsForASaveAFinalTakeOvertook`
  - `internal/sandbox.TestAnEndDoesNotWaitForASaveAKeptCaptureSuperseded`
  - `internal/sandbox.TestASaveWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow`
  - `internal/sandbox.TestAnUnresolvedSaveIsAskedAgainBeforeARelaunchOrAnEndDecides`
  - `internal/sandbox.TestARelaunchWaitsForASaveOfTheJobItFollows`
  - `internal/sandbox.TestWhatAReaderIsToldOfARelaunchIsTheRelaunchsOwnDecision`
  - `internal/sandbox.TestARowIsReadAgainOnlyWhereThatCanChangeItsAnswer`
  - `internal/api/queries.TestWhetherARelaunchContinuesIsUndecidedWhereARelaunchWouldNotDecideEither`
  - `internal/sandbox.TestARelaunchWaitsOnlyForACaptureItsDecisionDependsOn`
  - `internal/sandbox.TestACaptureTheLogTookIsSettledLandedWhateverItsTakersLease`
  - `internal/sandbox.TestAReadStandingByKeepsNothingWhereTheLogTookTheCaptureItStoodByFor`
  - `internal/sandbox.TestARelaunchInItsJobsOwnBoxContinuesTheCopyThatWentPastItsCapture`
  - `internal/sandbox.TestARelaunchContinuesTheNewestKeptCaptureOfItsJobPastOneNotKept`
  - `internal/sandbox.TestARelaunchContinuesTheNewestKeptCaptureBesideANewerOneStillInFlight`
  - `internal/sandbox.TestWhetherASessionContinuesIsOneDecision`
  - `internal/engine.TestALostJobWithItsBoxGoneContinuesItsNewestKeptCapture`
  - `internal/api/queries.TestALiveRowIsDecidedOnTheLogAsARelaunchReadsIt`
  - `internal/api/queries.TestAParkedRunsSessionIsContinuedInItsOwnBoxWhereNoOtherBoxWould`
  - `internal/agent/prompts.TestASessionSavedWhileItsRunWorkedSaysWhenAndThatItWentOn`
  - `internal/agent/runner.TestASessionSavedWhileItsHarnessWorkedIsSaidSo`
  - `internal/agent/runner.TestALostJobContinuedFromASaveIsToldItWentOnPastIt`
  - `internal/sandbox.TestALostRunsLineSaysHowLongItRanAndSinceItsSave`
  - `internal/sandbox.TestAReadingOfACaptureTheBoxsCopyWentPastIsSaidUnused`
  - `internal/sandbox/sandboxtest`
  - `ASaveReservesItsPlaceOnlyWhileItsJobRunsUndecided`
  - `AFinalTakeAfterASaveTakesThePlaceAfterIt`
  - `OneSaveOfARunIsInFlightAcrossTheFleet`
  - `ASaveStaysInSightUntilItCanNoLongerBeTheTurnsKeptCapture`
  - `ASaveIsSettledOnlyOverItsBegunStamp`
  - `AStampTheRowHoldsInAnySectionIsAnsweredTaken`
  - `internal/config.TestTheSaveIntervalIsBoundedAndZeroTakesTheDefault`
  - `internal/sandbox/executor.TestASaveNotTakenIsRemovedFromItsBox`
  - `internal/sandbox/executor.TestASessionsStreamedLengthIsItsHarnesssEventStream`
  - `internal/sandbox/executor.TestEveryCarryingRunnerSaves`
  - `internal/sandbox/executor.TestASaveDuringASubagentIsTakenAndToldWhatTheSubagentDid`
  - `internal/sandbox.TestASaveIsDueOnlyOnceItsIntervalPassedAndItsSessionMoved`
  - `internal/sandbox.TestASaveAfterARestartIsDueWithinOneIntervalWhateverTheClocks`
  - `internal/sandbox.TestASaveThatFailedIsNotTriedAgainWithinTheInterval`
  - `internal/sandbox.TestASaveAfterAnUnresolvedSaveAsksItAgainFirst`
  - `internal/sandbox.TestASaveAfterItsTakerDiedSettlesThatTakersSave`
  - `internal/sandbox.TestASeatsSavesStayWithinItsAllowance`
  - `internal/sandbox.TestASeatsSavesArePacedBySizeAsItsAllowanceRunsShort`
  - `internal/sandbox.TestASaveIsScrubbedOfEverySecretItsBoxWasHanded`
  - `internal/sandbox.TestASaveTakenAfterARenewalHoldsNoRenewedValue`
  - `internal/sandbox.TestASaveNeverHoldsThePollOrReadsTheBoxInIt`
  - `internal/sandbox.TestEveryRunningBoxThePollFindsIsOfferedForASave`
  - `internal/sandbox.TestTheSaveSlotsBoundTheTakesAndTheKeepsOnANode`
  - `internal/sandbox.TestItsWaitersStopCancelsASavesPackAndNeverItsTake`
  - `internal/sandbox.TestAPollThatEndsAfterItsWaiterStoppedOffersNoSave`
  - `internal/sandbox.TestOneSaveOfARunIsUnderWayOnANode`
  - `internal/sandbox.TestAStoppingCoordinatorTakesNoSaveAndStampsTheOneItCut`
  - `internal/sandbox.TestASaveIsCutOffAndPlacedAtItsLaunchsNewestSavePoint`
  - `internal/sandbox.TestASavesOutcomeLandsWithinItsLife`
  - `internal/sandbox.TestASaveIsTakenWholeAfterAPackAtItsBound`
  - `internal/sandbox.TestARunnerThatTakesNoSaveIsOfferedNone`
  - `internal/sandbox.TestALostJobWithItsBoxGoneContinuesItsNewestSave`
  - `internal/sandbox.TestADrainSavesEveryMovedDirectJobOnItsHostMostStaleFirst`
  - `internal/sandbox.TestADrainAdmitsNoPeriodicSave`
  - `internal/sandbox.TestADrainSaveEndsWithinItsWait`
  - `internal/sandbox.TestTheSaveLedgerHoldOutlastsTheCollectorAndFitsTheUploadsBucket`
  - `internal/agent/prompts.TestAFreshBriefSaysItsTurnsEarlierSessionIsKeptUntilItsRunSaves`
  - `internal/engine.TestTheWaiterTheEngineBuildsOffersItsBoxesForASave`
  - `internal/engine.TestASavesIntervalIsReadOffThisNodesRevision`
  - `internal/engine.TestTheDrainSavesBesideItsWaitForTurns`
  - `internal/agent/runner.TestTheReviewerReadsALateCallAsAnEarlierRunsCall`
  - `internal/agent/runner.TestAResumedRunMayCiteADeliveryALateCallMade`
  - `internal/engine.TestARecordOfAnEarlierRoundReachesTheNextRoundsLogThroughItsSessionsLedger`
  - `internal/engine.TestACallLandingBeforeASendBacksOpeningReachesTheSentBackJob`
  - `internal/engine.TestASendBacksLaunchHandsItsOpeningTheReadItsBriefsWereBuiltFrom`
  - `internal/agent/runner.TestALateCallWithNoPlaceIsToldByTheNextRoundsContinuation`
  - `internal/sandbox/executor.TestEachReleasesBridgedCallsReadAsItsOwnExportHoldsThem`
  - `internal/sandbox/executor.TestAClassIsHeldOnlyWhereEveryCallOfItHasAPart`
  - `internal/sandbox/executor.TestACallTheLogCannotStateMakesItsClassWild`
  - `internal/sandbox/executor.TestAPartMatchingTwoClassesCountsForNeither`
  - `internal/sandbox/executor.TestOnlyAPartOpenCodeHandsItsModelVerbatimCounts`
  - `internal/sandbox/executor.TestAnOpenCodeReadingReadsOnlyReleasesItWasMeasuredOn`
  - `internal/sandbox/executor.TestAReadingHoldsNoMessage`
  - `internal/agent/runner.TestAnOpenCodeCallIsKeyedByItsPlaceInItsLog`
  - `internal/sandbox/executor.TestAnOpenCodeSaveIsExportedBesideItsServerToItsTakersOwnPath`
  - `internal/sandbox/executor.TestAnOpenCodeExportPastWhatTheScrubReadsIsNeverPacked`
  - `internal/sandbox/executor.TestAnOpenCodeSaveWaitsForTheRoomItsExportTakes`
  - `internal/sandbox.TestEveryCaptureNoReadPlacesIsReservedAtZeroAsTheStoreHoldsIt`
  - `internal/sandbox.TestANodePacksOneSessionAtATimeInItsOwnMemory`
  - `internal/sandbox.TestASaveItsBoxHasNoRoomForWaitsNeverDoubled`
  - `internal/sandbox.TestALaunchRecordsThePointOfTheKeptCaptureItHandsItsRun`
  - `internal/sandbox.TestAnOlderBuildsWriteKeepsTheContinuedPoint`
  - `internal/sandbox.TestASavesReservationStatesZeroAndItsOutcomeTheRaisedPoint`
  - `internal/sandbox.TestAStandbysTakeoverReservesAtThePointItsOwnTakeRaised`
  - `internal/sandbox.TestAClaudeCodeSaveOutsideTheResumeReadSetIsNeverRaised`
  - `internal/executordomain.TestAConversationWhoseWakeARunReadContinuesItsSession`
  - `internal/executordomain.TestAServedTurnTakesItsPlaceInTheConversationsLine`
  - `internal/executordomain.TestARowAnotherConversationReadsIsNotAbsorbedByItsOwn`
  - `internal/executordomain.TestAServedTurnsSessionIsCountedOnce`
  - `internal/executordomain.TestAnEndServingOtherConversationsIsScopedToItsSeat`
  - `internal/engine.TestATurnsEndNamesEveryOtherConversationWhoseWakeItsRunRead`
  - `internal/coord/coordtest`
  - `internal/coord/memory.TestFleetContract`
  - `internal/coord/kv.TestFleetContract`
  - `internal/sandbox/executor.TestEveryHarnessThatContinuesASessionStatesHowItsFirstRequestIsJudged`
  - `internal/sandbox/executor.TestAGeminiOrQwenSessionIsJudgedInItsBox`
  - `internal/agent/runner.TestAGeminiOrQwenContinuationIsJudgedAtItsFirstRequest`
  - `internal/sandbox/executor.TestAGeminiCountIsNeverBelowWhatItsResumeHands`
  - `internal/sandbox/executor.TestAGeminiSessionThatCompressedAtItsFirstRequestCountsPastItsPoint`
  - `internal/sandbox/executor.TestAGeminiSessionItsFirstRequestWouldMaskIsMarked`
  - `internal/sandbox/executor.TestAQwenCountIsItsNewestReplysUsageAndAllAfter`
  - `internal/sandbox/executor.TestAGeminiOrQwenPointIsMarkedWhereABoxSettingMovesIt`
  - `internal/sandbox/executor.TestTheChatPointIsTheThresholdOfItsWindow`
  - `internal/sandbox.TestAnUnanchoredPointIgnoresTheProvider`
  - `internal/sandbox/executor.TestARunStartIsKeptAsTheLinesItNamed`
  - `internal/sandbox/executor.TestAChangedPrefixLeavesTheCaptureNoStart`
  - `internal/sandbox/executor.TestACarriedSessionContinuesOnlyUnderTheEntryItRanUnder`
  - `internal/sandbox.TestALaunchContinuesASessionOnlyUnderTheEntryItRanUnder`
  - `internal/providers/llm/harness.TestAnEntrysIDMovesWithWhereItsCallsGoAndWithNothingElseOfItsEnv`
  - `internal/providers/llm/harness/harnessprofile.TestARouteIsItsCleanedAddressAndAValueThatIsNoRouteIsRefused`
  - `internal/providers/llm/harness/harnessprofile.TestAValueShapedLikeAKeyIsRefusedWithoutIt`
  - `internal/sandbox/executor.TestEveryEndpointARunnerDeclaresIsItsEntrysRoute`
  - `internal/sandbox/executor.TestOnlyAQwenPartAfterTheRunsStartCounts`
  - `internal/sandbox/executor.TestAQwenFileShorterThanItsStartIsNotRead`
  - `internal/sandbox/executor.TestAQwenPartBeforeTheNewestCompactionNeverCounts`
  - `internal/sandbox/executor.TestOnlyAPartQwenHandsItsModelVerbatimCounts`
  - `internal/sandbox/executor.TestAQwenToolNameAnotherServerCanSpellIsNeverHeld`
  - `internal/sandbox/executor.TestAQwenRecordRepeatingAUuidLeavesTheSessionUnread`
  - `internal/sandbox/executor.TestAQwenChainItsLoadWouldNotWalkIsNotRead`
  - `internal/sandbox/executor.TestAMarkedChangeEndsTheQwenReading`
  - `internal/sandbox/executor.TestEachQwenReleasesBridgedCallsReadAsItsFileHoldsThem`
  - `internal/sandbox/executor.TestEachGeminiReleasesBridgedCallsReadAsItsFileHoldsThem`
  - `internal/sandbox/executor.TestAGeminiReadingReadsOnlyReleasesItWasMeasuredOn`
  - `internal/sandbox/executor.TestAGeminiReadingsFloorIsItsOldestRelease`
  - `internal/sandbox/executor.TestAGeminiReadingsReleasesAreTheReleasesMeasured`
  - `internal/sandbox/executor.TestAGeminiAskIsValidatedBeforeAnyRead`
  - `internal/sandbox/executor.TestAGeminiAskPastTheLogsBoundsIsNeverRead`
  - `internal/sandbox/executor.TestAGeminiSessionIDIsACanonicalUUID`
  - `internal/sandbox/executor.TestOnlyAGeminiPartAfterTheRunsStartCounts`
  - `internal/sandbox/executor.TestAGeminiFileShorterThanItsStartIsNotRead`
  - `internal/sandbox/executor.TestAGeminiPartBeforeTheNewestCompactionNeverCounts`
  - `internal/sandbox/executor.TestAGeminiCompressionIsFoundByItsLinesNotItsWords`
  - `internal/sandbox/executor.TestAGeminiCompressionsFormsHoldOnlyItsOwnLines`
  - `internal/sandbox/executor.TestOnlyAPartGeminiHandsItsModelVerbatimCounts`
  - `internal/sandbox/executor.TestAGeminiCallItsCutLeftUnansweredIsTold`
  - `internal/sandbox/executor.TestAGeminiPartIsTheAnswerTheResumeHands`
  - `internal/sandbox/executor.TestAGeminiFailureIsNeverHeld`
  - `internal/sandbox/executor.TestAMaskedGeminiOutputIsNeverHeld`
  - `internal/sandbox/executor.TestARewoundGeminiResponseNeverCounts`
  - `internal/sandbox/executor.TestAMarkedChangeEndsTheGeminiReading`
  - `internal/sandbox/executor.TestALineSeparatorLeavesAGeminiSessionUnread`
  - `internal/sandbox/executor.TestATornLastGeminiLineIsReadAsTheResumeCutsIt`
  - `internal/sandbox/executor.TestAGeminiPartsArgumentsAreReadByTheLogsOwnRule`
  - `internal/sandbox/executor.TestAScrubbedTextNeverStandsForACutGeminiOutput`
  - `internal/sandbox/executor.TestAGeminiSessionAnotherServerCouldNameIsNotRead`
  - `internal/sandbox/executor.TestGeminiReadingIsHeldToItsBounds`
  - `internal/sandbox/executor.TestGeminiReadingStopsBetweenLinesAndParts`
  - `internal/sandbox/executor.TestGeminiReadingHoldsNoLine`
  - `internal/sandbox/executor.TestGeminiReadingsCauseQuotesNothing`
  - `internal/sandbox/executor.TestGeminiReadingTakesNoSlotOfItsOwn`
  - `internal/sandbox/executor.TestASessionRecordReadsUnshadowedOnlyAsWritten`
  - `internal/sandbox/executor.TestTheGeminiHelperFindsEveryServerThatSpellsTheBridgesTools`
  - `internal/sandbox/executor.TestAGeminiRunRecordsItsGuardBeforeItsHarness`
  - `internal/sandbox.TestACaptureKeepsWhetherItsRunWasUnshadowed`
  - `internal/agent/runner.TestAGeminiCountThatMayMaskAsksNoReading`
  - `internal/sandbox.TestARelaunchWaitsForTheCaptureOfTheJobItFollowsAsLongAsItsSizeNeeds`
  - `internal/sandbox.TestARelaunchWaitsAsLongAsTheLongestKeepItDependsOnNeeds`
  - `internal/sandbox.TestARelaunchsWaitForACaptureCountsWhatItsSizeStillNeeds`
  - `internal/sandbox.TestAnUploadTheStoreTookNoneOfIsMadeAgainWithinTheKeepsBudget`
  - `internal/sandbox.TestTheScrubWaitIsDerivedFromTheArchivesBound`
  - `internal/sandbox/executor.TestASessionNoBoxContinuesIsNeverPackedOrRead`
  - `internal/sandbox/executor.TestAnOpenCodeSaveOfAReleaseNoBoxContinuesIsRefusedUnpacked`
  - `internal/sandbox.TestAJobWhoseSessionNoBoxContinuesIsSavedNoMore`
  - `internal/executordomain.TestAnOpenSplicesEveryMediaWhereItsFrameStands`
  - `internal/executordomain.TestNoMediaOutlivesItsSession`
  - `internal/executordomain.TestTheSeatBudgetCountsSharedMediaOnce`
  - `internal/sandbox/executor.TestTheTrimPointsAreTheTakesBounds`
  - `internal/sandbox/executor.TestATrimmedCodexPackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestACodexPackWithNothingToTrimIsPackedWhole`
  - `internal/sandbox/executor.TestOnlyTheMeasuredCodexReleaseIsTrimmed`
  - `internal/sandbox/executor.TestACodexPackPastThePackedPointIsTrimmed`
  - `internal/sandbox/executor.TestTheTrimReadsOnlyWholeLines`
  - `internal/sandbox/executor.TestEveryCodexPackTrimsAtTheTakesBounds`
  - `internal/sandbox/executor.TestATakeReadsItsPacksTrimOnlyBesideItsOwnArchive`
  - `internal/sandbox/executor.TestATrimmedCodexTakeIsSaidSoAndStillBounded`
  - `internal/sandbox/executor.TestTheHarnessesNoCutFitsAreNoTrimmers`
  - `internal/sandbox/executor.TestATarInPlaceCutsOnlyPastAPointAndOnlyItsOwnRelease`
  - `internal/sandbox/executor.TestATarInPlaceThatCannotCopyItsRecordPacksItAsItsOwnTarDoes`
  - `internal/sandbox/executor.TestATrimmedCopilotPackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestACopilotPackWithNothingToTrimAtIsPackedWhole`
  - `internal/sandbox/executor.TestOnlyTheMeasuredCopilotReleaseIsTrimmed`
  - `internal/sandbox/executor.TestEveryCopilotPackTrimsAtTheTakesBounds`
  - `internal/sandbox/executor.TestATrimmedHermesDumpKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestAHermesTrimLeavesOutOnlyInactiveRowsBelowTheNewestSummary`
  - `internal/sandbox/executor.TestAHermesTrimSlimsOnlySettledResultsItReads`
  - `internal/sandbox/executor.TestAHermesDumpWithNothingInactiveIsNotTrimmed`
  - `internal/sandbox/executor.TestATrimmedHermesPackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestAHermesPackWithNothingToTrimIsPackedWhole`
  - `internal/sandbox/executor.TestOnlyTheMeasuredHermesReleaseIsTrimmed`
  - `internal/sandbox/executor.TestAHermesPackPastThePackedPointIsTrimmed`
  - `internal/sandbox/executor.TestEveryHermesPackTrimsAtTheTakesBounds`
  - `internal/sandbox/executor.TestATrimmedHermesTakeIsSaidSoAndStillBounded`
  - `internal/sandbox/executor.TestATrimmedMusePackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestAMusePackWithNothingToTrimAtIsPackedWhole`
  - `internal/sandbox/executor.TestAMuseCutNeedsItsCheckpointWhole`
  - `internal/sandbox/executor.TestOnlyTheMeasuredMuseReleaseIsTrimmed`
  - `internal/sandbox/executor.TestEveryMusePackTrimsAtTheTakesBounds`
  - `internal/sandbox/executor.TestATrimmedMuseSessionReadsFromItsNewestSummaryOn`
  - `internal/sandbox/executor.TestATrimmedGeminiPackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestATrimmedQwenPackKeepsWhatItsResumeLoads`
  - `internal/sandbox/executor.TestAChatPackIsNeverCutPastItsRunsStart`
  - `internal/sandbox/executor.TestAGeminiSyncLeftOpenIsNoBoundary`
  - `internal/sandbox/executor.TestAChatPackWithNothingToTrimAtIsPackedWhole`
  - `internal/sandbox/executor.TestOnlyTheMeasuredChatReleasesAreTrimmed`
  - `internal/sandbox/executor.TestEveryChatPackTrimsAtTheTakesBoundsFromItsRunsStart`
  - `internal/sandbox/executor.TestATrimmedChatTakeFindsItsRunStartInTheCutCopy`
  - `internal/sandbox/executor.TestALineFormReadsAlikeInGoAndInAwk`
  - `internal/sandbox/executor.TestThePlaintextBoundIsTooLarge`
  - `internal/sandbox.TestThePlaintextBoundIsTooLarge`
  - `internal/sandbox.TestACaptureKeptTrimmedSaysSoAndIsCountedOnce`
  - `internal/sandbox.TestALostSessionKeepsItsSummary`
  - `internal/sandbox.TestAStopsSummaryIsNamedByTheTurnsEnd`
  - `internal/sandbox.TestASummaryKeepNeverHoldsTheSessionsKeep`
  - `internal/sandbox.TestAParkedTurnsEndNamesAFreshCopyOfItsSummary`
  - `internal/sandbox.TestTheTakeReadsItsScrubsValuesOnce`
  - `internal/sandbox/executor.TestASummaryPastAnEntryIsReadFromItsEnd`
  - `internal/executordomain.TestASummaryGoesWithItsRow`
  - `internal/executordomain.TestAnEndNamesNoSummaryPastRecordWithin`
  - `internal/executordomain.TestAnEndNamesTheSummaryItsRowKeeps`
  - `internal/executorread.TestASessionNotKeptReadsAsItsSummary`
  - `internal/sandbox/executor.TestTheNewestSummaryIsTheMainThreadsLast`
  - `internal/sandbox.TestASummaryIsPlacedAtTheCaptureThatFirstNamedIt`
  - `internal/executorkeep.TestALaterPlaceIsALaterLaunchsOrALaterCaptureOfIt`
- **Measured:** a harness's session of a day's work compresses to one or two mebibytes, and one that read a whole 20 MiB PDF, Claude Code's limit, to 40.1 MiB as kept; bounded here at 48 MiB, so a session holding more media than that is not kept (crewlet/crewlet#1626); at the reference company's 3 000 turns a day the log that names the sessions grows about 32 MB a day, about a kibibyte of it each turn's first read of its conversation (its `begin`), while the sessions themselves would be gigabytes; crewlet-operator's smallest data volumes leave 448 MiB (12Gi with the objects in S3, and 16Gi beside the nats bucket) above the other logs at their floors, and this log, scaled to its 64 MiB floor there, still fits both — the minimum classes do not move, but the least a fresh node boots in rises by 64 MiB of logs (12348030975 bytes of broker on nats, up 85.3 MiB with the bucket's quarter beside it, and 64 MiB on S3), which is what the operator's own floors follow. At that floor the log holds about two days at the reference company's rate, under the seven-day `min_age` a healthy trim keeps, so a company above about thirty seats on those volumes sets `stream.executor_log_max_bytes` or a larger volume, and `log_ceiling_short` says so; a continued session's first request: Claude Code 2.1.310 and 2.1.322 compact before it at 167,000 tokens under a 200,000-token window (a count of 165,922 sent whole, one of 167,834 compacted, the harness's own count of it replicated to the token on both), and a resume adds at most 2,597 bytes of records beside its brief and its turn's attachments at most 2,232; the count's rule matched the harness's recorded count to 73 tokens at 186 real compactions, each just before its boundary, and the bound read at or above that replica at 376,937 cut points of real sessions; the take's meter walks a 118 MiB thread in 1.61 to 1.75 s beside its scrub's 10.7 to 10.8 s, and the collection's read of a main thread at its 256 MiB bound of ordinary lines takes 1.96 to 2.00 s (`BenchmarkTheMeterAtItsBounds`, `BenchmarkTheMainThreadReadAtItsBound`, on two dedicated cores), within the 15 s wait at twice its figure; a continued Claude Code session hands its model what a reading of its kept archive counts on, as probe T measured 2.1.310 and 2.1.322 continue one: 14 continuations a release, 25 whole-line results after the last restart among them and 4 of those off the chain parallel calls leave, every one carried once with its call, and a result the harness wrote itself only for a call its file held none of; five continuations each carrying a seat server's call and its result whatever its server's state; a task notification carried verbatim; every path behind a flag Anthropic's servers set read in both releases' shipped code, not run; the chain a resume hands its model, its leaf and what it recovers off it read in both releases' shipped code, and the rule the reading builds on it reading every one of the 402 of the probes' 432 main sessions that hold a call (no reading for none) and leaving not whole three of the 80 subagent threads it reads there: P2(d)'s torn thread in its two copies, whose line 25 is no record, and its box's copy cut back whole, a record of its own on a branch its own load does not hand (the strict rule); and a reading holds what its readers read of a line, never the line: a whole PDF's Read, 55,924,056 bytes on one line, is read in 26 to 35 ms holding 146.5 to 151.5 KiB (`BenchmarkAReaderAtALineOfAnyLength`), 255 MiB of ordinary lines, the plaintext's bound, in 3.1 to 3.2 s, four prompts of about 64 MiB to it in 1.09 to 1.11 s, the slowest archive measured, a subagent's lines of blocks to the plaintext's and the lines' bounds (2^20 lines, past which a reading fails closed), in 21.4 to 22.6 s, inside the reading's 30 s wait, a thread at both bounds of the model of the resume in 2.3 to 2.5 s, holding 43 MiB, a reading's worst, and an ordinary session in 72 to 88 ms (`BenchmarkAReadingAtItsBounds`, on a dedicated two-core build slot); a seat's saves of its running sessions hold at most its session budget again in the object store while they wait for the collector; a Codex 0.171.0 rollout cut to its first line and everything from its newest `compacted` line (17 lines, 36,975 bytes of 514,791) resumed from a fresh `CODEX_HOME` with the same request as the whole one, 8 input items and 38,769 bytes each with the same instructions and tools, only the new message's minted id differing; a Hermes Agent 0.23.3 dump cut to all but its inactive messages below its newest compaction summary, each settled admission's stored result slimmed (125,568 bytes of 900,353), resumed in a fresh home with the same requests as the whole one, over 13 request pairs in three cases, one that compacted again included, only a UTC time Hermes writes into a file's name differing; a Claude Code transcript gzips 10.4 to one, past the 5.33 of the plaintext and archive bounds, so text meets the plaintext bound first
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
which may be on another engine pod; a volume would need every engine pod to
mount every other's, and would be a second,
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
(`SessionTakeBudget`). The scrub's, `SessionScrubWait`, is six minutes, from
its measured worst: the slowest session a harness writes at the scrub's
256 MiB of plaintext (`scrubPlainMax`), 183 s, and the archive read twice out
of a box at a mebibyte a second, 96 s at 48 MiB, and the minute a take may
wait for its spool's room (`spoolRoomWait`), 339 s; lines that are no JSON, which
only a corrupted transcript holds, do not set it. A take past it FAILS CLOSED:
nothing is kept, its place is refused and the last kept copy stands, so a box
can only cost its own session. The KEEP — the upload at the object store's
pace and the record — runs after the read has answered, off its frame, on the coordinator's
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
shape) and the tar's metadata are scrubbed too. The scrub reads at most its
own bound of plaintext (`scrubPlainMax`, 256 MiB), refusing a session past it,
and holds no line, string or stretch of text whole, so it refuses none for its
length and what it holds stays within its own bound whatever the box wrote.

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
working read as ended, and its session taken final.

**Off every frame, and with the session closed first.** Every stop of a job that
started in a box is read in the ending's own retry, whatever
`conversation_session.enabled` says — the switch governs what is kept of a
session, not how a stop ends a job — and a loss closes the run's bridge session
before its read, as a stop does, so a lost job still working stops acting as the
seat before its take.

**The read's budget holds the end.** `BoxReadBudget` is per read: a stop's and
a loss's add `JobEndBudget` (the wait and the kill, 145 seconds) to a
collection's at the host's three-quarter share, about 193.3 seconds more, so a
host's take after an end at its bound is still given its whole
`SessionTakeBudget`. As `BoxReadBudget` (`internal/sandbox/boxhost.go`) computes
it, a collection of a turn with no cap waits about 5,380.1 seconds (a Claude
Code one 36 more, for its main thread's read, `MainThreadReadWait`, below) and
a stop's or a loss's about 5,573.5: the poll's 60-second bound, and at the
share the take's 480 seconds, its reservation's 30, its read of the run's row's
30 and the 240 that read's reply takes to cross at the slowest node's pace
(`coord.MaxValueReadCrossing`: a read sends no write), three 30-second graces
and the four changes of the run's row its host makes, each at the slowest
node's crossing (`coord.MaxValueCrossing`, about 780.03 seconds), so
60 + 3,990.1 / 0.75. The figures first given here, 460 and 653 seconds, were
from before those changes were counted at their crossings.
`internal/sandbox.TestABoxReadGivesEachChangeOfTheRowItsHostMakesItsCrossing`
holds the collection's terms and
`internal/sandbox.TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake` the end
on top of them. No wire changes: a host's reply carries `Result.JobEnd`
additively, an older engine never sends the request, and a script of an older
build is asked nothing and killed.

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
"releasing the head then loses nothing" true at the 49 MiB floor, where a hold
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
the sessions a job of the turn ran out in, each with how (`PendingRun.SpentSessions`),
written as the job is collected and before its turn resumes, which every decision a
relaunch asks reads: its kept capture (`ContinuityOf`), its frozen base
(`BaseContinuityOf`) and the box's own copy (`continueSession`, `inParkedBox`). The
turn's end carries them, with how, to this log (`Ending.SpentSessions`) as it
carries a capture that was not kept, which is record version 8, since a build
applying that end without reading them would leave its copy of the rows naming
sessions no other node's names.

**The log retires the session, not the row, and by its id.** The apply takes it off
every ended row of the conversation that names it, its own included: a turn that
continued its base and whose capture never landed leaves that base as the head
otherwise, and the next turn fails on it. Taking the object off a row is what every
reader already refuses, so no read learns a new rule. `retired_by` and
`retired_loss` keep the row's account truthful, where it would otherwise read as a
budget's release, and exact: the conversation's next turn says how the session was
spent as the end named it, never a loss guessed from that end's unkept capture.

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
naming no cause.

**The read's budget holds the end.** `BoxReadBudget` takes the run as well as
the read: every collection of a turn with a cap waits as a stop's and a loss's
read do, with the record that the cap ended a job it saw working on top, at
its grace and the slowest node's crossing (`recordsTheCap`), whether or not
the cap has passed on the asker's clock, since the host decides on its own as
it reads; a collection of a turn with none waits as it did. As the code
computes them, that is about 6,653.5 seconds for a collection of a turn with a
cap, 5,380.1 + (145 + 30 + 780.03) / 0.75, and about 5,380.1 for one with none
(the derivation is in the amendment above), and a Claude Code collection waits
36 seconds more for its main thread's read (`MainThreadReadWait`,
`internal/sandbox.TestTheBoxReadBudgetHoldsTheMainThreadRead`). A Claude Code
read whose run holds a hook's serve to its job (`asksReceipts`) waits for that
read, the verdict on the serve (`hookVerdictWait`, 10 s) and one more change of
the row at the slowest crossing: about 6,189.5 seconds for a collection of a
turn with no cap, 7,462.9 with one, and 6,382.8 for a stop's or a loss's
read. The figures
first given here, 653 and 460 seconds, were from before the changes of the
run's row were counted at their crossings and before the cap's record was
counted at all;
`internal/sandbox.TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake` holds
the record's terms. While builds are mixed, an asker of an older build still
waits an uncapped collection's budget on every collection, so a newer host's
take after an end at its bound is cut short of its budget there, and that
capture is stamped refused; this lasts only for the upgrade's window.

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
it (the amendment on a session that ran out of context) takes it from every
ended row that names it, a held base included, and a read of such a row then
says so; the seat's later turns are told how that turn ended.

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
wait out a slow keep (`Coordinator.relaunchWaitOf`): in the base the turn's first
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
a scale-down, a lost VM — on the twenty-one-minute wait. Letting the run's
loss verdict alone settle the stamp cannot tell a capture that landed from one
that did not, which only a reader of the log can. One taker lease per node rather than per process would
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
`claudeSessionGate`) — a session restored from an archive and the copy a box's
own last run left alike. A file with no whole line at all is left as it is: the
session's own transcript, being the session the script chose to resume because
it holds bytes, and a subagent's thread read as its subagent wrote its first
record, which 2.1.310 continues from nothing a restore could make of it —
measured against a scripted model, a fragment, an emptied file and a removed
one alike are answered that no transcript was found for the agent, the thread
written nothing and the run going on — so no record is written onto that
fragment. Emptying such a thread, so that a next record would be a whole line
of its own, was rejected on that measurement: no next record comes. A box
without `find` cuts no thread and says so on the run's error stream. Nothing on the log, the row or the archive changes: an archive holds
what it held, and the cut is the box's, made before its harness starts.

## Amendment: a capture whose harness did not end its own run is placed at the newest save point its launch holds

A capture was placed by one read of its round's log made before its take: its
save point is how many of the round's calls the log had taken then
(`SessionCapture.Through`), and a launch that continues it is told every call
after it. That holds only where the session holds every call the log had
taken, and a harness writes a call to its record only once the model's whole
message has streamed: Claude Code 2.1.310, measured against a scripted model,
makes a call and can have it answered before it writes the call to its
transcript — eleven of thirteen timed calls started with no line there, and
five quick ones were answered with neither their call nor their result
written — and OpenCode's export leaves out a step that has not settled. So no
count read off the log says what such a session holds: not while the job
works, and not once its harness was killed (past its end's grace, by its
memory limit, by a crash) while a message streamed, told to stop (2.1.310
records the call it was cut off in, measured only with that call's message
already written, never with one still streaming), ended before it started, or
lost with a read that cannot say how it went. A continuation from such a
capture, told only the calls after that count, was never told a call its
harness had made and had answered without writing it, and could make that call
again.

**Reserved at zero, raised after its take.** The take of a job still working
(one its box would not end, packed on demand),
every save and every capture of a job cut off are reserved at zero, the
round's start, and raised once the take's count states the session's newest
compaction (internal/sandbox's `placeAtNewest`), to the save point of a
capture the launch's session holds whose own newest compaction is that one,
both identities read and equal: the launch's own newest landed capture (the
row's kept capture only where it is that launch's, its job and its ordinal
both, since before the launch's first capture lands it is an earlier job's),
or the capture the launch records it continued (`LaunchRecord.Continued`).
The point is sound while a session only grows between two captures with one
newest compaction ("an OpenCode session is saved as it works, beside its own
server", below, gates it where that is read). A continuation is told, and a
reading asked of, only the calls after the point, a subagent's included: some
more than once, none never. The places an older build reads a save point by
(`bridge_seq`, `bridge_open`) stay zero, so a peer reading them is told every
call.

**What it costs.** A continuation of a capture cut off is told every call
after its point, those its session holds among them (every call of its
round where no raise applied), within the prior-work ledger's budgets, so its
brief is the longer for it, and the floor its launch is held to, and any
rewrite of a long call by the seat's auxiliary model, grow with it. Its skill
guard arms the session's loads through the point alone, so a skill it loaded
after the point is loaded again before a call it covers, its body never assumed
to have reached the session.

**Told as calls the session may already show.** Since such a capture can hold
calls after its point, a continuation of a capture cut off
(`runner.SessionBasis.Interrupted`) is never told that the calls after it are
not in the session: they are headed as calls the session may not show, a call
shown both there and in the session is said to be one call made once, and the
calls a long round's log dropped are counted since that point
(internal/agent/prompts' `CalledAfter`, `AfterSave.Interrupted`).

**Only a capture of a job whose harness ended its own run stays placed by its
log read** — over, gone cleanly by itself, its script packing it — that harness
having been answered every call it made and written each before it exited. The
read is made once the job is over, after its script packed the archive and
before the take. It is the one capture `readCapture` does not call cut off, the
same line the notes it holds are drawn by (`notesHeld`), and the only one that
reads the log at all: a capture cut off is placed off its row, so a log the
store cannot read refuses none of those, and a stamp of one refused before its
place was reserved states no save point, while one of a job whose harness ended
its own run states the count its own read saw; no launch continues either.
Nothing on the wire changes: the stamp carries the fields it carried, and a
capture an older build took in a fleet mid-upgrade is read at the point it
states.

## Amendment: a pack beside a working harness is held to what it listed, and a save is its taker's own

The end-job and cap amendments left the pack the engine makes on demand as the
one pack beside a harness that may still work — a job its box would not end —
and it took GNU tar's exit 1 wherever "changed as we read it" stood in its
error file, as the run's own end does. Beside a working Claude Code that keeps
a torn file: its harness rewrites task files, a subagent's metadata as the
subagent ends, and its own transcript, which a trim renames a new file over
and the withdrawal of a message already written rewrites IN PLACE, in the same
inode, and can grow back past its old size (2.1.310's shipped code, read). And
tar says too little of it: measured on GNU tar 1.35 and BusyBox 1.37.0, GNU
pads a member that shrank as it read it with NULs and busybox cuts its archive
inside one, both exit 1, and busybox exits 0 on a file that grew. So every pack
made beside a harness that may still work — a Claude Code pack on demand of a
job still working, which the take that asks for it says
(`SessionTake.Pack`), and a SAVE, below — is held to a rule of its own
(internal/sandbox/executor's `claudeHeldTar`), and every pack made once its
harness is gone keeps the rule it had: the run's own end, and a pack on demand
of a job that is over, which would otherwise wait on the box's clock with no
harness left to write, and refuse a file that a process the harness started,
outliving it, writes as it packs.

**What the pack listed, held after the tar.** The pack asks the box for the
tools it reads the session with (`heldTools`), naming one it lacks rather than
refusing on what the lack makes another check read. It sets every file of the
session stamped in the box's future back to the box's present (below), then
writes a marker and waits until the box's own `find` reads a file written after
it as newer — busybox's compares whole seconds, so up to a second there, and
the wait reads whatever grain the box's filesystem keeps rather than assume
one; a box whose `find` never does is refused after fifty sleeps of a tenth of
a second, five seconds, rather than packed blind. A box's `sleep` is required
to take a tenth of a second — every wait the engine's own scripts make sleeps a
fraction of a second at a time (a run's end, the kill of a job's group,
OpenCode's waits for its server), and the page's image requirements say so —
and a box whose `sleep` refuses one fails the pack, saying so, rather than run
its wait out at once. It then lists every file of the session, its inode and
its size, and refuses a session whose files add up past what one reading of an
archive yields a kept session's scrub (`scrubPlainMax`, 256 MiB) before
any tar, since the take would refuse it once packed. After the tar, whatever
its exit 0 or 1 said, it
refuses the pack where the archive does not list back through gunzip —
busybox's archive cut inside a member that shrank fails there as GNU's reading
of it does — or lists no file, a file is gone, its inode changed or it shrank,
a file it listed is not in the archive (one tar could not read, which busybox's
leaves out with exit 1), a member that is no `.jsonl` transcript was written
since the marker (find's `-newer`, held to files and to members), the
session's own transcript, or another `.jsonl` member written since then, is not
a byte prefix of the live file — the member read back out of the archive as a
stream, its `cksum` against the live file's first as many bytes, which is what
catches the in-place rewrite grown back and a shrink grown back within the
read — or, looked at last, a member of the
archive is gone, so a file removed while the checks ran, which no check of a
live file can read, is refused too. No check reads an empty file of patterns,
which busybox's grep and GNU's read in opposite ways. And none rests on a
tool's word that what it wrote landed: a tool need not check a write it makes,
and one that does not exits 0 having written nothing, or half, to a full disk
— the disk a harness still writing can fill under the pack. So every list and
every answer a check reads comes to the shell through a pipe, each `find`'s own
exit held rather than that of a pipeline's last command, and the shell writes
it to the pack's staging directory and reads it back, refusing the pack where
the file's `cksum` is not that of what was written; a list that cannot be made
or kept, and a comparison `grep` cannot make — its exit 2, or an exit 0 whose
answer names no line — refuse the pack as a difference would. Writing each
list straight from its tool, and taking the tool's exit, `grep`'s 0 or 1, or a
list's holding a line, for the proof that it was written, was rejected: it held
a pack whose checks compared nothing, its lists emptied by a disk filled under
a harness still writing. The refusal names its check and its file, and the
pack's error carries enough of that line to read both. A transcript or a
thread that only grew is taken as tar first saw
it, a whole prefix with at most its last line torn, which the resume cuts back
(above). Reading tar's words was rejected, since busybox says nothing; so was
keeping a torn task file by leaving out a `.json` that is not one value, which
needs a parser no box is known to have.

**The session's own transcript is read back on every pack, and a subagent's
thread only where it was written since the marker.** The decision that asked
for the byte-prefix proof named every `.jsonl` member; the pack proves the
session's own transcript always, whatever any of its lists says, so that proof
rests on no list, and of the rest those its box's `find` reads
as written since its marker, taking the others as they were when it began,
since a file unwritten since is the file tar read. Each proof decompresses the
whole archive once, and a long session holds a hundred subagents' threads, so
proving every one would run the pack past its bound (`SessionPackWait`) where
proving the transcript and the few threads that grew does not. The transcript
is never narrowed so, because it is the one `.jsonl` of the session Claude Code
rewrites in place — withdrawing a message rewrites the session's own file and
never a subagent's thread, which it only appends to (2.1.310's
`removeTranscriptMessage`, read in its shipped code) — and a clock that steps
back while a pack runs can stamp that rewrite before the marker, where a proof
made only of what `find` reads as written would keep the transcript torn
mid-file. The threads' narrowing rests on what WRITTEN already rests on: a
box's clock that does not step back while a pack runs, and one grain stamping
the marker (in the work directory) and the session's files (in the
configuration directory), both under the box's home. A clock that does step
back while a pack runs costs at most a subagent's thread appended to as it was
packed and missed by the check, kept with a partial last line — as a thread
that only grew always is — which the resume's cut of a torn `.jsonl` removes
(above); and, since WRITTEN rests on the same clock, a file that is no
transcript written as it was packed — rewritten in place (a task file, a
subagent's `.meta.json`) or still being written (a spilled tool result) — kept
torn or cut short.

**No file of a session is packed from its box's future.** A box cannot hold a
file from its own future, and such a pack reads one as written since its marker
until the box's clock passes it — refusing on a task file, reading back a thread
— whatever stamped it: a box whose clock ran ahead, its session carried here
with the times a restore keeps, or this box before its clock stepped back. So
every such pack, before its marker, writes a reference in its own staging
directory and sets every file and directory of the session that its box's `find`
reads as newer than the reference to the box's present (`touch -c`, a link left
alone, since `touch` would follow it out of the session), and leaves every
earlier time as it is, which costs the session nothing: the harness's own
retention sweep is held off (internal/sandbox/executor's `claudeHeldTar` and
`claudeRetentionDays`). Once set, such a file is read as every other is: as
written where the harness writes it after the marker, and whole where its last
write came before the marker, since tar reads it after. The time set is the
present, never an earlier one, so a file the walk sets reads as no older than
any write made before the walk set it, and no write ends up behind a marker: not
this pack's, written after the walk, nor that of another pack of the same
session running beside it, as two takers' saves, a save and the take of a job
still working, or two takes of one job at once may, each in a staging directory
of its own. What one pack's walk sets after another's marker reads as written to
that other, which costs it a pack and never keeps a torn file. A `find` that
compares whole seconds, as busybox's does, leaves a file stamped later than the
reference within the reference's own second, which that `find` never reads as
newer than the marker either; and a file the pack could not set stays ahead,
read as written. Setting the times at the resume instead was rejected: a clock
that steps back after the resume leaves every file stamped before the step ahead
of the clock, which every such pack would refuse, the last archive standing,
until the clock passed it. So was setting them to the reference's time: a walk
that ran late, its `find` held up on a busy box, would set a file the harness
rewrote under another pack's tar back behind that pack's marker, and that pack
would keep the file torn. So was holding a box's packs to one at a time with a
lock, which the present makes needless and which a pack killed while holding it
would leave behind. And so was comparing each file with its own time at the
manifest rather than with one marker, at a reference file per member made on
every such pack, where setting the times costs one walk of the session.

**What a pack beside a working harness lists is its own.** Since every pack
made on demand is staged in a directory named for it alone
(internal/sandbox/executor's `Paths.SessionPackStaging`), a pack beside a
working harness keeps its reference, its marker, its lists and its archive
there; and every save is staged in a directory named for that save alone,
after its taker (`Paths.SessionSaveStaging`), so two saves of one taker at once
are two packs as much as two takers' saves are. So no pack of a box reads
another's lists as its own or finds its own gone, or moves an archive whose
checks never ran. Holding a box's packs, or a taker's saves, to one at a time
was rejected again, for the lock a pack killed while holding it would leave
behind.

**A pack of a job still working is its read's own.** Such a pack, made on
demand, moves its archive once made to a place of its own beside its staging
directory (`Paths.SessionPackHeld`), where the read that made it streams it
from and removes it once done, never over `session-capture`. The job's own end
pack moves the job's final archive over the capture the moment its harness
exits, which can come while such a pack still checks what it read; moved there
after, the archive of the job as it stood replaced the final one, and a read of
the job once over took it as everything the job did — a collection that found
its harness had ended its own run placing it at the whole of the run's log,
though the calls after it were in no session and were never told. That was so
with the plain tar such a pack ran before this amendment, and its checks widened
the window. And two reads of a job still working each stream their own archive,
where over one capture a take that reads its archive twice was refused whenever
the other's move fell between its two passes. A pack of a job that is over
still moves its archive over the capture, as the run's own end does, since
nothing else is left to.

**A save is its taker's own archive.** A save of a session its job still works
in is packed by the same pack under the same rule (`Runner.PackSave`, Claude
Code's runner alone: an OpenCode session is reachable only through its run's
own server), packed in a staging directory of its own (above) and moved to a
path of its taker's own — the taker lease's holder, named by a digest — and
taken from there alone (`SessionTake.Save`), never packed by the take, so the
scrub's values are read once it is packed. Never to
`session-capture`, which every read of a job that is over takes as its final
archive: a save left there would be collected as the last archive of a job
whose own exit pack failed. A save that fails leaves its taker's last one
standing, and the clear before every run removes every save's files. What
takes a save, and how one is kept, this does not decide.

**A save's save point.** A save is placed as the amendment above places a
capture whose harness did not end its own run (internal/sandbox's
`placeAtNewest`). The capture its launch continued is the one the launch's row
records (`LaunchRecord.Continued`), never one the save is handed.

**What it costs, and no wire changes.** Each pack beside a working harness
waits up to a second for the box's clock, walks the session's files with `find`
a few times — once before its marker, to set a time in the box's future back to
its present — and decompresses its archive once to list it, once more for the
session's own transcript and once more for each other transcript written since
its marker, reading those back as a stream rather than onto the disk of a box
whose harness is still writing — within the minute `SessionPackWait` keeps for
an OpenCode server's start, which such a pack never makes; and the archive a
pack of a job still working keeps for its read costs one more call into the box
once the read is done, to remove it. Nothing on the log,
the row or the archive's format changes: a pack refused is a capture refused,
as a pack that failed always was.

## Amendment: the copy a job's own box holds is told every call of the round where its harness may have been cut off, and every session cut off every note the turn has read

A relaunch that carries a round on in the job's own box — a person's answer, or
a sign-in replaced while the job ran, where the relaunch takes up no kept
capture of the job (`LaunchRequest.Continue`): none was kept, its keep was still
under way when the relaunch's wait ran out, or was refused, or the company keeps
no session — continues the copy of the session that box holds, and its brief
told no call at all. That copy holds every call the job made and every note it
read only where its harness ended its own run. Where it was cut off — killed
past its end's grace, by its memory limit or by a crash, told to stop, gone with
any status but 0 — it can lack calls the harness made and had answered, since
Claude Code 2.1.310 makes a call, and can have it answered, before it writes the
call to its transcript. Nor is it known to hold the job's brief: no probe has
measured whether 2.1.310 writes the message a continued session is handed before
it answers a call (it was measured answering a fresh session's first two calls
before that session's transcript file existed, but a fresh session cut off then
leaves its box nothing to continue, and the relaunch starts a session of its
own), and a job that ended before its harness started leaves the session it was
handed, which never read it. That is the under-tell the amendment on a capture
whose harness did not end its own run fixed for a kept capture, on the path it
did not touch.

**How the job's harness ended is recorded on the job's own record.** The read
that collects a job writes `harness_end` on its `LaunchRecord` — `own_run`, or
`cut_off` by the line `readCapture` draws a capture's save point by — whether or
not it takes or keeps a capture, through `PendingStore.MarkHarnessEnd`, with no
fence, as `MarkEndedAtDeadline` takes none; once recorded cut off it is cut off
for the job, and nothing is recorded over a word this build does not know
(`HarnessEnd.after`). It is written after the take, within the room the read
leaves the take's own records (`recordsBy`): the grace they share and the
crossing of this change of the run's row on the host's connection
(`takeRecordsRoom`), which `BoxReadBudget` counts at the slowest node's
crossing as one of the four changes a read makes (`boxReadChanges`); when this
was first written `BoxReadBudget` held no time of its own for the record. So a
store slow to answer it costs neither the take nor the box's pause their time,
and a record that does not land is never read as an own run. The read's result
carries it back (`Result.HarnessEnd`) to the claim it
collects for, which was read before the record was written, for the resume that
follows on that claim. The job's record and no other: the fact is the job's, a
capture's stamp exists only where a capture was taken, and the record is
replaced whole by the next launch, after the relaunch that reads it has written
its briefs. A read made while the run's ending is decided — a stop's, a loss's
ending — records nothing, since nothing carries that round on; a lost job whose
round is carried on is read by a collection, which records it.

**What the copy is told, and what its guard holds.** Unless the record says
`own_run` (`runner.Resume.HarnessEnd`), and where the job's box is still there,
the continuation is told after what carries it on (`runner.inPlaceAfter`): that
it was cut off where the record says so (`prompts.InterruptedCopy`), and that it
may have been where the record says nothing or says it in a word this build does
not know (`prompts.MayBeInterruptedCopy`), in words that speak of no save,
whether or not any call follows; EVERY CALL OF THE ROUND, under the header of
calls the session may not show (`prompts.AfterSave.Interrupted`), with every
call the log dropped from its middle counted (`runner.calledInPlace`) — what a
cut-off capture's continuation is told from its save point, from the round's
start; and EVERY NOTE THE TURN HAS READ, under a header that counts them as
notes the session may already show, a note shown in both places being one note
(`prompts.NotesReadAgain`), before the notes waiting. Never the job's own calls
or notes alone: a copy that lacks the job's brief lacks what that brief told —
an earlier job's calls, the call a person approved, the notes the session it
continued lacked — whatever the job made or read itself, and no record says
which those were. Its skill guard holds the skills its launch armed and none of
the job's own loads, each loaded again before a call it covers. The skills a
launch armed are recorded on the job's parked state as the launch armed them,
before its run started (`launchDecided.skills`): read off the guard once the
launch returned, they took in a load the run's harness reported while the launch
was still returning, which is one of the job's own calls, and the guard armed it
where it arms none of them. A job whose harness ended its own run is told none
of it — only that a successful `submit_work` among the calls its copy holds does
not stand (`prompts.AfterSave.HeldSubmission`), the reads its job's brief
counted without listing them, which its copy holds only as that count (since
the amendment on a fresh run's brief, below), the calls the log took late,
which no copy is known to hold (the amendment after this one; and, since the
amendments on them below, the calls nobody waited for and the ones still in
flight as its round was read), and the notes waiting — and armed with its loads,
as before.

**A kept capture cut off is told every note the turn has read too.** A
continuation of a kept capture whose harness was cut off
(`runner.SessionBasis.Interrupted`) was told the notes from its job's last take
on, as notes it lacked: `notesHeld` stopped the capture's cursor before that
take, since a take is recorded read before its answer reaches the harness. Its
session is no more known to hold its job's brief than the copy in the job's box
is, and that brief told the notes the session it continued lacked, so the cursor
said it held notes that may never have reached it. It is now told every note the
turn has read, under the same header (`prompts.NotesReadAgain`), after the calls
it may not show and before the notes waiting (`runner.keptSession`), whatever
cursor the capture states; and a capture cut off states none (`notesHeld`), the
notes it is known to hold being none. A capture whose harness ended its own run
states every note the run's record says was read when it was packed, and its
continuation is told only those read after, as before. The notes after the ones
told again are said to be ones the turn had not read before the brief, never new
to the session: the record is all a brief goes by, and OpenCode's delivery of a
note is recorded read only once its plugin confirms it.

**What it does not reach.** A copy that lacks the job's brief, and a kept
capture cut off that lacks its own, lack the lead that brief opened with too:
the turn's task where the job continued the session its conversation's last
turn left, a review that sent the round back, an earlier answer. The engine
kept no copy of it to tell again until the amendment on a session's leads
(below), since which it is told them again.

**What it costs.** The told calls are held to the prior-work ledger's budgets,
built before the relaunch's last ask of the seat's budget, so a rewrite of a
long call by the seat's auxiliary model is charged before anything is launched;
none is built where the job's box is gone. The notes are told whole, as every
brief tells them. The brief is the longer for every call of the round and every
note the turn has read, for every copy not known to have ended its own run,
which includes every relaunch after a replaced sign-in whose refused harness
exited with a status other than 0, and every job a build before this one
collected; and a continuation of a kept capture cut off is the longer for every
note the turn has read. The floor its launch is held to counts that brief and
grows with it, beside the copy's context, the larger of its job's newest
capture's measure, kept or not, and what the read that collected the job found
the copy's newest request carried (the amendment on the copy's measure, below),
and beside the static floor where neither measured it (ADR-0066's amendment on
a launch's brief):
where it is past what the seat's capped windows or its model's window can
carry, or past what its window leaves a launch that already waited a turnover
for it, the launch does not continue the copy, continuing the turn's kept
capture instead where it offers one whose own floor fits (ADR-0066's
amendments), and otherwise starting a session of its own, which is told every
call of the round since the amendment on a fresh run's brief (below); and a
launch that offers both is held to the larger of their floors, since its box
takes up the copy only where it cannot restore the capture.

**Mid-upgrade.** The field is wire-additive (`omitempty`), and an older build
keeps it in `LaunchRecord.Extra` through every write it makes to the record. A
job an older build collected has no record, and its copy is told that it may
have been cut off, every call of the round and every note the turn has read:
told more, and never fewer calls than before, at the cost above. An older build
ignores the field and tells nothing in place, as before. A capture this build
takes cut off states no note cursor (`note_cursor` omitted), so an older build,
which tells a continuation the notes past the cursor, tells it every note the
turn has read, without the header. No state-log record and no
`coord.ProtocolVersion` change.

## Amendment: every read of a job streams the archive its own pack made

The amendment that held a pack beside a working harness to what it listed
kept one pack's archive off `session-capture`, the archive of a job still
working, and left a pack of a job that is over moving its archive there, as
the run's own end does. Every read streamed what that path held when it opened
it, and a Claude Code take reads its archive twice (internal/sandbox/executor's
`scrubTar`), opening it again by its path for its second pass. So of two reads
of a job that is over and left no archive — a stop's and a collection's — each
packing it, one whose passes the other's move fell between found a member
changed and was refused; and where the run's own end pack landed between them,
which a script outliving what its read saw of its job can still make, the same.

**No pack made on demand moves its archive over `session-capture`.** Every
such pack, of a job that is over as much as of one still working, moves its
archive to its own place beside its staging directory (`Paths.SessionPackHeld`)
and streams it from there for the read that made it, which removes it once it
is done (`Runner.PackCapture`). `session-capture` is the job's own script's
alone, moved into place once as its harness exits, so a read of a job that
left its archive streams one that nothing moves over while it reads it, and
what a collection describes there (`Result.Captured`) is what the script left —
never a pack on demand that a later collection would read as the script's own
end. Linking the capture to a path of each read's own before it streams was
rejected: it costs a call into the box on every read, and leaves the capture's
path shared with packs a later read takes for the script's. So was refusing a
take whose two passes read different archives, which is what happened, and
lost the session; and so was moving a read's archive over the capture once
the read was done, which no later read needs: a job taken once it had ended
is not taken again once that take has settled.

**And every save is its own archive.** The amendment that made a save its
taker's own moved every save of one taker over that taker's one path, and a
taker can save one box twice at once — the drain's save beside one the
waiter asked for — so a take of the one save was refused wherever the
other's move fell between its two passes. Each save now moves its archive to
a place of its own, named after its taker and the save (`Paths.SessionSave`),
and its take names that place (`SessionTake.Save`, the path `Runner.PackSave`
described) and streams it alone, removing it once done; a save that fails
leaves nothing. A taker's last save standing at one path served no take: a
save is taken by the read that made it, and none other reads it.

**The removal is held to the take's own time.** A pack's or a save's archive
is removed on its take's close path, which the read the take is part of waits
on, so the removal runs under the take's own deadline (`releaseContext`),
never under a fresh bound of its own from the moment it runs: under that, a
box that stopped answering once its archive was read held the read the two
minutes a pack may take past its take's time, into the room left for the
outcome stamp and the box's pause. Where the take has no time left, the
removal still gets five seconds (`packReleaseFloor`), and what it cannot
remove the clear before the next run does.

## Amendment: a read whose place another read of the job holds stands by for it

Two reads of one job at once ask for one place on the run's record, and the
store gives it to one of them. The other stood down: its reservation refused,
or the job's capture found begun by the first, it walked away and left the
session to the read holding the place. Where that read then failed — its scrub
refused its archive, its seal or its upload failed, its node stopped
mid-keep, its record went unanswered — neither kept the session, though the
read that stood down had taken it whole.

**It stands by instead** (internal/sandbox's `Coordinator.standBy`). It takes
the session all the same, records itself on that capture's stamp as its
STANDBY — its taker lease's holder and token and its take's instant
(`SessionCapture.StandbyAt`, written by `PendingStore.StandBy` only on a
capture of its own job, at its own place, that has not landed and has no
standby yet) — before it spools, spools the session sealed on its own node, and
its keep waits for that capture to come to something (`Coordinator.takeOver`).
Where it LANDS, the standby keeps nothing, and the session is uploaded once.
Where it does not — refused, its record unanswered and refused once asked
again (below), or begun past what a capture can take — the standby takes the
place after it, in a write the store makes only
while that capture still names it and has not landed (`PendingStore.TakeOver`),
and keeps its own session there. A standby that cannot keep what it took — its
spool failed, its node is stopping — withdraws (`PendingStore.StandDown`), and
every wait for the turn's captures — the end's, a relaunch's — waits on a
standby as on a capture still being kept (`captureInFlight`, for at most
`standbyBudget`) and withdraws one whose taker lease has lapsed
(`Coordinator.standDownGone`), as it settles a begun capture whose taker is
gone. The read holding the place stamps its outcome from what it holds, which
never knew of the standby, so the row's rule carries the standby across every
later stamp of the same take (`stampOnto`). One standby per place: a third read
of the same job at once stands down, as every read once did. A read of a job
whose final capture has settled still takes nothing (internal/sandbox's
`takeCapture`, held by `TestAJobTakenOnceItHadEndedIsNotTakenAgain`).

**Every read that finds its place taken stands by.** A loss's read passed the
box over wherever the job's final capture was on the row, one still being
kept among them (`keptWhole`), and the ending reclaimed the box unread beside
a keep that could still fail; it now passes over only a final capture that
has settled, and beside one still begun stands by as any read does. A read
whose reservation the store never answered, settling its place, found it
another read's and walked away (`abandonPlace`); it now goes on beside that
read as one refused outright does.

**Except a read of the job ended beside one taken as the job stood.** A stop's
read whose end could neither end its job nor poll it takes the session as it
stands, not final; where it holds the place, a read of the job ended — its
collection's — takes the place after it instead of standing by
(`Coordinator.besideAnother`), since its session is the more whole and,
standing by, it kept nothing where the other landed.

**An unresolved capture is asked again, not taken over.** A capture whose
record was sent and never answered has its upload made and may be the log's;
a takeover beside it uploaded the session twice and left that record asked no
more, the turn's end asking only its newest capture, so where the log then
refused the takeover, the end named the turn's last word unkept. The standby
asks such a record again under its own operation, as the turn's end does
(`Coordinator.settleCapture`), keeps nothing where the log holds it, and takes
the next place only where the log refuses it.

**Writes the store never answered are settled.** A standby the store wrote and
never answered stood on the row with nothing keeping it; a takeover so made
left the next place begun under a lease that still held, with no keep behind
it, and every wait held for it. So a read whose standing by went unanswered
spools the session all the same and its keep reads the row before it keeps
anything (`Coordinator.awaitStood`), and a standby whose takeover went
unanswered withdraws first and, where nothing is left to withdraw, keeps its
session under the place the row holds as its own (`Coordinator.tookOver`).

**A turn's end waits for a standby's takeover and its keep.** The end waited a
capture's own budget, which a standby taking over at the end of it and then
keeping its own session outlasts; it gave up, recorded the end, and the end's
tombstone refused the standby's record. It now waits up to `endCaptureWait`,
`standbyBudget` and a keep's budget, ending as soon as nothing is in flight.

Keeping both reads' captures, each in a place of its own, was rejected: it
uploads every overlapped session twice, and the turn's end waits on the newest
place alone, so a refused newest beside an older one still uploading let the
end tombstone the turn ahead of it, and a refused newest beside a landed older
capture of the same job read as the turn going on past its session. So was
reserving the place only once a read held its session spooled: it covers a
scrub or a seal that fails, and still leaves a refused upload, or a node
stopping mid-keep, to keep nothing. So was waiting for the place-holder's
outcome on the read's own frame: its keep runs off the frame for minutes, after
the box is paused or reclaimed. A build without the standby drops it with its
own stamp of the capture, and its end does not wait on it: mid-upgrade such a
session is left to the read holding the place, as before.

## Amendment: a pack reports its archive made only once its digest is recorded

Every pack of a box's session keeps its archive's digest in the box's own list
(`Paths.SessionDigests`), so the box never takes an archive of its own for
another box's and restores it over the copy that went on past it. The packs —
the run's own as its harness exits, and the engine's on demand — moved their
archive to where its read reads it, reported it made, and only then appended
its digest, a write nothing checked: on a full disk a pack's archive stood with
no digest of it in the box.

**The digest is recorded first, and read back** (internal/sandbox/executor's
`packDigest`): the archive's `cksum` is appended to the list and the line found
there again before the archive is moved, and a pack whose line is not there —
the append refused, or lost by a disk that told its writer it landed — fails as
a pack that could not make its archive does, moving nothing. A digest recorded
for an archive then not moved is never false, the box's copy holding that
archive's session either way. Keeping the archive with no digest and saying so
was rejected: the box would later take it for another box's. A box without
`cksum` or `grep`, which every box's userland has, packs nothing it cannot
record.

**And a restore is continued only once its digest is recorded.** The restores
of a carried session — Claude Code's files moved into place, OpenCode's
export imported — appended the archive's digest once done, unchecked, and the
run continued what it restored: a copy that went on past an archive the box
did not know it held, which that archive, carried back in, was restored over.
The line is now written and read back once the archive is restored
(`recordDigest`), and a run whose line is not there starts a session of its
own, saying why (`restoreDigestUnrecorded`), so the copy restored is never gone
on past without it. Recording the line before the restore was rejected: a
restore that failed after the box's own copy went, or an import that did not
happen, would leave the line naming a copy that does not hold the archive.

## Amendment: a call the log took after its job's relaunch is held by no save point

The bridge runs a call under the run's context, never the request's, so a call
can finish after its harness died, gave up on it or was replaced. Where a
relaunch had opened the round's log to the next job by then, the store dropped
the call's append as one of a job that is over — so that its spend never
landed on the next job's record — even where the relaunch carried the same
round on: a loss's, a person's answer's, a replaced sign-in's. A call the round
made, whose effect happened, was on no record of the round, and no
continuation, by save point or otherwise, could ever be told it.

**Listed, marked, and paid by nobody.** Every append names its job and the
round that job works in (`sandbox.BridgeAppend.Round`, as the launch names the
job, `OpenedJob.Round`, and a recovery's reopen names it off the same row,
`PendingRun.NamedJob`), and the store decides once, on the log the append
lands on (`sandbox.BridgeLog.take`): a call of the log's own job is listed and
its spend written, as before; a call of an earlier job of the log's round is
listed — its entry, written before it ran, updated — numbered as every call is,
and marked late (`sandbox.BridgeCall.Late`, `late` on the wire), its spend written
to no record, since the next job's resume would pay it as its own — answered
recorded beside `sandbox.ErrJobReplaced`, so the ledger asks no more for a
spend no record can take, and answers an earlier attempt's failed spend the
same whichever relaunch replaced the job; and a call of another round is
dropped, as before, the new round numbering its own calls. The mark is the
log's to give, as a call's number is, and never the call a person approved,
which the store appends for its own round. It is kept through every write of
the round, by an older build as a key it does not know, and by the log's
dropped middle, which gives up a late call only once no ordinary call is left
there and counts one it gives up (`BridgeLog.LateElided`), since a dropped
call keeps no field. In a fleet mid-upgrade an older build reads a listed late
call as its job's own; no release ships either build, so nothing reads it
otherwise.

**What it committed counts.** Its effect happened, so the turn's outcome counts
what it committed as it counts any call's: the bridge records on every call the
kinds of change it committed to the turn (`mcpbridge.Call.Committed`, which the
ledger hands the log as `sandbox.BridgeCall.Committed`, `committed` on the
wire), and the log takes a late call through the one append every call goes
through (`sandbox.BridgeLog.appendCall`), which folds its kinds into the
round's own union of them (`sandbox.BridgeLog.Committed`) before the log's
bound can drop the call. The round's collection settles the turn's outcome by
that union and the listed calls' own (`sandbox.BridgeLog.Changes`),
so a late call's kinds count whether the log still lists it or has dropped it
from its middle, which keeps no field of a dropped call; and so do those of a
call nobody waited for (the amendment on such a call), which is its job's own.

**Held by no save point.** No brief of the round had told a late call when the
log took it, and the session of the job that made it may not hold it. Yet a save
point is a prefix of the log: a later capture of the job carrying the round on,
placed by its own read, as the amendment on a capture whose harness did not end
its own run places one whose harness did, counts the late call among the calls
its session holds, and every continuation after it would skip the call. So a marked call is
held at no save point (internal/agent/runner's `heldAt`, the one test of what a
session holds): every continuation of the round built after it landed is told it
whatever its point, and again by every later one (but one of a session that took
it, as the amendment on handing a late call to the job working as it lands
says), in a section of its own under
words that hold whatever the session's end — it finished only after the run that
made it was replaced, the session may show it with no outcome or as interrupted,
or an earlier brief may have told it, and it is one call, made once, whose
outcome as told stands (internal/agent/prompts' `lateCallsHeader`) — the telling
counted in the continued launch's floor;
and how many late calls the log dropped from its middle is told under those
words too, whatever the point (`prompts.AfterSave.LateDropped`): a dropped call
keeps no mark and no number, and under the words of calls after a save the count
would say the session's own run made them after it was saved; one past the point
is counted among those as well, by its number, an over-count. No rebuilt skill
guard counts a skill a late call loaded, which is loaded again before a call the
skill covers.

**And told in the job's own box.** A person's answer or a replaced sign-in that
carries the round on in the job's own box, taking up no kept capture of it,
continues the copy of the session that box holds, which the amendment on that
copy tells what it may lack. That copy is told the late calls by the same
test: where the job's harness ended its own run, `heldAt` at no save point holds
every call its job made and every one its job's brief told — every call of the
round but the late ones, a fresh run's brief telling every one of them, and the
reads that brief counted without listing them, which it holds only as a count
(both since the amendment on a fresh run's brief, below), and, since the
amendment on a call nobody waited for, but one its harness gave up on, which it
holds only as timed out — so the copy
is told the late ones (and, since the amendments on a call nobody waited for and
on a collection's calls in flight, the ones its harness gave up on and the ones
still in flight as its round was read) alone, under their own words, with the
line on a late submission and how many of them the log
dropped from its middle, as every continuation counts them; where the harness
may have been cut off, the copy is told every call of the round, the late ones
apart. Its guard counts none of a late call's loads, and the telling is in the
brief the copy is continued with, which its launch's floor counts. So every
continuation of the round built after a marked call landed is told it, in a
kept session and in the job's own box alike.

**Never the round's submission.** The round's collection never takes a late
`submit_work` for the round's submission (`sinceAnswer`), and the log never
keeps one whole as the one the collection replays (`sandbox.MaxSubmissionArgsBytes`,
chosen as the collection chooses: of the submissions it could replay, the one
placed last, a tie to the one the log took last): the job carrying the round on
was never told of it and makes the round's submission itself. Every later
continuation is told it, as no submission of the work (`lateSubmission`), so its
run makes its own.

**And a call no brief read is marked by the opening.** A relaunch that carries
the round on builds its briefs from one read of the round's log, then readies
its launch — its probe of the kept session, a rewrite of a long call by the
seat's auxiliary model, the call a person approved — before its opening moves
the log on. A call of the job it replaces that lands in between was taken as
that job's own, told by no brief, and held by the save point of a later
capture placed by its own read: the same under-tell. So the read's count rides
the relaunch to its opening (`sandbox.ResumeRequest.BridgedRead`,
`runner.AgentRunRequest.BriefRead`, the row's transient `PendingRun.BriefRead`),
which marks late every call of the round the log took after it — but the call
a person approved, which the relaunch's lead tells — and counts late any of
them the middle dropped since (`sandbox.BridgeLog.lateAfter`). The count is
taken before the call a person approved that the row alone holds is added to
the read, since the log numbers that call only at its own append or the
opening, after any call that lands in between. A launch that knows no read
marks nothing.

A late call is copied into no executor-log record: the mark lives on the run's
log in the coordination store alone.

**What it does not reach.** A job running as a late call lands — the one
carrying the round on, or the one a relaunch was readying as it landed — is
handed it as it works only as the amendment on handing a late call to the job
working as it lands says (below); every continuation built after it lands
tells it, but one of a session that took it. And what a
late call committed counts in the turn's outcome only where the log takes the
call before its round's last collection reads the log, which waits for every
call of the round in flight up to the first of its own bound, the turn's cap
and its process gone (the amendment on a collection's calls in flight, below):
one whose update lands later is in no outcome.

## Amendment: the call a person approved is told once, by the answer that carries it on

A person's answer to a held call carries the round on, and the relaunch makes
the call they approved on the engine host as its last step before it launches
the next job (ADR-0077); the continuation it carries the round on with says what
the call returned (`runner.Asked.Continuation`). Where that relaunch is refused
after it made the call, by the floor of the session it continues or by every
member sitting out, the answer is attempted again, and the later attempt reads
the call in the round's log: appended once its result was recorded, or, where
that append never landed, added off the row (`sandbox.BridgeLog.withHeld`),
either way numbered after every call the log had taken, so after the save point
of every capture taken before the call was made (one placed after it counts it,
and lists it nowhere). Its lead tells the call from its mark; and the calls
after a kept session's save point, and every call of the round told to the copy
in the asking job's own box that may have been cut off, listed it again. One
call was told twice in one brief, the second time under the words of calls the
session is not known to hold.

**Told by the lead alone.** The runner's account of a call carries the held call
an approved one was made for (`runner.BridgedCall.HeldID`, off
`sandbox.BridgeCall.HeldID`), and the continuation of an answer to a held call
names that held call (`carried.tellsHeld`), whose approved call no list of the
round's calls in the brief it leads tells (`runner.calledAfter`, on a kept
session's path and the copy's alike). A call an earlier answer of the round
approved is told as any call is: no lead of this relaunch tells it, and a
session continued since may not hold it; unless the brief tells that answer's
lead again, to a session that may lack the brief it opened (the amendment on a
session's leads, below), when that lead tells it, and no list does
(`runner.withHeld`). A refused reply names no held call, and
its continuation tells none; nor do a loss's, a replaced sign-in's or a
send-back's.

**And told made where its result cannot be read.** The lead is now the call's
only telling, so where what it returned is recorded but cannot be opened on the
node carrying the run on — no key it was sealed under is there — the lead says
the call was made and whether the tool reported a failure, off the call's entry
recorded with its result (`runner.HeldPrior.Unread`, from internal/engine's
`heldCommit.prior`): what the round's log lists of it. It had said the node
making the call stopped before its result was recorded, which was not so, and
the line the list now leaves out was the only word that the call was made.

**What it costs.** Nothing. The brief is shorter by the call's line, and by the
whole section of calls after the save where that call was the only one listed —
as it is where the later attempt continues the capture the asking job left when
it parked, placed by its own read: that save point counts every call before
the approved one, the held call among them. A fresh run carrying a round on,
told every call of the round since the amendment on a fresh run's brief
(below), is not told that call among them either: its reseed's lead tells it.

**Mid-upgrade.** Nothing on the wire changes: the log already names the held
call an approved call was made for (`held_id`). A later attempt an older build
relaunches tells the call twice, as before. No state-log record and no
`coord.ProtocolVersion` change.

## Amendment: a call nobody waited for is held by no save point

The bridge runs a call under the run's context, never the request's, so a call
goes on after its harness stops waiting for it, which the bridge sees in three
ways. The harness cancels the request (`notifications/cancelled`), as Claude
Code and OpenCode do at their tool timeout, discarding a late answer, so its
model reads the call as timed out. The harness hangs up on the call's response,
which the bridge keeps going while the call runs
([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)'s amendment on a
call's answer), so the next progress finds it gone. Or the job's session on the
bridge ends with the call still running — the run settled, its next job's
opening replaced it, the seat left the node, or a recovery opened the job again
— and the end answers the call at once that it is still running (ADR-0056's
amendment on an opening's end), so its harness is handed that answer and never
the call's own. The call's effect happens all the same. A harness that gave up
on a call can still end its own run, since it waits for no answer it gave up on,
and its job can park on a question with the call still running. Landing before a
relaunch read the round's log, the call was listed as its job's own and
unmarked: the job's own copy in its box, where its harness ended its own run,
held it by the line the amendment on that copy draws, and a capture of that
session placed by its own read counted it at its save point — while the session
held only that the call timed out. No continuation was told it, and a run could
make it again.

**Marked by the bridge, as its tool returns.** The tool session hands the
dispatcher whether anybody can still read the answer
(`toolsession.Call.Abandoned`: the request's `Done`, or a progress that could
not be written on the call's response, and nothing else of the request, so a
call still never runs under the request); the bridge decides, once, under the
opening's lock, as the call's tool returns, whether the opening's end answered
the call already (an end that comes after leaves it to its own answer, and
waits for that answer), and records the call as one nobody waited for where the
end answered it or where the request says so, in the call's one record step
(`mcpbridge.Call.Abandoned`); the engine's ledger carries the mark to the
round's log
(`sandbox.BridgeCall.Abandoned`, `abandoned` on the wire), which keeps it as
handed through every write of the round, and the resume to the runner
(`runner.BridgedCall.Abandoned`).

**Held by no save point, as a late call is.** `heldAt` holds neither, so every
continuation of the round built after the call landed is told it whatever its
point — a kept session's, and the job's own copy in its box whatever its
harness's end, which where that harness ended its own run is told only such
calls, the late ones and the ones still in flight as its round was read — in
the late calls' section, whose words now say each of its calls is one the run
that made it stopped waiting for before it finished, having given up on it or
been cut off or replaced, or one whose outcome is unknown (the amendment on a
collection's calls in flight, below), so the session may show one with no
outcome, as still running (the answer an opening's end hands a call it does
not wait for), as interrupted or as failed (internal/agent/prompts'
`lateCallsHeader`). No rebuilt skill guard
counts a load such a call made, whose body never reached the session; and the
log's dropped middle keeps such a call while an ordinary one is left there and
counts one it gives up beside the late ones it drops (`BridgeLog.LateElided`,
`sandbox.BridgeCall.unheld`), since a dropped call keeps no mark. Unlike a late
call it is its job's own: its spend is written to the job's record, and a
successful `submit_work` among such calls can be the round's submission, though
a continuation told one is told, as of a late one, to make its own, which the
collection replays over it. What it committed counts in the turn's outcome as
a late call's does, whether the log lists it or has dropped it from its middle
(`sandbox.BridgeLog.Changes`), and on the same condition: only where the log
takes it before its round's last collection reads the log (below).

**What it costs.** Each such call is told to every continuation of its round
that no telling credits it to (the amendment on handing a late call to the job
working as it lands), in its brief and so in the continued launch's floor, and
again by every later one. The harness of a call still running as its opening ends is handed the
end's answer at once, before the end closes
the box's sessions on the bridge, and waits for no other (ADR-0056's amendment
on an opening's end): the session that made the call holds that answer, never
the call's output, so a continuation of the round is told the call's result
from the log alone. A call whose tool returns just before its opening ends is
answered with its own result, which the end waits for, and recorded unmarked:
no call is both handed its own answer and marked for its opening's end. The
worst call the log keeps carries the mark within the per-call share the other
fields are held to (`TestABridgedCallIsHeldToItsShare`).

**Mid-upgrade.** A build that does not know the mark carries it in `Extra`, and
holds the call at a save point as any call; no release ships either build, so
nothing reads it otherwise. No state-log record and no `coord.ProtocolVersion`
change.

**And handed to the job that gave it up, as it works,** by the amendment on
handing a late call to the job working as it lands (below), so its model need
not take its own timeout for the call's outcome and make it again.

**What it does not reach.** A harness that stops waiting for a call without
cancelling its request, while its job's session on the bridge is still open as
the call returns, where nothing the bridge writes finds the call's response
gone first: one that hangs up in the last 20 seconds before the tool returns,
after the last progress, or on a call whose request carries no progress token,
on which nothing is kept going; and one that gives up while it still reads the
response, which the bridge cannot tell from one still waiting. Such a call
lands unmarked, and a save point holds it as any call, though its session holds
it as timed out. Claude Code's own watchdogs on a call give up this way,
answering its model with an error and cancelling nothing. On 2.1.310, at the
settings the engine runs it with, its first-byte budget and its check on a
silent call no longer give up a running call, whose answer starts at once and
is kept going, and whose server's timeout in the box's settings is the bridge's
own bound, at which it cancels; its transport-loss check still can. Made every
30 seconds, that check gives up a call 90 seconds after its transport was
marked lost unless a progress has cleared the mark since (by reading its
bundle), and the mark is routine, made on every call in flight whenever it
loses the session's standing GET stream, which it does 360 seconds after
opening it (measured). A call whose response is gone is not among them: the
next progress finds the response gone, and the call is marked. What the check
gives up unmarked is a call whose progress a hop between the box and the node
holds back for 90 seconds while the response stays intact and the bridge's
writes succeed, and a call whose request carries no progress token, on which
nothing clears the mark (measured: a 540-second call with no token, given up at
449.9 seconds, 90 after its session's GET stream was lost, and recorded as any
call when its tool returned). And a harness that gives up on a call in the
moment between the tool's answer and that answer's delivery, while the bridge
records the call. And what such a call committed counts in the turn's outcome
only where the log takes it before its round's last collection reads the log,
as a late call's does: every collection waits for it, up to the first of its
own bound, the turn's cap and its process gone (the amendment on a collection's
calls in flight, below), past which it is read as a call whose outcome is
unknown. One whose update lands once its run's ending has deleted the run's
record is on no record of the run at all, though its effect happened, which the
ending's delete and the engine's log each say at warning, naming the call
(`sandbox_run_deleted_with_calls_in_flight`,
`bridge_abandoned_call_after_its_job`). And a call still running when the
process running it is killed never makes its update: a node's graceful stop
waits for it once it has given its seats back, and of a process killed first
only the call's entry outlives it, the call on the record by its identity and
its outcome unknown (the amendment on a call's identity, below).

## Amendment: a bridged call's identity is on its run's record before it runs

A call the bridge ran was recorded on its run's log only once it returned, and
a call whose record the coordination store refused was owed: carried by its
session's next call, by the carry its run's collection asked for, and by the
one its session made as it ended. A store that refused all of them, from the
call until its job's session had ended, left the call on no record of the run,
though it ran. A resumed segment seeds its turn's call log by identity
(`internal/agent/runner`'s `seedCalls`), so a later call that asked for
something else derived the operation id that call had used, and a tool keyed
on it took the new call for a retry of the old one, or the other way round.

**Every call's entry is written before it runs.** The bridge writes each call's
ENTRY on the run's log ahead of it — its identity alone: its tool, its place,
the instant the bridge took it, the digest it was admitted under, the process
running it and the instant past which nobody waits for it
(`sandbox.BridgeLog.InFlight`, `in_flight` on the wire) — and runs the call
only once the log has taken the entry (`mcpbridge.Ledger.Begin`, the engine's
`bridgeLedger.Begin`, `sandbox.AppendBegin`). The call's one update as it
returns takes the entry out and lists the finished call in its place, in the
same write (`sandbox.AppendRecord`), finding it by its place, its instant and
its tool, never its digest: so the log never holds two records of one call,
and every mark a call carries — late, which the log gives, and nobody waited
for, which the bridge gives — rides that one update. `AppendBridgeCall` stays
the one writer of every step and of a call's spend, which only a record of the
log's own job writes. A digest is on an entry only where the call's tool can
never ask a person first (`tools.Surface.AsksFirst`): a held call's arguments
ride sealed (ADR-0077), and the entry is written before anything knows the call
will be held. A call a person approved never passes the bridge: its resume
makes it, its identity on the row before it is made and its append to the log
keyed by the held call's id, through the person's answer and a delegate's
decision alike, so it is told once as before; its place is now after any call
still in flight.

**A call whose entry cannot be written is not run.** Its box is told, failed,
"This call was not run: it could not be recorded. Call it again if it is still
needed." — the shape of the refusal of a call that reached a closing session,
nothing having run, so making it again is safe — where the store refused every
attempt within five seconds (the engine's `beginBudget`), or the log holds as
many calls in flight as it keeps (256, `sandbox.MaxCallsInFlight`, measured
into the log's byte budget); and that its run has ended where the log takes no
more calls of its job. A call whose entry was written and that is then not run
— its opening ended while the entry was written, which answers it as not run,
failed, or the seat's fence found closed after it — has the entry withdrawn in
the same step (`sandbox.AppendWithdraw`), so nothing waits on a call nobody
runs.

**A call is counted only once its entry is taken.** The run's call log, which
every operation id a write derives counts its repeats from
(`turnctx.CallLog`), admits a call (`tools.Surface.Admit`) as the bridge decides
to run it, its entry taken, never as it arrives (`mcpbridge.binding.writeAhead`):
a later segment seeds its call log from the run's record alone, and a call
admitted as it arrived and then refused at its entry, or withdrawn, raised the
next call's count in this segment and in no record — so a resumed segment
derived, for a new call, the operation id a call of this one had already used,
which is the defect this amendment closes reopened. Calls made side by side are
still admitted in the order they arrived: each is decided only once the call
that arrived before it has been (`mcpbridge.bridgedCall.admitAfter`), whichever
entry's write came back first. A call the log admitted that its tool then could
not run at all is recorded as not run, failed, by the identity the log counted,
never withdrawn.

**An update the store refuses is asked for again until the call's bound.** Its
entry stays meanwhile: the call is on the record by its identity, counted by
every later segment of the run — by its digest, or as a call of its tool whose
arguments are not known where its entry carries none. The ledger's own attempts
come before the call's answer goes back; past them the ledger answers
`mcpbridge.ErrUnrecorded`, and the call's own goroutine on the bridge, which the
bridge's registry holds, asks for the same update again once the answer is out,
every one to five seconds, until it lands, the log no longer takes the call, or
the call's own bound passes (`mcpbridge.binding.recordOwed`) — so a submission
or a work item whose update met an outage lands as soon as the store takes it,
and a collection waiting on the call ends on it rather than at its bound. Only
an update still refused at the call's bound loses what the call answered, cost
and committed: its entry stands, read by every reader as a call whose outcome is
unknown (`runner.Resume.Unfinished`, `ledger.Call.Unfinished`), failed and
answered so: its effects may stand, and are to be checked before it is made
again. Nothing is carried by another write: the session's owed calls, the carry
its run's collection asked for and the one its end made are gone, and with them
the lines that named a call on no record (`bridge_call_owed`,
`bridge_owed_calls_unrecorded`, `bridge_owed_calls_after_their_job`); the bridge
names an update it asks for again (`mcp_bridge_call_update_owed`) and one it
gave up on at the call's bound (`mcp_bridge_call_update_unrecorded`).

**Numbered as it finishes, counted as it begins.** A call is numbered among its
round's appends (`sandbox.BridgeCall.Appended`) by its update, as every call
was, so a save point keeps its meaning: no session holds the outcome of a call
in flight. But the log's last place counts it (`sandbox.BridgeLog.LastSeq`), so
a node that takes the seat over while the call runs on the node it left numbers
its next call after it, and every segment that seeds the turn's call log counts
it, as the segment that admitted it did: no operation id is derived twice.

**What it costs.** One coordination-store write before every bridged call runs,
measured at 11.89 ms at the median and 17.04 ms at the 99th percentile over a
thousand writes, on an embedded broker of three members, from a member
that does not lead the runs' bucket, with the log at its full count of calls
(`internal/coord/kv`'s `BenchmarkABoxRequestOnItsRun`, its `begin` arm); the
update after it is the write a call made before. A call made beside others waits,
to be admitted, for the entries of the calls that arrived before it, each
written at the same time as its own. And a store in an outage now refuses the
calls it let run off the record before, and is asked for each refused update
about once every seven seconds until it is back or the call's bound passes.

**Mid-upgrade.** The entries, and a call's runner and bound, are additive keys:
a build that does not know them carries them in `Extra` through every write it
makes of the log (`sandbox.BridgeLog.Extra`, `sandbox.BridgeCall.Extra`), and
reads the log as it did before — its own calls appended after they ran, its
seed blind to a call in flight, which is the case this amendment closes, never
a worse one. No executor-domain record copies a bridged call, so no record is
stamped for it, and no `coord.ProtocolVersion` change.

## Amendment: every collection waits for its round's calls in flight, on the run's shared record

A turn's outcome counts the kinds of change its run's bridged calls committed,
read off the run's log as the run is collected (`sandbox.BridgeLog.Changes`,
folded by the engine's `describeResume`), and its reviewer, its tool log and the
account an ending gives of whether the turn replied read the same log. A call
still running as its run was collected — its harness gave up on it and ended
its own run, or its job's session on the bridge ended around it, which waits for
no call (ADR-0056's amendment on an opening's end) — made its update after that
read, or found no record once the run's ending had deleted it: what it committed
was in no outcome, and where it made the turn's only change the turn was booked
as one that changed nothing. A person's stop, the turn's cap and a reclaim each
missed such a call once an opening's end stopped waiting for its calls.

**Every collection ends the job's session, waits, and only then reads.** A
completion's collection, a park's on its question, the turn's cap cutting a
job, a lost job's carried on (`sandbox.Coordinator.collectClaimed`,
`carryOnLost`), and an ending's before it decides how the turn ended on the
log — a stop's, a loss's, a reclaim's (`finishEnding`) — each ends the job's
session on the bridge to new calls (`CoordinatorOptions.Ended`), waits until
the run's log holds no call of the round in flight
(`sandbox.Coordinator.drainCalls`), and only then reads. A send-back's new round
and an answer's resume read nothing their round's collection did not wait for.
A park's collection now ends the asking job's session too: the answer's
relaunch opens its own, under a job of its own, so no call of the asking box
lands as the new job's.

**On the run's shared record, so any node may collect.** The wait reads the
run's log, where every call's entry is from before it ran (the amendment on a
call's identity, above), through a watch of the log's own key
(`coord.SandboxRuns.WatchSandboxRunPart`, `sandbox.PendingStore.WatchLog`), read
again from the store's leader whenever it is watched again, and never through
the bridge's registry of the calls it runs (`mcpbridge.Bridge.AwaitCalls`),
which is the memory of the node running them: the collection after a seat is
handed on is the seat's next holder's, often another node. A poll would read
the run's largest value whole at every pass for as long as a call's bound.

**What ends the wait.** For each call in flight, the first of: its update; the
turn's wall-clock cap (`sandbox.PendingRun.Deadline`), where the turn has one;
the call's own bound (`sandbox.BridgeCall.Until`: when the bridge took it, its
tool's bound and the bridge's margin), since most turns have no cap and an entry
whose update the store refused would otherwise hold a collection for ever; and
its runner gone — the process running it (`sandbox.BridgeCall.Runner`) holds its
taker lease at no epoch, read in the coordination store as a capture's taker is
(`sandbox.Coordinator.runnerGone`) as the runner first appears and every two
seconds after, never decided by a clock — so a crashed node's call is given up
within a lease's lifetime rather than at its bound, and a draining node, which
keeps its lease until its calls are recorded, is waited for.

**A call still in flight when the wait stops is unknown, never a failure read
plainly.** It is named at warning (`sandbox_calls_unfinished`, why the wait
stopped), and the read that follows reads it as a call whose outcome is not
known, with no third state on the turn's record: listed in the round's record,
failed so no delivery check, citation or replay takes it for something done,
under words that say its effects may stand and are to be checked before it is
made again (`runner.UnfinishedNote`, `ledger.Call.Unfinished`); counted by its
identity; told to every run carrying the round on beside its late calls, the
job's own copy in its box whatever its harness's end among them, in their
section, whose words never say its calls finished and except from the failed
calls that did not take effect one whose line says its outcome is unknown,
which may have and is checked before it is made again (internal/agent/prompts'
`lateCallsHeader`), since where they said its calls finished after their run
stopped waiting for them and that a failed one did not take effect, the section
contradicted the call's own line, and a run that trusted it made the call
again; read in the round's record, by its reviewer and by every later round,
under rules for a failed call that except it alike, in the same terms, the
executor's adding that it is checked before it is made again
(internal/agent/prompts' `ReviewHeader`, `PriorWorkHeader`, and
`ContinuedRoundRules` for a send-back continuing the session), since where they
said every failed call did not take effect, and the executor's to retry it, a
reviewer would send the round back to make it again and a run would make it;
and beside
it, an ending's account and a resumed turn's end read the turn's delivery as
unknown. Its update, landing later while the record stands, replaces the unknown
for every later reader, the bridge asking for it until the call's bound. The
ending's delete names every call still in flight as it removes the record
(`sandbox_run_deleted_with_calls_in_flight`), whose update then finds no record
(`bridge_abandoned_call_after_its_job`).

**An ending that may not wait hands the wait on.** Reached on a frame somebody
waits on — a completion's handler, a seat's acquisition — an ending whose log
holds a call a collection would still wait for is handed to this node's retry of
the ending, as every other step that waits on what is not the frame's own is
(`sandbox.Coordinator.drainEnding`, `errAskAway`); one whose log the store
cannot read is decided as before, the engine told why the log was unread. A
seat's retirement ends every run of the seat in one loop and then gives the seat
up, this node's retry of an ending with it, so it leaves such a run, its session
ended, to its own next tick (`sandbox.Coordinator.RetireSeat`): waited for in
the loop, one run's call held every later run's ending behind it, and handed to
that retry, the run was ended by nothing.

**What it costs.** A collection holds the run's seat while it waits, as the
resumed turn does after it, up to the first of those bounds, so a turn whose run
left a call running resumes only once that call is recorded or given up on; one
watch of the run's log, and one lease read per running node as it first
appears and every two seconds after.

**Mid-upgrade.** A build that does not know the entries writes none and reads
none: its collection waits for no call, as before; and a collection of this
build waits on nothing of a call an older build's bridge ran, which wrote no
entry. No state-log record and no `coord.ProtocolVersion` change.

## Amendment: a bridged call carries the id its harness gave it

A continued Claude Code session is told the calls of its round it may lack by
its save point: every call the run's log took after it, since nothing on the
log said which of them the session already held. Yet a harness names each call
it makes: Claude Code hands the server the model's `tool_use` id in the
request's `_meta` (`claudecode/toolUseId`), the id its session's transcript
files the call's use and its result under, on its main thread and in its
subagents, over the bridge's own transport, a tool its tool search loaded
included (measured on 2.1.310). So the run's log now names each call as its
harness's session does: what a continuation can later be told exactly the
calls it lacks by.

**The bridge records it on the call's entry and on its one update.** It reads
the id as the call arrives, under the key the runner that starts the box
declares and no other (`sandbox.CallIdentifier`; the executor's runner answers
it from its harness's built-in profile, `requests.call_id_meta`: Claude Code's
key, and none for OpenCode, whose `_meta` carries no id of a call). The key
travels on the job (`sandbox.OpenedJob.CallIDMeta`), named by the launch from
the runner its job is started on and by a node that serves the job again after
taking its seat over from its own runner (`internal/engine`'s
`reopenAgentRun`), and the engine's `openBridge` hands it the session
(`mcpbridge.Session.CallIDMeta`). The id rides the entry written ahead of the
call — which carried the call's identity alone until now ("a bridged call's
identity is on its run's record before it runs", above) and carries the id
beside that identity, as no part of it — and the one update that lists the
call (`sandbox.BridgeCall.CallID`, `call_id` on the wire), on every path that
records a call: one that ran, one refused before its tool ran, one its surface
could not run, one held for a person, one whose arguments did not parse, one
made once the run's tools had closed and one whose run's tools closed while its
entry was being written. An entry withdrawn takes its id with it.

**Whole or not at all, and never the call's identity.** The id is the box's
word. Only one the log keeps whole is taken (`sandbox.ValidCallID`: at most
`sandbox.MaxBridgeCallIDBytes` of letters, digits, `-` and `_`), and the log
holds every writer to the same rule again as it keeps a call or an entry; any
other value is recorded as none, never cut, since two ids cut alike would name
two calls as one. The update still finds its entry by the call's place, its
instant and its tool, and the log knows a listed call by those and its digest:
a harness that makes a call again may send the id it sent the first time, so
two calls under one id are two calls, and nothing is deduplicated by it. It is
for one decision alone, which of a round's calls a continuation is told, and
both sides of that match are the box's own.

**None where no harness named the call.** The call a person approved carries
none (`sandbox.approvedCall`): its resume made it on the engine host, and the
box's session holds the held call's answer under the held call's own id. A
skill load a harness's hook reports is the bridge's own call of the loader and
carries none either, and so does every call of a harness whose runner
declares no key.

**What it costs.** The id's bytes in each call's share of the log and in each
entry's, measured at its bound by the shares' own tests
(`TestABridgedCallIsHeldToItsShare`, `TestACallInFlightIsHeldToItsShare`)
inside the shares every call and every entry already had. No write is added.

**Mid-upgrade.** The id is an additive key: a build that does not know it
carries it in `Extra` through every write it makes of the log, on listed calls
and entries alike, records none of its own and reads its calls as before, by
their place. A later build that widened the id's rule would have its longer
ids dropped by this build's writes of the log, each then read as a call with
none, the direction that costs a continuation only what this build's calls cost
it before. No executor-domain record copies a bridged call, so no record is
stamped for it, and no `coord.ProtocolVersion` change.

## Amendment: a fresh run carrying a round on is told every call of it, and a session holds what its briefs told

A run that starts a session of its own carrying a round on, after a loss, a
person's answer or a replaced sign-in whose job's session the launch continues
neither from a kept capture nor in the job's box, was handed the round's own
brief and what became of the run before it (`Lost.Reseed`, `Asked.Reseed`,
`Rotated.Reseed`), and none of the round's calls: a run that remembers nothing
of the round, told only that the run before it did things through the seat's
tools, could make them again, a second post or a second delegation. So was the
session its conversation's last turn left, continued by a relaunch of a turn
that had kept none of its own and handed that brief whole; and so was the copy
of such a run's session its box holds once its harness ended its own run, which
the one test of what a session holds (`runner.heldAt`) read at no save point as
holding every call of the round but the late ones, every call its brief told,
when its brief told none. And every brief that told a round's calls under the
prior-work ledger's read cap counted the reads past it without listing them,
while the next own-run copy, and the next capture placed by its own read,
counted those reads as held: they were told to no continuation.

**A fresh run is told every call of the round.** Every relaunch carrying a round
on builds the fresh run's telling, `runner.calledAfter` from the round's start
as a copy cut off is told it (`runner.calledInPlace`), within the prior-work
ledger's budgets; and its brief is the round's own brief, then the turn's
earlier answers (the next amendment's), then what became of the run before it
(`prompts.KeyCarriedOn`), then the round's calls in words of their own
(`prompts.CalledBefore`, `prompts.KeyRoundSoFar`): calls the runs before it
made, in the order they made them, a successful `submit_work` among
them said not to stand, and how many the log dropped from its middle; and,
apart, the late calls and the ones whose outcome is unknown, under the late
calls' words for a run with no session above, and how many such the log
dropped; the log's tally once, after the last of those counts. Never the call a
person approved that the reseed's lead tells. Each is a section of its own, so
the frame the phase publishes for a box that starts a session of its own keeps
its outline. It is built before the relaunch's last ask of the seat's budget,
whatever else the relaunch offers, since its box may fall back to the fresh
brief, so its rewrites are charged before anything is launched; and the base's
continuation takes the reseed whole, the calls with it. And every form of
`Asked.Reseed` now ends its account of the run before it as `Lost.Reseed` and
`Rotated.Reseed` do: what that run did through the seat's tools did happen, to
be checked before any of it is done again. The list cannot say that alone: it
cannot tell what the run did outside those tools, nor a call it counts past the
read cap or only in the tally.

**A session holds what its briefs told, never what their budgets left out.**
Each brief that tells a round's calls answers the reads its read cap counted
without listing them, read off the render's own walk (`ledger.ShownCallGroups`,
`runner.leftOut`) and never off a second rule, by their places in the round's
log. The launch carries the set of each brief it may hand its run
(`sandbox.BriefTold`: the copy's on `Continuation.Told`, the kept capture's and
the base's on `SessionContinuation.Told`, the fresh run's on
`Continuation.Fresh`), and the attach that names the job's command records, only
where the launch carries a round on, the set of the brief it handed
(`LaunchRecord.BriefOmitted`, `brief_omitted`) and, beside a brief that
continues a session, the fresh one's (`LaunchRecord.FreshOmitted`,
`fresh_omitted`). A reader takes the set of the brief the job's run read
(`LaunchRecord.OmittedCalls`): by the brief, then by the session the box said it
started, and both where the box never said, or where the record names a word
this build does not know: told again rather than never. The reads left out are
recorded rather than the calls told: a told list alone would not say what the
copy holds without what the session it continued held, whose record the relaunch
replaced, while the reads left out are what every reader needs, self-contained
per job and at most a log's calls. A capture placed by its own read states its
job's set (`SessionCapture.Omitted`, `bridge_omitted`), and one placed at an
earlier capture's point copies that capture's with the point. The one test of
what a session holds reads such a read as held at no point (`runner.heldAt`), so
the next brief to the copy in the job's box (`runner.Resume.Owed`), and to the
capture (`runner.SessionBasis.Owed`), tells it, ahead of every other call it
tells, under words that say an earlier message counted it
(`prompts.AfterSave.Owed`), with how many of them the log has since dropped from
its middle. Every count of dropped calls a brief gives says the log's tally is
below it, so the tally is said once, after the last of them
(`prompts.tallyBesideLast`): said beside the first, as a continuation's brief
said it where the late calls dropped were counted after the calls dropped after
its save, a later count sent the run looking below it for a line above it. The
one read cap is spent on the late calls first and on the owed reads last, and
each brief records the reads it leaves out in turn: a copy owes exactly the
reads its newest brief left out, each told by the next brief with room for it,
and a brief that tells the round from its start, a fresh run's or a copy cut
off's, starts the account afresh. No guard counts a load differently:
an owed read came before its brief's read, so it is never the job's own.

**What it costs.** The fresh telling is built for every relaunch that carries a
round on, held to the ledger's budgets, and a rewrite another brief of the same
relaunch already made is not made again, the seat's rewrites being kept by their
text for the turn. The fresh brief is the longer for every call of the round,
and the launch's floor counts it (ADR-0066's amendment on a launch's brief). A
set of reads left out is at most `sandbox.MaxBridgeCalls` places, refused past
it at the launch and at a capture's stamp, and the row's measured figures
(`sandbox.measuredOtherFields`) carry the two sets on the job's record and the
one on each capture at that bound.

**Mid-upgrade.** The fields are wire-additive (`omitempty`): an older build
carries `brief_omitted` and `fresh_omitted` in `LaunchRecord.Extra`, and
`bridge_omitted` in `SessionCapture.Extra`, through every write it makes but a
relaunch's. An older build's relaunch tells as before, and its capture states no
set, which this build reads as every call at or below its point held, as before.
A record an older build wrote names no set, read the same way. No state-log
record and no `coord.ProtocolVersion` change: the sets live on the run's row
alone, as a capture's save point does, and no executor-log record copies them.

**What it does not reach.** A send-back opens a new round, whose log restarts,
so its record names no read left out: the reads a send-back's brief counts of
the collected round past the read cap are counted once and never told again one
by one, as the prior-work ledger counts a closed round's reads past its cap in
every later brief.

## Amendment: a session not known to hold its brief is told again the leads that brief told

A session is known to hold the brief its job was handed only where its harness
ended its own run. The copy a job's own box holds whose harness was cut off, or
never said how it ended, and a kept capture cut off, may lack it, and so lack the
message that brief opened with: the answer that carried the job on, a review
that sent its round back, or, for a job that continued the session its
conversation's last turn left, where the turn began and the turn's task. Such a
session was told every call and every note again, and never that message: the
reset that opens the next job clears the answer, its question and the held call
in the write that names the job, the approved call's entry keeps no output, and
nothing said which message a job's brief opened with. The gap chains, across
rounds through a send-back's continuation of a capture, back to the newest job of
the lineage known to hold its brief. And a session cut off before its harness
wrote its brief, continued in place on a replaced sign-in, was told only to
continue from where it is: the copy of a job that continued the conversation's
last turn holds that turn and none of this one's task, and carried the earlier
turn on.

**The run keeps the turn's leads beside its record** (`coord.RunLeads`,
`sandbox.PendingStore.Leads`), oldest first, in a value of their own, as the
bridged log is: on the record, half a mebibyte of them would ride every read of
the row. A lead is sealed once, by the launch whose brief first told it, for that
job (`sandbox.LeadSeal`, ADR-0031's amendment on a lead), and the attach that
names a job's command takes the leads its brief opened with of its own
(`sandbox.BriefTold.Own`) into that value BEFORE its write of the record, once
each, numbered as taken; that write marks on the job's record the leads its
brief told, its own and the earlier ones it told again
(`sandbox.BriefTold.Told`; `sandbox.LaunchRecord.LeadMarks`, `lead_marks`), the
picked brief's alone; a turn's first launch and a send-back that start a session
of their own take nothing, a fresh brief being held by its session. The reset
that opens the next job clears its marks with the rest of its record, and the
leads stay: they outlive every relaunch as the record does, and no launch
carries them, so `(*agentLauncher).launch` is unchanged, and a lead a later
build wrote keys into survives. The record keeps only what a reader needs
besides the marks: the run its leads are of (`leads_run`), so a value an
earlier run of the turn left beside the record is none of this one's, and how
many leads that value has taken (`leads_appended`), so a value that is gone,
short or unreadable is said, never read as none. An answer
keeps its question, its words (cut to `sandbox.MaxLeadWordsBytes`, 16 KiB, the
cut counted), who answered and who wrote them, and of a held call what became of
it and what it returned as the run's row sealed it (`sandbox.HeldKept`, opened by
`sandbox.CallSeal.OpenKept` under the very binding its seal was made with). A
review and a task are the engine's words over records the turn keeps, so a lead
of either keeps only which: the round a review closed, and the last of the
closed rounds a task's brief rendered, by its number, and the review it carried
(`sandbox.LeadTask`). A number and never how many entries the brief rendered,
since a park past its share folds whole closed rounds into one span of them
(`ledger.Iteration.Through`): once a later park has folded rounds the brief
rendered, a count of its entries reaches past them into rounds closed after it
(`runner.roundsThrough`).

**A session that may lack its job's brief is told them again.** The resume reads
the run's leads beside its record inside its frame, beside the log
(`sandbox.Coordinator.leadsAt`), and opens them in order
(`internal/engine.runnerResume`), those the job's record marks told
(`sandbox.LaunchRecord.ToldLeads`: none where the job's brief was
fresh or its box started a session of its own) as told; one that does not open
here is handed on as one that cannot be read, never dropped. Where the copy in
the job's box may lack its brief (`runner.Runner.copyMayLackBrief`), and where a
kept capture may lack its own (`runner.SessionBasis.MayLackBrief`), its
continuation tells again the leads that brief told, after the section that says
it was cut off and before its calls, under a header that says the session may
already show them (`prompts.LeadsToldAgain`): one message wherever shown, the
later of two that differ standing, with how many earlier ones are not kept, and
never why the session may not show them, which the section before says in the
words its record bears out, so a summary whose harness ended its own run is
never told its run may have been cut off. And
it records them as told in turn, so a continuation of its own session, which may
lack its brief too, tells them again. Where the store cannot give the leads back
to the resume at all, the resume is not refused for it: every lead the record
counts is said as not kept, and the ones the job's record marks are recorded as
told in turn all the same, by their numbers (`sandbox.TurnLeads.Unread`,
`runner.Resume.LeadsUnread`, `sandbox.BriefTold.ToldUnread`), so a continuation
of that session that can read them tells them, and one failed read drops no lead
from the chain. The one predicate per path is what every telling that depends
on it reads (the calls, the notes and the leads), so a later change to when a
session may lack its brief widens all three at once.

**In words true of any session of the work, never in the form first told.** A
lead told again may reach a copy of another job of the lineage, or the session
its conversation's last turn left that was handed the answer in a fresh run's
words: told "you stopped to ask", or to check a branch out on a fresh machine,
or that it is in a new box, it is told what its session never did, or a box it
is not in. So an answer is one "a run of this work asked" and its answerer
answered, a chat reply fenced under who wrote it and an answer by turn defused
under the relaunching segment's token (ADR-0040); a held call is one
"a run of this work called", with what it returned, and the call a person
approved for it is in no list of the round's calls (`runner.withHeld`); a review
is recomposed from the notes of the round it closed, and said in its place as a
review that is not kept where a park has since folded that round into a span,
which keeps none of its rounds' reviews (`runner.reviewOf`,
`prompts.ReviewNotKept`): never told as a send-back that said nothing, as one
that cannot be read here, or among the earlier messages not kept, which are
said before every lead told, since it can follow one, as it follows the turn's
task a base continuation kept, and the later of two that differ stands; and
the turn's task keeps its boundary in the past tense, and none of the new box,
checkout or paths its first brief spoke of (`prompts.NextTurnAgain`). What it
says of the session the conversation's last turn left (that its files from
before the turn were the earlier box's, where it ends, that it was cut off when
it was kept) it says only of a session that began as that one, never that the
session above did; and a review is a send-back of this turn's work, its rules
said of every call the turn's earlier rounds made whether or not the session
above shows it, never "the same session" (`prompts.ContinuedRound.Again`). A
summary is told every lead the run keeps, another lineage's task or review
among them, and marks them told in turn, so no record says which session a
lead's brief went to: told in its first brief's words, a session that began
with a fresh brief would read its own opening as another turn's. The task is
told with only the closed rounds its brief rendered, so a round closed after it
is told by its own rule. The one exception is a span a park has since folded
rounds the brief rendered into, which runs on past them: told whole, since it
holds those rounds' calls, which the session is told no other way, it counts
by tool the calls of a round closed after them too (`runner.roundsThrough`).

**A run that starts a session of its own is told the turn's answers.** It holds
none of the briefs before it: a fresh run carrying a round on, a send-back that
starts one, and the session its conversation's last turn left, handed either
brief whole. Its brief told it of the answers a person gave the work only the one
carrying its round on now, and the prior-work ledger carries none of a person's
replies, which are never the reviewer's evidence: an answer that carried an
earlier job on, in that round or in one a review sent back, an instruction to
the turn rather than to one round, was lost at the first fresh start. So such a
brief tells every answer lead the run keeps beside its record
(`runner.Runner.turnsAnswers`), after the turn's own brief and before what
carries its round on now, oldest first, each naming the round it came in, in
the words a lead is told again in, under a header that says the session holds
none of them and the later of two that differ stands, with how many earlier
leads are not kept to tell, past the bound or because the store could not give
them back, said as not kept and never as no longer kept, which a read that
failed does not make true (`prompts.EarlierAnswers`, `prompts.AnswerRound`,
`prompts.KeyAnswersBefore`). The call a person approved that one of them tells
is in no list of the round's calls, and one approved in a round the brief lists
as work already done, which a list keeps no held call's id to leave out by, is
pointed to among that round's calls (`prompts.HeldCallListed`), never told
twice, but only where that list says how it ended: where the round is kept
whole, lists a call of the tool with its arguments sealed, and nothing of its
record could count the call instead (`runner.heldIn`). One approved in a round
a park folded into a span, whose calls a park folded into its tally, as every
park that makes a span does first, whose record dropped calls of the tool or
of no named tool from its middle, or of which the tool is a read the list's
read cap may leave out, is told with what became of it, as one of the calls
that round or span may count rather than list (`prompts.HeldCallCounted`).
A base continuation's task lead, told again to a session that may lack
that brief, carries those answers after the task, in that brief's order
(`runner.Runner.taskAgain`): every answer lead whose job came before the task's
own, the only ones the run kept when the brief was built, and told there all
the same where that brief's read of them failed. They are never told apart
where the task is told, though a summary's brief, which tells every lead the
run keeps, marks them told: the run keeps a turn's leads in the order their
jobs launched, every earlier answer before the task, so told again apart, the
task, read as the later message, would stand over the answers its own brief
told after it, and one answer and the outcome of the call it approved were
told twice in one section, and again by every later continuation the mark
reached. Where a summary is told two tasks of the turn, each answer is told
once, under the oldest task that carries it (`runner.tasksTold`,
`runner.answersBetween`). A task whose lead does not open on the node is said
as a message that cannot be read there, which carries nothing, so the answers
before it are told on their own (`runner.toldAsTask`, one test for what counts
as carrying and what is told as the task); and a summary's brief marks an
answer it told inside a task told all the same, so a later continuation whose
task does not open on its node tells that answer on its own, never nowhere.

**What it costs.** A brief to a session that may lack its job's brief is the
longer for the leads that brief told, and a fresh brief for the turn's answers,
each held to a lead's bounds; telling a task again renders the prior-work
ledger of the rounds its brief rendered, which can spend the seat's auxiliary
model, so it is built before the relaunch's last ask of the seat's budget, as
the calls are, and the launch's floor counts the brief. The run's leads are held
to `sandbox.MaxKeptLeadBytes` (512 KiB, one lead at its worst and many ordinary
ones) and `sandbox.MaxKeptLeads` (2,048), the oldest dropped first and counted,
in a value of their own measured against the value bound with its own fields at
their worst; the record holds each job's marks, at most that many numbers, which
its measured spare counts at that bound.

**Mid-upgrade.** An older build neither reads nor writes the leads' value: its
delete of the record leaves it, which a node of this build's marker sweep
removes, and a record of the turn created again reads it as another run's, none
of its own. `lead_marks` rides `LaunchRecord.Extra` through every older build's
write but its relaunch's, and `leads_run` and `leads_appended` ride the record's
top-level `Extra` through every write, a kept lead's own unknown keys its
`Extra`; an older build tells no lead, and its relaunch marks none, so a chain
crossing it restarts there: told less, as before. No state-log record and no
`coord.ProtocolVersion` change.

**What it does not reach.** A lead the run no longer keeps, past the bound, or
one its kept value cannot give back, is counted and said, never told; a lead
under a key the node building the brief does not hold is said as one that cannot
be read here until a node that holds the key builds a brief; and the notes are
never among the leads, each note being told by its own rule.

## Amendment: the copy a job's box holds is measured as the job ends

A relaunch that continues the copy a job's own box holds sends that session's
whole context again before it reads a word of its brief, so its floor is that
context with the brief. The only measure of it was a capture's, of the job's
newest capture, kept or not: a company that keeps no session takes none, a
job's newest capture can be missing or unmeasured, and the copy was then held
to the static floor with its brief beside it, whatever it carried, bounded only
by its harness's own compaction.

**The read that collects a job measures the copy as the job ends**, off the
run's own account of its requests, and records the figure on the job's record
beside how its harness ended (`sandbox.LaunchRecord.Context`,
`session_context`), in that read's one write (`sandbox.PendingStore.MarkHarnessEnd`,
handed `sandbox.EndRead`), which lands where the end or the measure changes,
with no read of the box of its own (`sandbox.Result.LastRequest`). Claude Code's
is its event stream's newest main-thread request: an assistant line whose
`parent_tool_use_id` is present and null, its usage read by the rule the thread's
reader reads a reply's by (`internal/sandbox/executor`'s `readUsage`), its
uncached input, cache reads and cache writes; a main-thread usage that does not
read, and a line past the stream's bound or cut short (a harness killed while it
wrote its last line), clear the figure, so no earlier request's is ever taken
for a later one, and a line with no such key is no main thread's. OpenCode's is its session's own message list, which the run's script
asks of the run's own server once the session is exported, never on an end
request: one assistant message a page, newest first, the next page by the
cursor the page before answered, read as text anchored at the answer's end, and
only past a page that held a failed message, at most six pages and one MiB in
all, a page past what is left said by its size and ending the asking; the first
settled message that measured a prompt is the figure, by the one rule an export
is read by (`measuredPrompt`), the run's own where its stream named that step.
Never off the pack's export, which a company that keeps no session never makes.

**A job that made no request of its own carries the figure along its copy's
lineage, and says where it came from** (`sandbox.MeasureSource`): the launch
writes, with its job's command, the measure of the session it handed its run
(`sandbox.BoxRef.Context`): the replaced job's record of its own copy, read by
the reset in the write that replaces that record (`sandbox.PriorBox.Context`),
where it continues that copy in place; a kept capture's own measure of its
archive, naming the capture's job as the archive's and never as that job's
request; the frozen base's; and none for a session of its own. The collection
then records the job's own request (`request`); an earlier request of the
session it took up, the carried measure where its figure is the same and the
job's box did not say it started a session of its own, and else the session's
(`session`); and none, the carried measure, unless the job's box said it
started a session of its own, whose figure the carried one never was.
No reader takes a carried figure for a fresh measure of the job. The resume
hands the record's figure to the runner (`runner.Resume.ContextTokens`), and a
relaunch that continues the copy in place is held to the larger of it and the
job's newest capture's measure (`runner.Continuation.ContextTokens`): the copy is
the capture, or grew past it.

**What it costs.** The pages of an OpenCode job's end add at most one MiB to the
stream its collection reads back, inside the poll's share of the read's budget
(`sandbox.BoxReadBudget`), a page past it said by its size alone; a job that
ended cleanly asks one page, and one whose last step its provider refused two.
The record carries the figure and where it came from, which the worst row
counts at its widest.

**Mid-upgrade.** `session_context` is additive, carried in `LaunchRecord.Extra`
by an older build's writes, and its own unknown keys in the measure's `Extra`;
an older relaunch ignores it. An older host's reply carries no `LastRequest`,
which leaves the record the carried figure, or none; an older collector ignores
it, and its claim's resume misses the figure. An older engine skips the new
stream lines' unknown type; a newer one finds none in an older script's stream.
No state-log record, no `RecordVersion` and no `coord.ProtocolVersion` change.

**What it does not reach.** A Claude Code release that drops
`parent_tool_use_id` leaves every job unmeasured, which the collection says at
warning (`harness_context_unmeasured`); an OpenCode session whose newest step
that measured a prompt carries an image past the pages' budget, or sits past six
pages, is unmeasured, said at info; and either way the carried figure stands.
The figure is the newest request's input side: what the session added after it,
the reply and the tool results, is not in it, nor in any capture's measure.

## Amendment: a continuation its harness may compact at its first request is told as a cut-off one

A relaunch that continues a session tells it what it lacks and counts the rest as
held: of the copy a job's box holds whose harness ended its own run, every call
of the round but the late ones, every note it read and its brief; of a kept
capture, the calls at or below its save point. Claude Code may not hand its model
that session. Before every request, a resumed run's first among them, it
compares its own count of the conversation with the window it holds the run to
less its reply reserve (`min(maxOutputTokens, 20000)`) and a 13,000-token buffer,
and where the count reaches that point it compacts first, keeping a summary of
everything before its last reply (read in 2.1.310's and 2.1.322's shipped code;
replicated to the token on both: 167,000 under a 200,000-token window). Its count
is its last counted reply's input, cache writes, cache reads and output, placed
at the first record of that reply's message id, an estimate of every record
after that place, and the tokens the server cleared since the last boundary.
The floor continues a session wherever its last request's input and the brief
fit the window less 20,000 tokens, so a continuation in between reached its
model as a summary, and what the engine counted as held could be gone.

**The count.** The engine bounds the harness's count from above, by the harness's
own anchor rule run forward over the main thread's whole lines through the line
layer (`internal/sandbox/executor`'s `resumedReading`): the anchor's four counts
as the layer reads a reply's usage, and every chained line after its place at two
bytes a token, its break included (the executor's `claudeCharge`), at most every rate
the harness charges a block. What those bytes cannot bound is a mark, kept apart
from the figure and naming why (`sandbox.CountMark`, in precedence order): the
thread not read whole (`unread`), a release older than 2.1.310 or none named
(`release`, `claudeCountFloor`, never below the settings pin's floor), a saved
precompact summary under either name Claude Code reads one by (`precompact`), a
compaction with no counted reply after it, whose preserved segment sits before
its boundary while the harness carries it (`compacted`), a reply's usage the
layer cannot read as the harness counts it (`usage`), a server clearing since the
last boundary (`cleared`), a PDF read after the anchor (`pdf`), and an image or a
document after it (`media`). Every mark, every absence of a count and a mark a
later build writes read as a session that may compact.

**Where it is made, and never carried.** At both places a copy is read whole:
the take's context meter, now one walk of the kept archive's main thread
through the layer by two readings, the floor's figure (the last main-thread
reply's usage, read by the layer's one rule for a reply's usage) and the count
(`sandbox.SessionCapture.ResumedCount`, `sandbox.TakenSession.Count`);
and ONE read of the job's main thread in the read that collects a job, on the
box's host, before the take, within `sandbox.MainThreadReadWait` (27 s), which
`sandbox.BoxReadBudget` holds for a Claude Code or an OpenCode collection, 36 s
more for its asker than a read that asks no thread (about 5,416 s for an
uncapped turn's, about 6,690 s with a cap), a look at each saved compaction's
size and never a read of
its body, written with how the harness ended in the one write
(`sandbox.EndRead.Count`, `sandbox.LaunchRecord.ResumedCount`). A job not
over is counted unread with no count read, since its harness may still write; a
stop's read and a loss's ask no count, only their served notes' receipts. The walk is the seam every later reading of the
thread joins (`walkThread`, `threadReading`), never a second read.
The count is this job's copy's, or this archive's: the next launch's record
starts with none. Beside it, the least `contextWindow` the run's result lines
reported (`sandbox.Result.ReportedWindow`, `ResumedCount.Window`), which only
lowers the window a continuation is judged under.

**The predicate, and the one place it is decided.**
`sandbox.ExecutorWindow.SendsWhole` is the harness's own threshold: the count's
bound, the brief at `claudeCharge` and what a resume attaches
(`claudeTurnAttachments`, 20,000) strictly below the window the harness holds the
run to less 33,000. That window is the standard one (`ExecutorWindow.Standard`,
per the Claude Code runner's `claudeRoute` (internal/sandbox/executor): claude-opus-4-6 and claude-sonnet-4-6 at 200,000 unless
the account is served more or the argument opts in with `[1m]`, an alias at the
window of the model it names on the route, no model and `default` at 200,000),
the least over the chain's Claude Code members, or the window the run reported
where that is less. The runner decides one value per path after the path's
tellings and its lead exist (`internal/agent/runner`'s `firstRequest`,
`sessionHolds`), the kept path on the worse of its capture's count and its job's
copy's where its harness ended its own run: a continuation that may compact, or
whose job's main thread restarted after its brief, is told as a cut-off one,
every call of its round from its start, every lead its record keeps and every
note the turn has read, as messages it may already show, under words that never
say it was cut off where nothing says so (`prompts.MayCompactSession`); and its
guard arms no skill, each loaded again before a call it covers.

**A restart in the job, by identity.** The count names the uuid of the newest
line the layer's restart rule flags (`ResumedCount.Restart`), and each launch
records its session's newest restart as its job's basis
(`LaunchRecord.BasisRestart`: the job before's collection's read of the copy, a
kept capture's own, none for a session of the run's own, not known for the
session a conversation's last turn left). A job whose newest restart is not its
basis, or cannot be read, restarted after its brief (`sandbox.RestartedSince`),
and a resume hands its model only what that restart left. Never by count: the
harness's trim after a compaction keeps only the newest restart. The identity is
blind only where the thread was not read whole (`unread`, a newest restart whose
uuid does not read among its causes, and the box's read that found a saved
compaction before it opened the thread, marked `precompact` there) or not read
by the rules of its release (`release`): there the count names no restart, and a
launch handed that session records its basis as not known. Every other mark is
about the bound and leaves the identity known: `compacted`, `usage`, `cleared`,
`pdf`, `media` and a kept archive's `precompact` say what a whole thread's bytes
cannot bound, never where it restarts, and the restart rule ran over every line
of the thread, so the count names its newest restart and a launch handed that
session records it as its basis. Read as no basis, a marked count would make the
next job read as restarted for any restart its thread already held, and every
continuation after one image read in a thread that ever restarted would be told
as a summary.

**What it does not reach.** The harness's skip of a reply whose first block is a
placeholder text; a usage's iterations whose counted entry agrees with it; a
request whose real size exceeds the harness's count by more than its last 33,000
tokens; a backend that refuses the long window for a `[1m]` argument; a chain
member other than the one whose run reported its window, whose model the box's
environment, a setup step or a settings file retargets, or whose box's release
does not know its model; managed settings; served flags; a kept capture restored
into a box whose release is below the count's floor, written by one at or above
it; a changed box's skill listing past `claudeTurnAttachments`; and a later
release. OpenCode's own compaction, which has this defect as widely, is judged
in the continuation's box (the amendment that judges an OpenCode continuation's
first request there, below). Over-telling,
stated: the bound charges a prompt snapshot the harness counts as nothing, and
reads a session as may compact where the harness declines to compact one with
fewer than two replies since its last boundary; a first-request compaction also
sets the restart that tells the next continuation as cut off.

**Mid-upgrade.** `resumed_count` and `basis_restart` are additive, carried in
`LaunchRecord.Extra` and `SessionCapture.Extra` by an older build through every
write but a relaunch's. An older host's reply leaves `Result.ResumedCount` nil
and `ReportedWindow` 0; an older collector's claim misses the count. This build
reads every absence as may compact, so after an upgrade every continuation of a
capture an older build took or a job it collected is told as cut off and its
guard holds no skill. An older asker computed `BoxReadBudget` without the read's
share, so a newer host's read spends up to 27 s of it, cutting the read's own
steps from 45 s to 18 s before the take is cut. An older relaunch tells as
before. No state-log record, no `RecordVersion`, no `coord.ProtocolVersion`
change.

## Amendment: an OpenCode run's calls to its own server are bounded, inside the end's wait

An OpenCode run's script reaches its session through its own server: the
readiness check before the brief, the usage before and after the run, the
interrupt an end request makes first, the leftover steers' inbox, the export
that is its capture, the pages that measure its session, and the login's
exports. None of those calls had a bound (crewlet#1088): a server that took one
and never answered it held the script there until the job was stopped, and an
end's wait (`JobEndWait`) ran out on a script still waiting, so the read killed
the job and packed the session itself, having spent the whole wait first.

Every such call now goes through one helper of the script's
(`openCodeBounded`), which ends it past its kind's bound and goes on as the
script goes on where the server is gone: the readiness loop to its own
deadline, which now holds, each poll held to what is left of it; no usage line;
no cancel after the first that ran out, the cancels together held to one bound;
the export said not made, so the read finds no capture and packs the session on
demand once the job is over, beside a database no server holds any more; the
login kept as the box last exported it. A call made before the run is ended by
an end request too, and none is started once one has come. The helper reads a
call's end from a status file its subshell writes, never `kill -0`, which reads
a child that has exited and is not yet reaped as running (crewlet#1164); ends
the call `SIGTERM` first and `SIGKILL` past `JobStopGrace`; and reaps it.

The bounds are constants, one a kind of call, each set from a probe of
OpenCode 2 (on 2.3.2 only the readiness `mcp.list` re-measured, 0.4 s): every call timed in a run's own
script, twenty of each kind on an idle box and on one whose two cores were kept
busy, and exports and pages of sessions of up to 256 MiB. `openCodeCallWait`,
ten seconds, holds a call whose answer is a few KiB, the slowest measured 687
ms; `openCodeSessionCallWait`, thirty seconds, one whose answer holds the
session or a message of it, the slowest 6,397 ms (a 256 MiB export with both
cores busy), past which a capture's session is packed on demand and nothing is
lost but the wait. No shorter bound is OpenCode's own: a client whose server
answered its first request waits on the call for as long as the server takes
(each measured to the probe's 150 seconds), and every client the probe ended
went at its `SIGTERM` within 57 ms and left no process behind. The calls on an
end request's way out, each at its bound with the grace after it, and the
three processes the script stops, add up to 125 seconds (`openCodeEndPath`),
inside the 135 seconds `JobEndWait` gives an ended job, so the script's own
export is never what the read's kill cuts short.

The standalone calls (`--standalone`: the session gate's, the pack on demand's,
the login's import and `quit`'s export) are not bounded. Beside a live idle
server, beside one stopped while its run streamed a step, and after one
killed, every one of them completed on every release probed, the slowest in
2.6 seconds six at once on two cores. OpenCode keeps its database in WAL mode,
so none of them waits on the run's server to read, and those that write run
before that server starts or beside one going with the job; and the pack on
demand is the engine's, held to the engine's own wait (`SessionPackWait`).

## Amendment: a cut-off session's archive is read for the calls whose logged results it holds

A relaunch that continues a kept capture whose harness was cut off tells the
session every call the run's log took after the capture's save point, and a
cut-off capture's save point is the newest its launch held, not where its
session ends ("a capture whose harness did not end its own run is placed at the
newest save point its launch holds", above): so the list repeats calls the
session holds. Yet the harness files each call's use and its result under the
id it gave the call, which the log now records ("a bridged call carries the id
its harness gave it", above). So the capture's own archive can say which of
those calls the session holds the logged result of, and a relaunch can tell it
exactly the calls it lacks.

**The reading, and its place.** `sandbox.Coordinator.HoldingsOf` reads it:
inside one wait taken before the run's record is read (`holdingsReadWait`, 30
s, so a reading at its bounds is one, and a relaunch's frame spends it on a
kept session beside its wait for the keep, and the one bounded step a reading may
overrun its wait by); only of the capture that record keeps, cut off, where
`ContinuityOf`
still continues the turn's row with the runner that reads it; through that
runner (`sandbox.HoldingsReader`, the executor's `Runner.ReadHoldings`, which
reads Claude Code's sessions alone), handed an opener that streams the row's
object (`SessionRestore.Open`). What a session holds is what its resume hands
its model, and that is the code of the release that continues it, not of the
one that wrote it: so a reading is asked for that release, the relaunch box's,
as it answered `--version` before the brief was chosen (`HoldingsAsk.Resumer`),
an unread one is no reading for every reader, and each reader reads only for
the releases whose resume it read (Claude Code's: `claudeResumeRead`, the
releases its model of a resume (`threadChain`) was read from, a set and never a
range, since a release before, between or after them may hand its model
otherwise). It answers facts, and one rule alone turns them
into a verdict (`SessionHoldings.Holds`). Every failure is no reading, never a
refusal: the relaunch then tells every call after its save point, as it does
without one, which is never wrong. It is the reading alone: the relaunch's
telling that asks for it is a piece of its own.

**What it reads.** The members the line layer names exactly: the session's main
thread, and each depth-one subagent's thread and metadata. Every other member
is passed over: another session's file, a deeper thread (a Workflow's, a nested
agent's, most of a real corpus's subagent threads, probe P6), and the session's
saved compaction. So a call recorded only in an unread member is absent from
the reading, and told. An archive naming any member absolutely or through `..`,
or holding a member the reading reads twice, is no reading, since a restore can
write either over the file the resumed session reads.

**What it costs, and its bounds.** A second object-store read and opening of the
object the launch that continues the session fetches again: at most
`executorkeep.SealedMaxBytes`, ordinarily a mebibyte or two, streamed and never
spooled, as the launch's own fetch is not: the node's scratch disk holds only
the spool room's captures (`sandbox.spoolRoomBytes`). The
archive's plaintext is held to `threadPlainMax` (256 MiB) by the scrub's meter,
and what the members it reads hand out to the same budget, which a sparse
member's holes cannot pass; the lines of the threads it reads to
`threadLinesMax` (2^20) between them; a line's facts to `threadLineMax` (8 MiB)
in sum, and a value it reads whole to `threadValueMax` (64 KiB), past any of
which the reading is over, while what no reader reads of a line is passed over
unheld at any length; the writer to `holdingsWriterFloor` (2.1.310, the oldest
release its rules were measured on; a writer below it, unknown or unreadable, is
no reading), the session's last as its row records it and every chained record's
own as its `version` names it (in a chain as Claude Code's resume reads one: a
user, assistant, attachment or system record whatever its `parentUuid`, or
another that carries one), since a box continues a session an older release
wrote and so holds the records of each (every chained record of the 432 main and
89 subagent threads the probes kept names its own writer): in the main thread
such a record is no reading, and in a subagent's thread it leaves that subagent
not whole, its calls told under it; subagents to `holdingsMaxAgents` (1,024),
legs to `holdingsMaxLegs` (4,096), a metadata to `holdingsMetaMaxBytes` (64
KiB), a description to `holdingsDescriptionBytes` (1 KiB), what the model of
Claude Code's resume, or of a subagent's own load, files of a thread to
`threadChainMaxRecords` and `threadChainMaxCalls` (2^17 each, a record or a call
a line of the 130,754 lines of 2,053 bytes, probe P6's average, the plaintext's
bound holds; one thread's at a time, a subagent's thread past them read on and
not whole); and the ask to `sandbox.MaxBridgeCalls` (200). Every id it keeps is
one the log's call-id rule holds: an id is the box's word, as long as a value
the layer reads whole, and a call under any other is one nothing can link,
answer or notice. It reads in a slot of its own, one (`HoldingsReadsAtOnce`),
outside the seats' and the process's transcript reads, and waits for it inside
its wait. Measured (`BenchmarkAReadingAtItsBounds`, on a dedicated
two-core build slot, one case a process, a time the spread of three runs at the
default GOGC; what is held at most the largest live heap with the collector run
at every tenth of the heap's growth, and a peak the largest heap at the default
GOGC): an ordinary session, a 1.6 MiB archive and 5.6 MiB of lines, its 200
asked calls each held, reads in 72 to 88 ms holding 1 MiB; one at the archive's
bound, 118 MiB of lines, in 1.7 to 1.8 s holding 16 MiB; 255 MiB of ordinary
lines, the plaintext's bound, in 3.1 to 3.2 s holding 31 MiB, its heap peaking
at 51 MiB; 1,024 subagents, 200 of their calls asked and held, in 0.17 to 0.27 s
holding 2 MiB; a whole PDF's Read, 53 MiB of lines, in 56 to 75 ms, allocating
222 to 236 KB in all; four prompts of about 64 MiB, the plaintext's bound, in
1.09 to 1.11 s, allocating 217 to 226 KB in all; a line of the smallest results
a message holds, 16,384 of them, the most whose facts the line's bound holds,
every one of which the layer keeps, in 41 to 51 ms holding 4 MiB, its heap
peaking at 5 MiB; a thread at both bounds of the model of the resume, 2^17
replies each making a call of its own and naming a parent written after it, in
2.3 to 2.5 s holding 43 MiB, its heap peaking at 70 MiB, the most a reading
holds; the model's other bounds, its calls and its retractions and kept uuids,
in 0.55 to 0.59 s and 0.91 to 0.98 s holding 13 and 11 MiB; and that line of the
smallest results as the record that fills both, read while the model decides, in
2.6 to 2.8 s holding 39 MiB, its heap peaking at 57 MiB. A reader's
own buffers hold about 146 KiB whatever a line's length, about 280 KiB between
lines once a line's values grow its capture and its scratch, up to 1.7 MiB
within a line that grows every scratch buffer (internal/sandbox/executor's "A
collection's read of a thread"), and what no reader reads of a line costs it
nothing more (`BenchmarkAReaderAtALineOfAnyLength`). An id the
call-id rule refuses is never kept: 96 Agent calls under ids as long as a value
read whole, and 1,024 metadata naming ids of 65,000 bytes, read in 29 to 34 ms
and 0.22 to 0.31 s holding 1 and 2 MiB; and one past `threadValueMax` ends the
reading. So a burst of 32 relaunches reads ordinary sessions in 2.3 to 2.8 s on
the one slot. A line costs a reading time whatever its length: tiny lines to
`threadLinesMax` read in 2.5 to 2.8 s; one line of blocks of an empty kind,
which no reader names, to the plaintext's bound in 17.7 to 18.0 s holding 1 MiB;
and the slowest measured, a subagent's thread of 256-byte lines of such blocks
to both bounds at once, which gzip shrinks to a mebibyte, in 21.4 to 22.6 s on
the same cores holding 15 MiB: about 25 s behind such a burst, inside the
reading's wait, a reading past which is no reading.

**The rules it reads by.** What Claude Code's resume hands its model of a
thread, which records and in what orders, is not stated here: it is the model
of that resume, `threadChain` (internal/sandbox/executor, claudechain.go),
whose type doc is the authority, every rule in it beside the offsets of the
code of 2.1.310, 2.1.322 and 2.1.330 it restates, under every variant of the release
and of the flags Anthropic's servers set that a reading cannot see, with three
answers, handed, not handed and cannot tell, and the reason for each. In the
main thread it is `claude --resume`'s load as the loader's caller hands it on,
past the retraction filter and the drop of malformed attachments; in a
subagent's thread, the one conversation that subagent's own load hands, which
is what a SendMessage continuation hands its model. A main thread the model
cannot place, whatever shape it met first, is no reading, and a subagent's
thread it cannot place is not whole. The reading cites the model for what is
handed, asks it only of a release it was read from (the gate, above), and adds
rules of its own alone, each of which tells where it is in doubt.

Rule 1: a call counts only through whole lines, and only past its thread's
last restart (a compaction's boundary, a microcompaction's, a span replaced, a
root after a thread's first chained record, a first chained record that roots
no chain). In the main thread that holds a result, a leg's own answer and a
task notification alike; a subagent's thread that restarts at all is not
whole. It is the reading's own FORWARD GUARD beside the model's truth: the
model says what the two releases hand, and a later one that cut a thread at a
microcompaction's boundary, a span replaced or a later root would hand less
than lies before it.

THE PAIRING RULE (`holdingsPairing`, the conservative rule (C)). A call is held
only where the request a resumed session sends pairs its use with its result,
which the reading reads off the orders the model hands and never off the
file's: the request merges a reply's records by their reply id as written, an
absent one among them, pairs a use only with a result in the message right
after the one holding it, and strips a result found anywhere else, so an
attempt the harness withdrew can lie past every restart and be handed to no
model, and a result handed after another reply is handed with no call. So a
call is held only where the record of its use carries a reply id (C0); one
use of its id and one result are counted over every user and assistant record
of its thread's file, chained or not, the thread's own or not, its use names a
tool of the bridge's own MCP server (`mcp__crewlet__`, so none whose result
Claude Code clears or withholds by its tool's name: its own tools' on a
context hint, its memory server's, or its Poll calls its code mode folds),
and no batch's list, split id or id Claude Code's request writes of its own
folds it into another call (C1); the resume hands both records under every
variant (C2); and on every order it hands, the result comes after the first
record of the use's reply, every record from that one to the later of the use
and the result is a plain record of that reply or a plain results record (C3,
C4), no record before that first one holds a result of a call the reply made
(C5), and no compaction's boundary lies at or after it (C6). The rule tells,
knowingly, shapes the request does pair that it cannot place, none of them
measured: a call it tells that the session held is told again, which is the
relaunch's behaviour without a reading. Measured (probe T, both releases): a
continuation hands its model each whole-line result of its main thread the
model hands after its last restart, verbatim and with its call, the results of
parallel calls off the chain among them, every one of which the rule holds;
and over the orders the facility hands each thread file the reading reads, of
the calls the probes kept with one use and one result, both handed, its
structure holds all 935 of the main threads' and all 113 of the subagents'
(and of the test fixtures', all 50 and all 24).

A result its harness replaced for its model is live in no thread: behind a
flag Anthropic's servers set, Claude Code hands its model a preview in place of
the largest results a reply's calls returned past a budget, records the
replacement beside the result's own record, which keeps the whole output, and
hands the preview again on resume. A record a retraction names (a refused
reply's records, which Claude Code takes back as it moves the request to
another model, or a malformed reply's, which it retries) is one the model's
retraction filter takes from the main thread, so the pairing rule's C2 tells a
call it holds; no load of a subagent's thread filters by such a list, so a
subagent's thread holding one is not whole. Rule 4: a call is held only
through its one counted result, whose `is_error` is the log's failure and
whose text is the output the log took, compared by SHA-256 digests so no tool
output leaves the archive: every byte of it where the log kept the output whole
(up to `MaxBridgeOutputBytes`, 4 KiB), and else its length and its first 256
bytes, inside the 672 bytes of its opening the log always keeps of an output it
cut (`HoldingsOpeningBytes`). Rule 3, a result a process wrote without the
call's outcome (a resume's for a call it found open, a stopped harness's, a
call its harness gave up on), is met by rule 4, and no mark such a result
carries is read.

Rule 2: a call in a subagent's thread is held only where the reading reports
that subagent and the pairing rule holds of the call on every order that
subagent's own load hands. It reports one only where its metadata, read by
exact keys, links it to exactly one Agent or Task call of the main thread; its
thread is whole: every line a record of its own, its member ending in no torn
piece, which its own load may read as one more record, never restarting, no
reply of it one its model refused, which Claude Code takes back without a
record of the fallback in a subagent's file, no record of it taking records
back, the model of its own load placing it, and THE STRICT RULE, every record
of its own that no record of its own continues being that load's leaf or one
it hands, so its file holds no branch of its conversation its model was not
handed; each leg (that call, and each SendMessage to it its own result did not
refuse) has a live answer in the main thread past its last restart, the call's
own completed foreground answer, which the pairing rule pairs with the call
(its id's one use and one result counted over every user and assistant record
of the main thread, which then holds no batch's list and no result of a split
id at all, since a leg's id is known only once its call is read), or a task
notification the resume hands its model under every variant, which a
continuation hands its model verbatim (probe T, T10); a SendMessage under an
id the log keeps none of is a leg nothing answers, and an answer carrying a
HANDBACK answers no leg, nor does any answer of a leg one of whose answers
carries one, since under one the subagent's report reaches the main thread
through a message of its own and its answer is the harness's words in its
place (its own answer's `handback` key, a notification's result naming
`SubagentHandback`); its file starts no more legs than that; and along every
order its load hands, each leg ends on a reply whose text says something, with
which its model ended its turn, its stop reason `end_turn`, no API error and a
model other than `<synthetic>`, Claude Code's own whole test of one, since a
subagent answers with the text of its last reply that says something: a
request that fails for good leaves a reply the harness writes itself, its text
the error's, on which it ends the subagent and answers the call that started
it as completed, though no model read what that request carried, and every
reply the harness writes in place of a model's is of the model `<synthetic>`.
A call in an unreported subagent's thread is told under that subagent's
heading. A seat server's call needs no rule of its own: every continuation
probe T made carried the call and its result whether its server answered, was
left out, held its `initialize`, refused it, or no longer offered the tool
(T6).

**A continuation's first request is its caller's question, not the reading's.**
Claude Code compacts a conversation before a request wherever its own count of
it reaches its threshold, a continuation's first request included, and then
hands its model a summary, the preserved tail and the brief rather than the
history the archive holds. So a reading says only what the archive holds, and
its caller may use a reading only where it has judged that the continuation's
first request hands its model that history whole; where it cannot judge so, it
tells the session every call after its save point, as with no reading. A saved
compaction the session holds is the same question, and the reading passes its
member over. The paths that would move or clear what that first request hands
over without the harness's count are closed apart from the reading: the box
unsets every variable that switches compaction off or moves its point
(`claudeCompactionOff`) and pins the point it compacts at over a repository's
settings files (`ClaudeCode.Pins`); and no MCP server may be named `container`
(`internal/config`'s `claudeContainerServer`), whose calls of a tool named like
a built-in Claude Code has the API clear from a long session's requests as that
built-in's own, so no logged call is one it clears.

**Mid-upgrade.** The reading writes nothing to the wire, the run's record, the
turn's row, the state log or the coordination store: no record is stamped, and
no `coord.ProtocolVersion` change. It reads an archive whatever build kept it,
since the archive's format is the runner's own, in the relaunching node's
process alone; a node of a build without it tells by the save point, which is
sound.

**What it leaves.** A gateway that repeats a `tool_use` id within one
conversation, which P6 found none of among 298,329 first-party calls: two calls
under one id read as one call's records, each told wherever they are more than
one use or one result.

WHAT THE GATE CANNOT CLOSE. The gate reads the resuming release, never the
box's settings. A managed settings file, which Claude Code applies after every
other source and so over the run's pinned settings copies (`ClaudeCode.Pins`),
can switch on either of the resume's two switches again
(`claudeResumeSwitches`), which the run's script unsets and its settings blank.
The interrupted turn's switch (CLAUDE_CODE_RESUME_INTERRUPTED_TURN) has the
continuation run again the turn its last process was cut off in, and acts as
the continuation's first request is built: its caller's question (above), and
one neither its caller nor the reading can see in the box, so it is left. The
precompact switch (CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP) moves which large-file
reader loads a thread past 5,242,880 bytes; the model reads both, so it costs
only a reading of a thread the two may load otherwise, which is no reading. A
switch of such a file that no read of the releases found is left with them,
as is a harness that answers `--version` as a release the model was read from
and resumes otherwise. A release before, between or after 2.1.310, 2.1.322 and
2.1.330 is not left: the gate refuses it, and adding one is reading its resume against
every step of the model and running the model's census over sessions it wrote
and resumed (`claudeResumeRead`'s doc).

A path behind a flag Anthropic's servers set that no read has found: probe T
ran with every such flag off, so each flag the model reads was read in both
releases' shipped code rather than run, and each is a variant the model hands
an answer under. What the resume hands its model and the reading does not hold
is told, the safe direction: a shape the request pairs that the pairing rule
cannot place; a call wholly inside the stretch a compaction kept, which lies
before the last restart (none of the 22 such stretches the probes kept holds
one); and every call of a thread the model cannot tell for.

## Amendment: a told call is kept apart from every call it could be taken for

A brief tells a continued session the calls it may lack, and two of its lists
say that a call shown both in the session and in the list is ONE call, made
once: the calls since a save point the session may already show, cut off or a
summary (`prompts.calledSinceHeader`), and the late calls, which an earlier
message may have told (`prompts.lateCallsHeader`; and
`prompts.MayCompactSession`, which says each is one message, call or note).
Where a call the session may show and a call one of those lists tells read
alike, the model reading both could take the told call for the other and lose
it: re-issue nothing it needed, or read it as a call that never ran. That is
reachable wherever a save point holds calls beside a late one (a capture whose
harness ended its own run, placed by its own read; a cut-off capture another
build stamped, its point above zero) and in the copy whose harness ended its
own run, which holds every call of the round but the late ones, on every
harness; and wherever the turn has closed rounds, whose calls a session that
continued through them holds as well as the round's own. A reading of what a
cut-off session holds, the amendment above, would make it routine. And two
calls the one brief tells can be taken for each other as well: a call of one
told list and one of the other, a told call and a held call a lead of the brief
tells, or a told call and a line of the work already done that the brief
renders, a run of its own's among them, whose late calls' words
(`prompts.lateBeforeHeader`) say each is ONE call, made once. And the session
may show calls the engine holds no line of to list again: an earlier turn's,
an earlier round's where the session may hold it only in a summary's words,
one the log no longer lists one by one, and a worker's under a delegation.
Three mechanisms answer it, chosen by what the engine can know of the other
call: listed again where it holds the call and the session may show it, a told
line marked where the same message tells the other call, and a section that
names the calls no list holds one by one where it holds no line of them.

**Listed again.** `runner.calledAfter` keeps the calls the session may show
and lists again each one a told call could be taken for (`listedAgain`), in a
section of its own after the calls since the save and before the late calls
(`prompts.AfterSave.Shown`, `prompts.shownAgainHeader`), under words that say
the session may show each and that the told call could be taken for it: a call
of its own, made as well, never this one shown twice. First the calls of the
turn's closed rounds that the brief tells nowhere else (`runner.closedShown`),
each line ending with a note that it was made in an earlier round of the turn,
or of the fire in a schedule's conversation (`prompts.EarlierRoundNote`), which
says where in the session it may be; then the round's own calls the save point
holds (`heldAt`), with no note, since on a send-back the round is the one just
sent back. True on every path: a call the save point holds may reach a resumed
session only in part (a Claude Code subagent's bridged calls reach its main
thread as the subagent's report), and a closed round's call is in the session
as it continued through that round, as a fresh brief's line of it, in a
summary's words, or not at all, so the words say "may show", never "shows", and
never that the session lacks one; and "could be taken for" is the engine's
reason on every path, where "arguments that read alike" would be false of a
told call in flight, a sealed call, one whose arguments are not comparable for
any other reason, or a value either line shows as a piece or a record left out
(`readAlike`, below).

**Read alike as the lines show it** (`runner.readAlike`). Symmetric, over two
`ledger.Call`s. The same tool; then alike where either call's arguments are not
comparable (`runner.argsComparable`: withheld, in flight, in text that could
not be read, recorded so or never parsed (`ledger.Call.ArgsUnread`), with no
digest, or left out whole by their record,
`kept.IsNotKept`) or the two digests are equal; otherwise apart only where a key
is in one call and not the other, a key whose value is null read as absent, or
in both with a value each line shows whole (`ledger.ShowsWhole`, the render's
own test, asked of both) and the two not the same JSON value
(`runner.valuesAlike`): a number by its value however it is spelled, compared
exactly as a sign, digits and an exponent, never as a float or by the power an
exponent names; an object or an array value by value; a string alike only the
same text, or the number or boolean it spells; and a value a record left out in
place (`kept.IsLeftOut`, the marker under any record's name), at any depth,
alike any value. Either line, because the session may show either call only as
a line (a fresh brief's prior work, the calls an earlier brief told), and a line
shows a value past the ledger's budget as a rewrite, or omitted by its size, so
two writes to one path whose contents differ there read alike in their lines,
digests apart. Never by how much a record kept (`ArgsBytes`): a record that
kept a call's arguments less than whole says so in them, each value it left out
a marker or the whole `{"not_kept": marker}`, so the values it kept are compared
as any are and only what it left out reads as anything, where reading the whole
call as unknown listed it beside every call of its tool. Per value, too, so no
digest is load-bearing beyond equal digests reading alike. Results are never
compared: a successful call's line carries none. Reading two calls alike that a
model would tell apart costs a line; reading apart two it would take for one
loses a call, so every unknown is alike.

**The held side and the told side.** Held, the closed rounds' calls first:
every call of every closed round, in order, but none on a run of its own
(`runner.toldAs`'s `fresh`, the reseed and its base form), which has no session
above; none of the closed entry that stands for the round whose calls the brief
tells (`roundCalls.of`), which a send-back's closed rounds hold last, so its
calls are listed once, as the round's; none of the rounds the brief renders as
work already done (a task told again renders the rounds its brief rendered),
where a call listed again too would be told twice in one message; never a
skipped meta-tool's; never a `submit_work`, which was sent back, as a told one
is already said not to stand; and never a withheld call of the round and tool
of a held call a lead of the brief tells (`runner.toldHeld`), whose answer tells
what became of it, since a closed list keeps no held call's id to pick it out
by. Then every call the point holds; never a skipped meta-tool's, the call a
person approved that the brief's lead tells (the amendment on the approved
call, above), nor a call the log dropped, which keeps no arguments. Told: every late call,
and the calls after the point only where they are told as calls the session
may already show (`toldAs.mayShow`, which now sets `AfterSave.Interrupted`
itself). Never the owed reads,
whose words say an earlier message counted them, and never a list under words
that say its calls are not in the session (`prompts.calledAfterHeader`), where
no call is shown twice. One pass is the whole answer: a call listed again is
told as one the session may show, never as a call it is told for the first
time, so nothing is matched against it.

**Two told calls, and a told call beside work already done** (`runner.notes`,
`runner.partners`). Listing again answers a call the session may show; a call
the same message tells is in front of the model already, and listed again it
would be told twice. So a told call another call of the brief could be taken
for is marked instead: its line ends with `prompts.ToldApartNote`, "(a call of
its own: another call this message tells could be taken for it)". Its partners,
each read as above: a shown call of the other told list (the late calls, the
calls after the point), the two each marked; a held call a lead of the brief
tells what became of (`toldAs.leads`), of the told call's tool, since that call
and the one a person approved for it read alike every call of the tool, being
withheld or in no list; and the closed rounds the brief renders as work
already done (`toldAs.rendered`: a fresh brief's prior work on a run of its own
and its base form, the rounds a task told again renders), each shown line
(`runner.partnersOf`, the walk `ledger.RenderIterations` draws them by) and
each tally that names the told call's tool or no tool, since a tally shows no
arguments. Never a held call of the lead's own round and tool
(`partners.couldBeTakenFor`): the lead tells that call's own fate, and marked,
the note would be false where nothing was approved (a denial, a withdrawal, a
call not made); a held call carries no id (only the call approved for one
does), so every held call of that round and tool is spared, and another rule
can still mark one. Never two calls of one list, which are two lines and so
two calls; never on or against the owed reads, which a run may run again; never
against a line the cap leaves out, by the render's own walk
(`ledger.ShownIndices`), so the note lands on the line of the call it names;
never on a call listed again, whose section says it already; and never on the
prior-work line, whose render is the reviewer's too. The marks are computed on
every path. True wherever printed: every partner is a call the message tells,
never the marked call itself (the lists are disjoint, a rendered round is not
the round whose calls are told, and the lead's own held call is spared), and
the engine could not rule out that one is taken for the other. A lead whose
held call names no id still counts by its tool (`runner.leadsHeld`).

**Calls no list holds one by one** (`runner.otherCalls`,
`prompts.AfterSave.Other`, `prompts.OtherCalls`). A section before every list,
after the held submission, says the session may also show calls no list below
holds one by one (never "names": the tally of the calls the record no longer
lists prints below and names them by tool), a bullet per part, and that
however alike one of them reads to a listed call it is a call of its own,
never that one shown twice. Each part names, of the tools its calls were made
to, the ones a told call names (the shown calls of the late list, and of the
list after the point where it is told as calls the session may already show),
each defused, and "tools the record does not name" where a count names no
tool: never a count, which is no figure of what the session shows, and never a
tool no told call names, which no told call could be taken for. The parts: the
conversation's earlier turns (`Resume.Base` set: the turn continued the
session its conversation's last turn left, of whose turns the engine keeps no
call, their ends carrying outcomes); the turn's earlier rounds where the
session may hold them only in a summary's words (`toldAs.summed`), the told
tools of every closed round's calls but the ones listed again, the rounds a
task told again renders above it included, and of every closed tally; the
calls the log no longer lists one by one, of the round where some of them may
be in the session (`runner.droppedMayShow`: never where the log this build
numbered puts every dropped call past a point the session ends at, under words
that say its calls are not in it) and of every closed tally where the closed
rounds are not told as a summary's; and the workers' calls under a delegation,
the round's and every closed round's (`roundCalls.absorbed`,
`ledger.Iteration.Absorbed`), which reach the session only in a worker's
report. Told only beside words that say a told call may be shown twice (the
late section, listed or holding only a count of dropped late calls, and the
calls after the point told as calls the session may already show), and never
on a run of its own or its base form: their lists name no session above, and
the base form's session holds only earlier turns, every one of them above the
boundary its own lead draws at the top of the same message. A schedule's
conversation says it of its fires. True of every part: the record lists a call
or counts it, never both; an earlier turn's call is never this turn's; a
worker's call is never one of the run's own; and an earlier round's call alike
a told one is listed again, so the part names only the rest.

**A summary's words, read from the session's restarts**
(`runner.sessionHolds.restarted`, `closedSummarized`, `restartedAnywhere`). A
Claude Code session whose thread restarted anywhere, before its job or in it,
or whose count cannot say whether it did (`sandbox.ResumedCount.Restarted`),
holds what came before its newest restart only in that restart's summary, and
the telling that followed a restart in an earlier job told that job's round
again from its start, never the turn's closed rounds before it. So the closed
rounds are told as ones the session may hold only in a summary's words where it
may be a summary now (`summarized`) or restarted at all. A restart in an
earlier turn's session counts too, an over-telling and the safe side. It feeds
only how the closed rounds are told; the round's own calls are told as the
first-request verdict decides. Closed calls are still listed again value by
value under a summary, beside the earlier-rounds part: the kept path hands its
summarized telling wherever the held one may compact, on the claim that it is
never the shorter, and with closed calls listed in both and the part's tools a
superset in the summarized telling, which tells every call of the round, the
claim holds.

**One cap.** The calls listed again, the closed rounds' among them, are fitted,
capped and defused with the other lists under the one read cap
(`ledger.FormatCallGroups`), spent on them last, after the owed reads, since the
session may show each; a read among them the cap leaves out is never recorded
as one the brief owes (`sandbox.BriefTold`). The earlier-round note is the
caller's words at the end of a line (`ledger.FormatOptions.Notes`), by the
call's place in its group, never fitted and never on a line the cap leaves out.
The section is rendered only where it lists a call, as each list is, so no
brief carries an empty one.

**What it costs.** A line per held or closed call that a told call reads
alike, a closed one 40 bytes more for its note, 75 bytes for each told call
another call of the brief could be taken for, and for the calls no list holds
one by one 220 bytes and 69 to 110 for each part with the tools' names, in the
brief and so in its launch's floor and any rewrite of a long value. A brief is
byte for byte what it was only where its held, closed and told calls, the
leads' held calls and the work already done it renders read apart, no call of
the round has unknown arguments, and no part of the calls no list holds one by
one is told: no earlier turn's session under it, no dropped, tallied or
delegated call of a told tool the session may show, and no closed round under
a summary's words, OpenCode's among them. The earlier-turns part alone prints
on every continuation of a turn that continued its conversation's session,
with a late call or a list of calls the session may already show. That is not
always a line or two: one told call whose arguments are not comparable (in
flight, sealed for a person's approval, left out whole by the log, or with no
digest) reads alike every held and closed call of its tool, so on the copy
whose harness ended its own run, which holds every call of the round but the
late ones, or beside many closed rounds, a single late call of a much-used
tool lists every held and closed call of that tool again, each write whole and
each long value rewritten by the seat's auxiliary model, within the ledger's
budgets and its one read cap, spent on these last. The closed rounds the park
keeps are held to the row's share (`ExecuteStateBytes`), which bounds them. A
launch's floor that a kept session's context and brief cannot meet in one
request drops that session and starts a fresh run instead. Closed calls are
listed value by value in both of the kept path's tellings, the one held and
the one told as a summary, so the summarized telling, which tells every call
of the round, is never the shorter, the claim the path hands it on. The longer
brief also counts toward Claude Code's compaction threshold, which the
first-request verdict judges the brief against, so it can tip a continuation
into being told as one that may be a summary. A fresh run's brief lists
nothing again, and marks its told calls as any brief does. And every line of a
call whose arguments are not known (its outcome unknown with none kept, or in
text that could not be read) now says why in their place, for every reader of
the ledger, never `tool()`.

**Mid-upgrade.** Two fields are new on the wire, each additive:
`ledger.Call.ArgsUnread`, which a parked turn's closed rounds carry for a call
whose arguments were in text that could not be read, and
`sandbox.BridgeCall.ArgsUnparsed` (`args_unparsed`), which the run's log
carries for a call whose arguments did not parse. An older build drops the
first and keeps the second verbatim (`BridgeCall.Extra`), and draws and counts
either call as it always did, as one made with no arguments: the over-listing
direction is lost only on that build's own briefs. The first is no
`execstate.Version` bump, a decode defaulting the field. The rest is how one
brief is worded, from what the round's log and the parked turn's closed rounds
already record, and each build draws a call's line for itself: an older
build's continuation of the same round tells the round as before. No state-log
record and no `coord.ProtocolVersion` change: `ledger.FormatOptions.Notes`,
`runner.toldAs`, `toldHeld`, `roundCalls.of`, `sessionHolds.restarted` and
`prompts.OtherCalls` live in memory.

**What it leaves.** An OpenCode session's restarts decide nothing here (a
later amendment counts the session and names them, read only once its first
request is judged), so its closed rounds are told as held unless it may be a
summary for another reason. A closed round with two approval flows of one
tool, one of whose answers the brief tells, lists neither withheld call again
(a closed list keeps no held call's id) and marks the told call against the
lead's. The per-value rule reads `ledger.ValueLimit` as the build that told the
session's lines did.

## Amendment: a cut-off session's relaunch is told only the calls its reading does not hold

The reading of what a cut-off session holds (the amendment that reads a
cut-off session's archive, above) was "the reading alone: the relaunch's
telling that asks for it is a piece of its own". This is that piece. A
relaunch that continues a kept capture whose harness was cut off now asks it,
and beside the briefs that tell every call after the capture's save point
hands the launch briefs told by it, which tell none of the calls it finds the
session holding. So "some more than once, none never" (the amendment that
places a cut-off capture at the newest save point its launch holds) and the
cut-off copy's and the first request's tellings (above) now say less of such a
session where a reading was read and its brief handed: nothing a reading holds
is told again, and every doubt is still a told call.

**Where it is asked, and how often.** Once per relaunch, in the runner, before
any brief is led (`runner.keptTellings`, `runner.heldReading`): never once per
telling, though the kept path builds up to three. Only of a kept capture whose
harness was cut off (`SessionBasis.MayLackBrief`), the one capture a reading
reads; only where the verdict before any brief is the brief's to decide
(`byBrief`, or `byBox` where the launch judges it in its box, each of which
needs `firstRequest.judges`: a reading may be used only where its caller
judged the continuation's first request, the reading's own amendment above),
so never of a session that compacts whatever its brief, nor of one whose
harness has no reader yet (an OpenCode one); only where the launcher reads
holdings (`runner.HoldingsAsker`, optional, the engine's
`agentLauncher.SessionHoldings`, which answers a value and never an error); and
only where at least one call is asked, since the reader refuses an ask of
none. A restart in the capture's job is no bar: the reading counts a call only
past the main thread's last restart, and the telling it thins keeps every word
a session that may be a summary is told (the cut-off section, every lead the
record keeps, every note the turn has read, an empty guard). Its wait and
every rewrite its telling makes are spent before the budget's last ask, as
every brief's are.

**What it is asked** (`runner.holdingsAsk`). Every call the round's log lists
but the ones a person approved, which the engine made on its host, each by its
key and its outcome as the log keeps it; a call in flight has no outcome and
rides no ask. Asked exactly where the telling would list the call after the
point or among the owed reads and the reader can name it: not a skipped
meta-tool's, not held at the save point (`heldAt`), neither late nor
abandoned, its key valid, and its key carried by no other call of the round,
listed or in flight. ONE harness-keyed function answers a call's key and
whether it is valid (`runner.callKey`): Claude Code's is the id its harness
gave the call (`BridgedCall.CallID`, carried to the runner for this from the
log and from the calls in flight, `bridgedCalls`, `unfinishedCalls`), valid
where the log keeps it (`sandbox.ValidCallID`); every other harness's is none,
so no call of its is asked. The ask's rules and the telling's lookup of the
reading go through it, and nothing else names the field. An owed read is
asked: it is held only where its records, live and the log's, are in the
session. A skill load a harness's hook reported carries no id, so it is told,
and the guard never arms it through a reading. What the OpenCode reader will
add to the ask and to `sandbox.LoggedCall` is its own and read by it alone,
and none of it exists yet: `Before`, `Unlisted` (the calls in flight and the
dropped ones by identity, with `OnlyFailed`), `ArgsText` and the
OpenCode-only arguments key, `HoldingsOf` handing it `RunStart`, and
`Appended`, `Seq`, `Name`, `Digest` and `Held` (held for a person's approval,
its arguments withheld and its output the held answer, never the save point's
hold); and its key, in `callKey`. The Claude Code reader reads only `ID`,
`Asked`, `Failed`, `Output` and `OutputBytes`, every one of which is filled.
No call is marked as told by an earlier brief's text for Claude Code: a
cut-off capture is not known to hold its job's brief, and no record keeps what
that brief told, so a call it told as text is asked, found unheld, and told,
as before.

**The resumer is predicted, then checked.** The reading's amendment above has
it asked for the relaunch box's release "as it answered `--version` before the
brief was chosen". The relaunch's box does not exist when its briefs are
written: it is acquired inside the launch, after the budget's last ask. So
the reading is asked for the release that WROTE the session, the turn's row's
(`SessionBasis.Release`, the `--version` the job's box answered before its
harness started), the relaunch's best word for the one that resumes it
(`HoldingsAsk.Resumer`, `ExactTelling.Resumer`); and the launch hands a brief
told by it only to a box whose harness, asked before its brief is chosen
(`CheckCarry`, `sandbox.Coordinator.continueKept`), answers exactly that
release, an empty answer on either side matching none. Every other box is
handed the brief told by the save point: an over-tell.

**The telling by the reading** (`runner.calledAfter` with `toldAs.read`). A
call the reading holds is told nowhere: it is held, as one at the save point
is, so a successful `submit_work` among them is said not to stand
(`AfterSave.HeldSubmission`), and it joins the held side of the calls listed
again (the told-apart amendment above, whose scope this follows rather than
restates). A call the reading finds in the thread of a subagent the session
holds no report of is told under that subagent's heading
(`AfterSave.Subagents`, `prompts.SubagentCalls`, `prompts.subagentsHeader`),
one group per subagent in the order of its first told call, after the calls
after the point and before the calls listed again, with its own line that a
successful `submit_work` among them does not stand, in words that claim
nothing of the session. The subagent is named by its agent id and its
description, the box's words, every run of whitespace one space, cut to
`ledger.ValueLimit` runes on a rune boundary and defused under the turn's
token (`runner.subagentName`), its id defused too. Its groups are told groups:
on the told side of the calls listed again, in the marks against every other
told group, never among the tools the other calls are named by. The calls
after the point are headed by words true of a list a reading thinned
(`prompts.readHeader`: the session may show some of these, less any it found
the session holds or lists below, never that they are every call since the
point), 471 bytes, no longer than `calledSinceHeader`, which it replaces. One
read cap still: spent on the late calls, then each subagent's, then the calls
after the point, then the owed reads, then the calls listed again, the
subagents' reads left out recorded as reads the next brief owes.

**One value per telling the launch may hand** (`runner.keptTellings.led`,
`exactHolds`). The kept path's default value is decided once by its verdict,
as before. The telling by the reading is told by the held telling's value and
judged on its OWN brief through the one predicate, never by the held brief's
verdict: it drops held calls and may add sections of its own, so it is
ordered against neither default, and it may ride beside the telling told as a
summary where the held brief compacts and it does not. Where it may compact
it is not attached, and `executor_session_holdings_unused` says so; the
default's `executor_session_may_compact` line says when it rides beside. The
guard reads the value of the telling the launch handed
(`runner.sessionDecided`, `SessionDecision.Exact`): where that is the brief
told by the reading, the job's own loads among the calls the reading holds are
armed beside those the save point holds, since a load the reading holds is in
the session whole, a subagent's included; none at all where the job restarted
the session.

**Carried, floored, chosen, recorded.** `SessionContinuation.Exact` carries
the briefs told by the reading, their own record of what they left out and the
release they were read for, beside the default ones, from the runner through
the engine (`sandboxSession`) to the launch. The launch's floor counts them,
and where it cannot carry them but carries the rest it drops them alone
(`internal/engine`'s `continuedFloor`). The launch chooses after `CheckCarry`,
and the job records what the brief it handed left out, whichever it was, the
reused box's path included (`continueSession`'s one assignment of `told`).

**Never a refusal.** Every way the brief told by the reading is not handed
falls back to the default briefs, which tell more and never less: no reading
(`executor_session_holdings_unread`), a reading that holds and moves no call
(no third telling is built), its own first request (the runner), the floor
(the engine), the box's release or an empty brief for that box (the launch),
each `executor_session_holdings_unused` with its own site's keys. No path
refuses a launch the reading would not have refused.

**The log lines.** `executor_session_holdings_read` (`seat`, `turn_id`,
`asked`, `held`, `told`, `unreported`, `writer`, `outcome`), `executor_session_holdings_unread`
(`seat`, `turn_id`, `detail`, `outcome`), `executor_session_holdings_unused`
(`turn_id`, `detail`, `outcome`, and the site's `agent_id`, `seat` or `agent`)
and one `executor_session_subagent_unreported` per subagent a told call is
under (`seat`, `turn_id`, `agent`, `detail`). Every value an int or a string;
a description never enters a line. The read line's outcome says what follows
it: where the reading holds no asked call and moves none, that the relaunch is
told every call after its save point, since no third telling is built and no
line follows; otherwise the brief told by it, or `_unused` saying why not, or
`executor_session_not_continued` where the session itself is not continued.
The floor's `_unused` offers the session told without the reading, since the
box that decides whether the session is continued is acquired after it.

**Mid-upgrade.** Nothing is written to the wire, the run's record, the turn's
row, the state log or the coordination store, and no stamp or
`coord.ProtocolVersion` change: the briefs, `LaunchRequest` and
`LaunchDecision` live in one node's memory, and `BridgedCall` is never
serialized. An older node relaunching the same turn tells by the save point, a
newer one by the reading; either is sound.

**What it leaves.** A relaunch box whose release differs from the writer's (an
image moved, an unreadable `--version`) is handed the default brief, an
over-tell; so is an OpenCode cross-release continuation, which gets no reading
at all. A call an earlier brief told as text is told again. The reading's up
to fifteen seconds are spent before a budget refusal. A brief told by a
reading longer than the held one is judged and floored on its own and dropped
alone. An OpenCode cut-off relaunch is never read until a reader of its
sessions exists; its first request is judged in its box (a later amendment).
The release a reading is read for
is the box's word, its job's `--version` as the turn's row records it: a
forged one still has to equal the relaunch box's own answer before its brief
is chosen, and every record the reading reads still has to name a release at
or past the writer floor.

## Amendment: a capture's life is counted step by step on one clock, and read on another within twice their skew

Every rule that reads a begun capture's age reads it against a capture's
budget (`SessionCaptureBudget`, `standbyBudget`): past it, the capture is
taken for abandoned, a read standing by for it takes the next place, and a
turn's end records over it. That holds only where the budget bounds the
capture's life, from its stamped instant to the stamp of what came of it, at
the slowest crossing, and some steps of that life were bounded by nothing the
budget counts. Each is now decided as follows (internal/sandbox's
`coordinator.go`, `session.go` and `restore.go`).

**A collection's keep starts once its take is spooled.** The amendment that
takes a session before its box goes and keeps it off the read (above) ran the
keep "after the read has answered, off its frame".
A stop's and a loss's read answer as their take returns, but a collection's
goes on after its take: it records how its job's harness ended, pauses the box
and records the pause, and its keep started only once all of that returned. A
collection of a box the node hosts reads under its admission, which has no
deadline, so its pause was bounded only by its box backend and the pause's
record by nothing the sandbox states: the keep, and with it the stamp of what
came of the capture, could start after any capture's budget had run out, and a
turn's end could cut a session still being kept. `readBox` now hands the spool
to the keep (`Coordinator.keepLater`) the moment the take returns it, for
every read alike. The keep reads the spool and the claim's copy of the run,
never anything the read writes after its take. And the pause's record is held
to the room the read already leaves it (`Coordinator.pauseRoom`: a teardown's
grace and this node's crossing of one change of the row,
`Coordinator.markBoxPaused`), so a store that does not answer it no longer
holds the read, nor the resume behind it; a record so cut is the missing pause
stamp the read already warns of, and a run that parks dates its wait from its
park instead. Counting the read's later steps in the capture's budget was
rejected: a local collection's pause has no bound of the sandbox's to count.

**A takeover's read of the row it may have taken is held to one change of
it.** A standby whose takeover the store never answered withdraws first and,
where nothing is left to withdraw, reads the row to learn whether the place
after the capture it stood by for is its own (`Coordinator.tookOver`, above).
That read ran under the keep's own context, which has no deadline before the
keep's budget starts, after it; and the store's contract promises no bound to
a caller that sets none. So a store that never answered it held the place
begun, and the keep behind it, for as long as it did not: a step of the
takeover's life no budget counted. The read is now held to one change of the
row, a record's budget and this node's crossing of the row at its bound
(`Coordinator.rowContext`), as each stamp of the takeover already is; a read
so cut is a row unread, and the standby keeps nothing, as where the read
failed. The store's own bound on a read with no deadline was not counted
instead: it is no promise of the contract, and a backend of the contract need
not have one.

**Each of a take's records is given a grace of its own.** A read leaves the
records it makes after its take a room before its pause
(`Coordinator.takeRecordsRoom`), and makes two there, one after the other:
the take's own, the stamp of what came of it where it was refused or the
digest it leaves in the box where it spooled, and how a collection's job's
harness ended (`Coordinator.markHarnessEnd`), each asked under a grace of its
own from where it is asked. The room counted one grace for the pair beside
their crossings, so a take at its bound whose own record took its whole room
left the harness end a grace short of its own, and none at all on a node in
the store's own process, where the room was that grace alone: the record was
cut at the room's end and said as unrecorded
(`sandbox_harness_end_unrecorded`). The room is now a grace for each, and
the box read's budget (`BoxReadBudget`), which counts the host's rooms at its
share, counts the third grace with them: forty seconds more for every
cross-node read, so the host's take is still handed its whole budget.

**Every step of a capture's life is counted on one clock, and its instant
read on another within twice their skew.** The budget every age rule read
(`SessionCaptureBudget`, thirty-four and a half minutes at the floor) counted
a take, a keep and a stamp. It left out the reservation's crossing, the
keep's own reads of the coordination store (the turn's operations minted and
the seat's runs listed, which ran inside the upload's pace and at the floor
could spend all of it), and every step of a takeover (the takeover, and where
the store did not answer it the standby's withdrawal and its read of the row
back); and it read a stamp's instant, which its taker's clock wrote, on the
reader's own, though the engine holds two machines' clocks only within two
hours of each other (`objstore.AssumedClockSkew`). So a capture still being
kept could be read as abandoned: a standby took its place over and uploaded
the session again, and a turn's end recorded over it, its tombstone refusing
the session's record. The figures are now derived step by step at the slowest
crossing (internal/sandbox's `session.go`): the keep's two reads each in a
room of their own beside the upload's pace (`keepReadRoom`,
`SessionKeepBudget`, twenty-six minutes), a mint past it refusing the capture
and a listing past it deciding nothing; a take's life from its instant
(`takeLife`, an hour and one and a half minutes) and a takeover's (`takeOverLife`,
an hour and eleven), the longer of them a capture's life (`captureLife`);
and a read standing by's window from its own instant, its take's rest, its
own wait, the read of the turn's row on the log past it and the settle it
decides, and around its takeover a settle, its last read of the run's row and
the takeover itself (`standbyWindow`, two hours and thirty-one and a half).
Each change of the run's row counts a change's crossing, and each read of it
(a takeover's read back, a standby's last read, the end's last read) its
reply's alone (`captureReadBudget`, four and a half minutes with its reason),
since a read sends no write. Past a capture's life nothing its taker sends
lands, and past a standby's window nothing the standby sends does. Every node
reads a stamp's age against those and twice the skew (`SessionCaptureBudget`,
three hours and eleven minutes; `standbyBudget`, four hours and
thirty-one and a half), so a reader whose clock stands ahead never releases a capture still
being kept. The waits decide on their own monotonic clocks instead of on a
reader's word: a read standing by waits for the capture it stood by for that
capture's whole life and one read (`standbyWait`), then reads the turn's row
on the log once for one still begun and keeps nothing beside one it names,
reads the run's row once more and takes the place after one still begun or
refused, keeps nothing beside one that landed, and stands down beside one whose record still awaits
the log's answer (`Coordinator.waitedOut`); and a turn's end holds each
capture it sees for its life, and each standby for its window, from the
moment it first sees it (`Coordinator.awaitEndCaptures`), so a standby that
stands after the end began (a read admitted on another node's host before a
stop, which reads on after the ending is decided) is waited for whole; and
once every bound has passed, the end reads the row once more and decides on
that read, since its last read inside a bound is issued up to a tick before
it, and a takeover that landed after that read, in the window's last
instants, was otherwise never seen and its record met the end's tombstone.
The bounds are allowances at the floor: an hour and fifteen and a half
minutes for a capture from first sight, its life and that read, and three
hours and fifty-one and a half for a standby's, its window, its takeover's
life and a read after each. The
spools a crash left behind are swept by their process's lock, never their age
(the amendment on a node's session spools, below). At an ordinary pace every step answers in
milliseconds and every wait ends as soon as what it waits on has; these are
allowances at the floor. Rejected: reading no age at all on a stamp that
names a taker and leaving it to the taker's lease, since a reader of the row
that reads no lease (a relaunch deciding what to continue, a retirement's
thirty-second ticks) would then hold a stuck stamp with no bound at all; and
capping the end once from its own start, which a standby that stands after
the end began outlasts.

**An unanswered record is asked again whatever its key's age.** A capture
whose record was sent and never answered is asked again under its own
operation by the turn's end and by a relaunch, and one whose object's key
read past the window a record may name it in (`objstore.RecordWithin`,
twelve hours) was refused without asking (`Coordinator.settleCapture`). That
age was read on the asking node's clock, from an instant the spooling node's
clock minted, while the log decides the same window on its own deciding
clock (internal/executordomain's `ErrUploadStale`): an asker whose clock
stood ahead refused a record the log would still have taken, and the turn's
end then tombstoned the turn ahead of it, losing its last session. The record
is now always asked, and the log decides: it refuses one past its window on
its own clock, definitely, so the ask is not repeated, and the collector's
grace outlasts the window by more than twice the skew, so the object an ask
names is never collected under it. A reader holds a capture whose record
awaits its answer until the window and twice the skew have passed on its own
clock (`pastRecordWithin`, fourteen hours), past which the log refuses it on
any clock the engine holds. Refusing unasked past the window and the skew
was not taken instead: the ask costs one record's budget, and it is the
log's answer either way.

**A wait's settles are written in their row's room past it.** Every wait for a
capture settles what it can on the way (`Coordinator.awaitCapture`): a begun
capture whose taker is gone, from the turn's row on the log; a read standing
by whose taker is gone, withdrawn; and, on a relaunch, the followed job's
capture whose record went unanswered, asked again and stamped with the log's
answer. Each of those writes was held to the wait's own end, so a settle
decided in the last seconds of a relaunch's wait had at most those seconds
to cross, far less than a write of the row is given at the slowest crossing
a link is allowed; one the store took too late was not applied, and the
relaunch read a capture whose taker was gone as one still being kept and
started a session of its own beside it, or left a capture its ask had landed
unstamped on the row. The wait's patience now bounds its waiting, and the
reads and asks it makes, and nothing else: each settle write is this node's
own, given a record's budget and this node's crossing of the row past the
wait (`Coordinator.settleContext`), as every stamp made off a read is, and a
settle the store took is kept on the wait's copy where the row cannot be read
back after it (`standDownOnto` beside `stampOnto`). The settling functions
take no deadline of a caller's any more, so none can be held to a wait again.
An ask that leaves the record unanswered stamps nothing: the row already holds
that capture unresolved, which the row's rule leaves as it stands
(`Coordinator.settleCapture`). So a relaunch's frame can run past its wait
by one write's room, and only where the store is that slow to take
it; the record's ask itself stays within them. Giving the relaunch a longer
patience was not taken instead: the frame holds the seat and a slot of the
node's admission, and a write past the wait is this node's, bounded on its
own.

**Mid-upgrade.** Nothing on the wire, the run's row, the executor log or the
coordination store changes. An older node still starts a collection's keep
once its read has returned, still reads back an unanswered takeover's row
under its keep's context alone, and still leaves a take's two records one
grace. A newer host asked by an older node reads to the deadline the older
budget sets, so a take at its bound there leaves the second grace's thirty
seconds of its own time to its records. An older node still reads a begun
capture as abandoned past thirty-four and a half minutes on its own clock, so
in a mixed fleet it can still take over beside a capture still being kept, or
end a turn over one. A node of this build does neither beside a capture of
this build; beside an older taker's it can, since that capture's life is not
one its figures bound (an older taker starts a collection's keep only once
its read returns, behind a pause no sandbox bound holds, and reads back an
unanswered takeover with no deadline), until the fleet has rolled. An older node still refuses an unanswered record unasked
once its key reads past twelve hours on its own clock, and still cuts a
wait's settles at the wait's end, so there a settle the store is slow to take
is still lost to that wait, and the capture waited for as before.

**What it leaves.** A collection's upload now runs beside its read's
harness-end and pause records on the same link, each given its allowance as
though it had the link alone; keeps already share a node's link with other
turns' transfers, and the floor's model is per transfer throughout. Those
three also contend for the run's row: the keep's outcome stamp, the harness
end's record and the pause's record each read the row, decide and write it
whole, and one that loses the race to another reads and decides again, a
second change of the row no room counts. So at the floor any one of them can
be cut: the harness end left unrecorded, the pause's stamp missing, or the
capture left begun until a turn's end gives up on it. A taker
whose leases answer while its stamp never reaches the row is held by readers
for up to three hours and eleven minutes on their clocks, and by a turn's end
for its life from first sight and one more read of the row. A read standing
by beside a capture whose
record the log took but whose stamp never reached the row takes the place
after it at the end of its wait and uploads the session again, as one did by
the capture's age before; only a read of the log at the end of the wait
closes that. A keep's listing of the seat's runs reads every run of the
fleet (crewlet/crewlet#1369), so on a large fleet across a slow link it can
pass its room, and the seat's budget is then decided only at the record.

## Amendment: a save under way stays in sight on its run's record

A SAVE is a capture of a job still working, taken while its harness runs on:
not final, cut off, holding no note, reserved at zero and raised after its take
(above). Its place is reserved on the run's row after the newest, as
every capture's is. What the record could not say was where a save stood once
the job's final take overtook it: a collection, a stop's read or a loss's take
the final capture in the place after the save while the save may still be
uploading, and every stamp of that take replaces the row's newest capture,
which is the only section the turn's waits read. Where the final take is
refused, the save is the newest session of the job anybody kept; an end that
waited on the final alone recorded over it, and the save's record met the end's
tombstone.

So the run's row keeps a second capture section, `PendingRun.Saving`
(`session_saving`), from a save's reservation until it can no longer become the
turn's kept capture: it lands or is refused, or a capture at or past its place
is kept, so its landing could change nothing. Every stamp of the save's own
take moves it on there, whatever the newest place holds, and every stamp that
takes lets it go once that holds, decided on the newest capture as the stamp
found it (`stampOnto`, `savingAfter`), so a final take that overtakes a save a
build without the section settled in the newest place lets it go rather than
leave it in sight. A stamp the row holds in any section is answered taken
(`holdsStamp`), and a settle is written over a save's begun stamp in its own
section (`stillBegun`). The section is a READING, not a row invariant:
a build without it carries the key verbatim and writes it back, so a row may
hold a stale save, and nothing reads the field but through one rule
(`PendingRun.savingTake`, `PendingRun.saving`), which a walk of the module's
sources holds every reader to. A save is never final, holds no note and is
never stood by for, so the row's worst case carries a third capture short of
those fields, and `measuredOtherFields` and `measuredSpare` restate the row.

A save takes its place only while its job RUNS UNDECIDED
(`PendingStore.ReserveSave`, `licensesSave`), decided in one read-decide-write
of the row as it stands, on the clock of the node making the write: the run
running; the save of its current launch, by its job and its ordinal; nothing
of the job's end read and no refusal of its completion owed
(`PendingRun.jobOver`: a final capture of this launch as the newest or the kept
one, how its harness ended, its cap seen ending it, its box read back, a
collection whose read failed, or `LaunchRecord.RefusalOwed`, the job's
completion delivered, refused and acknowledged, which a refusal made before
any claim leaves alone on a running row); no loss marked; no ending decided,
owed or recorded; no capture or save of the turn that may still land
(`PendingRun.capturesMayLand`, over `SessionCapture.mayLand`: being kept, a
read standing by for it, or its record sent and not yet answered within the
window a record may name its object in), so one save of a run is in flight at
a time, fleet wide; and the save placed at the next place after the row's
newest. Every read that takes a job's final capture writes its decision on the
row before it reads the row for its place (the claim, a stop's ending, a loss),
so a final take either places itself after a reserved save or the save is
refused: a final take never loses its place to a save. A save's place, its save
point and its note cursor are read off ONE read of the row made before its
pack, so the place order is the content order: a capture placed since that
read moves the next place and refuses the save, which a save packed before a
final take could otherwise be placed after, taking the save point that final
take's own read placed after the job's end and counting calls it never held.

The license leaves one window, stated rather than closed: a save of a job whose
completion is announced but not yet recorded on its row as owed or read. Its
completion may be on its way to the seat's holder, claimed and handed back
before its collection heard from the box's host, or refused on a condition the
holder hands on to the seat's next holder (`SeatLost`, `Draining`), which no
write records. Such a save is an ordinary take of a job that may have ended:
the collection's final take is placed after it and supersedes it, and where
that take is not kept, the save is kept holding all the job did, told as a
session its run went on past. It over-tells, and never holds a call the session
did not see. Nothing of this build reserves a save yet, so no row holds one:
the section is read, kept and licensed here, a piece before any writer, so that
every node already reads it when a build that saves runs beside it. A build
before this one reads the newest capture alone, so every node runs this build
before any node runs a build that saves.

Every wait reads the save under way as it reads the newest capture. The turn's
end waits for every stamp still in flight (`captureInFlight` over
`stampInFlight`): the newest capture being kept or stood by for, else a save
being kept, so an end never records over a save that may still become the
turn's kept capture. A save a kept capture superseded is not waited for, by its
place and never its age: its landing could change nothing, since the record
keeps the newer capture and the log refuses a capture no newer than its row's.
A relaunch waits its wait for the same stamps (`relaunchWaitsOn`), and
its reason names the stamp it waited on by its own place, never the refused
final take that holds the newest place; a reader is undecided for a save still
being kept, or whose record awaits its answer, as for a capture
(`relaunchContinuity`, `captureAwaitsAnswer`), and reads no row again for it
(`RereadsRow`). Every pass of a wait settles a save whose taker is gone as it
settles the newest capture (`settleGone`): one coordination read per taker
lease the begun stamps name, and, where a taker is gone, one read of the
turn's row on the log for both, the stamp whose place it names landed and the
other refused. The turn's end asks again every unanswered record in sight, the
save's among them, and a relaunch those of the job it follows
(`settleUnresolved`). A read standing by takes over by the one age rule a begun
stamp is read by (`SessionCapture.keeping`), which the turn's waits read too.
An end a build without the section records still waits on the newest capture
alone, and asks only it again: beside a final take that overtook a save and was
refused, it can record over a save still uploading, or one whose record went
unanswered, and the save's record then meets the end's tombstone, the end saying
the turn went on past the session before it. A fleet reaches that only by
running a build that saves beside one older than this, which is why every node
runs this build first.

A relaunch waits only for a keep its decision depends on (`relaunchWaitsOn`,
`relaunchDependsOn`), where the turn's end waits for every one: a capture or a
save of the job it follows, and an earlier job's only while the turn has kept
nothing, since once one lands a relaunch no longer continues the session the
turn's first launch was handed. Beside a capture the turn kept, an earlier
job's keep changes nothing a relaunch decides (it continues only a capture of
the job it follows, and the turn has already kept one), and a wait for it held
the seat's frame and a slot of the node's admission for the whole wait
for nothing, and named it as the followed job's: the relaunch decides at once.
A reader follows the same rule, decided beside that keep rather than told the
relaunch is still waiting, and reads the row again where the rule says
(`RereadsRow`); where the turn has kept nothing, the reason names the earlier
job's keep as that, beside what came of the job the relaunch follows.

A capture the log took is settled landed whatever its taker's lease
(`settleGone`'s held arm). A host whose coordination store answers its leases
and not its runs, the two being other streams, keeps its taker lease while no
stamp of its own reaches the run's row, so a wait that settled by the lease
alone held the turn's end for the capture's whole budget beside a record the
log had taken, and a relaunch that followed a job lost with that host
(`launch_node_store_silent`) started without that capture. So a wait reads the
turn's row for a begun stamp naming a taker whose lease holds, or that the
store cannot say of, as well, and settles it landed where the row names its
place: the turn's end at most once every record's budget, thirty seconds on
its wait's own clock, never at its two-second tick (`heldRowPace`,
`beganUnderTaker`), about a hundred and forty-four linearizable reads for
one capture held its whole life from first sight, and about four hundred and
sixty over a standby's chain, the end's rounds alone reading the log; a
relaunch once, after its wait has run out, for a stamp of the job it follows
however long ago it began (`ContinuedCapture`,
`followedBegunUnderTaker`), since such a host holds its lease for as long as
its silence lasts and its job is found lost only once that silence has lasted
the host grace, which can put a keep under way when it began past a
capture's budget; and that read is held to a wait of its own as long as the
relaunch's, since the frame still holds the seat and a slot of the node's
admission, its settle's stamp given its own room past it as every settle a
wait decides is; and a read standing by for such a
capture once, when the capture has stood begun past its budget and before it
takes the place after it (`awaitStood`), keeping nothing and withdrawing where
the row names it, on the row's word alone (`settleGone`'s named stamps),
whether or not the run's record takes the landed settle, since by the age
alone it took the place, uploaded the session a second time beside the record
the log took, and both records landed, and decided on the row as the store
took the settle, a settle the store did not take read the capture back begun
and did the same. The standby's read is held to its own wait, so it never
moves the takeover past that wait's end; and where that wait runs out before
the capture is begun past its budget on the standby's clock, the standby
reads the turn's row on the log once there, held to one change of the record,
before it reads the run's row and decides (`waitedOut`), so a wait that ran
out first does not upload the session a second time either. The standby's
window counts that read and the settle it decides (`standbyWindow`, two
hours and thirty-one and a half minutes). The landed arm needs no fence: the row names the place
only once the log has taken the record, and nothing takes a record back; so a
lease the store cannot say of is admitted to it as a lease that holds is, for
the end's, the relaunch's and the standby's reads alike, since a store that
cannot answer is never a taker gone and never a reason to leave a capture the
log took begun. The refused arm stays a gone taker's, since a taker not read
gone may still send its record, and a stamp that names no taker is read by its
age alone, as before. This amends the taker amendment's rule that a wait
settles a begun capture only where its taker is gone; a capture the log has
not taken, still begun under a live lease when the relaunch's wait runs out,
is still being kept, as that amendment says. And it amends the standby
amendment's rule that a standby takes the place after a capture begun past
what a capture can take: it does so only where that one read does not find the
row naming the capture. A log that does not answer and a stamp that names no
taker leave the takeover to the capture's age, as before, so there the session
can still be uploaded twice.

A relaunch in its job's own box continues the copy that went on past a kept
capture (`copyWentPast`). A capture taken while its job still worked is a point
the job's session passed through: the harness went on in the box after it. So
where a person's answer or a replaced sign-in carries the round on in that box,
offered the box's copy of the same session, restoring the capture would replace
the copy, since such a capture is cut off, and roll the session back to where it
was taken. The launch passes the capture over, says so
(`executor_session_not_continued`, its outcome the box's copy), and continues
the copy, told what a copy cut off is told, which reads the job's record and its
carry and never the kept capture, so nothing the run is told rests on the
capture it passed over. A brief told by a reading of that capture rides the
continuation and is not handed, so the launch says so as it does where it
continues the kept capture without such a brief
(`executor_session_holdings_unused`, at the launch's site and from its one
place, `exactUnused`, naming the copy), once the copy is what it continues: where the copy is a session its job ran
out of context in, the run starts a session of its own, and
`executor_session_not_continued` alone says why. This amends the list of
where the reading's amendment says the line is logged. What decides it is the content, never an age or a place:
a final capture holds the ended job's whole session, so its copy went nowhere
past it, and it is restored there as anywhere, even where a standby's takeover
kept it older than a refused newest capture of its job. Only a capture the
run's record keeps as the one chosen, of the session the copy is, is passed
over, and only where the launch is offered the copy: a lost job's carry and a
refused request's offer none, and a launch's caller clears one its floor cannot
carry. A reader still says such a session continues in any box (`any_box`): the
capture is restored in any other, and in its job's own box the copy that went
past it is continued. Before this, such a capture, where it was its job's
newest, was restored over the copy; a capped collection whose end of its job
failed leaves one, landed and kept.

A relaunch continues the NEWEST CAPTURE ITS JOB KEPT, past newer captures of
that job (`followedCapture`). It continued only the job's newest capture, so a
job whose newest capture was refused, begun and never finished, or never
answered was relaunched without the older capture it kept, and lost everything
up to it: a newer capture went further than the kept one, and a run continued
from the older would have taken itself to know what the newer held. That no
longer holds, since every continuation is told the calls after its own save
point, read off what the capture holds where its archive is read for the
calls (the amendment on a cut-off session's archive) and off the save point
otherwise, and so is told every call its session did not see and counts none
of them held. The relaunch continues the kept capture past a newer one that
will not be kept (refused; begun past a capture's budget, by the one age rule,
`SessionCapture.keeping`; unanswered past the window a record may name its
object in), and past one still in flight once its wait for it has run out: it
still waits its wait for a newer capture or save of that job, asks one
whose record awaits its answer, and continues the newer one where it lands. The
asks come first, whatever else is in flight, and again where the wait ended
early, so an ask that lands a save beside a final take still being kept has the
wait's time to land in. The decision past the wait reads no age and no state
of the newer one. Each newer capture passed over is named once, as not kept and
why or as one that may still be kept and how, in `executor_session_not_continued`,
its outcome the capture continued (`passedOver`). Only where the job kept none
before a keep still under way does that keep leave the relaunch without one, as
before. A newer capture that lands between the relaunch's choice and its
launch moves the record's kept capture, or the turn's row, and the launch's own
check refuses the older one (`continueKept`): its briefs were built for it, so
the run starts a session of its own, or continues its box's copy in the job's
own box where it is offered it, never the newer capture. One that lands after
the launch changes nothing its session or its words rest on: the launch's own
captures are placed by its own kept captures and the capture it continued.
Where the record keeps the older capture and stamps the newer refused while
the row names the newer (a record its taker handed on, landed unseen by the
stamp), the relaunch continues neither, saying the log keeps the newer
(`ContinuityOf`); a reader holding a row that names the kept capture reads it
again wherever the record holds a newer capture of the job it did not keep
(`RereadsRow`, `unkeptNewer`), since the log may have taken that one, and a
newer one the log took ahead of its stamp is refused by the row's own naming.
A reader is still undecided while a newer capture or save of the job may land,
since the relaunch would wait for or ask it and continue it where it lands, and
is told the older one continues, in any box, beside one that never will. This
amends the taker amendment, whose relaunch that will not wait out a slow keep,
and whose keep still under way past the wait, now leave the relaunch without a
kept capture only where the job kept none before it (beside an older one, a
relaunch past a settled late landing still starts without it, the row naming
the newer); and the box copy amendment's list of when no kept capture is taken
up, which now reads: where the job kept none (its keep refused, or still under
way when the relaunch's wait ran out, with none kept before it), where the one
it kept was taken while it still worked (above), or where the company keeps no
session. Readers state the relaunch's own rule: a session whose newer
capture will never be kept is `continuable` in any box, where it was not, or
was only in a parked box, before. A lost job whose box was kept and whose final
take was refused is relaunched from the newest capture that job kept, rolled
back to it and told so, never from the copy the box holds: the lost path offers
no copy, by design.

A continuation of a capture taken while its job still worked is TOLD THAT ITS
RUN WENT ON (`prompts.SavedSession`), when it was taken, and that the session
ends where it was saved, not where the run stopped, in place of the words that
say only it was cut off (`InterruptedSession`). The probe answers it off the
capture (`runner.SessionBasis.WentOn`, a capture that is not final, and
`CapturedAt`, its stamp's instant), the names and shape the base's basis already
gives a session its turn went on past, so both say it alike, and the zero
value, on a basis that continues no kept capture, says nothing. Told only that
it was cut off, the run read the session's end as where its work stopped,
while its job went on in its box past it: so it is told too that any call
through the seat's tools after the save is told below, and that what else the
run did, the commands it ran and the files it read or changed, is in no record,
so it checks the working tree before doing any of it again. Such a capture is
always stamped cut off, as every capture taken before its job ended is, and
every capture a relaunch continues states a save point and so whether it is
final: its calls, leads and notes are told as a cut-off session's are, and only
its own words move. `Lost.Continuation` stays as it is: it says why the
run was cut off, which only a loss can say, and the rest is said once, by
`SavedSession`, for every relaunch from such a capture, a loss's, an answer's,
a send-back's or a replaced sign-in's alike; on the lost path the brief tells
the loss, then the save, then the calls. A call the lost run was making through
the seat's tools is on its run's record before it runs, and told as one whose
outcome is unknown where it never finished, so what may have run with no record
is the harness's own tools, which the working tree answers for. A lost job
continued from a final capture, cut off as it was lost, is told it was cut off,
as before.

A lost job's carry SAYS WHAT THE LOSS COST. The line that carries its round on
(`sandbox_run_lost_carried_on`) gives, in whole seconds to the instant the loss
was found (`JobLoss.At`), how long the job ran from its launch (`ran_s`,
`LaunchRecord.StartedAt`) and how long since the newest capture of its own
launch that was kept, final or not (`since_save_s`, `launchKept`): the work a
relaunch continuing that capture does again, which a relaunch now continuing
the newest capture its job kept, past newer ones, makes worth reading
(`lossSpans`). Each is left out where its instant is not known, never said
from no instant: a launch an older build recorded with no start, a kept
capture of an earlier launch, which says nothing of the job lost, or a stamp
or a verdict stating none. The instants are three machines' clocks, the
finder's, the clock of the node whose store write launched the job and the
capture's taker's, so each figure holds within twice
`objstore.AssumedClockSkew`; one below zero, which those clocks give, or the
final capture the loss's own take kept after the loss was found, is zero,
since no kept work was lost. A log line only: no record field, so no version
moves.

An older capture is restored as any capture is, with no `tar -m`: a carried
session keeps the modification times it was packed with (`tar -xzf`), and
every Claude Code run holds its box's own retention sweep off
(`cleanupPeriodDays`, `claudeRetentionDays`), so a session restored from a
capture older than its job's newest is never swept as stale, and setting every
time to the restore's would change every restore for no reason. OpenCode
imports an export and runs no sweep of its own.

## Amendment: a running executor's session is saved as it works

A job can work for hours, and a box lost with its host, or a
`direct` job gone with the container, unit or host its engine ran in, leaves
nothing to read: its
relaunch continued what the turn kept before the job began, or nothing, and
did all the job's work again. So the node driving a running job's box SAVES
ITS SESSION AS IT WORKS (`Coordinator.SaveRunning`, internal/sandbox's
save.go), a capture of a job still working, licensed as the amendment on a
save under way says, and the relaunch that follows the job continues its newest save,
told that its run went on past it. This amends that amendment: a build now
reserves saves. Every executor runner packs a save (`Runner.Saves`),
OpenCode's beside its run's own server ("an OpenCode session is saved as it
works, beside its own server", below).

THE POLL IS THE ONE TRIGGER. The waiter on the box's host reaches every
running box each interval already, so each box a poll finds running within its
turn's cap is offered (`WaiterOptions.Save`).
A call through the bridge would save only a run that makes one, a clock in
the box's script would pack with nobody to take the pack, and a signal is a
second way into the box: each would touch the box beside its waiter. The offer
DECIDES FROM MEMORY AND NEVER BLOCKS, since the poll is the box's keepalive:
saves on, the runner saving, the node not draining, no save of the turn under
way on the node, the save due, and one of four slots a node
(`maxConcurrentSaves`) taken without waiting. The save is a task on the
coordinator's lifetime, counted for its stop, and carries THE WAITER'S LIFE,
which ends when the waiter stops, so a poll that returns after its waiter
stopped offers nothing.

DUE-NESS IS COUNTED ON ONE CLOCK, the offering node's: the interval
(`turn_engine.conversation_session.save_interval_seconds`, 600 s by default,
300 to 3600, refused outside, read off this node's revision since a cadence
binds only the node driving the box) after the last attempt, and only once
the job's event stream grew past the length this node's last landed save
read (`Runner.Streamed`). The classes are what the memory does with an
outcome: a failure doubles the wait, up to the larger of the interval and an
hour; what was not there (the box, the license, the taker, the place, the
box's room to pack) moves the clock without doubling; a stream that has not moved or a paced seat
waits; a landing resets it and remembers the stream's length. AT FIRST SIGHT,
after a restart, the last save is taken as the
later of the launch's start and that launch's newest capture, instants other
machines stamped, clamped into the last interval: as the amendment on a
capture's life says, an age read across machines decides only a wait one
interval bounds, so the first save falls due within an interval of first
sight whatever any clock says, and is paced as any other.

THE SLOTS HOLD THROUGH THE KEEP: a save gives its slot back only once its
upload has ended, so four bound a node's packs, scrubs, spools (about 192 MiB
at the sealed bound, within the spool room) and uploads together, for the waiter and the drain.
A job's final captures take none, as before. THE BOX PHASE IS THE WAITER'S and
ends at the pack: the attach, the stream's read and the pack end with the
waiter's life, and a save cut there stamps nothing and is released from its box
(`Runner.ReleaseSave`), where an untaken save waited for the clear before the
box's next run, which periodic saves would make routine. Past the pack the save
is the coordinator's: its take context starts then, so its scrub has its
whole budget, and the scrub's values are read then, so a token the poll
renewed during the pack is taken out.

A save runs CHEAPEST FIRST on ONE READ OF THE RUN'S ROW, made before the pack:
the license's pre-check, a settle of a save of the turn its taker left begun
or unanswered, the fleet's policy, the seat's allowance, what the box was
handed and the taker lease, and only then the pack. Its place and cursor,
and what raises its save point, come from that read. THE POINT IS RAISED
AFTER THE TAKE: the ledger is charged before the stamp's instant, the place
reserved at zero, and the point raised once the take's count states the
session's newest compaction, before the keep, whose outcome states it
("an OpenCode session is saved as it works, beside its own server", below);
a zero tells more, never less.

THE SEAT'S ALLOWANCE. The collector cannot tell a superseded save's object
from an upload in flight, so it keeps it a day and more, and a seat's saves
could fill the shared bucket in hours. So `max_bytes_per_seat` is also the
allowance of a seat's saves, and a seat holds up to twice it. The whole
company has to agree on it, so it is one value per seat in the coordination
store's uploads bucket (`coord.SaveAllowances`, key class `save.<seat>`), aged
out by the bucket's thirty hours with no sweep and no retirement step. Each
entry is held `saveLedgerHold`, 29 h 2.5 min: the take's life before its
object is stored, the collector's grace and two of its passes, and twice the
assumed clock skew, since its end is written on one node's clock and read on
another's. THE CHARGE COMES BEFORE THE STAMP'S INSTANT, by compare-and-swap,
sized from the packed archive sealed with an eighth or 64 KiB more for what
the scrub changes, keyed by an entry id so an answer lost and asked again
counts once: a crash, a lost answer or a keep cut at the stop can then only
over-count, the safe direction for a bound every artifact's bucket shares,
and nothing waits between the instant and the reservation that a capture's
life does not count. A spool that seals past what was counted is refused
before its keep, so a landed save is never under-counted. Beyond the interval
a periodic save is PACED: spaced by the hold times this launch's last save's
size over the room left, or before its first the size of the kept capture the
launch continued (`LaunchRecord.Continued`), so at the defaults a session past
about 0.37 MiB sealed is saved about every 27 minutes per sealed MiB, doubling
once the seat has saved past the hold and its room settles near half; a
session growing 1.2 to 1.8 MiB sealed an hour is saved every 0.54 to 0.81
times its age, 1.1 to 1.6 in a busy seat. Paced by nothing, a relaunch
continuing a session at the bound would pack and charge it an interval in and
hold most of the seat's allowance for the hold; paced by the capture it
continued, a 2 MiB one is first saved about 54 minutes in, and a crash before
then continues the kept copy. A first save no row sizes (the box's own copy,
the conversation's base, a fresh session) is paced again at its pack's size
before its charge: released uncharged where its pace has not come, its size
kept so the next is paced unpacked. Never again past an estimate, which a
growing session would outrun pack after pack. Releasing a superseded save's
object without waiting out the collector is the root fix
(crewlet/crewlet#1469).

A DRAINING NODE SAVES EACH DIRECT JOB IT LAUNCHED whose session moved
(`Coordinator.SaveHosted`, from `Engine.Drain`), beside its wait for turns,
for at most 30 s (`drainSaveWait`): the stop a drain comes before usually
takes such a job with it. A
listing rather than the offers, so a box whose poll is stuck is not missed;
most stale first, under the same slots; a save of the turn under way counts;
charged, never paced; and from the drain's first instant no offer starts a
save. Not a `container` job, which survives an engine's restart. The drain's end bounds each save's box phase,
and its take once the take's own context is made, never that context's
parent. The drain joins these saves before it returns, so a pod's grace is
the longer of its longest turn and 30 s, plus 150 s; a shorter one cuts the
takes, and the run continues its newest periodic save.

A reader of a turn in progress whose row names a newer capture is still told
its runs have kept its session again (`executorread.LiveKept`): that is true
of a save, and a reader of the row reads no run record, so whether a run
saved it is nothing it can say.

What is left, stated rather than closed: the pacing above, which only the
root fix shortens; slots taken in the order offers arrive, not most stale
first, so a due save can wait a poll or more (`late_s`); the ledger over-counts every save counted that never
uploaded until its entry lapses, and the save its row names; a save whose
scrub grows it past its margin is refused; a pod grace shorter than the
drain's takes cuts them; a pack's staging on a box disk near full can fail the
harness's own write; a release in a box that stops answering as the
coordinator stops can hold that stop up to its pack's wait; and a
container's save time and the scrub's size change are judgments until the
saved line (`took_s`, `counted` against `bytes`) measures them.

## Amendment: an OpenCode session is counted as OpenCode counts it

OpenCode decides before every request, a continued run's first among them,
whether to compact the session first, by a rule of its own: its count of the
conversation from its last counted reply (that reply's input, cache reads and
writes, output and reasoning, as its provider reported them, the reply
qualifying where those three prompt parts are above zero and it ended on no
error) and its estimate of every message after it, the reply's own tool
results among them, at a rounded quarter of each one's UTF-16 length,
restarted at its newest completed compaction. Read in the shipped code of
2.3.2, and measured on it: every boundary its point predicts split there, one
token apart.

**The count.** The engine bounds that count from above off the session's
export, at both places one is read whole: the take's meter, over the archive
it keeps, and the collection's read of the export the job's script left as the
job ended, inside the read's one wait (`internal/sandbox/executor`'s
`openCodeCountReading`, in one walk of the export on `internal/jsonscan`
beside the floor's last-request figure, holding no string of it). Every
message after the anchor is charged at a quarter of its bytes as the export
holds it, rounded up, and the anchor's own tool results at the spans of their
answers and failures: OpenCode renders a message from the strings it stores
and at most a fixed template, whose bytes the message's own keys exceed, and a
string's UTF-8 bytes are never fewer than its UTF-16 units, so the charge is
never under OpenCode's estimate, and over it by the export's keys and
indentation (about 95 tokens a message on the committed real exports). What
the bytes cannot bound is a mark: `unanchored` (no counted reply since the
session's start or its newest completed compaction, where OpenCode estimates
the system prompt and the tools, which no export holds), `pdf` and `media` (a
file after the reply, charged by what it is), `unread` (an export not read
whole, a provider or a compaction's id or status that does not read, a job
whose harness did not end its own run, whose export may lack what its copy
holds) and `release`.

**The set.** The count is read only for a session that release wrote
(`openCodeCountRead`): a set, never a range, since any release may move a rule
the count rests on (earlier OpenCode 2 releases moved the share of the window
held back and how a model's limits combine), and a release
joins it only once it has been read and probed the same way, the procedure
the set's own doc states.

**The identity, and the provider beside the count.** Where an OpenCode session
restarts is the id of its newest compaction message, whatever its status, the
transcript's own restart rule (`openCodeRestart`), so a job whose compaction
failed reads as restarted while its count goes on from its last reply, as
OpenCode's does; it rides `ResumedCount.Restart`, which an OpenCode message id
(thirty bytes) fits. The provider of the anchor's reply is recorded beside the
count, never in it (`count_provider` on the capture's stamp and on the job's
record, written and cleared with the count, never carried): `ResumedCount` is
closed, and an older build that knows it would drop a field added to it on its
every write. The provider is held to sixty-four bytes of printable ASCII but
JSON's escapes, so the row's model of its widest record is exact, and one that
is not is no count.

**What it is read for now.** The size of the session's next request: an
OpenCode continuation's launch floor is the count's bound where it holds one,
and its last request's input otherwise. Its first request is not judged by
this amendment but by the next, in its box: the runner's restart values and
verdicts read a count only for a harness it judges (`firstRequest.judges`),
which that amendment teaches OpenCode. The
OpenCode collection's read is held inside the box read's budget as a Claude
Code one's is (`BoxReadBudget`, `asksMainThread`).

**Mid-upgrade.** `count_provider` is additive on the job's record and the
capture's stamp, carried in `Extra` by an older build. `unanchored` reads on an
older build as a mark it does not know, a session that may compact. An older
host's reply names no provider and an OpenCode collection's read on an older
host reads no count, each leaving the floor on the last request's input. An
older asker computed `BoxReadBudget` for an OpenCode collection without the
read's share, so a newer host's read spends up to 27 s of it, as the
Claude Code read did once. No state-log record, no `RecordVersion`, no
`coord.ProtocolVersion` change.

**What it leaves.** The overflow route: a server whose real count passes its
window while OpenCode's estimate stays under its point, or one that cuts the
prompt and says nothing. A session one release wrote and another continues is
counted by the writer's rules, which the box's own may differ from.

## Amendment: an OpenCode continuation's first request is judged in its box

OpenCode compacts a session before a request, a continued run's first among
them, where its count of the session with the brief is at least its point: the
run's model's limit less a buffer. The amendment that counts an OpenCode
session (above) bounds that count from above, but the point is not the
engine's to pin as Claude Code's window is: it is read off the model catalog
and the configuration documents of the box the run is handed, and the
relaunch's briefs are written before that box exists. So the verdict moves to
the launch.

**The runner hands both tellings.** For an OpenCode session whose count has a
bound, `firstRequest.byBox` settles the verdict `byBox`: both tellings are
built, the held one is picked, and the one told as a session that may be a
summary rides beside it (`SessionContinuation.Summarized`,
`Continuation.Summarized`), with the count and its reply's provider the launch
judges by. A count with no bound is `compactsWhatever`: the summarized telling
alone, and no box is read. `firstRequest.judges` now holds for OpenCode too,
so the restart values (`restartedInJob`, `restarted`) read an OpenCode count's
restarts as they read Claude Code's. Claude Code's verdicts, tellings and
values do not move.

**The launch reads the box's point** (`PointReader`, the executor's
`Runner.ReadBoxPoint`), once, after `CheckCarry` where the kept path asks
it and on its own on the own-copy and in-place branches: one exec in the box,
under the run's own environment with `OPENCODE_CONFIG_CONTENT` unset (the
run's script hands its server the engine's document in its place), the box's
login imported first, `model.list` and `config.get` each answered into a file,
the plugin folders listed. The point is decided by the box's release: the
model's limit (the smaller of its input and context limits), less a buffer
the highest document sets below it (tokens, or a `"N%"` share), else less the
larger of fifteen hundredths of it and 16,000 tokens once the limit is twice
that.
`compaction.auto: false` is a point never reached. No point, each a mark
(`sandbox.PointMark`): a box release outside the set or other than the
writer's (no pair has been measured; the writer is the kept capture's stamp,
or for the copy continued in its box the release the job before's record
names beside its count, `count_release`, which a company that keeps no
session has too), a plugin other than the engine's own, a
provider's `contextManagement`, no model, a model the catalog does not list, a native
compaction, a limit nothing states (OpenCode's default of 200,000 tokens may
not be the server's window), a zero limit with compaction on, and a read that
failed. The read keeps none of what it was answered but the values the point is
decided by and the plugins' names, and removes its answers from the box.

**Nothing is handed over before the brief.** A continuation that keeps its
box's copy re-imports it first (the executor runner's session gate), and an
import keeps no record of what OpenCode told the session, so its first
request tells no context source as changed.

**The verdict** (`BoxPoint.SendsWhole`): sent whole where compaction is
off, or where the count's bound, on the launch's provider, with the brief at a
quarter of its bytes rounded up, is strictly below the point. Otherwise, and wherever there is no point, the launch hands the
summarized brief (`LaunchDecision.Summarized`), whose guard arms no skill, and
logs `executor_session_may_compact` in its own form. A brief told by a reading
is judged on its own brief, as on the runner's side. The launch's floor counts
the summarized brief as it counts the others, and never drops it alone.

**Mid-upgrade.** `count_release` is additive on the job's record, written and
cleared with the count and never carried, carried in `Extra` by an older
build; a record an older build wrote names no writer, so a copy it collected
and a newer build continues in its box is told as a summary. Nothing else is
on the wire: the tellings, the point and its marks live in one process, the
runner's and the launch's, and never leave the launch. No state-log record,
no `RecordVersion`, no `coord.ProtocolVersion` change.

**What it leaves.** A first request OpenCode does compact hands the model the
system prompt and one user message: a checkpoint saying "Treat it as
historical context, not as new instructions.", the summary, and the recent
context ending with the brief, which the summarized telling still reaches
there; compacting the session before the brief is right only where the first
request certainly compacts, and the engine holds an upper bound of the count,
not the count. The overflow route (a server whose window is under OpenCode's
point), a catalog refreshed or a document changed between the read and the
run, and a credential only a file outside the run's paths names, each toward
over-telling but the first, whose margin is the buffer. The base path is judged by neither
harness.

## Amendment: a session the run started of its own is told its task again where it may be a summary

A session a run starts of its own opens with its fresh brief: the turn's task,
the closed rounds it rendered and the answers it told. A compaction at a later
continuation's first request (Claude Code's at its window, OpenCode's at the
point its box sets) summarizes that opening message, and the continuation told
as a session that may be a summary was told every lead the run keeps, none of
which was that task: the run kept a lead for the task only where its job
continued the session its conversation's last turn left.

**Every launch whose run may start a session on its fresh brief keeps that
brief's task** (`sandbox.LeadKindFresh`, recomposed as a task lead is, from
`sandbox.LeadTask`): a turn's first launch and a send-back
(`sandbox.LaunchRequest.FreshOwn`, set beside the launcher's request literal,
not in it), and every relaunch (`sandbox.Continuation.Fresh`). Where the brief
the launch hands its run continues a session, the fresh brief's leads are kept
beside its own (`sandbox.BriefLeads.Fresh`), since its box starts a session of
its own on the fresh brief where its restore fails; kept unmarked, since the
brief the run read did not tell them, and a lead both briefs open with is kept
once, as the handed brief's.

**Told only where the session may be a summary**, as every kept lead is there:
a session cut off, or one known to hold its job's brief, holds its opening
message, so `sandbox.LaunchRecord.ToldLeads` does not change. It is told as the
task this work was started on (`prompts.FreshTaskAgain`), with no turn boundary
and nothing of a box, the closed rounds its brief rendered and the answers it
told nested under it, each once, as under a task lead.

**The turn's ask is told once a section** (`runner.oneAsk`): every launch keeps
a fresh lead, and a base continuation a task lead beside it, each rendering the
ask and its closed rounds' ledger, so told each, a summary would be handed that
ledger as many times as the turn launched. A task lead that opens here tells
it, with where the turn began; else the newest fresh lead that opens here,
whose brief rendered the most closed rounds.

**Mid-upgrade.** A lead is a sealed value in the run's leads part, read by its
kind: an older build reads a fresh lead as a kind it does not know and tells it
as a message that cannot be read here. No record, row field, `RecordVersion` or
`coord.ProtocolVersion` changes; each fresh lead weighs a few hundred bytes of
the leads part's own share (`sandbox.MaxKeptLeadBytes`).

## Amendment: a late call reaches the job working as it lands, and a continuation of the session that took it is not told it again

A late call, and a call its own job's harness gave up on, were told only by the
continuations built after they landed: the job working on the round as one
landed ran on, its session showing the call with no outcome, timed out or still
running, and could make it again, the duplicate the late words exist to
prevent.

**Handed to the working job at its next request.** The run's record carries an
ITEM for that job (`sandbox.RunNote.LateCall`, [ADR-0067](0067-a-note-to-an-executor-waits-sealed-on-its-record.md)):
a copy of the call as the log listed it, held to an ordinary call's share
(`sandbox.itemCall`), with the round that numbered it. The store files it in the
writes that decide the call is late or nobody waited for it, and nowhere else
(`sandbox.CoordStore.fileLateItems`): the append that lists a late call, for the
job the log is open to, and a call its own job's harness gave up on, for that
job once its spend is written; and the opening of a job's log that carries the
round on, for every call it walks past the read the relaunch's briefs were built
from, late already or not, for the job it opens. The log's job, never the
caller's: the round runs on the job the log's own compare-and-swap read. Pinned
to the row naming that job launching or running, its ending undecided; once a
call (`call:<job>:<number>`); at most `sandbox.MaxJobItems` a job. A filing
refused or failed fails neither write, and every continuation still tells the
call by its mark, so every late call still reaches somebody: a call landing
after the read a job's brief was built from is walked by that job's opening
where it lands before it, and filed for the log's job where it lands after.

**In the late words, as statements, the outcome first** (`prompts.LateCallArrived`):
that the run stopped waiting for the call and it finished since, that it reached
the run while the work was under way and is no new request, that the session may
show it without its outcome or an earlier message may have told it, that it is
one call whose outcome stands, a success having run and a failure having taken
no effect; for a successful `submit_work`, that it is not the work's submission;
and that where it reached a run finishing, the answer it gives next is the
run's report.

**Claude Code at a tool batch, never at its finish; OpenCode at its next step.**
A finishing job makes no more calls, so the duplicate an item prevents cannot
happen in it, and the review judges the run on its report, which an item at
the finish would replace with the answer after it.

**A continuation of the session that took it is not told it again.** Where the
job's own session is known to hold an item's call (OpenCode's plugin confirmed
it delivered, `sandbox.NoteTake.Confirms`; the job's main thread shows its hook
took it after its last restart, `sandbox.HookSettle.Held`), the write that
records the read records a TELLING on the run's record (`sandbox.RunSteer.Tellings`):
the round, the call's place in its log, the job, and the write's OWN new place
among the turn's reads, never the item's. Nothing else records one: not a
hook's take, whose answer may never reach its harness, not the review's, not a
brief. A telling outlives its item, goes with its round, and is held to
`sandbox.MaxCallTellings`, one a call at its highest place. A continuation credits
it (`runner.heldByTelling`) only to a session not cut off, not restarted in its
job and not a possible summary, whose capture's note cursor, as stamped, is at
or past the telling's place, and whose briefs counted no read of the call
without listing it, and never one a hook's step recorded
(`sandbox.CallTelling.Hook`) to a box whose release's resume this build did not
read, which may not rebuild what the hook handed the session (the briefs for
such a box, `runner.UnheldTelling`, are built wherever that tells more); the
copy in a job's box whose harness ended its own run credits every telling of
the log's round on the record; a fresh run credits none. Credited, the call is held as a call at the save point is. Sound because:

1. A telling is written only where the job's own session is known to hold the
   call.
2. Its place is the recording write's, and a capture's cursor is read off the
   record, whose count only grows: a capture read before that write is below
   it, and one read after it is of the job's session or of a later job's.
3. A later job's brief accounts for the call: it credited a telling to the
   session it continued, told the call whole, or counted it, which the launch
   records APART from the reads it owes (`sandbox.BriefTold.LateOmitted`) and
   which credits no telling to that session.
4. One call is handed to one job: an item is filed only by the write that
   appends its call or opens a job's log, for a job launching or running, and a
   relaunch's briefs read the log only once the collection has claimed the job
   before, so the opening carrying the round on never walks a call that job was
   handed. Were it handed to two all the same, the telling keeps the higher
   place, since the lower would credit a capture of the later session read
   between the two.

Apart, because a call the log drops from its middle keeps no mark: recorded
among the owed reads, a dropped late call was counted twice in one brief, as an
owed read dropped and as a late call dropped. Every other continuation tells the
call, under words that say an earlier message may have told it: a cursor
stopped lower, a session cut off or possibly compacting, a telling past the
bound, a hook's telling in a box whose release's resume was not read.

**What it does not reach.** A Claude Code job finishing as a call lands is not
handed it, so its report may say a call failed that ran, which its reviewer
reads beside the run's tool log. OpenCode can be handed an item after its final
step, since nothing tells its plugin whether a step will call a tool: the run
then goes on, and its report is its answer after the item, which the item's
words say. A call of a round a send-back replaced, and one that lands after its
round's collection read the log and before the next round's opening, are told
to nobody, as before. A job parked, claimed or past its ending, a job's
eleventh call, and a filing that failed are left to the continuations. An
OpenCode session that compacts holds an item, and a telling credited to it, as
it holds every call a capture holds: its compaction is not judged yet.

**Mid-upgrade.** Additive on the run's record ([ADR-0067](0067-a-note-to-an-executor-waits-sealed-on-its-record.md)'s
amendment on the item); nothing of the item or the telling is in a state-log
record. An older build's appends and openings file none, its readers hand an
item to nobody and its confirmations record no telling, so the call is told by
the continuations alone, as before; its new-round reset keeps the tellings as
keys it carries, which this build reads for the log's round alone. One case
tells less: an older build's brief records no late call it counted without
listing (`sandbox.BriefTold.LateOmitted`), nor does its capture
(`bridge_late_omitted`), so a continuation by this build of a session holding
only that count of a call, such as a fresh run's whose brief the read cap
thinned, can be credited a telling of the call recorded before it, and is not
told the call. No compatibility is owed to an unreleased build, so this is
stated and not bridged.

## Amendment: an OpenCode session is saved as it works, beside its own server

**OpenCode's runner saves** (`OpenCode.PackSave`, internal/sandbox/executor):
the on-demand pack's own export, made by a private OpenCode process
(`--standalone`) beside the run's server and never through it, since that
server's address and password are the run's script's alone. Every release
measured opens its database in WAL mode with a five-second busy timeout, and on
2.3.2 a standalone probe and export complete beside a server writing small
session rows, 7 to 8 saves in 30 s on a loaded 2-core slot with none failed on
either side (beside one stopped mid-step or killed, untested on 2.3.2). A save
holds the session's settled messages: the step in flight is absent, and its
calls lie past any point the save states. Every standalone call runs with the
models fetch and the updater off, so a save reaches nothing off the box.

**Bounded by the export, never the database.** After the export and before
gzip, every pack made on demand requires the export's bytes in 1 to
`heldPlainMax`, what a kept session's scrub reads, and refuses one past it or
empty, saying which and moving nothing. The database holds every session in
the box, so it is an estimate alone.

**One pack at a time in a node's own memory, and room for each save.** An
export peaks at up to about ten times its session once the session holds a
character past Latin-1, and a node takes four saves at once. Either bound alone
fails: a room check passes several saves that read one room at once, and a
slot lets one export take a container past its limit. So every pack made in a
box that shares the node's memory (a `direct` box), a save's or a take's on
demand, a final one included, takes the node's one pack slot
(`Coordinator.sharedPack`); and every OpenCode save first reads its box's room,
its memory domain's limit less what its kernel cannot reclaim, capped by
`MemAvailable`, and waits one interval, never doubled, while that room is short
of 12 x (database + WAL) + 128 MiB (`sandbox.ErrSaveNoRoom`). A room not read
refuses nothing. A final pack checks no room: refused, it would lose the job's
newest state on an estimate.

**Saved only on a release measured**: the set OpenCode's count is read on
(`openCodeCountRead`). A save of a session whose record names another release
is refused before its pack. Adding a release measures its saves too: a server
importing at size beside standalone probes and exports, a standalone call
beside a proxy that never answers, the export's peak within the need, and an
export holding a compaction, imported, its newest compaction's identity
compared before and after.

**A capture no read places is reserved at zero, and raised after its take.**
This amends the amendment placing a capture whose harness did not end its own
run, and the save amendment's order. The ledger is charged before the stamp's
instant, the place reserved at zero (`placeAtZero`), and once the take's count
states the session's newest compaction the point is raised (`placeAtNewest`),
before the keep, to the newest save point a held capture states whose own
newest compaction is that one, both identities read and equal: the launch's own
newest landed capture, or the capture the launch continued, which the launch
records with its command (`LaunchRecord.Continued`), read only where the launch
is the job before's and its box took that session up. The keep's outcome stamp
carries the raised point; a standby's takeover, reserved after its own take,
states that take's; a log's settle lands a reservation as it stands, at zero.
The raise stands on the session only growing between two captures with one
newest compaction, so it is gated where that is read: an OpenCode count names
its compaction only on a release in the set above, and a Claude Code capture
is raised only where its archive's release is one whose resume this build read
(`claudeResumeRead`), on which a withdrawal of a message reaches only the
request attempt in flight, whose calls lie past the point. Outside, a capture
stays at zero. A relaunch tells, and asks a reading of, only the calls past
the point; the reading runs at the relaunch (`Coordinator.HoldingsOf`), never
at the save. Reserved or raised, the capture states every late call its
launch's brief counted without listing it (`LaunchRecord.LateOmittedCalls`),
whatever its place: a telling of a late call is credited by the capture's note
cursor, never its save point (`runner.heldByTelling`), so a session whose brief
only counted the call is credited none at any point.

**The drain saves `direct` OpenCode jobs** with no code of its own: it takes
every running job whose runner saves, one pack after another.

**No record version or protocol moves.** `LaunchRecord.Continued` is a row
field of at most 86 bytes, which an older build's write keeps in
`LaunchRecord.Extra`; no stamp field is added, and the save reads the room
with a script of its own, so a job any build launched is saved. A node without the identity
check of a continued capture (crewlet/crewlet#1538) beside a node of this build
carries a raised OpenCode point across a compaction and tells from it, so a
fleet takes a build with that check before this one.

What is left, stated rather than closed: the room counts every session in the
box's database, so a job whose database holds much besides its session waits
where its room would hold the export; a final pack on demand checks no room, so
one whose export alone exceeds it can take its box or the engine past its
limit; the run's own end export runs in its server, which may peak alike, and
takes no slot; on 2.3.2 a standalone call writes the database file
(measured); a relaunch from an OpenCode save no
reading answers is told every call of its round past the save's point; a save
stays at zero, and tells more, where its launch continued the box's own copy in
place, the conversation's base or no kept capture of its round, where its box
had not said which session it started by the save's row read, where its
session compacted since, or where the log's settle landed it before its
outcome; a release outside the set takes no save until a probe adds it; a pack
the exec's two minutes kill leaves its staging until the clear before the next
run; a save holds settled messages only; a box whose memory domain cannot be
read is saved unchecked, one pack at a time; a save's staging holds the export,
up to what its room admits, and its gzip until its take, and an export past the
bound is written whole before it is measured and removed; and whether every
release keeps a compaction message's id across an import is measured as each
release is added, one that does not raising no continued save's point.

## Amendment: a cut-off OpenCode session is read by content, held to its run and its class

The reading of a cut-off session's archive (above) read Claude Code's alone,
by the id the harness gives each call. OpenCode gives the bridge none
(`sandbox.BridgeCall.CallID` is empty on every OpenCode call), so a relaunch
continuing a cut-off OpenCode capture was told every call after its save
point. It is now read by what each call was.

**The ask names a call by its place in the log** (the runner's `callKey`: an
OpenCode call's `Appended` in decimal, valid beside its admission place and
never for a call a person's approval held), and carries what a content
reading reads: each call's tool, digest, outcome, places and arguments' key
(`internal/tools/toolargs`' `Key` of the text the log keeps, the log's own rule
on both sides), whether a person's approval held it, and the calls the log
does not list (`HoldingsAsk.Unlisted`: those in flight, and the tally of those
it dropped, which already holds every late one). The reader validates the ask
by the same predicate read back, and refuses one past the log's own bounds.

**A reading by content** (`sandbox.HoldingsByContent`) answers, per class of
calls alike in tool, arguments, failure and output (`sandbox.CallClass`), how
many calls of the pool are of it, how many of the run's live parts it counted,
and whether a call the log cannot state may be of it; `Holds` holds an asked
call only as a member of a class whose every call has a part of its own and
no such call may be of it. The pool is every call of the ask but a held one,
a call held at the save point included, since a save of a job's running
session holds the run's own calls. The parts counted lie after the run's start
(the capture's record of where its run began, which a capture keeps) and after
the newest compaction, are the engine's own tool's (`crewlet_<tool>`), in
exactly the shape OpenCode hands its model verbatim, with exactly the log's
output: whole, or of its size and all the opening the log kept, never a text
the take's scrub rewrote; a completed part's empty or blank text never counts,
nor a failure's message other than its output exactly. A part two classes
match counts for neither. The held classes' parts must lie in steps the log's
order explains. Every rule, its reason and the argument that no call is held
whose outcome the continued model is not handed (but on the text-equality
standard, above) are the executor's package doc's "A session's calls
are read by what they were"; the executor's `contentVerdict` takes the verdict
as a pure function of the walk's facts and the ask.

**One walk.** The reading is a visitor of the executor's one export walk, on
the scanner, as the take's meter is: it holds no message or value whole, reads
each member it asks for once, as a word, a capture of at most 64 KiB, a frame
with its key facts, or a text's digest (its size, its sum and the sums of the
openings a cut class keeps), and decides each item and message at its end, so a
step whose type follows its content reads as one written type first. No message
is past a bound but the plaintext's. Measured on two dedicated cores
(`BenchmarkAnOpenCodeReadingAtItsBounds`, three runs): every case holds at most
2 MiB live; one answer of 64 MiB reads in 143 to 155 ms; 255 MiB of ordinary
steps in 3.5 to 4.7 s, of `{"type":"idle"}` messages in 5.9 to 6.1 s and of one
step's `0` items, the slowest, in 10.7 to 11.0 s, inside the 30 s wait; beside a
person's read of a 64 MiB export, which holds 18 to 19 MiB on the same walk,
the two hold 18 to 19 MiB.

**The releases.** A session is read only where OpenCode 2.3.2 wrote it
(`openCodeHoldingsReleases`, a set and never a range, compared whole): it was
driven, writer and box, through the engine's own
bridge and render, and every part these rules count, on the export as the take
keeps it, reached the continuation's first request exactly as the log recorded
the call, on both of its model routes. The brief told by a reading goes only
to a box of the writer's release whose first request sends the session whole
(the box's verdict, above), and no other MCP server may join to the engine's
prefix (the run's script refuses one).

**What a take's reading must read.** A take that places a cut-off capture by a
reading reads the export as kept, the scrub's output, never the box's bytes:
the cut-class rule passes a text holding the scrub's marker, which is sound
only over the text the continuation imports. A carried save point stands only
where no compaction followed it (`sandbox.RestartedSince`), and a save's point
never marks a call as made before the run.

**Mid-upgrade.** Nothing reaches the wire, a record, a row or the coordination
store: the ask, the kinds, the argument text a resumed run carries and the
read-back of a cut opening are in-process. An older node relaunching the turn
tells by the save point, as before.

**What it leaves.** Toward over-telling: a session continued by another
release (no pair is read); a step's `completed` written as a number literal
past 64 KiB, which no writer writes; Code Mode's calls, a subagent's, a
truncated or cut-argument call; and a run call identical to an
earlier job's call of the round, since no record states where the cut-off
job's brief read the log. Toward a false hold, each on the text-equality
standard: a refusal in OpenCode's own words equal to a logged failure alike in
tool and arguments; a box that rewrites the engine's record of where the run
began, or gives two messages of the export one id, the import keeping one of
them, since the reading reads no message's id but the start's; a tool whose
schema goes from open to closed between two jobs of the round; a truncating
server or the overflow route past the buffer; and a plugin loaded in the
writer's box that registers a tool named `crewlet_<tool>` and answers a call of
it with the very output a logged call returned, since nothing records the
plugins the writer's box loaded.

## Amendment: the round's record marks a late call

A late call (`sandbox.BridgeCall.Late`) was marked only on the runner's own view
of a call, which the round's record does not keep, so the round's reviewer read
it as a call of the run under review and judged that run on a call an earlier
run of the work made.

**The mark is the record's.** It is `ledger.Call.Late`, which the round's record
(`turn.Work.Calls`) and a parked turn's closed rounds carry, and which every
reader of the runner's view reads unchanged. The reviewer's log ends a late
call's line with words saying an earlier run of the task, replaced before the
call finished, made it once, and that an earlier round may list the same call
with its outcome unknown, which is no second call (`prompts.ReviewLateCall`),
put by the call's place in the record, so a name the log skips moves them onto
no other line. The line itself, which the item and the briefs draw, is
unchanged.

**The delivery checks stay turn-wide.** The submission's citations, `turn.Check`
and `turn.OverrideDone` count a late call as the turn's, as they count a closed
round's: what it delivered reached somebody whichever run made it, and what one
of them refuses another must not accept, so a citation check narrowed to the
round's own calls would refuse a submission the check after the review accepts.
Only the reviewer's reading tells the round's own calls apart.

**Mid-upgrade.** Additive within the parked state's version: a build before it
drops the key and reads the call unmarked, as before.

## Amendment: a call that outlives its round's collection is a late call of the next round

A collection drains its round's calls in flight only up to each call's bound,
the turn's cap or its process gone, so a call can still run as it reads the log.
Where a send-back's opening moved the log on to a new round before the call's
record landed, the log answered the record as of another round and dropped it:
told to nobody, and what it committed in no outcome.

**The round after takes it late.** A RECORD naming an earlier round of the turn,
any earlier round and an earlier run's under the same turn id included, is
taken by the log as a late call of the round it holds (`sandbox.takeEarlier`):
listed, numbered in the round's order of appends, marked late, what it committed
and what its own work called folded into the round's tallies, its spend
nobody's, answered recorded beside `sandbox.ErrJobReplaced`. The append files
its item for the round's job, as for a late call of the round, and every
continuation of the round tells it. A begin or a withdrawal of an earlier round
is still taken for nothing, so no call of a closed round starts. Any earlier
round, since a second send-back can open while the call still runs.

**With no place.** Its place (`sandbox.BridgeCall.Seq`) was its own round's.
Kept, it would enter the round's last place, by which the next opening, an
answer's cut and the call a person approved are numbered, and a cut past the
round's own later calls would drop a submission made after the answer. With
none, no reader of the round's order sees it, and none needs it: a late call is
never the submission, a job's own, or held by a save point. A record asked
again is found by its instant, name and digest (`sandbox.BridgeLog.listsEarlier`).

**It counts toward the round after's delivery check**, as a late call of the
round does: its effect happened in the turn and no earlier collection read it.
The record marks it late and the reviewer reads it as an earlier run's (the
amendment on the round's record, above). This answers the first residual of the
amendment on handing a late call to the job working as it lands, and of the one
on a call the log took after its job's relaunch.

**One that lands before the next opening is carried into it.** Between the
collection's read and the next round's opening the log is still the old round's,
and takes the record as a call of that round past the read. So the read rides
every relaunch's launch, a send-back's as much as one carrying the round on
(`sandbox.LogRead`: the job the log was open to, how many calls it had taken and
what it had absorbed; `runner.AgentRunRequest.BriefRead`, `sandbox.LaunchRequest.BriefRead`,
built by the coordinator at the read), and a new round's opening carries every
call the log took past it but the call a person approved, which the collection
read with the round: late, with no place, its nobody-waited mark kept, each
handed to the job the opening opens (`sandbox.BridgeLog.carried`). The new
round's absorbed tally starts with only what the old one absorbed past the
read, since the collection seeded the rest into the turn's call log; and every
new round starts with the old round's committed kinds whole, a set the next
collection folds into the turn's outcome. A read is applied only while the log
is open to the job it read, so while no opening has moved it since: the store
answers a log on another job as empty under the row's job, and applied, such a
read would carry, or mark, a round an earlier read told.

So what a late call, or one nobody waited for, committed counts in the turn's
outcome where the log takes it before the TURN'S last collection reads the log,
not its round's, as the amendments above put it.

**What it does not reach.** A call that lands after the turn's last collection
is told to nobody, its round's log deleted with the run. One the old log dropped
from its middle past the read is carried by name and kinds only, told to no job.
A record with no instant, a call answered without running, asked again within
the ledger's retries, is listed twice and told twice, and so is a record that
landed before the collection's read and lost its answer, asked again after the
next opening: the read saw it, so no opening carries it, and the round after
lists it again. A carried call's identity is seeded twice into the turn's call
log, once unfinished by the collection that read it in flight. Each is the safe
direction.

**Mid-upgrade.** No wire key and no record version: a late call with no place
is a shape the log already carries. An older build appending the record drops
it, as before.

## Amendment: a node's session spools

A spool's age said nothing a sweep could trust: a crash's spools waited hours
for one, and a container restart keeps `/tmp`, so a crash loop stacked them.

**Each process spools into a directory of its own** under the node's
(`sandbox.CoordinatorOptions.SpoolDir`), made under a staging name, its lock
taken with `internal/flock` and held for the process's life, then renamed into
place (`Coordinator.spoolHere`). Every start and every take remove each such
directory whose lock they can take, its process gone however it ended, and
never one whose lock is held (`Coordinator.sweepSpools`). The age sweep and
its bound are gone.

**Every spool of a process shares one room**, `(maxConcurrentSaves + 1) ×
sandbox.takeRoomBytes` (`sandbox.spoolRoomBytes`), 481.17 MiB: a take's room
is a sealed session at its bound for its sealed spool, taken first, and as
much again for its scrub's spool, each with what gzip adds to input it cannot
shrink and to the scrub spool's members. A save takes a take's room before its pack slot and never waits, so
saves leave a final take the fifth take's room; a take waits for its room at
most a minute (`spoolRoomWait`) before its pack slot and its scrub's slots, a
save's order, shrinks its sealed part to its archive's counted size once
packed and its scrub's part as that spool is read back and removed, and past
the minute keeps nothing and stamps nothing. Every seal is refused before it
writes past its room, a save's what its seat's ledger counted.

**A restore spools nothing.** It reads the object whole once, its seal opened,
before a byte reaches the box, and then again, streamed into the box as the
run starts, held to the first read's digest and length with its last byte
held back until both match (`sandbox.Coordinator.fetchSession`), so a store
that answers otherwise the second time fails the run's start, retried with
the launch, and the box restores nothing of it. `SessionStore.Fetch` and
`OpenSpooled` are gone. [Code
sandbox](../docs/concepts/code-sandbox.md#a-sessions-spools-on-its-node) has
the rest.

**Mid-upgrade.** Nothing reaches the wire or a record. An older build's flat
spools in the node's directory are not swept by this one.

## Amendment: a turn serves every conversation its run read

A run reads a wake of another conversation when it matched by work item
(ADR-0167), so its session holds that conversation's context; that
conversation's next turn read its own older head and started without it.

**A turn's `end` names every other conversation whose wake a step of its run
read** (`End.Served`, record version 5, at most eight), and the apply writes
one served row per conversation beside the turn's one row
(`executor_session_serves`). A conversation's head, endings and siblings read
its served rows beside its own. **A served row is placed where the end
applied**: the wake's own turn would have been dispatched once the seat freed
and read there or later, so the row outranks every turn of the conversation
that read before, and one that read after outranks it by its later end.

**The object is counted once.** A served row names no object; the turn's row
names it, so the collector, the backup, the budget and the retention count it
once. An end of the conversation shown the turn absorbs its served row alone;
while one stands, the turn's own conversation absorbs and dedupes around its
row, as around a held one. A retirement in the conversation drops the served
row; the retention and a seat's removal take the turn's served rows with it.

**An end's scope is the seat**, as is a retention delete's: each reads or writes
other conversations' rows. Both stay arbitrated on the turn's own
conversation, since neither decides anything from another's snapshot.

**Mid-upgrade.** A build reading version 4 retains a version 5 end, and every
later record of its seat, until it is upgraded.

## Amendment: a Gemini CLI or Qwen Code continuation's first request is judged in its box, and its run's start is a counted prefix

Both harnesses stated no session traits, so a continued session of either was
held whatever its brief (`heldWhatever`), yet both compress at a continuation's
first request, before the brief: every call before the session's save point
was counted as held and never told again.

**Both are judged in their box** (`FirstRequestBox`, no call keying, a brief at
a byte a token), as OpenCode is: a box's own settings, `.env` files,
environment or sign-in can move either's point, and runs have full access. A
sweep holds every harness that resumes a session to stating its first request;
`FirstRequestNone` is now a harness that continues none.

**The take counts each from above** off the kept chat file. Gemini CLI 0.70.0
compresses where the floor of its estimate of the restored history is at least
half the window, the brief not counted; the count is that estimate's bound
(each record's and answer's largest copy, a call's id, thoughts, the synthetic
answer a resume adds for each kept call no kept record answers), its masking
of tool output past 50,000 estimated tokens or its collapse of more than three
answers at 256 KiB marked `masked`, never bounded, and a line separator, raw
or as the scrub escapes it, or a patch ordering the records (`orderIds`)
`unread`. Qwen Code 0.28.4 compresses on a recorded figure (the newest reply's
prompt and output, or a newer compression's size) beside a quarter of the
brief and a dated reminder, at 85% of its window held 33,000 below it; the
count is that figure's bound, every message after it a token a byte, twenty
tool-answer images are `media`, and tool answers, an MCP tool's among them
(their share not measured), reaching the 500,000 characters past which its
first request clears the oldest are `masked`. A collection counts the box's own copy by the same rules once its
harness ended its run (the job's `resumed_count`), so a copy continued there,
and a capture taken whole in it, are judged by the worse of the two.

**The launch reads each box's point** in one exec: the threshold of the window
its release states for the model, Gemini CLI's session context (its tree and
`GEMINI.md` files) beside the brief, Qwen Code's reminder; a knob's name found
in any source it loads, a Gemini CLI sign-in other than a key, or Qwen Code's
subscription login, is `settings`, a `GEMINI.md` import or a context past 64
KiB `boxsources`, a Gemini CLI launch naming no model `nomodel`.
`BoxPoint.Anchored` keeps OpenCode's provider check to OpenCode's points: the
other two count no provider.

**What the scrub took out is counted back.** The meter reads the scrub's
output, but a box that resumes its own copy resumes it unscrubbed, and every
value or redaction the scrub took out may have cost its harness more than the
marker left in its place: a non-ASCII password shorter than its marker costs
Gemini CLI more, so a length less its marker's is no bound. So the take tallies,
for the members whose bytes its meter's count charges (`carrier.charges`; not
Codex's, a figure its rollout states), what each text loses: its
charge less that of the own words its kept text holds, each byte charged as
JSON.stringify writes it, a credential's whole value and a loaded tool
definition the cut takes out among it. The tally travels with the bytes through
the scrub's spool and its ways, cut back with them, and is added once the scrub
ends: to the bound and to the next request's estimate at a token a byte, above
every charge these harnesses put on a byte of a file their JSON writer wrote,
and for Gemini CLI and Qwen Code also to the sizes their masking, collapse and
clearing read. A marker the text already held counts as taken, which only
tells more. Gemini CLI's charge of a text is no sum of its characters: past
100,000 UTF-16 units it falls to a quarter a unit, so a marker longer than its
value could carry a kept text over that line at a sixth of what its box copy
costs. A text holding a marker is therefore charged by the short rule
whatever its length; every text the scrub changes holds one, and every other
is kept as it was.

**A run's start is a counted prefix** (`RunStart.Lines`): the script records
the chat file's line count and the POSIX `cksum` of those lines once the gate
cut it back, and the take checks that digest against the archive's raw bytes
before its scrub, keeping the count (`run_after_lines`, stamped once the
session is spooled) only where it matches, so a file rewritten, cut or rewound
keeps no start and a reading of it is never asked.

**Measured** (MEASURE-P2): Gemini CLI's rule to the quarter token at 524,288
and Gemma 4's 128,000, its estimator, masking and collapse boundaries, the
sources that move its point under a key sign-in, and 52 resumed runs
append-only; Qwen Code's rule to the token at 167,000, 98,072 and 850,000, its
window table, sources and image trigger, and 94 runs append-only. Not
measured, and told as a summary where met: media in a Gemini CLI history, a
Google login's server flags, a Vertex AI sign-in, a window `modelConfigs`
sets, what the context keys `includeDirectoryTree`, `fileName` and
`memoryBoundaryMarkers` do, a `.env` file naming the system settings' paths,
the window of a launch naming no model, a patch ordering the records
(`orderIds`), Qwen Code's subscription login, and a provider refusing a Qwen
Code first request over its window.

**Mid-upgrade.** No record version moves. An older build carries
`run_after_lines` among a capture's unknown keys, reads no start from it, and
its take reads no `from` of a chat file's shape, so it tells everything.

## Amendment: a carried session continues only under the entry it ran under

A harness keeps what a session was created with in its own files, and a seat's
chain may hold two entries of one harness with different routes and sign-ins.
A session was bound to its harness alone, so a launch under one entry could
continue a session another entry created, on whatever that harness kept.

**Every capture names the entry its session ran under** (`Capture.Entry`,
record version 6): the job's own launch's (`LaunchRecord.Entry`), never the
box's word, an identity built from the entry's route and sign-in mode only
(`harness.Provider.EntryID`: an HMAC, under the fleet's entry key, derived
from the keyring apart from the sign-in key (no id without it, as in a process
with no keyring, which no other node matches), of its key,
harness, auth mode, the provider its model names where the harness reads one
there, and the entry's route: the profile's `identity_env` and `sign_in_env`
switches as its env sets them, read once when the entry is built; an
allowlist, since the env is secret whole, and keyed, so a credential that rode
into a route is never a plain digest). An address is its scheme, host, port and
cleaned path: a gateway routed by its path is two routes, while userinfo,
query and fragment, where a credential may ride, and what only spells one
place two ways are dropped. A value that is no route, or a value or path
segment shaped like a key (a credential's prefix and a key's length), is
refused where the entry is
validated (a literal) and built (a resolved `${VAR}`), naming the variable,
never the value. A box's own routing is outside the identity. Every registered
runner declares, with its role, each variable that moves, signs or shapes its
requests, and every one it declares an endpoint is in its profile's route
(`executor.TestEveryEndpointARunnerDeclaresIsItsEntrysRoute`). The row keeps it,
and a base and a box's own copy carry it.

**Every decision whether a launch continues a session holds it to that entry**
(`sandbox.EntryContinues`): the relaunch's and a conversation's next turn's
(`sandbox.RestorerUnder`, through which every runner answers `ContinuityOf` and
`BaseContinuityOf`), the launch's own check, the in-place copy, the engine's
floor, and the read surface's. Under another entry, or one no record names, the
launch starts a session of its own, saying why. A replaced credential keeps the
entry, as a refusal on one relaunches its round in the same session.

**Fail closed, unless measured.** A runner continues a session under another
entry only where its harness was measured, on its newest release, to send a
resumed session's calls on its launch's route: a session created against one
stub endpoint, resumed with the route moved to a second, and the resumed call,
carrying the session's history, seen at the second (the numbers at each
declaration in internal/sandbox/executor), each on the release its runner
supports. Measured so on 2026-11-23: Claude Code 2.1.310 (its carry floor and
the box image's) and 2.1.330, Codex 0.171.0 (another provider id is refused
before any request, and none is met: a run's config names no provider),
OpenCode 2.3.2, Gemini CLI 0.70.0, Qwen Code 0.28.4 over ACP, and Pi 1.6.0.
Copilot, Grok Build, Muse Code, Kimi Code and Hermes Agent are not measured and
continue a session only under the entry it ran under.

**Mid-upgrade.** A build reading version 5 retains a version 6 capture, and
every later record of its seat, until it is upgraded. A capture an older build
wrote names no entry, which no entry matches on an unmeasured harness.

## Amendment: a cut-off Qwen Code session is read by content, along the chain its load walks

Qwen Code hands the bridge no call id (a call reaches it with a progress
token alone), so a relaunch continuing a cut-off Qwen Code capture was told
every call after its save point. It is now read by content, as OpenCode's is
(above), its calls asked by their place in the log (`CallKeyingByLogPlace`);
the verdict and the pool are that amendment's. What differs is the file and
what a part is.

**The releases.** A session is read only where Qwen Code 0.28.4 wrote it
(`qwenHoldingsReleases`, a set compared whole), the archive's writer, the
ask's resumer and every record's `version` alike: it was driven through the
engine's own bridge (MEASURE-qwen, MEASURE-P3-qwen) and every part these
rules count reached the continuation's first request as the log has the call.

**The run's start** is P2's counted prefix (`RunStart.Lines`), kept only where
its digest matched, or the session's beginning; a start named by a message is
refused. The file must hold at least those lines, and the line after them, if
any, is the run's brief, a person's record with no subtype (94 of 94 runs).
A shorter file, or another record there, is no reading: a rewritten, cut or
rewound file is never read.

**One chain, each uuid once.** A load does not replay file order: it walks
`parentUuid` from the newest record, merges records of one uuid and drops
what the walk skips (N5, N6). So a file is read only where it is one chain in
file order: line 1's parent null, each later line's the line before's, every
uuid a canonical lowercase uuid held once (by its own last 64 bits, never a
hash, a collision only telling), every record of the archive's session. Lines end at
`\n` alone; a `\r` anywhere, which Node's readline ends a line at, a blank
line, one that is not one JSON object, and a torn tail beyond the last break
(dropped, as the resume's cut drops it) are each read as that rule says, the
first three no reading. Each record holds exactly the eight members every
record holds and its kind's (a person's prompt or note, a step, a tool's
result, a system record of six subtypes), each once; a message exactly
`{role, parts}` with the parts its kind holds; a compaction's
`compressedHistory` a list. Anything else is no reading.

**The counted part.** After the start, a step is one `assistant` record; each
call has an id of 1 to 256 bytes, once in its step, at most 4,096 a step. A
`tool_result` after the start must answer a call of the open step not yet
answered (a load drops a second answer). It counts for the one class whose
call it is the verbatim answer of, only where the call is exactly `{id, name,
args}`, its arguments an object of at most 64 KiB keyed by the log's rule
(the record and the bridge hold the same text); the answer exactly `{id,
name, response}`, its name the call's, `response` exactly `{output}` or
`{error}`, a string; `toolCallResult` exactly Qwen's success or tool failure
(`errorType` `mcp_tool_error`, `error` `{}`), its `callId` the call's; and the
text neither blank nor a stub (`<persisted-output>`, `Output too large (`,
`Tool output was too large`, `Tool output truncated.`), nor Qwen's own words
for an empty output or a failure that sent none, and matched as OpenCode's
part is. A compaction after the start drops every counted part.

**The name rule.** Qwen Code names an MCP tool `mcp__<server>__<tool>`, cut
at 63 characters after a hash where a name is too long or invalid (N3). A
call counts only where its name is `mcp__crewlet__` and a class's tool of 1
to 48 characters of `[A-Za-z0-9_-]`, opening with no `_` and holding no `__`.
Another server reaches that prefix only through one of the three: a server
`crewlet_` gives `mcp__crewlet___x` (N1), a server `crewlet__dup` gives
`mcp__crewlet__dup__x` (N2, from a repository's own settings too), and a
cut hashed name is 63 characters, a 49-character tool. A workspace server
named `crewlet` stops the run before it starts (N8), so no call of it is
logged. A class whose tool fails the rule is never held: told in full. No
other prefix-keyed reader needs this rule: OpenCode's script refuses a
colliding server, Claude Code's reading keys by the id the bridge got,
`crewlet llm` admits only the server `crewlet`, and the rest (a subagent's
name map, the profiles' and the MCP layer's validation, Codex's stream, the
dashboard) hold nothing. Gemini CLI's `mcp_crewlet_<tool>` is ambiguous for
every bridge tool holding `_` (a server `crewlet_get` and a tool `page` name
`get_page`), so a Gemini CLI reading needs a rule of its own.

**Never counted, so told:** a part at or before the start or before the
newest compaction; `compressedHistory`, `ui_telemetry` and `resultDisplay`;
a spilled, MCP-capped or batch-rewritten output; an image or structured
answer; `invalid_tool_params`; a call with no `tool_result` (its synthetic
failure is written nowhere, N4); a tool another server can be named into.
**Not measured, never held:** a blank failure, argument coercion, a
`tool_result` subtype, a part key past those named (`thoughtSignature`), a
response with `parts`, and an earlier release's records in a continued file.

**The bounds.** The plaintext at the scrub's 256 MiB; no line held: each line
is read once on the scanner, its members decided at its end. Live: the walk's
buffers, the open step's calls (1.4 MiB at 4,096 calls of 256-byte ids) and
8 bytes a record (a record is at least 266 bytes, so at most 8 MiB): within
12 MiB, under Claude Code's 43 MiB, so `HoldingsReadsAtOnce` stays one.
Measured on two dedicated cores (`BenchmarkAQwenReadingAtItsBounds`, `GOGC=10`,
two runs a case): 255 MiB of the least record holds 10 MiB live, within the
12, and reads in 3.5 to 3.9 s, the slowest, inside the 30 s wait; of real
`ui_telemetry` 3 MiB in 2.5 s; one 255 MiB compaction 2 MiB in 0.33 to 0.36
s; 50 KB tool results 3 MiB in 0.8 s; a 32 or 64 MiB output 2 MiB in 74 to
140 ms; a step of 4,096 calls 3 MiB, and 200 classes over 4,096 parts 2 MiB,
each within 0.16 s.
A person's read holds a line whole up to the 32 MiB line bound and decodes
it, past the 29 MiB `TranscriptReadHeldBytes` measured on Claude Code's and
OpenCode's walks: beside the reading, from 1 MiB live before, the two hold
84 MiB where the output's line falls just within the bound (the person's read
alone 82 to 83), and 69 MiB of the 64 MiB line, which the person's read holds
to the bound and passes over.

**Mid-upgrade.** Nothing reaches the wire, a record, a row or the coordination
store. An older node keys no Qwen Code call and tells by the save point.

**What it leaves.** Toward over-telling: every never-counted and unmeasured
case above, a file continued by another release, and a run call identical to
an earlier job's. Toward a false hold, on the text-equality standard: a box
that writes its run's records itself, a part with a logged output, as
OpenCode's amendment leaves.

## Amendment: a cut-off Gemini CLI session is read by content, from the lines its take verified

Gemini CLI hands the bridge no call id, so a relaunch continuing a cut-off
Gemini CLI capture was told every call after its save point. It is now read
as an OpenCode session is (above): the ask keys each call by its place in the
log (`CallKeyingByLogPlace`), and the executor's `contentVerdict` holds a
class only where every call of it has a part of its own.

A Gemini CLI session is read only where 0.70.0 wrote it, replayed as its
loader replays it, and only from the records its run wrote after the lines
its take verified. A call counts only where the resume hands its model the
user record's answer, wrapped once, in exactly the log's text; failures and
truncated, masked, interrupted or multi-part outputs are told. A line
separator, an `orderIds` patch, a rewind to no record, an unread shape, or a
box that could admit another server spelling the bridge's names (its record
says so) leaves the session unread. Masking, collapse and compression at the
first request are the count's verdict; a reading is asked only where it rules
them out.

**The replay** (the executor's `geminiHoldingsWalk`): line 1 the session's
header; a record's latest copy keeps its first place, one that left the view
takes the next; `$patch` updates replace a viewed record's content (results
are never read) and its removals drop ids; `$rewindTo` drops its record and
every later place, and one naming no viewed record (0.70.0 then clears the
history) is no reading. Every key set is exact and no key is written twice.
A record is the run's where its id first appears past the start's line count;
the restart is the newest viewed person's record that is a compression's
summary by its lines: after a `$set` of the session alone and before the
harness's own "Got it" reply (`geminicompression.go`). A part counts only where its reply is the run's, after the
restart, its content a string; the call succeeded as `mcp_crewlet_<tool>`, of
a class's tool and arguments still at the end (`toolargs.Key`, at most 64 KiB);
and the first answer of its id among the person's records before the next
reply sits in a record of answers alone, exactly `{id,name,response:{output}}`
with the call's name, its output `<untrusted_context>\n`, the log's text with
no raw closing tag, and `\n</untrusted_context>`, matched by the text's digest
(`matchedClass`; never blank, never past the scrub's marker for a cut class).
A reply's calls whose ids are unread or repeated count none.

**Why it never holds falsely**: the parts follow a start the take's `cksum`
checked against the archive's own bytes; the answer is the one 0.70.0's
converter hands (the first of its id in the reply's turn, from the person's
record, never `toolCalls[].result`); the count rules out masking, collapse and
compression, since a count that may mask has no bound and its first request
compacts whatever the brief; the guard rules out another server; and a call
has at most one part, never a failure.

**The guard.** Before its harness starts, the run's script runs its helper's
mode `unshadowed`, and the session record gets `unshadowed yes` before its
release only where no source 0.70.0 loads a server from (both system files at
their variables' paths, the user's, the workspace's unless it is the home,
each extension in the home, a link's at its source) holds one whose sanitized
name is `mcp`, `crewlet`, `mcp_crewlet` or opens `crewlet_` or `mcp_crewlet_`
(the user file's own `crewlet` aside), none sets `mcp.serverCommand`, every
source parses, and the user file (the override in its place) and every file
read after it that names a sign-in name a key or Vertex AI: a login's remote
admin controls add servers no box sees, and 0.70.0 skips a system file that is
not root-owned (measured), so a later file's key cannot stand for a login
beneath it. Measured on 0.70.0 in 24 runs, main's
`--allowed-mcp-server-names crewlet` among them, which does not stop a
workspace or system `crewlet` replacing the bridge. The take keeps the word
on the capture (`run_unshadowed`), and the reader refuses a capture without
it. The record is the box's word, and the settings are read once, at start.

**Bounds, measured** (`BenchmarkAGeminiReadingAtItsBounds`, one run each on two
cores, `GOGC=10`): the plaintext at the scrub's meter, 2^14 ids of at most 512
bytes, 2^13 each of the run's records, calls and answers. One `$patch` line of
255 MiB of `{}` parts reads in 7.5 s and 255 MiB of `$set` lines in 5.0 s,
each holding at most 2 MiB live and 4 MiB of heap; a 64 MiB answer in 0.26 s
within 2 MiB; 2^14 ids with the run's records, calls and answers at their
bounds in 0.22 s, 4 MiB live and 5 MiB of heap; beside a person's read of the
64 MiB file, which holds 68 MiB, the two hold 70 MiB.

**Not measured, so told**: an `orderIds` patch, when a collapse runs, a U+2028
decoded in a box's own copy, a login's admin servers, the single-record
`$patch`, `$set.messages`, and `info`, `error` and `warning` records; each is no
reading.

**Other readers keyed on the prefix**: none is this reading's defect. The
host's text phases (`internal/providers/llm/harness`'s `callOf`) are pinned by
`--allowed-mcp-server-names crewlet --extensions none`, leaving the host's own
system `crewlet`; a subagent's `engineName` cuts the prefix and then holds the
name to the engine's tools; OpenCode's `bridgedToolName`, reused here, is
guarded by `openCodeShadowCheck`; the record, the stream, the take script, the
count, the helper's usage and the bridge log's accounting key on nothing a
false hold could follow.

**Mid-upgrade.** `run_unshadowed` is a new key an older build carries among a
capture's unknown keys and reads nothing from, so its relaunch tells as
before; a capture an older build took holds no word, and is never read.

## Amendment: a relaunch waits for a capture as long as its size needs, and an upload the store took none of is made again

A relaunch waited fifteen seconds for a capture of the job it follows still
being kept, whatever its size, and passed over one still under way past that
(`capture_late`), though its keep had its whole budget to land it; and a keep
whose one upload the store took none of refused the capture. Both lost a
conversation's working memory to a keep that would have landed.

**The wait.** A relaunch waits for each capture or save it depends on
(`relaunchWaitsOn`) as long as a capture of that archive's size may still take
once its place is reserved, each step at its floor (`captureRestFor`): the
scrub's two readings of the archive out of its box at the box-read floor, a
mebibyte a second (`boxReadPace`), and its slowest plaintext, 183 s; the digest
left in its box; and one keep of the sealed session the archive may become
(`keepBudgetFor` of `countedFor`). 5 min 35 s for an archive of half a
mebibyte, 31 min 9 s at the bound, the longest where it waits on two
(`Coordinator.relaunchWaitOf`). A take stamps its archive's size with its
place (`archive_size`, as the box reported it before the scrub). The
relaunch's asks, of a record never answered and its one read of the log past
the wait, keep fifteen seconds (`relaunchAskWait`, the begin record's budget).
The frame holds the seat and a slot of the node's admission that long only
while a keep is under way: one that lands, is refused or whose taker is gone
ends the wait at its next read. This amends the capture's-life amendment's
refusal of a longer patience: a seat held while a keep lands costs less than
the session a relaunch passes over.

**The upload.** An upload the store took none of (`objstore.ErrUnanswered`:
nothing is stored, and a later put may be) is made again into the same object,
spaced as a record's attempts are, each begun only while the keep's budget
left holds the session at the store's slowest pace and its record's room
(`Coordinator.uploadSpool`), the taker lease's fence asked before each; any
other refusal is final. `SessionKeepBudget` is unchanged (`keepBudgetFor` of
the sealed bound, twenty-six minutes), so a capture's life and every bound
read from it are too.

**The scrub's wait** is derived in code from the archive's bound: the spool's
minute, the archive read twice at the box-read floor (`scrubArchiveReads`,
96 s at 48 MiB) and the slowest plaintext (`scrubPlainSlowest`), rounded up to
the minute, six minutes as before.

**Mid-upgrade.** `archive_size` is a new key an older build carries among a
capture's unknown keys. An older relaunch still waits fifteen seconds, and an
older keep still uploads once.

## Amendment: a session no box of this build continues is not kept

Every launch's session was captured wherever its box could be read, and a
session no runner restores (written by a harness release below its floor or
outside the one it supports, by one that named no release this build reads, or
in another archive format) was packed, scrubbed, uploaded and counted against
the seat's budget, only for every launch to refuse it (`release_floor`).

**The take refuses it unread.** A take reads the release off the run's record
before it packs or streams anything, and asks its runner's restore floor
(`Runner.restores`, every check a box makes but the box's own release, under
any entry): one it refuses is an `UnrestorableError` carrying the floor's loss
and reason, before a pack runs (`Runner.packNow`) and before an archive the
run's script or a save packed is streamed (`Runner.TakeSession`). The capture
is stamped refused with that loss, as any refused take is, so the turn's end
names it (`unkept.loss`) and its next launch says it. The floor is decided
once, by the runner that restores, so the capture and the restore cannot
disagree.

**A save of it is refused the same way, and the job is saved no more.** The
release that wrote a job's session is its record's, written before its harness
started, so it holds for the job's whole run: the node remembers the refusal
for that job and box (`saveUnrestorable`), and neither its poll nor its drain
offers the job a save again. A restart forgets it and costs one refused save.

**What is not affected.** A relaunch into the box the job ran in continues the
copy that box holds as before: it never needed the capture. A carried session
continues only under the entry it ran under, which is a later launch's to
decide, so the take holds a session to its floor alone.

**Mid-upgrade.** Nothing on the wire changes: the refusal is a capture stamped
refused with a loss every build names. An older build still keeps such a
session, which every launch of either build refuses.

## Amendment: a session's large media is kept apart

A harness keeps every image, PDF or file it read inline, as base64 in its
transcript: a whole-PDF Claude Code read is 40.1 MiB of the 48 MiB bound, kept
twice where a read repeats, and kept again in every capture of the session. So a
kept session's large payloads are stored apart from its text, once each
(crewlet/crewlet#1626). The log and the session store hold and restore a
session so; the take still keeps every session whole until its scrub cuts the
payloads apart (crewlet/crewlet#2020).

**Stored by its member, spliced back byte for byte.** Each payload is a gzip
member of its own in the archive a take writes; the session's text object
holds every other member as text frames and names each payload's member by its
SHA-256 in a media frame (`executorkeep.FrameText`, `FrameMedia`). One sealed
object holds each distinct member, sealed under
`executor-media/<seat handle>/<object key>` (`executordomain.MediaAAD`): the
seat and the key, never the turn, so a later turn of the seat that carries it
opens it. `executordomain.SessionStore.Open` splices every member back where
its frame stands, checking it against its digest once read, so the box, every
reader and the take's own digest of the archive see exactly the bytes the take
wrote, and no reader parses a reference inside a transcript. A session with no
media is stored as before.

**Named by rows, kept by reachability.** A capture names its session's whole
media set (`Capture.Media`, record version 9; one with none keeps the older
shape an older build reads), and a row of `executor_session_media` per object
names it on the turn's row, a Required reference
(`executordomain.SessionMediaReferences`). A capture carries, without
uploading again, an object its turn's own row or the base that row holds
already names ([ADR-0027](0027-a-deletion-from-the-shared-store-needs-no-lock.md)'s
amendment "a session's media, carried"). Every record that takes a turn's object
off its row, or deletes the row, takes its media rows in the same apply, so the
collector deletes a media object once no live session names it; there is no
count. A media object missing, sealed for another seat, or not its digest is
`ErrUnreadable`, so a restore starts fresh (`restore_failed`) rather than hand a
harness an archive with a hole.

**Counted once, bounded together.** The seat's budget counts each media object
once however many rows name it, and a release frees one only when no other row,
nor the capture making room, names it. The log bounds a capture's text and
media together at `executorkeep.SessionKeptMaxBytes` (94 MiB, sealed:
`KeptSealedMaxBytes`) and at `MediaMaxCount` (32) objects; the text object stays
within `SessionMaxBytes`. A rotation waits out media objects as it does
sessions. The API row and the Sessions screen state `media_bytes` beside
`object_bytes`.

**Mid-upgrade.** An older build retains a capture naming media, so it never
applies a row whose frames its estate does not name; its collector then reads
an incomplete estate and deletes nothing. A capture with no media is unchanged.

## Amendment: a kept session is what its harness's resume loads

A text session grows by about one context window between two of its harness's
compactions, and every harness but Claude Code, whose own trim rewrites its
transcript, appends its compaction and keeps every line before it. So a long
session reached the bound and was refused (`too_large`), its next launch
continuing an older capture or starting fresh, though its resume loads only the
lines from its newest compaction on.

**A pack past the bound trims its copy.** Every pack (the run's end, a stop's,
one made on demand, a save, a drain's save) measures what its take is held to:
the record's bytes before it tars, against what the scrub reads less a mebibyte
for the tar's own bytes (`trimPlainPoint`), and the archive after, against what
a take keeps (`trimPackedPoint`). Past either it keeps what its harness's resume
loads: everything from the record's newest compaction on and, before it, what
that release reads there, cut from the pack's copy and never the live record,
so it is safe beside a working harness. A record with nothing
to cut at is packed whole, and one still past the bound once trimmed is refused
as before.

**At the bound, not below it.** Every pack trims, so no growth between a trim
and its take needs room, and a trim only where the whole would be refused keeps
the most history Sessions and `read_session` can read. No compaction is forced:
it costs a model call, takes the model's detail before its own compaction would,
can run mid-turn on no harness, and on every harness but Claude Code appends
rather than shrinks.

**Per harness, on its measured release alone** (`trimmer`). A session any other
release wrote is packed whole, and so is every harness with no measured rule.
Codex 0.171.0 keeps the rollout's `session_meta` line and everything from its
newest `compacted` line, whose replacement history its resume replays in place
of every line before it (Measured, above). Copilot 1.0.114 keeps everything
from the start of its newest compaction that applied and, before it, every
event but the turns, tools, usage and bookkeeping that compaction replaced: its
resume rebuilds the summary's request from every person's message and reads
every instruction update (measured: the cut sent the request the whole did,
byte for byte). Muse Code 1.4.25 keeps everything from its newest installed
compaction, and only where the next line is that compaction's checkpoint, the
context its resume loads, needing nothing before it; a record with no
checkpoint after its newest install is packed whole, since 1.4.25 refuses to
resume a record with none (measured: the cut sent the requests the whole did).
Gemini CLI 0.70.0 and Qwen Code 0.28.4 keep the chat file's
first line and every line from their newest compaction's first on (Gemini
CLI's the `$set` opening a closed compression sync, found by its three lines;
Qwen Code's its newest `chat_compression` record), never cutting past the line
the session had reached as the run began (`trimStartVar`), so a session the
run began is packed whole (measured: the cut, and cuts keeping more lines
before the boundary, sent the request the whole did, byte for byte).
Hermes Agent 0.23.3 keeps every line of its dump but the session's inactive
messages below its newest active compaction summary, which its resume never
loads, and slims each settled admission's stored result, which holds its turn's
messages again, of its copies of the transcript, as Hermes's own retirement
does; nothing Crewlet reads takes them (Measured, above).

**Said, never a loss.** The pack leaves its word beside the archive (the
archive's bytes, the record's before and kept, and for a chat file the lines
its run began after, restated over the cut copy as the run's script states
them), which a take reads only for a trimming harness's release and only where
it names the archive the take describes (`Runner.trimOf`). The take checks the
restated start against the archive in place of the record's, which names
lines the cut left out (`trimmedPrefix`). The capture stamps it (`trimmed`), the log carries
it from the record version `trimmedVersion` names and the row keeps it
(`executor_sessions.trimmed`), and
Sessions and `read_session` say the history before the newest compaction was
trimmed (`Session.TrimmedNote`). It names no loss and raises no alarm; it is
counted (`crewlet.executor.session.trimmed`) and logged
(`executor_session_trimmed`).

**The plaintext bound is a size bound.** A take expanding past the scrub's
256 MiB of plaintext was stamped `keep_failed`, a take that failed; it is
`too_large` (`executorkeep.ErrSessionPlainTooLarge`), its reason naming that
bound, which for text binds before the archive's.

**No trim where no cut leaves out only what a resume skips.** Claude Code's own
trim rewrites its transcript, and its subagent threads stay whole: 2.1.310
continues a subagent spawned before the newest compaction after a resume, by its
id, from its thread's file (measured). Kimi Code's replay folds every record
before its compaction into the state it resumes. Cursor Agent carries no chat a
compaction summarized. Each packs whole past the bound.

**Mid-upgrade.** A capture carrying `trimmed` is stamped `trimmedVersion`, which
an older build retains until it can apply it. An older build's pack trims nothing and its take
reads no word, so a session it packs past the bound is refused as before.

## Amendment: a session's newest summary is kept apart

A session not kept (too large, refused by the store or the log, lost with its
box after its read) or kept and not continued left its next run only what the
engine recorded, while the box held the harness's own account of it: its newest
compaction summary.

**Every box read keeps it.** A collection's, a stop's and a loss's read ask the
job's main thread for its newest summary in the same one read as its count and
receipts, on the runner's supported release alone. It is scrubbed of what the
capture is scrubbed of and redacted as a capture is, held to
`executorkeep.SummaryMaxBytes` by its end (the largest output a runner's harness
sets on its compaction call, at the engine's four bytes a token), and sealed as an
object of its own beside the session's take, never inside it, under its own
associated data. **Its keep adds no write and no read the box read does not
make**: the take reads the values its capture is scrubbed of once, for itself and
the summary, and the run's row names the summary in the take's own stamp
(`SessionCapture.Summary`). A take that spools the session uploads the summary in
the session's keep, beside the session's upload, so a summary never holds that
keep or the node's stop past its reason, and the turn's end waits for the keep
as for every capture begun; a take refused before its keep uploads it before the
refusal's stamp, in a summary's room the take's time leaves before its records
(`BoxReadBudget` adds it, 80 s at the host's share). A read keeps it once, and a
summary the run's row names already, under a key minted within half of
`objstore.RecordWithin`, is not kept again.

**Three names.** The run's row names the newest (`PendingRun.Summary`), the
turn's end names it on the log (`Ending.Summary`, record version 11: no text,
only the object, when it was kept, its whole size and the bytes kept, its harness
and its session), and
the apply keeps it on `executor_session_summaries` beside the turn's row,
declared Required to the collector. The end's decide leaves out a summary minted
past `objstore.RecordWithin` by its own clock, as it refuses such a capture, and
lands the end; so the turn's end first copies a summary minted past half of it
under a fresh key (`SessionKeep.CopySummary`), and a turn parked past the window
still ends naming its summary.

**Placed among the turn's captures.** The run's row names a summary at the
place of the capture whose stamp first named it (`SummaryStamp.CaptureSeq`
beside its launch), and the end carries that place (`Summary.Place`, record
version 12) onto the summary's row. A summary found again under a fresh key
keeps its first place. A session captured at or after the place holds what the
summary tells, and one captured before does not, so a run that continues an
older capture is told a summary newer than it only where the place is later
(`CapturePlace.After`): never a summary the session it continues holds.

**It goes with the row.** A retirement, a stale release and the budget's release
of the row's session take the session and keep the summary, which is what a run
that cannot continue the session starts from: the summary is not counted against
the seat's budget, one a row, bounded by `SummaryMaxBytes`, and gone with the
row. An absorption, a dedupe, the retention and a seat's removal take it.

**Read where the session is not.** A row keeping no session reads as its
summary (`executorread.TranscriptSummary`), on the Sessions screen and through
`read_session`: from its end where it is past an entry's bound
(`TranscriptEntry.Tail`), and on a first page read again after the session went
mid-read.

**Mid-upgrade.** A build reading an older version retains a version 11 end, and
a build reading 11 a version 12 end, and every later record of its
conversation, until it is upgraded.
