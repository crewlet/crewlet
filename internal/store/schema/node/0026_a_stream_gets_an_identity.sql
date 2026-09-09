-- What this node has seen of a durable stream's own identity.
--
-- # The failure it closes
--
-- A stream that is deleted and remade restarts its sequences at 1, and the new
-- stream is EMPTY. A consumer that reads it sees nothing and reports nothing —
-- not an error, not a refusal, but a successful read of a stream that has
-- forgotten everything. On the memory changelog that is every seat on the node
-- silently losing its diary and its episodes, with a `memory_hydrated` line
-- saying zero rows and nothing anywhere saying why.
--
-- The broker's own creation instant is the only thing that detects it. A
-- sequence cannot, because a recreated stream's sequences are perfectly
-- plausible; a message count cannot, because an empty stream and an emptied
-- one are the same count.
--
-- # Why here rather than in the replicated estate
--
-- This is a PER-NODE OBSERVATION and not shared state: two nodes can
-- legitimately have seen a stream at different moments, and a donated snapshot
-- must not carry the donor's answer. `statelog_cursor` — the framework's own
-- checkpoint, which carries the same instant for a DOMAIN — is in the
-- replicated estate and is walked by both the backup manifest and the adoption
-- path, each of which reads every row as a domain. A row there for a stream
-- that is not a domain would put a phantom domain in an artefact's manifest,
-- and a recipient refuses an artefact naming a domain it does not register.
--
-- # Why it is not one row per consumer
--
-- The identity is the STREAM's, not a reader's. Two consumers of one stream
-- that disagreed about whether it had been recreated would be two answers to a
-- question with one, and the second reader to notice would repair the first's
-- record without either of them acting on it.
CREATE TABLE stream_identity (
    stream     TEXT    NOT NULL PRIMARY KEY,
    -- The broker's own creation instant for that stream, as this node last saw
    -- it. Zero is never written: an instant nobody could establish is UNKNOWN,
    -- which is a different fact from a stream this node has never seen, and
    -- storing it would collapse the two.
    created_at INTEGER NOT NULL,
    -- When this node last confirmed it, for an operator reading a node that
    -- refuses to hydrate.
    seen_at    INTEGER NOT NULL
);
