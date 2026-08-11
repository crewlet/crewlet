-- agent_onboarding_markers -- per-agent onboarding bookkeeping.
--
-- One row per agent (``agent_id`` is the primary key, so re-onboarding
-- UPSERTs the existing row rather than accumulating duplicates).
-- ``chain_hash`` is a stable hash over the agent's org chain
-- (org name + each ancestor unit + role); a chain change silently
-- invalidates the marker, and the Plan-phase ``## First-turn
-- onboarding`` hint re-fires until the agent reads + re-marks.
--
-- ``is_onboarded`` answers with one indexed equality lookup --
-- no metadata-filter scan, no client-side post-processing.

CREATE TABLE agent_onboarding_markers (
    agent_id      TEXT        PRIMARY KEY,
    chain_hash    TEXT        NOT NULL,
    agent_handle  TEXT        NOT NULL DEFAULT '',
    role          TEXT        NOT NULL DEFAULT '',
    summary       TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
