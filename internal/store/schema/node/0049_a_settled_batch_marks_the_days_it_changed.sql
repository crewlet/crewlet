-- usage_rederive — the days a custody settle has changed since this node's
-- usage publisher last derived them again; and usage_watermark — the oldest
-- day the publisher still owes a derivation of on its own account.
--
-- WHO HAS TO AGREE ON THEM: this node alone. Both are facts about which of
-- THIS node's own rows count, and only this node derives and publishes its
-- days (internal/usage, ADR-0020). What every node agrees on is the day the
-- publisher then publishes, which is the usage domain's.
--
-- # usage_rederive
--
-- Every usage count leaves out the rows of a custody batch this node has
-- written and not settled (internal/store/unsettled.go), so the day a node
-- publishes is the rows it keeps and a fleet sums each row once. A batch
-- settled as KEPT therefore changes the days its rows fall on — and a batch
-- can carry a stateless node's events of any earlier day, from a partition
-- that outlasted midnight. The publisher fingerprints only today and
-- yesterday on its own, so a day older than that would keep the value it was
-- last published at for the 181 days the history holds it.
--
-- So the settle records the oldest instant of a usage row it made count, IN
-- ITS OWN TRANSACTION — a crash between a settle and a mark written apart from
-- it would leave rows counting on a day nothing derives again. The publisher
-- derives every day from the oldest mark through the day before yesterday —
-- never one older than the event log's read floor, whose rows may already be
-- swept — and then deletes every mark up to the newest `seq` it read.
--
-- A ROW PER UTC DAY OF THE OLDEST INSTANT, with the newest `seq` of any settle
-- that wrote it. Each settle stamps its row with a `seq` above every row's, so
-- a settle that lands while the publisher derives survives the delete, which
-- takes only what it read — and a settle of today's rows lands on TODAY's row
-- rather than lowering one row the whole table shares: folded into one row, a
-- mark ten days back that a busy keeper's today-settle kept moving would never
-- be cleared, and every tick would derive the ten days again for as long as
-- custody traffic lasted. A row per settle would have the same property and grow by
-- one a second per stateless node for as long as no publisher clears it; a
-- day's settles fold into one row, so the table holds at most the days the
-- event log keeps.
--
-- # usage_watermark
--
-- ONE ROW, `id = 1`: the start of the oldest day whose derivation the
-- publisher has not confirmed published whole — the day before the one it
-- last fingerprinted, or an older day one of its ticks could not settle. A
-- node down across midnight comes back with the day it stopped in older than
-- yesterday, and the tail of that day — written after its last tick — is in
-- no published record. The boot reads this row and derives every day from it
-- again. The publisher rewrites it when it moves, about once a day.
--
-- Before it, a start re-derived only today and yesterday, so a node down
-- across two midnights never published the tail of the day it stopped in;
-- and the day the node stopped in is not found from its NEWEST ROW, which the
-- first custody batch a keeper writes at boot — or the first fire the
-- scheduler's catch-up records — makes today.
--
-- No sweep for either: a mark lives from a settle to the publisher's pass that
-- publishes the days it names, and the watermark is one row.
CREATE TABLE usage_rederive (
    day   INTEGER NOT NULL PRIMARY KEY,
    since INTEGER NOT NULL,
    seq   INTEGER NOT NULL
);

CREATE TABLE usage_watermark (
    id    INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    since INTEGER NOT NULL
);
