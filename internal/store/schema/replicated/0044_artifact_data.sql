-- The artifact datastore lands: the state log's seventh domain, the shared,
-- durable collections of documents an artifact keeps. internal/artifactdata is
-- the domain and internal/artifacts the vocabulary its writes are spoken in.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ARTIFACT_DATA_LOG stream by one deterministic applier that DECIDES
-- each write as it applies it (a sequenced log: no broker expectation, the log
-- orders and the applier judges every write from the record and these rows).
-- Nothing here is written by a caller; an identity claim compares these rows
-- byte for byte across the fleet.
--
-- ## Compact keys
--
-- The pinned driver refuses WITHOUT ROWID, so every table is a rowid table
-- plus its primary-key index and every index entry repeats its key. A text
-- artifact id and collection name in every field-index entry would store each
-- row about four times over, so a store is numbered once (`store_no`, the
-- packed position of its first record, the same on every node), a collection
-- within it (`collection_no`) and an indexed path within it (`path_no`), and
-- the hot tables key on those integers.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY: a purge removes a store's rows in its own statement list.
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY: a constraint violation inside the apply
-- transaction aborts it on every node at once. NO `COLLATE`: the default
-- BINARY collation reproduces Go's comparison, which the query compiler's
-- range and prefix filters rely on.
--
-- ## Attribution is a seat handle, and instants are the broker's
--
-- Every column naming who acted holds a seat handle (or a machine login) and
-- the actor's kind, never a display name. Every instant is the broker's store
-- timestamp of the record that wrote it, as `store.EncodeTime` integers, and a
-- version is a record's packed position `(generation << 40) | sequence`.

-- artifact_data_stores: one row per artifact store, the catalogue row a later
-- build's tiering keeps for a store it has moved to object storage.
CREATE TABLE artifact_data_stores (
    artifact_id        TEXT    NOT NULL PRIMARY KEY,
    -- The packed position of the first record applied on the store: its
    -- number in every other table, identical on every node.
    store_no           INTEGER NOT NULL,
    -- Live documents and the tombstones still held for catch-up.
    docs               INTEGER NOT NULL DEFAULT 0,
    tombstones         INTEGER NOT NULL DEFAULT 0,
    -- THE FOOTPRINT, in real bytes held on every data node: canonical
    -- bodies, field-index entries at 48 bytes each, and hot history. It is
    -- what `artifacts.limits.data_store_bytes` bounds.
    body_bytes         INTEGER NOT NULL DEFAULT 0,
    field_entries      INTEGER NOT NULL DEFAULT 0,
    history_bytes      INTEGER NOT NULL DEFAULT 0,
    footprint          INTEGER NOT NULL DEFAULT 0,
    -- Every record applied on the store, refused ones included.
    writes             INTEGER NOT NULL DEFAULT 0,
    -- The state digest: the sum modulo 2^256 of one SHA-256 per live
    -- document and one per collection (internal/artifactdata/canon).
    state_digest       BLOB    NOT NULL,
    -- The oldest write still in the hot history (0 before anything was
    -- pruned), and the position below which a catch-up by version cannot
    -- see every delete, so a client there reads the store whole.
    history_floor      INTEGER NOT NULL DEFAULT 0,
    tombstones_floor   INTEGER NOT NULL DEFAULT 0,
    -- The next collection and path numbers this store assigns.
    next_collection_no INTEGER NOT NULL DEFAULT 1,
    next_path_no       INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    version            INTEGER NOT NULL
);
CREATE INDEX artifact_data_stores_no_idx ON artifact_data_stores (store_no);                 -- a store by its number

-- artifact_data_collections: one row per collection, its compiled rules and
-- schema as the canonical JSON of an artifacts.Collection. A preset is
-- compiled into the rule fields before it is stored, so one evaluator reads
-- one vocabulary.
CREATE TABLE artifact_data_collections (
    store_no      INTEGER NOT NULL,
    collection_no INTEGER NOT NULL,
    name          TEXT    NOT NULL,
    spec          TEXT    NOT NULL,
    -- 1 for a collection a write created under the closed default
    -- (participants read, editors write, no schema) rather than a schema
    -- change.
    implicit      INTEGER NOT NULL DEFAULT 0,
    docs          INTEGER NOT NULL DEFAULT 0,
    body_bytes    INTEGER NOT NULL DEFAULT 0,
    version       INTEGER NOT NULL,
    PRIMARY KEY (store_no, collection_no)
);
CREATE INDEX artifact_data_collections_name_idx ON artifact_data_collections (store_no, name); -- a collection by name

-- artifact_data_paths: the store's interned indexed paths.
CREATE TABLE artifact_data_paths (
    store_no INTEGER NOT NULL,
    path_no  INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (store_no, path_no)
);
CREATE INDEX artifact_data_paths_path_idx ON artifact_data_paths (store_no, path);           -- a path's number

-- artifact_data_docs: one row per document. A delete keeps the row as a
-- tombstone (body NULL), because a reconnecting client catches up by version
-- and must see deletes, until the history floor passes it.
CREATE TABLE artifact_data_docs (
    store_no        INTEGER NOT NULL,
    collection_no   INTEGER NOT NULL,
    doc_id          TEXT    NOT NULL,
    -- Canonical JSON (internal/artifactdata/canon), or NULL for a tombstone.
    body            TEXT,
    size            INTEGER NOT NULL,
    created_by      TEXT    NOT NULL,
    created_by_kind TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_by      TEXT    NOT NULL,
    updated_by_kind TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL,
    -- The packed position of the last write that changed the document:
    -- what a write's if_version guard compares.
    version         INTEGER NOT NULL,
    PRIMARY KEY (store_no, collection_no, doc_id)
);
CREATE INDEX artifact_data_docs_since_idx ON artifact_data_docs (store_no, version);          -- catch-up by version
CREATE INDEX artifact_data_docs_creator_idx ON artifact_data_docs (store_no, collection_no, created_by); -- own rows, private rows, a principal's cap

-- artifact_data_fields: the field index, one row per indexed scalar of a live
-- document, and one presence marker for a top-level field holding nothing
-- scalar, so every top-level field a document holds has a row here. A value
-- inside a body cannot be indexed, and a query that decodes every body is a
-- full scan wearing an index's name.
CREATE TABLE artifact_data_fields (
    store_no      INTEGER NOT NULL,
    collection_no INTEGER NOT NULL,
    path_no       INTEGER NOT NULL,
    doc_id        TEXT    NOT NULL,
    -- 0 for a scalar field, 1 and up for each element of an array of
    -- scalars, which is what `contains` (a chip set) reads, and -1 for a
    -- presence marker, which no query reads.
    ord           INTEGER NOT NULL,
    -- s (string), n (number), b (boolean, num 1 or 0), z (null), p (a
    -- presence marker).
    kind          TEXT    NOT NULL,
    num           REAL,
    -- A string's first 256 bytes, cut at a rune boundary.
    txt           TEXT,
    PRIMARY KEY (store_no, collection_no, doc_id, path_no, ord)
);
CREATE INDEX artifact_data_fields_num_idx ON artifact_data_fields (store_no, collection_no, path_no, num, doc_id); -- numeric filters and order
CREATE INDEX artifact_data_fields_txt_idx ON artifact_data_fields (store_no, collection_no, path_no, txt, doc_id); -- text filters and order

-- artifact_data_writes: every record applied on a store, applied or refused:
-- who, when, the verdict, what changed (bounded values, enough to undo it) and
-- the state digest after it. Kept seven days and at most 8 MiB per store,
-- pruned in each write's own transaction from broker time.
CREATE TABLE artifact_data_writes (
    store_no     INTEGER NOT NULL,
    -- The record's packed position.
    seq          INTEGER NOT NULL,
    op_id        TEXT    NOT NULL,
    -- write, undo or configure.
    op           TEXT    NOT NULL,
    actor        TEXT    NOT NULL,
    actor_kind   TEXT    NOT NULL,
    operator_id  TEXT    NOT NULL DEFAULT '',
    -- tool, dashboard, bridge or api: the surface the write came through.
    origin       TEXT    NOT NULL DEFAULT '',
    work_item    TEXT    NOT NULL DEFAULT '',
    -- 1 when the admin path admitted an actor with no relation.
    elevated     INTEGER NOT NULL DEFAULT 0,
    client_id    TEXT    NOT NULL DEFAULT '',
    at           INTEGER NOT NULL,
    -- applied, conflict, invalid, forbidden or quota.
    verdict      TEXT    NOT NULL,
    reason       TEXT    NOT NULL DEFAULT '',
    -- JSON: per document, the op, the versions before and after, and each
    -- changed field's values before and after (each at most 4 KiB, a hash
    -- beyond).
    changes      TEXT    NOT NULL DEFAULT '[]',
    -- What this row counts against the store's history bound.
    bytes        INTEGER NOT NULL,
    digest_after BLOB    NOT NULL,
    undoable     INTEGER NOT NULL DEFAULT 0,
    -- The write an undo reverted, 0 for anything else.
    undoes       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (store_no, seq)
);
CREATE INDEX artifact_data_writes_op_idx ON artifact_data_writes (op_id);                     -- a writer reading its own verdict back
CREATE INDEX artifact_data_writes_at_idx ON artifact_data_writes (store_no, at, seq);         -- the hot-window prune, in a total order
CREATE INDEX artifact_data_writes_actor_idx ON artifact_data_writes (store_no, actor, seq);   -- one person's changes

-- artifact_data_touches: which documents each history row changed, so a
-- document's and a cell's history is one indexed read rather than a decode of
-- the store's whole history. Pruned with the row it indexes.
CREATE TABLE artifact_data_touches (
    store_no      INTEGER NOT NULL,
    collection_no INTEGER NOT NULL,
    doc_id        TEXT    NOT NULL,
    seq           INTEGER NOT NULL,
    PRIMARY KEY (store_no, collection_no, doc_id, seq)
);
CREATE INDEX artifact_data_touches_seq_idx ON artifact_data_touches (store_no, seq);         -- the prune of one history row's touches

-- artifact_data_usage: the company's datastore counters, which
-- `artifacts.limits.data_bytes` is enforced against. One row, scope 'company'.
CREATE TABLE artifact_data_usage (
    scope     TEXT    NOT NULL PRIMARY KEY,
    stores    INTEGER NOT NULL DEFAULT 0,
    docs      INTEGER NOT NULL DEFAULT 0,
    footprint INTEGER NOT NULL DEFAULT 0
);

-- artifactdata_deletions: the permanent deletion marker a store's purge
-- leaves, so a record about the store that arrives later produces no rows on
-- any node.
CREATE TABLE artifactdata_deletions (
    artifact_id     TEXT    NOT NULL PRIMARY KEY,
    at              INTEGER NOT NULL,
    by              TEXT    NOT NULL DEFAULT '',
    by_kind         TEXT    NOT NULL DEFAULT '',
    -- The purge's operation id: the gate lets exactly that record through.
    purge_record_id TEXT    NOT NULL DEFAULT '',
    rejects         INTEGER NOT NULL DEFAULT 0,
    last_reject_at  INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);

-- artifactdata_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE artifactdata_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- artifactdata_log_generations: every reanchor's audit row.
CREATE TABLE artifactdata_log_generations (
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

-- artifactdata_ops: what this node's applier has written. It TRAVELS in every
-- donated snapshot and stays out of the identity claim.
CREATE TABLE artifactdata_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX artifactdata_ops_swept_idx ON artifactdata_ops (applied_at);                     -- the ops retention sweep

-- artifactdata_log_deferred: a record this build could not decode, retained
-- byte for byte at its original position.
CREATE TABLE artifactdata_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX artifactdata_log_deferred_subject_idx ON artifactdata_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE artifactdata_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX artifactdata_log_deferred_scope_idx ON artifactdata_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
