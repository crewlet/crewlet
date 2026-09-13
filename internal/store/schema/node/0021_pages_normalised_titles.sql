-- A page's TITLE IS ITS ADDRESS, and this makes the projection agree with the
-- record about what that address is.
--
-- WHAT WAS WRONG
--
-- A title is claimed fleet-wide on a key carrying its NORMALISED form —
-- lowercased and whitespace-collapsed — which is what makes "Deploy Runbook"
-- and "deploy  runbook" one address rather than two pages. The projection
-- stored the title verbatim and resolved an address with `LOWER(title) = ?`
-- against that normalised form. Two of those are not the same function:
--
--   * SQLite's LOWER() folds ASCII ONLY. A title with a non-ASCII capital —
--     "Qualité Étendue", "Über Deploy" — lowercases one way in Go, where the
--     claim was made, and another in SQL, where the lookup happens. Measured:
--     the page was UNREACHABLE BY ITS OWN ADDRESS, and a link that resolves
--     through the address read as a missing page.
--   * `LOWER(title)` cannot use `pages_container_idx`, so every address
--     lookup was a scan of the container's pages.
--
-- The whitespace half of the normalisation happened to agree, because the
-- store collapses runs of whitespace before it writes a title. That is a
-- second place the same rule lives rather than a second bug — and it is
-- exactly the arrangement that produced the first one: two functions that
-- must agree about what a page's address is, with nothing making them.
--
-- WHAT THIS DOES
--
-- `pages` gains `title_norm TEXT NOT NULL`, written by the applier from the
-- same NormalizeTitle the claim key uses, with its own index. The address
-- lookup becomes one indexed equality against a value computed by exactly one
-- function.
--
-- WHY IT DROPS AND RECREATES RATHER THAN ALTERS
--
-- The column cannot be backfilled in SQL. NormalizeTitle lowercases with Go's
-- own Unicode case tables, which is precisely what this engine's LOWER() does
-- not do — an `UPDATE … SET title_norm = LOWER(title)` would write exactly the
-- wrong value into the column added to fix it, on every row with a non-ASCII
-- capital, permanently. So the rows are dropped and the projection is told to
-- rebuild them from coordination, which is what these tables are for: every
-- one of them is a rebuildable copy of a record held elsewhere.
--
-- CHILDREN FIRST. `PRAGMA foreign_keys = ON` is set on every connection and
-- 0020's child tables declare ON DELETE CASCADE, so dropping `pages` first
-- would cascade rather than fail — but the drop order is written out anyway,
-- because a cascade is a delete nobody read.
--
-- The search index rows for pages go with them: `kb_docs` and `kb_postings`
-- are derived from page bodies, and a rebuild re-indexes what it re-applies.
-- And the pages family's projection cursor is reset, which is what makes the
-- rebuild happen at all — a cursor left at its old revision would leave every
-- page missing until somebody edited it.

DROP INDEX IF EXISTS page_history_created_idx;
DROP INDEX IF EXISTS page_history_page_idx;
DROP TABLE IF EXISTS page_history;

DROP INDEX IF EXISTS page_comments_page_idx;
DROP TABLE IF EXISTS page_comments;

DROP INDEX IF EXISTS page_revisions_page_idx;
DROP TABLE IF EXISTS page_revisions;

DROP INDEX IF EXISTS page_watchers_handle_idx;
DROP TABLE IF EXISTS page_watchers;

DROP INDEX IF EXISTS page_labels_label_idx;
DROP TABLE IF EXISTS page_labels;

DROP INDEX IF EXISTS pages_trashed_idx;
DROP INDEX IF EXISTS pages_onboarding_idx;
DROP INDEX IF EXISTS pages_parent_idx;
DROP INDEX IF EXISTS pages_container_idx;
DROP TABLE IF EXISTS pages;

-- pages — one row per page head.
--
-- `skill` and `onboarding` are DERIVED at apply time rather than stored in the
-- document: whether a body parses as a tool skill is this build's answer about
-- this build's parser, and a page written by a newer node must not carry a
-- claim an older node's parser disagrees with. Recomputing on apply means a
-- parser fix reaches every existing page on the next rebuild.
--
-- `title_norm` is derived the same way and for the same reason: it is the
-- ADDRESS the fleet claimed, and one function computes it.
CREATE TABLE pages (
    id           TEXT    NOT NULL PRIMARY KEY,
    container    TEXT    NOT NULL,
    parent_id    TEXT    NOT NULL DEFAULT '',
    title        TEXT    NOT NULL,
    title_norm   TEXT    NOT NULL,
    body         TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,
    author       TEXT    NOT NULL DEFAULT '',
    version      INTEGER NOT NULL,
    skill        INTEGER NOT NULL DEFAULT 0,
    onboarding   INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    trashed_at   INTEGER,
    revision     INTEGER NOT NULL,
    document     TEXT    NOT NULL
);

-- A container's tree, and the ordering a listing renders in.
CREATE INDEX pages_container_idx ON pages (container, status, title);

-- THE ADDRESS LOOKUP: one indexed equality over the value the claim key
-- carries. NOT UNIQUE, deliberately — the claim is what makes a title unique
-- fleet-wide, and a unique index here would raise on a state a rebuild from a
-- restored bucket can produce and wedge the applier on every node at once.
CREATE INDEX pages_address_idx ON pages (container, title_norm);

-- A container's page tree. Not partial, for the reason work_items_assignee_idx
-- gives: the predicate would make it unusable for the query it exists for.
CREATE INDEX pages_parent_idx ON pages (parent_id);

-- The onboarding chain's lookup: the page a seat reads first.
CREATE INDEX pages_onboarding_idx
    ON pages (container)
    WHERE onboarding = 1;

-- The trashed-page sweep. Partial, and its query must restate the predicate —
-- see work_items_closed_idx.
CREATE INDEX pages_trashed_idx
    ON pages (trashed_at)
    WHERE trashed_at IS NOT NULL;

CREATE TABLE page_labels (
    page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    label   TEXT NOT NULL,
    PRIMARY KEY (page_id, label)
);

CREATE INDEX page_labels_label_idx ON page_labels (label, page_id);

CREATE TABLE page_watchers (
    page_id TEXT    NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    handle  TEXT    NOT NULL,
    muted   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (page_id, handle)
);

CREATE INDEX page_watchers_handle_idx ON page_watchers (handle, page_id);

-- page_revisions — METADATA ONLY. The body stays in the bucket.
CREATE TABLE page_revisions (
    id         TEXT    NOT NULL PRIMARY KEY,
    page_id    TEXT    NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    author     TEXT    NOT NULL DEFAULT '',
    message    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    revision   INTEGER NOT NULL
);

CREATE INDEX page_revisions_page_idx
    ON page_revisions (page_id, version DESC);

CREATE TABLE page_comments (
    id          TEXT    NOT NULL PRIMARY KEY,
    page_id     TEXT    NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    author      TEXT    NOT NULL DEFAULT '',
    author_kind TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    reply_to    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    revision    INTEGER NOT NULL,
    document    TEXT    NOT NULL
);

CREATE INDEX page_comments_page_idx
    ON page_comments (page_id, created_at);

CREATE TABLE page_history (
    id          TEXT    NOT NULL PRIMARY KEY,
    page_id     TEXT    NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    operator_id TEXT    NOT NULL DEFAULT '',
    comment_id  TEXT    NOT NULL DEFAULT '',
    excerpt     TEXT    NOT NULL DEFAULT '',
    turn_id     TEXT    NOT NULL DEFAULT '',
    quiet       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    revision    INTEGER NOT NULL,
    document    TEXT    NOT NULL
);

CREATE INDEX page_history_page_idx
    ON page_history (page_id, created_at DESC);

CREATE INDEX page_history_created_idx ON page_history (created_at);

-- The search index's page half is derived from the bodies just dropped, so it
-- goes with them and is rebuilt by the same pass.
--
-- ON A COMPANY WITH EMBEDDINGS CONFIGURED, this also drops each page's vector:
-- kb_vectors references kb_docs with ON DELETE CASCADE. They are recomputed by
-- the embed duty after the rebuild, at the provider's usual per-document cost,
-- and semantic recall over pages is degraded until it catches up — which the
-- coverage alarm reports rather than leaving to be noticed.
DELETE FROM kb_postings WHERE doc_id IN (SELECT id FROM kb_docs WHERE source = 'page');
DELETE FROM kb_docs WHERE source = 'page';

-- AND THE CURSOR, which is what makes the rebuild happen. Without this the
-- projector resumes above every revision it has already seen and the pages
-- tables stay empty until somebody edits every page.
DELETE FROM projection_keys WHERE family = 'pages';
DELETE FROM projection_cursor WHERE family = 'pages';
