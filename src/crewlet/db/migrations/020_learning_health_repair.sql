-- Repair `learning_health` for databases migrated by racing processes.
--
-- `learning_health` is defined by CREATE OR REPLACE VIEW in TWO files:
-- 005_skill_use_telemetry.sql introduces it, and 009_skill_curator.sql
-- redefines it with `WHERE s.state <> 'archived'` (archived skills are
-- aged out by the curator and hidden from the Plan prefetch, so counting
-- them only deflates the average and muddies the signal).
--
-- Before the migrator took an advisory lock, three entry points could run
-- it concurrently -- `crewlet run`, `crewlet run api`, and
-- `crewlet config import` -- and the split-deployment docs told operators
-- to start the first two together.  Two racers could therefore apply 005
-- and 009 out of order: the LAST writer wins the view definition while
-- `schema_migrations` records both versions as applied.  The result is a
-- database that reports 009 applied while serving 005's definition, i.e.
-- operator metrics that silently include archived skills.  Because the
-- version rows exist, no amount of re-running the migrator ever fixes it.
--
-- This migration is that fix: it unconditionally re-issues the canonical
-- (009) definition.  Idempotent by construction -- on a correctly-ordered
-- database it replaces the view with the identical text and changes
-- nothing.  Kept verbatim in sync with 009_skill_curator.sql; a future
-- change to the view belongs in a NEW migration, never in an edit to
-- either of these files.

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
