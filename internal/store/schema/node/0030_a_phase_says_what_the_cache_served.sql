-- A phase's prompt-cache counts and the provider slot it ran on become
-- columns, beside the token counts schema/0015 promoted.
--
-- The phase record has carried `cache_read_tokens` and `cache_write_tokens`
-- since the tool loop started counting them, and `provider_key` since it was
-- first published. None of the three reached the rollup: the spend reader
-- selects COLUMNS — that is the whole of 0015, which exists so a month of
-- phases folds without hauling each one's prompt and response across the
-- driver — so a value left in the payload is a value the breakdown reads as
-- zero. The cache share of a company's input read 0% on every screen that
-- asked, which looks exactly like a company whose cache never hits.
--
-- THE CACHE COUNTS ARE A BREAKDOWN OF input_tokens, NEVER AN ADDITION. Every
-- backend reports input_tokens as the full prompt, cached prefix included, so
-- the cache's share is cache_read_tokens / input_tokens and total_tokens stays
-- input plus output. A reader that adds them double-counts the prefix. See
-- internal/tokens' Bucket for the contract as the code states it.
--
-- `provider_key` is IDENTITY rather than cost: WHICH configured provider entry
-- served the call, as distinct from `model`, which is the model that entry
-- reported. A fallback chain serves several models under one key and one
-- model can sit under several keys, so neither answers the other's question.
-- 0015's `model` column already falls back to it when a phase names no model,
-- and that stays: this column is the key itself, never the fallback.
--
-- Three columns on a table where most rows are not phase completions, for the
-- reason 0015 gives for its nine: a byte each on those rows, against a side
-- table's second insert, join and retention sweep.
ALTER TABLE crewlet_events ADD COLUMN cache_read_tokens  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN provider_key       TEXT    NOT NULL DEFAULT '';

-- BACKFILLED FROM THE PAYLOAD, as 0015 was, so an upgrade keeps what the
-- store already recorded. `provider_key` is on every phase record ever
-- published; the cache counts are on the ones a cache-counting build wrote,
-- and absent on the rest — which COALESCE turns into the 0 the column means
-- ("the cache served nothing that anybody reported"), the same answer a
-- reader of the old payload got.
--
-- ONE STATEMENT, UNBATCHED, for the reason 0029 gives: a migration runs inside
-- Open, before this node serves anything or holds a broker connection, so
-- there is no live append to starve — what it costs is boot time, once.
-- Scoped to the one event type that carries a spend, which the column
-- defaults already answer for every other row.
UPDATE crewlet_events SET
    cache_read_tokens  = COALESCE(json_extract(payload, '$.cache_read_tokens'), 0),
    cache_write_tokens = COALESCE(json_extract(payload, '$.cache_write_tokens'), 0),
    provider_key       = COALESCE(json_extract(payload, '$.provider_key'), '')
WHERE event_type = 'agent_phase_completed';
