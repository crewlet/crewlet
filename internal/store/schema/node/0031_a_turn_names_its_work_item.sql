-- The work item a turn is charged to becomes a column of the event log, and
-- the episode column that was meant to hold it gets its real name.
--
-- WHAT A COLUMN BUYS HERE is the question "everything that happened on this
-- item". Every turn-level record — agent_turn_started, each
-- agent_phase_completed, agent_turn_completed, turn_completed, a coding run's
-- start and its clarification — carries `work_item{backend, id, key,
-- project}`, and a filter on it through the payload is a json_extract over
-- every row in the window, which is what schema/0015 exists to stop the reads
-- here doing. The column holds the item's IDENTITY across trackers,
-- `<backend>:<id>` (types.WorkItem.Ref) — never the key, which a move or a
-- project rename rewrites, and never the bare id, which two trackers can both
-- mint.
--
-- WHO HAS TO AGREE ON IT: this node alone. It is a column of the node's own
-- audit log, derived by the writer from the event it stores (store.ExtractTags'
-- one nested rule), exactly as turn_id and work_key are.
--
-- An empty string, never NULL, for a row on no item: the same shape every
-- promoted column has (0015, 0029, 0030), so a reader compares against ''
-- and the partial index below leaves those rows out.
ALTER TABLE crewlet_events ADD COLUMN work_item TEXT NOT NULL DEFAULT '';

-- BACKFILLED FROM THE PAYLOAD, as 0015 and 0030 were, so the rows an upgrade
-- already holds answer the filter too. Only a record naming BOTH halves of the
-- identity is given one: a backend with no id is not an item, and writing
-- `native:` would make every such row one item. The payload is read rather
-- than the stored tags because no build before this one extracted the tag.
--
-- ONE STATEMENT, UNBATCHED, for the reason 0029 gives: a migration runs inside
-- Open, before this node serves anything or holds a broker connection, so
-- there is no live append to starve — what it costs is boot time, once.
UPDATE crewlet_events
   SET work_item = json_extract(payload, '$.work_item.backend') || ':' ||
                   json_extract(payload, '$.work_item.id')
 WHERE json_type(payload, '$.work_item') = 'object'
   AND COALESCE(json_extract(payload, '$.work_item.backend'), '') <> ''
   AND COALESCE(json_extract(payload, '$.work_item.id'), '') <> '';

-- THE ITEM'S HISTORY IS A RANGE SEEK: (work_item, event_time) is the order the
-- turns list's item filter and `/events?work_item=` read in. PARTIAL over the
-- rows that name one, the shape 0029 gave work_key, because most of the log —
-- every chat delivery, every webhook, every fleet event — names no item and an
-- index entry for each of them would be all cost.
CREATE INDEX IF NOT EXISTS crewlet_events_work_item_idx
    ON crewlet_events (work_item, event_time, event_id)
    WHERE work_item <> '';

-- The episode column is renamed, not added beside. `task_id` was declared in
-- schema/0002 and never held a value until the build before this one began
-- storing the turn's work-item ref in it; a column named for a worker's task
-- that holds a tracker item is the misreading internal/events/types'
-- TestWorkItemIsNeverReadFromTaskID exists to stop.
--
-- THE MEMORY CHANGELOG CARRIES COLUMNS BY NAME (internal/learning/memsync), so
-- the rename is a change to a payload two builds share, and it is additive in
-- both directions without a second spelling: a row an older build publishes
-- carries `task_id`, which this build does not carry and skips — and every
-- such row that build ever wrote holds '' there, because no released build
-- put anything in it — while a row this build publishes carries `work_item`,
-- which an older build does not know and leaves at its own column's default.
ALTER TABLE episodes RENAME COLUMN task_id TO work_item;
