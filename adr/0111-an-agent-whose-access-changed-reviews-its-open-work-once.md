# ADR-0111: An agent whose access changed reviews its open work once, and the work it raised says so

- **Status:** accepted
- **Authority:** `internal/accessreview`
- **Enforced-by:** `internal/engine.TestALevelChangeAsksForExactlyOneReview`, `internal/engine.TestThreeQuickRevisionsAskForOneReview`, `internal/engine.TestAPausedSeatReviewsWhenItResumes`, `internal/engine.TestASettledReviewIsAckedWithoutATurn`, `internal/engine.TestAReviewTurnsToldRecordStopsTheNextTurnRepeatingIt`, `internal/engine.TestAnAskRaisedBeforeAChangeCarriesTheNoteUntilAskedAgain`, `internal/engine.TestTheSaveDryRunCountsOpenItems`, `internal/engine.TestAChangeOutsideTheClosedListAsksForNoReview`, `internal/engine.TestASeatWithNothingOpenIsAskedForNoReview`, `internal/accessreview.TestAWakeIsDerivedFromItsSeatItsToldRecordAndItsDigest`, `internal/accessreview.TestAReviewIsTheCompanysOwnTurn`, `internal/accessreview.TestTheSectionNamesTheWorkAndHowToWithdrawIt`, `internal/agent/builtin.TestASeatWithdrawsItsOwnHeldCallAsDeny`, `internal/agent/builtin.TestASeatWithdrawsItsOwnQuestionSayingWhy`, `internal/agent/builtin.TestASeatCannotWithdrawAnotherSeatsRun`, `internal/agent/builtin.TestASeatCannotWithdrawAQuestionItDidNotAsk`, `internal/agent/runner.TestARunIsToldItsSeatWithdrewWhatItWaitedOn`, `internal/api/queries.TestAQuestionAskedBeforeAnAccessChangeIsNotedUntilAskedAgain`
- **Cost-when-tried:** an SRE agent raised approval requests that assumed write access to a production server, and its access was then lowered to read. Told the change only at its next turn (ADR-0104), it had none: every turn it could have had waited on a person answering those requests. The requests stayed in the administrator's Inbox, worded for the old access; the administrator approved one, and only then did the agent find it could not run it. Its other open requests were stale the same way, and nothing on them said so.
- **Tag-status:** unreleased

## The decision

**A revision that changes an agent seat's access, by the closed change list a
turn is told, asks that seat for one review turn when the seat has open work.**
Open work is the questions it asked that are still open, what its executor
waits on a person for (a question, or a call held for approval), and the work
items assigned to it that are in progress or blocked.

- **Measured against what the seat was last told**, never the previous
  revision: the node holding the seat, once its applies have gone quiet for two
  minutes, compares the seat's snapshot with its told baseline (ADR-0104). A run
  of saves is one review, a revert before the review asks for nothing, and a
  rotation is no change.
- **Derived, not published as a courtesy** (ADR-0012, ADR-0095): the request is
  an inbound delivery of source `access_review`, re-derivable at any time from
  the document, the seat access ledger and the access log, so the seat's next
  holder checks on admission. Its wake id is derived from the seat, the told
  record the change is measured from and the digest it is measured to, so a
  republish collapses, and a change made, reverted and made again is reviewed
  again.
- **One turn, held as any turn is**: every review of a seat shares one
  partition, so waiting reviews merge; a seat runs one turn at a time; the
  seat's pause, budget and limits hold it as they hold any mail. The wake names
  no sender, so the turn is the company's own (ADR-0109).
- **Told once, through the told record**: the review turn is told the change at
  its dispatch and writes its told record at its launch, like every turn.
  Whichever turn tells a change lists the seat's open work and asks it to update
  or withdraw what the change affects, saying why; a review that finds nothing
  left to tell is acked without a turn.
- **The seat can act on it**: `withdraw_ask` withdraws its own question on a
  work item (the reason posted in the thread, the question resolved as its
  author) or what its own executor waits on (answered by the seat itself, a held
  call as Deny, so the call is never made and the run is told it withdrew it).

**A question or held call raised before a later change to its agent's access
carries a note** in the Inbox, "Asked before this agent's access changed", with
the change: the snapshot told at or before it was raised against the access now.
It goes when the agent asks again, when the person answers, or when the change
is reverted.

**The access save says it first**: its plan names each agent whose access the
draft changes and that has open work, with the count.

## Why the obvious alternative is wrong

ADR-0104 refused a wake at apply time because it is per node and per revision,
tells a revert as two changes and a rotation as one, and costs a turn nobody
asked for. Each holds against a wake that TELLS the change. This one tells
nothing: the turn it starts is told the change from the told record like every
other turn, it is asked by the one node holding the seat after the applies go
quiet, a revert or a rotation leaves no difference to ask about, and it is asked
only where a person is waiting on work the change may have made wrong. Leaving
the change to the seat's next turn is the alternative this record replaces: a
seat whose work waits on a person has no next turn until the person acts on
stale work.

Marking an ask stale from the revision history alone would note a question a
revert has made current again; comparing against the access now does not.

## Amendments to earlier records

[ADR-0104](0104-a-seat-is-told-its-access-every-turn-and-what-changed-once.md):
a change still wakes no seat by itself. A seat with open work is asked for one
review turn (this record), which is told the change as every turn is.
