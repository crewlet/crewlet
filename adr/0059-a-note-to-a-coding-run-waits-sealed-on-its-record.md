# ADR-0059: A note to a coding run waits sealed on its record

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestAnUnsealedNoteOnARowReachesNoModel`, `internal/engine.TestANoteSealedForAnotherTurnIsNotDelivered`, `internal/engine.TestANoteUnderAnUnheldKeyWaitsAndNamesTheHeldKeys`, `internal/engine.TestAnAnchorThatDoesNotOpenDecidesNoAuthority`, `internal/engine.TestARunIsSteerableAfterItsLaunchingNodeRestarts`, `internal/engine.TestATakenNoteIsRecordedDeliveredOnce`, `internal/engine.TestTheReviewerOpensWithANoteTheRunTook`, `internal/engine.TestAParkedRunHoldsItsNotesForWhatResumesIt`, `internal/engine.TestAnOpenCodeRunsNoteIsReadAtItsNextStepByItsJob`, `internal/engine.TestARunsRecordIsASteerSource`, `internal/engine.TestAnEndingExpiresWaitingNotesOnceUnderStableIDs`, `internal/engine.TestAnOfferRacingTheLaunchIsNeverAnsweredClosed`, `internal/engine.TestEveryNoteAcceptedAcrossTheHandOverIsOnTheRecord`, `internal/engine.TestEveryInstructionChannelSignsInDomainsOfItsOwn`, `internal/sandbox.TestPendingStoreContract`, `internal/agent/builtin.TestARunRecordDecidesWithoutAScatter`, `internal/agent/builtin.TestAuthorityIsDecidedOnTheAnchoredSeat`, `internal/api/mcpbridge.TestSteerTakeIsServedUnlistedAndNeverRecorded`, `internal/sandbox/codingagent.TestARealBoxsHooksReachTheBridgeTool`, `internal/sandbox/codingagent.TestThePluginAdmitsNotesAtTheRunsNextStep`, `internal/sandbox/codingagent.TestThePluginReadsTheBridgesOwnNotesRoute`, `internal/sandbox/codingagent.TestARunCancelsTheNotesItsSessionNeverDelivered`
- **Measured:** on Claude Code 2.1.263 and 2.1.310, against stub APIs: a hook's tool call that fails makes the CLI drop and re-initialise the run's whole MCP server, telling the model its tools are still connecting — so a hook's call is never answered with an error; a hook's timeout is in seconds, and at it the CLI cancels the call and discards a late answer — so the engine answers within half of it; additional context longer than 10,000 UTF-16 code units is moved to a file the model is shown a 2 KB preview of — so one answer carries at most 9,000. On OpenCode 2.0.32 and 2.1.4, a second admission under an id the session already holds is answered with the first — so a note's id is derived from the job and the note; a note admitted during a tool call, or during the final model call, is read in the same execution, and on 2.1.4 one admitted after the execution settles, with `resume: false`, starts nothing — so a note sent after a run's last step waits on the record for the review — but stays pending in the session, across the server's restart, and is handed to the next run in that session ahead of its prompt unless it is cancelled (`session.inbox.cancel`) — so a run's script cancels every admission OpenCode never delivered once the run has ended. A sealed note is at most about 11 KiB, and a turn takes at most ten.
- **Cost-when-tried:** before this, a turn whose executor was its coding CLI's own run answered every note `steer_unsupported`, and a turn parked on its run answered `closed`: a person could not correct a coding run at all, and a correction sent while a run worked never reached the reviewer that judged the work.
- **Tag-status:** unreleased

## The decision

**From the launch of a turn's coding run until the turn ends, the turn's notes
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
whichever node it is on. **Only an ending expires a note**, under an id derived
from the ending, so a retried ending records it once; a park never does.

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
`internal/sandbox/codingagent`'s, CLI by CLI — nor what a note to a turn in the
engine's own loop does, which is the turn engine's.

It leaves two things unsealed, and states them. The `read` marker on a note is
guarded only by the store's compare-and-set: a party that reaches the
coordination store could mark a genuine note read on its own turn, which keeps
it from the run's CLI — the review still reads every note the record holds, so
the correction still binds the work that is judged — or clear the marker, which
hands the run a genuine note again. And a run's box can take its own notes
without putting them before its model, by calling the bridge's take itself; a
note taken so is recorded read and carried into the review.
