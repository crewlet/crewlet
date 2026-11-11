# ADR-0067: A note to an executor waits sealed on its record

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestAnUnsealedNoteOnARowReachesNoModel`, `internal/engine.TestANoteSealedForAnotherTurnIsNotDelivered`, `internal/engine.TestANoteUnderAnUnheldKeyWaitsAndNamesTheHeldKeys`, `internal/engine.TestAnAnchorThatDoesNotOpenDecidesNoAuthority`, `internal/engine.TestARunIsSteerableAfterItsLaunchingNodeRestarts`, `internal/engine.TestATakenNoteIsRecordedDeliveredOnce`, `internal/engine.TestTheReviewerOpensWithANoteTheRunTook`, `internal/engine.TestAParkedRunHoldsItsNotesForWhatResumesIt`, `internal/engine.TestAnOpenCodeRunsNoteIsReadAtItsNextStepByItsJob`, `internal/engine.TestARunsRecordIsASteerSource`, `internal/engine.TestAnEndingExpiresWaitingNotesOnceUnderStableIDs`, `internal/engine.TestANoteLeftWaitingWhenARunEndsIsRecordedExpiredByTheEnginesCoordinator`, `internal/engine.TestAnOfferRacingTheLaunchIsNeverAnsweredClosed`, `internal/engine.TestEveryNoteAcceptedAcrossTheHandOverIsOnTheRecord`, `internal/engine.TestTheBoxsNotesAreOnTheRecordAheadOfOneFiledAsItIsWritten`, `internal/engine.TestALaunchThatFailsAfterItsWriteLeavesItsNotesToTheRecordsEnding`, `internal/engine.TestAResumedTurnCarriesItsNotesInTheOrderRead`, `internal/sandbox.TestAFailedLaunchsEndingReportsTheNotesItsWriteHeld`, `internal/engine.TestEveryInstructionChannelSignsInDomainsOfItsOwn`, `internal/sandbox.TestPendingStoreContract`, `internal/agent/builtin.TestARunRecordDecidesWithoutAScatter`, `internal/agent/builtin.TestAuthorityIsDecidedOnTheAnchoredSeat`, `internal/api/mcpbridge.TestSteerTakeIsServedUnlistedAndNeverRecorded`, `internal/sandbox/executor.TestARealBoxsHooksReachTheBridgeTool`, `internal/sandbox/executor.TestThePluginAdmitsNotesAtTheRunsNextStep`, `internal/sandbox/executor.TestThePluginReadsTheBridgesOwnNotesRoute`, `internal/sandbox/executor.TestARunCancelsTheNotesItsSessionNeverDelivered`, `internal/sandbox.TestACapturesNoteCursorStopsBeforeAReadItsHarnessNeverHad`, `internal/sandbox.TestEveryStampOfACaptureStatesTheNotesItsSessionHolds`, `internal/engine.TestAHooksTakeWhoseAnswerWasLostIsSettledUnanswered`, `internal/engine.TestAReviewsTakeWhoseAnswerWasLostHandsItsNotesOver`, `internal/agent/runner.TestAKeptSessionIsToldAgainEveryNoteFromAReadItsHarnessNeverHad`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldAgainEveryNoteTheTurnRead`, `internal/agent/runner.TestTheExecuteRecordListsItsRoundsReads`, `internal/engine.TestASettleAsksUntilItsStoreAnswersAndNoLongerThanItsNode`, `internal/engine.TestAPolledNoteIsServedOnItsRecordBeforeItIsAnswered`, `internal/agent/runner.TestASessionIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow`, `internal/agent/runner.TestARunCarriedOnInItsBoxIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow`
- **Measured:** on Claude Code 2.1.263 and 2.1.310, against stub APIs: a hook's tool call that fails makes the CLI drop and re-initialise the run's whole MCP server, telling the model its tools are still connecting — so a hook's call is never answered with an error; a hook's timeout is in seconds, and at it the CLI cancels the call and discards a late answer — so the engine answers within half of it; additional context longer than 10,000 UTF-16 code units is moved to a file the model is shown a 2 KB preview of — so one answer carries at most 9,000. On OpenCode 2.0.32 and 2.1.4, a second admission under an id the session already holds is answered with the first — so a note's id is derived from the job and the note; a note admitted during a tool call, or during the final model call, is read in the same execution, and on 2.1.4 one admitted after the execution settles, with `resume: false`, starts nothing — so a note sent after a run's last step waits on the record for the review — but stays pending in the session, across the server's restart, and is handed to the next run in that session ahead of its prompt unless it is cancelled (`session.inbox.cancel`) — so a run's script cancels every admission OpenCode never delivered once the run has ended. A sealed note is at most about 11 KiB, and a turn takes at most ten.
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
and its execute record lists it where it did; meanwhile, for thirty
seconds, it stands for a note waiting, so a round asking for tools ends and the
next one opens with it (`runner.SteerSource.Waiting`). Only a take still
unsettled when the segment ends is settled off its path, marked, as a hook's
is. An OpenCode run's confirmation of a note OpenCode delivered needs no
settle: its read is true whether or not its answer arrives, and the plugin
confirms again.

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
nothing reads it. A note a settle marks has no `delivered` record: the step
that recorded it never handed it over, and the review reads it carried, which
records nothing anew.

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
mark, and a continuation tells it as one the turn had not read, as before. And
a served note still waiting when the turn ends is reported `expired`, as every
note no job's read recorded is, though the session may have read it.

**Mid-upgrade.** The field is wire-additive (`omitempty`), and an older build
keeps it in the note's `Extra` through every write it makes. An older build's
poll serves without the mark, and reads a served note as waiting, as before. No
state-log record and no `coord.ProtocolVersion` change.
