-- A change is walked in the order its records moved it.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_changes stays reproducible, and an index adds no row.
-- About 20 bytes a change; every write that moves a change's version names its position and rewrites this entry, one beside the four a live head move rewrote.
CREATE INDEX code_changes_position_idx ON code_changes (position);    -- this node's index of changes' words walks forward from its watermark (search changeindex.go), and a listing narrowed by words reads what moved past it (gitlet changetext.go)
