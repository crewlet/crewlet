-- The changes ready to merge are listed.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_mergeability stays reproducible, and an index adds no row.
-- About 50 bytes a live change whose stored readiness is ready (its id and the row's), none for another; a mergeability, a refresh or a clear writes its entry only where the row is ready before or after.
CREATE INDEX code_mergeability_ready_idx ON code_mergeability (ready, change_id) WHERE ready = 1;    -- Reader.Read(changes) readiness=ready: the changes whose stored readiness is ready, newest first (changereadiness.go)
