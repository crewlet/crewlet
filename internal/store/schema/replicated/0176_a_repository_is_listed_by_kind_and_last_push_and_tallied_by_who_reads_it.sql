-- A listing of repositories is read by kind and by last push, and the repositories are tallied by who reads them, kind and status.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; the applier keeps the tally and re-derives it (gitlet.DerivationVersion), so this file fills nothing.
-- About 35 bytes a repository in the kind index and 45 a repository in use in the pushed one, 120 a tally row; a push record rewrites its repository's pushed entry, and a lifecycle record moves one or two tally rows.
-- Class: code_repos stays reproducible, and code_repo_tallies is reproducible (gitlet.ReproducibleTables).

CREATE INDEX code_repos_kind_idx ON code_repos (kind, name);    -- Reader.Read(repos) of one kind in name order, and a first page's total size (repolisting.go)
CREATE INDEX code_repos_pushed_idx ON code_repos (kind, last_push_at, name) WHERE status IN ('importing', 'active', 'archived');    -- Reader.Read(repos) by last push, one kind's range at a time (repolisting.go)

CREATE TABLE code_repo_tallies (
    visibility      TEXT    NOT NULL,
    owner_kind      TEXT    NOT NULL,
    owner           TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    importing_repos INTEGER NOT NULL,
    active_repos    INTEGER NOT NULL,
    archived_repos  INTEGER NOT NULL,
    deleted_repos   INTEGER NOT NULL,
    purging_repos   INTEGER NOT NULL,
    PRIMARY KEY (visibility, owner_kind, owner, kind)
);
