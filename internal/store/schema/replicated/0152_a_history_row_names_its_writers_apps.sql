-- A history row names its writer's apps.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_TRACKER_LOG stream by its one applier, as the rest of tracker_history.
--
-- `actor_apps` is the record's own `actor_apps` (a credential's apps when it
-- wrote, as a JSON list), '' for every other writer. The work pickup reads it
-- off an item's last assignment, so the turn it starts is held to the apps of
-- the credential that assigned the work (ADR-0109, ADR-0112).
ALTER TABLE tracker_history ADD COLUMN actor_apps TEXT NOT NULL DEFAULT '';
