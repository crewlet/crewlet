-- An integration's read of the atlas records only what changed and withdraws
-- what it no longer holds (ADR-0129), so before each pass the collect duty
-- reads every live claim of that one voice. The voice index serves one slot's
-- current claim and the live index every voice's claims together; neither
-- finds one voice's claims without a scan of all of them.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. An index derives from
-- rows every node already holds alike, and changes no row.
CREATE INDEX atlas_claims_source_idx ON atlas_claims (source_kind, source_name, position)
    WHERE replaced = 0; -- one voice's live claims, the collect duty's input
