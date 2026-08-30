-- The agent-learning subsystem's durable memory: episodes, the private
-- diary, counterparty profiles, synthesized skills and their version
-- history, and the onboarding markers. See docs/concepts/agent-learning.md.

-- episodes — one row per completed agent turn, plus the compacted
-- cluster summaries the lifecycle worker folds them into.
--
-- Two row shapes share the table, told apart by `kind`:
--   * 'raw'       — one turn. Embedded for similarity recall over prior
--                   plans and outcomes.
--   * 'compacted' — a cluster summary. `count` is how many raw rows it
--                   replaces, the common_* / subjects_involved /
--                   notable_patterns fields carry the LLM-summarised
--                   aggregate, and exemplar_turn_ids points at the two or
--                   three raw rows kept as drill-down anchors.
CREATE TABLE episodes (
    -- The Postgres table keyed on (ended_at, id) because a TimescaleDB
    -- hypertable requires its partitioning column in the primary key.
    -- That was never a statement about identity — an episode is its id —
    -- and carrying the composite here would only preserve the constraint
    -- that forced the advisory-lock dance below out of existence.
    id             TEXT    NOT NULL PRIMARY KEY,
    agent_handle   TEXT    NOT NULL,
    agent_role     TEXT    NOT NULL,
    task_id        TEXT    NOT NULL DEFAULT '',
    turn_id        TEXT    NOT NULL,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL,
    plan_summary   TEXT    NOT NULL,
    task_summary   TEXT    NOT NULL,
    tool_sequence  TEXT    NOT NULL DEFAULT '[]',
    skills_used    TEXT    NOT NULL DEFAULT '[]',
    review_outcome TEXT    NOT NULL,
    duration_ms    INTEGER NOT NULL,
    -- A packed little-endian float32 vector; NULL when the embeddings
    -- provider was unreachable at write time, exactly as the Postgres
    -- column allowed. A transient outage must never cost an episode:
    -- recall filters `embedding IS NOT NULL`, while the time-window and
    -- outcome queries still surface the row.
    --
    -- Declared BLOB, not F32_BLOB(n). The width is a RUNTIME property —
    -- it comes from the active company config's embedding model — and
    -- templating it into DDL is what forced the Postgres migrator into a
    -- two-phase run (read the config to learn the width, then migrate).
    -- Turso does not enforce a declared F32_BLOB width anyway (measured:
    -- a 3-element vector inserts happily into F32_BLOB(4)), and
    -- vector_distance_cos() reads a plain BLOB column and a bound []byte
    -- identically — so the declaration bought nothing and cost a phase.
    -- The Go layer validates the width on write instead, and recall filters
    -- on `length(embedding)` on read: vector_distance_cos FAILS THE WHOLE
    -- STATEMENT on a width mismatch, so a company that changed embedding
    -- model would otherwise find recall erroring rather than skipping the
    -- rows it cannot compare.
    embedding      BLOB,

    kind                       TEXT    NOT NULL DEFAULT 'raw',
    count                      INTEGER NOT NULL DEFAULT 1,
    exemplar_turn_ids          TEXT    NOT NULL DEFAULT '[]',
    consolidated_into_skill_id TEXT,
    common_task_pattern        TEXT    NOT NULL DEFAULT '',
    common_outcome             TEXT    NOT NULL DEFAULT '',
    success_rate               REAL    NOT NULL DEFAULT 0,
    subjects_involved          TEXT    NOT NULL DEFAULT '[]',
    notable_patterns           TEXT    NOT NULL DEFAULT '',

    -- work_key — the identity of the unit of work this turn did, NULL
    -- when the turn had no ledgerable trigger (a scheduled fire, a
    -- sub-agent, a sandbox resume). Two nodes can both complete a turn
    -- for one trigger — a zombie finishing between fence checks, or an
    -- honest re-run after the completion ledger fails open — and an
    -- episode keyed on nothing simply lands twice, then feeds every later
    -- recall and skill synthesis, weighting the agent's behaviour with an
    -- event that happened once.
    --
    -- NULL rather than '' for "unconstrained" is the whole trick, and it
    -- is what deletes the Postgres idiom wholesale. There the exclusion
    -- was `UNIQUE(agent_handle, work_key) WHERE work_key <> ''`, which a
    -- hypertable refuses outright, so the write path took a
    -- transaction-scoped advisory lock and inserted WHERE NOT EXISTS.
    -- SQL treats NULLs as distinct, so a PLAIN unique index over a
    -- nullable column gives identical semantics — and a plain index is
    -- the only thing a bare `ON CONFLICT (agent_handle, work_key)` can
    -- target: aiming ON CONFLICT at a partial index is a parse error
    -- unless the predicate is repeated verbatim in the statement
    -- (measured, and still measured — see TestPartialIndexConflictTarget).
    -- The store maps '' <-> NULL at the boundary so Go callers keep the
    -- zero value.
    work_key         TEXT,
    -- conversation_key — which conversation the turn served, as
    -- {source}:{local}. NULL for triggers with no derivable conversation
    -- and on compacted rows, whose clusters span conversations by
    -- construction.
    conversation_key TEXT
);

-- Per agent, not global: two seats legitimately act on one trigger (a
-- broadcast, a task assigned to a unit) and each one's episode is its own
-- memory.
CREATE UNIQUE INDEX episodes_agent_work_key_idx
    ON episodes (agent_handle, work_key);

-- Per-agent time windows — the recent-episodes read, and the scan vector
-- recall runs over. Recall is still a SCAN (no ANN index reaches the Go
-- driver, adrs/002), even though the distance arithmetic itself is the
-- database's since adrs/003 — so this index doing the agent-scoping is
-- what keeps that scan over one seat's thousands of rows rather than the
-- whole table. It also carries recall's tie-break: equal distances order by
-- ended_at DESC.
CREATE INDEX episodes_agent_ended_at_idx
    ON episodes (agent_handle, ended_at DESC);

-- Outcome-filtered scans ("my last N successful turns").
CREATE INDEX episodes_outcome_ended_at_idx
    ON episodes (review_outcome, ended_at DESC);

-- Per-agent kind-aware scans: counting raw rows against the compaction
-- threshold, listing recent raw rows for the compaction pass.
CREATE INDEX episodes_agent_kind_ended_at_idx
    ON episodes (agent_handle, kind, ended_at DESC);

-- Episodes ready to drop once the post-consolidation grace elapses.
-- Partial keeps it tiny: almost every row is NULL here.
CREATE INDEX episodes_consolidated_idx
    ON episodes (consolidated_into_skill_id, ended_at)
    WHERE consolidated_into_skill_id IS NOT NULL;

-- "The previous turn on this same ticket." Partial and per agent for the
-- same reasons as work_key's index; most rows in a schedule-driven
-- company carry no conversation at all.
CREATE INDEX episodes_agent_conversation_idx
    ON episodes (agent_handle, conversation_key)
    WHERE conversation_key IS NOT NULL;


-- agent_diary — per-agent PRIVATE semantic memory.
--
-- The agent's own observation log, not knowledge other agents can query;
-- "diary" rather than "memory" to keep that distinction in the name.
-- Written by the post-turn persist decider and the in-flight
-- reflect_and_persist builtin; read by similarity search scoped to the
-- calling agent and by the Plan-phase personal-memory prefetch.
--
-- Keyed by agent_id (the deterministic UUID derived from org name plus
-- handle) rather than by handle, so renaming a handle cleanly orphans the
-- old rows instead of mixing them into the new identity.
CREATE TABLE agent_diary (
    id                TEXT    NOT NULL PRIMARY KEY,
    agent_id          TEXT    NOT NULL,
    kind              TEXT    NOT NULL
                      CHECK (kind IN ('diary_long', 'diary_short')),
    content           TEXT    NOT NULL,
    -- NULL for diary_long (no expiry); a deadline for diary_short.
    ttl_until         INTEGER,
    source            TEXT    NOT NULL DEFAULT '',
    turn_id           TEXT    NOT NULL DEFAULT '',
    metadata          TEXT    NOT NULL DEFAULT '{}',
    retrieval_count   INTEGER NOT NULL DEFAULT 0,
    last_retrieved_at INTEGER,
    embedding         BLOB,
    created_at        INTEGER NOT NULL
);

CREATE INDEX agent_diary_agent_created_idx
    ON agent_diary (agent_id, created_at DESC);

-- The expiry path scans only the SHORT rows.
CREATE INDEX agent_diary_ttl_idx
    ON agent_diary (ttl_until)
    WHERE ttl_until IS NOT NULL;


-- counterparty_profiles — what an observer has learned about a subject.
--
-- One row per (observer, subject), updated in place; lookups are exact,
-- never vector. `traits` is a flexible JSON blob whose keys the LLM
-- invents.
--
-- Two timestamps, deliberately:
--   * last_updated_at      moves on every upsert, no-ops included, so it
--                          measures INTERACTION cadence.
--   * last_corroborated_at moves only when the traits patch is non-empty,
--                          so it measures trait-CHANGE cadence. The
--                          Plan-phase prefetch demotes stale traits by it.
CREATE TABLE counterparty_profiles (
    observer_handle      TEXT    NOT NULL,
    -- These three are '' when absent, NOT NULL — the opposite of
    -- episodes.work_key above, and for a reason worth keeping: they are
    -- primary-key columns whose emptiness is MEANINGFUL. The composite
    -- key is what lets a resolved agent (subject_handle set) and an
    -- unmapped external human (subject_external_id + subject_platform)
    -- coexist under one observer without colliding. NULL here would make
    -- every such row distinct from every other, which is the exact
    -- opposite of what the key is for.
    subject_handle       TEXT    NOT NULL DEFAULT '',
    subject_external_id  TEXT    NOT NULL DEFAULT '',
    subject_platform     TEXT    NOT NULL DEFAULT '',
    subject_name         TEXT    NOT NULL DEFAULT '',
    traits               TEXT    NOT NULL DEFAULT '{}',
    first_seen_at        INTEGER NOT NULL,
    last_updated_at      INTEGER NOT NULL,
    last_corroborated_at INTEGER NOT NULL,
    interaction_count    INTEGER NOT NULL DEFAULT 0,
    -- The last unit of work counted into interaction_count. One column
    -- rather than a side table: the duplicate this guards against is
    -- always the IMMEDIATELY preceding write (two nodes racing, or a
    -- redelivery), never one from last week, so the last key is exactly
    -- as much history as the guard can use.
    last_work_key        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (
        observer_handle,
        subject_handle,
        subject_external_id,
        subject_platform
    )
);

CREATE INDEX counterparty_profiles_subject_handle_idx
    ON counterparty_profiles (subject_handle)
    WHERE subject_handle <> '';

CREATE INDEX counterparty_profiles_subject_external_idx
    ON counterparty_profiles (subject_platform, subject_external_id)
    WHERE subject_external_id <> '';


-- synthesized_skills — one row per (agent_handle, name).
--
-- Written by the synthesizer (single-turn or clustering pass) and updated
-- in place by the refiner, which archives the prior state to
-- synthesized_skill_versions first. Cross-agent promotion drafts a
-- knowledge-base page instead of writing a unit-scope row: the engine
-- carries agent-scope skills only.
CREATE TABLE synthesized_skills (
    id                 TEXT    NOT NULL PRIMARY KEY,
    agent_handle       TEXT    NOT NULL,
    name               TEXT    NOT NULL,
    description        TEXT    NOT NULL,
    content            TEXT    NOT NULL,
    frontmatter        TEXT    NOT NULL DEFAULT '{}',
    tool_sequence      TEXT    NOT NULL DEFAULT '[]',
    source_episode_ids TEXT    NOT NULL DEFAULT '[]',
    version            INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    -- Use telemetry. Without these two the question "do agents actually
    -- load the skills the synthesizer drafts, or is the apparent benefit
    -- just extra sampling?" has no answer at all.
    use_count          INTEGER NOT NULL DEFAULT 0,
    last_used_at       INTEGER,
    -- Curator state machine. active -> stale -> archived on disuse;
    -- stale -> active on revival. Pinned rows are exempt from every
    -- automatic transition, and nothing is ever deleted: archived rows
    -- stay readable so an operator can restore one.
    state              TEXT    NOT NULL DEFAULT 'active'
                       CHECK (state IN ('active', 'stale', 'archived')),
    pinned             INTEGER NOT NULL DEFAULT 0,
    stale_at           INTEGER,
    archived_at        INTEGER
);

CREATE INDEX synthesized_skills_agent_created_idx
    ON synthesized_skills (agent_handle, created_at DESC);

CREATE UNIQUE INDEX synthesized_skills_agent_name_idx
    ON synthesized_skills (agent_handle, name);

-- The curator buckets skills by transition class, which the
-- created_at index above does not cover.
CREATE INDEX synthesized_skills_agent_state_idx
    ON synthesized_skills (agent_handle, state);


-- synthesized_skill_versions — the prior state of a skill, archived on
-- every refine, replace, promotion or rollback.
CREATE TABLE synthesized_skill_versions (
    id                 TEXT    NOT NULL PRIMARY KEY,
    -- Skills are archive-never-delete in normal operation, but the
    -- foreign key ENFORCES that invariant rather than assuming it: if an
    -- operator ever does delete a skill row, its history cascades with it
    -- instead of orphaning. The store turns foreign keys on per
    -- connection — SQLite's default is off, so an unenforced constraint
    -- would look exactly like an enforced one until the day it mattered.
    skill_id           TEXT    NOT NULL
                       REFERENCES synthesized_skills(id) ON DELETE CASCADE,
    agent_handle       TEXT    NOT NULL DEFAULT '',
    name               TEXT    NOT NULL,
    description        TEXT    NOT NULL,
    content            TEXT    NOT NULL,
    frontmatter        TEXT    NOT NULL DEFAULT '{}',
    tool_sequence      TEXT    NOT NULL DEFAULT '[]',
    source_episode_ids TEXT    NOT NULL DEFAULT '[]',
    version            INTEGER NOT NULL,
    -- observed_in_practice | counter_example | refine_skill_tool
    --   | replace | promotion | rollback
    refinement_kind    TEXT    NOT NULL,
    refinement_note    TEXT    NOT NULL DEFAULT '',
    archived_at        INTEGER NOT NULL
);

CREATE INDEX synthesized_skill_versions_skill_idx
    ON synthesized_skill_versions (skill_id, archived_at DESC);


-- learning_health — the per-agent skill-use rollup an operator queries
-- directly (docs/concepts/agent-learning.md). avg_uses_per_skill is the
-- single load-bearing metric; below 0.1 the library is not paying for
-- itself.
--
-- Archived skills are excluded: the curator deliberately aged them out
-- and the Plan prefetch hides them, so counting them only deflates the
-- average and muddies the signal.
--
-- The Postgres tree defined this view THREE times — once in 005, again in
-- 009 with the archived filter, and a third time in 020 to repair
-- databases where two concurrent migrators applied 005 and 009 out of
-- order and left the earlier definition serving under the later version
-- number. One definition in one file makes that failure unrepresentable,
-- which is the point of consolidating rather than replaying.
CREATE VIEW learning_health AS
SELECT
    s.agent_handle                                        AS agent_handle,
    COUNT(*)                                              AS total_skills,
    SUM(CASE WHEN s.use_count > 0 THEN 1 ELSE 0 END)      AS skills_used_at_least_once,
    SUM(s.use_count)                                      AS total_skill_uses,
    MAX(s.last_used_at)                                   AS most_recent_skill_use,
    AVG(s.use_count)                                      AS avg_uses_per_skill,
    -- created_at is microseconds; 86 400 000 000 of them make a day.
    AVG((unixepoch() * 1000000 - s.created_at) / 86400000000.0)
                                                          AS avg_skill_age_days
FROM synthesized_skills s
WHERE s.state <> 'archived'
GROUP BY s.agent_handle;


-- agent_onboarding_markers — per-agent onboarding bookkeeping, one row
-- per agent so re-onboarding upserts rather than accumulating.
--
-- chain_hash is a stable hash over the agent's org chain (org name, each
-- ancestor unit, role). A chain change silently invalidates the marker
-- and the first-turn onboarding hint re-fires until the agent reads and
-- re-marks.
CREATE TABLE agent_onboarding_markers (
    agent_id     TEXT    NOT NULL PRIMARY KEY,
    chain_hash   TEXT    NOT NULL,
    agent_handle TEXT    NOT NULL DEFAULT '',
    role         TEXT    NOT NULL DEFAULT '',
    summary      TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    -- The cross-process single-flight lease for the onboarding pass. A
    -- claim for a never-marked agent inserts the row with an empty
    -- chain_hash, which reads correctly as "not onboarded"; the TTL
    -- bounds a claimant that died mid-pass. Claiming FAILS CLOSED — a
    -- store that cannot answer means the pass is skipped and retried,
    -- because running a possibly-duplicate onboarding is the worse
    -- outcome.
    in_progress_until INTEGER
);
