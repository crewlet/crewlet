-- The advisor's patterns read comments by their author's kind and time, and a
-- mention notice keeps whether its author said the comment asks.
--
-- WHO HAS TO AGREE ON IT: the index is derived from tracker_comments, which
-- every data node's applier writes identically; tracker_notifications is
-- divergent (outside the identity claim), so a node on an older build that
-- leaves `asks` NULL never makes two copies disagree about a claimed table.
--
-- `asks` is the comment author's statement on its record (1 asks, 0 asks
-- nothing), NULL for a person's comment, a notice that is not a comment's, and
-- a record an older build wrote. The patterns read (worklet.Reader.WorkPatterns)
-- counts an agent's mention as a wake that asked nothing only where it is not 1.
CREATE INDEX tracker_comments_kind_created_idx ON tracker_comments (author_kind, created_at); -- read by Reader.WorkPatterns (patterns.go)
ALTER TABLE tracker_notifications ADD COLUMN asks INTEGER;
