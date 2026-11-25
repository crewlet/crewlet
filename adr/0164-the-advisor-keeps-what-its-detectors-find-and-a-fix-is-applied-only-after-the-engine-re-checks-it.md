# ADR-0164: The Advisor keeps what its detectors find, and a fix is applied only after the engine re-checks it

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:**
  - `internal/advisor.TestNoDetectorCallsAModel`
  - `internal/advisor.TestEveryClassSaysOneThingAboutEverySubject`
  - `internal/advisor.TestEveryClassHasWordsAndAFix`
  - `internal/advisor/modelpass.TestTheModelPassListsOnlyWhatItsOwnReadsShow`
  - `internal/advisor.TestAReaderWithoutTheAtlasAppIsToldHowManyAndNeverWhich`
  - `internal/advisor.TestEveryFindingOfAnAnswerHasAKeyOfItsOwn`
  - `internal/advisor.TestOneOpenFindingPerClassAndSubject`
  - `internal/advisor.TestAFindingWhoseConditionEndedIsResolvedOnTheNextRun`
  - `internal/advisor.TestAFindingWhoseVerdictMovedIsRevised`
  - `internal/advisor.TestAFindingIsStaleAfterThreeMissedRunsAndExpiresAfterSevenDays`
  - `internal/advisor.TestAnAppliedFixIsCheckedForItsEffect`
  - `internal/advisor/duty.TestEachClassRunsOnItsCadence`
  - `internal/advisor/duty.TestAConfigActivationRunsTheSetupClassesNow`
  - `internal/advisor/duty.TestARunNowIsRunByTheHolderUnderItsId`
  - `internal/config.TestTheAdvisorIsConfiguredByOneModel`
  - `internal/api/queries.TestTheAdvisorNamesAnAtlasEntryOnlyToAHolderOfTheAtlasApp`
  - `internal/advisor.TestApplyRechecksAndAnswersHoldsChangedResolvedOrNotApplicable`
  - `internal/advisor.TestAnAppliedFixThatDidNotClearTheConditionIsNeverOfferedAgain`
  - `internal/api.TestTheSendIsBoundToTheRevisionTheRecheckRead`
  - `internal/api.TestAChangedFixIsShownBeforeItCanBeSent`
  - `internal/api.TestAResolvedFindingIsNeverApplied`
  - `internal/advisor/duty.TestTheWeeklyReportCountsWhatTheWeekOpenedResolvedAndApplied`
  - `internal/advisor.TestAnExplanationSaysOnlyItsFindingsOwnNumbersAndRecords`
  - `internal/advisor/modelpass.TestThePassExplainsAFindingOnlyInItsOwnNumbers`
  - `internal/advisor/duty.TestAnExplanationIsWrittenForTheVerdictThePassRead`
- **Measured:** every read the model pass makes answers about 21,000 tokens for a twelve-agent company's busy week, and a pass reading in one round spends 60,000 to 80,000, under its 200,000 ceiling. On one company an agent seat's mailbox held more than 300 wakes for hours while other seats sat idle, and no finding named it: no record said how long work waited for a seat.
- **Cost-when-tried:** findings were computed again on every read of `GET /advisor` and stored nowhere, so the change an administrator previewed was built from the finding as the page had loaded it, and nothing asked whether it still held before the change was sent: only the entity's tag guarded the write. Nothing could be dismissed or snoozed, nothing ran unless somebody opened the page, and pool waits, objective alarms and model limits reached people only as alarm notices.
- **Tag-status:** unreleased

## The decision

**The Advisor is one background duty over deterministic detectors, configured by one model, and what it finds is kept.** `advisor.model` (a `providers.llm` key, held to the same harness check as Ask Crewlet's, [ADR-0163](0163-ask-crewlet-runs-on-one-model-and-reads-the-engine-through-its-own-api.md)) runs the model pass daily at 06:00 on the company clock and on Run now, under a 200,000-token ceiling charged to the company and no seat; absent, the detectors still run and only the model's work is off. The analyst is no longer a component of its own, and [ADR-0157](0157-the-advisors-analyst-lists-only-what-the-engines-records-show.md) is superseded: its rule that a model's finding is listed only when every record and number it cites is one its own reads answered, and its fix is one the engine builds, is this record's rule for the model pass.

- **Detectors call no model**, as [ADR-0115](0115-a-setup-finding-carries-its-fix-and-only-a-person-sends-it.md) requires, and each is one implementation called by the duty, by a re-check, by the go-live check and by `crewlet doctor`. Each class has a cadence (every 15 minutes, hourly, daily, or on a change), and the events that make it due now (a configuration applied, a seat paused or resumed, an alarm moving, a flow notice) only move it forward; the duty adds no publisher.
- **A finding is kept, with what it rests on.** Its key is its class and subject, one open finding per key, a returning condition a new episode that says it was seen before. It carries the window, the facts and records it read, the revisions its fix was built against, a canonical verdict and the fix, each with a digest. It lives in a state-log domain of its own, applied into the replicated estate, because the whole company reads it from any node with its history.
- **It is re-derived on every run of its class.** Observed again, its numbers are refreshed; observed with another verdict or fix, it is revised and says what moved; not observed, it is resolved by itself. A finding whose class has not run for three of its intervals is shown as not re-checked, and expires after seven days.
- **Apply re-checks first.** Pressing Apply runs the finding's detector for its subject now and rebuilds its fix, and answers `holds`, `changed` (what moved and the new change, which the person confirms or not), `resolved` or `not applicable` (closed, nothing applied). The person then sends the change from their own session under the `If-Match` of the revision the re-check read, so anything that moved its target in between is refused by the route; a re-check older than ten minutes is run again.
- **An applied fix is checked for its effect.** The condition clearing within the class's horizon resolves the finding as fixed by it; still holding at the horizon, the finding reopens as ineffective and that fix is never offered again for that key.
- **Staffing is advised from the flow record.** Per group of identical seats, over a rolling window, the hours work wanted a seat and did not get it, the hours it was held and why, and each seat's busy and idle hours (read off the flow state `internal/worklet` records for every item in progress, never a second record) decide whether to add a seat like the busiest one, add a fallback model on another account, or remove one seat, each behind a minimum of evidence.

The engine still never sends a fix (ADR-0115), and atlas findings name their entries only to a holder of the atlas app, now at read rather than when computed ([ADR-0141](0141-the-advisor-lists-what-the-atlas-shows-needs-a-person-and-names-entries-only-to-a-holder.md)). A platform notice is still read off its condition and never kept ([ADR-0116](0116-an-administrator-notice-is-read-off-its-condition.md)): a notice is not a finding.

## Why the obvious alternatives are wrong

**Computing findings on read, as before.** A finding that exists only while a page is open has no history, cannot be dismissed, explained or reported on, and is never there when the condition passes at night. Keeping it costs a domain; the re-derivation on every run keeps what is kept honest.

**Re-checking only when somebody presses Apply.** A list re-checked only on Apply fills with findings that stopped being true, and every Apply becomes a surprise. Re-deriving on every run keeps the list true, and the re-check on Apply closes the last minutes.

**Comparing the numbers on re-check.** The numbers drift every minute; a re-check that refused on any drift would refuse every time. What must not move unseen is what the decision rests on: the verdict, the change and the revision it is built against.

**The engine sending the fix after its re-check.** The Advisor would then hold a write path. The person's own session sends it, under the route's own authority row and step-up, and `If-Match` binds that send to the re-check as tightly as a server-side send would.

## What this does not decide

Each class's thresholds and words, which `internal/advisor` keeps beside its detector. The domain's records and tables, which are `internal/advisordomain`'s. The operating insights shared under their own consent ([ADR-0119](0119-operating-insights-are-typed-weekly-records-under-a-consent-of-their-own.md)), which keep reading the same detectors.
