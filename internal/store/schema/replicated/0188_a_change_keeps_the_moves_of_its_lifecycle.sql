-- A change keeps the moves of its lifecycle.
--
-- WHO HAS TO AGREE ON IT: every data node, identically (gitlet.ReproducibleTables).
--
-- A change's row holds where its lifecycle stands now; its timeline needs how it got there: who
-- closed it and why, who reopened it, who made it a draft or asked again for it to be merged, who
-- edited its title, its description or its work item, and who moved its target. One row a move,
-- keyed by the change, the position of the record that made it and the move's kind (an edit of a
-- title and a description in one record is two moves): by and by_kind are who made it, as merged_by
-- is who merged it (0114); reason a close's reason; was and became what a retarget moved the target
-- from and to, and the work item key an edit set (became, '' where it cleared one); at the record's
-- broker instant. Every lifecycle record of a live or closed change writes its moves, and a push
-- that closes a change by deleting its source writes that close, named for its pusher.
--
-- A move is a few dozen bytes and a change makes a handful, so the table is a fraction of
-- code_revisions. Read a page at a time in position order (a change's timeline), on its key; a
-- purge part counts and takes a change's moves with its other narrow rows.
--
-- Class: code_change_moves is reproducible: written only by the applier, from the records on the
-- repository's subject, and removed only by the repository's purge, a change's moves before the
-- change.
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
