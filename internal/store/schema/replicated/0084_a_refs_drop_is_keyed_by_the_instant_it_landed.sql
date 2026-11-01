-- A ref's drop is keyed by the instant it landed.
--
-- 0082 keyed code_ref_drops (repo_id, position, ref, old) and gave it a second
-- index, code_ref_drops_at_idx (repo_id, at, position, ref, old), for the one
-- order every statement over a repository's drops reads: the push apply's
-- expiry of the drops past code.PruneGrace, oldest first by instant and cut at
-- the record's row budget; a purge part's removal, in the same order; the
-- final purge's; and the purge's bounded count, which only asks whether a
-- repository holds any. The index was the key with the instant moved ahead of
-- the position — and the instant is ONE VALUE A POSITION, the broker instant
-- of the record that wrote the drop, which every drop that record writes
-- carries — so the two held the same columns and no statement read the key's
-- own order. Every drop was written into three b-trees, the table, its key
-- and the index, and every expiry and purge removed it from three.
--
-- SO THE KEY LEADS WITH THE INSTANT and the index goes: one order serves the
-- expiry, the purge and the bounded count, each seeking the key's own index
-- (internal/code names it, sqlite_autoindex_code_ref_drops_1, and its plan
-- test holds every statement to that seek), and a drop is two b-trees' entries
-- rather than three — a third fewer, each of which carried the repository, the
-- position, the ref and the object again, on every data node. The instant
-- following the repository is also how the expunge will ask for a
-- repository's drops since an instant. Unique exactly as before: with the
-- position in it, the instant adds no second row a record could write, so an
-- object one record leaves twice is still one row, and the apply's insert,
-- which skips a conflict on this key, still never aborts on it.
--
-- CREATED AGAIN, EMPTY, RATHER THAN REBUILT: no release has shipped 0082, so
-- no store a release opened holds one of its rows, and a reshape of what no
-- release shipped carries nothing across. 0082 IS NOT EDITED:
-- `schema_migrations` keys on the filename, so a store that applied it would
-- never see the change. The old index is dropped by name first — dropping the
-- table would take it anyway — so the file says where it went.
--
-- Class: code_ref_drops is reproducible, as 0082 declared it: written only by
-- the applier, from the push records that force-move or delete refs, removed
-- by the push records past its grace and by a purge's records.
DROP INDEX code_ref_drops_at_idx;

DROP TABLE code_ref_drops;

CREATE TABLE code_ref_drops (
    repo_id  TEXT    NOT NULL,
    at       INTEGER NOT NULL,
    position INTEGER NOT NULL,
    ref      TEXT    NOT NULL,
    old      TEXT    NOT NULL,
    PRIMARY KEY (repo_id, at, position, ref, old)
);
