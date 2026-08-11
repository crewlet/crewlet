-- Counterparty profiles for the agent-learning subsystem.
--
-- One row per ``(observer, subject)`` pair, updated in place as the
-- observer interacts with the subject over time.  Updates are keyed
-- upserts, not append-only inserts; lookups are by exact subject
-- identifier (no vector search); ``traits`` is a flexible JSONB blob
-- whose keys are LLM-invented.
--
-- Two separate timestamps:
--   * ``last_updated_at``      -- bumped on every upsert (NOOPs included)
--                                 so it captures INTERACTION cadence.
--   * ``last_corroborated_at`` -- bumped only when the traits patch is
--                                 non-empty so it captures trait-CHANGE
--                                 cadence.  The Plan-phase prefetch uses
--                                 the latter to demote stale traits in
--                                 ranking.

CREATE TABLE counterparty_profiles (
    observer_handle      TEXT        NOT NULL,
    subject_handle       TEXT        NOT NULL DEFAULT '',
    subject_external_id  TEXT        NOT NULL DEFAULT '',
    subject_platform     TEXT        NOT NULL DEFAULT '',
    subject_name         TEXT        NOT NULL DEFAULT '',
    traits               JSONB       NOT NULL DEFAULT '{}'::jsonb,
    first_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_corroborated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    interaction_count    INTEGER     NOT NULL DEFAULT 0,
    -- Composite key so both resolved agents (subject_handle set) and
    -- unmapped external humans (subject_external_id + subject_platform)
    -- can coexist without collisions.
    PRIMARY KEY (
        observer_handle,
        subject_handle,
        subject_external_id,
        subject_platform
    )
);

-- Resolved-handle lookups (e.g. when lookup_agent resolves a query to
-- an internal agent handle).
CREATE INDEX idx_counterparty_profiles_subject_handle
    ON counterparty_profiles (subject_handle)
    WHERE subject_handle <> '';

-- External-humans lookups (e.g. a Slack user who isn't a Crewlet agent).
CREATE INDEX idx_counterparty_profiles_subject_external
    ON counterparty_profiles (subject_platform, subject_external_id)
    WHERE subject_external_id <> '';
