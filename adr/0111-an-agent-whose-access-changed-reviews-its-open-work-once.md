# ADR-0111: An agent whose access changed reviews its open work once, and the work it raised says so

- **Status:** accepted
- **Authority:** `internal/accessreview`
- **Enforced-by:** `internal/engine.TestALevelChangeAsksForExactlyOneReview`, `internal/engine.TestAChangeRevertedAndMadeAgainIsReviewedAgain`, `internal/engine.TestThreeQuickRevisionsAskForOneReview`, `internal/engine.TestALedgerWriteMeasuresTheSeatItNames`, `internal/engine.TestAPausedSeatReviewsWhenItResumes`, `internal/engine.TestASettledReviewIsAckedWithoutATurn`, `internal/engine.TestAReviewTurnsToldRecordStopsTheNextTurnRepeatingIt`, `internal/engine.TestAnAskRaisedBeforeAChangeCarriesTheNoteUntilAskedAgain`, `internal/engine.TestTheSaveDryRunCountsOpenItems`, `internal/engine.TestOpenWorkCountsEachItemOncePastThePage`, `internal/engine.TestTwoFirstMeasurementsOfAChangeKeepOne`, `internal/engine.TestALateWriterLeavesAToldRecordTold`, `internal/engine.TestATurnsEndLeavesANewerChange`, `internal/engine.TestASettledReviewDropsItsUntoldRecord`, `internal/engine.TestARemovedSeatLeavesNoReviewBehind`, `internal/engine.TestAChangeOutsideTheClosedListAsksForNoReview`, `internal/engine.TestASeatWithNothingOpenIsAskedForNoReview`, `internal/accessreview.TestAWakeIsDerivedFromItsSeatItsToldRecordAndItsDigest`, `internal/accessreview.TestEveryCopyOfAWakeCarriesOneInstant`, `internal/accessreview.TestAQuestionIsStaleUntilTheChangeIsTold`, `internal/accessreview.TestAReviewIsTheCompanysOwnTurn`, `internal/accessreview.TestTheSectionNamesTheWorkAndHowToWithdrawIt`, `internal/capability.TestAPersonReadsAChangeAboutTheSeat`, `internal/capability.TestAnAudienceIsOneThisBuildWordsLinesFor`, `internal/agent/builtin.TestASeatWithdrawsItsOwnHeldCallAsDeny`, `internal/agent/builtin.TestASeatWithdrawsItsOwnQuestionSayingWhy`, `internal/agent/builtin.TestAWithdrawalIsHeldToWhoeverWokeTheTurn`, `internal/agent/builtin.TestASeatCannotWithdrawAnotherSeatsRun`, `internal/agent/builtin.TestASeatCannotWithdrawAQuestionItDidNotAsk`, `internal/agent/runner.TestARunIsToldItsSeatWithdrewWhatItWaitedOn`, `internal/api/queries.TestAnAgentsAccessChangeIsDetailedOnlyToASetupReader`, `internal/api/queries.TestEachSeatsReviewIsReadOncePerAnswer`
- **Cost-when-tried:** an SRE agent raised approval requests that assumed write access to a production server, and its access was then lowered to read. Told the change only at its next turn (ADR-0104), it had none: every turn it could have had waited on a person answering those requests. The requests stayed in the administrator's Inbox, worded for the old access; the administrator approved one, and only then did the agent find it could not run it. Its other open requests were stale the same way, and nothing on them said so.
- **Tag-status:** unreleased

## The decision

**A change to an agent seat's access, by the closed change list a turn is
told, is recorded as the seat's pending review and asks the seat for one review
turn when it has open work.** Open work is the questions it asked that are
still open, what its executor waits on a person for (a question, or a call held
for approval), and the work items assigned to it that are in progress or
blocked, each item counted once, past the items a review lists too.

- **Measured against what the seat was last told**, never the previous
  revision: the node holding the seat measures it once its applies have gone
  quiet for two minutes, on admission, and whenever a write to the seat access
  ledger names it (a sync finishing, a vendor step, observed drift). A run of
  saves is one review, a rotation is no change, and a change undone before any
  turn was told it drops its record.
- **A pending review is a record in the coordination store**, per seat: the
  told record it is measured from, the digest it is measured to, when it was
  first measured, the turn first told it and the change in a person's words. A
  change made, reverted and made again is measured afresh, so it is reviewed
  again. Any node may measure a seat, so every write is conditional on the
  revision it read (a first record is created, a change updates the revision
  read, the end of a review deletes it), and a write that loses reads the
  record again. A seat the company removes takes its record with it, so a seat
  added later under its handle starts with none.
- **Derived, not published as a courtesy** (ADR-0012, ADR-0095): the request is
  an inbound delivery of source `access_review`, its wake id the seat, the told
  record, the digest and the instant the change was first measured, its wake
  instant the told record's, the same on every copy. Reviews waiting together
  merge into one turn, the seat's pause, budget and limits hold it as any mail,
  and the wake names no sender, so the turn is the company's own (ADR-0109).
- **Told once, through the told record**: the review turn is told the change at
  its dispatch and writes its told record at its launch, like every turn.
  Whichever turn tells a change lists the seat's open work and asks it to update
  or withdraw what the change affects, saying why; a review that finds nothing
  left to tell is acked without a turn and drops a record no turn was told, so
  a change undone and made again inside one quiet window is reviewed again.
  When the turn first told the change ends, the pending review ends with it.
- **The seat can act on it**: `withdraw_ask` withdraws its own question on a
  work item (the reason posted in the thread, the question resolved as its
  author, held to whoever woke the turn as a comment is (ADR-0109): somebody
  who uses Worklet, or the person the question asks; the review's own turn,
  which nobody woke, is the seat's, and somebody outside the company never has
  the seat withdraw anything) or what its own executor waits on (answered by the seat itself,
  a held call as Deny, so the call is never made and the run is told it
  withdrew it).

**A question or held call raised before a pending review's change carries a
note** in the Inbox, "Asked before this agent's access changed", until the turn
told the change ends, the person answers, or the change is reverted untold. The
change itself, in the third person, is the company's setup: only a caller who
may read the setup (`setup.read`) is sent its lines, and everybody else is told
only that it changed. Each asking seat's record is read once per answer.

**The access save says it first**: its plan names each agent whose access the
draft changes, by the save's own edits, and that has open work, with the count,
said as "at least" where the tracker stopped counting at its ceiling.

## Why the obvious alternative is wrong

ADR-0104 refused a wake at apply time because it is per node and per revision,
tells a revert as two changes and a rotation as one, and costs a turn nobody
asked for. Each holds against a wake that TELLS the change. This one tells
nothing: the turn it starts is told the change from the told record like every
other turn, it is asked by the node holding the seat after the applies go quiet,
a revert or a rotation leaves no difference to ask about, and it is asked only
where a person is waiting on work the change may have made wrong.

Computing the note from the access log and the seat's live access on every
Inbox read costs a ledger read per integration and pages of told records per
asking seat on the Inbox's hot path; the pending record is one read.

## Amendments to earlier records

[ADR-0104](0104-a-seat-is-told-its-access-every-turn-and-what-changed-once.md):
a change still wakes no seat by itself. A seat with open work is asked for one
review turn (this record), which is told the change as every turn is.
