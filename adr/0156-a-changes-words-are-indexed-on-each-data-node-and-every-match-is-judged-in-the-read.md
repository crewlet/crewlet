# ADR-0156: A change's words are indexed on each data node, and every match is judged in the read that lists it

- **Status:** accepted
- **Authority:** `internal/search`
- **Enforced-by:**
  - `internal/gitlet.TestAListingNarrowedByWordsIsExactAsOfItsSnapshot`
  - `internal/gitlet.TestAWordsListingNeverAnswersAChangeItsAskerMayNotRead`
  - `internal/search.TestTheChangeIndexHoldsEveryChangesWords`
  - `internal/search.TestAChangeIndexNeverWalksPastARetainedRecord`
  - `internal/search.TestEveryChangeIndexWriteRecordsWhatItWasDecidedFrom`
  - `internal/search.TestEveryTermOfTheLargestChangeIsKept`
  - `internal/engine.TestAReplacedEstateRebuildsTheChangeIndex`, and for the amendment below `internal/gitlet.TestAWordsListingSaysHowFarBackItsIndexReaches`
  - `internal/search.TestTheFirstBuildIndexesTheNewestChangesFirst`
  - `internal/search.TestTheFirstBuildBeginsBelowARetainedRecord`
  - `internal/search.TestADocumentIsKeyedByWhenItsChangeWasMinted`
  - `internal/search.TestARebuildKeepsTheChangeIndexAndWritesItAgainNewestFirst`
  - `internal/search.TestEachKindOfWriteIsHeldByItsOwnTimes`
- **Measured:** the index takes about 2.0 KB a change of short text, 8.1 KB a change of 150 distinct words and 46 KB at a 32 KiB description, with its documents numbered a year in (`BenchmarkTheChangeIndexBuild`), so about 120 GB a year on every data node at 40 000 changes a day of the middle kind, where a term list kept beside each document took 13.3 KB a change and 195 GB a year; its first build walks 0.7 ms a change of short text and 17 ms at a 32 KiB description, and 5.8 ms a change of 150 distinct words, 110 of them rare, at 10 000 indexed and 12.3 ms at 30 000 (about 15.5 ms a change between the two), a cost that grows with the index, so a year of such changes takes at least two and a half days; a search at its scan bound reads in 7 ms on one word and 32 ms on eight words and three scopes (`BenchmarkAChangeSearchAtItsScanBound`); a page at its candidate bound reads in about 102 ms where the index is trusted and 121 ms where every match's description is read (`BenchmarkAWordsPageAtItsCandidateBound`); a push record moving 64 changes' heads applies in 43 to 48 ms with `code_changes_position_idx` against 43 to 51 ms without it (`BenchmarkApplyAPushRecord`'s `group+changes`). For the amendment below, built newest first from ids minted at 40 000 a day: about 2.6 KB a change of short text, 9.9 KB a change of 150 distinct words at 10 000 indexed and 10.4 KB at 30 000, and 55 KB at a 32 KiB description, so about 152 GB a year at 40 000 changes a day of the middle kind; the first build walks 6.9 ms a change of the middle kind at 10 000 indexed and 18.9 ms averaged over 30 000 (24.9 ms a change between the two), so the newest day is searchable after about 14 minutes, the week after 1.9 hours, the month after 8.2 hours and the year after 101 hours, each a lower bound; a search at its scan bound reads in 7 ms on one word and 29 ms on eight words and three scopes, and a page at its candidate bound in about 116 ms trusted and 141 ms judged. The write hold's smaller batches cost both: the same 10 000 changes take 10.9 KB and 14.7 ms a change at a 64 KiB batch and 11.1 KB and 19.7 ms at 32 KiB, which the hold reaches as writes slow with the index, so at a year's scale the index likely passes 10.5 KB a change. A rebuild over a copy that matches the documents kept walks 0.2 ms a change (`BenchmarkTheChangeIndexRebuild`).
- **Cost-when-tried:** the chat index once remembered its readiness as a flag set the first time its walk caught up, and answered every later search as caught up through any backlog; and its walk once kept no record of the copy it was built from, so a replaced copy's documents stayed searchable (node 0057). Both are designed out here from the start.
- **Tag-status:** unreleased

## The decision

A listing of Gitlet's changes narrowed by its words (`ReadQuery.Text`) keeps the changes in whose
title or description **every word appears**, in the listing's own order. Nothing is ranked.

- **Each data node keeps its own index of those words** in its own file (`change_docs`,
  `change_postings`, `change_index_state`), kept by `search.ChangeIndexer`: a forward walk over
  `code_changes.position` from a durable watermark that never passes the code log's applied
  prefix, a rotation that drops a purged repository's documents, and a rebuild when the estate
  under it is replaced (see the amendment). A document keeps its change's repository, owner and
  state, so a search reads the narrowest of its scope and its words first and cuts within it.
- **A document's postings are one range of the postings table's own key**: a number this node
  gives the document, shifted 16 bits, plus each word's place. An edit or a purge removes them by
  that range, without the words they held, and every word of the largest title and description
  has a place. Newest, for the bounds below, is the order the index took the changes in (the
  amendment keys a document by when its change was minted instead).
- **Every index write records what it was decided from**: the code log's settled position in the
  estate snapshot it read.
- **The read judges every match in its own snapshot.** It trusts a match at or below the watermark
  only where its snapshot has applied every record up to that recorded position, so the index read
  that very row (the amendment adds the first build's reach); it judges every other match, and
  every change moved past the watermark (at most a hundred), from the row's title and description.
- **What it could not do it says**: `building` where the index is behind the read's rows,
  rebuilding or built from another copy, and `truncated` where more changes may hold the words than
  one search reads (the newest 5 000 of what it reads first, the newest 1 000 matches). It never
  lists a change by words its row does not hold, nor one its asker may not read.
- **Beside a reviewer, a need, a work item or a readiness bucket**, those walks lead as they do,
  and the words are judged on each change they read.

## Why the obvious alternatives are wrong

**Ranking, as the knowledge base and chat do.** A listing keeps the order its reader chose, so no
score is ever read, and the per-term statistics a ranking needs would be moved on every write for
nobody.

**Testing the words on each row a listing's walk reads, with no index.** Exact, and a rare word
over a repository's merged history is hundreds of empty pages, each read with its descriptions.

**Admitting changes to the knowledge base's index.** A change is its repository's content, held to
its repository's grant and visibility and listed by the listing's filters; and a corpus in the
lexical half and not the semantic one is the asymmetry ADR-0071 refuses.

**Trusting the index's answer, as a chat search trusts its own.** Every push to a change's branch
moves its row, so in a busy company something has always moved past the watermark: a readiness
that asks whether anything has would say "building" most of the time, and an answer from the index
alone misses a change opened a second ago.

**Walking to the log's checkpoint.** During a rolling upgrade a node retains a record its build
cannot read, and every later record about the same repository, while the checkpoint moves past
them; its next build applies them at their own positions. A watermark already past those positions
never reads the rows they write, so a change they opened or retitled would never be found by its
words.

**Keeping each document's words beside it, or reading back the words a change held.** The
estate keeps only a change's current title and description, and the code log keeps a record only
until its trim (a day at least, seven by default), sealed for the applier that decodes it, while a
change is edited at any age and a purge comes a deletion's retention after the words were written.
So removing postings by the old words needs a copy of them for every document, about 2 KB of the
13.3 KB a change of 150 words took; numbering the document into the postings' key needs none.

**Replicating the index.** Every data node would write every change's postings inside the apply,
on the lane every domain waits behind, to agree on what any node rebuilds from rows it holds.

## What this does not decide

A relevance order, prefixes and phrases (a later decision, which would have to pay for the
statistics above); a seat tool that takes words, which would narrow the candidates by its grant
first; the words of a change's conversation, which are not indexed.

## Amendment: the first build walks newest first, and says how far back it has reached

The first build walks the changes **newest first by id**, over the rows at
or below the code log's applied prefix where it began, beside the forward walk,
which starts there and keeps every later move indexed. Built oldest first, a
year at 40 000 changes a day left the last months unsearchable for days
(measured at least 63 hours to the end); newest first, the last day is
searchable within minutes and older changes fill in behind while each page says
how far back it reaches. Beginning at the applied prefix rather than the
checkpoint keeps the first rule: a record this node retains is applied at its
own position later, above where the build began, so the forward walk reads it.

While it runs the oldest changes are unreached for hours at a large company's
scale, so the index answers, beside its watermark, where that build began
(`BackPos`) and the least change id it has reached back to (`BackFloor`, empty
once it reached the oldest). The read
trusts a match only where a walk has reached its row: moved past where the
build began and no further than the watermark, or at or below it with an id at
or above the floor. Every page the index leads carries `indexed_back_to`, the
instant that floor's id was minted at, while the build walks back; a change
opened at or before it may be missing. It stands beside `building`, which keeps
its meaning. Their zero is an index built whole.

A document is now **keyed by the millisecond its change's id was minted in** and
five more bits of the id, no longer by a number this node gives it as its walk
takes the change: walking back would number the oldest changes newest, and each
node would keep its own order. So newest, for the bounds above, is when each
change's open was asked, the same on every node, and what a first build has
reached is always the newest stretch of that order. Two ids on one key take the
next free one, and an id with no plausible instant takes a key below every
natural one, so no id fails a batch. It costs about 2 KB a change over the
node-local number (8.1 KB) at full 256 KiB batches; the write hold below adds
more as batches shrink.

A node whose index was built under the earlier layout drops it (node migration
"A change is found newest first by when it was minted") and builds again,
newest first. A replaced estate no longer drops the index: one statement
begins the first build again over the documents it keeps, each walk rewrites
the documents it reaches by id where the text or scope moved, and a backward
batch removes the documents of ids in its stretch the copy does not hold.
Dropping first cost about 13 ms a document, some 53 hours at a year's scale
before the build could begin.

Each walk's batch, and a purge's drop step, halves after a write of its own over
two seconds and doubles after fast ones, since the node file's busy wait is
five; one budget each, so the forward walk's small writes never undo the hold
on the backward walk's. The hold costs both size and time (see Measured): the
30 000 arm's 10.4 KB and 18.9 ms a change already carry it, and smaller batches
cost more. So the whole first build, at least 101 hours for a year, takes
longer than oldest first did (63 hours); what newest first buys is the newest
day after about 14 minutes and the week after about 2 hours. The build's
throughput and the hold's cost are left to a later decision.
