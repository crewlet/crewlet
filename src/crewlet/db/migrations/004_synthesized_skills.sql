-- Synthesized skills for the agent-learning subsystem.
--
-- One row per ``(agent_handle, name)``.  Written by:
--   * ``SkillSynthesizer`` -- single-turn (>=min_tool_calls + done)
--                            or scheduled clustering pass.
--   * ``SkillRefiner``     -- updates an existing row in place;
--                            archives prior state to
--                            ``synthesized_skill_versions``.
--
-- Cross-agent promotion (when N siblings in the same OrgUnit converge
-- on a similar pattern) drafts a knowledge-base page (Confluence or
-- Plane, via the active backend's PromotionPageWriter) rather than
-- writing a unit-scope row to this table -- the engine carries
-- agent-scope skills only.  See
-- ``crewlet.learning.skill_synthesizer.PromotionSynthesizer``.

CREATE TABLE synthesized_skills (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_handle             TEXT        NOT NULL,
    name                     TEXT        NOT NULL,
    description              TEXT        NOT NULL,
    content                  TEXT        NOT NULL,
    frontmatter              JSONB       NOT NULL DEFAULT '{}'::jsonb,
    tool_sequence            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    source_episode_ids       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    version                  INTEGER     NOT NULL DEFAULT 1,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-agent scans: list_for_agent (ordered by created_at) and
-- count_for_agent (cap enforcement).
CREATE INDEX idx_synthesized_skills_agent_created
    ON synthesized_skills (agent_handle, created_at DESC);

-- One skill per ``(agent_handle, name)``.
CREATE UNIQUE INDEX idx_synth_skills_agent_name
    ON synthesized_skills (agent_handle, name);


-- Version history: every SkillRefiner update / refine_skill tool
-- call / rollback archives the prior state here.
CREATE TABLE synthesized_skill_versions (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Skills are archive-never-delete in normal operation, but the FK
    -- enforces that invariant rather than assuming it: if an operator
    -- ever does delete a skill row, its version history cascades with
    -- it instead of orphaning.
    skill_id                 UUID        NOT NULL
        REFERENCES synthesized_skills(id) ON DELETE CASCADE,
    agent_handle             TEXT        NOT NULL DEFAULT '',
    name                     TEXT        NOT NULL,
    description              TEXT        NOT NULL,
    content                  TEXT        NOT NULL,
    frontmatter              JSONB       NOT NULL DEFAULT '{}'::jsonb,
    tool_sequence            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    source_episode_ids       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    version                  INTEGER     NOT NULL,
    -- observed_in_practice | counter_example | refine_skill_tool
    --   | replace | promotion | rollback
    refinement_kind          TEXT        NOT NULL,
    refinement_note          TEXT        NOT NULL DEFAULT '',
    archived_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ssv_skill_id_time
    ON synthesized_skill_versions (skill_id, archived_at DESC);
