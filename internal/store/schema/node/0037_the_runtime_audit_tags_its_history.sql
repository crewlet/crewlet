-- The runtime audit's rows an upgrade already holds get the tags the writer
-- now promotes on every new one.
--
-- WHY A BACKFILL rather than the discontinuity the writer's other late tags
-- accept (store.tagKeys' `notification_source`): those tags feed a count, and
-- a count that starts on the day the tag did is a count that says so. These
-- feed ROWS a person reads side by side — the Audit log's Runtime source and
-- the backup history — and a row written a day before the upgrade drew the
-- token's name where the next one draws the person it is bound to, "tool
-- call" where the next names the tool, and no directory and no host for a
-- backup whose host is the one fact its record exists to keep. Nothing is
-- lost to recover: the whole event is in `payload`, and each of these is a
-- top-level field of it, exactly what store.ExtractTags reads on a new row.
--
-- WHO HAS TO AGREE ON IT: this node alone. The rows are this node's own audit
-- log, derived by the writer from the event it stores.
--
-- ONLY THE RUNTIME AUDIT'S TYPES (types.RuntimeAuditTypes: operator_acted,
-- backup_requested) and only where the tag is absent. `node` means the same
-- on every type, but these rows are the only ones any reader reads it from,
-- and rewriting the tags of every event in the log would make one boot pay
-- for a column nobody asks of the rest. A value the payload does not carry as
-- a non-empty string is left absent, as ExtractTags leaves it.
--
-- ONE STATEMENT PER TAG, UNBATCHED, for the reason 0029 gives: a migration
-- runs inside Open, before this node serves anything, and the rows are the
-- few a person's own calls produced.
UPDATE crewlet_events
   SET tags = json_set(tags, '$.node', json_extract(payload, '$.node'))
 WHERE event_type IN ('operator_acted', 'backup_requested')
   AND json_type(payload, '$.node') = 'text'
   AND json_extract(payload, '$.node') <> ''
   AND json_type(tags, '$.node') IS NULL;

UPDATE crewlet_events
   SET tags = json_set(tags, '$.actor_seat', json_extract(payload, '$.actor_seat'))
 WHERE event_type IN ('operator_acted', 'backup_requested')
   AND json_type(payload, '$.actor_seat') = 'text'
   AND json_extract(payload, '$.actor_seat') <> ''
   AND json_type(tags, '$.actor_seat') IS NULL;

UPDATE crewlet_events
   SET tags = json_set(tags, '$.tool', json_extract(payload, '$.tool'))
 WHERE event_type = 'operator_acted'
   AND json_type(payload, '$.tool') = 'text'
   AND json_extract(payload, '$.tool') <> ''
   AND json_type(tags, '$.tool') IS NULL;

UPDATE crewlet_events
   SET tags = json_set(tags, '$.dir', json_extract(payload, '$.dir'))
 WHERE event_type = 'backup_requested'
   AND json_type(payload, '$.dir') = 'text'
   AND json_extract(payload, '$.dir') <> ''
   AND json_type(tags, '$.dir') IS NULL;
