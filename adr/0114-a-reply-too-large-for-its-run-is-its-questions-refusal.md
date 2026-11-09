# ADR-0114: A reply too large for its run is its question's refusal

- **Status:** accepted
- **Authority:** `internal/sandbox`
- **Enforced-by:** `internal/sandbox.TestAReplyPastWhatTheRowCarriesIsRecordedAsItsQuestionsRefusal`, `internal/sandbox.TestAMessageAfterARefusedReplyIsNotTheRunsAnswer`, `internal/sandbox.TestARefusalLetGoOfSendsTheReplyBackAsItCame`, `internal/sandbox.TestARefusalLetGoOfLeavesItsQuestionToTheNextReply`, `internal/sandbox.TestARefusalTheInlineAttemptLetGoOfIsTheOrdinaryMessageAtOnce`, `internal/sandbox.TestARefusalsResumeIsRaisedWhereTheReplyWasPosted`, `internal/sandbox.TestARefusalATurnGaveBackKeepsItsReply`, `internal/sandbox.TestARefusedReplyOfferedAgainIsOneRecord`, `internal/sandbox.TestARefusalOpensOnlyAsTheRefusalItsRowRecords`, `internal/sandbox.TestARefusalTheInlineAttemptsTurnKeptIsSpentWhateverTheLedgerSays`, `internal/sandbox.TestARefusedReplyWhoseRoundWasCarriedOnFromItsRefusalIsSpent`, `internal/sandbox.TestARefusalATurnTookAndDiedHoldingIsSpentByTheReap`, `internal/sandbox.TestARefusalWhoseSealDoesNotOpenIsLetGoOfAndSaidOnce`, `internal/sandbox.TestPendingStoreContract`, `internal/engine.TestARefusedReplyIsKeptAtTheHeadAndRunsNoTurn`, `internal/engine.TestAResumedRecordCarriesItsRoundsRefusedReplies`, `internal/agent/runner.TestARunToldItsReplyWasTooLargeIsGivenNoneOfIt`, `internal/agent/runner.TestACollectedRoundCarriesItsRefusedRepliesToItsReview`, `internal/agent/runner.TestAReviewerIsHandedTheRoundsRefusedReplies`, `internal/agent/prompts.TestAReviewerIsToldOfARepliedQuestionsRefusal`, `internal/agent/builtin.TestAnAnswerAfterARefusedReplyIsToldTheReplyWasNotTaken`, `internal/events/types.TestSummaries`
- **Measured:** the run's record holds a reply at about 3.4 times its words in plain text and up to about 20 times in markup (each `<`, `>` and `&` six bytes, in the event and again in the answer's seal), so its 512 KiB share takes a pasted log of a hundred KiB and refuses five 32 KiB Chatlet messages delivered together, one Slack message of 40,000 characters of emoji, or one GitLab note of a megabyte. A refusal weighs a few hundred bytes on the row: the delivery ids, two figures, where the reply was posted (ids, under a KiB on every shipped surface, held to 4 KiB), and a seal with no words.
- **Cost-when-tried:** a reply past that share was not recorded and was handed to the seat as the ordinary message it looked like: the seat answered the person as if the message were new, the run went on waiting, and the person's next short message there ("any luck?") was taken for its answer. A second attempt had the seat's turn told of the refused reply so it would ask the person for a shorter one, and review found the same defect inside it: from the refusal to the seat's post the run still waited on its question, so a "thanks" or "any luck?" written in that window was taken as the answer. Review of this decision found its first draft deciding the delivery in hand off the completion ledger, which fails open: with the ledger unreadable, a reply whose run the inline attempt had just told it never reached it went to the seat as a new message.
- **Tag-status:** unreleased

## The decision

**A reply to a parked run's question that is past what the run's record
carries as its answer is not taken; its refusal is recorded as the question's
outcome, and the run is resumed with that refusal through the path an answer
takes.**

A reply within the record's share (`MaxAnswerBytes`) is the run's answer, word
for word, as it always was. One past it — one message, or several delivered
together — is recorded on the run's row in the answer's place as a refusal
(`PendingStore.RecordRefusal`, `PendingRun.AnswerRefusal`): its delivery ids,
how many messages it was, when it was posted, where it was posted (the
transport metadata of its newest message, ids alone, which the resumed turn
raises its working indicator in), and the two figures the engine states (the
reply's size rounded up with `kept.SizeUp`, the share in `kept.Size`), and
none of its words. The refusal is a key of its own at the top of the row,
which a build that does not know it carries through every rewrite
(wirekeys.go), and it binds to the recorded answer by the deliveries both
name.

From the record on, while the refusal is the run's, the question no longer
waits, so nothing the person writes after the refusal is taken for its answer.
The refusal is then resumed exactly as an answer is: recorded before anything
is done with it, owed with the seat's inbox held behind it, retried on the
answer's own schedule, taken once by a resumed turn, revived after a lost
claim, or let go of, by the same rules (`internal/sandbox`'s protocol and
answer-owed docs). The resumed run is told, in the engine's words and through
the channel a person's answer reaches it (`runner.Asked.Refused`), that the
person replied, that the reply was past what an answer can carry — with its
message count and both figures — so it was not taken and the run was given
none of it; that if it still needs it, it asks again with `ask_and_wait`,
saying in the question why, for a much shorter form or another way to get it
(a file in its checkout, a path, a link); and that otherwise it carries on and
says in its reply that the reply never reached it. A question asked again
takes its own anchor (`PendingRun.AskedAt`), so whatever the person wrote
before it is an ordinary message to the seat.

The row cannot carry the reply, so the delivery is the only copy there is: the
coordinator answers `AnswerRefused`, and the dispatcher hands it back by
deferral, unspent, at the head of the seat's inbox behind the hold the owed
refusal keeps. A turn that takes the refusal and keeps it records the delivery
as worked, and it is dropped when it comes round — never at the take, which a
turn that broke before it reached anybody gives back; a turn that took it and
died holding it has it spent by the reap of its claim, as an answer a turn took
is. The delivery in hand while the resume's first attempt runs is decided by
what that attempt did: kept by its turn, it is spent then and there, on the
attempt's own account, as an answer's is at its record — never on the
completion ledger's, which fails open, and whose miss would hand the seat, as a
new message, a reply its run had just been told never reached it. Otherwise it
is decided off the row as the attempt left it, read again: a refusal another
attempt's turn kept is spent, since the relaunch that told the run keeps the
refused question in the round's record (`PendingRun.RoundRefusals`), which only
such a turn writes; one still the run's is kept; and one that attempt already
let go of leaves the delivery the ordinary message it is at once. A copy that
comes round once the refusal has left the row — after a retry's turn kept it,
or to the seat's next holder — is dropped by the completion ledger that turn
recorded it in, one event at a time as every redelivered event is, and like
every such read it fails open: a miss runs it as the ordinary message it is.
A refusal let go of — after its attempts, by an ending no turn reached, or by a reap that
finds no turn took it — spends nothing, so the delivery comes round as the
ordinary message it is: the reply goes back to the seat the way it came, as an
answer let go of does. A reply to a run
answered another way, moved on to another question, or ended meanwhile is
matched to nothing and is the ordinary message every reply after an answer is.

**Let go of, the question waits again.** A refusal declined after its attempts,
or whose seal does not open, puts the run back on its question under the
anchor it was asked at, with the reply declined for it, as an answer let go of
does — and the next qualifying reply answers it, a short message written since
included. That is the one case the guarantee above does not cover, and it is
the answer path's own: nothing told the run, so the question is the person's
to answer again. The let-go is announced (`executor_answered` `declined` on the
chat route, with the refusal's figures, by whom the seal names — nobody where
it does not open), and an ending that hands such a reply back says so in its
announcement, because the refusal's own record said the run would be told.

The `executor_answered` record of the refusal stays, with the outcome
`too_large`, its figures and none of its words, under one id and one instant
per run, question and delivery, so every publish of it is one row; nothing
publishes a `resumed` record of a refusal, which a person's answered decisions
would read as an answer. The record states what is recorded — the reply
refused, the run to be resumed told so — never a telling that has not
happened.

**The turn's reviewer is told.** It judges the round from its calls, where the
reply is not, and where the run's question asked again reads as the same
question sent twice — the duplicate it sends a round back for — and a run's
word that the reply never reached it has nothing of the engine's to be held
against. So the relaunch that carries the round on from a refused question
keeps the refusal's message count and figures for the round's review
(`PendingRun.RoundRefusals`), and the review prompt shows them beside the log
in the engine's words (`## Replies the agent did not get`), saying that a
question asked again after a refused reply is the follow-up the engine told the
run to make, which the duplicate rule does not hold against it. Said there, in
a section only such a round renders, rather than in the rules every review
carries and pays for.

## Why the obvious alternative is wrong

**Having the seat tell the person** leaves the question waiting from the
refusal to the seat's post, with the anchor it already had, and a short message
written in that window is taken as the run's answer — the defect in its most
common form. Closing that window there needs a new anchor taken at the seat's
post, markers of what is owed, or bounded retells. And the seat does not know
what the run needed, so it cannot ask for the shorter answer that would serve.

**Having the engine tell the person** needs the engine to post in the
conversation. It sends nothing as itself on a vendor's chat, so on Slack or
Mattermost, the main case, the person would hear nothing.

**Having the seat answer the run** with what it read in the reply makes the
seat decide what the person meant: a summary of a pasted log drops the one line
the run asked about. Answering a run's question is a person's
(`authz.ActionExecutorAnswer`, and no seat's surface carries
`answer_executor`); the one way a seat closes its own run's question is
`withdraw_ask` ([ADR-0111](0111-an-agent-whose-access-changed-reviews-its-open-work-once.md)),
which hands the run the seat's reason as its own withdrawal, never as the
person's answer. Used here, it would still be the seat summarising a reply it
read — the line the run asked about dropped as surely — told to the run as a
question the seat took back, and the person's reply never named as the thing
that did not arrive.

**Lowering the bound** to what an answer by turn may say refuses exactly the
answer a coding run's question gets, a log or a configuration pasted whole,
which the record carries with room to spare.

**Handing the run a cut prefix** of the reply can drop the line it asked about,
and a run handed a prefix can take it for the whole reply.

## What this does not decide

Who may answer a run, which stays the authority table's and the question's
audience; the answer by turn's own bound (`MaxAnswerTextBytes`), which its tool
holds before anything is sent; and #814, a parked executor that takes any
notification on its conversation as its answer, which this does not change.
