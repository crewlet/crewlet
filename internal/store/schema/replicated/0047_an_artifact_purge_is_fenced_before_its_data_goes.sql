-- An artifact's purge is fenced before its data goes.
--
-- A purge removes state other domains keep per artifact (its datastore) before
-- the artifact's own rows, and the datastore's removal is irreversible: its
-- deletion marker is never lifted. Judging the purge only before and after
-- that step left a window in which a restore could land between them, leaving
-- a live artifact whose datastore was gone for good; a crash in the same
-- window left the same artifact for anybody to restore.
--
-- So a purge first stamps the artifact on its own subject: `purge_begun_at` is
-- the broker instant the purge was committed to, 0 for an artifact no purge
-- has begun on. From that record on the artifact is in the trash, a restore is
-- refused, and only a purge (the same gesture retried, an administrator's, or
-- the trash reaper's next pass) finishes it.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- artifacts log by its applier, like every other column of the row.
ALTER TABLE artifacts ADD COLUMN purge_begun_at INTEGER NOT NULL DEFAULT 0;

CREATE INDEX artifacts_purging_idx ON artifacts (purge_begun_at) WHERE purge_begun_at > 0; -- the purges the trash reaper finishes
