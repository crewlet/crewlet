-- The company's chat gets its own keyword index, beside the knowledge base's
-- rather than inside it.
--
-- # Why chat is not admitted to kb_docs
--
-- The engine already has a BM25 index and the obvious move is to put messages
-- in it. Three things say otherwise, and the first is already written down in
-- this tree: the knowledge indexer deliberately does NOT index a work item's
-- comment thread, because "a thread is a conversation ABOUT the item rather
-- than a statement of it, and indexing it would make one busy item outrank
-- every concise one on any word said in passing". A chat message is that
-- shape at ten times the volume.
--
-- The second is a recorded failure: a corpus admitted to ONE HALF of search
-- and not the other. The lexical half held pages while the embedding duty
-- carried work items, so a company paid for a vector on every task and the
-- keyword half held none of them. Chat is in neither half here — it is its own
-- corpus with its own index and no vectors at all — which is a decision the
-- reader can see rather than an asymmetry they discover.
--
-- The third is arithmetic. Embedding a year of chat at the declared census
-- needs several times the whole supported vector corpus, against a duty whose
-- entire budget is about a thousand sources a minute shared across every
-- corpus the company has. Keyword search over chat is affordable; semantic
-- search over it is not, and pretending otherwise would starve the knowledge
-- base's own embeddings to do it.
--
-- # Why the maintenance strategy is different, and has to be
--
-- The knowledge indexer LAPS its sources by id, a thousand at a time, with the
-- cursor in memory and "ready" meaning the first lap wrapped. That is right
-- for a corpus of a few thousand mutable documents and untenable for one of
-- several million mostly-immutable ones: a lap over a year of chat is
-- thousands of index-only scans, and a restart re-walks from the beginning.
--
-- So this index walks FORWARD ONLY, over `chat_messages.version` — the packed
-- log position of the last record that changed a row. An edit and a tombstone
-- both move it, so the forward walk re-visits exactly the rows that changed
-- and never re-reads the rest. The watermark is durable here rather than in
-- memory, so a restart resumes instead of restarting.
--
-- What that costs is the orphan pass: a forward walk cannot see a row that
-- VANISHED. Two things delete chat rows and each is followed explicitly — the
-- retention prune, whose range this index follows with the same predicate, and
-- the compliance erase, which the indexer follows through `chat_deletions`.
-- Both are cheap because both are bounded; a corpus-wide orphan lap would not
-- be.

-- chat_docs — one row per indexed message.
--
-- The body is NOT copied here beyond an excerpt: the message row is in the
-- replicated estate and a hit hydrates from it, so a second copy of the whole
-- transcript in the node estate would double the largest thing on this disk to
-- save one join.
CREATE TABLE chat_docs (
    -- The message id, which is already unique and already what a hit
    -- hydrates by. No synthetic key: the knowledge index needs one because
    -- its documents come from several sources, and this corpus has one.
    id           TEXT    NOT NULL PRIMARY KEY,
    channel_id   TEXT    NOT NULL,
    -- The bucket, computed by the INDEXER from the message's own identity —
    -- the same function the knowledge index and the vector tables use, so a
    -- fan-out divides this corpus exactly as it divides theirs.
    search_shard INTEGER NOT NULL,
    excerpt      TEXT    NOT NULL DEFAULT '',
    -- Total token count, the |D| in BM25's length normalisation.
    length       INTEGER NOT NULL DEFAULT 0,
    -- The message's `created_at` — the BROKER's stored instant — so the
    -- prune's range delete reaches these rows on an index rather than through
    -- a join into the other estate, which no statement here may cross.
    created_at   INTEGER NOT NULL,
    -- The packed position this row was indexed AT, so the walk can tell a row
    -- it has already seen from one an edit moved past the watermark.
    indexed_rev  INTEGER NOT NULL DEFAULT 0,
    indexed_at   INTEGER NOT NULL
);

-- The prune's range delete, and the only index on this table besides the key.
--
-- NO INDEX ON search_shard, which is the finding migration 0027 recorded for
-- the knowledge index and 0004 measured in the other estate at 1 min 44 s
-- against 50 ms: a bucket range is SEEKABLE, so an index over it makes the
-- planner drive on a sixty-fourth of the corpus rather than on the term. The
-- shard is only ever a filter on a row the join already reached.
CREATE INDEX chat_docs_prune_idx ON chat_docs (channel_id, created_at);

-- chat_postings — the inverted list.
--
-- Keyed (term, doc_id) so a term's postings are contiguous, which is what lets
-- a query stream one term at a time rather than materialise the corpus. The
-- cascade is real here, unlike in the replicated estate: this whole estate is
-- DERIVED and owned by one process, so a delete that takes its postings with
-- it is the indexer's own effect rather than a delete nobody committed.
CREATE TABLE chat_postings (
    term   TEXT    NOT NULL,
    doc_id TEXT    NOT NULL REFERENCES chat_docs(id) ON DELETE CASCADE,
    freq   INTEGER NOT NULL,
    PRIMARY KEY (term, doc_id)
);

CREATE INDEX chat_postings_doc_idx ON chat_postings (doc_id);

-- chat_index_state — the durable watermark, and the corpus statistics a score
-- needs.
--
-- ONE ROW, and the single-row shape is enforced by a constant primary key
-- rather than by convention: a second row here would be a second answer to
-- "how far has this node indexed", and the walk would resume from whichever
-- the planner returned first.
--
-- THE STATISTICS ARE MAINTAINED RATHER THAN COUNTED. BM25 needs the corpus
-- size and its average document length on EVERY query, and the knowledge index
-- computes both with a `SELECT COUNT(*), AVG(length)` over its whole doc table
-- per search. At a few thousand documents that is invisible; at several
-- million it is a full scan on the path of every question anybody asks. The
-- indexer already touches every row it changes, so it maintains the two
-- numbers as it goes and a query reads one row.
CREATE TABLE chat_index_state (
    id           INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    -- The packed log position this node has indexed through. A forward walk
    -- resumes here, so a restart costs nothing.
    through_rev  INTEGER NOT NULL DEFAULT 0,
    -- The corpus, maintained by the indexer: how many documents and how many
    -- tokens across them. The average is the quotient, kept as two integers
    -- because a running average cannot be adjusted by a delete.
    doc_count    INTEGER NOT NULL DEFAULT 0,
    token_count  INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
);

INSERT INTO chat_index_state (id, through_rev, doc_count, token_count, updated_at)
VALUES (1, 0, 0, 0, 0);

-- chat_index_pruned — how far the index has followed each channel's prune.
--
-- The prune deletes replicated rows the indexer can no longer see, so the
-- forward walk would leave their documents behind for ever. This is the
-- indexer's own record of which channels it has caught up with, and its cost
-- is one row per channel rather than a corpus-wide orphan pass.
CREATE TABLE chat_index_pruned (
    channel_id     TEXT    NOT NULL PRIMARY KEY,
    -- The cutoff this node has already deleted its documents below.
    followed_at    INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
