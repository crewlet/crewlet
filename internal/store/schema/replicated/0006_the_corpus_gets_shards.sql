-- The vector tables gain a SHARD, so a candidate scan's cost is measurable per
-- bucket range rather than per corpus held.
--
-- # What a shard is here
--
-- A stable hash of a document's own identity into [search.SearchShards]
-- buckets, and deliberately unrelated to everything else this engine
-- partitions on: not a stream, not a project, not a container, not a log
-- position, not the node holding the row. See internal/search/shard.go for why
-- each of those independences is load-bearing — the short version is that a
-- source which MOVES must keep its bucket, or a query scanning the old one
-- misses it, and a search returning one fewer result looks exactly like a
-- corpus with one fewer document.
--
-- Nothing computes a routing plan. Every query still consults every bucket,
-- which is what a single node does today; what the column buys now is the
-- measurement a fan-out is decided from.
--
-- # AND NO INDEX ON IT, which is 0004's finding rather than an omission
--
-- 0004 deleted both of this domain's scope indexes because each made the query
-- it was written for slower — the stage-1 scan reads a NARROW table in full by
-- design, and an index over a table whose rows are all being read is a second
-- copy of the row order plus a random access per row. Two thousand times
-- slower, measured.
--
-- A shard predicate does not change that. It NARROWS the scan to a range of
-- buckets; reading a sixty-fourth of the rows through an index is still a
-- random access per row against a sequential read of a sixty-fourth of a
-- narrow table. `kb_vectors_model_idx` stays because it is the one index here
-- that COVERS its only reader, which is the case an index is actually for.
--
-- # Why the rows go rather than being backfilled
--
-- The shard is a hash of a Go function and no SQL statement can compute it, so
-- a migration cannot fill the column for rows already here. The alternatives
-- were a sentinel the hot scan had to branch on for ever, or this: the column
-- is NOT NULL with no default, the rows are removed, and what refills them is
-- what built them in the first place.
--
-- `kb_vectors_bin` is DERIVED — one INSERT … SELECT vector1bit(embedding) away
-- from kb_vectors with no network. `kb_vectors` is the one with a real cost,
-- and the embedding duty refills it on its own sweep. No release has ever
-- contained these tables, so there is no corpus anywhere to pay that bill; a
-- deployment that did exist would need a backfill PASS instead, and that would
-- be a migration task rather than a statement here.

DROP INDEX kb_vectors_model_idx;
DROP TABLE kb_vectors_bin;
DROP TABLE kb_vectors;

CREATE TABLE kb_vectors (
    source       TEXT    NOT NULL,
    source_id    TEXT    NOT NULL,
    container    TEXT    NOT NULL DEFAULT '',
    -- The bucket, computed once by the applier from (source, source_id). It is
    -- written rather than derived at read time because no SQL expression can
    -- reproduce the hash.
    search_shard INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    source_rev   INTEGER NOT NULL DEFAULT 0,
    text_sha     TEXT    NOT NULL DEFAULT '',
    embedding    BLOB,
    embedded_at  INTEGER NOT NULL,
    version      INTEGER NOT NULL,
    PRIMARY KEY (source, source_id)
);

-- The one index 0004 kept, and for its reason: it COVERS its only reader, the
-- `GROUP BY model, dim` that names every embedding space the corpus holds.
CREATE INDEX kb_vectors_model_idx ON kb_vectors (model, dim);

CREATE TABLE kb_vectors_bin (
    source       TEXT    NOT NULL,
    source_id    TEXT    NOT NULL,
    container    TEXT    NOT NULL DEFAULT '',
    -- The same bucket as its kb_vectors row, written in the same transaction
    -- by the same applier. Two rows that disagreed would put a document in one
    -- bucket for the candidate scan and another for the rerank, which is a
    -- document neither can find.
    search_shard INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    bits         BLOB    NOT NULL,
    PRIMARY KEY (source, source_id)
);
