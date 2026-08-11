-- Skill curator state machine.
--
-- Adds ``state``, ``pinned``, ``stale_at``, ``archived_at`` to
-- ``synthesized_skills`` so the new ``SkillCuratorWorker`` can
-- age out unused synthesized skills. Curated by an interval-driven
-- background worker (see ``crewlet.learning.skill_curator``).
--
-- Transitions:
--   * active → stale     when last_used_at < now - stale_after_days
--                        AND NOT pinned AND last_used_at < now - safety_window
--   * stale → archived   when last_used_at < now - archive_after_days
--                        AND NOT pinned AND last_used_at < now - safety_window
--   * stale → active     (revival) when the agent uses the skill again
--                        and last_used_at > now - stale_after_days
--
-- Pinned skills are exempt from all auto-transitions.  Archived
-- skills are still in the table -- the prefetch filter hides them
-- from the Plan catalogue and ``_use_skill`` refuses to load them,
-- but operators can restore one with
-- ``UPDATE synthesized_skills SET state='active' WHERE id=...``.
-- No data is ever deleted.

ALTER TABLE synthesized_skills
    ADD COLUMN state       TEXT        NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'stale', 'archived')),
    ADD COLUMN pinned      BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN stale_at    TIMESTAMPTZ NULL,
    ADD COLUMN archived_at TIMESTAMPTZ NULL;

-- Curator queries scan ``(agent_handle, state)`` to bucket skills
-- by transition class; the existing
-- ``idx_synthesized_skills_agent_created`` does not cover the state
-- predicate.
CREATE INDEX idx_synth_skills_agent_state
    ON synthesized_skills (agent_handle, state);

-- Redefine ``learning_health`` (created in 005_skill_use_telemetry.sql)
-- to exclude archived skills.  The Berlot-Attwell metric
-- (``avg_uses_per_skill``) measures whether the *live* skill library
-- earns its keep; counting archived rows -- which the curator
-- deliberately aged out and the Plan prefetch hides -- only deflates
-- the average and muddies the signal.  Archived skills stay in the
-- table for operator restore; they just don't count toward library
-- health.
CREATE OR REPLACE VIEW learning_health AS
SELECT
    s.agent_handle,
    COUNT(*)::int                                                    AS total_skills,
    SUM(CASE WHEN s.use_count > 0 THEN 1 ELSE 0 END)::int            AS skills_used_at_least_once,
    SUM(s.use_count)::int                                            AS total_skill_uses,
    MAX(s.last_used_at)                                              AS most_recent_skill_use,
    AVG(s.use_count)::float                                          AS avg_uses_per_skill,
    AVG(EXTRACT(EPOCH FROM (now() - s.created_at)) / 86400)::float   AS avg_skill_age_days
FROM synthesized_skills s
WHERE s.state <> 'archived'
GROUP BY s.agent_handle;
