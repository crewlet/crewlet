-- A change keeps the moves of its lifecycle.
-- WHO HAS TO AGREE ON IT: every data node, identically (gitlet.ReproducibleTables); one row a move (a close, reopen, draft, ready, retarget, or each part an edit sets), keyed by the change, the position of the record that made it and its kind: by_handle and by_kind who made it, reason a close's, was and became a retarget's target or the work item an edit set, at the record's broker instant.
-- Class: code_change_moves is reproducible, written only by the applier from the records on the repository's subject and removed by its purge, a change's moves before the change; a few dozen bytes a move.
CREATE TABLE code_change_moves (
    change_id TEXT    NOT NULL,
    position  INTEGER NOT NULL,
    kind      TEXT    NOT NULL,
    by_handle TEXT    NOT NULL DEFAULT '',
    by_kind   TEXT    NOT NULL DEFAULT '',
    reason    TEXT    NOT NULL DEFAULT '',
    was       TEXT    NOT NULL DEFAULT '',
    became    TEXT    NOT NULL DEFAULT '',
    at        INTEGER NOT NULL,
    PRIMARY KEY (change_id, position, kind)
);
