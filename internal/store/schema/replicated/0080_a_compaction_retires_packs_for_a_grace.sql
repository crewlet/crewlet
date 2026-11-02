-- A compaction retires packs for a grace.
--
-- A pack a compaction merges into a larger one leaves code_packs, and its
-- object cannot leave the store with it: a cache that has not yet synced past
-- the compaction may still be fetching it, and a clone of the largest bundle a
-- row can name may read it for objstore.ReadBudget(code.MaxPackBytes), 21 h 21
-- min. So the row moves here, into a RETIRED reference (ADR-0074: a retired
-- reference keeps its object for a grace), which keeps the object from the
-- collector for code.RetireGrace (24 h) and does nothing else — the audit never
-- asks after it and no backup carries it. A retire_sweep record of the
-- code-retention duty removes a repository's rows past the grace, oldest first,
-- and the collector's next pass deletes their objects as it deletes anything no
-- row names; a purge removes a repository's whole.
--
-- The columns are code_packs' but its state, and where and when the row was
-- retired: retired_position, the packed position of the record that retired
-- it — what a sweep removes the rows at or below — and retired_at, that
-- record's broker instant as store.EncodeTime writes it (unix microseconds),
-- which the duty's clock judges the grace against.
--
-- code_repos.retired_through is the highest position a sweep has removed rows
-- at or below: the rows that would tell a reader of the repository's history
-- about a retirement at or below it are gone. Its default is the value that
-- says nothing was swept, which SQLite also requires of a NOT NULL column
-- added to a table.
--
-- Class: code_packs_retired is reproducible: written only by the applier, from
-- the records that retire packs, a retire_sweep's and a purge's. code_repos
-- stays reproducible: retired_through is written only from a retire_sweep
-- record.
CREATE TABLE code_packs_retired (
    repo_id          TEXT    NOT NULL,
    pack_id          TEXT    NOT NULL,
    part             TEXT    NOT NULL,
    object           TEXT    NOT NULL,
    hash             TEXT    NOT NULL,
    size             INTEGER NOT NULL,
    index_bytes      INTEGER NOT NULL,
    reverse_bytes    INTEGER NOT NULL,
    git_checksum     TEXT    NOT NULL,
    objects          INTEGER NOT NULL,
    kind             TEXT    NOT NULL,
    added_position   INTEGER NOT NULL,
    retired_position INTEGER NOT NULL,
    retired_at       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, pack_id, part)
);

CREATE INDEX code_packs_retired_object_idx ON code_packs_retired (object);    -- the collector's question: which of these objects a row names
CREATE INDEX code_packs_retired_repo_idx ON code_packs_retired (repo_id, retired_position, pack_id, part);    -- a repository's retired rows oldest first: a sweep's decide and apply, a purge's parts
CREATE INDEX code_packs_retired_due_idx ON code_packs_retired (retired_at, repo_id);    -- the code-retention pass's listing of the repositories holding rows past the grace

ALTER TABLE code_repos ADD COLUMN retired_through INTEGER NOT NULL DEFAULT 0;
