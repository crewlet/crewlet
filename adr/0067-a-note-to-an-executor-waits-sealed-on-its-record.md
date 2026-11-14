# ADR-0067: A note to an executor waits sealed on its record

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestAnUnsealedNoteOnARowReachesNoModel`, `internal/engine.TestANoteSealedForAnotherTurnIsNotDelivered`, `internal/engine.TestANoteUnderAnUnheldKeyWaitsAndNamesTheHeldKeys`, `internal/engine.TestAnAnchorThatDoesNotOpenDecidesNoAuthority`, `internal/engine.TestARunIsSteerableAfterItsLaunchingNodeRestarts`, `internal/engine.TestATakenNoteIsRecordedDeliveredOnce`, `internal/engine.TestTheReviewerOpensWithANoteTheRunTook`, `internal/engine.TestAParkedRunHoldsItsNotesForWhatResumesIt`, `internal/engine.TestAnOpenCodeRunsNoteIsReadAtItsNextStepByItsJob`, `internal/engine.TestARunsRecordIsASteerSource`, `internal/engine.TestAnEndingExpiresWaitingNotesOnceUnderStableIDs`, `internal/engine.TestANoteLeftWaitingWhenARunEndsIsRecordedExpiredByTheEnginesCoordinator`, `internal/engine.TestAnOfferRacingTheLaunchIsNeverAnsweredClosed`, `internal/engine.TestEveryNoteAcceptedAcrossTheHandOverIsOnTheRecord`, `internal/engine.TestTheBoxsNotesAreOnTheRecordAheadOfOneFiledAsItIsWritten`, `internal/engine.TestALaunchThatFailsAfterItsWriteLeavesItsNotesToTheRecordsEnding`, `internal/engine.TestAResumedTurnCarriesItsNotesInTheOrderRead`, `internal/sandbox.TestAFailedLaunchsEndingReportsTheNotesItsWriteHeld`, `internal/engine.TestEveryInstructionChannelSignsInDomainsOfItsOwn`, `internal/sandbox.TestPendingStoreContract`, `internal/agent/builtin.TestARunRecordDecidesWithoutAScatter`, `internal/agent/builtin.TestAuthorityIsDecidedOnTheAnchoredSeat`, `internal/api/mcpbridge.TestSteerTakeIsServedUnlistedAndNeverRecorded`, `internal/sandbox/executor.TestARealBoxsHooksReachTheBridgeTool`, `internal/sandbox/executor.TestThePluginAdmitsNotesAtTheRunsNextStep`, `internal/sandbox/executor.TestThePluginReadsTheBridgesOwnNotesRoute`, `internal/sandbox/executor.TestARunCancelsTheNotesItsSessionNeverDelivered`, `internal/sandbox.TestACapturesNoteCursorStopsBeforeAReadItsHarnessNeverHad`, `internal/sandbox.TestEveryStampOfACaptureStatesTheNotesItsSessionHolds`, `internal/engine.TestAHooksTakeWhoseAnswerWasLostIsSettledUnanswered`, `internal/engine.TestAReviewsTakeWhoseAnswerWasLostHandsItsNotesOver`, `internal/agent/runner.TestAKeptSessionIsToldAgainEveryNoteFromAReadItsHarnessNeverHad`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldAgainEveryNoteTheTurnRead`, `internal/agent/runner.TestTheExecuteRecordListsItsRoundsReads`, `internal/engine.TestASettleAsksUntilItsStoreAnswersAndNoLongerThanItsNode`, `internal/engine.TestAPolledNoteIsServedOnItsRecordBeforeItIsAnswered`, `internal/agent/runner.TestASessionIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow`, `internal/agent/runner.TestARelaunchTellsANoteALostTakeRecordedWhereTheRecordReadIt`, `internal/engine.TestAnEndingReportsEachNoteAsItsRecordKnowsIt`, `internal/sandbox.TestEachNoteEndsAsItsRecordKnowsIt`, `internal/agent/steer.TestARecordIsDeliveredExactlyWhereItWasRead`, `internal/events/types.TestTheDashboardNamesEverySteerOutcome`, `internal/engine.TestAMarkedNoteIsDeliveredWhereALaterLaunchToldItAgain`, `internal/engine.TestAMarkedNoteIsToldAgainByTheNextLaunchThroughTheRealTurn`, `internal/agent/runner.TestALaunchNamesEveryMarkedNoteItsBriefsTellAgain`, `internal/agent/runner.TestAContinuationOfAJobCutOffAfterTellingAMarkedNoteAgainIsStillToldIt`, `internal/engine.TestAConfirmationWhoseAnswerWasLostReportsItsNoteOnce`, `internal/engine.TestAReviewsTakeLostAtItsSegmentsEndIsMarkedBeforeItReturns`, `internal/engine.TestEveryReaderOfWaitingNotesNamesItsJob`, `internal/engine.TestATurnCarriesNoJobsItem`, `internal/engine.TestANoteWhoseServeACancelLostIsToldAsOneItMayShow`, `internal/engine.TestAnEndingReportsAHookServedNoteAtItsServe`, `internal/engine.TestAnItemForAJobNeverLeavesIt`, `internal/engine.TestAConfirmationAnInboxServingNoJobCannotTakeSaysSo`, `internal/engine.TestAConfirmationItsJobsRecordNoLongerTakesSaysSo`, `internal/engine.TestAConfirmationOfANoteUnderAnUnheldKeyWaitsForANodeThatHoldsIt`, `internal/engine.TestAConfirmationIsTurnedAwayOnlyForANoteItNamesThatWaitsForItsKey`, `internal/engine.TestEveryNoteThatWaitsForItsKeyIsSaidOnceWhereverItsConfirmationIsTurnedAway`, `internal/engine.TestAConfirmationIsJudgedByTheRecordReadAfterItsTake`, `internal/engine.TestAConfirmationTurnedAwayStillReportsWhatItsJobReadBefore`, `internal/engine.TestAnOpeningIsFiledOnlyOnceItsInboxIsBound`, `internal/api/mcpbridge.TestAReadItsOpeningsEndOvertookIsTurnedAwayToAskAgain`, `internal/api/mcpbridge.TestAFencedRunTakesNothingMoreFromItsNotes`
- **Measured:** on Claude Code 2.1.263 and 2.1.310, against stub APIs: a hook's tool call answered with an error makes the CLI drop and re-initialise the run's whole MCP server, telling the model its tools are still connecting — so a hook's call is never answered with an error; a hook's timeout is in seconds, and at it the CLI cancels the call and discards a late answer — so the engine answers within half of it; additional context longer than 10,000 UTF-16 code units is moved to a file the model is shown a 2 KB preview of — so one answer carries at most 9,000. On OpenCode 2.0.32 and 2.1.4, a second admission under an id the session already holds is answered with the first — so a note's id is derived from the job and the note; a note admitted during a tool call, or during the final model call, is read in the same execution, and on 2.1.4 one admitted after the execution settles, with `resume: false`, starts nothing — so a note sent after a run's last step waits on the record for the review — but stays pending in the session, across the server's restart, and is handed to the next run in that session ahead of its prompt unless it is cancelled (`session.inbox.cancel`) — so a run's script cancels every admission OpenCode never delivered once the run has ended. A sealed note is at most about 11 KiB, and a turn takes at most ten.
- **Cost-when-tried:** before this, a turn whose executor was its coding CLI's own run answered every note `steer_unsupported`, and a turn parked on its run answered `closed`: a person could not correct an executor at all, and a correction sent while a run worked never reached the reviewer that judged the work.
- **Tag-status:** unreleased

## The decision

**From the launch of a turn's executor until the turn ends, the turn's notes
wait on the run's record in the coordination store** — the whole company, now:
the node a person's request reaches, the node serving the run's tool bridge,
the node that resumes the turn and the node that finishes the run's ending can
be four processes across a restart and a seat moving, and each already reads
the record. Any node files a note there with one compare-and-swap, and no node
keeps a note only in its memory.

**Every note on a record is sealed under the fleet keyring** by the node that
files it, in a key domain of its own (`seat.steer.note`), bound to its turn and
its id; and the record carries a **sealed anchor** naming the seat the run
serves (`seat.steer.seat`), written by the launch, on which a person's
authority to steer the turn is decided. Every reader opens before use: a note
whose seal does not open is never handed to a model, carried or attributed,
and one under a key a node does not hold waits for a node that holds it — the
[ADR-0031](0031-every-state-log-record-is-signed.md) amendment states the
dispositions.

**Every read is recorded on the record by the write that hands the note
over** — the launch's own for a brief, a take pinned to the run's job for its
CLI or for the review — so the record's read notes are exactly what the turn
has read, and a segment resuming the turn carries them from the record
whichever node it is on, in the order the record numbered the writes that read
them, never by the clock of the node that made each. **The first segment's box
is handed over in the write that creates the record**: what it held rides that
write, waiting, so nothing filed on the record once it exists lands ahead of a
note the box took first. **Only an ending expires a note that reached a
record**, under an id derived from the ending, so a retried ending records it
once; a park never does. That includes a note the brief of a job that never
started recorded read — a launch that fails after its write ends the record and
leaves its brief's notes to that ending — so nothing else that read a note can
report it a second time.

The detail — the seals, the inbox a run's bridge serves, the hand-over from the
first segment's box, the records published — is `internal/engine`'s
(runsteer.go and steer.go).

## Why the obvious alternatives are wrong

**A note box per run in the launching node's memory** is lost when the node
restarts or the seat moves, and makes a second routing problem — which node
holds this run's box — beside the one the bridge already solves
([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)).

**Pushing notes into the box** needs an atomic write into every kind of cell,
which none offers alike, and leaves the person's words in a box that runs the
code it was handed.

**Feeding notes on the CLI's standard input** reaches nothing: both CLIs read
their prompt once.

**An unsealed field on the record** is the instruction channel ADR-0031 exists
to close: the coordination store decides between writers and authenticates
none of them, and a note is words a model reads as a person's.

## What this does not decide

How each coding CLI is handed its notes between its steps — that is
`internal/sandbox/executor`'s, CLI by CLI — nor what a note to a turn in the
engine's own loop does, which is the turn engine's.

It leaves two things unsealed, and states them. The `read` marker on a note is
guarded only by the store's compare-and-set: a party that reaches the
coordination store could mark a genuine note read on its own turn, which keeps
it from the run's CLI — the review still reads every note the record holds, so
the correction still binds the work that is judged — or clear the marker, which
hands the run a genuine note again. And a run's box can take its own notes
without putting them before its model, by calling the bridge's take itself; a
note taken so is recorded read and carried into the review.

## Amendment: a take whose answer was lost is settled, never left saying it was read

A take's write can land and its answer not. A hook's take of its run's notes is
answered within five seconds or not at all (internal/api/mcpbridge's
`steerTakeBudget`), and a compare-and-swap the store answered after them — or
one whose answer a broken connection lost — may still have landed: the record
then said the job read a note its harness was never handed. Where that harness
ended its own run, its capture's note cursor counted the note held, and no
continuation of its session ever told it; the job's execute record listed it as
read. The read was the handover, and a read whose handover failed was one the
record could not tell from one that succeeded.

**Every take names itself** (`sandbox.NoteRead.Take`, fresh for every call),
and **one whose call answered an error is settled by that name**
(`sandbox.PendingStore.SettleTake`): the store answers the notes that take
recorded read, and, where its reader never had them, marks each read
**unanswered** (`sandbox.NoteRead.Unanswered`), keeping where it was read. The
note stays read by the record's word — no later take hands it over, and every
reader that carries the turn's read notes carries it, the review among them —
but no capture of the job's session counts it held: a capture's note cursor
stops before the first read marked unanswered, whichever job's take it was
(`notesHeld`), and a launch that continues a session whose notes stop there,
or the job's own copy in its box where its harness ended its own run, is told
every note from that read on, under the header of notes it may already show
(`runner.notesHeldBy`, `prompts.NotesReadAgain`) — reading the record's marks
as the relaunch's brief reads the record (`steer.Briefing.Unanswered`), so a mark
that landed after the capture was stamped, or after the segment that relaunches
resumed, is counted all the same. A fresh brief tells every note the turn has
read, as before. The execute phase's record lists no read marked unanswered.

**Where the settle is made.** A hook's take settles OFF THE HOOK'S PATH, its
notes marked: the hook has already been answered nothing, and a settle on its
path would hold the harness's next request for an answer that can only be
nothing. It is asked until the store answers — never given up by a clock, since
a settle given up leaves the record saying the run read notes it was never
handed while the store that did not answer in time answers once it is back —
one loop a run however many takes it loses, said at ERROR once thirty seconds,
which outlast a leader election, have passed unanswered
(`steer_take_unsettled`), and ended only by its node stopping, once it has
asked every take once more — a store back while it slept between rounds, and a
take a segment the stop ended handed off, settle then — in a window of its own
from the teardown's start, five seconds, beside the give-backs and never on
the stop's allowance (`steerSettleStopBudget`): a settle that runs out leaves a
take unsettled, not a lease lapsing, and the run records' bucket can lose its
quorum alone, so settles on the allowance spent what the give-backs made later
in the stop are owed. A review's take
settles ON ITS OWN PATH, in answer mode, and hands over what its write
recorded, its round still waiting on it, never marking it. One whose settle the
store does not answer either is kept by the segment that reviews the run and
settled again, the same way, at its next round's take and at the brief of a
relaunch it makes, which hand over what it recorded — the brief naming the take
beside each such note (`sandbox.RunNote.LostTake`), so the launch's write
records the note read by the job it opens in the take's place: the brief is the
first to tell it, and the job's ending reports it where the job never started,
and its execute record lists it where it did. Who read it is the job's; where
it was read stays the take's own place among the reads, and the brief tells it
there, among the notes the turn has read, from the places the segment's source
says its reads were recorded at (`steer.Briefing.Orders`): a note a later round
read, a correction of it, is never told ahead of it, by that brief or any
after it. Meanwhile, for thirty
seconds, it stands for a note waiting, so a round asking for tools ends and the
next one opens with it (`runner.SteerSource.Waiting`). Only a take still
unsettled when the segment ends is marked, as a hook's is: in the segment's own
path, one attempt before the segment returns (`rowSource.Close`), since the
run's ending reads the record next where the segment ended the turn, and reads
a lost take's note, unmarked, as one its step reported; and off the path where
the store answers that attempt no more. An OpenCode run's confirmation of a
note OpenCode delivered is never marked: its read is true whether or not its
answer arrives. Only its report rode on that answer, and a confirmation whose
answer was lost is settled for it, on its own path (the third amendment below).

**And it leaves the take nothing more to record.** A take whose caller gave up
on the answer can still have its write on its way to the store, taken only
after the settle read the record. So a settle that finds no read of the take
writes the record all the same, in either mode: the take's write was
conditioned on a version before that one, and can never land after it. A settle
that finds the take's reads, marked as asked, writes nothing — the take's
write landed, and nothing of it is left in flight. And one whose attempt lost
its race to the record's removal answers nothing, as a record gone before it
does.

**Why not put the note back to waiting.** It would reach the running harness at
its next step, but a read once recorded is carried by whoever read the record
before the settle landed — the segment that resumes the turn, a capture's
stamp — and a note handed back to waiting after that is read twice, the second
time as a note of its own. Marked, it is never handed to a model twice without
the header that says it may already have been, and the settle needs no pin to
the job's status to be right.

**What it does not reach.** A take whose answer the store returned in time but
the network held past the hook's own ten seconds is recorded read and was never
seen by the harness, and nothing on the engine's side can tell it from one the
harness read: the five seconds are what keep it to a stall that long. A node
that stops mid-settle asks the store once more, in the stop's settle window, and
leaves a take that attempt does not settle unsettled for good: no other node
knows of it, and the record says the run read notes its reader was never handed —
a capture counts them held, and a continuation is not told them — though the
review reads them; the stop says so. A review's take still lost when the
review concludes — its last round asked for no tool, or the store stayed silent
past the thirty seconds — leaves that review without the note: where the turn
goes on, the relaunch's brief settles it, or the mark the segment's end makes
tells it to a launch that continues the session; where the turn ends there,
nothing reads it. A note a settle marks has no `delivered` record of the step
that recorded it, which never handed it over, and the review reads it carried,
which records nothing anew. Its run's ending reports it instead (below).

**Mid-upgrade.** Both fields are wire-additive (`omitempty`), and an older build
keeps them in the read's `Extra` through every write it makes. An older build's
takes name nothing and are never settled, as before; it reads a read marked
unanswered as read, and its capture's cursor counts it held, as before — while
this build's capture stops before it, so an older build continuing it tells
every note past that cursor, the unanswered one among them, with no header. No
state-log record and no `coord.ProtocolVersion` change.

## Amendment: a note a harness may have delivered is on the record before the harness has it

OpenCode's notes plugin is answered its notes by a poll of the run's notes
route, admits each into the run's session, and confirms a note to the route
only once OpenCode reports delivering it; only that confirmation recorded the
note read. A harness cut off between the delivery and a confirmation that
landed — killed, stopped at its turn's cap, or its confirmation refused because
the run's claim had already moved it on — left a session that may hold the note
while the record said it waited, and the next launch's brief told it as a note
the turn had not read: the same note twice, the second time as an instruction
of its own.

**A poll answers a note only once the record says it was SERVED** to the job
(`sandbox.RunNote.Served`, `sandbox.PendingStore.ServeNotes`), in a write before
the answer, pinned as a take is to the job, its working statuses and an
undecided ending; a note the record could not mark is not answered, nor is any
after it, since the plugin's cursor moves past every note it is answered, and
they wait for the next poll. The mark stays once the note is read. A segment resuming the
turn carries the served notes' ids (`runner.Resume.Served`), final for the
segment since no poll serves a run its claim has moved on; and a launch that
continues a session — a kept capture, or the job's own copy in its box — tells a
served note under the header of notes it may already show
(`runner.toldNotes`): read since by a review, among the notes past its cursor,
or still waiting, among the notes the brief is the first to read, the header
taking every note up to the last served one, so the order the notes were read
in is kept, and only the notes after it following plainly — each one the turn
had not read before the brief, which is what the header says of them. A fresh
run is told every note, as before.

**Why the mark rather than reading the session.** Whether OpenCode delivered a
note is OpenCode's to say, in a session the engine reads only once it is
packed, and a delivery the plugin never confirmed leaves nothing on the engine's
side but the poll that handed it over. The run's script cancels, once the run
has ended, every steer the plugin admitted and OpenCode never delivered, so a
served note may also be in no session at all: "may already show" is the most
the record can say, and it says it.

**What it does not reach.** A note a bridge of an older build served carries no
mark, and a continuation tells it as one the turn had not read, as before. A
served note still waiting when the turn ends is reported `unconfirmed`, never
`expired` (below).

**Mid-upgrade.** The field is wire-additive (`omitempty`), and an older build
keeps it in the note's `Extra` through every write it makes. An older build's
poll serves without the mark, and reads a served note as waiting, as before. No
state-log record and no `coord.ProtocolVersion` change.

## Amendment: a note's end is reported as its record knows it

A run's ending reported what became of a note in one word, and only of the
notes no job ran with: `expired`, for one still waiting or read into the brief
of a job that never started. A note a take recorded read whose answer was lost,
marked unanswered, got no report at all: no step reported reading it, and the
ending did not report it either, though the brief of every later launch tells
it again and a launch whose job started had handed it to its harness. And a
served note its harness never confirmed was reported `expired`, though its
session may hold it.

**A launch records the marked notes its brief tells again.** The runner names
every read it carries marked unanswered as the brief is built
(`runner.AgentRunRequest.Retold`), and every brief a launch may hand its run
tells each of them again: the fresh one among every note the turn has read, and
a continued session, kept or the job's own copy in its box, from the first such
read on. The launch's write records each one the record still marks read by
the job it opens, told again (`sandbox.PendingRun.RetoldNotes`), and keeps the
marked take's read beside the telling (`sandbox.NoteRead.Retold`). Who read it
changes to that job's brief, unmarked; where it sits among the reads stays the
marked take's own place (`sandbox.NoteRead.Order`), as a lost take's note is
recorded (the first amendment above), so every later brief tells it where the
record first read it. That job's session holds it from its brief, so a capture
of it counts it held, and a continuation of the job cut off is told it with
every note the turn has read, as a cut-off session always is. The take's read
is kept for the one case where the brief handed nothing over: a launch that
fails after its write, whose job never starts, ends the record, and its ending
reports the note where the record knew it was missed before that write.

**The ending reports every note no step reported reading, as the record knows
it** (`sandbox.PendingRun.EndedNotes`, `sandbox.CoordinatorOptions.SteersEnded`,
the engine's `runSteers.ended`), in this order of precedence:

- `delivered`, told again (`types.AgentTurnSteered.Retold`): read by a later
  launch's brief that told it again, where that job started, at that brief
  (`boundary: brief`, its `launch_id` and iteration). The step that took it
  first never handed it over, and nothing reported the telling; its harness
  was handed it in its brief, under the header of notes it may already show.
- `unconfirmed` (`types.SteerUnconfirmed`): served to a job's harness and read
  by no step that handed it over (still waiting, or read only by a take marked
  unanswered or by the brief of a job that never started), at that job
  (`boundary: step`, its `launch_id`, and the iteration the record keeps for it
  where it is still the record's job). Ahead of the two below, since a session
  may hold it, and a note that may have reached the run is never said to have
  reached nothing.
- `missed` (`types.SteerMissed`): read by a take marked unanswered and told
  again by no later launch whose job started, at the step that took it (its
  phase, iteration, round, boundary and job). The turn ended with the mark
  standing: its review read the note carried and accepted the work, or the
  turn ended before any later launch. Or a later launch told it again and
  failed after its write, its job never started: the brief handed nothing
  over, and the ending reports the note at the take's read the telling kept.
- `expired`, as before: still waiting, or read into the brief of a job that
  never started where that brief was the first to tell it, at no step.

A note a step reported reading is none of them: that step published it
`delivered` as it read it. So every note on a record is reported once, by the
step that read it or by the ending, under the ids the ending used before (the
turn, the ending and the note, never the outcome), at the ending's instant.

**A confirmation whose answer was lost reports its read all the same.** Inside
an OpenCode run the step that reads a note is the plugin's confirmation, and
its report rode on the answer of the take that recorded the read: a take whose
write landed and whose answer was lost reported nothing, the confirmation the
plugin then made again found the note read and skipped it, and the ending, a
step having read the note, reported nothing either. So the confirmation's take
names itself, and one that answered an error is settled on its own path, in
answer mode, under a bound of its own (`runSteers.settleOnce`: ten seconds and
the node's stop window), never the request's, which the plugin gives up on:
what its write recorded is reported `delivered` there, and the confirmation is
answered done (`runInbox.confirm`). And a confirmation that finds a note it
names already read at a step of its job reports it `delivered` too, under the
id the first report of that read took (the turn and the note) and at the
read's own instant (`runInbox.confirmedBefore`), so a note both reported is
one record.

**A confirmation no inbox took is turned away, never answered done.** An
inbox that serves no job, closed by its session's end,
records nothing and says so (`mcpbridge.ErrNoteUntaken`), and the
bridge turns the confirmation away to ask again, as it turns away one that
reaches a session whose end took the inbox already. Answered done, the plugin
never confirmed the note again, and the ending reported unconfirmed a note
OpenCode had delivered. The plugin makes the confirmation again on its own
backoff, a second doubling to thirty, and reads no Retry-After; made again, it
reaches the inbox that serves the job now, or is refused where the run is
over. So is a confirmation reaching a run whose tools are closed, which takes
nothing more from its notes: a seat leaving its node closes them before its
release ends its session there, and the confirmation made again reaches the
seat's next holder, which records it. And no session serves an inbox not yet
bound: a run's inbox is bound to its job before the bridge files the session
(`Engine.openBridge`), which a recovery's box reaches the moment it is filed.
An inbox whose job its record takes no read of any more says so the same way,
for a note named that its take left waiting: the job's completion claimed the
run while its session still serves, and the confirmation made again is
recorded where the claim is handed back and the run goes on in its box. And a
node that cannot open a note named that still waits, sealed under a key it
does not hold, records the confirmation's other notes and turns it away the
same way, saying so once for the note (`steer_read_key_unknown`): the note
waits for a node that holds its key, as it does for every reader, and the
confirmation made again is recorded once it reaches one. A run that ends
first leaves the note served and never confirmed, which its ending reports
`unconfirmed` where the node ending the run holds the key; a node that does
not records nothing of the note (`steer_note_end_unrecorded`).

**One constructor states every outcome with its step.** `steer.Record` takes an
`End` that `steer.Delivered`, `Retold`, `Missed`, `Unconfirmed` and `Expired`
alone make: each but an expiry carries the step it came to, an expiry none, and
the zero `End` is an expiry.

**Why the ending reports a note told again, and not the runner that briefed
it.** The record is the one witness of both the telling and whether the job
started, which the ending already reads to report a brief's notes expired; and
an ending's report is made again until it lands, before the record goes, where
a runner's, made once after its launch returned, is lost with its node.

**Why two words, not one.** The record knows different things of a missed note
and an unconfirmed one. A missed note's reader certainly never had it from the
take that recorded it, the take having answered its reader nothing, and no
later launch whose job started was told it. An unconfirmed note's session may
hold it, and nothing says it does. One word for both would tell a person less
than the record knows.

**What it does not reach.** A take still unsettled when its node stops, its
last attempt unanswered, leaves its read unmarked, and the ending reports
nothing of a note the record says was read and answered. A mark that lands
after a launch's brief was built is on no brief as marked: where that brief told
every note all the same (a fresh run's, or a cut-off session's continuation),
the note is reported `missed` unless a later launch tells it again, though that
harness was told it. A review's take still unsettled at its segment's end
whose mark the store answers neither in the segment's path nor before the
ending reads the record, and a hook's take whose mark a person's stop outruns
the same way, leave a read the ending takes for a step's report, unmarked, and
that note is reported by no node. A missed note the review reads carried,
which records nothing anew, is still `missed`. A note told again is
reported when the turn ends, not when its brief was handed over. And a
confirmation whose take and settle both went unanswered, and which its plugin
never makes again, its run over first, is reported by no node: the record says
the note was read at that step, which it was.

**Mid-upgrade.** Additive throughout. The read gains `retold`, wire-additive
(`omitempty`), which an older build keeps in the read's `Extra` through every
write it makes. The event gains `retold` and two values of `outcome`
(`types.SteerOutcome.Valid`): an older build's dashboard draws the two as
`expired` and a note told again as read at that brief, and this build's draws
an outcome it does not know as one it cannot name, never as `expired`. An older
node's launch names no note told again, so its write leaves the mark standing,
and the ending reports the note `missed` unless a later launch tells it. An
older node that finishes an ending reports as before, under the same ids: a
served note `expired`; a marked one, and one a job that started told again,
not at all. No state-log record and no `coord.ProtocolVersion` change.
