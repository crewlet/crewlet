# ADR-0023 — A decision is a structured ask, and the engine enforces only its shape, its answer and its promise

- **Status:** accepted
- **Authority:** `internal/tracker`
- **Enforced-by:** `internal/tracker.TestADecisionRecordIsRetainedByABuildThatCannotReadIt`, `internal/tracker.TestAChoiceMustNameAnOptionOfTheAskItAnswers`, `internal/engine.TestAnInformedAnswerOwesTheChatSurface`
- **Cost-when-tried:** "no decision engine" was read as "no decision structure". A question that needed somebody to choose travelled as prose on an `ask` comment — the options somewhere in the body, the recommendation a sentence among others, the evidence links a reader went looking for — and the answer was prose the asker then had to map back onto the options it had in mind. The dashboard design dropped its "Also posts to Slack" line because nothing could keep it; when the line came back as `inform`, the tracker accepted it from an operator, for a surface the company did not run and for a channel nothing declared, and a second answer to one ask landed beside the first, claiming to close a question the first had already closed.
- **Tag-status:** unreleased

## The decision

A decision somebody needs from somebody else is an **ask on a work item that
carries a `Decision`** — a question, two to six options, what the asker
recommends and why, the evidence it looked at, the role the person is asked in
and, optionally, the chat channel the asker will report the outcome in — and
the answer is a comment that names **one of those options by id**. The type,
its caps and its validation are `internal/tracker/decision.go`; the writing
tools are `comment_on_work_item` and `create_work_item` in `agent/builtin`,
the same tools a seat, an operator's assistant and the dashboard's buttons all
reach.

It is a tracker record and nothing else, so it answers "who has to agree on
it?" the way every other comment does: every node, identically, derived from
the tracker's log. There is no decision table, no decision stream and no
decision event.

The engine enforces **exactly three things**, and forbids itself a fourth:

1. **The shape** — a decision is well formed, rides only the comment that
   asks, and never changes after it was asked, because an answer names an
   option by id and options edited under it would change what the answer
   meant. A record carrying one is version-gated, so a build that cannot read
   it retains it rather than applying the comment without it.
2. **The answer** — a `choice` names an option of the ask it answers, checked
   against that ask's own row inside the writer's decide, and an ask is
   answered once.
3. **The promise** — an `inform` is accepted only from an agent seat, only for
   a chat surface the company runs and the seat holds a bot on, and only for a
   channel a unit declares; the asker's `answered` wake then **owes** that
   surface, and the turn it wakes is not finished until a tool there has
   posted.
4. **Nothing else.** No approval chain, no quorum, no state machine, no
   escalation timer and no routing of a decision to an approver the asker did
   not name. The DACI roles stay behavioural guidance; `role` is a label that
   tells the person whether their answer *is* the decision or an input to one,
   and gates nothing.

## Why the obvious alternatives are wrong

**Leaving decisions to chat prose**, the previous reading, puts every part a
person needs to answer well somewhere a reader has to go and find it, and
returns an answer only a model can map back onto an option — and wrongly, at
exactly the moment it matters. A card cannot render a choice nobody
structured, and a button cannot send one.

**A decision engine** — an approval workflow the engine routes, times out and
escalates — would make one company's way of deciding the engine's, and would
be a second owner of who-answers-what beside the org chart and the tracker's
own ask routing. Every rule it would carry is a policy a company changes in
prose to its agents today; written into the engine, each would be a
configuration surface, a migration and a state machine to keep consistent
across a rolling upgrade.

**Enforcing the inform by trusting the asker** is what the card's line cost the
first time: a promise on the answerer's screen that nothing kept. It is held on
the turn because the turn is the only thing that can post, which is also why a
person, who has no turn, may not make it.

## What this does not decide

It does not decide who is asked — the asker names the person, and the tracker's
ask routing wakes them — nor how the dashboard lays a decision out, which is
the dashboard design's. It does not decide what an answered asker does next:
that is the asker's turn. And it does not give meaning to the `decision`
event category's four mapped types, which nothing in this build publishes.
