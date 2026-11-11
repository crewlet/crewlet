-- The atlas domain lands: the state log's thirteenth domain, the record of what
-- people tell the engine about what the company runs on (an entry they add, a
-- fact they correct, two entries they join or keep apart) and of what an
-- administrator purged. internal/atlasdomain is the domain (ADR-0126).
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ATLAS_LOG stream by one deterministic applier. The atlas is read from
-- whichever node a person reaches, several people edit it, and its history must
-- outlive any one node's event store.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY. NO `UNIQUE` OUTSIDE A PRIMARY KEY. Every instant a row records
-- of its own is the broker's store timestamp of the record that wrote it, as
-- `store.EncodeTime` integers, and a position is a record's packed position.
-- The applier stores every vocabulary word as text and judges no value: the
-- writer judged it before it was published.

-- atlas_entries: one row per entry the log holds a claim about, by its id (a
-- name-based UUID of its kind and key). `version` is the position of the newest
-- record on the entry's own subject, the tag a person's edit names. A person's
-- newest word on the entry itself is at `exists_position`; `retired` is 1 while
-- that word retires the entry, and `retired_at` when it did. `last_at` is the
-- newest claim recorded about it, which a retired entry's retention runs from. `key_digest`
-- is the SHA-256 of its key, which a value's purge finds it by.
CREATE TABLE atlas_entries (
    entry_id        TEXT    NOT NULL PRIMARY KEY,
    kind            TEXT    NOT NULL,
    key             TEXT    NOT NULL,
    key_digest      TEXT    NOT NULL,
    version         INTEGER NOT NULL,
    exists_position INTEGER NOT NULL DEFAULT 0,
    retired         INTEGER NOT NULL DEFAULT 0,
    retired_at      INTEGER NOT NULL DEFAULT 0,
    last_at         INTEGER NOT NULL,
    at              INTEGER NOT NULL,
    position        INTEGER NOT NULL
);
CREATE INDEX atlas_entries_key_idx ON atlas_entries (key_digest);                                  -- a value's purge
CREATE INDEX atlas_entries_retired_idx ON atlas_entries (last_at, entry_id) WHERE retired = 1;     -- the retention

-- atlas_claims: one row per claim, kept as history when a newer claim of the
-- same voice (`source_kind`, `source_name`) on the same `slot` replaces it:
-- `replaced` is then 1, and `superseded_at` and `superseded_position` record
-- when and by which record; the retention takes it after that. A relation, a "the same" or a "not the same" names its other
-- entry by id, kind and key; a relation to an identity outside the atlas names
-- it by `external` and `target_name`. A fact's value is its type, its canonical
-- text and whether it names a private network. `value_digest` and
-- `target_key_digest` are what a value's purge finds a claim by. `claim` is the
-- record's own claim, its JSON as the log carries it, which the read of every
-- live claim decodes in one column rather than many.
CREATE TABLE atlas_claims (
    claim_id            TEXT    NOT NULL PRIMARY KEY,
    entry_id            TEXT    NOT NULL,
    slot                TEXT    NOT NULL,
    facet               TEXT    NOT NULL,
    fact                TEXT    NOT NULL DEFAULT '',
    relation            TEXT    NOT NULL DEFAULT '',
    target_id           TEXT    NOT NULL DEFAULT '',
    target_kind         TEXT    NOT NULL DEFAULT '',
    target_key          TEXT    NOT NULL DEFAULT '',
    target_key_digest   TEXT    NOT NULL DEFAULT '',
    external            TEXT    NOT NULL DEFAULT '',
    target_name         TEXT    NOT NULL DEFAULT '',
    value_type          TEXT    NOT NULL DEFAULT '',
    value_text          TEXT    NOT NULL DEFAULT '',
    value_private       INTEGER NOT NULL DEFAULT 0,
    value_digest        TEXT    NOT NULL DEFAULT '',
    denied              INTEGER NOT NULL DEFAULT 0,
    source_kind         TEXT    NOT NULL,
    source_name         TEXT    NOT NULL DEFAULT '',
    by                  TEXT    NOT NULL DEFAULT '',
    by_kind             TEXT    NOT NULL DEFAULT '',
    observed_at         INTEGER NOT NULL,
    at                  INTEGER NOT NULL,
    position            INTEGER NOT NULL,
    replaced            INTEGER NOT NULL DEFAULT 0,
    superseded_at       INTEGER NOT NULL DEFAULT 0,
    superseded_position INTEGER NOT NULL DEFAULT 0,
    claim               TEXT    NOT NULL
);
CREATE INDEX atlas_claims_entry_idx ON atlas_claims (entry_id, position);                     -- an entry's history, newest first
CREATE INDEX atlas_claims_voice_idx ON atlas_claims (entry_id, slot, source_kind, source_name)
    WHERE replaced = 0;                                                                      -- a voice's current claim on a slot
CREATE INDEX atlas_claims_live_idx ON atlas_claims (position) WHERE replaced = 0;               -- every live claim, the fold's input
CREATE INDEX atlas_claims_target_idx ON atlas_claims (target_id);                              -- an entry's purge: the claims naming it
CREATE INDEX atlas_claims_target_key_idx ON atlas_claims (target_key_digest);                  -- a value's purge: the claims naming it
CREATE INDEX atlas_claims_value_idx ON atlas_claims (value_digest);                            -- a value's purge: the claims carrying it
CREATE INDEX atlas_claims_expiry_idx ON atlas_claims (superseded_at, claim_id) WHERE replaced = 1; -- the retention

-- atlas_purges: one marker per purged entry (`kind` entry, `id` its id) or
-- value (`kind` value, `id` the SHA-256 of its text), never the value itself.
-- Permanent: every later record about it lands as nothing on every node, its
-- purge's own excepted (`purge_record_id`), and `rejects` counts those above
-- the purge's own position, each once (`last_reject_position`).
CREATE TABLE atlas_purges (
    kind                 TEXT    NOT NULL,
    id                   TEXT    NOT NULL,
    at                   INTEGER NOT NULL,
    by                   TEXT    NOT NULL DEFAULT '',
    by_kind              TEXT    NOT NULL DEFAULT '',
    purge_record_id      TEXT    NOT NULL,
    rejects              INTEGER NOT NULL DEFAULT 0,
    last_reject_at       INTEGER NOT NULL DEFAULT 0,
    last_reject_position INTEGER NOT NULL DEFAULT 0,
    position             INTEGER NOT NULL,
    PRIMARY KEY (kind, id)
);
CREATE INDEX atlas_purges_position_idx ON atlas_purges (position); -- the purges, newest first

-- atlas_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE atlas_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- atlas_log_generations: every reanchor's audit row.
CREATE TABLE atlas_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The log's own machinery, whose statements the framework generates (0001 and
-- 0021).
-- ---------------------------------------------------------------------------

-- atlas_ops: what this node's applier has written. It TRAVELS in every donated
-- snapshot and stays out of the identity claim.
CREATE TABLE atlas_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX atlas_ops_swept_idx ON atlas_ops (applied_at); -- the ops retention sweep

-- atlas_log_deferred: a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE atlas_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX atlas_log_deferred_subject_idx ON atlas_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE atlas_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX atlas_log_deferred_scope_idx ON atlas_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
