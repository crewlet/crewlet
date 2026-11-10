-- A skill counts the times an agent loaded it (use_skill) beside the times it
-- was offered (`use_count`, the turn-start offers the reflection counts), so
-- its load rate says whether agents take up what they are offered.
--
-- WHO HAS TO AGREE ON IT: whichever node holds the seat, as every skill column;
-- memsync carries it with the row.
ALTER TABLE synthesized_skills ADD COLUMN load_count INTEGER NOT NULL DEFAULT 0;

-- The health rollup names the loads and the load rate (loads over offers,
-- NULL for a seat whose skills were never offered, never a false 0).
DROP VIEW learning_health;
CREATE VIEW learning_health AS
SELECT
    s.agent_handle                                        AS agent_handle,
    COUNT(*)                                              AS total_skills,
    SUM(CASE WHEN s.use_count > 0 THEN 1 ELSE 0 END)      AS skills_used_at_least_once,
    SUM(s.use_count)                                      AS total_skill_uses,
    SUM(s.load_count)                                     AS total_skill_loads,
    CASE WHEN SUM(s.use_count) > 0
         THEN 1.0 * SUM(s.load_count) / SUM(s.use_count) END
                                                          AS skill_load_rate,
    MAX(s.last_used_at)                                   AS most_recent_skill_use,
    AVG(s.use_count)                                      AS avg_uses_per_skill,
    -- created_at is microseconds; 86 400 000 000 of them make a day.
    AVG((unixepoch() * 1000000 - s.created_at) / 86400000000.0)
                                                          AS avg_skill_age_days
FROM synthesized_skills s
WHERE s.state <> 'archived'
GROUP BY s.agent_handle;
