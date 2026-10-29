# ADR-0068: A run asks a person in the shape a decision is asked

- **Status:** accepted
- **Authority:** `internal/decision`
- **Enforced-by:** `internal/agent/builtin.TestARunsAskIsRecordedAsADecisionOnItsJob`, `internal/agent/builtin.TestEveryFaultOfARunsAskIsNamedAtOnce`, `internal/agent/builtin.TestARunIsToldWhatBecameOfItsAsk`, `internal/agent/builtin.TestAnAskThatDidNotLandIsSaidToTheRun`, `internal/api/queries.TestARunsAnsweredQuestionIsAnAnsweredDecision`, `internal/agent/runner.TestASubmissionBeforeTheRunAskedIsNotReplayed`, `internal/agent/builtin.TestAnswerRunDeliversAChoiceOfTheQuestionsOwnOptions`, `internal/sandbox.TestAnAskIsRefusedWithEveryFaultAtOnce`, `internal/sandbox.TestAnAskIsRedactedInEveryTextItCarries`, `internal/sandbox.TestAnAnswerNamesTheOptionItChoseAsTheRunAskedIt`, `internal/sandbox.TestAnAnswerByTurnRecordsTheOptionItChose`, `internal/sandbox.TestAnAnswerByTurnChoosingWhatWasNotOfferedIsSpentUnrecorded`, `internal/sandbox.TestARunEndedAtItsDeadlineIsNotParkedOnItsQuestion`, `internal/sandbox.TestPendingStoreContract`, `internal/engine.TestARunsAskEndsItLikeItsSubmission`, `internal/engine.TestTheAskBriefNamesWhomTheParkCanResolve`, `internal/api/queries.TestAParkedQuestionReachesItsAudience`, `internal/e2e.TestARunsStructuredQuestionIsAnsweredWithAChoiceAndResumes`, `internal/decision.TestADecisionIsValidatedField`
- **Cost-when-tried:** a run that could not go on without a person ran one shell command, `crewlet-ask "<question>" --to <audience> --branch <branch>`, whose one free-text argument carried everything: the options, the recommendation and the context arrived as a paragraph ("Options: (A) [recommended] keep EXPECTED_DIFFERENCES ... (B) ... (C) Something else ... Which option?"), and the Inbox drew it as a heading repeating its first sentence over the whole text quoted verbatim, with nothing to press. The engine bounded and condensed the text after the run had ended, when the run could no longer fix it, and a question too long for that was not asked at all.
- **Tag-status:** unreleased

## The decision

**A coding run that cannot go on without a person's answer asks through one
tool on its bridge, `ask_and_wait`, in the shape a work item's decision is
asked** ([ADR-0023](0023-a-decision-is-a-structured-ask.md)): a question in one
sentence, its context in Markdown, and, where the person is to choose, a
`decision.Decision` (two to six options, the one recommended and why, the
evidence, the role). The shape, its caps and its structural contract are
`internal/decision`'s, shared by the tracker and the run record; the tool reads
it with the same schema and checks its evidence against the same tracker and
knowledge base a work item's decision is checked against.

1. **Validated whole before anything is written**, every fault at once
   ([ADR-0059](0059-a-tools-contract-is-its-validators-and-every-fault-comes-back-at-once.md)),
   so the run fixes its ask while it still can. Nothing is condensed or cut
   afterwards.
2. **Recorded on the run's own row, by the job that asked** (the bridge carries
   the session's job on every call), only while that job is running, one
   question per job. The collection parks the run on what the row holds, never
   on anything read out of the box; a run ended at its turn's cap is not
   parked.
3. **Answered with a choice by id or in words** (`answer_run{turn_id, choice,
   answer}`), the choice checked against the question it answers, and read by
   the run as the option it wrote: `Chosen option: <label> (<id>).`
4. **Its answer outlives the run.** The record of the answer
   (`sandbox_run_answered`) carries the question, its decision, the choice and
   the person's words, and a person's answered decisions read it back beside a
   work item's, so what they decided for a run stays theirs to read after the
   run is gone. An ask that did not land is said to the run as a failed call,
   never a silent stop.
5. **No `inform`.** A run's answer resumes the run itself, so there is no
   answered turn of the seat's the engine could hold to a promise.

## Why the obvious alternatives are wrong

**Growing the shell shim's flags** (`--option`, `--recommended`) keeps the
structure in shell quoting an LLM composes, keeps validation after the run has
ended, and keeps a script the engine writes into somebody else's image.

**Filing the run's question as a decision on its work item** would leave a run
with no work item, and every run of a company on an external tracker, with no
way to ask; and it would make the answer travel two records that must agree,
the item's comment and the run's row, where the run's resume reads only the
row.

## What this does not decide

Who a question is put to (the park resolves the audience against the chart,
`internal/engine`), how an answer reaches the run (the seat's control topic and
the run record's own answer path, `internal/sandbox`), and how the dashboard
lays the ask out, which is the dashboard design's; it draws a run's ask with the
same components as a work item's.
