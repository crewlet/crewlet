-- A check is recorded on its repository's log.
--
-- What became of a check — one attempt of one check (a name of the
-- repository's .crewlet/checks.yaml) of one commit: queued, claimed by a
-- runner, ended — is state on the code log like everything a repository holds,
-- written by records on a subject of the repository's CHECKS
-- (crewlet.code.log.checks.<repo>), never on its repository's own: a check's
-- records share nothing with the repository's pushes and would wait behind
-- their batches in its committer's queue. The subject is ADDITIVE — its records
-- carry no expectation — and every apply is a JOIN over the row it is about, so
-- two records in either order leave one row (internal/code's checks.go): the
-- state only rises, a running tie keeps the lower (started_at, runner_node), an
-- ending's tie keeps a measurement over infrastructure's.
--
-- code_checks is one row an attempt, keyed (repo_id, commit_oid, name,
-- attempt): commit_oid is the commit checked, as code_findings names one,
-- and never `oid`, which is the rowid's own alias — the pinned driver reads
-- `excluded.oid` in an upsert as the rowid (store's
-- TestNoColumnIsNamedForTheRowid).
--   - The QUEUE — purpose (head, candidate, tip, bisect), change_id and revision
--     for a change's head, candidate_id and entry_id for a queue's candidate,
--     target_ref, base_oid (what an API-first check's diff is read against),
--     defs_oid (the checks file the definition came from), required, and the
--     definition's run_in and timeout_seconds beside the definition whole
--     (def, its JSON) — is the queuing record's with the lowest position
--     (queued_position), its instant queued_at; empty, 0 and '' for an attempt
--     only a person's CI reported.
--   - The RUN — runner_node, placement (container, e2b, external), box_handle
--     and borrowed — is the claim with the lower (started_at, runner_node); 0
--     and '' until a claim arrives.
--   - The ENDING — state's ending, reason, exit_code (NULL for none),
--     duration_ms, summary, tail, failures (JSON), failures_omitted, url and
--     reporter (the human seat of the person whose CI reported it) — is the
--     measurement over infrastructure's, then the earlier (finished_at,
--     reporter), then the lower finished_position; 0 and '' until one arrives.
--   - state is DERIVED, never joined on its own: an ending's once one arrived,
--     running once a claim did, queued otherwise.
--   - row_bytes is what the row's wide columns weigh — its definition, url,
--     summary, tail and failures — read by the horizon and a purge part off
--     code_checks_finished_idx, so neither reads a wide row to learn what
--     deleting it costs (0089's rule for a change's body). THE WIDE COLUMNS ARE
--     DECLARED LAST, so a row read for a narrow column stops at its leaf page.
--
-- code_check_logs is the log a failed run kept: one row an attempt whose ending
-- named one, the object it is stored as (object, hash, size — declared Required
-- in internal/objstore/references, internal/code's CheckLogReferences), moved
-- with the ending the join keeps.
--
-- # The indexes
--
-- code_checks_queued_idx is the attempts waiting for a runner, oldest first,
-- partial on the queued state: a runner's claim walk, and the backlog's oldest.
--
-- code_checks_running_idx is a repository's running attempts by runner,
-- partial on the running state: the home's sweep of the attempts whose runner
-- left.
--
-- code_checks_change_idx is a change's attempts by commit, name and attempt:
-- what a change's readiness, its checks and its page read.
--
-- code_checks_finished_idx is a repository's attempts by when they ended,
-- oldest first, carrying each row's width: the horizon an ending and a sweep
-- remove past CheckHistory, a purge part's cut, the purge's count — each a
-- covering read that weighs a row without reading it — and the repository's
-- checks listed.
--
-- code_checks_recent_idx is every repository's ended attempts by when they
-- ended, partial on an ending: the checks of every repository in a window, and
-- the code-retention duty's listing of the repositories whose rows outlive the
-- history.
--
-- code_check_logs_object_idx is the logs by object: the collector's question of
-- a batch of objects, and the audit's and a backup's walk.
--
-- Class: code_checks and code_check_logs are reproducible: written only by the
-- applier, from the records on a repository's checks subject, and removed only
-- by those records' horizon, by the code-retention duty's checks_sweep records
-- on the same subject, and by the repository's purge.
CREATE TABLE code_checks (
    repo_id           TEXT    NOT NULL,
    commit_oid        TEXT    NOT NULL,
    name              TEXT    NOT NULL,
    attempt           INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    purpose           TEXT    NOT NULL,
    change_id         TEXT    NOT NULL,
    revision          INTEGER NOT NULL,
    candidate_id      TEXT    NOT NULL,
    entry_id          TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL,
    base_oid          TEXT    NOT NULL,
    defs_oid          TEXT    NOT NULL,
    required          INTEGER NOT NULL,
    run_in            TEXT    NOT NULL,
    timeout_seconds   INTEGER NOT NULL,
    reason            TEXT    NOT NULL,
    runner_node       TEXT    NOT NULL,
    placement         TEXT    NOT NULL,
    box_handle        TEXT    NOT NULL,
    borrowed          INTEGER NOT NULL,
    reporter          TEXT    NOT NULL,
    queued_at         INTEGER NOT NULL,
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER NOT NULL,
    duration_ms       INTEGER NOT NULL,
    exit_code         INTEGER,
    failures_omitted  INTEGER NOT NULL,
    queued_position   INTEGER NOT NULL,
    finished_position INTEGER NOT NULL,
    row_bytes         INTEGER NOT NULL,
    url               TEXT    NOT NULL,
    summary           TEXT    NOT NULL,
    def               TEXT    NOT NULL,
    failures          TEXT    NOT NULL,
    tail              TEXT    NOT NULL,
    PRIMARY KEY (repo_id, commit_oid, name, attempt)
);

CREATE TABLE code_check_logs (
    repo_id    TEXT    NOT NULL,
    commit_oid TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    attempt    INTEGER NOT NULL,
    object     TEXT    NOT NULL,
    hash       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, commit_oid, name, attempt)
);

CREATE INDEX code_checks_queued_idx ON code_checks (queued_at, repo_id, commit_oid, name, attempt)
    WHERE state = 'queued';    -- the attempts waiting for a runner, oldest first: a runner's claim walk, the backlog's oldest

CREATE INDEX code_checks_running_idx ON code_checks (repo_id, runner_node)
    WHERE state = 'running';    -- a repository's running attempts by runner: the home's sweep of a lost runner's

CREATE INDEX code_checks_change_idx ON code_checks (change_id, commit_oid, name, attempt);    -- a change's attempts: its readiness, its checks, its page

CREATE INDEX code_checks_finished_idx ON code_checks (repo_id, finished_at, commit_oid, name, attempt, row_bytes);    -- a repository's attempts by their ending, oldest first, with their widths: the horizon, a sweep, a purge part, the purge's count

CREATE INDEX code_checks_recent_idx ON code_checks (finished_at, repo_id, commit_oid, name, attempt)
    WHERE finished_at > 0;    -- every repository's ended attempts by their ending: a window's checks, and the retention duty's listing of what outlives the history

CREATE INDEX code_check_logs_object_idx ON code_check_logs (object);    -- the collector's question of a batch of objects, the audit's and a backup's walk
