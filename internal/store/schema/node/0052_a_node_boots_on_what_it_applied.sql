-- A node's config history says three things apart: what the node BOOTS on,
-- the FLEET's revision as the node last saw it, and an OFFLINE choice not yet
-- published.
--
-- One column, is_active, used to say all three, and each reader took it for
-- the one it needed. So a peer marked the fleet's revision active as it
-- fetched it, before it had validated or built it, and a node that refused
-- that revision booted on it at its next start and was refused before the
-- engine ran. A write through the API marked its revision active the moment
-- the pointer moved, and a reconciler that had read the pointer a moment
-- earlier walked it back. And an offline command marked a revision active at
-- the current time, which the next boot published over whatever the fleet had
-- moved on to.
--
-- WHO HAS TO AGREE ON IT: this node alone, like every other column of this
-- table. The company is the activation pointer in coordination; these are the
-- node's own facts about it.
--
--   * is_active keeps its name: it is the revision this node BOOTS on — the
--     last one its engine applied, or one an offline command chose. Online,
--     only the reconciler moves it, and only after an apply succeeds.
--   * is_fleets is the newest pointer revision this node's reconciler opened,
--     whether this build could run it or not. It is what a node restores when
--     the pointer is gone, and what an offline command builds on.
--   * offered, offered_over and offered_at mark a boot row an offline command
--     chose and the fleet has not taken yet: the revision it was built on
--     (NULL for a change built on no company) and the instant the boot publish
--     planned for it. The next start publishes it only over that base — or
--     over offered_unconfirmed, the earlier offer it replaced while a start's
--     publish of that one had not been confirmed: it was built on it, and the
--     fleet runs it if that publish landed.
--   * served_at is the boot row's alone: the instant of the activation this
--     node applied, which the engine stamps its chart with when it boots.
--     activated_at is every row's latest activation this node knows of, the
--     boot row's included. They were one column, and a re-activation this
--     node refused could raise neither: the boot row's instant had to stay the
--     one it applied, so the fleet's newer one was lost.
ALTER TABLE company_config ADD COLUMN is_fleets INTEGER NOT NULL DEFAULT 0;
ALTER TABLE company_config ADD COLUMN offered INTEGER NOT NULL DEFAULT 0;
ALTER TABLE company_config ADD COLUMN offered_over TEXT;
ALTER TABLE company_config ADD COLUMN offered_unconfirmed TEXT;
ALTER TABLE company_config ADD COLUMN offered_at INTEGER;
ALTER TABLE company_config ADD COLUMN served_at INTEGER;

-- At most one row is the fleet's, enforced by the database, for the reason
-- company_config_one_active_idx exists: two would make the answer depend on
-- which a query returned first.
CREATE UNIQUE INDEX company_config_one_fleets_idx
    ON company_config (is_fleets) WHERE is_fleets <> 0;
