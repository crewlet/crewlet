-- An event row names the node that PUBLISHED it, which is no longer the node
-- that holds it.
--
-- Until custody (ADR-0025) the two were one: the event log was written by a
-- publish listener inline on the publishing node, so the node a row was READ
-- from was the node it happened on, and every reader that wanted "where did
-- this run" answered it with "whose store answered". A node without the
-- `data` role keeps no event log of its own — its store is scratch — and
-- hands everything it publishes to exactly ONE data node, which writes it into
-- its own log. From then on "whose store answered" names the keeper, and the
-- turn page told an operator debugging a turn that ran on an agent host to
-- read the logs of whichever data node happened to keep its events.
--
-- THE ORIGIN WAS NEVER LOST, only unread: every event carries `node` on its
-- envelope, stamped by the queue of the node that first published it and
-- carried forward by every re-publish (internal/events), and a custody batch
-- carries each event whole. This column is that field, promoted so a reader
-- asks it of the rows rather than of the store that answered.
--
-- WHO HAS TO AGREE ON IT: this node alone, like every column of the audit log.
-- It is a fact about the row, written by the writer from the event it stores
-- (internal/observe's Record, the one function both the publishing node's
-- listener and a custody keeper write through), so a row kept for another
-- node and a row this node published are one shape.
--
-- An empty string, never NULL, for a row no node published: the shape every
-- promoted column has (0015, 0029, 0033). Only an event published through a
-- queue client built with no node — which only a test harness builds — has
-- none.
ALTER TABLE crewlet_events ADD COLUMN node TEXT NOT NULL DEFAULT '';

-- BACKFILLED FROM THE PAYLOAD, as 0015, 0032 and 0033 were, so the rows an
-- upgrade already holds name their origin too. ONLY WHERE THE PAYLOAD IS THE
-- ENVELOPE: a stored row's payload is the whole event — except an inbound
-- delivery's, which is filed with the vendor's own body (internal/observe's
-- delivery), and a vendor body may carry a `node` of its own that names
-- nothing here. The envelope's `id` is the row's own `event_id`, which no
-- vendor body repeats, so that equality is what tells the two apart. A
-- delivery row stays '' here and is stamped on every write from this build
-- on, since the writer reads the envelope rather than the payload.
--
-- ONE STATEMENT, UNBATCHED, for the reason 0029 gives: a migration runs inside
-- Open, before this node serves anything or holds a broker connection, so
-- there is no live append to starve — what it costs is boot time, once.
UPDATE crewlet_events
   SET node = json_extract(payload, '$.node')
 WHERE json_type(payload, '$.node') = 'text'
   AND json_extract(payload, '$.id') = event_id;
