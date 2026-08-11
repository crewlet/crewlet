-- agent_diary -- per-agent private semantic memory.
--
-- Holds the agent's LONG (no-TTL) and SHORT (TTL'd) observations.
-- Written by ``PersistDecider`` (post-turn) and the
-- ``reflect_and_persist`` builtin (in-flight).  Read by
-- ``KnowledgeSystem.query_unified`` (vector similarity, scoped to
-- the calling agent) and by the Plan-phase ``## Personal memory``
-- prefetch (newest-first listing, TTL-filtered).
--
-- Naming: ``diary`` not ``memory`` to underscore the semantic
-- difference -- this is the agent's *private observation log*,
-- not knowledge other agents can query.  The kind values are
-- ``diary_long`` (no expiry) and ``diary_short`` (TTL'd).
--
-- Identity: keyed by ``agent_id`` (the deterministic UUID derived
-- from ``(org.name, handle)`` via ``crewlet.db.agents.derive_agent_id``)
-- so a renamed handle cleanly orphans old rows rather than mixing
-- them with the new identity.

CREATE TABLE agent_diary (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id           TEXT        NOT NULL,
    kind               TEXT        NOT NULL,
    content            TEXT        NOT NULL,
    ttl_until          TIMESTAMPTZ NULL,
    source             TEXT        NOT NULL DEFAULT '',
    turn_id            TEXT        NOT NULL DEFAULT '',
    metadata           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    retrieval_count    INTEGER     NOT NULL DEFAULT 0,
    last_retrieved_at  TIMESTAMPTZ NULL,
    embedding          vector($embedding_dimensions),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_diary_kind_check CHECK (kind IN ('diary_long', 'diary_short'))
);

-- Per-agent listing: ``list_for_agent`` queries by ``agent_id``
-- ordered by ``created_at DESC``.  Index ordering matches the query
-- so Postgres can serve it without a sort node.
CREATE INDEX idx_agent_diary_agent_created
    ON agent_diary (agent_id, created_at DESC);

-- TTL-expiry path: ``delete_expired`` and ``list_for_agent`` both
-- filter ``ttl_until < now()`` (or null) -- a partial index on
-- non-null TTLs lets Postgres scan only the SHORT rows during
-- cleanup.
CREATE INDEX idx_agent_diary_ttl_until
    ON agent_diary (ttl_until)
    WHERE ttl_until IS NOT NULL;

-- Vector similarity over the agent's diary, used by
-- ``query_unified`` to merge personal hits with shared ``doc_index``
-- hits in one ranked output.  Partial: NULL embeddings (test mode
-- without an EmbeddingProvider) are excluded from vector search but
-- still visible to ``list_for_agent``.
CREATE INDEX idx_agent_diary_embedding
    ON agent_diary USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;
