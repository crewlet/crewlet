-- A queue's ejections are read across every repository.
-- WHO HAS TO AGREE ON IT: every data node, identically; code_queue stays reproducible, and an index adds no row.
-- About 50 bytes an ejected entry, kept while the queue's history keeps it; the record that ejects an entry writes one entry, its sweep or the purge removes it, and landings and dequeues write nothing.
CREATE INDEX code_queue_ejected_recent_idx ON code_queue (ended_at, repo_id)
    WHERE state = 'ejected';    -- every repository's ejections by their ending: a queue read of every repository asking for its trains (queueread.go)
