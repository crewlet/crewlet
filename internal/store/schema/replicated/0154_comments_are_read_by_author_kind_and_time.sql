-- Comments are read by their author's kind and their time.
--
-- WHO HAS TO AGREE ON IT: nobody beyond the node; an index is derived from
-- tracker_comments, which every data node's applier writes identically.
--
-- The advisor's patterns read (worklet.Reader.WorkPatterns) counts the comments
-- agents wrote and the answers people gave over a window, a range of one kind.
CREATE INDEX tracker_comments_kind_created_idx ON tracker_comments (author_kind, created_at); -- read by Reader.WorkPatterns (patterns.go)
