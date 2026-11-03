-- A ref's drop is kept for the expunge's grace.
--
-- A push that FORCE-MOVES a branch, or deletes a branch or a tag, leaves the
-- object the ref held behind it: no ref names it any more, and every object
-- only it reached stays in the repository's packs, fetchable by its id.
-- Dropping a repository's unreachable objects (an expunge) must keep those for
-- code.PruneGrace — git's own gc.pruneExpire, two weeks — so history
-- force-pushed away or deleted can still be fetched, and a branch restored at
-- it, for that long after the drop. Which objects they are cannot be learned
-- from the refs, which no longer name them, nor from the push history without
-- a scan of every push the repository took in 181 days (code_pushes, whose
-- commands carry every old value). So the push's apply records each one here:
-- a row for every command it applied that moved a ref off an object — a forced
-- update or a deletion — naming the ref, the object it left (old), and where
-- and when the record that did it landed.
--
-- position is the packed position of that record, and at its broker instant
-- as store.EncodeTime writes it (unix microseconds), which the grace is judged
-- against. ONE RECORD MAY LEAVE SEVERAL OBJECTS ON ONE REF — a batch whose
-- pushes moved it in turn, each seeing the one before — so the object is part
-- of the key: every one is kept, and an apply that meets one twice keeps it
-- once.
--
-- A push record removes its own repository's rows older than code.PruneGrace
-- by its broker instant, oldest first, with what its row budget leaves after
-- the tombstones (internal/code's push apply), and a purge removes a
-- repository's whole. The index's order is the one both read — by instant,
-- and then by the key, so a removal cut short at a record's budget cuts every
-- node's rows at the same row.
--
-- Class: code_ref_drops is reproducible: written only by the applier, from the
-- push records that force-move or delete refs, removed by the push records
-- past its grace and by a purge's records.
CREATE TABLE code_ref_drops (
    repo_id  TEXT    NOT NULL,
    position INTEGER NOT NULL,
    ref      TEXT    NOT NULL,
    old      TEXT    NOT NULL,
    at       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, position, ref, old)
);

CREATE INDEX code_ref_drops_at_idx ON code_ref_drops (repo_id, at, position, ref, old);    -- a repository's drops oldest first: the push apply's PruneGrace expiry, and a purge's parts
