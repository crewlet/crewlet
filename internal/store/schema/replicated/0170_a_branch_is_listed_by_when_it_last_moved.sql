-- A branch is listed by when it last moved, and a branch's newest change is found by its source.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_refs and code_changes stay reproducible, and an index adds no row.
-- About 90 bytes a live branch and 80 a change; a push rewrites one entry per branch it moves, a change's open adds one.
CREATE INDEX code_refs_moved_idx ON code_refs (repo_id, at, ref)
    WHERE target != '' AND ref >= 'refs/heads/' AND ref < 'refs/heads0';    -- Reader.Read(refs) in moved order and its active count (refslisting.go)

CREATE INDEX code_changes_source_idx ON code_changes (repo_id, source_ref, number);    -- Reader.Read(refs) in detail: the newest change from a branch (refslisting.go)
