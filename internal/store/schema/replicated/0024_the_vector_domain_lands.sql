-- The vector domain: the framework's SECOND, and its first COMPACTED one.
--
-- Everything here is written by exactly one thing — the vector domain's
-- applier, from records committed on CREWLET_TRACKER_VECTORS — and by nothing
-- else, which is the rule 0023 states for the tracker and the reason both live
-- in this estate rather than beside the lexical index.
--
-- # Why an embedding is replicated and the lexical index is not
--
-- 0020 put `kb_vectors` in the NODE estate beside `kb_docs` and `kb_postings`,
-- on the rule that file states: a table there is a rebuildable copy of
-- something the fleet already agrees on. That is true of the inverted list —
-- every node tokenises its own rows with no network — and false of an
-- embedding, which costs a provider call per source and cannot be recomputed
-- by a node on its own. One fleet-singleton duty embeds each source once, the
-- record carries the vector, and every node applies it: the company pays the
-- bill once and holds the answer N times. `0022_the_vectors_leave.sql` in the
-- node estate is where the unwritten table this replaces is dropped.
--
-- # Why this domain is COMPACTED, and what that costs
--
-- An embedding has no history worth keeping: the newest vector for a source is
-- the only one anybody wants, and the one before it is a provider call nobody
-- would repeat. So its stream keeps one message per subject, which turns it
-- from a log into a keyed table — and makes an ordinary write remove an
-- interior sequence, which is why the domain declares ReplayCompacted and why
-- a gap here is a coverage number rather than a fault. Three consequences are
-- durable and are the reason this file differs from 0023:
--
--   * NO OPERATION LEDGER. There is no `vectors_ops` table, because the apply
--     is a total function under a monotone version guard and the duty that
--     publishes never reports a committed position to a caller. The framework
--     asserts that pairing rather than assuming it.
--   * NO ANCHOR ROWS. The domain arbitrates no kind, so nothing forms a
--     per-subject expectation and `statelog_anchor` stays the mutation log's.
--   * NO IDENTITY CLAIM. Two nodes legitimately hold different coverage — one
--     joined after a trim, one is mid-fill — so these tables are `Divergent`
--     and `Derived` rather than `Replicated`, and the fleet twin comparison
--     does not read them.

-- kb_vectors — the embedding of one source, as the fleet last computed it.
--
-- `embedding` is declared BLOB and never F32_BLOB(n), for the reason 0002
-- gives at length: the width is a runtime property of the active company's
-- model, a `vector(N)` column is fixed at creation, and the schema is
-- forward-only — so a width guessed at migrate time refuses every later insert
-- for ever. The read filters `length(embedding) = ?` bound from the query
-- vector's own encoded length instead, which is what makes a width change
-- converge in place rather than refuse at open.
--
-- `model` sits beside `dim` and is not decoration: a model change at the SAME
-- width leaves both models' vectors in one candidate pool, ranked against each
-- other in incompatible embedding spaces, for as long as a refill takes.
--
-- `version` is a POSITION on this domain's stream — the composed
-- (generation << 40) | seq — and is the applier's monotone guard. It is not
-- comparable with a position on the mutation log, and nothing compares them:
-- two positions on different streams are two different number spaces.
CREATE TABLE kb_vectors (
    -- 'page' or 'item'. Not a foreign key to anything: the source row lives in
    -- another estate, and this table is written by an applier that has never
    -- seen it.
    source      TEXT    NOT NULL,
    source_id   TEXT    NOT NULL,
    container   TEXT    NOT NULL DEFAULT '',
    model       TEXT    NOT NULL,
    dim         INTEGER NOT NULL,
    -- The source's own version this vector was computed from, so the duty
    -- knows a row is stale without re-embedding it.
    source_rev  INTEGER NOT NULL DEFAULT 0,
    -- The digest of the exact text that was embedded. A source can be rewritten
    -- into the same words — a re-file, a label, a parent move — and this is what
    -- stops the duty paying for that.
    text_sha    TEXT    NOT NULL DEFAULT '',
    embedding   BLOB,
    embedded_at INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    PRIMARY KEY (source, source_id)
);

-- The container prune, which is every scoped search's own predicate.
CREATE INDEX kb_vectors_scope_idx ON kb_vectors (source, container);

-- The refill sweep after a model or width change: every row that is not on the
-- configured pair.
CREATE INDEX kb_vectors_model_idx ON kb_vectors (model, dim);

-- kb_vectors_bin — the same vectors as 1-bit sign codes, and the first stage
-- of every semantic search.
--
-- # Why it is a SIBLING TABLE and not a column
--
-- A row is stored contiguously, so reading ANY column of a 12 KB row costs
-- traversing that row's overflow pages. Measured at 3 072 dimensions on this
-- driver: the identical 1-bit column costs 7.31 µs/row inside the wide row and
-- 0.99 µs/row in a narrow one — 7.4× for the same bits and the same function.
-- A generated column, an expression index and a second column on kb_vectors
-- all buy the compression; none of them buys the speed, and the speed is the
-- entire reason the first stage exists.
--
-- # Why it is a PLAIN ROWID TABLE
--
-- `WITHOUT ROWID` is the obvious narrower choice and the pinned driver refuses
-- it: `Parse error: WITHOUT ROWID tables are an experimental feature`, measured
-- through store.Open with this engine's own session pragmas. A migration
-- carrying it would fail on every node and every store would refuse to open.
-- internal/store/caps.go probes for it, so the day the flag lands is a day this
-- decision can be revisited from a measurement rather than from memory.
--
-- # What it does NOT carry
--
-- No `source_rev`, no `text_sha`, no `embedded_at` and no `version`. Every one
-- of those would be a second copy of a fact kb_vectors already holds, and a
-- second copy is a thing that can disagree. `model` and `dim` are the exception
-- and they are load-bearing: `vector_distance_cos` over mismatched lengths is
-- undefined, and a same-width model change needs a predicate that can exclude
-- the old space. The disagreement objection is answered by the WRITE rather
-- than by the omission — both rows are written by one applier in one
-- transaction, so there is no window in which they differ.
--
-- Class `Derived`: rebuildable from kb_vectors IN THIS FILE with no network,
-- by one INSERT … SELECT … vector1bit(embedding).
CREATE TABLE kb_vectors_bin (
    source    TEXT    NOT NULL,
    source_id TEXT    NOT NULL,
    container TEXT    NOT NULL DEFAULT '',
    model     TEXT    NOT NULL,
    dim       INTEGER NOT NULL,
    bits      BLOB    NOT NULL,
    PRIMARY KEY (source, source_id)
);

-- The stage-1 scan's own predicate, in the order it filters: the embedding
-- space first, then the source kind, then the container.
CREATE INDEX kb_vectors_bin_scope_idx ON kb_vectors_bin (model, dim, source, container);

-- vectors_log_deferred — a record this build cannot decode, byte for byte.
--
-- The shape 0022's header documents, with ONE addition a compacted domain
-- needs and a strict one does not: an index on `subject`. A compacted domain's
-- retention keys on the subject rather than the position — the stream itself
-- keeps one message per subject, so a positional retention would keep records
-- the stream has already superseded — and the supersede is a DELETE by subject
-- on every deferred write. Without the index that delete scans the table.
CREATE TABLE vectors_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX vectors_log_deferred_subject_idx ON vectors_log_deferred (subject_id, subject_kind);
CREATE INDEX vectors_log_deferred_wire_idx ON vectors_log_deferred (subject);

-- vectors_log_deferred_scope — one row per scope path of a deferred record,
-- written in the SAME transaction as its parent.
--
-- A plain rowid table and no foreign key, for the two reasons 0023 gives for
-- the tracker's: `WITHOUT ROWID` is refused by the pin, and a cascade is a
-- delete nobody committed — the supersede removes these rows through the
-- parent's own position, child first, in the statement pair the framework
-- writes.
CREATE TABLE vectors_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX vectors_log_deferred_scope_idx ON vectors_log_deferred_scope (path, position);
