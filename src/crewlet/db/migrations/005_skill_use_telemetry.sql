-- Skill-use telemetry.
--
-- Adds use_count + last_used_at to ``synthesized_skills`` so the
-- engine can answer the Berlot-Attwell measurement question: do
-- agents actually load the skills SkillSynthesizer drafts, or is
-- the apparent benefit coming from extra sampling?  Without these
-- columns the question is unanswerable.
--
-- Also defines the ``learning_health`` view -- a flat per-agent
-- aggregate operators can query directly to see whether their
-- agents' synthesized-skill libraries are paying for themselves.

ALTER TABLE synthesized_skills
    ADD COLUMN use_count    INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN last_used_at TIMESTAMPTZ NULL;

-- Per-agent skill-use rollup.  One row per ``agent_handle``.
-- ``use_count > 0`` is the "this skill has been loaded at least
-- once" signal -- the cohort of skills that are paying their way.
-- ``avg_uses_per_skill`` is the headline metric: <0.1 is the
-- Berlot-Attwell warning threshold.
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
GROUP BY s.agent_handle;
