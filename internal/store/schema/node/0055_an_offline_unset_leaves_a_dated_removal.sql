-- secret_removals — a secret an operator removed while this node's engine was
-- stopped: the name, when, by whom and through which path. The bootstrap half
-- of the secret store's OTHER direction, beside secret_values.
--
-- WHO HAS TO AGREE ON IT: this node alone, until the engine's next start
-- carries it onto the fleet's store (internal/fleetsecrets.Migrate) and
-- forgets it — exactly as secret_values is this node's alone until then.
--
-- # Why a removal is a row
--
-- `crewlet secrets unset` on a stopped node cannot reach the fleet's store,
-- whose broker is inside the engine's process, and it used to delete this
-- node's own row and nothing else: the fleet's value, which every node reads,
-- survived the unset, and the next start migrated nothing because a deleted
-- row is not one a migration can see. So the removal is RECORDED, with the
-- instant it was made, and the next start removes the fleet's value when the
-- removal is the later write and keeps it — saying so — when somebody wrote
-- the name through a running node afterwards: the later-wins rule an offline
-- write is carried by.
--
-- A NAME IS IN AT MOST ONE OF THE TWO TABLES: a removal deletes this node's
-- own value in the same transaction and a later write deletes the removal, so
-- the last thing the operator did to a name is the one that travels.
--
-- NO INDEX beyond the key: the boot migration reads every row once, and a
-- stopped node's table holds a handful.
CREATE TABLE secret_removals (
    name            TEXT    NOT NULL PRIMARY KEY,
    removed_at      INTEGER NOT NULL,
    removed_by      TEXT    NOT NULL,
    removed_by_kind TEXT    NOT NULL,
    operator_id     TEXT    NOT NULL,
    source          TEXT    NOT NULL
);
