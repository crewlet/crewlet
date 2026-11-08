-- The access domain lands: the state log's twelfth domain, the record of what
-- each seat was told about its own access and of every SSH certificate the
-- engine signed and command it ran. internal/accessdomain is the domain
-- (ADR-0103).
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ACCESS_LOG stream by one deterministic applier. A seat's next turn may
-- dispatch on any node and diffs its access against what it was last told, and
-- an audit is answered by whichever data node a person reaches.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY. NO `UNIQUE` OUTSIDE A PRIMARY KEY. Every instant a row records
-- of its own is the broker's store timestamp of the record that wrote it, as
-- `store.EncodeTime` integers, and a position is a record's packed position.
-- Every record is created once and never updated, but a told row's `latest`.

-- access_told: one row per turn of a seat that was told something: a change of
-- its access, or its first snapshot (`baseline`). `snapshot` and `changes` are
-- the capability snapshot and change list as the record carried them. `latest`
-- is 1 on each seat's newest row alone, its baseline for the next diff, which
-- the retention never takes; a seat's removal takes them all.
CREATE TABLE access_told (
    agent_handle  TEXT    NOT NULL,
    turn_id       TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    snapshot      TEXT    NOT NULL,
    changes       TEXT    NOT NULL DEFAULT '',
    baseline      INTEGER NOT NULL DEFAULT 0,
    revision_from TEXT    NOT NULL DEFAULT '',
    revision_to   TEXT    NOT NULL DEFAULT '',
    authors       TEXT    NOT NULL DEFAULT '',
    latest        INTEGER NOT NULL DEFAULT 0,
    at            INTEGER NOT NULL,
    position      INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, turn_id)
);
CREATE INDEX access_told_seat_idx ON access_told (agent_handle, position);              -- a seat's told records, newest first
CREATE INDEX access_told_latest_idx ON access_told (agent_handle) WHERE latest = 1;     -- a seat's baseline
CREATE INDEX access_told_expiry_idx ON access_told (at, agent_handle) WHERE latest = 0; -- the retention

-- access_seat_removals: the position a removed seat's told records were last
-- forgotten at. A told record carries the removal its decide saw, and one
-- decided before a removal it did not see writes nothing when it lands after,
-- so a turn racing its seat's removal is not told into a seat that is gone,
-- while the seat's next tenure is. It goes with the retention.
CREATE TABLE access_seat_removals (
    agent_handle TEXT    NOT NULL PRIMARY KEY,
    at_seq       INTEGER NOT NULL,
    at           INTEGER NOT NULL
);
CREATE INDEX access_seat_removals_at_idx ON access_seat_removals (at, agent_handle); -- the retention

-- access_certificates: one row per SSH certificate the engine signed, its
-- public facts alone. `host_ids` and `principals` are JSON arrays.
CREATE TABLE access_certificates (
    agent_handle      TEXT    NOT NULL,
    serial            TEXT    NOT NULL,
    key_id            TEXT    NOT NULL,
    turn_id           TEXT    NOT NULL DEFAULT '',
    held_call         TEXT    NOT NULL DEFAULT '',
    purpose           TEXT    NOT NULL,
    host_ids          TEXT    NOT NULL,
    principals        TEXT    NOT NULL,
    valid_after       INTEGER NOT NULL,
    valid_before      INTEGER NOT NULL,
    public_key_sha256 TEXT    NOT NULL,
    ca_sha256         TEXT    NOT NULL,
    node              TEXT    NOT NULL DEFAULT '',
    at                INTEGER NOT NULL,
    position          INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, serial)
);
CREATE INDEX access_certificates_position_idx ON access_certificates (position);             -- the audit, newest first
CREATE INDEX access_certificates_seat_idx ON access_certificates (agent_handle, position);    -- one seat's audit
CREATE INDEX access_certificates_expiry_idx ON access_certificates (at, agent_handle);       -- the retention

-- access_commands: one row per SSH command the engine ran, or a person denied.
-- `command_sealed` is the command text sealed under the fleet keyring by the
-- writer (accessdomain.AAD), opened only for an administrator's audit read;
-- `content_key` is the keyring key it is sealed under, which the secret store's
-- census counts. Stdin and output are kept as their sizes and digests alone, and
-- `failure` is a closed word, never the command's text or output.
CREATE TABLE access_commands (
    agent_handle       TEXT    NOT NULL,
    command_id         TEXT    NOT NULL,
    turn_id            TEXT    NOT NULL DEFAULT '',
    held_call          TEXT    NOT NULL DEFAULT '',
    host               TEXT    NOT NULL,
    login_user         TEXT    NOT NULL,
    level              TEXT    NOT NULL,
    command_sealed     TEXT    NOT NULL,
    content_key        TEXT    NOT NULL,
    stdin_bytes        INTEGER NOT NULL DEFAULT 0,
    stdin_sha256       TEXT    NOT NULL DEFAULT '',
    ran                INTEGER NOT NULL DEFAULT 0,
    exit_code          INTEGER NOT NULL DEFAULT 0,
    duration_ms        INTEGER NOT NULL DEFAULT 0,
    stdout_bytes       INTEGER NOT NULL DEFAULT 0,
    stderr_bytes       INTEGER NOT NULL DEFAULT 0,
    output_sha256      TEXT    NOT NULL DEFAULT '',
    truncated          INTEGER NOT NULL DEFAULT 0,
    approver           TEXT    NOT NULL DEFAULT '',
    decision           TEXT    NOT NULL DEFAULT '',
    certificate_serial TEXT    NOT NULL DEFAULT '',
    failure            TEXT    NOT NULL DEFAULT '',
    at                 INTEGER NOT NULL,
    position           INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, command_id)
);
CREATE INDEX access_commands_position_idx ON access_commands (position);             -- the audit, newest first
CREATE INDEX access_commands_seat_idx ON access_commands (agent_handle, position);    -- one seat's audit
CREATE INDEX access_commands_host_idx ON access_commands (host, position);            -- one host's audit
CREATE INDEX access_commands_expiry_idx ON access_commands (at, agent_handle);        -- the retention
CREATE INDEX access_commands_key_idx ON access_commands (content_key);                -- the sealed-value census

-- access_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE access_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- access_log_generations: every reanchor's audit row.
CREATE TABLE access_log_generations (
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

-- access_ops: what this node's applier has written. It TRAVELS in every donated
-- snapshot and stays out of the identity claim.
CREATE TABLE access_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX access_ops_swept_idx ON access_ops (applied_at); -- the ops retention sweep

-- access_log_deferred: a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE access_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX access_log_deferred_subject_idx ON access_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE access_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX access_log_deferred_scope_idx ON access_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
