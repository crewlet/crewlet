-- The Dashboard Assistant's conversations land: the state log's eighth domain,
-- a person's conversations with the assistant, their turns, the messages,
-- steps and cards the person saw, and the proposals they decided.
-- internal/assistantdomain is the domain (ADR-0053).
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ASSISTANT_LOG stream by one deterministic applier. A person opens the
-- drawer on any node and a reconnect may land on another, so a conversation is
-- read from whichever data node serves the request; and an approval is decided
-- exactly once across the fleet, by an arbitrated record on the proposal's own
-- subject.
--
-- ## Sealed by the writer, never opened here
--
-- Every column named `*_sealed` holds a keyring envelope (internal/secrets'
-- AES-256-GCM under the fleet keyring) whose associated data names the
-- conversation, the record kind, the record and the column, or the empty string
-- for an empty value. The writer seals before it publishes and the applier
-- writes the bytes through, so every node holds identical rows whatever keys it
-- holds, and a value copied under another record does not open there.
--
-- ## Owner-keyed, and gone in thirty days
--
-- Every row carries the conversation it belongs to, and every read is keyed on
-- the conversation's owner. A conversation untouched for its retention is purged
-- through its own purge record (the trash reaper), and a person's erasure takes
-- every conversation they own in one record; both leave a deletion marker so a
-- record that arrives later produces no rows on any node.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY: a purge removes a conversation's rows in its own statement
-- list. NO `UNIQUE` OUTSIDE A PRIMARY KEY: a constraint violation inside the
-- apply transaction aborts it on every node at once. Every instant a row records
-- of its own is the broker's store timestamp of the record that wrote it, as
-- `store.EncodeTime` integers, and a version is a record's packed position
-- `(generation << 40) | sequence`.

-- assistant_conversations: one row per conversation.
CREATE TABLE assistant_conversations (
    id               TEXT    NOT NULL PRIMARY KEY,
    -- The person the conversation belongs to, by their identity-estate id.
    owner_id         TEXT    NOT NULL,
    title_sealed     TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    -- The broker instant of the last record about anything in the
    -- conversation: what its retention is measured from.
    last_activity_at INTEGER NOT NULL,
    -- The number the conversation's next turn takes.
    turns            INTEGER NOT NULL DEFAULT 0,
    -- The turn still running or parked, or '' for none: one at a time.
    open_turn_id     TEXT    NOT NULL DEFAULT '',
    version          INTEGER NOT NULL
);
CREATE INDEX assistant_conversations_owner_idx ON assistant_conversations (owner_id, last_activity_at, id); -- a person's conversations, newest first
CREATE INDEX assistant_conversations_activity_idx ON assistant_conversations (last_activity_at, id);      -- the retention reaper's nomination

-- assistant_turns: one row per turn.
CREATE TABLE assistant_turns (
    id                TEXT    NOT NULL PRIMARY KEY,
    conversation_id   TEXT    NOT NULL,
    owner_id          TEXT    NOT NULL,
    seq               INTEGER NOT NULL,
    -- An internal/assistant/wire TurnState.
    state             TEXT    NOT NULL,
    -- The node whose runner holds the turn now.
    node              TEXT    NOT NULL DEFAULT '',
    started_at        INTEGER NOT NULL,
    ended_at          INTEGER NOT NULL DEFAULT 0,
    prompt_version    TEXT    NOT NULL DEFAULT '',
    model             TEXT    NOT NULL DEFAULT '',
    provider_key      TEXT    NOT NULL DEFAULT '',
    steps             INTEGER NOT NULL DEFAULT 0,
    input_tokens      INTEGER NOT NULL DEFAULT 0,
    output_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_read        INTEGER NOT NULL DEFAULT 0,
    error_code        TEXT    NOT NULL DEFAULT '',
    trace_id          TEXT    NOT NULL DEFAULT '',
    page_title_sealed TEXT    NOT NULL DEFAULT '',
    page_screen       TEXT    NOT NULL DEFAULT '',
    version           INTEGER NOT NULL
);
CREATE INDEX assistant_turns_conversation_idx ON assistant_turns (conversation_id, seq); -- a conversation's turns in order

-- assistant_messages: one row per message the person sent or was shown.
-- `apps` is the JSON array of the apps the turn had read through when the
-- message was written: a reader who no longer holds one is shown a notice in
-- its place.
CREATE TABLE assistant_messages (
    id                TEXT    NOT NULL PRIMARY KEY,
    conversation_id   TEXT    NOT NULL,
    turn_id           TEXT    NOT NULL,
    -- An internal/assistant/wire MessageRole.
    role              TEXT    NOT NULL,
    text_sealed       TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    page_title_sealed TEXT    NOT NULL DEFAULT '',
    page_screen       TEXT    NOT NULL DEFAULT '',
    apps              TEXT    NOT NULL DEFAULT '[]',
    version           INTEGER NOT NULL
);
CREATE INDEX assistant_messages_conversation_idx ON assistant_messages (conversation_id, version); -- a transcript's messages in log order

-- assistant_steps: one row per tool step, its last state. The step itself
-- (its label, request, summary and refusal) is one sealed wire StepEvent.
CREATE TABLE assistant_steps (
    id              TEXT    NOT NULL PRIMARY KEY,
    conversation_id TEXT    NOT NULL,
    turn_id         TEXT    NOT NULL,
    idx             INTEGER NOT NULL,
    tool            TEXT    NOT NULL,
    -- An internal/assistant/wire StepState.
    state           TEXT    NOT NULL,
    refusal_code    TEXT    NOT NULL DEFAULT '',
    body_sealed     TEXT    NOT NULL,
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_steps_conversation_idx ON assistant_steps (conversation_id, turn_id, idx); -- a transcript's steps in order

-- assistant_cards: one row per card, its body a sealed wire Card of at most
-- 256 KiB. `apps` is the JSON array of the apps a reader must hold for the
-- card to be shown (and exported) rather than a notice.
CREATE TABLE assistant_cards (
    id              TEXT    NOT NULL PRIMARY KEY,
    conversation_id TEXT    NOT NULL,
    turn_id         TEXT    NOT NULL,
    message_id      TEXT    NOT NULL,
    -- An internal/assistant/wire CardKind.
    type            TEXT    NOT NULL,
    apps            TEXT    NOT NULL DEFAULT '[]',
    body_sealed     TEXT    NOT NULL,
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_cards_conversation_idx ON assistant_cards (conversation_id, version); -- a transcript's cards in log order

-- assistant_proposals: one row per proposal, at its state now.
CREATE TABLE assistant_proposals (
    id              TEXT    NOT NULL PRIMARY KEY,
    conversation_id TEXT    NOT NULL,
    turn_id         TEXT    NOT NULL,
    owner_id        TEXT    NOT NULL,
    tool            TEXT    NOT NULL,
    -- The act the proposal executes: the authority verb, or the page
    -- action's id.
    act             TEXT    NOT NULL DEFAULT '',
    -- An internal/assistant/wire ProposalScope.
    scope           TEXT    NOT NULL,
    method          TEXT    NOT NULL,
    path            TEXT    NOT NULL,
    query_sealed    TEXT    NOT NULL DEFAULT '',
    body_sealed     TEXT    NOT NULL DEFAULT '',
    request_digest  TEXT    NOT NULL,
    digest          TEXT    NOT NULL,
    -- The Idempotency-Key the approved request is sent with, so a Retry
    -- after an unknown outcome is the same operation.
    op_key          TEXT    NOT NULL,
    source_revision TEXT    NOT NULL DEFAULT '',
    step_up         INTEGER NOT NULL DEFAULT 0,
    card_sealed     TEXT    NOT NULL,
    -- An internal/assistant/wire ProposalState.
    state           TEXT    NOT NULL,
    expires_at      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    decided_at      INTEGER NOT NULL DEFAULT 0,
    settled_at      INTEGER NOT NULL DEFAULT 0,
    -- What the executed request answered: the state log's outcome and the
    -- position it landed at, for a server write.
    outcome         TEXT    NOT NULL DEFAULT '',
    position        TEXT    NOT NULL DEFAULT '',
    receipt_sealed  TEXT    NOT NULL DEFAULT '',
    refusal_code    TEXT    NOT NULL DEFAULT '',
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_proposals_conversation_idx ON assistant_proposals (conversation_id, version); -- a transcript's proposals

-- assistant_parked: a turn's conversation while it waits on the person, at
-- most 512 KiB before sealing, deleted when the turn closes.
CREATE TABLE assistant_parked (
    turn_id         TEXT    NOT NULL PRIMARY KEY,
    conversation_id TEXT    NOT NULL,
    messages_sealed TEXT    NOT NULL,
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_parked_conversation_idx ON assistant_parked (conversation_id); -- a purge's delete

-- assistant_audit_reads: every administrator's compliance read of a
-- conversation (decision D06), shown to its owner.
CREATE TABLE assistant_audit_reads (
    conversation_id TEXT    NOT NULL,
    at              INTEGER NOT NULL,
    -- The administrator, as the audit trail names an author.
    reader          TEXT    NOT NULL,
    reader_kind     TEXT    NOT NULL DEFAULT '',
    operator_id     TEXT    NOT NULL DEFAULT '',
    reason_sealed   TEXT    NOT NULL,
    record_id       TEXT    NOT NULL,
    PRIMARY KEY (conversation_id, at, reader)
);

-- assistant_deletions: the permanent deletion marker a conversation's purge
-- leaves, so a record about it that arrives later produces no rows.
CREATE TABLE assistant_deletions (
    conversation_id TEXT    NOT NULL PRIMARY KEY,
    owner_id        TEXT    NOT NULL,
    at              INTEGER NOT NULL,
    by              TEXT    NOT NULL DEFAULT '',
    by_kind         TEXT    NOT NULL DEFAULT '',
    -- cleared, retention or erased.
    reason          TEXT    NOT NULL,
    -- The purge's operation id: the gate lets exactly that record through.
    purge_record_id TEXT    NOT NULL,
    rejects         INTEGER NOT NULL DEFAULT 0,
    last_reject_at  INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_deletions_owner_idx ON assistant_deletions (owner_id); -- an owner's purged conversations

-- assistant_erasures: a person whose conversations their erasure took, so a
-- conversation created for them afterwards produces no rows.
CREATE TABLE assistant_erasures (
    owner_id  TEXT    NOT NULL PRIMARY KEY,
    at        INTEGER NOT NULL,
    by        TEXT    NOT NULL DEFAULT '',
    by_kind   TEXT    NOT NULL DEFAULT '',
    record_id TEXT    NOT NULL,
    version   INTEGER NOT NULL
);

-- assistant_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE assistant_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- assistant_log_generations: every reanchor's audit row.
CREATE TABLE assistant_log_generations (
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

-- assistant_ops: what this node's applier has written. It TRAVELS in every
-- donated snapshot and stays out of the identity claim.
CREATE TABLE assistant_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX assistant_ops_swept_idx ON assistant_ops (applied_at); -- the ops retention sweep

-- assistant_log_deferred: a record this build could not decode, retained byte
-- for byte at its original position.
CREATE TABLE assistant_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX assistant_log_deferred_subject_idx ON assistant_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE assistant_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX assistant_log_deferred_scope_idx ON assistant_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
