# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`, `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`, `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`, `internal/executordomain.TestALateCaptureNeverRollsASessionBack`, `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`, `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`, `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`, `internal/executordomain.TestARemovedSeatStaysRemoved`, `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`, `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`, `internal/engine.TestTheExecutorLogFitsBesideTheOtherLogsAtTheOperatorsSmallestVolumes`, `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`, `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`, `internal/estate.TestEveryExecutorWriteIsTheEngines`, `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`, `internal/sandbox.TestEachLaunchOfATurnIsNumberedPastTheOneBefore`, `internal/sandbox.TestACapturesPlaceOnlyGrowsOnTheRow`, `internal/sandbox.TestACollectionTakesTheSessionBeforeThePauseAndKeepsItAfter`, `internal/sandbox.TestEveryCollectionTakesWhatItsJobLeft`, `internal/sandbox.TestAJobTakenOnceItHadEndedIsNotTakenAgain`, `internal/sandbox.TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd`, `internal/sandbox.TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd`, `internal/sandbox.TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim`, `internal/sandbox.TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt`, `internal/sandbox.TestALostRunsBoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAnEndWaitsForItsTurnsCaptureToLand`, `internal/sandbox.TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded`, `internal/sandbox.TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused`, `internal/sandbox.TestTheScrubReadsTheLoginAgainWhereItIsRefreshed`, `internal/sandbox.TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse`, `internal/sandbox.TestWhatABoxWasHandedIsKeptBesideItsRowForItsJobAlone`, `internal/executordomain.TestAnEndWhoseNewestCaptureWasNotKeptReleasesTheOlderOne`, `internal/executordomain.TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted`, `internal/sandbox.TestTheSwitchOffKeepsNothingAndStillEndsTheTurn`, `internal/sandbox/executor.TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox`, `internal/sandbox/executor.TestAPackOnDemandKeepsItsDigestInTheBox`, `internal/sandbox/executor.TestATranscriptLineStaysJSONAndLosesEverySecret`, `internal/sandbox/executor.TestTheDigestIsPOSIXCksums`, `internal/sandbox/executor.TestTheScrubHoldsNothingPastItsBounds`, `internal/sandbox/executor.TestATarLeavesTheBoxWithOnlyItsSessionsFiles`, `internal/sandbox/executor.TestAnExportIsOneDocumentKeptScrubbed`, `internal/sandbox/executor.TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed`, `internal/sandbox/executor.TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession`, `internal/sandbox/executor.TestATakeIsBoundedByTheArchiveItKeeps`, `internal/engine.TestAResumedTurnsEndIsRecordedWithItsOwnDecision`, `internal/engine.TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog`, `internal/engine.TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestALostTurnEndsAsLostOnTheExecutorLog`, `internal/engine.TestTheEndCarriesTheTurnsOwnDecisionAndBreach`, `internal/engine.TestTheEndSaysWhatTheEngineAnsweredAColleague`, `internal/engine.TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid`, `internal/engine.TestAStopsAccountReadsEveryRoundsCalls`, `internal/engine.TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds`, `internal/engine.TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped`, `internal/engine.TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn`, `internal/engine.TestAResumedTurnThatPanicsOnceBegunEndsFailed`, `internal/engine.TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose`, `internal/engine.TestAnMCPServersURLSecretIsHandedToTheScrub`, `internal/engine.TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives`, `internal/sandbox.TestARetiredSeatsRunIsFinishedWithItsTurnsEnd`, `internal/sandbox.TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded`, `internal/sandbox.TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt`, `internal/sandbox.TestTheSwitchOffAsksNoHostForALostRunsSession`, `internal/sandbox.TestASpoolACrashLeftIsSweptByTheTakesAfterItAges`, `internal/sandbox.TestASpoolNeverOutlivesItsCapture`, `internal/sandbox.TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded`, `internal/sandbox.TestTwoAttemptsAtOneEndingOnANodeFinishItOnce`, `internal/sandbox.TestALoginTheBoxGrewPastItsBoundRefusesTheCapture`, `internal/sandbox.TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt`, `internal/sandbox.TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt`, `internal/sandbox.TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail`, `internal/sandbox.TestAnEndingDecidedBeforeTheNextHoldersFenceIsFinishedByIt`, `internal/sandbox.TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer`, `internal/sandbox.TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled`, `internal/sandbox/executor.TestAHandedKeysArmourStaysForTheShapePass`, `internal/sandbox/executor.TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack`, `internal/redact.TestALineIsCutAtTheArmourAKeyIsReadBy`, `internal/procgroup.TestASignalReachesOnlyALivingMemberOfTheGroup`, `internal/sandbox.TestADirectBoxEndsOnlyAProcessOfItsJob`, `internal/sandbox.TestADirectBoxKillsEveryProcessOfItsJobAndKeepsTheBox`, `internal/sandbox.TestAContainerBoxEndsAndKillsItsJobInsideTheContainer`, `internal/sandbox.TestAContainerJobThatRecordedNoGroupIsKilledByTheGroupItsExecLeads`, `internal/sandbox.TestAContainerJobWhoseGroupNoExecLeadsIsSignalledNothing`, `internal/sandbox.TestAContainerJobWithNoGroupRecordedIsReadOverOnlyOnceItIs`, `internal/sandbox.TestAnE2BBoxEndsAndKillsItsJobAsItsUser`, `internal/sandbox/executor.TestAnEndedClaudeCodeRunPacksAsItsOwnEndDoes`, `internal/sandbox/executor.TestAnEndedOpenCodeRunExportsItsSessionBeforeItsServerStops`, `internal/sandbox/executor.TestAnEndIsAskedOfThePIDTheScriptRecorded`, `internal/sandbox/executor.TestACollectionSaysItsJobWasEnded`, `internal/sandbox.TestAStopEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestALossEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestAJobThatOutlivesItsEndIsKilledBeforeItsTake`, `internal/sandbox.TestAJobAskedNothingIsKilledNotWaitedFor`, `internal/sandbox.TestAStopOfABoxOnAnotherHostEndsItsJobThere`, `internal/sandbox.TestAStopWithTheSwitchOffEndsItsJobOffTheFrame`, `internal/sandbox.TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake`, `internal/engine.TestABoxReadWaitsAsLongAsItsKindTakes`, `internal/sandbox.TestACappedJobIsEndedBeforeItsSessionIsTaken`, `internal/sandbox.TestACappedRunStillSaysItWasCutOffAtItsCap`, `internal/sandbox/executor.TestAnEndedOpenCodeJobsSessionIsTakenAsItsScriptExportedIt`, `internal/sandbox.TestACapsVerdictOutlivesAReadCutShortAsItEndsTheJob`, `internal/executordomain.TestABeginHoldsTheBaseItsReadNamed`, `internal/executordomain.TestABeginAndAnEndOfItsBaseAreOrderedByTheLog`, `internal/executordomain.TestNoReleaseTakesAHeldBase`, `internal/executordomain.TestTheHoldLiftsAtTheHoldersFirstCapture`, `internal/executordomain.TestTheHoldLiftsWhenTheHolderEnds`, `internal/executordomain.TestATurnThatNeverCapturedAbsorbsNothing`, `internal/executordomain.TestTheRetentionListsNoHeldRow`, `internal/executordomain.TestAPageNamesEachConversationsHead`, `internal/executordomain.TestABeginTheRowsCannotTakeIsRefusedAtTheDecide`, `internal/executordomain.TestABeginSaysWhetherTheLogHoldsItsBase`, `internal/executordomain.TestACapturesContextAndCutOffReachTheRow`, `internal/executordomain.TestTheExecutorStatementsArePlanned`, `internal/sandbox.TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff`, `internal/executordomain.TestABeginHoldsOnlyTheSessionItsReadSaw`, `internal/executordomain.TestALateBeginRecordsTheReadAndHoldsNothing`, `internal/executordomain.TestABeginOfATurnThatEndedWritesNothing`, `internal/executordomain.TestABeginNoReadHandsOutIsRefusedByItsWriter`, `internal/executordomain.TestABeginThatHoldsNothingSaysWhy`, `internal/executordomain.TestAHolderWhoseEndWasLostIsEndedLostAndItsHoldLifts`, `internal/sandbox.TestATurnsFirstLaunchContinuesItsConversationsHead`, `internal/sandbox.TestTheFreezeIsWrittenAtTheFirstLaunchOnly`, `internal/sandbox.TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase`, `internal/sandbox.TestWhetherABaseIsContinuedIsOneDecision`, `internal/sandbox.TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone`, `internal/sandbox.TestABaseIsContinuedOnlyInABoxOfItsOwn`, `internal/engine.TestAFrozenReadFitsItsBound`, `internal/engine.TestATurnReadsItsConversationOnce`, `internal/engine.TestTheFrozenBaseOutlivesASiblingsEnd`, `internal/engine.TestAnEndingLeftOutOfTheBriefIsNotAbsorbed`, `internal/engine.TestATurnThatNeverLaunchedTellsTheNextTurnHowItEnded`, `internal/engine.TestAnAbandonedFirstLaunchEndsItsTurnOnTheLog`, `internal/engine.TestTheSwitchOffCarriesNothingBetweenTurns`, `internal/agent/prompts.TestTheEndingsKeepTheNewestWholeAndCountTheRest`, `internal/sandbox.TestWhatAReaderIsToldOfAnEndedTurnIsTheNextTurnsOwnDecision`, `internal/sandbox.TestARelaunchOfATurnThatKeptNothingIsSaidToContinueItsBase`, `internal/api/queries.TestARowSaysWhetherTheNextTurnContinuesIt`, `internal/api/queries.TestARowReadAgainIsDecidedOnTheHeadItsOwnReadNamed`, `internal/engine.TestAnUnreadableConversationIsLessContextNotAFailedTurn`, `internal/schedule.TestEachFireOfAScheduleIsOneConversationOfItsRunner`, `internal/schedule.TestTheLongestScheduleNameFitsTheLogsKey`, `internal/org.TestAScheduleNameIsBoundedAsItsKeySpellsIt`, `internal/notify.TestAScheduleConversationIsKnownByItsPrefixAlone`, `internal/engine.TestAScheduledFireContinuesThePreviousFiresSession`, `internal/engine.TestFiresOfOneScheduleThatWaitedTogetherRunOnce`, `internal/engine.TestAFireIsNeverTakenAsTheAnswerToTheLastFire`, `internal/api/queries.TestARunNoChatCanAnswerSaysSo`, `internal/sandbox.TestAScheduleRowIsToldItsNextFire`, `internal/api/queries.TestAScheduleRowSaysWhetherItsNextFireContinuesIt`, `internal/executordomain.TestAConversationsHeadIsTheTurnThatReadItLast`, `internal/executordomain.TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued`, `internal/executordomain.TestAHeadReleasedBeforeAStaleSiblingHandsTheNextTurnTheSibling`, `internal/engine.TestTheNextFireContinuesTheSchedulesLineNotTheFireThatEndedLast`, `internal/executordomain.TestAnEndThatRanOutOfContextLeavesTheConversationNoHead`, `internal/executordomain.TestARetiredSessionGoesFromTheBaseWhereNoCaptureLanded`, `internal/executordomain.TestARetiredSessionGoesFromARowALiveTurnHolds`, `internal/executordomain.TestASessionStartedAfterRunningOutIsTheNextHead`, `internal/executordomain.TestAnEndThatFailedAnyOtherWayKeepsItsSession`, `internal/executordomain.TestARetirementReachesNoOtherSessionOrConversationOrSeat`, `internal/executorkeep.TestAnEndingsExhaustedSessionsAreBounded`, `internal/sandbox.TestAJobThatRanOutOfContextIsRecordedAndKeepsNoCapture`, `internal/sandbox.TestACappedRunThatAskedAndRanOutOfContextKeepsNoCapture`, `internal/sandbox.TestARelaunchContinuesNoKeptCaptureOfASessionThatRanOut`, `internal/sandbox.TestARelaunchContinuesNoBaseThatRanOut`, `internal/sandbox.TestARelaunchContinuesNoCopyInItsBoxOfASessionThatRanOut`, `internal/sandbox.TestAFreshSessionAfterRunningOutIsContinued`, `internal/sandbox.TestAReaderIsToldASessionThatRanOutIsNotContinued`, `internal/sandbox.TestATurnsEndNamesTheSessionsItRanOutOfContextIn`, `internal/engine.TestATurnAfterASessionRanOutOfContextStartsItsOwnSayingWhy`, `internal/executordomain.TestAnEndWhoseCaptureGoesWithItAbsorbsNothing`, `internal/executordomain.TestARetiredRowSaysSoWhereItsNewestAttemptWasNotKept`, `internal/executordomain.TestASeatsSearchFindsEverySessionHoldingEveryWord`, `internal/executordomain.TestASearchMatchesWhatAReadWouldShow`, `internal/executordomain.TestASearchThatStopsAtItsBoundSaysWhereAndHowFar`, `internal/estate.TestEveryExecutorOperationCrossesWhole`, `internal/estate.TestTheExecutorFacadesAskExactlyTheExecutorOperations`, `internal/executorread.TestASessionReleasedWhileItIsReadIsSaidAsReleased`, `internal/executordomain.TestASeatReadsNoOtherSeatsSession`, `internal/sandbox/executor.TestAMatchIsDecidedOnWhatItsReaderIsShown`, `internal/sandbox/executor.TestTwoRecordsThatRedactAlikeAreReadAlike`, `internal/sandbox/executor.TestAFilterHandsOutEveryPartItCouldNotRead`, `internal/agent/builtin.TestASeatReadsOnlyItsOwnSessions`, `internal/agent/builtin.TestTheSwitchStopsEveryModelsReadsAtItsNextCall`, `internal/agent/builtin.TestAPersonIsNeverOfferedASeatsSessions`, `internal/agent/builtin.TestReadingASessionWritesNothing`, `internal/agent/builtin.TestASessionListedThenReleasedIsReadAsReleased`, `internal/agent/runner.TestTheReviewerIsOfferedNoSessionTool`, `internal/engine.TestTheSessionToolsAreOmittedWhereNoSessionIsKept`, `internal/engine.TestEveryBarrierDeclaresItsReadModel`, `internal/e2e.TestAnExecutorReadsItsSeatsSessionOverTheBridge`, `internal/sandbox.TestARelaunchIsToldOfABaseItRanOutInWhileItsRowHoldsIt`, `internal/sandbox.TestACaptureWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow`, `internal/sandbox.TestAnEndDoesNotWaitOutACaptureWhoseTakersLeaseLapsed`, `internal/sandbox.TestADrainingTakersCaptureIsNotSettledUnderIt`, `internal/sandbox.TestATakerLeaseHeldAtAnotherEpochIsGone`, `internal/sandbox.TestAStoreThatCannotAnswerNeverReadsATakerAsGone`, `internal/sandbox.TestAGoneTakersCaptureIsSettledOnlyByAnAnswerFromTheLog`, `internal/sandbox.TestARelaunchSettlesACaptureWhoseTakerIsGoneBeforeItDecides`, `internal/sandbox.TestAnUnresolvedCaptureIsAskedAgainBeforeARelaunchDecides`, `internal/sandbox.TestACaptureARelaunchsAskLandsIsTheTurnsOwnToRead`, `internal/sandbox.TestARelaunchsAskOfAnUnansweredRecordEndsWithinItsWait`, `internal/sandbox.TestAFencedKeepUploadsAndRecordsNothing`, `internal/sandbox.TestATakerLeaseOutlastsTheDrainAndIsReleasedAfterTheLastKeep`, `internal/sandbox.TestATakeWithNoTakerLeaseBeginsNoCapture`, `internal/sandbox.TestAStampWithNoTakerIsSettledByAge`, `internal/sandbox.TestAKeepIsFencedOnceItsLastRenewalNoLongerCoversIt`, `internal/sandbox.TestATakeStampsTheTenureItReservesItsPlaceUnder`, `internal/sandbox.TestAnAttemptAtAKeepsRecordEndsWithTheWindowItWasSentIn`, `internal/sandbox.TestASettleLeavesWhatItsTakerStampedSince`, `internal/sandbox.TestATakerLeaseIsNeverAskedForLongerThanTheStoreKeepsOne`, `internal/sandbox.TestAStopUnderARenewalSaysNothingOfTheStore`, `internal/sandbox.TestACoordinatorNeedsItsCollaborators`, `internal/sandbox.TestACaptureRecordedAfterItsSettleIsKeptThroughTheTurnsEnd`, `internal/sandbox.TestARelaunchStartsWithoutACaptureItsRecordSettledRefused`, `internal/sandbox.TestARelaunchOfATurnThatFrozeNoBaseReadsItsRowForACaptureItsRecordRefused`, `internal/sandbox.TestARelaunchThatWillNotWaitOutAKeepContinuesTheBaseTheTurnStillHolds`, `internal/executordomain.TestAnEndsUnkeptCaptureStandsOnlyWhereItIsNewerThanTheRowsPlace`, `internal/api/queries.TestAnEndingSendsAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`, `internal/engine.TestAnEndingTellsTheNextTurnAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace`, `internal/executordomain.TestARowIsShownWithNoUnkeptCaptureItKeeps`, `internal/coord/kv.TestALapsedTakerLeasesKeyIsRemoved`, the `internal/coord/coordtest` contract case `every_taker_lease_mints_from_its_class_counter`, which every coordination backend runs, `internal/engine.TestTheEnginesCoordinatorHoldsThisProcesssTakerLease`, `internal/engine.TestTheSandboxIsStoppedOnlyByTheTeardownAfterTheDrain`, `internal/engine.TestASandboxsTakerLeaseIsGivenBackOnTheStopsAllowance`, `internal/engine.TestATokenThePollRenewsIsRecordedBeforeItIsWritten`, `internal/engine.TestARenewalTheStoreDidNotTakeIsNotWritten`, `internal/sandbox.TestACaptureTakenAfterARenewalHoldsNoRenewedValue`, `internal/sandbox.TestATokenIsRecordedOnlyInTheSetOfTheJobItsRowHoldsRunning`, `internal/sandbox.TestATokenTheLaunchWritesInAKeptBoxIsSealedWithItsJob`, `internal/sandbox.TestAnIssuedDocumentIsScrubbedByEveryStringItHolds`, `internal/sandbox.TestAServiceAccountKeysPublishedConstantsStayInItsSessions`, `internal/sandbox.TestARenewedValueIsDroppedAnHourPastItsExpiry`, `internal/sandbox.TestAnOlderBuildsScrubTakesTheRenewedValues`, `internal/sandbox.TestARenewalPastTheSetsBoundIsWrittenAndRefusesEveryLaterCapture`, `internal/sandbox.TestARenewalTheStoreDidNotAnswerIsKeptOutOfTheBox`, `internal/sandbox.TestALaterBuildsKeysInTheHandedSetSurviveARenewal`, `internal/sandbox.TestAWaiterThatRefreshesIsRefusedWithoutARecorder`, `internal/engine.TestBothWaitersKeepARunsTokenFileCurrent`, `internal/executordomain.TestAnEndWhoseTurnWentOnPastItsSessionKeepsIt`, `internal/executordomain.TestAnEndStampedBelowFourIsAppliedByTheRuleItWasWrittenUnder`, `internal/executordomain.TestARowKeptUpToASessionItsTurnWentPastSaysSo`, `internal/sandbox.TestAnEndOfATurnThatEndedOnASaveSaysItWentOn`, `internal/sandbox.TestAnEndWithoutWentOnNeverCarriesAnUnkeptCaptureNewerThanItsRow`, `internal/engine.TestAHeadItsTurnWentOnPastIsHandedTheNextTurnAsOne`, `internal/agent/runner.TestARunContinuingASessionItsTurnWentPastIsLedSo`, `internal/agent/prompts.TestANextTurnOnASessionItsTurnWentPastIsToldSo`, `internal/agent/prompts.TestACutOffSessionIsSaidTrueOfASaveAndOfAStop`, `internal/sandbox.TestAnEndOfATurnWhoseLaterLaunchKeptNothingSaysItWentOn`, `internal/sandbox.TestACaptureTheLogTookAfterARelaunchSettledItStaysTheTurnsSession`, `internal/executordomain.TestARowThatNamesNoSessionSaysWhichOfThreeReasons`, `internal/sandbox.TestALastWordTheLogTookAfterItsSettleIsReadAsTheTurnsLastWord`, `internal/executorkeep.TestAnEndingsLastWordIsPlacedFromTheFirstCapture`, `internal/executordomain.TestALiveRowNamingASessionReadsAsInProgress`, `internal/api/queries.TestARowKeptUpToASessionItsTurnWentPastCarriesItsNote`, `internal/api/queries.TestTheSessionsScreenReadsWhatTheAnswersSend`, `internal/agent/builtin.TestASeatIsToldOfASessionInProgressOrOneItsTurnWentPastAsAPersonIs`, `internal/executorread.TestALiveRowWithNoSessionYetIsSaidAsSuch`, `internal/executorread.TestASessionKeptAgainSinceItsCursorIsReadAgainInItsRowsWords`, `internal/sandbox/executor.TestARestoreCutsATornLastLineOfEverySubagentThread`, `internal/sandbox/executor.TestABoxWithoutFindResumesItsSessionAndSaysItCutNoThread`, `internal/sandbox.TestACutOffCaptureIsPlacedAtTheNewestSavePointItsLaunchIsKnownToHold`, `internal/sandbox.TestACaptureIsPlacedByItsLogReadOnlyWhereItsHarnessEndedItsOwnRun`, `internal/sandbox.TestOnlyACaptureItsLogReadPlacesReadsTheLog`, `internal/agent/prompts.TestACutOffSessionIsToldItsCallsAsOnesItMayAlreadyShow`, `internal/agent/runner.TestACarriedOnSessionIsToldWhatCameAfterItWasSaved`, `internal/sandbox/executor.TestASaveIsPackedToItsTakersOwnPathAndNeverTakenAsTheJobsFinal`, `internal/sandbox/executor.TestASaveOfAMemberRewrittenWhileItWasPackedFails`, `internal/sandbox/executor.TestASaveOfASessionThatOnlyGrewAsItWasPackedHolds`, `internal/sandbox/executor.TestAPackWhoseBoxClockNeverMovesIsRefused`, `internal/sandbox/executor.TestAFailedSaveLeavesTheLastOneStanding`, `internal/sandbox/executor.TestASaveIsNotPackedPastWhatTheScrubWouldRead`, `internal/sandbox/executor.TestTheClearBeforeEveryRunRemovesTheSavesFiles`, `internal/sandbox/executor.TestAPackMadeOnceItsHarnessIsGoneKeepsTheEndsRule`, `internal/sandbox/executor.TestAPackOnABoxWhoseSleepTakesNoTenthIsRefused`, `internal/sandbox/executor.TestAPackBesideAWorkingHarnessNamesAToolItsBoxLacks`, `internal/sandbox/executor.TestAPackProvesTheSessionsOwnTranscriptWhateverItsFindReads`, `internal/sandbox/executor.TestAPackSetsEveryFileOfItsSessionFromItsBoxsFutureToItsStartAndHolds`, `internal/sandbox/executor.TestAFileFromItsBoxsFutureRewrittenAsItIsPackedIsStillRefused`, `internal/sandbox/executor.TestTwoPacksBesideOneWorkingHarnessHideNoWriteFromEachOther`, `internal/sandbox/executor.TestAHeldPackWhoseChecksCannotBeMadeIsRefused`, `internal/sandbox/executor.TestTwoPacksOnDemandOfOneBoxAtOnceBothHold`, `internal/sandbox/executor.TestTwoSavesOfOneTakerAtOnceBothHold`, `internal/sandbox/executor.TestATakeOfAJobStillWorkingNeverMovesItsArchiveOverTheJobsCapture`, `internal/sandbox.TestASaveIsPlacedAtTheNewestSavePointItsLaunchIsKnownToHold`, `internal/sandbox.TestACollectionRecordsHowItsJobsHarnessEnded`, `internal/sandbox.TestAnOlderBuildsWriteCarriesAJobsHarnessEnd`, `internal/sandbox.TestPendingStoreContract`, `internal/engine.TestAResumeReadsHowTheJobsHarnessEnded`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldTheCallsItsCopyMayLack`, `internal/agent/runner.TestARunCarriedOnInItsBoxArmsItsJobsLoadsOnlyWhereItsHarnessEndedItsOwnRun`, `internal/agent/runner.TestARunCarriedOnInItsBoxLaunchesNothingWhenTheCallsItsCopyMayLackFillTheWindow`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldAgainEveryNoteTheTurnRead`, `internal/agent/prompts.TestACopyCutOffInItsBoxIsToldSoWithoutASave`, `internal/agent/prompts.TestNotesToldAgainAreOnesTheSessionMayAlreadyShow`, `internal/agent/runner.TestAnApprovedCallCarriedOnInItsBoxIsToldFirstThenWhatItsCopyMayLack`, `internal/sandbox.TestAReadIsNeverRecordedOverAWordThisBuildDoesNotKnow`, `internal/sandbox.TestAJobsHarnessEndIsRecordedAfterItsTakeWithinTheReadsRoom`, `internal/agent/runner.TestALaunchRecordsTheSkillsItArmedNeverALoadItsRunMadeAsItStarted`, `internal/sandbox.TestACaptureStatesTheNotesItHoldsOnlyWhereItsHarnessEndedItsOwnRun`, `internal/agent/runner.TestAKeptSessionCutOffIsToldAgainEveryNoteTheTurnRead`, `internal/sandbox.TestEveryStampOfACaptureStatesTheNotesItsSessionHolds`, `internal/sandbox/executor.TestTwoReadsOfAJobThatIsOverEachStreamTheirOwnPacksArchive`, `internal/sandbox/executor.TestTheJobsOwnEndPackLandingAsAReadStreamsChangesNothingItReads`, `internal/sandbox.TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceFails`, `internal/sandbox.TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceStopped`, `internal/sandbox.TestAReadWhoseReservationAnotherReadBeatStandsByForItsCapture`, `internal/sandbox.TestAReadStandingByKeepsNothingOnceTheCaptureItStoodByForLands`, `internal/sandbox.TestAReadStandingByThatCannotKeepWhatItTookIsWithdrawn`, `internal/sandbox.TestAReadStandingByWhoseTakerIsGoneIsWithdrawnNotWaitedFor`, `internal/sandbox/executor.TestAPackReportsItsArchiveMadeOnlyOnceItsDigestIsRecorded`, `internal/sandbox/executor.TestARestoreIsContinuedOnlyOnceItsDigestIsRecorded`, `internal/sandbox/executor.TestATakeOfASaveStreamsItsOwnWhateverAnotherSaveOfItsTakerLands`, `internal/sandbox/executor.TestAPacksReleaseIsHeldToItsTakesOwnDeadline`, `internal/sandbox.TestATurnsEndWaitsForAStandbysTakeoverAndKeep`, `internal/sandbox.TestALossReadStandsByForACaptureOfTheJobStillBeingKept`, `internal/sandbox.TestAReadWhoseReservationWentUnansweredBesideAnotherStandsByForIt`, `internal/sandbox.TestAFinalReadBesideACaptureTakenAsTheJobStoodTakesTheNextPlace`, `internal/sandbox.TestAReadStandingByTakesOverAnUnansweredCaptureOnlyOnceTheLogRefusesIt`, `internal/sandbox.TestAStandbyTheStoreNeverAnsweredKeepsTheSessionWhereItStands`, `internal/sandbox.TestATakeoverTheStoreNeverAnsweredIsKeptUnderWhereItWasMade`, `internal/sandbox.TestAReadStandingByThatTookNothingLeavesTheCaptureItStoodByFor`, `internal/sandbox.TestAWaitLeavesAReadStandingByWhoseTakerHolds`, `internal/sandbox.TestAReadStandingByTakesOverACaptureBegunPastItsBudget`, `internal/agent/runner.TestEveryContinuationOfTheRoundIsToldALateCall`, `internal/agent/runner.TestNoGuardCountsALoadTheLogTookLate`, `internal/agent/runner.TestALateSubmissionIsNeverTheRounds`, `internal/sandbox.TestALaunchNamesItsJobsRound`, `internal/sandbox.TestALongRunsLogKeepsALateCallWhileAnOrdinaryOneIsLeft`, `internal/sandbox.TestALateCallsMarkSurvivesABuildThatDoesNotKnowIt`, `internal/engine.TestALateCallReachesItsRoundsLogThroughItsSessionsLedger`, `internal/agent/runner.TestALateCallIsToldApartUnderWordsOfItsOwn`, `internal/agent/runner.TestALateSubmissionIsToldAsNoneOfTheRounds`, `internal/agent/prompts.TestALateCallIsToldUnderWordsOfItsOwnWhateverTheSessionsEnd`, `internal/agent/runner.TestADroppedLateCallIsToldPastEverySavePoint`, `internal/sandbox.TestALateCallTheMiddleDropsIsCountedLate`, `internal/sandbox.TestALogsCountOfItsDroppedLateCallsSurvivesABuildThatDoesNotKnowIt`, `internal/engine.TestALaunchedSessionsCallOutlivingItsJobIsListedInItsRound`, `internal/engine.TestAReopenedSessionsLateCallIsListedInItsRound`, `internal/engine.TestALateCallsAppendIsAskedOnceAndAnswersNoError`, `internal/engine.TestACallTheLogTookIsNeverSaidToHaveLostItsOutcomeWhateverBecameOfItsSpend`, `internal/engine.TestAResumeHandsTheRunnerHowManyLateCallsItsLogDropped`, `internal/sandbox.TestAnOpeningCountsLateTheCallsTakenAfterItsReadThatTheMiddleDropped`, `internal/sandbox.TestALaterAttemptAtAnAnswerIsHandedTheCallTheFirstMade`, `internal/engine.TestAnOpeningMarksLateWhatTheLogTookAfterItsBriefsRead`, `internal/engine.TestAResumeHandsTheRunnerTheCountItsReadSaw`, `internal/agent/runner.TestARelaunchHandsItsLaunchTheReadItsBriefsWereBuiltFrom`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldTheCallsTheLogTookLate`, `internal/agent/prompts.TestALateCallTheRecordDroppedIsCountedUnderTheLateCallsWords`, `internal/engine.TestACallLandingWhileARelaunchIsReadiedIsToldToTheNextContinuation`, `internal/engine.TestAResumesReadReachesItsRelaunchsOpening`, `internal/agent/runner.TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnce`, `internal/engine.TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnceOffTheRoundsLog`, `internal/engine.TestWhatTheLogKeptOfACallIsWhatTheResumedTurnReads`, `internal/agent/runner.TestAMadeCallWhoseResultCannotBeReadHereIsToldAsMade`, `internal/engine.TestALaterAttemptAnswersAHeldCallFromTheMarkItsClaimRead`, `internal/toolsession.TestACallItsHarnessGaveUpOnReachesTheDispatcherAsAbandoned`, `internal/toolsession.TestACallWhoseResponseIsGoneReachesTheDispatcherAsAbandoned`, `internal/toolsession.TestACallItsHarnessCancelsReachesTheDispatcherAsAbandonedWhileItsAnswerIsKeptGoing`, `internal/api/mcpbridge.TestACallNobodyWaitedForIsRecordedSo`, `internal/api/mcpbridge.TestEveryEndingOfAnOpeningReturnsWhileACallIsHeld`, `internal/api/mcpbridge.TestACallWhoseToolReturnedBeforeItsOpeningEndedIsAnsweredWithItsOwnResult`, `internal/sandbox.TestALongRunsLogKeepsACallNobodyWaitedForAndCountsOneItDrops`, `internal/agent/runner.TestACallNobodyWaitedForIsToldPastEverySavePoint`, `internal/agent/runner.TestNoGuardCountsALoadNobodyWaitedFor`, `internal/engine.TestACallNobodyWaitedForIsToldToTheCopyInItsBox`, `internal/engine.TestACallNobodyWaitedForThatLandsOnNoRecordIsNamedAtWarning`, `internal/api/mcpbridge.TestACallIsOnItsRunsRecordBeforeItsToolRuns`, `internal/api/mcpbridge.TestACallWhoseEntryCannotBeWrittenIsNotRunAndIsRefused`, `internal/api/mcpbridge.TestACallWhoseEntryIsStillBeingWrittenIsNeverRunWhereItsOpeningEnds`, `internal/api/mcpbridge.TestAHeldCallIsRecordedWithoutItsArgumentsOrTheirDigest`, `internal/engine.TestAnUpdateTheStoreRefusesLeavesTheCallsIdentityOnRecord`, `internal/engine.TestABridgedCallsWritesOutlastALeadersReElection`, `internal/engine.TestACallWhoseEntryTheLogDoesNotTakeIsNamedAndRefused`, `internal/agent/runner.TestACallStillInFlightIsCountedByItsIdentityOnEveryPath`, `internal/agent/runner.TestACollectedRoundListsACallWhoseOutcomeIsUnknown`, `internal/sandbox.TestACallInFlightIsHeldToItsShare`, `internal/sandbox.TestTheWholeLogFitsItsBudget`, `internal/engine.TestACallsRecordIsUpdatedOnceByItsIdentity`, `internal/engine.TestAResumedTurnsDeliveryIsUnknownBesideACallWhoseOutcomeIsUnknown`, `internal/sandbox.TestEveryCollectionWaitsForACallInFlightBeforeItReadsTheLog`, `internal/sandbox.TestAWaitStopsAtTheCapAtACallsBoundAndAtItsProcessGone`, `internal/sandbox.TestAWaitWhoseWatchStopsHearingWatchesAgainAndMissesNothing`, `internal/sandbox.TestAParkedJobsSessionIsEndedAtItsCollection`, `internal/sandbox.TestAnEndingThatMayNotWaitHandsACallStillWaitedForToItsRetry`, `internal/sandbox.TestASeatsRetirementLeavesARunWhoseCallStillRunsToItsNextTick`, `internal/engine.TestACallStillRunningAsItsHarnessEndsItsRunCountsInTheTurnsOutcome`, `internal/engine.TestAnAnsweredParkContinuesOnAFreshJobAndSession`, `internal/api/mcpbridge.TestTheRunsCallLogCountsOnlyTheCallsItsRecordHolds`, `internal/api/mcpbridge.TestSideBySideCallsAreAdmittedInTheOrderTheyArrived`, `internal/api/mcpbridge.TestAnUpdateTheLedgerCouldNotLandIsAskedAgainOnceItsAnswerIsOut`, `internal/engine.TestASubmissionWhoseUpdateTheStoreRefusedLandsOnceTheStoreIsBack`, `internal/sandbox.TestEveryEndingWaitsForACallInFlightBeforeItDecidesTheTurnsEnd`, `internal/sandbox.TestACappedRunNamesACallInFlightAtOnceAndAsItsRecordGoes`, `internal/api/mcpbridge.TestEveryRecordOfABoxsCallCarriesTheIDItsHarnessGaveIt`, `internal/api/mcpbridge.TestACallIDIsReadOnlyUnderItsRunnersKeyAndOnlyWhole`, `internal/sandbox.TestACallsHarnessIDSurvivesABuildThatDoesNotKnowIt`, `internal/sandbox.TestABridgedCallIsHeldToItsShare`, `internal/sandbox/executor.TestEachBoxRunnerDeclaresTheCallIDKeyItsHarnessProfileReads`, `internal/engine.TestABridgedCallCarriesItsHarnessIDFromItsEntryToItsRecord`, `internal/engine.TestARecoveredRunsCallsCarryTheIDsTheirHarnessGaveThem`, `internal/e2e.TestAnExecutorThatAsksAQuestionParksAndResumesOnTheAnswer`, and the `internal/sandbox/sandboxtest` contract cases `ALateCallOfTheRoundIsListedMarkedAndPaidByNoJob`, `ALateCallAskedAgainIsListedOnce`, `ALateCallKeepsItsMarkThroughEveryWriteOfItsRound`, `ALateCallTouchesNoRecord`, `ALateSubmissionNeverTakesTheRoundsWholeShare`, `OfTwoSubmissionsAtOnePlaceTheOneTakenLastIsKeptWhole`, `TheCallAPersonApprovedIsNeverLate`, `ACallTakenAfterTheBriefsReadIsMarkedLateByTheOpening`, `ACallNobodyWaitedForKeepsItsMarkAndIsPaidByItsJob`, `ACallsEntryIsUpdatedOnceByItsIdentity`, `ALogAtItsCallsInFlightRefusesTheNextBegin`, `ALaterCallIsNumberedAfterACallStillInFlight`, `WhatALateCallOrOneNobodyWaitedForCommittedIsTheRounds`, `ABridgedCallOutlivingItsRoundIsNotTheNextRounds`, `ACallCarriesItsHarnessIDWholeAndIsNeverFoundByIt` and `TheCallAPersonApprovedIsACallOfItsRound`, which the pending-run store runs; `internal/sandbox.TestACallIsHeldOnlyThroughItsLoggedResult`, `internal/sandbox.TestHoldingsAreReadOnlyOfTheCaptureItsRelaunchContinues`, `internal/sandbox/executor.TestParallelResultsAfterTheLastRestartAreLive`, `internal/sandbox/executor.TestAResultBeforeTheLastRestartIsNotLive`, `internal/sandbox/executor.TestASubagentIsReportedOnlyWhereEveryLegWasAnsweredAndFinished`, `internal/sandbox/executor.TestAReadingDecidesOnlyOnceItsStreamEnded`, `internal/sandbox/executor.TestWhatMembersHandOutIsHeldToTheArchivesBudget` and `internal/engine.TestTheRestoreOpensTheObjectItsRowNamesAsItStreams`, `internal/sandbox/executor.TestAResultItsHarnessReplacedIsNotLive`, `internal/sandbox/executor.TestNoIDTheCallIDRuleRefusesIsKept`, `internal/sandbox/executor.TestAResultIsLiveOnlyWhereTheResumeHandsItsModelItsRecords`, `internal/sandbox/executor.TestAMainThreadWhoseLeafTheReadingCannotFollowIsUnread`
- **Measured:** a harness's session of a day's work compresses to one or two mebibytes and is bounded here at 32 MiB; at the reference company's 3 000 turns a day the log that names the sessions grows about 32 MB a day, about a kibibyte of it each turn's first read of its conversation (its `begin`), while the sessions themselves would be gigabytes; crewlet-operator's smallest data volumes leave 448 MiB (12Gi with the objects in S3, and 16Gi beside the nats bucket) above the other logs at their floors, and this log, scaled to its 64 MiB floor there, still fits both — the minimum classes do not move, but the least a fresh node boots in rises by 64 MiB of logs (12348030975 bytes of broker on nats, up 85.3 MiB with the bucket's quarter beside it, and 64 MiB on S3), which is what the operator's own floors follow. At that floor the log holds about two days at the reference company's rate, under the seven-day `min_age` a healthy trim keeps, so a company above about thirty seats on those volumes sets `stream.executor_log_max_bytes` or a larger volume, and `log_ceiling_short` says so; a continued Claude Code session hands its model what a reading of its kept archive counts on, as probe T measured 2.1.310 and 2.1.322 continue one: 14 continuations a release, 25 whole-line results after the last restart among them and 4 of those off the chain parallel calls leave, every one carried once with its call, and a result the harness wrote itself only for a call its file held none of; five continuations each carrying a seat server's call and its result whatever its server's state; a task notification carried verbatim; every path behind a flag Anthropic's servers set read in both releases' shipped code, not run; the chain a resume hands its model, its leaf and what it recovers off it read in both releases' shipped code, and the rule the reading builds on it refusing none of the 264 main threads and leaving not whole only the one torn of the 89 subagent threads the probes kept; and a reading of ordinary lines at its bounds reads in 1.8 s, of the slowest lines measured about 10 s by their rate, of a thread at the most records it walks the chain of in 1.4 s, and holds about 120 MiB at its worst by the sum of its two worsts, an ordinary session in 52 ms (`BenchmarkAReadingAtItsBounds`, on two cores)
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

**Placed at the newest save point the launch is already known to hold.** The
take of a job still working (one its box would not end, packed on demand) and
every capture of a job cut off are placed at the save point of the launch's
own newest landed capture — the row's kept capture only where it is that
launch's, its job and its ordinal both, since before the launch's first capture
lands it is an earlier job's, one this launch may never have continued — and at
zero, the round's start, where no landed capture of the launch states one
(internal/sandbox's `placeAtNewest`). Each point is sound by itself, since what
a session holds only grows, and a continuation is told every call after it, a
subagent's included — some more than once, none never. On a fleet of this
version alone that point is zero: a capture placed by its read is final and
nothing of its launch is taken after one, so the landed captures of a launch
before one placed so were placed so too, and a launch records on its row no
capture it continued, so one that carried its round on from a capture is placed
at zero as well. A point above zero is one another build stamped, in a fleet
mid-upgrade. The places an older build reads a save point by (`bridge_seq`,
`bridge_open`) are copied off the same capture, or reset where there is none,
so a peer reading them is never told less either.

**What it costs.** A continuation of a capture cut off is told every call
after its point, those its session holds among them — on a fleet of this
version alone every call of its round, within the prior-work ledger's budgets —
so its brief is the longer for it, and the floor its launch is held to, and any
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
archive yields a kept session's scrub (eight times `SessionMaxBytes`) before
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
capture whose harness did not end its own run, at the newest save point its
launch is known to hold (internal/sandbox's `placeAtNewest`): the newer of its
launch's own newest landed capture's and, where the launch carried its round
on, that of the capture whose archive the launch's session started from, which
the save is handed since no row records it — every call that archive held, the
session holds — else zero.

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
leaves the take's own records (`recordsBy`): `BoxReadBudget` holds no time of
its own for it, so a store slow to answer it costs neither the take nor the
box's pause their time, and a record that does not land is never read as an own
run. The read's result carries it back (`Result.HarnessEnd`) to the claim it
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
of it — only the calls the log took late, which no copy is known to hold (the
amendment after this one), and the notes waiting — and armed with its loads, as
before.

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
capture cut off that lacks its own, lack the lead that brief opened with too —
the turn's task where the job continued the session its conversation's last
turn left, a review that sent the round back, an earlier answer — which the
engine keeps no copy of to tell again. And a fresh run carrying a round on is
told none of the round's calls, as before (`Lost.Reseed`, `Asked.Reseed`,
`Rotated.Reseed`).

**What it costs.** The told calls are held to the prior-work ledger's budgets,
built before the relaunch's last ask of the seat's budget, so a rewrite of a
long call by the seat's auxiliary model is charged before anything is launched;
none is built where the job's box is gone. The notes are told whole, as every
brief tells them. The brief is the longer for every call of the round and every
note the turn has read, for every copy not known to have ended its own run,
which includes every relaunch after a replaced sign-in whose refused harness
exited with a status other than 0, and every job a build before this one
collected; and a continuation of a kept capture cut off is the longer for every
note the turn has read. Where a capture measured the copy's context — its job's
newest, kept or not — the floor its launch is held to counts that brief and
grows with it: where it is past what the seat's capped windows or its model's
window can carry, the launch does not continue the copy, continuing the turn's
kept capture instead where it offers one whose own floor fits (ADR-0066's
amendment), and otherwise starting a session of its own, which is told none of
the round's calls; and a launch that offers both is held to the larger of their
floors, since its box takes up the copy only where it cannot restore the
capture. A copy no capture measured — every copy in a company that keeps no
session — holds its launch to the static floor whatever its brief, and only its
harness's own compaction bounds what that brief adds.

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
whatever its point, and again by every later one, in a section of its own under
words that hold whatever the session's end — it finished only after the run that
made it was replaced, the session may show it with no outcome or as interrupted,
or an earlier brief may have told it, and it is one call, made once, whose
outcome as told stands (internal/agent/prompts' `lateCallsHeader`) — the telling
counted in the continued launch's floor where a capture measured the session;
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
round but the late ones, wherever that brief told the round's calls (a fresh
run's brief tells none), and, since the amendment on a call nobody waited for,
but one its harness gave up on, which it holds only as timed out — so the copy
is told the late ones alone, under their
own words, with the line on a late submission and how many of them the log
dropped from its middle, as every continuation counts them; where the harness
may have been cut off, the copy is told every call of the round, the late ones
apart. Its guard counts none of a late call's loads, and the telling is in the
brief the copy is continued with, which its launch's floor counts where a
capture measured the copy. So every continuation of the round built after a
marked call landed is told it, in a kept session and in the job's own box alike.

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
`runner.Continuation.BriefRead`, the row's transient `PendingRun.BriefRead`),
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
carrying the round on, or the one a relaunch was readying as it landed — is not
told it while it runs: every continuation built after it lands is. And the copy
of a job that was a fresh run carrying the round on, continued in its box after
its harness ended its own run, is told the late calls alone, as every such copy
is, while it holds none of the round's earlier calls, which that fresh run was
told none of (the amendment on the copy a job's own box holds). And what a late
call committed counts in the turn's outcome only where the log takes the call
before its round's last collection reads the log, which waits for every call of
the round in flight up to the first of its own bound, the turn's cap and its
process gone (the amendment on a collection's calls in flight, below): one whose
update lands later is in no outcome.

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
session continued since may not hold it. A refused reply names no held call, and
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
the approved one, the held call among them. A fresh run carrying a round on is
told none of the round's calls, as before, so it never listed the call.

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
harness's end, which where that harness ended its own run is told such calls and
the late ones alone — in the late calls' section, whose words now say the run
that made them stopped waiting for them: it gave up on them, or was cut off or
replaced, so the session may show one with no outcome, as still running (the
answer an opening's end hands a call it does not wait for), as interrupted or
as failed (internal/agent/prompts' `lateCallsHeader`). No rebuilt skill guard
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

**What it costs.** Each such call is told to every continuation of its round,
in its brief and so in the continued launch's floor where a capture measured
the session, and again by every later one. The harness of a call still running
as its opening ends is handed the end's answer at once, before the end closes
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
identity; told to a run carrying the round on beside its late calls; and beside
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
inside one wait taken before the run's record is read (`holdingsReadWait`, 15
s, the keep's own patience, so a relaunch's frame spends 30 s on a kept
session, and the one bounded step a reading may overrun its wait by); only of
the capture that record keeps, cut off, where `ContinuityOf`
still continues the turn's row with the runner that reads it; through that
runner (`sandbox.HoldingsReader`, the executor's `Runner.ReadHoldings`, which
reads Claude Code's sessions alone), handed an opener that streams the row's
object (`SessionRestore.Open`). It answers facts, and one rule alone turns them
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

**What it costs, and its bounds.** A second object-store read and opening of
the object the launch that continues the session fetches again: at most
`executorkeep.SealedMaxBytes`, ordinarily a mebibyte or two, streamed and never
spooled, since a spool of its own would sit on the node's scratch disk beside
the launch's, which is not what crewlet-operator sizes an engine pod's scratch
volume by, and one handed on to the launch would change the fetch it copies.
The archive's plaintext is held to `threadPlainMax` (256 MiB) by the scrub's
meter, and what the members it reads hand out to the same budget, which a
sparse member's holes cannot pass; a line to `threadLineMax` (8 MiB); the
writer to `holdingsWriterFloor` (2.1.310, the oldest release its rules were
measured on; a writer below it, unknown or unreadable, is no reading);
subagents to `holdingsMaxAgents` (1,024), legs to `holdingsMaxLegs` (4,096), a
metadata to `holdingsMetaMaxBytes` (64 KiB), a description to
`holdingsDescriptionBytes` (1 KiB), the records of a thread whose chain it
walks to `holdingsMaxRecords` (262,144, twice the 130,754 lines of 2,053 bytes,
probe P6's average, the plaintext's bound holds; one thread's at a time); and
the ask to `sandbox.MaxBridgeCalls` (200). Every id it keeps is one the log's
call-id rule holds: an id is the box's word, as long as a line, and a call
under any other is one nothing can link, answer or notice. It reads in the
seats' one transcript slot, inside the process's two, with no pool of its own
(`SeatTranscriptReadsAtOnce`), and waits for it inside its wait. Measured
(`BenchmarkAReadingAtItsBounds`, run once on two cores): an ordinary session, a
1.6 MiB archive and 5.6 MiB of lines, reads in 52 ms holding 1 MiB live; one at
the archive's bound, 118 MiB of lines, in 1.0 s; 255 MiB of ordinary lines, the
plaintext's bound, in 1.8 s holding 9 MiB live; 1,024 subagents in 81 ms; 248
MiB of lines at the line's bound in 1.0 s holding 26 MiB live; its worst for
one line, a line at that bound of the smallest results a message holds, every
one of which the layer keeps, in 0.21 s holding 88 MiB live, its heap peaking
at 94 MiB; its worst for a thread's chain, a thread at `holdingsMaxRecords`,
each record a reply of its own id naming a parent that comes later, in 1.4 s
holding 35 MiB live, its heap peaking at 69 MiB; so about 120 MiB for a thread
holding both, by their sum (computed, not measured), within what a seat's read
for words holds at its worst in the same slot. The ids the call-id rule refuses
cost it nothing: 96 Agent calls under ids of a mebibyte, and 1,024 metadata
naming ids of 65,000 bytes, which held 97.7 and 65.3 MiB live before the
reading kept no such id, read in 0.52 s and 0.15 s holding 6 and 2 MiB. And the
slowest lines for their size, a line at the line's bound of blocks of a kind no
reader names, read 8 MiB in 0.30 s. So a burst of 32 relaunches reads ordinary
sessions in about 1.7 s on the one slot, and an archive of the slowest lines to
the plaintext's bound, which gzip shrinks to almost nothing, reads in about 10
s by their rate (computed, not measured whole): inside its wait, though on a
loaded node it holds the seats' slot for most of that wait.

**The rules it reads by.** Rule 1: a call counts only through whole lines. A
restart (a compaction's boundary, a microcompaction's, a span replaced, a root
after a thread's first chained record, a first chained record that roots no
chain) starts what a resumed session is handed over, and a result is live only
past its thread's last restart, and only WHERE CLAUDE CODE'S RESUME HANDS ITS
MODEL the records of its call and its result, as both releases build what a
resumed model is handed: the chain the resume walks from its leaf, parent by
parent, until a parent that is null, empty or absent; and off it only the other
records of a reply on it, found by the reply's id (an id holding the
replacement character, which the decode makes of a lone surrogate the harness
compares as written, recovers nothing), and a result whose parent is
one of them, which is how the results of calls made at once reach the model
(probe T measured them carried), and never a record of another reply, so an
attempt the harness withdrew and left in the file holds nothing. The leaf of a
main thread is the nearest user or assistant record up from its last record,
which the resume starts from wherever nothing moves it: so a main thread where
a checkpoint a rewind, a withdrawal or a fork marked, one its last record does
not descend from, or a record the resume keeps out of a session's last record's
place (a sidechain's, a fork's briefing, a file's snapshot, a reply of string
content) could move it is no reading; where a compaction cut it, a checkpoint
that is a user or assistant record before that leaf is taken for the leaf,
since the resume then picks its leaf among the thread's leaves; and the walk
never crosses the last restart, since a compaction that kept a segment deletes
what lies before its boundary from what the resume walks. Where the walk cannot
be read (a parent naming no record, which the harness replaces by a guess from
timestamps, or a record already walked), it ends there and what lies past it is
told; a record of no lowercase canonical uuid, or of another's, is no reading,
and so is a progress record, a legacy shape none of the files the probes kept
holds, whose uuid Claude Code's load hands the records after it that name it
to the progress record's own parent, a link the reading does not follow.
A subagent's thread is walked from its last record of its own, what its model
had seen when it answered, and a walk that does not reach its root leaves it
not whole. A result is live only past its call's use in its own thread, since
the API refuses a result no call of its own precedes. A result its harness
replaced for its model is live in no thread: behind a flag Anthropic's servers
set, Claude Code hands its model a preview in place of the largest results a
reply's calls returned past a budget, records the replacement beside the
result's own record, which keeps the whole output, and hands the preview again
on resume. Rule 4: a call is held only through its one counted result, whose
`is_error` is the log's failure and whose text is the output the log took,
compared by SHA-256 digests so no tool output leaves the archive: every byte of
it where the log kept the output whole (up to `MaxBridgeOutputBytes`, 4 KiB),
and else its length and its first 256 bytes, inside the 672 bytes of its
opening the log always keeps of an output it cut (`HoldingsOpeningBytes`). Rule
3, a result a process wrote without the call's outcome (a resume's for a call
it found open, a stopped harness's, a call its harness gave up on), is met by
rule 4, and no mark such a result carries is read. Rule 2: a call in a
subagent's thread is held only where the reading reports that subagent: its
metadata, read by exact keys, links it to exactly one Agent or Task call of the
main thread; its thread is whole and never restarts; each leg (that call, and
each SendMessage to it its own result did not refuse) has a live answer in the
main thread, the call's own completed foreground answer or a task notification,
which a continuation hands its model verbatim (probe T, T10), and a SendMessage
under an id the log keeps none of is a leg nothing answers; its thread starts
no more legs than that; and each leg ends on a reply its model ended its turn
with, its stop reason `end_turn`, no API error and a model other than
`<synthetic>`, Claude Code's own whole test of one: a request that fails for
good leaves a reply the harness writes itself, its text the error's, on which
it ends the subagent and answers the call that started it as completed, though
no model read what that request carried, and every reply the harness writes in
place of a model's is of the model `<synthetic>`. A call in an unreported
subagent's thread is told under that subagent's heading. A seat server's call
needs no rule of its own: every continuation probe T made carried the call and
its result whether its server answered, was left out, held its `initialize`,
refused it, or no longer offered the tool (T6).

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
one use or one result. A later release whose resume drops a result it hands
over today, or a path behind a flag Anthropic's servers set that no read has
found: probe T ran with every such flag off, so each was read in both releases'
shipped code rather than run, and each release's probe checks again. What the
resume hands over and the reading does not count is told, the safe direction: a
result it recovers behind such a flag through its call's record where that is
not its parent, and what it links past a parent naming no record by a guess
from timestamps.
