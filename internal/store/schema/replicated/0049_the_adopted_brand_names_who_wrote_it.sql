-- The adopted brand names who wrote it.
--
-- An adoption records the administrator who adopted one version of a brand
-- kit, and the voice an agent reads from it was fenced as that administrator's
-- words. The version's own author, the person or the agent who drafted the
-- kit, wrote them, so the adoption keeps that author beside the adopter, copied
-- from the version by the applier when it adopts, and the voice's fence names
-- both. A row adopted before this migration names no author ('').
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- artifacts log by its applier, like every other column of the row.
ALTER TABLE artifact_brand ADD COLUMN kit_author TEXT NOT NULL DEFAULT '';
ALTER TABLE artifact_brand ADD COLUMN kit_author_kind TEXT NOT NULL DEFAULT '';
