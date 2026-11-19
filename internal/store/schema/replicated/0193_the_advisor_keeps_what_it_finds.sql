-- The advisor domain lands: the state log's fourteenth domain, the record of
-- what the Advisor found and what became of it (each finding's episodes and
-- their history, the runs, the weekly reports, the alarm episodes).
-- internal/advisordomain is the domain.
--
-- WHO HAS TO AGREE ON IT: the whole company, with history, readable from any
-- node, derived from the CREWLET_ADVISOR_LOG stream by one deterministic
-- applier on every data node. The Advisor's duty writes it from one node and
-- people's decisions from any node.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY. NO `UNIQUE` OUTSIDE A PRIMARY KEY. Every instant a row records
-- is the broker's store timestamp of the record that wrote it or an instant the
-- record's payload names, as `store.EncodeTime` integers (0 for one never set),
-- and a position is a record's packed position. No REAL column. The applier
-- stores every word as text and judges no body: the writer judged it.

-- advisor_keys: one row per finding key (class, `:`, subject key), by its id
-- (a name-based UUID of the key). `version` is the position of the newest
-- record on the key's subject, the tag a write names; `episodes` how many
-- episodes it opened; `current_episode` its open or applied one, NULL when
-- none. A hold set by a dismissal keeps the key quiet until `hold_until` (0 for
-- good, NULL for no hold) unless a graver episode than `hold_severity` opens;
-- `hold_episode` is the dismissed episode. `ineffective` is every fix, by
-- digest, that did not take effect, as a JSON array.
CREATE TABLE advisor_keys (
    key_id          TEXT    NOT NULL PRIMARY KEY,
    key             TEXT    NOT NULL,
    version         INTEGER NOT NULL,
    episodes        INTEGER NOT NULL DEFAULT 0,
    current_episode TEXT,
    hold_until      INTEGER,
    hold_reason     TEXT,
    hold_severity   TEXT,
    hold_episode    TEXT,
    ineffective     TEXT    NOT NULL DEFAULT '[]'
);

-- advisor_findings: one row per episode, its current revision. `closed_at` is
-- 0 while it is open or applied. `seen_before` is how many episodes of its key
-- opened before it. `explanation` is the model's words for the verdict
-- `explained_digest` names. `body` is the advisor's finding as JSON, opaque
-- here. `position` is the newest record that wrote the row.
CREATE TABLE advisor_findings (
    id               TEXT    NOT NULL PRIMARY KEY,
    key_id           TEXT    NOT NULL,
    key              TEXT    NOT NULL,
    class            TEXT    NOT NULL,
    source           TEXT    NOT NULL,
    actor            TEXT    NOT NULL,
    area             TEXT    NOT NULL,
    severity         TEXT    NOT NULL,
    subject_kind     TEXT    NOT NULL,
    subject_id       TEXT    NOT NULL,
    state            TEXT    NOT NULL,
    close_reason     TEXT    NOT NULL DEFAULT '',
    revision         INTEGER NOT NULL,
    verdict_digest   TEXT    NOT NULL,
    fix_digest       TEXT    NOT NULL,
    impact_minutes   INTEGER,
    impact_tokens    INTEGER,
    opened_at        INTEGER NOT NULL,
    revised_at       INTEGER NOT NULL,
    facts_at         INTEGER NOT NULL,
    closed_at        INTEGER NOT NULL DEFAULT 0,
    snoozed_until    INTEGER NOT NULL DEFAULT 0,
    effect_due_at    INTEGER NOT NULL DEFAULT 0,
    applied_fix      TEXT    NOT NULL DEFAULT '',
    applied_at       INTEGER NOT NULL DEFAULT 0,
    applied_by       TEXT    NOT NULL DEFAULT '',
    ineffective_fix  TEXT    NOT NULL DEFAULT '',
    ineffective_at   INTEGER NOT NULL DEFAULT 0,
    seen_before      INTEGER NOT NULL DEFAULT 0,
    useful_by        TEXT    NOT NULL DEFAULT '',
    useful_at        INTEGER NOT NULL DEFAULT 0,
    explanation      TEXT    NOT NULL DEFAULT '',
    explained_digest TEXT    NOT NULL DEFAULT '',
    body             TEXT    NOT NULL,
    position         INTEGER NOT NULL
);
CREATE INDEX advisor_findings_list_idx ON advisor_findings (state, severity, opened_at, id);    -- the list
CREATE INDEX advisor_findings_opened_idx ON advisor_findings (opened_at, id);                  -- the list, newest first
CREATE INDEX advisor_findings_key_idx ON advisor_findings (key_id, opened_at, id);             -- a key's episodes
CREATE INDEX advisor_findings_closed_idx ON advisor_findings (closed_at) WHERE closed_at != 0; -- the retention

-- advisor_finding_history: one line per record that moved an episode: when,
-- what (`event`), who (`by_kind`, `by_name`: the engine or a person) and the
-- facts that moved, as a JSON array.
CREATE TABLE advisor_finding_history (
    finding_id TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    at         INTEGER NOT NULL,
    event      TEXT    NOT NULL,
    by_kind    TEXT    NOT NULL DEFAULT '',
    by_name    TEXT    NOT NULL DEFAULT '',
    moved      TEXT    NOT NULL DEFAULT '[]',
    PRIMARY KEY (finding_id, position)
);

-- advisor_runs: one row per run, created by a person's request
-- (`requested_at`) or by its record, which fills `status` (empty while only
-- requested), when it started and ended, and its per-class summary and model
-- pass as JSON.
CREATE TABLE advisor_runs (
    run_id       TEXT    NOT NULL PRIMARY KEY,
    trigger_kind TEXT    NOT NULL DEFAULT '',
    by_name      TEXT    NOT NULL DEFAULT '',
    requested_at INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL DEFAULT 0,
    ended_at     INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT '',
    classes      TEXT    NOT NULL DEFAULT '[]',
    model        TEXT    NOT NULL DEFAULT 'null',
    position     INTEGER NOT NULL
);
CREATE INDEX advisor_runs_ended_idx ON advisor_runs (ended_at, run_id) WHERE status != '';        -- the latest run
CREATE INDEX advisor_runs_pending_idx ON advisor_runs (requested_at, run_id) WHERE status = '';   -- the requests no run ended

-- advisor_class_runs: per class, when its newest run whose class status was
-- done ended, which a finding's freshness is judged by.
CREATE TABLE advisor_class_runs (
    class    TEXT    NOT NULL PRIMARY KEY,
    ended_at INTEGER NOT NULL,
    position INTEGER NOT NULL
);

-- advisor_reports: one row per ISO week's report, written once.
CREATE TABLE advisor_reports (
    week        TEXT    NOT NULL PRIMARY KEY,
    period_from INTEGER NOT NULL,
    period_to   INTEGER NOT NULL,
    written_at  INTEGER NOT NULL,
    body        TEXT    NOT NULL,
    position    INTEGER NOT NULL
);
CREATE INDEX advisor_reports_written_idx ON advisor_reports (written_at); -- the retention

-- advisor_alarm_episodes: one row per episode of an alarm series (a node, an
-- alarm's kind and its instance), from its raise to its clear (NULL while
-- open). A series has at most one open episode.
CREATE TABLE advisor_alarm_episodes (
    series_id        TEXT    NOT NULL,
    raised_position  INTEGER NOT NULL,
    node             TEXT    NOT NULL,
    kind             TEXT    NOT NULL,
    instance         TEXT    NOT NULL DEFAULT '',
    raised_at        INTEGER NOT NULL,
    cleared_at       INTEGER,
    cleared_position INTEGER,
    PRIMARY KEY (series_id, raised_position)
);
CREATE INDEX advisor_alarm_episodes_open_idx ON advisor_alarm_episodes (series_id)
    WHERE cleared_position IS NULL;                                                          -- a series' open episode
CREATE INDEX advisor_alarm_episodes_cleared_idx ON advisor_alarm_episodes (cleared_at);     -- the read since an instant, and the retention

-- advisor_expiries: one row per UTC day the retention swept, so a day is swept
-- once; kept as long as a run.
CREATE TABLE advisor_expiries (
    day      TEXT    NOT NULL PRIMARY KEY,
    at       INTEGER NOT NULL,
    position INTEGER NOT NULL
);

-- advisor_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE advisor_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- advisor_log_generations: every reanchor's audit row.
CREATE TABLE advisor_log_generations (
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

-- advisor_ops: what this node's applier has written. It TRAVELS in every
-- donated snapshot and stays out of the identity claim.
CREATE TABLE advisor_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX advisor_ops_swept_idx ON advisor_ops (applied_at); -- the ops retention sweep

-- advisor_log_deferred: a record this build could not decode, retained byte
-- for byte at its original position.
CREATE TABLE advisor_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX advisor_log_deferred_subject_idx ON advisor_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE advisor_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX advisor_log_deferred_scope_idx ON advisor_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
