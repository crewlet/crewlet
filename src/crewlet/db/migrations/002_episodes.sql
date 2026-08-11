-- Episodic memory store for the agent-learning subsystem (phase 1
-- + phase 6 lifecycle).
--
-- Two row shapes share this table, distinguished by ``kind``:
--
--   * ``kind='raw'`` (the original): one row per completed agent
--     turn, embedded for similarity search over prior plans + outcomes.
--   * ``kind='compacted'``: a cluster summary written by the
--     ``EpisodeLifecycleWorker`` after compaction; ``count`` is the
--     number of original episodes the row replaces, the
--     ``common_*`` / ``subjects_involved`` / ``notable_patterns``
--     fields carry the LLM-summarised aggregate, and
--     ``exemplar_turn_ids`` references 2-3 raw rows kept as
--     drill-down anchors.
--
-- A TimescaleDB hypertable rather than a plain table because
-- episodes are time-ordered, fast-growing (one per turn per agent),
-- and queried by time window as well as vector similarity -- the
-- hypertable's time-range chunking matches that workload.
--
-- Lifecycle: the ``EpisodeLifecycleWorker`` subscribes to
-- ``crewlet.events.compaction_requested`` (published by ``EpisodeStore``
-- when an agent's raw count crosses ``max_raw_episodes_per_agent``)
-- and drains the table by:
--
--   1. Dropping non-terminal episodes older than N days
--      (self_iterate mid-states never feed skill synthesis; only
--       feed query_episodes recall as noise).
--   2. Dropping episodes whose ``consolidated_into_skill_id`` is
--      set and which are older than the grace window (the skill
--      itself carries the learning forward).
--   3. Clustering remaining raw episodes and LLM-summarising each
--      cluster into one ``kind='compacted'`` row, dropping the
--      originals (except a few exemplars).
--   4. (Optional) evicting very old compacted entries.
--
-- See ``docs/concepts/agent-learning.md`` for the full design.

CREATE TABLE episodes (
    id              UUID        NOT NULL DEFAULT gen_random_uuid(),
    agent_handle    TEXT        NOT NULL,
    agent_role      TEXT        NOT NULL,
    task_id         TEXT        NOT NULL DEFAULT '',
    turn_id         UUID        NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ NOT NULL,
    plan_summary    TEXT        NOT NULL,
    task_summary    TEXT        NOT NULL,
    tool_sequence   JSONB       NOT NULL DEFAULT '[]'::jsonb,
    skills_used     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    review_outcome  TEXT        NOT NULL,
    duration_ms     INTEGER     NOT NULL,
    -- Nullable: a row written during an embeddings-API outage lands
    -- with ``embedding=NULL`` (same degradation as ``agent_diary`` /
    -- ``doc_index``) so a transient hiccup never loses an episode.
    -- Vector recall filters ``embedding IS NOT NULL``; the time-window
    -- and outcome queries still surface NULL-embedding rows.
    embedding       vector($embedding_dimensions),
    -- Lifecycle / compaction columns (defaults preserve raw shape so
    -- raw rows never need to set them explicitly).
    kind                       TEXT        NOT NULL DEFAULT 'raw',
    count                      INTEGER     NOT NULL DEFAULT 1,
    exemplar_turn_ids          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    consolidated_into_skill_id UUID,
    -- Compacted-row payload (raw rows leave these empty / zero):
    common_task_pattern        TEXT        NOT NULL DEFAULT '',
    common_outcome             TEXT        NOT NULL DEFAULT '',
    success_rate               DOUBLE PRECISION NOT NULL DEFAULT 0,
    subjects_involved          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    notable_patterns           TEXT        NOT NULL DEFAULT '',
    -- Hypertables require the time column in the primary key.
    PRIMARY KEY (ended_at, id)
);

SELECT create_hypertable(
    'episodes',
    'ended_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists => TRUE,
    migrate_data  => TRUE
);

-- Per-agent time-window lookups (recent-episodes queries).
CREATE INDEX idx_episodes_agent_ended_at
    ON episodes (agent_handle, ended_at DESC);

-- Vector similarity search; HNSW is the right default for pgvector
-- at this scale (matches the index style on the other vector tables).
-- Partial: rows written during an embeddings outage have a NULL
-- embedding and are excluded from vector search (but stay readable by
-- the time-window / outcome / id queries).
CREATE INDEX idx_episodes_embedding
    ON episodes USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;

-- Outcome-filtered scans (e.g. "my last N successful turns").
CREATE INDEX idx_episodes_outcome_ended_at
    ON episodes (review_outcome, ended_at DESC);

-- Per-agent kind-aware scans (counting raw rows for the
-- threshold-trigger; listing recent raw for compaction).
CREATE INDEX idx_episodes_agent_kind_ended_at
    ON episodes (agent_handle, kind, ended_at DESC);

-- Find episodes ready to drop after consolidation grace.  Partial
-- index keeps it tiny (most rows are NULL here).
CREATE INDEX idx_episodes_consolidated_into_skill
    ON episodes (consolidated_into_skill_id, ended_at)
    WHERE consolidated_into_skill_id IS NOT NULL;
