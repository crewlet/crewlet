# ADR-0156: A change's words are indexed on each data node, and every match is judged in the read that lists it

- **Status:** accepted
- **Authority:** `internal/search`
- **Enforced-by:** `internal/gitlet.TestAListingNarrowedByWordsIsExactAsOfItsSnapshot`, `internal/gitlet.TestAWordsListingNeverAnswersAChangeItsAskerMayNotRead`, `internal/search.TestTheChangeIndexHoldsEveryChangesWords`, `internal/search.TestAChangeIndexNeverWalksPastARetainedRecord`, `internal/search.TestEveryChangeIndexWriteRecordsWhatItWasDecidedFrom`, `internal/search.TestEveryTermOfTheLargestChangeIsKept`, `internal/engine.TestAReplacedEstateRebuildsTheChangeIndex`
- **Measured:** the index takes about 2.0 KB a change of short text, 8.1 KB a change of 150 distinct words and 46 KB at a 32 KiB description, with its documents numbered a year in (`BenchmarkTheChangeIndexBuild`), so about 120 GB a year on every data node at 40 000 changes a day of the middle kind, where a term list kept beside each document took 13.3 KB a change and 195 GB a year; its first build walks 0.7 ms a change of short text and 17 ms at a 32 KiB description, and 5.8 ms a change of 150 distinct words, 110 of them rare, at 10 000 indexed and 12.3 ms at 30 000 (about 15.5 ms a change between the two), a cost that grows with the index, so a year of such changes takes at least two and a half days; a search at its scan bound reads in 7 ms on one word and 32 ms on eight words and three scopes (`BenchmarkAChangeSearchAtItsScanBound`); a page at its candidate bound reads in about 102 ms where the index is trusted and 121 ms where every match's description is read (`BenchmarkAWordsPageAtItsCandidateBound`); a push record moving 64 changes' heads applies in 43 to 48 ms with `code_changes_position_idx` against 43 to 51 ms without it (`BenchmarkApplyAPushRecord`'s `group+changes`).
- **Cost-when-tried:** the chat index once remembered its readiness as a flag set the first time its walk caught up, and answered every later search as caught up through any backlog; and its walk once kept no record of the copy it was built from, so a replaced copy's documents stayed searchable (node 0057). Both are designed out here from the start.
- **Tag-status:** unreleased

## The decision

A listing of Gitlet's changes narrowed by its words (`ReadQuery.Text`) keeps the changes in whose
title or description **every word appears**, in the listing's own order. Nothing is ranked.

- **Each data node keeps its own index of those words** in its own file (`change_docs`,
  `change_postings`, `change_index_state`), kept by `search.ChangeIndexer`: a forward walk over
  `code_changes.position` from a durable watermark that never passes the code log's applied
  prefix, a rotation that drops a purged repository's documents, and a rebuild when the estate
  under it is replaced. A document keeps its change's repository, owner and state, so a search
  reads the narrowest of its scope and its words first and cuts within it.
- **A document's postings are one range of the postings table's own key**: a number this node
  gives the document, shifted 16 bits, plus each word's place. An edit or a purge removes them by
  that range, without the words they held, and every word of the largest title and description
  has a place. Newest, for the bounds below, is the order the index took the changes in.
- **Every index write records what it was decided from**: the code log's settled position in the
  estate snapshot it read.
- **The read judges every match in its own snapshot.** It trusts a match at or below the watermark
  only where its snapshot has applied every record up to that recorded position, so the index read
  that very row; it judges every other match, and every change moved past the watermark (at most a
  hundred), from the row's title and description.
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
