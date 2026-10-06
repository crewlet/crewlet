-- A spend row says how many PROVIDER CALLS it covers, and an auxiliary one
-- says whose cost it is.
--
-- TWO SPEND TYPES NOW. Every completion the seat's auxiliary model made — the
-- turn-start memory filter, knowledge query and episode summary, each
-- compaction rewrite, the reflection workers, the background passes, a
-- person's answered question — was charged to the token counters and reached
-- no row, so every spend figure folded from this table understated the
-- counters by exactly that. Those calls are now `auxiliary_spend` rows, one per
-- (stage, seat or person, turn, purpose, model, provider entry, company day)
-- per flush of the publishing node's ledger, and they fill the spend columns
-- 0015 and 0032 promoted beside `agent_phase_completed`'s: phase `auxiliary`,
-- the purpose in `worker`, the tokens.
--
-- `calls` IS PROVIDER CALLS, ON EVERY SPEND ROW. A rollup's "N calls" counted
-- ROWS, which was one unit while one type wrote them — one phase per row —
-- and becomes two the moment a coalesced row stands for seventy rewrites: a
-- forty-round executor would read "1 call" beside it. So the column counts the
-- model calls a row covers, and the rollups sum it. A phase row's calls are its
-- rounds: one `rounds[]` entry per provider call where the phase recorded
-- them, its `rounds_used` where it predates that list, and one where neither
-- says (a judge's single call, a coding run collected whole, whose own calls
-- happened inside a CLI this engine does not see). Every other row is 0, the
-- column's default: it is no call.
--
-- `spend_stage` is the auxiliary row's STAGE — `turn`, `reflection`,
-- `background`, `operator` — and empty on every other row. It is a column
-- because one read keys on it: a turn's cost is its phases and its IN-TURN
-- auxiliary spend, and a reflection's rows carry the turn's id (the turn's page
-- draws them) without being part of what the turn cost, so the turn list's
-- sums leave them out by this column rather than by opening every payload in
-- the group.
--
-- WHO HAS TO AGREE ON IT: this node alone, like the rows it describes — the
-- event log is the node's own estate, and a fleet's history is asked of every
-- node at read time (ADR-0021). An older peer asked for phase tokens answers
-- without either value, which a reader takes as one call of a phase.
--
-- BACKFILLED FROM THE PAYLOAD, in one statement, for the reason 0032 gives: a
-- migration runs inside Open before this node serves anything, so what it
-- costs is boot time, once, and only over the phase rows.
ALTER TABLE crewlet_events ADD COLUMN calls       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN spend_stage TEXT    NOT NULL DEFAULT '';

UPDATE crewlet_events SET calls = CASE
        WHEN COALESCE(json_array_length(payload, '$.rounds'), 0) > 0
            THEN json_array_length(payload, '$.rounds')
        WHEN COALESCE(json_extract(payload, '$.rounds_used'), 0) > 0
            THEN json_extract(payload, '$.rounds_used')
        ELSE 1
    END
WHERE event_type = 'agent_phase_completed';
