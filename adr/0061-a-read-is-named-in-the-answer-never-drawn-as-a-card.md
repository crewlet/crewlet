# ADR-0061: A read is named in the answer, never drawn as a card

- **Status:** accepted
- **Authority:** `internal/assistant/wire`
- **Enforced-by:** `internal/assistant/cards.TestARecordCarriesTheKeyProseWritesItBy`, `internal/assistant/cards.TestTheModelIsToldARecordsKey`, `internal/assistant/cards.TestAKeyedKindNeedsItsKeyAndNoOtherHasOne`, `internal/assistant/catalog.TestARecordIsKeyedTheWayTheAnswerNamesIt`, `internal/assistant/catalog.TestAReadTooLargeToShowIsNamedLinkedAndToldToTheModel`, `internal/assistant/catalog.TestAPageWithMoreIsNeverTheWhole`, `internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol`, and the dashboard's `mentions.test.tsx`, `reads.test.tsx` and `lib/markdown.test.tsx` (`prose`)
- **Cost-when-tried:** the drawer drew a folded card for every read a turn made. An answer on the Inbox that read two tasks, a table and an agent, and then both tasks again, stood six cards of about 140px each between the reader and the answer in a 560px drawer, each saying its key twice (in its summary and as its link's label), and the same record twice. An answer on Company › Brand added a blue notice per read and table cards whose CSV, XLSX and link wrapped onto a second line or not depending on the length of the title.
- **Tag-status:** unreleased

## The decision

The rule spans the wire (`EntityCard.key`), the read catalog
(`internal/assistant/catalog`, which reads carry a key), the prompt
(`internal/assistant/prompt.go`, how the model names a record) and the
dashboard (`app/assistant/chunk/mentions.ts`, the matching rule, and
`reads.tsx`, the chips and their peeks); the wire is where the two sides meet,
so it is the record's authority.

**A table or a record a turn read is never drawn as a card.** The answer's
prose names it, and the first mention of each record the turn read is that
record's chip, which opens a peek holding everything its card held: the
facts, the body and the list under it, a table's rows, its downloads, and the
way to its page. A record the turn read and the answer never named is listed
on one "Also read" line under the answer. A proposal, a refusal, a notice, a
file, a brief and an open in app are the point of their turn and stay cards.

**The records are the turn's reads, never the model's words.** Only a record
a read built a card for can become a chip; a key the model writes that the
turn never read stays text, so nothing the model writes becomes a link, and
every chip's link is the engine's own address, checked by the dashboard as
every card's was.

**A record carries the key prose writes it by.** `EntityCard.key` is a task's
key or a project's, set from the record by its read's spec, never guessed from
`subtitle`. A seat carries none: the prompt has the model name a seat by its
name, never its handle, so a seat is found by its name. The model's digest of
the card carries the key, and the prompt tells it to name each record exactly
as its tools gave it, outside code.

**The matching rule** is stated once, in `mentions.ts`: a key exactly, at
token boundaries; a name only for the kinds whose title is a name people write
(an agent, a team, a project, a page, a space, an artifact, a schedule), whole
and in its own case; never a task's title, a table's or a record of no kind;
the earliest mention first and, at one place, the longer; only the first
mention of a record. It reads only the words a reader reads: the Markdown
renderer hands each run of prose to it (`RenderOptions.prose`), never a code
span, a code block or a link's label.

## Why the obvious alternatives are wrong

- **Matching on the subtitle.** A task's subtitle happens to be its key today,
  but a page's is its space and a seat's its handle. A rule that guessed would
  turn a space's name into a page's chip.
- **Matching the rendered DOM or React tree.** The renderer draws code through
  a component and an inert link as a span, so a walker that skips "code" and
  "a" elements by tag misses both. The renderer already knows which words are
  prose, as search highlighting does, and says so.
- **Keeping the cards, compacted.** Measured in the prototype at the drawer's
  width, compact cards still cost about 165px for four reads and stack a
  surface per record; chips cost none, and the owner chose them.

## A read too large to show, and a page of one

**A read past what one step reads is still a read.** Its answer is never
shown, but it is an oversize card (`OversizeCard`): named for what it read
(the table it would have been, or its step), with the screen that shows all
of it, built from the read's own arguments through the same link table every
card's Open uses, and what the tool takes that would narrow it. The drawer
lists it on the Also read line as "Matching tasks, ... (too large to show
here)", whose peek says so, links that screen and names what to narrow. It
replaced a notice that told a person inside the app to "open it in the app".

**The model is told the read was cut**, never handed a bare notice: what it
read, that none of it was shown, the bound, the fields the cut answer began
with and what narrows it, and not to answer as though it read it.

**A card that is a page says so itself.** A table carries `truncated` (rows
cut to fit the card) and `more` (rows past the page the read asked for), and
a record its `items_total`; the drawer says "first 20" on the chip and which
screen shows the rest in the peek. The notice beside each shortened card is
gone.

Measured on a real company before deciding: every dedicated read stays under
the step's 64 KiB at its defaults (the largest, the token breakdown, 55 KB).
What crossed it were generic `api_request` reads of routes whose defaults are
heavy: `GET /agents/{id}` without `history=false` answers 2.1 MB of whole
transcripts. The route's documentation already names the flag, and the model
is now told which fields the cut answer began with, so it can ask for less.
