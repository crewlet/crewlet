-- work_key — the identity of the unit of work a turn did.
--
-- Closes the last unfenced seat-scoped write. Two nodes can both
-- complete a turn for one trigger: a zombie whose loop stalls past its
-- lease and finishes between fence checks, or a genuine re-run after
-- the completion ledger fails open (which it does deliberately — not
-- knowing whether work was done has one safe answer). The outbound
-- effects of that are visible and recoverable; a doubled Slack post is
-- obvious. These two writes are neither:
--
--   * `episodes` — one row per turn, keyed on nothing stable, so the
--     duplicate simply lands. It is then retrieved by every later
--     turn's episode recall and feeds skill synthesis, quietly
--     weighting the agent's behaviour with an event that happened once.
--   * `counterparty_profiles.interaction_count` — an unconditional
--     `+ n`, so a duplicate turn counts one interaction twice and the
--     profiler's cadence logic reads a person as twice as chatty as
--     they are.
--
-- The fix is a key on the WORK rather than a fence on the WRITER. See
-- `crewlet.work_key` for why: an insert has no prior row to hang
-- `WHERE owner_epoch = $epoch` on, and a fence loses the row in the
-- case where nothing went wrong (a node that completes, acks, and only
-- then lapses would have its episode refused — the turn happened and
-- the memory of it is gone).
--
-- Additive and backfill-free. Existing rows keep `work_key = ''` and
-- are never deduplicated against — a backfill would have to invent an
-- identity for turns whose triggers are long since swept. An empty key
-- also skips the exclusion entirely on the write path, which is what
-- lets a turn with no ledgerable trigger (a scheduled fire, a
-- sub-agent, a sandbox resume that runs in a later task than the
-- dispatch) keep writing at full speed: it has no cross-node duplicate
-- to collapse, so it pays nothing for the guard.
--
-- Mixed-version safe in both directions. An older build never selects
-- either column and inserts `work_key = ''`, so it keeps writing
-- exactly as it did and a newer peer's keyed rows never collide with
-- it. No protocol bump — an old node does not deduplicate, which is
-- the behaviour it already had.

ALTER TABLE episodes
    ADD COLUMN IF NOT EXISTS work_key TEXT NOT NULL DEFAULT '';

-- NOT a unique index, and the reason is worth writing down because the
-- obvious fix does not compile here. `episodes` is a TimescaleDB
-- hypertable partitioned on `ended_at` (see 002_episodes.sql), and a
-- hypertable requires the partitioning column in EVERY unique index.
-- `UNIQUE (agent_handle, work_key)` is rejected outright, and the
-- version Timescale would accept — `(ended_at, agent_handle, work_key)`
-- — deduplicates nothing, because the two duplicate turns end at
-- different times and would land in different chunks.
--
-- So exclusion comes from the write path instead: the insert takes a
-- transaction-scoped advisory lock on (agent_handle, work_key) and
-- inserts WHERE NOT EXISTS. `WHERE NOT EXISTS` alone is not enough —
-- it is evaluated under the statement's snapshot, so two concurrent
-- transactions can both see "not there" and both insert. The lock is
-- what serializes them; being transaction-scoped, it releases on
-- commit with no unlock bookkeeping and survives a killed process.
-- See `EpisodeStore.write`.
--
-- Per AGENT, not global: two seats legitimately act on the same trigger
-- (a broadcast, a task assigned to a unit), and each one's episode is
-- its own memory.
CREATE INDEX IF NOT EXISTS episodes_agent_work_key_idx
    ON episodes (agent_handle, work_key)
    WHERE work_key <> '';

-- The last work unit counted into `interaction_count`. One column
-- rather than a side table: the duplicate this guards against is always
-- the IMMEDIATELY preceding write (two nodes racing, or a redelivery),
-- never one from a week ago, so remembering the last key is exactly as
-- much history as the guard can use.
ALTER TABLE counterparty_profiles
    ADD COLUMN IF NOT EXISTS last_work_key TEXT NOT NULL DEFAULT '';
