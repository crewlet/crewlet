-- A seat's mutable memory rows carry a change sequence too: a counterparty
-- profile, a synthesized skill and an onboarding marker are stamped from
-- 0041's counter on every insert and on every update of a column memsync
-- carries, so the memory changelog carries them past a mark as it carries the
-- append-only tables, rather than whole on every publish.
--
-- WHAT WAS WRONG. Sent whole, a publish cut inside one of these tables kept
-- nothing of it, and the next publish started it again. A seat whose profiles
-- and skills outlast a cycle never carried its onboarding marker after them,
-- and a release flush cut inside them lost every change after the cut, which
-- the next holder's hydration then overwrote with the changelog's older copy
-- (crewlet/crewlet#1040).
--
-- THE UPDATE IS THE CONTENT here (a profile rewritten, a skill's state and use
-- count, a marker flipping), so an update of any carried column stamps the
-- row, whoever wrote it: a turn, the curator, the onboarding claim, or a
-- hydration's DO UPDATE of a carried copy over a stored one. A trigger, as
-- 0041 says why. `UPDATE OF` names the carried columns and never change_seq,
-- so the stamp's own update fires nothing; memsync's
-- TestEveryCarriedColumnOfAMutableTableStampsItsUpdate holds each list to the
-- registry's columns.
--
-- EXISTING ROWS take the counter plus their rowid, unique within the table,
-- and the counter moves past the largest, so every stamp taken after this file
-- ran is above every value before it.
--
-- WHO HAS TO AGREE ON IT: this node alone, as 0041 says of the column; the
-- wire is unchanged.
ALTER TABLE counterparty_profiles ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE synthesized_skills ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_onboarding_markers ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;

UPDATE counterparty_profiles
   SET change_seq = (SELECT last FROM memory_change_sequence) + rowid;
UPDATE memory_change_sequence
   SET last = last + (SELECT COALESCE(max(rowid), 0) FROM counterparty_profiles);

UPDATE synthesized_skills
   SET change_seq = (SELECT last FROM memory_change_sequence) + rowid;
UPDATE memory_change_sequence
   SET last = last + (SELECT COALESCE(max(rowid), 0) FROM synthesized_skills);

UPDATE agent_onboarding_markers
   SET change_seq = (SELECT last FROM memory_change_sequence) + rowid;
UPDATE memory_change_sequence
   SET last = last + (SELECT COALESCE(max(rowid), 0) FROM agent_onboarding_markers);

CREATE TRIGGER counterparty_profiles_change_on_insert AFTER INSERT ON counterparty_profiles
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE counterparty_profiles SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER counterparty_profiles_change_on_update
AFTER UPDATE OF observer_handle, subject_handle, subject_external_id, subject_platform,
    subject_name, traits, first_seen_at, last_updated_at, last_corroborated_at,
    interaction_count, last_work_key
ON counterparty_profiles
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE counterparty_profiles SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER synthesized_skills_change_on_insert AFTER INSERT ON synthesized_skills
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE synthesized_skills SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER synthesized_skills_change_on_update
AFTER UPDATE OF id, agent_handle, name, description, content, frontmatter, tool_sequence,
    source_episode_ids, version, created_at, updated_at, use_count, last_used_at, state,
    pinned, stale_at, archived_at, load_count
ON synthesized_skills
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE synthesized_skills SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER agent_onboarding_markers_change_on_insert AFTER INSERT ON agent_onboarding_markers
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE agent_onboarding_markers SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER agent_onboarding_markers_change_on_update
AFTER UPDATE OF agent_id, chain_hash, agent_handle, role, summary, created_at, updated_at,
    in_progress_until
ON agent_onboarding_markers
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE agent_onboarding_markers SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

-- THE EXPORT IS A RANGE SEEK, as 0041's: one seat's rows past its mark, in
-- sequence order, every memory sync cycle for every seat this node holds.
CREATE INDEX counterparty_profiles_agent_change_idx
    ON counterparty_profiles (observer_handle, change_seq);
CREATE INDEX synthesized_skills_agent_change_idx
    ON synthesized_skills (agent_handle, change_seq);
CREATE INDEX agent_onboarding_markers_agent_change_idx
    ON agent_onboarding_markers (agent_id, change_seq);
