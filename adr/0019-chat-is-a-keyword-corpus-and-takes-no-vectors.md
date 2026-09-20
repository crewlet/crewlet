# ADR-0019 — Chat is a keyword corpus of its own and takes no vectors

- **Status:** accepted
- **Authority:** `internal/search`
- **Enforced-by:** `internal/search.TestTheVectorCorpusIsPagesAndTasksOnly`
- **Measured:** 20,000 messages a day declared census — 7.3 M documents a year — against an embedding duty budget of about a thousand sources a minute shared by every corpus a company has, and a supported vector corpus of a few hundred thousand documents
- **Cost-when-tried:** the asymmetry this refuses already shipped once in the other direction: the lexical half held pages while the embedding duty carried work items, so a company paid for a vector on every task and the keyword half held none of them
- **Tag-status:** unreleased

## The decision

The company's chat is searched, and it is searched **lexically only**. It has
its own inverted list in the node estate (`chat_docs`, `chat_postings`,
maintained by `search.ChatIndexer` over `textindex.ChatProfile`), it is not a
member of `search.Sources`, and the fleet-singleton embedding duty never walks
it. There is no `hybrid` mode over chat, no vector column on a message, and no
per-message embedding anywhere.

It is also **its own corpus** rather than rows admitted to the knowledge
base's index. A message and a page are ranked against different statistics,
by a different length normalisation, over different visibility rules — a
private room's contents are not a fact about the company the way a page is —
and one query never fuses them.

## Why the obvious alternative is wrong

The obvious alternative is that chat joins the corpus every other document is
in: one index, one `search_knowledge`, both halves, and the same `hybrid`
default. It reads as consistency, and it is wrong three times.

**It is unaffordable, by an order of magnitude.** A year of chat at the
declared census is several times the entire supported vector corpus, against a
duty whose whole budget is about a thousand sources a minute shared by every
corpus the company has. The knowledge base's own embeddings are what would be
starved to pay for it, and nothing anywhere would report that trade.

**It ranks worse, not better.** This tree already decided the same question
against a work item's comment thread and wrote the reason down at the
knowledge indexer: a thread is a conversation *about* the item rather than a
statement of it, so indexing it makes one busy item outrank every concise one
on any word said in passing. Chat is that shape at ten times the volume, and a
BM25 corpus mixing one-line acknowledgements with written pages hands every
query to whoever typed the shortest reply — which is why the two corpora carry
different length normalisations (`textindex.ProseProfile` at B 0.75,
`textindex.ChatProfile` at B 0.30) and cannot share one.

**And the half-admitted shape is the one that already failed here.** A corpus
in one half of search and not the other is not hypothetical: the lexical half
held pages while the embedding duty carried work items, so a company paid for
a vector on every task and the keyword half had none of them. Chat being in
neither half is a decision a reader can see; chat being in one of them is an
asymmetry a reader discovers.

## What this does not decide

It does not say chat is unsearchable, or that its search is a lesser one: the
keyword half is the half a person uses to find something they remember the
words of, which is what chat search is for.

It does not decide the retention of the chat index, which follows the message
horizon (`chat.native.message_retention_days`) because a document whose row is
gone cannot be hydrated.

And it does not generalise to "a high-volume corpus takes no vectors". The
list in `search.Sources` is a budget, not a principle — what it forbids is
adding to it without paying the arithmetic above.
