-- A check is keyed by the branch it is checked for.
--
-- An attempt of a check was keyed (repo_id, commit_oid, name, attempt): by
-- the commit alone. But what a check of a commit measures is the definition
-- its TARGET's checks file gives it — the file read at the target's tip, never
-- the commit's own, so no change narrows the checks that judge it — and its
-- target's protection is what requires it. One commit checked for two targets
-- is two checks: a head another change into another branch shares, a tip of
-- one branch that a change into another names as its head. Keyed by the
-- commit, the second target's dispatch found the first's attempts and queued
-- nothing — its own file's definition never ran, and its change's checks,
-- read by the change's id off attempts carrying the first change's, read
-- empty — and a change into a branch whose file defined a check trivially
-- could pass, for every change sharing its head, the check a stricter branch
-- requires.
--
-- So code_checks and code_check_logs are keyed (repo_id, commit_oid,
-- target_ref, name, attempt): target_ref is the branch the commit is checked
-- for, a ref, as the attempt's queue already named it. Two changes into ONE
-- branch with one head still share their attempts — the same commit judged by
-- the same branch's file — and a change's checks are read by its head and its
-- target on the key, whichever change's dispatch queued them.
--
-- code_checks_change_idx is gone: a listing of the attempts a change's id
-- queued read nothing of the attempts another change into the same branch
-- queued at a head both share, so no read of a change's checks uses it, and
-- an index nothing reads is a write cost on every check record of every node.
-- It is dropped by name first — dropping the table would take it anyway — so
-- the file says where it went. Every other index carries target_ref in the
-- key's order. overdue_at (replicated 0106) is declared before the wide
-- columns, as row_bytes is, and with NO DEFAULT: the applier writes every
-- column of a row whole, so no row reads as never overdue by omission — 0106's
-- default was only SQLite's price for adding a NOT NULL column.
--
-- THE TABLES ARE CREATED AGAIN, EMPTY, RATHER THAN ALTERED: a primary key is
-- no column ALTER TABLE adds. No release carries the code domain (the
-- domain's stated condition, code.RecordVersion), so no store anywhere holds a
-- row of 0098 outside a development branch, and no build of this one reads a
-- row keyed by the commit alone. 0098 and 0106 ARE NOT EDITED:
-- `schema_migrations` keys on the filename, so a store that applied them
-- would never see the change.
--
-- Class: code_checks and code_check_logs stay reproducible: written only by
-- the applier, from the records on a repository's checks subject, and removed
-- only by those records' horizon, by the code-retention duty's checks_sweep
-- records on the same subject, and by the repository's purge.
DROP INDEX code_checks_change_idx;

DROP TABLE code_check_logs;

DROP TABLE code_checks;

CREATE TABLE code_checks (
    repo_id           TEXT    NOT NULL,
    commit_oid        TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL,
    name              TEXT    NOT NULL,
    attempt           INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    purpose           TEXT    NOT NULL,
    change_id         TEXT    NOT NULL,
    revision          INTEGER NOT NULL,
    candidate_id      TEXT    NOT NULL,
    entry_id          TEXT    NOT NULL,
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
    overdue_at        INTEGER NOT NULL,
    url               TEXT    NOT NULL,
    summary           TEXT    NOT NULL,
    def               TEXT    NOT NULL,
    failures          TEXT    NOT NULL,
    tail              TEXT    NOT NULL,
    PRIMARY KEY (repo_id, commit_oid, target_ref, name, attempt)
);

CREATE TABLE code_check_logs (
    repo_id    TEXT    NOT NULL,
    commit_oid TEXT    NOT NULL,
    target_ref TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    attempt    INTEGER NOT NULL,
    object     TEXT    NOT NULL,
    hash       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, commit_oid, target_ref, name, attempt)
);

CREATE INDEX code_checks_queued_idx ON code_checks (queued_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE state = 'queued';    -- the attempts waiting for a runner, oldest queued first: a runner's claim walk

CREATE INDEX code_checks_running_idx ON code_checks (repo_id, runner_node)
    WHERE state = 'running';    -- a repository's running attempts by runner: the home's sweep of a lost runner's

CREATE INDEX code_checks_finished_idx ON code_checks (repo_id, finished_at, commit_oid, target_ref, name, attempt, row_bytes);    -- a repository's attempts by their ending, oldest first, with their widths: the horizon, a sweep, a purge part, the purge's count, the repository's checks listed

CREATE INDEX code_checks_recent_idx ON code_checks (finished_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE finished_at > 0;    -- every repository's ended attempts by their ending: a window's checks, and the retention duty's listing of what outlives the history

CREATE INDEX code_checks_overdue_idx ON code_checks (overdue_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE state = 'queued' AND overdue_at > 0;    -- the queued attempts by when each falls overdue: the backlog alarm's walk

CREATE INDEX code_check_logs_object_idx ON code_check_logs (object);    -- the collector's question of a batch of objects, the audit's and a backup's walk
