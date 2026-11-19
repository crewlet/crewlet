-- An item in progress records each change of its flow state, and each notice that told a person it is not moving (ADR-0162).
-- WHO HAS TO AGREE ON IT: every data node, identically; written only by the tracker's applier from a `flow` record (worklet.KindFlow, record version 20), which a build before it retains rather than applies.
-- One row per record, keyed on the record's derived key, so a fact the flow duty published twice lands once. A change of state tells nobody (`notice` and `told` empty); a notice names the role it told in and the person. `since` is the instant the facts give for the state (the start, the last change, the hold's start) and NULL where they give none; `recorded_at` is the broker's instant for the record, what a blocked item's `since` reads.
-- Never a row of the item's own: recording a state moves neither the item's version, its history nor its `updated_at`, which the flow is read from.
CREATE TABLE tracker_flow (
    id             TEXT    NOT NULL PRIMARY KEY,
    task_id        TEXT    NOT NULL,
    state          TEXT    NOT NULL,
    cause          TEXT    NOT NULL DEFAULT '',
    since          INTEGER,
    until          INTEGER,
    seat           TEXT    NOT NULL DEFAULT '',
    notice         TEXT    NOT NULL DEFAULT '',
    told           TEXT    NOT NULL DEFAULT '',
    backfilled     INTEGER NOT NULL DEFAULT 0,
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    recorded_at    INTEGER NOT NULL
);
CREATE INDEX tracker_flow_task_idx ON tracker_flow (task_id, log_seq DESC);                                    -- an item's latest change and the notices since, its flow history, and a purge's reach
-- The flow duty reads every item in progress each minute, a page at a time in id order: the items in progress are a sliver of every item a company has filed.
CREATE INDEX tracker_tasks_in_progress_idx ON tracker_tasks (id) WHERE status = 'in_progress' AND removed_at IS NULL; -- the flow duty's pages
