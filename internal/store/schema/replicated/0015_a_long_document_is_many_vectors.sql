-- A document is embedded in CHUNKS, so `kb_vectors` holds several rows per
-- source rather than one.
--
-- # What was wrong with one
--
-- The embedding duty cut every source at its first 8 KiB and embedded that.
-- On a page longer than that — which is most runbooks, most design notes and
-- every meeting record a company keeps — the tail was never sent to the
-- provider at all, so it was not in the vector, so semantic search could not
-- find it. A reader asking "how do we handle rate limits" against a handbook
-- whose rate-limit section is on page four got nothing, from a corpus that
-- holds the answer, with no result and no note to say part of the document was
-- never indexed.
--
-- The cut was defended on the grounds that refusing the document outright
-- would be worse, which is true and was the wrong comparison: the third option
-- is to embed the whole thing in windows and keep a vector for each. That is
-- what this migration is for.
--
-- # The key
--
-- `chunk` is the window's ordinal, and it joins the primary key. Chunk 0 is
-- the one every embedded document has, which is what every reader counting
-- DOCUMENTS filters on — the anti-joins that select stale sources, the
-- coverage fraction, and the corpus statistics an operator reads. A reader
-- counting VECTORS leaves it out.
--
-- # Why a rebuild rather than an ALTER
--
-- SQLite cannot add a column to a primary key in place, and the alternative —
-- a second table keyed the new way beside the old — is two tables that must
-- agree about the same corpus for ever.
--
-- Nothing is lost by dropping the rows. `kb_vectors` is DERIVED: the duty
-- refills it from the sources on its own sweep, and `kb_vectors_bin` is one
-- `vector1bit()` away from `kb_vectors` with no network at all. 0006 made the
-- same judgement for the same reason. The cost of the refill is the provider
-- bill for one pass over the corpus, which a company that has been running
-- this build has already paid once — and would pay again anyway, because
-- every document past the old cut is about to be embedded whole for the first
-- time.

DROP INDEX kb_vectors_model_idx;
DROP TABLE kb_vectors_bin;
DROP TABLE kb_vectors;

CREATE TABLE kb_vectors (
    source       TEXT    NOT NULL,
    source_id    TEXT    NOT NULL,
    -- The window's ordinal within its document, 0 upward and contiguous. A
    -- document that shrank leaves no hole: the duty publishes a forget for
    -- every ordinal it no longer has, so the retained record on the compacted
    -- stream becomes a removal rather than a stale insert a replay would
    -- resurrect.
    chunk        INTEGER NOT NULL DEFAULT 0,
    container    TEXT    NOT NULL DEFAULT '',
    -- The bucket, computed once by the applier from (source, source_id) — NOT
    -- from the chunk. Every window of one document shares its document's
    -- bucket, because the fan-out divides the corpus by document and a chunk
    -- in another node's range is a chunk its own document's scan never sees.
    search_shard INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    source_rev   INTEGER NOT NULL DEFAULT 0,
    text_sha     TEXT    NOT NULL DEFAULT '',
    embedding    BLOB,
    embedded_at  INTEGER NOT NULL,
    version      INTEGER NOT NULL,
    PRIMARY KEY (source, source_id, chunk)
);

-- The one index 0004 kept, and for its reason: it COVERS its only reader, the
-- `GROUP BY model, dim` that names every embedding space the corpus holds.
CREATE INDEX kb_vectors_model_idx ON kb_vectors (model, dim);

CREATE TABLE kb_vectors_bin (
    source       TEXT    NOT NULL,
    source_id    TEXT    NOT NULL,
    chunk        INTEGER NOT NULL DEFAULT 0,
    container    TEXT    NOT NULL DEFAULT '',
    -- The same bucket as its kb_vectors row, written in the same transaction
    -- by the same applier. Two rows that disagreed would put a document in one
    -- bucket for the candidate scan and another for the rerank, which is a
    -- document neither can find.
    search_shard INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    bits         BLOB    NOT NULL,
    PRIMARY KEY (source, source_id, chunk)
);
