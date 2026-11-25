# ADR-0166: The Advisor investigates in rounds over the engine's own reads, and states every finding with its numbers

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:**
  - `internal/advisor.TestASnapshotsNumbersAreTheRecordsItReads`
  - `internal/advisor.TestASourceASnapshotCouldNotReadIsNamedAndLeftOut`
  - `internal/advisor.TestEveryClassSaysWhatItConcluded`
  - `internal/advisor.TestTheStaffingClassesSayWhyTheyCouldNotJudge`
  - `internal/advisor/duty.TestARunRecordsWhatItConcludedAboutEveryClass`
  - `internal/api/queries.TestAFindingLeadsWithItsHeadlineNumbersAndOnePrimaryAction`
  - `internal/api/queries.TestARunsSummaryNamesWhatItConcludedAboutEachClass`
  - `internal/api/queries.TestASnapshotIsReadOverItsRangeAndTheOneBefore`
  - `internal/advisor/modelpass.TestADeepRunInvestigatesAndListsOnlyWhatItsReadsShow`
  - `internal/advisor/modelpass.TestADeepRunNearItsBoundIsToldToSubmit`
  - `internal/advisor/duty.TestADeepRunRunsBesideTheTick`
  - `internal/advisor/duty.TestAHolderThatLosesTheLeaseStopsItsDeepRun`
  - `internal/advisor/duty.TestADeepRunIsDueNightlyAndEarlyOnMonday`
  - `internal/advisordomain.TestARunSaysWhatItDoes`
  - `internal/api/queries.TestTheDeepRunReadsOnlyItsAllowlist`
  - `internal/api/queries.TestTheDeepRunAsksAsTheAdvisor`
  - `internal/api/queries.TestADeepRunsSummarySaysWhatItDid`
  - `internal/advisor.TestAPremiseIsGroundedAndMustHoldWhenSubmitted`
  - `internal/advisor/duty.TestAFindingWhosePremiseNoLongerHoldsIsResolved`
  - `internal/api/queries.TestAPremiseIsAskedAgainAsTheAdvisor`
  - `internal/advisordomain.TestADeepRunsProgressIsReadWhileItRuns`
  - `internal/api/queries.TestADeepRunInFlightIsTheLatestRunWithItsProgress`
  - `internal/advisor.TestCapacityIsTheHoursEachEntryWasOut`
  - `internal/advisor.TestAModelEntryOutForHoursIsOneFindingWithItsSeats`
  - `internal/advisor.TestASnapshotDrawsTheHoursEachEntryWasOut`
  - `internal/api/queries.TestAParkStretchIsReadOnceHoweverOftenItWasRenewed`
  - `internal/advisor.TestAKindOfWakeThatMostlyChangedNothingIsOneFinding`
  - `internal/advisor.TestSeatsWhoseTurnsCostSeveralTimesTheCompanysAreOneFinding`
  - `internal/advisor.TestASeatWhoseSchedulesKeepItBusyWhileItsWorkWaitsIsAFinding`
  - `internal/advisor.TestEveryClassLeadsWithItsNumber`
  - `internal/advisor.TestEveryClassSaysOneThingAboutEverySubject`
  - `internal/advisor.TestAModelFindingThatRestatesARuleFindingIsADuplicate`
  - `internal/advisor/duty.TestAModelFindingARuleFindingSaysIsClosedAsADuplicate`
  - `internal/api.TestOneCauseOnManySeatsIsSentAsOneActionOfSeveralChanges`
  - `internal/advisor.TestASeatHoldingItsTwinsShareOfTheWorkIsMovedToIt`
  - `internal/advisor.TestWorkBlockedOnNothingAnyoneHoldsIsOneFindingThatAsksWhy`
  - `internal/worklet.TestOpenWorkNamesWhatIsBlockedOnNothingAnyoneHolds`
  - `internal/advisor/modelpass.TestTheDeepRunAnswersItsWholeAgendaInFewTokens`
  - `internal/advisor/duty.TestAMovedPremiseOnWorkStillBlockedIsReplacedNotResolved`
  - `internal/advisor.TestAPartialReadingOpensWhatItSawAndClosesNothing`
  - `internal/advisor/modelpass.TestARunsRecordKeepsWhatFitsWhateverItDid`
  - `internal/advisor/duty.TestARunWhoseRecordIsRefusedConcludesAsFailedAndIsNotRunAgain`
  - `internal/advisor.TestACostFixStandsOnlyWhereFreshTokensDroveThem`
  - `internal/advisor.TestAModelsNumbersReadAsTheEngines`
  - `internal/api.TestAReassignmentIsRecheckedAndMovedItemByItem`
  - `internal/advisordomain.TestADeepRunsNewestCheckpointIsWhatATakeoverReads`
  - `internal/advisor/duty.TestATakeoverResumesFromTheCheckpointTheLostHolderWrote`
  - `internal/advisor/modelpass.TestADeepRunTakenOverMidAgendaResumesWhereItStood`
  - `internal/advisordomain.TestARunsWholeTrailIsRead`
  - `internal/api/queries.TestAFinishedRunKeepsItsWholeTrailReadable`
  - `internal/advisor.TestAProposalWhoseFixIsNotBuiltStillStands`
  - `internal/advisor.TestWordsTheRuleRefusesAreSaidByTheEngine`
  - `internal/advisor.TestTheRulesReasonsAreSaidSoAModelCanRewrite`
  - `internal/advisor/modelpass.TestADeepRunRewritesWhatTheWordingRuleRefused`
  - `internal/advisor/duty.TestOnePressIsOneRunOfTheChecksAndTheInvestigation`
  - `internal/api/queries.TestAPressesRunSaysWhyItsDeepPartDidNotRun`
  - `internal/advisor/duty.TestAnOrphanOfTheCurrentSlotIsResumedUnderItsOwnId`
  - `internal/advisor/duty.TestAnOrphanedSlotRunIsRecordedInterruptedOnTheNextTick`
  - `internal/advisor/duty.TestAHolderWhoseRenewalsFailForTheLeaseTTLCancelsItsDeepRun`
  - `internal/advisordomain.TestARunWhoseHolderStoppedIsUnownedUntilARecordClosesIt`
  - `internal/api/queries.TestTheLatestRunPrefersNewerTruthOverAStaleLiveRun`
  - `internal/advisordomain.TestAProgressLineSaysItsStepThroughTheLog`
  - `internal/advisor/modelpass.TestADeepRunsStepsSayWhatTheyAreAndWhyOneFailed`
  - `internal/api/queries.TestARunReadsAsItsSteps`
- **Measured:** on a live company of nine agent seats over 48 hours, the model entries reported a usage limit about 25 times and one sat out roughly eleven hours of one day; the Advisor ran sixteen classes in about four seconds, one model pass of 85,000 tokens over a fixed bundle, and named no capacity loss at all. The same company's lead-fallback wakes changed nothing in 124 of 185 turns, which no finding stated with its number, and one condition on nine seats was nine identical rows.
- **Cost-when-tried:** the model pass of ADR-0164 reads only what the duty gathered before it ran (`internal/advisor/modelpass`), so a lead outside that bundle (a limits timeline, schedule fires per seat, tokens per turn) could not be followed; the staffing classes stay silent on any seat whose time was not measured every day, and the run recorded them as concluded, so a company that moved its seats between model entries mid-week read as well staffed.
- **Tag-status:** unreleased

## The decision

**The Advisor states what it finds the way an analyst does, and says what it could not judge.** Every finding leads with a headline that carries its number, the stats behind it, a chart where one shows it, the seats it is about inside it, and one primary action (apply a change, reassign work, open the place to act, or ask Crewlet to draft it) with any others beside it. Every rule run records, for every class it runs, whether it found something, judged the company clear, could not read its source, or read it and still could not judge all of the company, and then why, in words. The page reads the company's numbers over 7, 14 or 30 days from the same records the detectors read (`GET /advisor/snapshot`), each beside the range before it, and a source that could not be read is named and its numbers left out, never drawn as zero.

**The deep run investigates in rounds, read-only.** On its model, on every press and on a schedule, it reads the snapshot, then the engine's registry of read questions (the same answers the dashboard reads) as the Advisor's own engine principal over an allowlist, follows leads and tests them against the data, and only then submits findings; a grounding pass drops any whose numbers, records or actions its reads did not show. It is bounded by tokens, wall time and rounds an administrator can change, runs one at a time, says what it is doing as it goes, and resumes after a takeover from its checkpoint: its agenda with each verdict, and the reads it asked. A run whose holder stopped and that no holder resumes is recorded interrupted, never left reading as running. The deep run lands in later changes of this record; the shape above is fixed first so the page can build on it.

**One press, one run.** A person presses Run and never chooses a kind of run: the checks run at once and their findings are recorded as they are found, then the run goes on into the investigation under the same id and progress line, recorded once with both parts. The investigation is kept, not run again, where one concluded within six hours (the nightly one serves the working morning, and a company pressing all day pays for at most four beside it, each within its bound), and cannot run where no model is assigned; the run records which, and why. What the model proposes reaches the page: a fix the engine cannot build leaves the finding listed with a plain line instead, and words the wording rule refuses are sent back in the same run with the rule's reasons, then said by the engine from the finding's facts. Only a number, a record or a premise that does not hold drops a finding, and the run counts what it mended beside what it dropped.

The engine still never sends a fix (ADR-0115), and ADR-0164's rules for keeping, re-deriving and re-checking findings stand.

## Why the obvious alternatives are wrong

**A larger bundle for the one-shot pass.** Every bundle is a guess at what the next company needs; the one this record answers was missing exactly the series that mattered. Reading the registry on demand costs only what is read, and grounding keeps the model to it.

**Reading over HTTP with a person's grant.** A scheduled run has no person and no session, and a minted credential is one more secret on the wire. The registry answers in process, checks each question's posture as it always does, and answers no credential or secret in clear.

**Skipping a class that could not judge without saying so.** A silent class and a clear one read the same, which is how a company losing half its capacity read as fine.
