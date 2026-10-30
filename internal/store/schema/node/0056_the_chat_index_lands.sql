-- The company's chat gets its own keyword index, beside the knowledge base's
-- rather than inside it.
--
-- WHO HAS TO AGREE ON IT: this node alone. The index is derived from replicated
-- rows by a local walk, so a peer's copy buys nothing a rebuild does not, and
-- two nodes are legitimately at different points of that walk.
--
-- # Why chat is not admitted to kb_docs
--
-- The engine already has a BM25 index and the obvious move is to put messages
-- in it. Three things say otherwise, and the first is already written down in
-- this tree: the knowledge indexer deliberately does NOT index a work item's
-- comment thread, because "a thread is a conversation about the item rather
-- than a statement of it, indexing it would make one busy item outrank every
-- concise one on any word said in passing" (internal/search's TaskSource). A
-- chat message is that shape at ten times the volume.
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
-- retention prune, which the indexer follows room by room from the gap between
-- the oldest document it holds for a room and the oldest message the room's
-- transcript still has, and the compliance erase, which the indexer follows
-- through `chat_deletions`. Both are cheap because both are bounded; a
-- corpus-wide orphan lap would not be.
--
-- The prune follower keeps NO RECORD of how far it has followed each room. It
-- decides from that gap, which needs no record and so cannot drift from one: a
-- table of followed cutoffs would be a second piece of state to keep in step
-- with what the follower derives anyway.
--
-- # The statistics are the searched rooms', never the company's
--
-- ONE document count and ONE token count for the whole corpus, with a query's
-- term weights counted from each term's documents in every room, would make a
-- score a function of how often a word is said in rooms the asker cannot read:
-- a private room or a direct conversation saying a word more often would lower
-- that word's weight in every hit the asker CAN see, so the scores and the
-- order of a search over visible rooms would move with the contents of
-- invisible ones. The statistics are therefore kept PER ROOM —
-- `chat_index_rooms` (each room's documents and tokens) and `chat_index_terms`
-- (how many of each room's documents carry each term) — and a search sums them
-- over exactly the rooms it searches, every one of which its asker may read.
--
-- THE STATISTICS ARE MAINTAINED RATHER THAN COUNTED. BM25 needs the corpus
-- size, its average document length and each query term's document count on
-- EVERY query, and the knowledge index computes the first two with a
-- `SELECT COUNT(*), AVG(length)` over its whole doc table per search. At a few
-- thousand documents that is invisible; at several million it is a full scan on
-- the path of every question anybody asks. A term's document count read off the
-- postings is a range count the length of the term's list, and this analyzer
-- keeps its stop words: at the declared census a year is 7.3 million messages,
-- a word in a third of them is two and a half million postings, and Turso
-- counted a posting in about 65 ns (measured over 20 000 messages) — some
-- 160 ms per query term, on every search that typed "the". The indexer already
-- touches every row it changes, so it maintains the counts as it goes, and kept
-- per room a term's count is one row per room it was ever said in.
--
-- # A document names its author, and keeps no copy of what it says
--
-- A search narrowed to one speaker needs something to narrow on, and the author
-- lives on the replicated row, which no statement here may join. So it is
-- copied here once, when the message is indexed, because a message's author
-- never changes.
--
-- The body is NOT copied here at all: the message row is in the replicated
-- estate and a hit hydrates from it — which it has to read anyway, to skip a
-- message deleted since it was indexed — and the hit's snippet is cut from that
-- row's body. Even an excerpt would be, for a corpus of mostly one-line
-- messages, the whole transcript a second time, doubling the largest thing on
-- this disk. Nor does a document record the position or the instant it was
-- indexed at: the walk is forward over the transcript's own version, so it
-- never asks a document which position it was indexed at.
--
-- # A rebuild is a mark, and the drop goes a batch at a time
--
-- An estate replaced under the index (an adoption) is a change its forward walk
-- cannot see, so the index is dropped and walked again. Emptied in one
-- transaction that drop holds this node's only writer for as long as the index
-- is large — some 3.5 s at 80 000 messages on Turso, minutes at a year of chat
-- — so `chat_index_state.rebuilding` marks it instead, and each sweep removes
-- one batch until nothing is left.

-- chat_docs — one row per indexed message.
CREATE TABLE chat_docs (
    -- The message id, which is already unique and is what a hit hydrates by.
    -- No synthetic key: the knowledge index needs one because its documents
    -- come from several sources, and this corpus has one.
    id           TEXT    NOT NULL PRIMARY KEY,
    channel_id   TEXT    NOT NULL,
    -- The speaker's handle, which a search narrowed to one speaker filters on
    -- through the join the posting scan already makes.
    author       TEXT    NOT NULL,
    -- The bucket, `search.ChatShardOf` — the rule the replicated row's own
    -- bucket is stamped by, so a fan-out divides this corpus exactly as it
    -- divides the transcript.
    search_shard INTEGER NOT NULL,
    -- Total token count, the |D| in BM25's length normalisation.
    length       INTEGER NOT NULL,
    -- The message's `created_at` — the BROKER's stored instant — which the
    -- prune follower compares with the oldest message its room still has, and
    -- which its range delete reaches these rows by on an index rather than
    -- through a join into the other estate, which no statement here may cross.
    created_at   INTEGER NOT NULL
);

-- The prune follower's oldest-document read and its range delete, and the only
-- index on this table besides the key.
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

-- One document's terms: what a re-index or a removal subtracts from the term
-- counts, and what the cascade above deletes by.
CREATE INDEX chat_postings_doc_idx ON chat_postings (doc_id);

-- chat_index_rooms — one room's share of the corpus: how many of its messages
-- are indexed and how many tokens they hold. A search sums the rows of the
-- rooms it searches; the average length is the quotient, kept as two integers
-- because a running average cannot be adjusted by a delete. A room with no
-- documents left has no row, so the prune follower's rotation over this table
-- visits only rooms that hold something.
CREATE TABLE chat_index_rooms (
    channel_id TEXT    NOT NULL PRIMARY KEY,
    docs       INTEGER NOT NULL,
    tokens     INTEGER NOT NULL
);

-- chat_index_terms — how many of one room's documents carry one term, the
-- document frequency a search sums over the rooms it searches. Keyed (term,
-- channel_id) so one term's rooms are contiguous and a search seeks them. A
-- count that reaches zero is deleted, so the table holds the vocabulary rooms
-- still use rather than every token anybody ever typed.
CREATE TABLE chat_index_terms (
    term       TEXT    NOT NULL,
    channel_id TEXT    NOT NULL,
    docs       INTEGER NOT NULL,
    PRIMARY KEY (term, channel_id)
);

-- chat_index_state — how far this node's two forward walks have reached, and
-- whether a rebuild is dropping the index.
--
-- ONE ROW, and the single-row shape is enforced by a constant primary key
-- rather than by convention: a second row would be a second answer to "how far
-- has this node indexed", and the walk would resume from whichever the planner
-- returned first.
CREATE TABLE chat_index_state (
    id             INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    -- The packed log position this node has indexed the transcript through. A
    -- forward walk resumes here, so a restart costs nothing.
    through_rev    INTEGER NOT NULL,
    -- The packed position of the last compliance erase whose documents this
    -- index has removed. ITS OWN rather than `through_rev`: the two walks read
    -- two tables whose positions interleave, so one watermark advanced by
    -- either would step the other past rows it had not reached.
    erased_through INTEGER NOT NULL,
    -- Whether a rebuild is dropping the index: 1 from the request until the
    -- last document is gone, during which neither walk runs. DURABLE because
    -- the drop is the size of the index and goes a batch at a time, so a
    -- process that stopped halfway finishes it on its next start rather than
    -- resuming a walk from these watermarks over an index it half emptied.
    rebuilding     INTEGER NOT NULL CHECK (rebuilding IN (0, 1)),
    updated_at     INTEGER NOT NULL
);

INSERT INTO chat_index_state (id, through_rev, erased_through, rebuilding, updated_at)
VALUES (1, 0, 0, 0, 0);
