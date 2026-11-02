-- A repository is made by a tool.
--
-- A repository was code.repos' alone. A person, a unit's lead and a seat now
-- make, rename, archive, delete and restore repositories through tools
-- (code.LifecycleRequest), and every row says what the repository is FOR and
-- who made it:
--
--   - kind: `project` — declared, or created by a person or the owning unit's
--     lead; it never expires — or `scratch`, a seat's own, named
--     <handle>.<slug> after it, deleted once it has been idle
--     code.scratch.idle_days. The kind follows the name (gitref), and a rename
--     keeps its kind's form;
--   - created_by and created_by_kind: who made it — a seat's handle and
--     `agent`, a person's seat and `operator` (the credential is on the
--     record), or nobody and `config` for a declaration;
--   - last_activity_at: the broker's instant of the repository's last push,
--     rename, settings change or restore — its IDLE CLOCK, which the scratch
--     expiry reads. Never a read, and never a live turn's hold, which keeps a
--     repository while the turn lasts and moves no clock.
--
-- And a renamed repository's old name keeps redirecting to it until
-- code_names.redirect_until (code.RedirectWindow, a month after the rename),
-- when the code-retention pass releases it; NULL is a name a repository holds
-- as its own.
--
-- EVERY DEFAULT EXISTS BECAUSE NOT NULL REQUIRES ONE: every create writes kind,
-- created_by, created_by_kind and last_activity_at. kind's is no kind at all,
-- for the reason 0086's via is no way: a default that is a domain value would
-- hide an apply that forgot the column.
--
-- code_repo_kinds is how many repositories of each kind the log holds, every
-- status short of purged — a create counts its repository in, the purge that
-- removes its row counts it out — which a tool's create of a PROJECT is held
-- to (code.LifecyclePolicy.MaxRepos, refused repo_limit). A COUNTER RATHER
-- THAN AN INDEX, the repository row's own idiom and code_seat_days': a count
-- over a partial index of the project rows reads every entry of it — ten
-- thousand at the bound — inside the create's snapshot, where the counter is
-- one row read by its key.
--
-- THREE INDEXES, each its reader's:
--   - code_repos_scratch_idx: a scratch create's count of what its seat holds,
--     every status short of purged (code.scratchCountSQL) — a seek on the
--     owner, at most code.scratch.max_per_seat entries;
--   - code_repos_idle_idx: the scratch expiry's listing of the active scratch
--     repositories idle longest first, keyset on (last_activity_at, id)
--     (code.idleScratchSQL);
--   - code_names_redirect_idx: the release's listing of the redirects whose
--     window has ended, oldest first (code.redirectsDueSQL).
--
-- Class: code_repos and code_names stay reproducible: every column is written
-- only from the records the applier applies; an index carries no rows of its
-- own. code_repo_kinds is reproducible (code.ReproducibleTables): written only
-- by the create and the purge applies, from those records, so a replay from
-- zero rebuilds it.
ALTER TABLE code_repos ADD COLUMN kind             TEXT    NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN created_by       TEXT    NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN created_by_kind  TEXT    NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN last_activity_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX code_repos_scratch_idx ON code_repos (owner, status) WHERE kind = 'scratch';                          -- a scratch create's per-seat count
CREATE INDEX code_repos_idle_idx ON code_repos (last_activity_at, id) WHERE kind = 'scratch' AND status = 'active'; -- the scratch idle expiry, oldest first
ALTER TABLE code_names ADD COLUMN redirect_until INTEGER;
CREATE INDEX code_names_redirect_idx ON code_names (redirect_until, token) WHERE redirect_until IS NOT NULL;      -- the release_name listing, oldest first

CREATE TABLE code_repo_kinds (
    kind  TEXT    NOT NULL PRIMARY KEY,   -- code.RepoKind
    repos INTEGER NOT NULL                -- its repositories, every status short of purged
);
