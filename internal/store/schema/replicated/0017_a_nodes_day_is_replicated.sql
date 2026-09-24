-- The usage domain: what each node's seats and schedules did each company day,
-- and the framework's FOURTH domain — its second COMPACTED one.
--
-- Everything here is written by exactly one thing — the usage domain's
-- applier, from records committed on CREWLET_USAGE_LOG — and by nothing else,
-- which is the rule 0002 states for the tracker and the reason these tables
-- live in this estate rather than beside the event log they are derived from.
--
-- # Why a node's day is replicated at all
--
-- Every answer about spend, turns, page reads and schedule runs was a query
-- over `crewlet_events`, the NODE estate's audit log — so each one described
-- the node that answered it and nothing else. A three-node fleet showed a
-- third of its spend on whichever node the dashboard reached, and a node that
-- left took its history with it. Who has to agree on a node's day? Every node,
-- and only its current value: the day is re-derived from the owning node's
-- own log and republished whole, so the newest record for a (node, day,
-- object) is the only one anybody wants. That is ADR-0014's FOURTH answer — a
-- compacted changelog — applied to aggregates rather than to a seat's memory.
-- adr/0020 is the decision.
--
-- # Why this domain is COMPACTED, and what that costs
--
-- One message per subject turns the stream into a keyed table, which is why
-- the domain declares ReplayCompacted and why a gap is a coverage number rather
-- than a fault. The consequences are the vector domain's (0003), for the same
-- reasons:
--
--   * NO OPERATION LEDGER. A record REPLACES its object's rows under a
--     monotone position guard, so the apply is total and a redelivery writes
--     the same rows or none; and nothing waits on a usage record, so no
--     writer ever reports a committed position to a caller.
--   * NO ANCHOR ROWS. Exactly one writer publishes on a subject — the node the
--     day belongs to — so nothing arbitrates and no expectation is formed.
--   * NO IDENTITY CLAIM. A node that joined late holds fewer days than one
--     that has been here since the start, so these tables are `Divergent`: they
--     travel inside a snapshot and are outside the fleet twin comparison.
--
-- # The horizon is the APPLIER's, never a sweep's
--
-- Nothing deletes these rows on a timer. A record for day D removes every row
-- older than D minus the history (181 days, usage.History) in the same
-- transaction that writes its own, which is a pure function of the record: a
-- local sweep would be a second writer of the replicated estate, which is the
-- thing store's applier-exclusivity gate refuses. So the leading `day` of
-- every primary key below is what that range delete seeks on.
--
-- # The shape of a key
--
-- `day` is the company-local date label (`2026-09-23`), which sorts as a
-- string and is what every node computes alike; `node` is the publishing node's
-- id. Every row of one record shares (day, node, object), which is what the
-- apply's delete-then-insert replaces.

-- usage_turns — one row per (day, node, seat): the seat's turn statistics, its
-- name as that day knew it, and the record's position.
--
-- THE HEAD ROW. Every seat record writes it, even one with no turns, because
-- `version` here is the guard for the seat's whole record — its tokens and its
-- reads are replaced only when this row's version is older than the record.
--
-- `duration_hist` is the turn-duration histogram as JSON: sparse [bin, count]
-- pairs at 2^(1/6) per bin from 1 s to 24 h, which bounds a quantile's error at
-- ±6% and MERGES across nodes and days by adding counts — a per-turn list would
-- not fit a day's row, and a stored p50 cannot be combined with another.
CREATE TABLE usage_turns (
    day           TEXT    NOT NULL,
    node          TEXT    NOT NULL,
    agent_id      TEXT    NOT NULL,
    handle        TEXT    NOT NULL DEFAULT '',
    role          TEXT    NOT NULL DEFAULT '',
    turns         INTEGER NOT NULL DEFAULT 0,
    failed        INTEGER NOT NULL DEFAULT 0,
    reviewed      INTEGER NOT NULL DEFAULT 0,
    first_pass    INTEGER NOT NULL DEFAULT 0,
    sent_back     INTEGER NOT NULL DEFAULT 0,
    duration_hist TEXT    NOT NULL DEFAULT '[]',
    last_ended_at INTEGER NOT NULL DEFAULT 0,
    reads_elided  INTEGER NOT NULL DEFAULT 0,
    version       INTEGER NOT NULL,
    PRIMARY KEY (day, node, agent_id)
);

-- A seat's history across days, which is what a seat's own activity answer
-- reads: every node's row for one seat, newest days last.
CREATE INDEX usage_turns_agent_idx ON usage_turns (agent_id, day);

-- usage_tokens — one row per (day, node, seat, phase, worker, model, provider
-- key): the tokens and the calls that spent them.
--
-- The cache counts are a BREAKDOWN of `input`, never an addition to it, which
-- is the contract node/0030 states for the event log's columns: `total` is
-- input plus output.
CREATE TABLE usage_tokens (
    day          TEXT    NOT NULL,
    node         TEXT    NOT NULL,
    agent_id     TEXT    NOT NULL,
    phase        TEXT    NOT NULL DEFAULT '',
    worker       TEXT    NOT NULL DEFAULT '',
    model        TEXT    NOT NULL DEFAULT '',
    provider_key TEXT    NOT NULL DEFAULT '',
    input        INTEGER NOT NULL DEFAULT 0,
    output       INTEGER NOT NULL DEFAULT 0,
    cache_read   INTEGER NOT NULL DEFAULT 0,
    cache_write  INTEGER NOT NULL DEFAULT 0,
    total        INTEGER NOT NULL DEFAULT 0,
    calls        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node, agent_id, phase, worker, model, provider_key)
);

-- Every spend window is a day range over every node and every seat.
CREATE INDEX usage_tokens_day_idx ON usage_tokens (day);

-- usage_reads — one row per (day, node, seat, backend, page, via): how often a
-- seat reached a page that day and what the newest of those reads was.
--
-- Capped per seat-day (usage.ReadsPerSeatDay) by the WRITER, so a record stays
-- far below the stream's message limit however much a seat searched; what the
-- cap dropped is counted on the head row's `reads_elided`, never silently.
CREATE TABLE usage_reads (
    day           TEXT    NOT NULL,
    node          TEXT    NOT NULL,
    agent_id      TEXT    NOT NULL,
    backend       TEXT    NOT NULL DEFAULT '',
    page_id       TEXT    NOT NULL,
    via           TEXT    NOT NULL,
    reads         INTEGER NOT NULL DEFAULT 0,
    last_at       INTEGER NOT NULL DEFAULT 0,
    last_turn_id  TEXT    NOT NULL DEFAULT '',
    last_work_key TEXT    NOT NULL DEFAULT '',
    last_query    TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (day, node, agent_id, backend, page_id, via)
);

-- "Who has read this page lately" is a page's own question, over a day range.
CREATE INDEX usage_reads_page_idx ON usage_reads (page_id, day);

-- usage_schedule_runs — one row per fire a node's scheduler dispatched: when,
-- to whom, what the dispatch ledger recorded, and the trace the turn ran
-- under. `version` is on every row because a schedule record has no head row:
-- the guard is the newest version among its object's rows.
CREATE TABLE usage_schedule_runs (
    day        TEXT    NOT NULL,
    node       TEXT    NOT NULL,
    scope_type TEXT    NOT NULL,
    scope_id   TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    fired_at   INTEGER NOT NULL,
    target     TEXT    NOT NULL DEFAULT '',
    outcome    TEXT    NOT NULL DEFAULT '',
    trace_id   TEXT    NOT NULL DEFAULT '',
    turn_id    TEXT    NOT NULL DEFAULT '',
    version    INTEGER NOT NULL,
    PRIMARY KEY (day, node, scope_type, scope_id, name, fired_at, target)
);

-- usage_log_deferred — a record this build cannot decode, byte for byte, in
-- the shape 0003 gives the vector domain's, and for its reason: a compacted
-- domain's retention of a deferred record keys on the SUBJECT, so the
-- supersede is a delete by subject and needs the index.
CREATE TABLE usage_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX usage_log_deferred_subject_idx ON usage_log_deferred (subject_id, subject_kind);
CREATE INDEX usage_log_deferred_wire_idx ON usage_log_deferred (subject);

-- usage_log_deferred_scope — one row per scope path of a deferred record,
-- written in the SAME transaction as its parent, as 0003's.
CREATE TABLE usage_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX usage_log_deferred_scope_idx ON usage_log_deferred_scope (path, position);
