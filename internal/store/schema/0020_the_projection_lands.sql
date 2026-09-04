-- The per-node PROJECTION of the fleet's work items and knowledge pages.
--
-- NOTHING IN THIS FILE IS AUTHORITATIVE. Every table here is a rebuildable
-- copy of records held in internal/coord's document buckets, and that is the
-- opposite of what 0010-0013 moved OUT of this database. Those migrations
-- deleted tables holding facts a fleet had to agree on; this one adds tables
-- holding facts a fleet already agrees on, kept here so a node can query them.
-- The distinction is the whole design, so it is worth stating in the terms
-- 0010 used:
--
--   0010-0013 asked "does the company have to agree on this?" and moved the
--   answer to coordination, because a per-node copy of a shared FACT is a
--   fleet disagreeing with itself.
--
--   This asks a second question the first never had to: "how is that fact
--   READ?" Every record those migrations moved is read by key — a claim, a
--   counter, a lease. These are read by the thousand, filtered, sorted and
--   searched, and a listing over a KV bucket is O(keys) message deliveries.
--   So the record of truth stays in coordination and each node keeps a local
--   copy to answer questions with.
--
-- WHAT THAT MEANS FOR ANYONE READING THESE ROWS
--
--   * A row here can be stale. It carries the coordination revision it was
--     built from, and a caller that needs its own write back asks the
--     projection to wait for that revision rather than assuming.
--   * A row here can be DELETED and rebuilt at any time. The boot reconcile
--     drops what the bucket no longer holds; a corrupt or unconvergeable
--     projection is fixed by truncating these tables and re-running it.
--   * NEVER write to these tables from anywhere but the projector. A write
--     that coordination did not see is a write the next reconcile silently
--     erases, and it will look like the engine losing a person's work.
--
-- One writer per family owns every statement below. The driver's BeginTx
-- issues a plain BEGIN, so a read-then-write that loses a race fails at once
-- with "database snapshot is stale"; store.DB.Tx retries that, but two
-- projectors racing over one row would still interleave two DIFFERENT
-- revisions' decisions. The single writer is what makes an apply a pure
-- function of (row, change).

-- ---- the tracker ------------------------------------------------------ --

-- work_items — one row per item, the head as coordination last showed it.
--
-- `document` carries the whole encoded record, including fields THIS BUILD
-- DOES NOT KNOW: a rolling upgrade puts a newer node's fields on the wire,
-- and a projector that kept only its own columns would hand a dashboard PATCH
-- a head with the new field already stripped. The columns beside it are
-- extracted for filtering and sorting, and are a cache of what the document
-- says, never a second source.
CREATE TABLE work_items (
    id            TEXT    NOT NULL PRIMARY KEY,
    -- The human key, <PROJECT>-<n>. Immutable once minted, and unique across
    -- the company: two items sharing one key is a person opening the wrong
    -- record from a link somebody pasted into chat.
    item_key      TEXT    NOT NULL UNIQUE,
    project       TEXT    NOT NULL,
    type          TEXT    NOT NULL,
    parent_id     TEXT    NOT NULL DEFAULT '',
    title         TEXT    NOT NULL DEFAULT '',
    body          TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL,
    close_reason  TEXT    NOT NULL DEFAULT '',
    duplicate_of  TEXT    NOT NULL DEFAULT '',
    priority      TEXT    NOT NULL DEFAULT 'none',
    reporter      TEXT    NOT NULL DEFAULT '',
    assignee      TEXT    NOT NULL DEFAULT '',
    due_at        INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    closed_at     INTEGER,
    -- The coordination revision this row was built from. The ETag a caller
    -- passes back, and what makes an apply idempotent: a change carrying a
    -- revision at or below this one is a redelivery and is dropped.
    revision      INTEGER NOT NULL,
    document      TEXT    NOT NULL
);

-- The board: a project's open items, newest first.
CREATE INDEX work_items_project_status_idx
    ON work_items (project, status, updated_at DESC);

-- "What am I on?", the query every seat's own view runs.
CREATE INDEX work_items_assignee_idx
    ON work_items (assignee, status, updated_at DESC)
    WHERE assignee <> '';

-- A parent's children, for the epic/story/subtask walk.
CREATE INDEX work_items_parent_idx
    ON work_items (parent_id)
    WHERE parent_id <> '';

-- The trashed sweep's range scan. A partial index, because closed items are
-- kept for ever by decision and only the removed ones age out.
CREATE INDEX work_items_closed_idx
    ON work_items (closed_at)
    WHERE closed_at IS NOT NULL;

-- work_labels — an item's labels, one row each.
--
-- A table rather than a JSON column on work_items, because "every item
-- labelled X" is a filter a board offers and a JSON scan cannot index.
CREATE TABLE work_labels (
    item_id TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    label   TEXT NOT NULL,
    PRIMARY KEY (item_id, label)
);

CREATE INDEX work_labels_label_idx ON work_labels (label, item_id);

-- work_watchers — who hears about an item.
--
-- `muted` records an EXPLICIT unwatch, and it is a row rather than an absence
-- because absence is the state every automatic re-add fills: a person who
-- unwatches and is then named as assignee would be re-added by the assignee
-- rule and start hearing about it again. A directed mention still reaches
-- them, which is the one thing a mute must not swallow.
CREATE TABLE work_watchers (
    item_id TEXT    NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    handle  TEXT    NOT NULL,
    muted   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, handle)
);

CREATE INDEX work_watchers_handle_idx ON work_watchers (handle, item_id);

-- work_links — a typed relationship between two items.
--
-- The document holds ONE END of each link; both directions are derived here,
-- with `derived` marking the half nobody wrote. A board renders "blocked by"
-- from the derived row and an edit always writes the authored one, so the
-- pair can never disagree about which item owns the link.
CREATE TABLE work_links (
    item_id      TEXT    NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    other_id     TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    derived      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, other_id, kind)
);

CREATE INDEX work_links_other_idx ON work_links (other_id, kind);

-- work_comments — the thread on an item.
CREATE TABLE work_comments (
    id         TEXT    NOT NULL PRIMARY KEY,
    item_id    TEXT    NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    author     TEXT    NOT NULL DEFAULT '',
    author_kind TEXT   NOT NULL DEFAULT '',
    body       TEXT    NOT NULL DEFAULT '',
    reply_to   TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    revision   INTEGER NOT NULL,
    document   TEXT    NOT NULL
);

CREATE INDEX work_comments_item_idx
    ON work_comments (item_id, created_at);

-- work_history — the change record, projected from the change class.
--
-- Append-only here as it is in the bucket. It is what an item's activity
-- panel renders and what an audit answers from, so a projector NEVER rewrites
-- a row: a change key is created once and never touched again.
CREATE TABLE work_history (
    id          TEXT    NOT NULL PRIMARY KEY,
    item_id     TEXT    NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
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

CREATE INDEX work_history_item_idx
    ON work_history (item_id, created_at DESC);

-- The 365-day bucket sweep's companion scan on this side.
CREATE INDEX work_history_created_idx ON work_history (created_at);

-- work_counters — the last number minted per project.
--
-- A CACHE of the counter document, kept so a board can show a project without
-- a bucket read. A mint NEVER reads it: minting reads the counter document
-- and compare-and-sets it, because a stale local number would mint a key that
-- already exists.
CREATE TABLE work_counters (
    project  TEXT    NOT NULL PRIMARY KEY,
    last     INTEGER NOT NULL,
    revision INTEGER NOT NULL
);

-- ---- the knowledge base ----------------------------------------------- --

-- page_containers — a space: a unit's, the org root's, the skills container.
CREATE TABLE page_containers (
    key        TEXT    NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL DEFAULT '',
    purpose    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    revision   INTEGER NOT NULL,
    document   TEXT    NOT NULL
);

-- pages — one row per page head.
--
-- `skill` and `onboarding` are DERIVED at apply time rather than stored in the
-- document: whether a body parses as a tool skill is this build's answer about
-- this build's parser, and a page written by a newer node must not carry a
-- claim an older node's parser disagrees with. Recomputing on apply means a
-- parser fix reaches every existing page on the next rebuild.
CREATE TABLE pages (
    id           TEXT    NOT NULL PRIMARY KEY,
    container    TEXT    NOT NULL,
    parent_id    TEXT    NOT NULL DEFAULT '',
    title        TEXT    NOT NULL,
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

-- A container's tree, and the title lookup a link resolves through.
CREATE INDEX pages_container_idx ON pages (container, status, title);

CREATE INDEX pages_parent_idx
    ON pages (parent_id)
    WHERE parent_id <> '';

-- The onboarding chain's lookup: the page a seat reads first.
CREATE INDEX pages_onboarding_idx
    ON pages (container)
    WHERE onboarding = 1;

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
--
-- A page keeps its last hundred revisions, and a 512 KiB body times a hundred
-- times every page is a projection an order of magnitude larger than the
-- record it copies, on every node, to answer a question a person asks about
-- one page at a time. So the history list renders from these rows and opening
-- one revision reads its body from coordination.
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

-- ---- the search index ------------------------------------------------- --

-- kb_docs — one row per indexed document, page or item alike.
--
-- ITS OWN TABLE rather than columns on pages and work_items, because the
-- index is a different lifecycle: it is built asynchronously after the rows
-- land, it can be dropped and rebuilt when the analyzer changes, and a search
-- that fused two sources would otherwise need a UNION over two schemas on
-- every query.
--
-- Turso has no fts5 and no `USING fts` outside an experimental flag (see
-- internal/store/caps.go, which probes for both and reports what it found), so
-- the index is BM25 over an inverted list this engine maintains. The
-- alternative was refusing knowledge search on the only driver this build
-- ships, which is not a knowledge base.
CREATE TABLE kb_docs (
    id          TEXT    NOT NULL PRIMARY KEY,
    -- 'page' or 'item'. Not a foreign key, deliberately: a source row is
    -- deleted by the projector and the index row by the indexer, in that
    -- order, and a cascade would delete a posting list the indexer is
    -- mid-write on.
    source      TEXT    NOT NULL,
    source_id   TEXT    NOT NULL,
    container   TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    excerpt     TEXT    NOT NULL DEFAULT '',
    -- Total token count, the |D| in BM25's length normalisation.
    length      INTEGER NOT NULL DEFAULT 0,
    -- The source's own version, so an indexer knows a row is stale without
    -- re-tokenising the body.
    source_rev  INTEGER NOT NULL DEFAULT 0,
    indexed_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX kb_docs_source_idx ON kb_docs (source, source_id);

CREATE INDEX kb_docs_container_idx ON kb_docs (container);

-- kb_postings — the inverted list: which documents hold a term, and how often.
--
-- (term, doc) with the frequency, which is everything BM25 needs beside the
-- per-term document count that is a GROUP BY over this table's own index.
CREATE TABLE kb_postings (
    term   TEXT    NOT NULL,
    doc_id TEXT    NOT NULL REFERENCES kb_docs(id) ON DELETE CASCADE,
    freq   INTEGER NOT NULL,
    PRIMARY KEY (term, doc_id)
);

-- The scan a query runs: every posting for one term, in doc order.
CREATE INDEX kb_postings_doc_idx ON kb_postings (doc_id);

-- kb_vectors — the semantic half, present only when the company configured
-- embeddings.
--
-- BLOB rather than F32_BLOB(n) for the reason 0002 gives at length: the width
-- is a runtime property of the active company's embedding model, and
-- vector_distance_cos fails the WHOLE STATEMENT on a mismatch, so every read
-- filters on length(embedding) rather than trusting a declared width.
CREATE TABLE kb_vectors (
    doc_id     TEXT    NOT NULL PRIMARY KEY REFERENCES kb_docs(id) ON DELETE CASCADE,
    embedding  BLOB,
    -- The source version this embedding was computed from. A vector whose
    -- source moved is stale and is recomputed rather than served.
    source_rev INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- ---- the cursor and the key set --------------------------------------- --

-- projection_keys — every coordination key this node has applied, and at what
-- revision.
--
-- THE BOOT RECONCILE'S OTHER HALF. Establishing that the projection matches
-- the bucket is a set comparison, and one side of it has to be enumerable:
-- the bucket's side is a metadata-only watch, and this is ours. Deriving it
-- from the rows instead would mean every table knowing how to name its own
-- keys back, in a mapping that must agree exactly with the one that wrote
-- them — and where it disagreed, the reconcile would delete live rows or
-- leave dead ones, silently, on one node.
--
-- `purged` marks a key the bucket no longer holds, kept as a row rather than
-- deleted so the reconcile can tell "we applied its removal" from "we never
-- saw it" without re-fetching. It is what stops a purge being re-applied on
-- every boot.
CREATE TABLE projection_keys (
    family   TEXT    NOT NULL,
    key      TEXT    NOT NULL,
    revision INTEGER NOT NULL,
    purged   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (family, key)
);

-- ---- the cursor ------------------------------------------------------- --

-- projection_cursor — how far each family has been applied on THIS node.
--
-- One row per family. `revision` advances only after the transaction holding
-- the rows it describes has committed, which is what makes a crash mid-batch
-- a re-apply rather than a hole: the changes are idempotent by revision, so
-- replaying them is free and skipping them is not.
--
-- `hydrated` is a separate fact from the revision and it is the one a seat's
-- mailbox waits on: a cursor at some revision says only where the tail is,
-- while hydrated says the boot reconcile has established that every key in
-- the bucket is present here. A node that resumed from a revision the stream
-- no longer has would otherwise sit at a plausible cursor over an empty
-- projection, for ever, silently.
CREATE TABLE projection_cursor (
    family      TEXT    NOT NULL PRIMARY KEY,
    revision    INTEGER NOT NULL DEFAULT 0,
    hydrated    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);
