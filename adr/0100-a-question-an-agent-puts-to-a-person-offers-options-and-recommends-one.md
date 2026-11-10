# ADR-0100: A question an agent puts to a person offers options and recommends one

- **Status:** accepted
- **Authority:** `internal/decision`
- **Enforced-by:** `internal/decision.TestAnAgentsDecisionRecommendsOneOptionAndSaysWhy`, `internal/decision.TestADecisionIsValidatedField`, `internal/worklet.TestAnAgentsDecisionRecommendsOneOptionAndSaysWhy`, `internal/agent/builtin.TestAnAgentsQuestionToAPersonIsADecision`, `internal/agent/builtin.TestAnAgentsRequestThatNamesAPersonIsAnAsk`, `internal/agent/builtin.TestARunsAskAlwaysOffersOptions`, `internal/sandbox.TestAnOwnQuestionIsAlwaysAChoice`
- **Measured:** one company's owner over seven days: 18 of 50 answered questions (36%) were asked in prose with no decision, every one answered in words; 31 of the 32 asked as decisions were answered with one press. One run's question and the one question still open were prose too.
- **Cost-when-tried:** [ADR-0023](0023-a-decision-is-a-structured-ask.md) and [ADR-0068](0068-a-run-asks-a-person-in-the-shape-a-decision-is-asked.md) made the decision available and left it optional, "where the person is to choose", with the recommendation "empty when it has no view". Agents read that as permission: a request ("the merge is yours", "please run the script on production", "QA passed, over to you") went out as an `ask` with a paragraph and nothing to press, and four of them wrote their options and a recommendation into the prose instead.
- **Tag-status:** unreleased

## The decision

**Every question an agent puts to a person carries a decision, and every
decision an agent composes names the option it recommends and why.** It
refines ADR-0023 and ADR-0068 in three places, and changes nothing else they
decide:

1. **Two to four options**, for every decision, whoever asks. Past four the
   options no longer sit side by side as one-press answers, and "Something
   else" is always offered beside them, so no fifth option is needed to let a
   person answer otherwise.
2. **An agent's decision recommends**: `recommended` (one of its options) and
   `rationale` are required of a decision an agent seat composes, on a work
   item's ask (checked by the asking tools and again by the tracker's writer,
   which every comment passes through) and on a run's ask (checked by
   `ask_and_wait` and again by the run record). A person's own decision may
   leave them out, since a person asking may have no view, and so does the
   engine's approval of a held tool call
   ([ADR-0077](0077-a-tool-call-that-needs-approval-parks-its-run-in-the-shape-a-runs-ask-does.md)),
   which asks on the model's behalf and has nothing to recommend.
3. **An agent's question to a person is a decision**: `comment_on_work_item`
   and `create_work_item` refuse an agent seat's `ask` that names anybody the
   roster does not hold as an agent seat unless it carries a `decision`, and
   `ask_and_wait` has no open question at all. The refusal tells the agent to
   offer the answers it expects. An agent's comment that asks (`asks` true)
   and names a person in its words, other than the one its `ask` names, is
   refused the same way: otherwise the question reaches the person as a
   mention, never among the decisions waiting on them. A question between two
   agent seats may still be asked in words, and so may a person's.

A question recorded before this, with no decision, is read as it was stored and
answered in words: nothing is migrated.

## Why the obvious alternatives are wrong

**Guidance alone** ("prefer a decision") is what ADR-0023 shipped, in the
escalation section and the tool descriptions, and over a third of the questions
a person was asked still arrived as prose. An agent composing a request does
not experience it as a choice; only a refusal at the tool turns it into one.

**A dashboard that guesses options from prose** would put words in the asker's
mouth at the moment a person acts on them, and would answer with an option id
the asker never defined.

**Requiring a decision of every ask**, agent to agent included, makes a
colleague's quick question a form, and an agent reads a prose answer as well as
a choice. The person is who needs the buttons.

## What this does not decide

How the dashboard lays a decision out (the dashboard design's), who a run's
question is put to (`internal/engine`), or what an answered asker does next.
It does not make a person's recommendation required, and it does not give the
engine's held-call approval a recommendation.
