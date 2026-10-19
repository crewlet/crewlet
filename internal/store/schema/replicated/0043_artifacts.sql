-- The artifacts domain lands: the state log's sixth domain, every fact about
-- an artifact except its datastore and its bytes. internal/artifactsdomain is
-- the domain and internal/artifacts the vocabulary it is written in.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ARTIFACTS_LOG stream by one deterministic applier. Nothing here is
-- written by a caller; the applier is the only writer, its transaction carries
-- the checkpoint, and an identity claim compares these rows byte for byte
-- across the fleet.
--
-- The bytes are not here. A version names its files by object (key, SHA-256,
-- size), and the object store keeps the bytes (ADR-0026). The two tables that
-- name objects, `artifact_objects` and `artifact_assets`, are declared in
-- internal/objstore/references, so the collector never deletes a file a
-- retained version or a live asset still names.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY: a cascade is a delete nobody committed, so the applier
-- removes an artifact's versions, objects, assets, shares, pins, reads and
-- history in its own statement list when a purge applies.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY: a constraint violation inside the apply
-- transaction aborts it identically on every node and stalls the log. A
-- version label's uniqueness per artifact and a source address's single
-- holder are decided in the writer's snapshot and arbitrated on the subject.
--
-- NO `COLLATE`: the default BINARY collation reproduces Go's comparison.
--
-- ## Attribution is a seat handle
--
-- Every column naming who acted holds a seat handle (or a machine login for an
-- unbound machine) and the actor's kind beside it, never a display name, so a
-- person's name can be pseudonymised without rewriting a row.
--
-- ## Instants are the broker's
--
-- Every instant below is the broker's own store timestamp of the record that
-- wrote it, as `store.EncodeTime` integers, never a writer's clock: that is
-- what makes one node's copy byte-identical to another's. A version guard is
-- the record's packed position `(generation << 40) | sequence`.

-- artifacts: one row per artifact. The head fields are what a listing shows.
CREATE TABLE artifacts (
    id              TEXT    NOT NULL PRIMARY KEY,
    -- The DNS host label, the id's own bytes in lowercase Crockford base32
    -- (D48). Stored beside the id so a serving node resolves a host without
    -- recomputing it, and so an operator can read it off the row.
    label           TEXT    NOT NULL,
    -- The kind (artifacts.Kind): html, doc or sheet.
    kind            TEXT    NOT NULL,
    title           TEXT    NOT NULL,
    description     TEXT    NOT NULL DEFAULT '',
    icon            TEXT    NOT NULL DEFAULT '',
    -- The owner, a seat handle or a machine login, and its actor kind: the
    -- kind is what opens the lead arm, which reaches only an agent's
    -- artifacts (D12).
    owner           TEXT    NOT NULL,
    owner_kind      TEXT    NOT NULL,
    -- The republish-in-place address the artifact was created at, empty for
    -- one created without one or released by a transfer.
    source_key      TEXT    NOT NULL DEFAULT '',
    head_number     INTEGER NOT NULL,
    -- The head version's runtime contract and declared capabilities, as a
    -- sorted JSON array.
    contract        TEXT    NOT NULL DEFAULT '',
    capabilities    TEXT    NOT NULL DEFAULT '[]',
    -- live or trashed. A trash is a stamp: rows stay, the source address
    -- stays held, serving stops at once.
    status          TEXT    NOT NULL,
    trashed_by      TEXT    NOT NULL DEFAULT '',
    trashed_by_kind TEXT    NOT NULL DEFAULT '',
    trashed_at      INTEGER NOT NULL DEFAULT 0,
    -- Moves on every NARROWING (an unshare, a lower role, a trash, a
    -- transfer), which is what a view ticket minted before it is checked
    -- against.
    access_epoch    INTEGER NOT NULL DEFAULT 0,
    -- The bytes every retained version and live asset of this artifact
    -- name, each distinct object once.
    bytes_held      INTEGER NOT NULL DEFAULT 0,
    -- What a gallery search matches: the head version's extracted text, at
    -- most 64 KiB.
    search_text     TEXT    NOT NULL DEFAULT '',
    created_by      TEXT    NOT NULL,
    created_by_kind TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_by      TEXT    NOT NULL,
    updated_by_kind TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL,
    version         INTEGER NOT NULL
);
CREATE INDEX artifacts_owner_idx ON artifacts (owner, updated_at);                          -- one owner's artifacts, newest first
CREATE INDEX artifacts_status_idx ON artifacts (status, updated_at);                        -- the gallery, newest first
CREATE INDEX artifacts_trashed_idx ON artifacts (trashed_at) WHERE trashed_at > 0;           -- the thirty-day trash reaper

-- artifact_source_claims: an owner's hold on one republish-in-place address.
--
-- THE ADDRESS IS ARBITRATED on the `source` subject, create-only, and this row
-- is the guard that still holds once the create that took it has been trimmed
-- off the log: the publisher refuses a second create on it from the row.
CREATE TABLE artifact_source_claims (
    owner       TEXT    NOT NULL,
    -- The first 16 bytes of SHA-256 over the source key, in hex: what the
    -- subject carries, since a source key is prose a subject may not hold.
    token       TEXT    NOT NULL,
    source_key  TEXT    NOT NULL,
    artifact_id TEXT    NOT NULL,
    version     INTEGER NOT NULL,
    PRIMARY KEY (owner, token)
);
CREATE INDEX artifact_source_claims_artifact_idx ON artifact_source_claims (artifact_id);   -- the claim a purge or a transfer releases

-- artifact_versions: one row per retained version.
--
-- RETENTION RIDES THE RECORD: a publish names the unlabelled versions past the
-- hundred it keeps, decided in the writer's snapshot, and the apply deletes
-- exactly those. Labelled versions are exempt.
CREATE TABLE artifact_versions (
    artifact_id   TEXT    NOT NULL,
    number        INTEGER NOT NULL,
    -- A name-based uuid over the publish's operation, so a retry names the
    -- version its first attempt made.
    version_id    TEXT    NOT NULL,
    -- The head the publish was decided against: number - 1, or 0 for the
    -- first version.
    base_number   INTEGER NOT NULL,
    restored_from INTEGER NOT NULL DEFAULT 0,
    forced        INTEGER NOT NULL DEFAULT 0,
    label         TEXT    NOT NULL DEFAULT '',
    entry         TEXT    NOT NULL DEFAULT '',
    contract      TEXT    NOT NULL DEFAULT '',
    capabilities  TEXT    NOT NULL DEFAULT '[]',
    files         INTEGER NOT NULL,
    bytes         INTEGER NOT NULL,
    -- The canonical manifest (internal/artifacts/manifest), script policies
    -- included: sorted by path, one form, so every node stores the same bytes.
    manifest      BLOB    NOT NULL,
    author        TEXT    NOT NULL,
    author_kind   TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    position      INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, number)
);
CREATE INDEX artifact_versions_label_idx ON artifact_versions (artifact_id, label);          -- the labelled versions, and a label's holder

-- artifact_objects: THE REFERENCE TABLE. One row per distinct object an
-- artifact's retained versions name, refcounted by the applier.
--
-- A CARRIED ENTRY IS WRITTEN ONLY WHEN THE APPLY FINDS ITS OBJECT HERE with a
-- positive count, in its own transaction; otherwise the record is skipped on
-- every node and its writer is told which paths to upload again. So once no
-- row names a key, no later record can name it, and the collector stays sound
-- without a lock (ADR-0027, as extended for carrying).
CREATE TABLE artifact_objects (
    artifact_id  TEXT    NOT NULL,
    object       TEXT    NOT NULL,
    hash         TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    -- How many manifest entries of retained versions name it. A row is
    -- deleted when this reaches zero, and only then is the object unnamed.
    refs         INTEGER NOT NULL,
    first_number INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, object)
);
CREATE INDEX artifact_objects_object_idx ON artifact_objects (object);                      -- the collector, the audit and the backup
CREATE INDEX artifact_objects_hash_idx ON artifact_objects (artifact_id, hash);             -- the carry lookup, within one artifact

-- artifact_assets: files a page references from the asset store, outside any
-- version. Never carried. A removal keeps the row as a tombstone with no
-- object, so the asset id stays held and its history stays readable.
CREATE TABLE artifact_assets (
    artifact_id     TEXT    NOT NULL,
    asset_id        TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    content_type    TEXT    NOT NULL,
    -- NULL once removed: a row naming no object is never a reference.
    object          TEXT,
    hash            TEXT    NOT NULL DEFAULT '',
    size            INTEGER NOT NULL DEFAULT 0,
    added_by        TEXT    NOT NULL,
    added_by_kind   TEXT    NOT NULL,
    added_at        INTEGER NOT NULL,
    removed_by      TEXT    NOT NULL DEFAULT '',
    removed_by_kind TEXT    NOT NULL DEFAULT '',
    removed_at      INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, asset_id)
);
CREATE INDEX artifact_assets_object_idx ON artifact_assets (object);                        -- the collector, the audit and the backup

-- artifact_shares: one audience's role on one artifact. A share is a
-- RELATION, never a permission.
CREATE TABLE artifact_shares (
    artifact_id     TEXT    NOT NULL,
    -- seat, unit or company; the id is the seat handle, the unit key, or
    -- empty for the company.
    audience_kind   TEXT    NOT NULL,
    audience_id     TEXT    NOT NULL,
    -- viewer, commenter, contributor or editor. Owner is never shared.
    role            TEXT    NOT NULL,
    granted_by      TEXT    NOT NULL,
    granted_by_kind TEXT    NOT NULL,
    granted_at      INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, audience_kind, audience_id)
);
CREATE INDEX artifact_shares_audience_idx ON artifact_shares (audience_kind, audience_id, artifact_id); -- what one seat, unit or the company is shared

-- artifact_pins: a person's pinned artifacts. Per-person state, replicated so
-- a listing reads the same on every node.
CREATE TABLE artifact_pins (
    seat        TEXT    NOT NULL,
    artifact_id TEXT    NOT NULL,
    pinned_at   INTEGER NOT NULL,
    PRIMARY KEY (seat, artifact_id)
);
CREATE INDEX artifact_pins_artifact_idx ON artifact_pins (artifact_id);                     -- the pins a purge drops

-- artifact_reads: when a person last opened an artifact. Monotone on broker
-- time, debounced by the API to one record per (seat, artifact) per fifteen
-- minutes, so its log cost is bounded by human browsing.
CREATE TABLE artifact_reads (
    seat        TEXT    NOT NULL,
    artifact_id TEXT    NOT NULL,
    opened_at   INTEGER NOT NULL,
    PRIMARY KEY (seat, artifact_id)
);
CREATE INDEX artifact_reads_artifact_idx ON artifact_reads (artifact_id, opened_at);        -- who has seen one artifact (D21)

-- artifact_history: what happened to an artifact, one row per record.
--
-- A REFUSED RECORD WRITES ITS ROW TOO, with outcome `refused` and the reason
-- (a carried object no longer named, a quota the stamped limits refuse, a head
-- that moved): that is how a writer whose record the broker accepted learns
-- the apply skipped it, by reading its own operation's row. Kept 365 days,
-- pruned by the applier from broker time in bounded batches.
CREATE TABLE artifact_history (
    artifact_id TEXT    NOT NULL,
    -- The record's packed position: one row per record, and the order a
    -- history page and its cursor read.
    position    INTEGER NOT NULL,
    op_id       TEXT    NOT NULL,
    op          TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    -- The version the record made or named, 0 where it names none.
    number      INTEGER NOT NULL DEFAULT 0,
    actor       TEXT    NOT NULL,
    actor_kind  TEXT    NOT NULL,
    -- The credential the actor acted through (`session:<lineage>`,
    -- `pat:<id>`), beside the actor and never instead of it.
    operator_id TEXT    NOT NULL DEFAULT '',
    -- 1 when the admin path admitted an actor with no relation (D09).
    elevated    INTEGER NOT NULL DEFAULT 0,
    origin      TEXT    NOT NULL DEFAULT '',
    work_item   TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    -- What the row says beyond its columns, as JSON: the paths a refusal
    -- names, a share's audience and role, a version's file count.
    summary     TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (artifact_id, position)
);
CREATE INDEX artifact_history_op_idx ON artifact_history (op_id);                           -- a writer reading its own verdict back
CREATE INDEX artifact_history_at_idx ON artifact_history (at, artifact_id, position);       -- the 365-day prune, in a total order

-- artifact_usage: the company's counters the stamped quotas are enforced
-- against. One row, scope 'company'. Derived by the applier, never pruned.
CREATE TABLE artifact_usage (
    scope     TEXT    NOT NULL PRIMARY KEY,
    artifacts INTEGER NOT NULL DEFAULT 0,
    bytes     INTEGER NOT NULL DEFAULT 0,
    versions  INTEGER NOT NULL DEFAULT 0,
    assets    INTEGER NOT NULL DEFAULT 0
);

-- artifacts_deletions: the permanent deletion marker.
--
-- How a node that was away tells an artifact that never existed from one that
-- was deliberately destroyed: a replay that reaches an artifact's records after
-- its purge would otherwise recreate it on that node alone.
CREATE TABLE artifacts_deletions (
    artifact_id     TEXT    NOT NULL PRIMARY KEY,
    at              INTEGER NOT NULL,
    by              TEXT    NOT NULL DEFAULT '',
    by_kind         TEXT    NOT NULL DEFAULT '',
    -- The operation id of the purge that wrote this row. The gate lets
    -- EXACTLY that record through and nothing else.
    purge_record_id TEXT    NOT NULL DEFAULT '',
    -- How many records this marker has dropped, and when it last did: a
    -- marker still catching redeliveries months later is a writer nobody
    -- stopped.
    rejects         INTEGER NOT NULL DEFAULT 0,
    last_reject_at  INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);

-- artifacts_evictions: a node's removal from THIS log, or its readmission.
-- Its own table because an eviction fences records above a position, and
-- positions on different streams are different number spaces.
CREATE TABLE artifacts_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    -- NULL until a readmission, which is an INVERSE COMMIT rather than a
    -- delete, so an evicted, readmitted and evicted again node reads
    -- correctly.
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- artifacts_log_generations: every reanchor's audit row.
CREATE TABLE artifacts_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The log's own machinery. The framework generates the statements that write
-- these, so their shape is the framework's (0001 and 0021), not this domain's.
-- ---------------------------------------------------------------------------

-- artifacts_ops: what this node's applier has written. It TRAVELS in every
-- donated snapshot and stays out of the identity claim.
CREATE TABLE artifacts_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX artifacts_ops_swept_idx ON artifacts_ops (applied_at);                         -- the ops retention sweep

-- artifacts_log_deferred: a record this build could not decode, retained byte
-- for byte at its original position.
CREATE TABLE artifacts_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX artifacts_log_deferred_subject_idx ON artifacts_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record, written in the same transaction
-- as its parent and the checkpoint advance.
CREATE TABLE artifacts_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX artifacts_log_deferred_scope_idx ON artifacts_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
