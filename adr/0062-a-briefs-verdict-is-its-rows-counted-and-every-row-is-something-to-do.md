# ADR-0062: A brief's verdict is its rows counted, and every row is something to do

- **Status:** accepted
- **Authority:** `internal/assistant/wire`
- **Enforced-by:** `internal/assistant/wire.TestTheVerdictIsTheNumberOfRows`, `internal/assistant/wire.TestABriefIsHeldToItsShape`, `internal/assistant/catalog.TestTheBriefSchemaAsksForAVerdictItCanCount`, `internal/assistant/catalog.TestABriefCallIsReadStrictly`, `internal/assistant/catalog.TestEveryBriefCapIsInTheSchemaTheModelReads`, `internal/assistant.TestTheBriefPromptStatesEveryCap`, `internal/assistant.TestABriefIsReReadWholeByTheNextTurn`, `internal/assistantdomain.TestACardInAnEarlierShapeIsReadAsANotice`, `cmd/crewlet.TestTheShippedAssistantEvalSuitePasses`
- **Measured:** a dogfood company's Inbox brief of one decision (prompt `assistant.brief.v2`), read back from its transcript: about 800 characters drawn, a head that said "2 need you" over one thing that needed the reader and one fact tagged `neutral`, a recommendation that repeated the first row and its button, and three follow-ups. The v2 caps allowed about 1,500 characters (a 200 character summary, three rows of 120 plus 120, a recommendation of 120, three follow-ups of 160). The v3 caps allow about 650: an about line of 90, three rows of 80 plus 60, a context line of 100, two follow-ups of 80.
- **Cost-when-tried:** v2 asked for concision in words ("numbers and names, no filler") and left the shape open to say everything twice. The model followed the words and filled the shape: a summary of counts, a row for a fact to know, and the first row again as the recommendation. The person asked for "what's up in the page, do I need to do anything about it", and the card made them read every row to find out.
- **Tag-status:** unreleased

## The decision

**A brief answers two questions: what this is about, and do I have to do
anything.** The first is the model's one `about` line. The second is never
written: it is the number of rows the engine kept, `wire.BriefCard.Verdict`
("Nothing needs you", "1 thing needs you", "2 things need you"), drawn once,
in the card's head, by the design system's `BriefCard` from the same rows. No
field of `answer_brief`, the wire card or the dashboard contract can carry a
verdict, so none can disagree with the rows beside it. A row the engine drops
(a record the turn never read, or one the person may not open) is a row the
verdict does not count.

**Every row is something the person has to do.** A need is an instruction
that starts with a verb and names its record, with one fact saying why now
and the one press that deals with it (open the record, or ask a question).
Its tone is `warning`, or `danger` only where the record's own read carries
the fact (an agent stopped or whose last turn failed, a task marked urgent).
There is no quiet tone: a fact worth knowing is the brief's optional
`context` line, which the schema asks for only when it changes what to do.
The rows are ranked and the first is the recommendation, so there is no
separate recommendation to repeat it.

**Concision is the shape, not a request.** Every line has a cap in characters
in the tool's schema, the prompt and the wire's validator, from one set of
constants, and every list a `maxItems` (three needs, two follow-ups).

**A stored card in a shape today's contract refuses reads as a notice.** Every
card is validated before it is recorded and never rewritten, so a card of a
kind this build draws that fails today's `wire.Card.Validate` was written to
an earlier contract of an unreleased surface. `assistantdomain`'s one card
reader serves it as `EarlierShapeNotice` (the transcript, the card read and
the model's history alike), and asking again writes it in today's shape. A
kind this build does not know is a newer peer's and passes through, as the
dashboard already draws one it cannot.

## Why the obvious alternative is wrong

**A `verdict` field the model writes** is the shape v2 already had in effect:
the head counted rows and the model chose what a row was. A written verdict
can say "nothing urgent" over a stopped agent, and a model asked to be brief
writes one in the about line as well. Derived from the rows, the verdict is
right whenever the rows are, and the rows are the part the engine checks.

**A `neutral` row for context** is how one need became "2 need you". A list
headed by a count has to hold only what it counts.

**Migrating stored v1 and v2 briefs** would mean a reader for a shape no
release ever shipped, which this repository does not keep. Tolerating them
field by field draws a card with an empty first line. The notice is honest
about what happened and costs the reader one press.

## What this does not decide

What a brief reads, how its rows are resolved for the person, its read and
answer rounds, its token cap and its one rewrite: those are
`internal/assistant`'s brief turn (and ADR-0059 for naming every fault at
once). How a brief is asked for (the drawer's row, its words, and where else
a brief might be offered) is the dashboard's.
