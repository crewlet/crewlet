-- An expunge drops a repository's unreachable objects in two records.
--
-- Every object a push stored stays in its repository's packs for as long as the
-- repository lives: a force-push or a deletion leaves the history behind it
-- fetchable by its id, and a leaked secret pushed and force-pushed away is
-- still in the store. An EXPUNGE drops what no ref reaches, in two records on
-- the repository's own subject, each decided by its committer:
--
--   - its BEGIN rewrites some of the repository's live packs (its inputs) into
--     packs holding what a ref reaches, which stay live, and packs holding the
--     rest, which are DROPPING: still served, so a reader already on one of
--     their objects finishes, and never a base a new push may rely on. It
--     keeps every object — the inputs' objects are all in its outputs — and it
--     moves the repository's DROP EPOCH, which every push carries the value of
--     its receiver read, so a push checked before the begin is refused rather
--     than land a ref onto an object the expunge is dropping;
--   - its COMMIT retires the dropping packs, with whatever of them a push that
--     landed before the begin made reachable again packed as a live RESCUE.
--
-- A person may ASK for an expunge — a purge of the repository's unreachable
-- objects — which records the request and nothing else; the repository's
-- maintainer serves it with as many begin and commit pairs as its live packs
-- need, and it is served once no pack of the repository dates its content
-- from before it — by what each pack holds, never by when its row was added,
-- which a rewrite moves past the request without judging anything (0090).
--
-- The rows (internal/code's apply_expunge.go writes them all):
--
--   - drop_epoch is how many begins the repository has taken. A push whose
--     receiver read another is refused `expunging`, decided nothing.
--   - expunge_id is the open expunge — a begin with no commit yet — and ''
--     for none. A second begin is refused while one is open, and so is a
--     publication of a layer, which may name no dropping pack.
--   - expunge_request is the open request's id and expunge_request_position
--     the packed position of the record that made it, '' and 0 for none: a
--     request while one is open WIDENS it to its own id and position, since
--     it asks about more — a pack pushed since, and what a ref dropped since
--     left.
--   - expunged_at is the last commit's broker instant as store.EncodeTime
--     writes it (unix microseconds), 0 for a repository never expunged, and
--     expunge_dropped_objects and expunge_dropped_bytes what it took out of
--     the repository's packs: its dropping packs less its rescue.
--
-- code_packs.state takes `dropping` beside `live`, and code_packs.kind the
-- kinds a begin and a commit write — `expunge` for a kept pack, `cruft` for a
-- dropping one, `rescue` for a commit's rescue — with no DDL: both columns are
-- TEXT the domain's closed sets name (internal/code's PackState and PackKind).
-- A dropping pack names its object as every row of code_packs does, Required,
-- until its commit moves the row into code_packs_retired for RetireGrace.
-- live_packs, live_bytes and live_objects count the live packs alone.
--
-- Each default is the value that says no expunge ever happened, which SQLite
-- also requires of a NOT NULL column added to a table.
--
-- Class: code_repos stays reproducible: every column is written only by the
-- applier, from the expunge's three records.
ALTER TABLE code_repos ADD COLUMN drop_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN expunge_id TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN expunge_request TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN expunge_request_position INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN expunged_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN expunge_dropped_objects INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN expunge_dropped_bytes INTEGER NOT NULL DEFAULT 0;

-- PARTIAL, ON THE PREDICATE THE LISTING REPEATS VERBATIM: the repositories with
-- an expunge open or a request waiting are the few a maintainer must finish or
-- serve before anything else, and the listing of them reads nothing else. What
-- it costs is the records that open or close one; no push touches it.
CREATE INDEX code_repos_expunging_idx ON code_repos (id) WHERE expunge_id != '' OR expunge_request != '';    -- Reader.Read(expunge_due): the open expunges and the requests waiting, by id

-- PARTIAL, as the compaction's listing is, on the repositories an expunge may
-- be due in: least recently expunged first — a repository never expunged (0)
-- before every other — and its cursor the last row's pair. A repository's
-- entry moves only with a commit's expunged_at and its status.
CREATE INDEX code_repos_expunge_due_idx ON code_repos (expunged_at, id) WHERE status IN ('active', 'archived');    -- the weekly expunge's listing: least recently expunged first
