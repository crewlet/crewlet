# ADR-0061: A read is named in the answer, never drawn as a card

- **Status:** accepted
- **Authority:** `internal/assistant/wire`
- **Enforced-by:** `internal/assistant/cards.TestARecordCarriesTheKeyProseWritesItBy`, `internal/assistant/cards.TestTheModelIsToldARecordsKey`, `internal/assistant/catalog.TestARecordIsKeyedTheWayTheAnswerNamesIt`, `internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol`, and the dashboard's `mentions.test.tsx`, `reads.test.tsx` and `lib/markdown.test.tsx` (`prose`)
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

## What this does not decide

How the engine's notices about a read (a result too large to show, a table's
first page) are drawn: they stay notices, as cards, until a notice can say
which read it is about.
