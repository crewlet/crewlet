-- The knowledge base's durable state: the read side of the pages log.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
--
-- # What this domain is, and what it replaces
--
-- Until now a page's record of truth was a coordination bucket and every node
-- kept a rebuildable PROJECTION of it in the node estate. That projection
-- could not say how far behind it was: a projector's cursor is a tail position
-- with no way to state a distance, where an applier's position is a place on a
-- log. So a read here reports the level it answered at and a caller can wait
-- for its own write, which is the whole reason the family moved.
--
-- It also removes three two-key sequences the bucket forced — a create, a save
-- and a rename each had a crash state and a grace rule for stepping over the
-- debris — because coordination has no multi-key transaction and this does. A
-- create is ONE record whose apply writes the title, the head, the first
-- revision and the history entry together.
--
-- # The three rules the schema enforces, and one it deliberately does not
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: the applier
-- removes a page's children in the same statement list, where the deletion is
-- part of the record's own effect and therefore identical on every node. That
-- is a CHANGE from the projection this replaces, whose `pages` children all
-- cascaded — there the projector was free to decide, and here it is not.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY. A constraint violation inside the apply
-- transaction aborts it deterministically on every node, which turns a rare
-- cosmetic anomaly into a fleet-wide stalled log. A title collision is the
-- worked example: it cannot happen, because the title IS the arbitration
-- subject — but a restored `pages_titles` beside newer heads could produce one,
-- and a unique index would wedge every node at once rather than flagging it.
--
-- NO `COLLATE` ANYWHERE. The default TEXT collation is BINARY, so an ORDER BY
-- reproduces the Go comparison exactly. The pinned driver ACCEPTS
-- `COLLATE NOCASE` (measured), which would silently collapse 'A' and 'a' — and
-- a title's normalisation is done in Go, once, so a collation here would be a
-- second answer to what one address is.
--
-- What it does NOT enforce is the object versions' monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because it has to be a
-- SKIP rather than an error — a redelivered record is ordinary traffic.

-- ---------------------------------------------------------------------------
-- The object tables.
--
-- Each carries `version INTEGER NOT NULL` — the COMPOSED
-- (generation << 40) | stream sequence of the commit that last arbitrated this
-- object, in the same integer space as the domain's own checkpoint — and the
-- ones with prose carry `document`, the object's full encoded state INCLUDING
-- fields this build does not know, so a rolling upgrade's newer fields
-- round-trip through a durable row.
-- ---------------------------------------------------------------------------

-- pages_containers — a space: a unit's, the org root's, the skills container.
CREATE TABLE pages_containers (
    key            TEXT    NOT NULL PRIMARY KEY,
    name           TEXT    NOT NULL DEFAULT '',
    purpose        TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    version        INTEGER NOT NULL,
    -- The highest record that wrote this row FROM ANOTHER SUBJECT — a page
    -- create inside it, which touches the container's own path. It is stamped
    -- instead of `version` so the row's broker expectation still matches its
    -- subject's last message and cannot be poisoned into permanent
    -- unwritability; a read barrier compares MAX of the two.
    scoped_through INTEGER NOT NULL DEFAULT 0,
    document       BLOB    NOT NULL
);

-- pages_heads — one row per page.
--
-- `body` is a column rather than only a document field because every read of a
-- page reads it, and decoding a 512 KiB document to answer "show me this page"
-- is the full scan wearing a column's name.
CREATE TABLE pages_heads (
    id             TEXT    NOT NULL PRIMARY KEY,
    container      TEXT    NOT NULL,
    parent_id      TEXT    NOT NULL DEFAULT '',
    -- The DISPLAYED title, keeping the author's own capitalisation, and
    -- title_norm the address it was arbitrated on. Both, because a link is
    -- resolved by the second and rendered from the first.
    title          TEXT    NOT NULL,
    title_norm     TEXT    NOT NULL,
    body           TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL,
    author         TEXT    NOT NULL DEFAULT '',
    -- The page's own monotonic edit number, which a save states and which is
    -- NOT the composed log version: a person reads "version 7" and an operator
    -- reads the log position, and one integer cannot be both.
    edit_version   INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    trashed_at     INTEGER,
    version        INTEGER NOT NULL,
    scoped_through INTEGER NOT NULL DEFAULT 0,
    document       BLOB    NOT NULL
);

-- A container's tree, and the listing a space's index page renders.
CREATE INDEX pages_heads_container_idx ON pages_heads (container, status, title);          -- a space's own listing
-- The address lookup a link resolves through, on the NORMALISED title: a
-- LOWER(title) predicate cannot use the index above, so every resolution
-- would be a scan of the container.
CREATE INDEX pages_heads_address_idx ON pages_heads (container, title_norm);               -- resolve a link by title
-- A container's page tree. Not partial: the predicate would make it unusable
-- for the query it exists for.
CREATE INDEX pages_heads_parent_idx ON pages_heads (parent_id);                            -- one page's children
-- The trashed-page sweep. Partial, so its query must restate the predicate.
CREATE INDEX pages_heads_trashed_idx ON pages_heads (trashed_at) WHERE trashed_at IS NOT NULL; -- the thirty-day trash sweep

-- pages_titles — one container's hold on one normalised title.
--
-- THE ADDRESS, and a reproducible table rather than machinery: a node whose
-- titles were not rebuilt would let a second page take a name the first
-- already holds. The primary key IS the subject the record arbitrated on,
-- which is what makes a create's race a broker race with exactly one winner.
CREATE TABLE pages_titles (
    container   TEXT    NOT NULL,
    -- The TOKEN the claim was arbitrated on: a digest of the normalised title,
    -- because a subject is a broker path and a title is prose that routinely
    -- carries a space, a dot or a wildcard. See [pages.TitleToken].
    title_token TEXT    NOT NULL,
    -- And the normalised title itself, so a listing can report what a claim
    -- holds without a reverse lookup nothing can perform: a digest has no
    -- inverse, and an operator asking "why can this name not be used" needs
    -- the name.
    title_norm  TEXT    NOT NULL,
    page_id     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    PRIMARY KEY (container, title_token)
);
-- The inverse: which titles one page has held, read by a rename's apply and by
-- the operator surface that answers "why can this name not be used".
CREATE INDEX pages_titles_page_idx ON pages_titles (page_id);                              -- a page's own claims

-- pages_revisions — one immutable body PER VERSION, kept whole.
--
-- REVISION N IS THE BODY AT VERSION N, including the newest — so the head's own
-- body appears here too. The other reading, "a revision holds what was
-- replaced", collides with itself on the first save (a create already wrote
-- version 1) and leaves the current body reachable only through the head, so
-- "open version 7" would work for every version except the one a person is
-- looking at.
--
-- The projection this replaces stored metadata only and read a body back out
-- of the bucket, because a 512 KiB body times a hundred revisions times every
-- page was an order of magnitude more than the record it copied. That trade is
-- gone with the bucket: this IS the record, so there is nowhere else to read
-- it from, and the hundred-revision cap plus the prune riding each commit is
-- what bounds it instead.
CREATE TABLE pages_revisions (
    page_id      TEXT    NOT NULL,
    edit_version INTEGER NOT NULL,
    title        TEXT    NOT NULL,
    body         TEXT    NOT NULL DEFAULT '',
    message      TEXT    NOT NULL DEFAULT '',
    author       TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    version      INTEGER NOT NULL,
    PRIMARY KEY (page_id, edit_version)
);
-- The history list, newest first.
CREATE INDEX pages_revisions_page_idx ON pages_revisions (page_id, edit_version DESC);     -- one page's revision list

-- pages_comments — one remark on a page.
CREATE TABLE pages_comments (
    id          TEXT    NOT NULL PRIMARY KEY,
    page_id     TEXT    NOT NULL,
    author      TEXT    NOT NULL DEFAULT '',
    author_kind TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    reply_to    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
CREATE INDEX pages_comments_page_idx ON pages_comments (page_id, created_at);              -- one page's comment thread

-- ---------------------------------------------------------------------------
-- The two exploded child tables. Each exists because a FILTER reads it: a
-- collection inside a document cannot be indexed, and a query that decodes
-- every row's document is a full scan wearing an index's name.
-- ---------------------------------------------------------------------------

CREATE TABLE pages_labels (
    page_id TEXT NOT NULL,
    label   TEXT NOT NULL,
    PRIMARY KEY (page_id, label)
);
CREATE INDEX pages_labels_label_idx ON pages_labels (label, page_id);                      -- every page carrying one label

CREATE TABLE pages_watchers (
    page_id TEXT    NOT NULL,
    handle  TEXT    NOT NULL,
    muted   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (page_id, handle)
);
CREATE INDEX pages_watchers_handle_idx ON pages_watchers (handle, page_id);                -- one person's watched pages

-- ---------------------------------------------------------------------------
-- The history, which is what a card and a digest render from.
-- ---------------------------------------------------------------------------

CREATE TABLE pages_history (
    id          TEXT    NOT NULL PRIMARY KEY,
    page_id     TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    actor_kind  TEXT    NOT NULL DEFAULT '',
    operator_id TEXT    NOT NULL DEFAULT '',
    comment_id  TEXT    NOT NULL DEFAULT '',
    excerpt     TEXT    NOT NULL DEFAULT '',
    turn_id     TEXT    NOT NULL DEFAULT '',
    quiet       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    -- broker_at is the RAW broker instant this entry was committed at, which is
    -- what makes the row byte-identical on every node rather than a clock each
    -- one read for itself.
    broker_at   INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
CREATE INDEX pages_history_page_idx ON pages_history (page_id, created_at DESC);           -- one page's activity
CREATE INDEX pages_history_created_idx ON pages_history (created_at);                      -- the company-wide digest

-- ---------------------------------------------------------------------------
-- Two fleet-wide tables that are the fleet's own record rather than a person's.
-- ---------------------------------------------------------------------------

-- pages_deletions — the permanent deletion marker.
--
-- How a node that was away tells a page that never existed from one that was
-- deliberately destroyed. Without it, a replay that reaches a page's records
-- after its purge would recreate it on that node alone.
CREATE TABLE pages_deletions (
    page_id         TEXT    NOT NULL PRIMARY KEY,
    container       TEXT    NOT NULL DEFAULT '',
    at              INTEGER NOT NULL,
    by              TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL DEFAULT '',
    -- The operation id of the purge that wrote this row. The gate lets EXACTLY
    -- that record through and nothing else: "any purge" would let a SECOND
    -- purge of the same page apply, and a purge is the one operation that
    -- destroys rows.
    purge_record_id TEXT    NOT NULL DEFAULT '',
    -- How many records this marker has dropped, and when it last did. An
    -- OPERATOR SURFACE rather than machinery: a page whose deletion is still
    -- catching redeliveries months later is a writer nobody stopped.
    rejects         INTEGER NOT NULL DEFAULT 0,
    last_reject_at  INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);

-- pages_evictions — a node's removal from THIS log.
--
-- ITS OWN TABLE rather than the tracker's, and the reason is the position: an
-- eviction fences records above a COMPOSED position, and positions on
-- different streams name different number spaces. Sharing one table would also
-- give two appliers one table, which is the rule the whole identity claim
-- rests on.
--
-- Keyed on the node alone here — unlike the tracker's, which carries a
-- log_stream column from when one table served two streams — because this
-- table serves exactly one log and a column with one value in every row is a
-- column a reader has to remember to filter on.
CREATE TABLE pages_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    -- NULL until a readmission, which is an INVERSE COMMIT rather than a
    -- delete: an eviction's whole history survives a replay, and a node that
    -- was evicted, readmitted and evicted again reads correctly rather than as
    -- one long absence.
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- pages_log_generations — every reanchor's audit row.
--
-- A reanchor is a committed record and therefore reproducible from the log;
-- this is what a recovery replay writes and what the operator surface reads.
CREATE TABLE pages_log_generations (
    generation             INTEGER NOT NULL PRIMARY KEY,
    at                     INTEGER NOT NULL,
    by                     TEXT    NOT NULL DEFAULT '',
    new_stream_created_at  INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen     INTEGER NOT NULL DEFAULT 0,
    record_id              TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The Divergent table: this build's own answer, travelling in a snapshot and
-- excluded from the identity claim.
-- ---------------------------------------------------------------------------

-- pages_skills — whether a page's body parses as a tool skill.
--
-- DERIVED AT APPLY TIME by THIS BUILD'S parser, which is why it is Divergent
-- rather than Replicated: a page written by a newer node must not carry a claim
-- an older node's parser disagrees with, and two builds mid-upgrade
-- legitimately differ. It travels inside a snapshot because a joining node
-- wants the answer rather than a rebuild, and it is excluded from the identity
-- claim because a rolling upgrade would otherwise report a fleet-wide
-- divergence for a difference that resolves itself on the next apply.
--
-- ITS OWN TABLE rather than a column on pages_heads, because a column would
-- put a divergent value inside a table the identity claim checksums.
CREATE TABLE pages_skills (
    page_id    TEXT    NOT NULL PRIMARY KEY,
    container  TEXT    NOT NULL,
    -- 1 when the body parses as a tool skill, 1 when the title is the
    -- onboarding page. Two flags rather than one, because the two are read by
    -- different subsystems and a page can be both.
    skill      INTEGER NOT NULL DEFAULT 0,
    onboarding INTEGER NOT NULL DEFAULT 0,
    at         INTEGER NOT NULL
);
-- The registry's re-read: every skill page in one container.
CREATE INDEX pages_skills_container_idx ON pages_skills (container, skill);                -- the tool-skill registry's re-read

-- ---------------------------------------------------------------------------
-- The log's own machinery: excluded from the identity claim, from the
-- completeness audit, and scrubbed out of every donated snapshot.
-- ---------------------------------------------------------------------------

-- pages_ops — contract 3's first layer: what this node's applier has written.
--
-- A DONOR'S OPERATION LEDGER IS THE SHARPEST THING A SNAPSHOT MUST SCRUB: an
-- adopted peer's ops table would let this node resolve its own ambiguous
-- publish against somebody else's history, which is a write reported as landed
-- that never happened.
CREATE TABLE pages_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    -- The COMPOSED (generation << 40) | sequence, so a position from before a
    -- reanchor is comparable and safely stale rather than plausible.
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
-- The sweep is a range delete over the age, and a range delete ships its index:
-- without it a node returning from a month away scans the whole table on every
-- tick, and deletes its whole month in one statement holding the only writer.
CREATE INDEX pages_ops_swept_idx ON pages_ops (applied_at);                                -- the ops retention sweep

-- pages_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE pages_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    -- The record version this build could not decode, which is the number an
    -- operator needs to pick a build.
    version      INTEGER NOT NULL,
    -- THE WHOLE MESSAGE, BYTE FOR BYTE. Lossless means the bytes: a build that
    -- can read this record must get exactly what its writer published, not
    -- what an intermediate build understood of it.
    payload      BLOB    NOT NULL,
    -- The broker's own instant, because a record reprocessed after an upgrade
    -- must contribute the instant it was COMMITTED rather than the instant it
    -- was reprocessed — and this row is the only other durable copy of it.
    stored_at    INTEGER NOT NULL
);
-- subject_id LEADS, because the per-object probe reads it ALONE: a page's own
-- save and a rename that moves it share an id under different kinds, and the
-- dependency is on the OBJECT rather than on the kind.
CREATE INDEX pages_log_deferred_subject_idx ON pages_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance — so there is no window
-- in which the position moved and the scope is unindexed.
--
-- A PATH RATHER THAN A TUPLE OF COORDINATES, because the framework's
-- containment model is a path hierarchy: it knows one thing about a scope, that
-- levels are separated left to right, and it computes coverage from that alone.
--
-- The probe is two clauses over this column and neither finds the other's case:
-- a stored path IN the query's closure is one that CONTAINS what the query is
-- about, and a stored path UNDER one of its roots is one INSIDE it.
--
-- A PLAIN ROWID TABLE: `WITHOUT ROWID` is refused by the pinned driver as an
-- experimental feature (measured), so a migration carrying it would fail on
-- every node and every store would refuse to open.
--
-- NO FOREIGN KEY: the parent's own delete removes these rows in the same
-- statement list, because a cascade is a delete nobody committed.
CREATE TABLE pages_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX pages_log_deferred_scope_idx ON pages_log_deferred_scope (path, position);    -- the read barrier's coverage probe and the writer's step 0
