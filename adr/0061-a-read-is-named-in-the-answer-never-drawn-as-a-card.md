# ADR-0061: A read is named in the answer, never drawn as a card

- **Status:** accepted
- **Authority:** `internal/assistant/wire`
- **Enforced-by:** `internal/assistant/catalog.TestEveryDefaultReadIsBounded`, `internal/assistant/catalog.TestAGenericListReadIsOnePage`, `internal/assistant/cards.TestARecordCarriesTheKeyProseWritesItBy`, `internal/assistant/cards.TestTheModelIsToldARecordsKey`, `internal/assistant/cards.TestAKeyedKindNeedsItsKeyAndNoOtherHasOne`, `internal/assistant/catalog.TestARecordIsKeyedTheWayTheAnswerNamesIt`, `internal/assistant/catalog.TestAReadTooLargeToShowIsNamedLinkedAndToldToTheModel`, `internal/assistant/catalog.TestACutReadTellsTheModelOnlyWhatTheEngineKnows`, `internal/assistant/catalog.TestEveryModelTextGoesThroughThePipeline`, `internal/assistant/catalog.TestAPageWithMoreIsNeverTheWhole`, `internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol`, and the dashboard's `mentions.test.tsx`, `reads.test.tsx` and `lib/markdown.test.tsx` (`prose`)
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

**The model is told the read was cut**, never handed a bare notice, and
**only in the engine's words**: the tool, the route pattern it read (the
catalogue's own, never the model's spelling), that none of it was shown, the
bound, what narrows it (the tool's own arguments, or for a generic read the
query parameters the API reference documents for the route) and not to answer
as though it read it. Not one byte of the cut answer reaches the model, not
even a field's name: a payload's keys are written by whoever wrote the
payload, so naming the fields a cut answer began with would hand the model
unvouched text past the fence. The fact goes through the same pipeline as every
result (`modelFact`, then `modelText`): fenced under the tool's trust where
the tool reads somebody's text, defused where it does not, and held to
`cards.ModelBytes`. `catalog.TestEveryModelTextGoesThroughThePipeline` holds
every `Result.Model` the catalogue builds to a pipeline producer, and only
`modelText` fences or defuses.

**A card that is a page says so itself.** A table carries `truncated` (rows
cut to fit the card) and `more` (rows past the page the read asked for), and
a record its `items_total`; the drawer says "first 20" on the chip and which
screen shows the rest in the peek. The notice beside each shortened card is
gone.

**No default read is cut, and no public route changes for it.** Measured on a
real company before deciding: every dedicated read stayed under the step's 64
KiB at its defaults (the largest, the token breakdown, 55 KB), and what crossed
it were generic `api_request` reads of routes whose defaults are heavy. The
routes' defaults stay their own: the dashboard and every other caller read them
as they always have. The bound is applied by the assistant's read layer
(`internal/assistant/catalog`), where its requests are built:

- `GET /agents/{id}` answers the seat's call history (fifty whole transcripts,
  2.1 MB for a busy seat) by default. Every read the assistant makes of it asks
  for the seat alone with `history=false`: `get_agent`, the create preview, and
  a generic `api_request` read, which adds it unless the model names `history`
  itself.
- A generic read of a list route the reference says pages by `limit` (the
  event log, the turns, the inbox) is asked for a page of 25 when the model
  names none, so it is answered whole with the route's cursor, and the model
  answers from that first page rather than from nothing.
- Every dedicated read declares its bound (a page, a window, one record, a
  route's own cap, or one row per configured thing) and any parameter it must
  send to stay inside it, and `catalog.TestEveryDefaultReadIsBounded` holds
  each request to it: a new read that declares none, a page that drops its
  limit, or a seat read that drops `history=false` fails the build. The inbox
  and the project list, which read whole, now ask for a page.

The oversize card and the model's `cut` answer stay as the net for what a
read the model narrows by hand still makes too large.
