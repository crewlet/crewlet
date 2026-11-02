-- The code host's durable state: the read side of the code log.
--
-- The company's git repositories as ROWS. A repository's settings, its
-- protection rules, its refs, the list of pack bundles its pushes stored and
-- its push history are replicated tables the code domain's applier writes from
-- one ordered log (CREWLET_CODE_LOG); the BYTES of every pack are one object a
-- push in the object store, named by a row of code_packs; and every git
-- node's bare repository is a disposable cache derived from these rows. It
-- replaces nothing: the engine hosted no code before it.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet.
--
-- # The rules the schema enforces, and why
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: a purge
-- removes a repository's rows by statements its own record runs, identically
-- on every node.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY. A constraint violation inside the apply
-- transaction aborts it deterministically on every node and stalls the fleet's
-- log. Two creates of one name are arbitrated at the BROKER — the name's token
-- is the create's subject — never by the schema.
--
-- NO `COLLATE` ANYWHERE. A ref's order is BYTE order, which is the order
-- packed-refs needs and the order Go compares in; a collation would be a
-- second answer to what one ref is.
--
-- # Why code_refs keeps tombstones
--
-- A deleted ref stays as a row with an EMPTY target, at the position of the
-- record that deleted it, so a cache syncing a delta learns the deletion from
-- the rows a delta reads. The push apply sweeps tombstones older than
-- code.RefTombstoneHorizon — as many as the record's row budget has left, so
-- a repository whose pushes delete refs in bulk does not keep them for ever —
-- and raises code_repos.tombstones_through as it does: a delta from below that
-- mark can no longer learn every deletion, so a snapshot asked for one answers
-- in full instead. code_repos.tombstones counts what is left.
--
-- # Why code_packs.object is the declared column
--
-- It names the object a push stored, and internal/objstore/references declares
-- the table, so the collector keeps the object, the audit asks the store for
-- it and every backup carries it — REQUIRED (ADR-0074). The object is a pack
-- BUNDLE: the pack, then its .idx and its .rev, back to back, so one GET
-- installs a push on any git node with no index-pack. `size` is the WHOLE
-- object's, which the collector's audit compares, and `index_bytes` and
-- `reverse_bytes` cut its sections from the end.
--
-- # Why push rows have a horizon, inside their repository
--
-- A push row is history ("who pushed what, when"), kept code.PushHistory
-- (usage.History, 181 days). The push apply deletes a bounded number of a
-- repository's own expired rows as it adds new ones — a pure function of the
-- record's broker instant, inside the record's own scope — because a sweep
-- would be a second writer of the replicated estate.

-- code_repos — one row per repository, whatever its status.
CREATE TABLE code_repos (
    id                 TEXT    NOT NULL PRIMARY KEY,
    name               TEXT    NOT NULL,
    status             TEXT    NOT NULL,
    managed_by         TEXT    NOT NULL,
    owner_kind         TEXT    NOT NULL,
    owner              TEXT    NOT NULL,
    default_branch     TEXT    NOT NULL,
    description        TEXT    NOT NULL DEFAULT '',
    -- The activation the settings came from (configplane.ActivationStamp).
    config_epoch       INTEGER NOT NULL DEFAULT 0,
    -- Live refs: rows of code_refs with a non-empty target. The applier keeps
    -- this and the three counts below as it moves the rows, so no read ever
    -- counts a namespace by walking it.
    refs               INTEGER NOT NULL DEFAULT 0,
    -- The live refs under refs/heads/ and under refs/tags/.
    branches           INTEGER NOT NULL DEFAULT 0,
    tags               INTEGER NOT NULL DEFAULT 0,
    -- Tombstones: rows of code_refs with an empty target — what the
    -- repository's refs hold beyond its live ones.
    tombstones         INTEGER NOT NULL DEFAULT 0,
    -- The highest position of a tombstone the push apply has swept: a delta
    -- from below it cannot learn every deletion, and is answered in full.
    tombstones_through INTEGER NOT NULL DEFAULT 0,
    live_packs         INTEGER NOT NULL DEFAULT 0,
    live_bytes         INTEGER NOT NULL DEFAULT 0,
    -- Pushes EVER, which the push horizon does not decrement.
    pushes             INTEGER NOT NULL DEFAULT 0,
    last_push_at       INTEGER,
    -- Set exactly while the status is deleted or purging.
    deleted_at         INTEGER,
    purge_parts        INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    -- The composed position of the last record on this repository's subject.
    version            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX code_repos_name_idx ON code_repos (name);                                              -- Reader.Read(repos): the listing in name order, paged by name
-- PARTIAL, on the predicate the query repeats verbatim rather than on the
-- status: the pinned driver's planner is not measured to prove an IN-list
-- implication, and deleted_at is set exactly while the status is deleted or
-- purging. The id makes the order total, so the pass's order is the index's.
CREATE INDEX code_repos_deleted_idx ON code_repos (deleted_at, id) WHERE deleted_at IS NOT NULL;    -- Reader.DeletedPastRetention: the code-retention pass

-- code_names — a repository name's claim, keyed by the token the create
-- arbitrated on. Insert-only until a purge releases it.
CREATE TABLE code_names (
    token   TEXT NOT NULL PRIMARY KEY,
    name    TEXT NOT NULL,
    repo_id TEXT NOT NULL
);

-- code_refs — a repository's refs, tombstones included (target '').
--
-- THE OBJECT A REF NAMES IS `target`, NEVER `oid`: `oid` is one of the three
-- names SQLite gives the rowid, and the pinned driver resolves
-- `excluded.oid` in an upsert to the excluded row's ROWID rather than to a
-- declared column of that name — measured: every ref an upsert moved was
-- stored as a small integer. The store's schema gate refuses a column named
-- for the rowid in either estate.
CREATE TABLE code_refs (
    repo_id  TEXT    NOT NULL,
    ref      TEXT    NOT NULL,
    target   TEXT    NOT NULL,
    position INTEGER NOT NULL,
    at       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, ref)
);
CREATE INDEX code_refs_changed_idx ON code_refs (repo_id, position, ref);                           -- Reader.Snapshot: the refs a delta reads, in cursor order
CREATE INDEX code_refs_tombstones_idx ON code_refs (repo_id, at, ref) WHERE target = '';            -- the push apply's RefTombstoneHorizon delete, oldest first

-- code_packs — every pack bundle a repository's pushes stored, one object each.
CREATE TABLE code_packs (
    repo_id        TEXT    NOT NULL,
    pack_id        TEXT    NOT NULL,
    part           TEXT    NOT NULL,
    object         TEXT    NOT NULL,
    hash           TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    index_bytes    INTEGER NOT NULL,
    reverse_bytes  INTEGER NOT NULL,
    git_checksum   TEXT    NOT NULL,
    objects        INTEGER NOT NULL,
    kind           TEXT    NOT NULL,
    state          TEXT    NOT NULL,
    added_position INTEGER NOT NULL,
    PRIMARY KEY (repo_id, pack_id, part)
);
CREATE INDEX code_packs_object_idx ON code_packs (object);                                          -- objstore.ReferenceTable statements (ObjectsAmong, ReferencesAfter): the collector, its audit and the backup
CREATE INDEX code_packs_added_idx ON code_packs (repo_id, added_position, pack_id, part);           -- Reader.Snapshot: the packs a delta and a tail read, in cursor order

-- code_pushes — the push history, kept code.PushHistory inside each repository.
CREATE TABLE code_pushes (
    push_id          TEXT    NOT NULL PRIMARY KEY,
    repo_id          TEXT    NOT NULL,
    position         INTEGER NOT NULL,
    at               INTEGER NOT NULL,
    actor            TEXT    NOT NULL,
    actor_kind       TEXT    NOT NULL,
    actor_seat       TEXT    NOT NULL DEFAULT '',
    node             TEXT    NOT NULL DEFAULT '',
    atomic           INTEGER NOT NULL DEFAULT 0,
    -- Every command the client sent, with its result, as JSON; a refusal's
    -- sentence is an answer and is never stored.
    commands         TEXT    NOT NULL,
    pack_id          TEXT    NOT NULL DEFAULT '',
    pack_bytes       INTEGER NOT NULL DEFAULT 0,
    introduced       TEXT    NOT NULL DEFAULT '',
    introduced_count INTEGER NOT NULL DEFAULT 0,
    received_at      INTEGER NOT NULL,
    -- The bytes of commands and introduced as written: what deleting the row
    -- costs, since a wide row's delete frees its overflow pages. A purge part
    -- and the PushHistory horizon are bounded by it as well as by rows
    -- (code.PurgePartBytes, code.HorizonBytesPerRecord); no default, so no
    -- writer can store a row that reads as free.
    row_bytes        INTEGER NOT NULL
);
-- push_id ends the key because group commit puts up to 64 pushes in one
-- record, sharing its position and its instant: a cursor or a delete order
-- without it is not total. row_bytes rides past the key so the deletes that
-- are bounded by it read the order AND the widths from the index alone,
-- never a wide row's overflow pages.
CREATE INDEX code_pushes_repo_idx ON code_pushes (repo_id, at, position, push_id, row_bytes);       -- Reader.Read(pushes) newest first, and the push apply's PushHistory horizon, the purge and its remaining count oldest first

-- code_protections — a repository's protection rules, replaced whole.
CREATE TABLE code_protections (
    repo_id      TEXT    NOT NULL,
    pattern      TEXT    NOT NULL,
    allow_force  INTEGER NOT NULL DEFAULT 0,
    allow_delete INTEGER NOT NULL DEFAULT 0,
    direct_push  TEXT    NOT NULL,
    PRIMARY KEY (repo_id, pattern)
);

-- code_activation — the ACTIVATION FENCE: the latest configuration activation
-- whose reconcile of code.repos has begun. A config-managed create, settings
-- change, delete or restore decided at an older activation applies as nothing,
-- on every node alike, because the fence is derived from the log — so a node
-- applying an older configuration late never walks a newer one back, and an
-- activation that changes nothing writes one record rather than one per
-- declared repository. One row, keyed by the company subject's id.
CREATE TABLE code_activation (
    id      TEXT    NOT NULL PRIMARY KEY,
    fence   INTEGER NOT NULL,
    version INTEGER NOT NULL
);

-- code_deletions — the permanent deletion marker: how a node that was away
-- tells a repository that never existed from one that was purged. A purge's
-- own record writes it and is the one record it does not gate.
CREATE TABLE code_deletions (
    repo_id         TEXT    NOT NULL PRIMARY KEY,
    name            TEXT    NOT NULL DEFAULT '',
    at              INTEGER NOT NULL,
    by              TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL DEFAULT '',
    purge_record_id TEXT    NOT NULL DEFAULT '',
    rejects         INTEGER NOT NULL DEFAULT 0,
    last_reject_at  INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);

-- code_evictions — a node's removal from THIS log; its own table because an
-- eviction fences records above a position on this stream alone.
CREATE TABLE code_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- code_log_generations — every reanchor's audit row.
CREATE TABLE code_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- code_ops — what this node's applier has written: the operation ledger,
-- which travels in a snapshot with the rows it describes.
CREATE TABLE code_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX code_ops_swept_idx ON code_ops (applied_at);                                           -- the ops retention sweep (maintenance.StatelogJobs)

-- code_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE code_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX code_log_deferred_subject_idx ON code_log_deferred (subject_id, subject_kind);          -- the applier's per-object deferral probe

-- code_log_deferred_scope — one row per scope path of a deferred record,
-- written in the same transaction as its parent.
CREATE TABLE code_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX code_log_deferred_scope_idx ON code_log_deferred_scope (path, position);              -- the read barrier's coverage probe and the writer's step 0
