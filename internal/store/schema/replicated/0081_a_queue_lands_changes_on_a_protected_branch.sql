-- A queue lands changes on a protected branch.
--
-- A branch whose merge gate requires a check is a QUEUE TARGET: its merges land
-- through its queue — pre_land, the speculative queue testing each change merged
-- on the branch before it lands, or post_land, a lane landing changes ahead of
-- the checks the branch's tip then runs (internal/code's queue.go). Every row
-- below is written by records on the REPOSITORY's own subject, the one its
-- pushes and its changes' lifecycles are on, so a change's queue entry, its
-- merge and its close are decided by one writer and arbitrated together: a
-- change is never both queued and closed.
--
-- code_queue is one row an ENTRY — one enqueue of one change — keyed
-- (repo_id, entry_id), entry_id derived from the enqueue's operation id, so an
-- enqueue asked again under it is the same entry.
--   - target_ref, change_id and head_oid are what was enqueued: the change, the
--     branch it lands on and the head it was judged at; seq is the packed
--     position of the record that enqueued it, its place in its target's queue.
--   - method, delete_source, close_task and auto_revert are how it lands: the
--     merge's method (merge, squash, fast_forward), whether the landing deletes
--     its source branch and closes its work item, and the enqueuer's consent to
--     a revert of a landing that breaks the branch.
--   - enqueued_by, enqueued_kind and enqueued_operator are who enqueued it: a
--     seat's handle or a person's human seat, the party's kind, and the
--     credential a person acted through (iam.ActorFor's).
--   - state is where it stands: waiting, testing (in a candidate), landed,
--     ejected or dequeued; train and candidate_id the train and the candidate
--     testing it, commit_oid the commit a candidate built of it — each '' until
--     a candidate takes it.
--   - enqueued_at is the enqueue's broker instant; ended_at and ended_position
--     the instant and the packed position of the record that ended it (0
--     while it is live), end_reason why — a dequeue's (asked, closed, drafted,
--     retargeted, merged) or an ejection's (check_failed, conflict, gate_unmet,
--     head_moved, not_fast_forward) — and ejected the ejection's detail (its
--     checks and paths, JSON, '' for none).
--
-- code_candidates is one row a CANDIDATE a pre_land queue builds: a train's
-- ordinal'th merge of its entries onto base_oid, tip_oid its result, entries
-- its entries and the commit each became (JSON), pack_id the pack its objects
-- were stored in (a code_packs row), state where it stands, position the
-- record that wrote it.
--
-- code_landings is one row a LANDING on a protected branch — a queue's, a
-- lane's, a direct merge's or a push's — keyed (repo_id, target_ref, position,
-- ordinal): the record that landed it and its place in that record. kind says
-- which; change_id and entry_id what landed ('' for a push that landed no
-- change); base_oid and commit_oid the branch before and after; enqueued_by and
-- merged_by who; green and broken what the branch's tip checks found of it; at
-- its broker instant.
--
-- code_green is one row a protected branch: its newest commit whose required
-- checks passed, commit_oid at position — what a mirror and a deploy follow.
--
-- # The indexes
--
-- code_queue_live_idx is a target's live entries in queue order, partial on
-- the live states: the queue read, an entry's place in it, the home's due
-- listing and its queue worker.
--
-- code_queue_change_idx is a change's entries in queue order: its readiness's
-- queue term, the change's own page, and the applies that end its live entry
-- when the change closes, turns draft, is retargeted, merges or moves its
-- head.
--
-- code_queue_ended_idx is a repository's ended entries by when they ended,
-- partial on the ended states: the queue's horizon.
--
-- code_candidates_train_idx is a target's train's candidates in order: the
-- queue read's trains, and the queue worker's chain.
--
-- code_landings_change_idx is a change's landings, partial on a landing that
-- names one: a broken landing's authors, and the change's page.
--
-- A target's landings are read above its green by their key, and its green
-- row by its own: neither takes an index of its own.
--
-- Class: code_queue, code_candidates, code_landings and code_green are
-- reproducible: written only by the applier, from the records on a repository's
-- own subject, and removed only by those records and by the repository's purge.

CREATE TABLE code_queue (
    repo_id           TEXT    NOT NULL,
    entry_id          TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL,
    change_id         TEXT    NOT NULL,
    seq               INTEGER NOT NULL,
    head_oid          TEXT    NOT NULL,
    method            TEXT    NOT NULL,
    delete_source     INTEGER NOT NULL,
    close_task        INTEGER NOT NULL,
    auto_revert       INTEGER NOT NULL,
    enqueued_by       TEXT    NOT NULL,
    enqueued_kind     TEXT    NOT NULL,
    enqueued_operator TEXT    NOT NULL,
    state             TEXT    NOT NULL,
    train             TEXT    NOT NULL DEFAULT '',
    candidate_id      TEXT    NOT NULL DEFAULT '',
    commit_oid        TEXT    NOT NULL DEFAULT '',
    enqueued_at       INTEGER NOT NULL,
    ended_at          INTEGER NOT NULL DEFAULT 0,
    ended_position    INTEGER NOT NULL DEFAULT 0,
    end_reason        TEXT    NOT NULL DEFAULT '',
    ejected           TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (repo_id, entry_id)
);

CREATE INDEX code_queue_live_idx ON code_queue (repo_id, target_ref, seq)
    WHERE state IN ('waiting', 'testing');    -- a target's live entries in order: the queue read, an entry's place, the home's due listing

CREATE INDEX code_queue_change_idx ON code_queue (change_id, seq);    -- a change's entries: its readiness, its page, the applies that end its live entry

CREATE INDEX code_queue_ended_idx ON code_queue (repo_id, ended_at)
    WHERE state IN ('landed', 'ejected', 'dequeued');    -- a repository's ended entries by their ending: the queue's horizon

CREATE TABLE code_candidates (
    repo_id      TEXT    NOT NULL,
    candidate_id TEXT    NOT NULL,
    target_ref   TEXT    NOT NULL,
    train        TEXT    NOT NULL,
    ordinal      INTEGER NOT NULL,
    base_oid     TEXT    NOT NULL,
    tip_oid      TEXT    NOT NULL,
    pack_id      TEXT    NOT NULL DEFAULT '',
    state        TEXT    NOT NULL,
    position     INTEGER NOT NULL,
    entries      TEXT    NOT NULL,
    PRIMARY KEY (repo_id, candidate_id)
);

CREATE INDEX code_candidates_train_idx ON code_candidates (repo_id, target_ref, train, ordinal);    -- a target's train in order: the queue read's trains, the worker's chain

CREATE TABLE code_landings (
    repo_id     TEXT    NOT NULL,
    target_ref  TEXT    NOT NULL,
    position    INTEGER NOT NULL,
    ordinal     INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    change_id   TEXT    NOT NULL DEFAULT '',
    entry_id    TEXT    NOT NULL DEFAULT '',
    base_oid    TEXT    NOT NULL,
    commit_oid  TEXT    NOT NULL,
    enqueued_by TEXT    NOT NULL DEFAULT '',
    merged_by   TEXT    NOT NULL DEFAULT '',
    green       INTEGER NOT NULL DEFAULT 0,
    broken      INTEGER NOT NULL DEFAULT 0,
    at          INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target_ref, position, ordinal)
);

CREATE INDEX code_landings_change_idx ON code_landings (change_id)
    WHERE change_id != '';    -- a change's landings: a broken landing's authors, the change's page

CREATE TABLE code_green (
    repo_id    TEXT    NOT NULL,
    target_ref TEXT    NOT NULL,
    commit_oid TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    at         INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target_ref)
);
