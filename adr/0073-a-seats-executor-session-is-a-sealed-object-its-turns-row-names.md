# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`, `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`, `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`, `internal/executordomain.TestALateCaptureNeverRollsASessionBack`, `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`, `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`, `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`, `internal/executordomain.TestARemovedSeatStaysRemoved`, `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`, `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`, `internal/engine.TestTheExecutorLogFitsBesideTheNineAtTheOperatorsSmallestVolumes`, `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`, `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`, `internal/estate.TestEveryExecutorWriteIsTheEngines`, `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`, `internal/sandbox.TestEachLaunchOfATurnIsNumberedPastTheOneBefore`, `internal/sandbox.TestACapturesPlaceOnlyGrowsOnTheRow`, `internal/sandbox.TestACollectionTakesTheSessionBeforeThePauseAndKeepsItAfter`, `internal/sandbox.TestEveryCollectionTakesWhatItsJobLeft`, `internal/sandbox.TestAJobTakenOnceItHadEndedIsNotTakenAgain`, `internal/sandbox.TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd`, `internal/sandbox.TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd`, `internal/sandbox.TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim`, `internal/sandbox.TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt`, `internal/sandbox.TestALostRunsBoxOnAnotherHostIsAskedOfThatHost`, `internal/sandbox.TestAnEndWaitsForItsTurnsCaptureToLand`, `internal/sandbox.TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded`, `internal/sandbox.TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused`, `internal/sandbox.TestTheScrubReadsTheLoginAgainWhereItIsRefreshed`, `internal/sandbox.TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse`, `internal/executordomain.TestAnEndWhoseNewestCaptureWasNotKeptReleasesTheOlderOne`, `internal/executordomain.TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted`, `internal/sandbox.TestTheSwitchOffKeepsNothingAndStillEndsTheTurn`, `internal/sandbox/executor.TestAKeptSessionCarriesNoSecretTheEnginePutInItsBox`, `internal/sandbox/executor.TestAPackOnDemandKeepsItsDigestInTheBox`, `internal/sandbox/executor.TestATranscriptLineStaysJSONAndLosesEverySecret`, `internal/sandbox/executor.TestTheDigestIsPOSIXCksums`, `internal/sandbox/executor.TestTheScrubHoldsNothingPastItsBounds`, `internal/sandbox/executor.TestATarLeavesTheBoxWithOnlyItsSessionsFiles`, `internal/sandbox/executor.TestAnExportIsOneDocumentKeptScrubbed`, `internal/sandbox/executor.TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed`, `internal/sandbox/executor.TestAPackThatFailsIsAnErrorAndNoTranscriptIsNoSession`, `internal/sandbox/executor.TestATakeIsBoundedByTheArchiveItKeeps`, `internal/engine.TestAResumedTurnsEndIsRecordedWithItsOwnDecision`, `internal/engine.TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog`, `internal/engine.TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestALostTurnEndsAsLostOnTheExecutorLog`, `internal/engine.TestTheEndCarriesTheTurnsOwnDecisionAndBreach`, `internal/engine.TestTheEndSaysWhatTheEngineAnsweredAColleague`, `internal/engine.TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid`, `internal/engine.TestAStopsAccountReadsEveryRoundsCalls`, `internal/engine.TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds`, `internal/engine.TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped`, `internal/engine.TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce`, `internal/engine.TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn`, `internal/engine.TestAResumedTurnThatPanicsOnceBegunEndsFailed`, `internal/engine.TestALaunchsSecretsAreItsCredentialPositionsAndOnlyThose`, `internal/engine.TestAnMCPServersURLSecretIsHandedToTheScrub`, `internal/sandbox.TestARetiredSeatsRunIsFinishedWithItsTurnsEnd`, `internal/sandbox.TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded`, `internal/sandbox.TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt`, `internal/sandbox.TestTheSwitchOffAsksNoHostForALostRunsSession`, `internal/sandbox.TestASpoolACrashLeftIsSweptByTheTakesAfterItAges`, `internal/sandbox.TestASpoolNeverOutlivesItsCapture`, `internal/sandbox.TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded`, `internal/sandbox.TestTwoAttemptsAtOneEndingOnANodeFinishItOnce`, `internal/sandbox.TestALoginTheBoxGrewPastItsBoundRefusesTheCapture`, `internal/sandbox.TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt`, `internal/sandbox.TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt`, `internal/sandbox.TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail`, `internal/sandbox.TestAnEndingDecidedBeforeTheNextHoldersFenceIsFinishedByIt`, `internal/sandbox.TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer`, `internal/sandbox.TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled`, `internal/sandbox/executor.TestAHandedKeysArmourStaysForTheShapePass`, `internal/sandbox/executor.TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack`, `internal/redact.TestALineIsCutAtTheArmourAKeyIsReadBy`
- **Measured:** a harness's session of a day's work compresses to one or two mebibytes and is bounded here at 32 MiB; at the reference company's 3 000 turns a day the log that names the sessions grows about 29 MB a day, while the sessions themselves would be gigabytes; crewlet-operator's smallest data volumes leave 704 MiB (11Gi, objects in S3) and 320 MiB (14Gi, beside the nats bucket) above the nine logs before this one, and this log, scaled to its 64 MiB floor there, still fits both — the minimum classes do not move, but the least a fresh node boots in rises by 64 MiB of logs (10916375210 bytes of broker on nats, up 85.3 MiB with the bucket's quarter beside it, and 64 MiB on S3), which is what the operator's own floors follow. At that floor the log holds about two days at the reference company's rate, under the seven-day `min_age` a healthy trim keeps, so a company above about thirty seats on those volumes sets `stream.executor_log_max_bytes` or a larger volume, and `log_ceiling_short` says so
- **Tag-status:** unreleased

## The decision

A seat's executor session — the harness's own transcript, captured so a later
launch of the same turn, in any box on any node, continues it, and so a later
turn on the same conversation can, which none does yet — is stored as **one sealed object per capture in the fleet's object store**
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
it continued, the endings it was told and the siblings it was offered, carried
on its captures — so what a conversation still holds is what no ended turn was
shown, and the next read shows all of it; a seat's byte budget releases the
oldest ended sessions; the company's conversation retention deletes a row past
its last instant, ended or not but never one whose run is live, and first ends
as `lost` a turn whose end never came, a day past its last capture with no run
live, so a lost end holds the seat's budget for a day rather than the whole
retention; and the mailbox retirement deletes a removed seat's rows with one
record, always published, whose position every capture and end is held to, so
one decided before the removal never recreates a row for a seat that is gone.

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

A person's removal does not reach these rows yet, as it did not reach the
conversation ledger they replace; later records amend this one with the
erasure, the capture and the restore.

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

Its writes are a class of their own: ENGINE-ORIGINATED. A capture, an end, a
retention delete and a removed seat's are the engine's record of what its own
runs did, which no tool, no person and no operator surface asks for. Declaring
the log to the person's write surfaces, as the domains a surface writes are
declared, would have meant a refusal class and an object argument for writes
nobody can make of them, met only by borrowing an argument some operator tool
takes for something else. So the router declares who originates each write,
the surfaces are held to the domains a surface writes, and two gates hold the
engine's class: every write on this log is declared the engine's and nothing
else is, and no package that serves a person or a model names the writer the
router hands out, the domain's own writer or an engine hand-out of either. The
second is a walk of the source and sees names, not values, so a writer smuggled
inside another type is review's to catch. The reads are ordinary reads, which a
surface may hold. `internal/estate` states the class.

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
headers, the run's telemetry token, the seeded login — and nothing else: the
seat's identity facts, a setting, a setup step's own script are no secrets,
and taken out by value they were lost from every kept transcript. Only the
engine knows where a value came from, so it says which are secrets
(`LaunchRequest.Secrets`). The set is sealed on the run's row under the fleet
keyring and bound to the turn; a job that continues in the turn's box carries
the previous job's set forward, since the box and the session it resumes still
hold those values; the login is read again in the box and on the host at the
capture. A row that keeps none, a continued box whose row says nothing of what
it was handed, or a seal the node cannot open, refuses the capture, and so does
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
(`executordomain.Unkept`): applied, it releases the object an older capture's
row named, so the log never names an older session as the turn's last — the
end being recorded, on every path but a frame's that found no row, by the run's
ending that read the row's stamps. And where the run's ending decides the end
from the row alone — a loss, a stop — it says whether the turn's reply is known
to have reached its party read off every round's successful calls
(`DeliveryUnknown` where the run's record elided calls); a turn its resumed
frame ends says what that frame's loop established.
