# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`, `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`, `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`, `internal/executordomain.TestALateCaptureNeverRollsASessionBack`, `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`, `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`, `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`, `internal/executordomain.TestARemovedSeatStaysRemoved`, `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`, `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`, `internal/engine.TestTheExecutorLogFitsBesideTheOtherLogsAtTheOperatorsSmallestVolumes`, `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`, `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`, `internal/estate.TestEveryExecutorWriteIsTheEngines`, `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`, `internal/sandbox.TestEachLaunchOfATurnIsNumberedPastTheOneBefore`, `internal/sandbox.TestACapturesPlaceOnlyGrowsOnTheRow`, `internal/sandbox.TestACollectionTakesTheSessionBeforeThePauseAndKeepsItAfter`, `internal/sandbox.TestEveryCollectionTakesWhatItsJobLeft`, `internal/sandbox.TestAJobTakenOnceItHadEndedIsNotTakenAgain`, `internal/sandbox.TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd`, `internal/sandbox.TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd`, `internal/sandbox.TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim`, `internal/sandbox.TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt`, `internal/sandbox.TestALostRunsBoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAnEndWaitsForItsTurnsCaptureToLand`, `internal/sandbox.TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded`, `internal/sandbox.TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused`, `internal/sandbox.TestTheScrubReadsTheLoginAgainWhereItIsRefreshed`, `internal/sandbox.TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse`, `internal/sandbox.TestWhatABoxWasHandedIsKeptBesideItsRowForItsJobAlone`, `internal/executordomain.TestAnEndWhoseNewestCaptureWasNotKeptReleasesTheOlderOne`, `internal/executordomain.TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted`, `internal/sandbox.TestTheSwitchOffKeepsNothingAndStillEndsTheTurn`, `internal/sandbox/executor.TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox`, `internal/sandbox/executor.TestAPackOnDemandKeepsItsDigestInTheBox`, `internal/sandbox/executor.TestATranscriptLineStaysJSONAndLosesEverySecret`, `internal/sandbox/executor.TestTheDigestIsPOSIXCksums`, `internal/sandbox/executor.TestTheScrubHoldsNothingPastItsBounds`, `internal/sandbox/executor.TestATarLeavesTheBoxWithOnlyItsSessionsFiles`, `internal/sandbox/executor.TestAnExportIsOneDocumentKeptScrubbed`, `internal/sandbox/executor.TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed`, `internal/sandbox/executor.TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession`, `internal/sandbox/executor.TestATakeIsBoundedByTheArchiveItKeeps`, `internal/engine.TestAResumedTurnsEndIsRecordedWithItsOwnDecision`, `internal/engine.TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog`, `internal/engine.TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestALostTurnEndsAsLostOnTheExecutorLog`, `internal/engine.TestTheEndCarriesTheTurnsOwnDecisionAndBreach`, `internal/engine.TestTheEndSaysWhatTheEngineAnsweredAColleague`, `internal/engine.TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid`, `internal/engine.TestAStopsAccountReadsEveryRoundsCalls`, `internal/engine.TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds`, `internal/engine.TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped`, `internal/engine.TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn`, `internal/engine.TestAResumedTurnThatPanicsOnceBegunEndsFailed`, `internal/engine.TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose`, `internal/engine.TestAnMCPServersURLSecretIsHandedToTheScrub`, `internal/engine.TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives`, `internal/sandbox.TestARetiredSeatsRunIsFinishedWithItsTurnsEnd`, `internal/sandbox.TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded`, `internal/sandbox.TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt`, `internal/sandbox.TestTheSwitchOffAsksNoHostForALostRunsSession`, `internal/sandbox.TestASpoolACrashLeftIsSweptByTheTakesAfterItAges`, `internal/sandbox.TestASpoolNeverOutlivesItsCapture`, `internal/sandbox.TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded`, `internal/sandbox.TestTwoAttemptsAtOneEndingOnANodeFinishItOnce`, `internal/sandbox.TestALoginTheBoxGrewPastItsBoundRefusesTheCapture`, `internal/sandbox.TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt`, `internal/sandbox.TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt`, `internal/sandbox.TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail`, `internal/sandbox.TestAnEndingDecidedBeforeTheNextHoldersFenceIsFinishedByIt`, `internal/sandbox.TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer`, `internal/sandbox.TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled`, `internal/sandbox/executor.TestAHandedKeysArmourStaysForTheShapePass`, `internal/sandbox/executor.TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack`, `internal/redact.TestALineIsCutAtTheArmourAKeyIsReadBy`, `internal/procgroup.TestASignalReachesOnlyALivingMemberOfTheGroup`, `internal/sandbox.TestADirectBoxEndsOnlyAProcessOfItsJob`, `internal/sandbox.TestADirectBoxKillsEveryProcessOfItsJobAndKeepsTheBox`, `internal/sandbox.TestAContainerBoxEndsAndKillsItsJobInsideTheContainer`, `internal/sandbox.TestAContainerJobThatRecordedNoGroupIsKilledByTheGroupItsExecLeads`, `internal/sandbox.TestAContainerJobWhoseGroupNoExecLeadsIsSignalledNothing`, `internal/sandbox.TestAContainerJobWithNoGroupRecordedIsReadOverOnlyOnceItIs`, `internal/sandbox.TestAnE2BBoxEndsAndKillsItsJobAsItsUser`, `internal/sandbox/executor.TestAnEndedClaudeCodeRunPacksAsItsOwnEndDoes`, `internal/sandbox/executor.TestAnEndedOpenCodeRunExportsItsSessionBeforeItsServerStops`, `internal/sandbox/executor.TestAnEndIsAskedOfThePIDTheScriptRecorded`, `internal/sandbox/executor.TestACollectionSaysItsJobWasEnded`, `internal/sandbox.TestAStopEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestALossEndsItsJobBeforeItTakesItsSession`, `internal/sandbox.TestAJobThatOutlivesItsEndIsKilledBeforeItsTake`, `internal/sandbox.TestAJobAskedNothingIsKilledNotWaitedFor`, `internal/sandbox.TestAStopOfABoxOnAnotherHostEndsItsJobThere`, `internal/sandbox.TestAStopWithTheSwitchOffEndsItsJobOffTheFrame`, `internal/sandbox.TestAStopOrLossReadIsGivenItsJobsEndOnTopOfTheTake`, `internal/engine.TestABoxReadWaitsAsLongAsItsKindTakes`, `internal/executordomain.TestABeginHoldsTheBaseItsReadNamed`, `internal/executordomain.TestABeginAndAnEndOfItsBaseAreOrderedByTheLog`, `internal/executordomain.TestNoReleaseTakesAHeldBase`, `internal/executordomain.TestTheHoldLiftsAtTheHoldersFirstCapture`, `internal/executordomain.TestTheHoldLiftsWhenTheHolderEnds`, `internal/executordomain.TestATurnThatNeverCapturedAbsorbsNothing`, `internal/executordomain.TestTheRetentionListsNoHeldRow`, `internal/executordomain.TestAPageNamesEachConversationsHead`, `internal/executordomain.TestABeginTheRowsCannotTakeIsRefusedAtTheDecide`, `internal/executordomain.TestABeginSaysWhetherTheLogHoldsItsBase`, `internal/executordomain.TestACapturesContextAndCutOffReachTheRow`, `internal/executordomain.TestTheExecutorStatementsArePlanned`, `internal/sandbox.TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff`, `internal/executordomain.TestABeginHoldsOnlyTheSessionItsReadSaw`, `internal/executordomain.TestALateBeginRecordsTheReadAndHoldsNothing`, `internal/executordomain.TestABeginOfATurnThatEndedWritesNothing`, `internal/executordomain.TestABeginNoReadHandsOutIsRefusedByItsWriter`, `internal/executordomain.TestABeginThatHoldsNothingSaysWhy`, `internal/executordomain.TestAHolderWhoseEndWasLostIsEndedLostAndItsHoldLifts`, `internal/sandbox.TestATurnsFirstLaunchContinuesItsConversationsHead`, `internal/sandbox.TestTheFreezeIsWrittenAtTheFirstLaunchOnly`, `internal/sandbox.TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase`, `internal/sandbox.TestWhetherABaseIsContinuedIsOneDecision`, `internal/sandbox.TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone`, `internal/sandbox.TestABaseIsContinuedOnlyInABoxOfItsOwn`, `internal/engine.TestAFrozenReadFitsItsBound`, `internal/engine.TestATurnReadsItsConversationOnce`, `internal/engine.TestTheFrozenBaseOutlivesASiblingsEnd`, `internal/engine.TestAnEndingLeftOutOfTheBriefIsNotAbsorbed`, `internal/engine.TestATurnThatNeverLaunchedTellsTheNextTurnHowItEnded`, `internal/engine.TestAnAbandonedFirstLaunchEndsItsTurnOnTheLog`, `internal/engine.TestTheSwitchOffCarriesNothingBetweenTurns`, `internal/agent/prompts.TestTheEndingsKeepTheNewestWholeAndCountTheRest`, `internal/sandbox.TestWhatAReaderIsToldOfAnEndedTurnIsTheNextTurnsOwnDecision`, `internal/sandbox.TestARelaunchOfATurnThatKeptNothingIsSaidToContinueItsBase`, `internal/api/queries.TestARowSaysWhetherTheNextTurnContinuesIt`, `internal/api/queries.TestARowReadAgainIsDecidedOnTheHeadItsOwnReadNamed`, `internal/engine.TestAnUnreadableConversationIsLessContextNotAFailedTurn`, `internal/schedule.TestEachFireOfAScheduleIsOneConversationOfItsRunner`, `internal/schedule.TestTheLongestScheduleNameFitsTheLogsKey`, `internal/org.TestAScheduleNameIsBoundedAsItsKeySpellsIt`, `internal/notify.TestAScheduleConversationIsKnownByItsPrefixAlone`, `internal/engine.TestAScheduledFireContinuesThePreviousFiresSession`, `internal/engine.TestFiresOfOneScheduleThatWaitedTogetherRunOnce`, `internal/engine.TestAFireIsNeverTakenAsTheAnswerToTheLastFire`, `internal/api/queries.TestARunNoChatCanAnswerSaysSo`, `internal/sandbox.TestAScheduleRowIsToldItsNextFire`, `internal/api/queries.TestAScheduleRowSaysWhetherItsNextFireContinuesIt`, `internal/executordomain.TestAConversationsHeadIsTheTurnThatReadItLast`, `internal/executordomain.TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued`, `internal/executordomain.TestAHeadReleasedBeforeAStaleSiblingHandsTheNextTurnTheSibling`
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
headers, the run's telemetry token and its code-host credential, the seeded
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
(`executorkeep.Unkept`): applied, it releases the object an older capture's
row named, so the log never names an older session as the turn's last — the
end being recorded, on every path but a frame's that found no row, by the run's
ending that read the row's stamps. And where the run's ending decides the end
from the row and the run's bridged log alone — a loss, a stop — it says whether
the turn's reply is known to have reached its party read off every round's
successful calls, the earlier rounds' folded into the log by the opening that
starts a new round (`DeliveryUnknown` where the log elided calls or could not
be read); a turn its resumed frame ends says what that frame's loop
established.

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

**A hundred and twenty linearizable reads per seat-day**: the dispatch's head
read, the base's row at a first launch that continues it, and two reads of the
turn's row at each relaunch, at the reference company's thirty turns a seat a
day and a mean of one relaunch a turn — the barrier term the log's ceiling
carries and `census_drift` holds it to.

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
continues the newest session the seat still keeps and is told every newer
ending, the released head's among them. Releasing in the head's order was
rejected: which sessions a capture releases is decided by the applier on every
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
schedule's head, which the log's hundred and twenty linearizable reads per
seat-day assumed of every one of a seat's thirty turns a day, fires included.
