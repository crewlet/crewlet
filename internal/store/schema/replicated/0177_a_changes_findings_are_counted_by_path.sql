-- A change's findings are counted by path.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_findings stays reproducible, and an index adds no row.
-- About 100 bytes a finding (its change's id, its path, its state, its revision and the row's id); a review adds one entry a finding it raises, and every answer, dismissal or supersession rewrites one a finding it moves, as code_findings_raiser_idx already does.
CREATE INDEX code_findings_path_idx ON code_findings (change_id, path, state, revision);    -- Reader.Read(change) finding_paths: a change's findings by path (changepage.go)
