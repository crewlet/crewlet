-- A live change is listed by when it last moved.
-- WHO HAS TO AGREE ON IT: every data node, identically; code_changes and code_conversations stay reproducible, and an index adds no row.
-- About 60 and 100 bytes a live change and 90 a conversation somebody spoke in; a write that moves a live change's version rewrites its two entries, and a record somebody writes on a change one more.
CREATE INDEX code_changes_live_updated_idx ON code_changes (state, updated_at, id)
    WHERE state IN ('draft', 'open');    -- Reader.Read(changes) of a live state by when its changes last moved (changesort.go)

CREATE INDEX code_changes_repo_live_updated_idx ON code_changes (repo_id, state, updated_at, id)
    WHERE state IN ('draft', 'open');    -- the same, of one repository (changesort.go)

CREATE INDEX code_conversations_repo_updated_idx ON code_conversations (repo_id, updated_at, change_id)
    WHERE updated_at > 0;    -- one repository's changes by when somebody last spoke of them (changesort.go)
