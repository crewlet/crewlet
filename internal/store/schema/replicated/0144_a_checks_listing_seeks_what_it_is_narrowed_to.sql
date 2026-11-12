-- A checks listing seeks what it is narrowed to.
--
-- A `checks` read narrows a repository's listing and the window by a check's
-- name and a state (gitlet.CheckQuery), and both were tested on the rows a page
-- had READ: a repository's walk of code_checks_finished_idx, or the window's of
-- code_checks_recent_idx, each in the order the listing answers. So a narrowed
-- page was the rows its walk read, and the walk held the history: a
-- repository's attempts still running sort after every attempt that ended in
-- the thirty days its rows are kept (gitlet.CheckHistory), and `state=running`
-- answered empty pages, each with a cursor, until the walk had passed all of
-- them — some 2,400 requests at the default page for a repository ending
-- 4,000 attempts a day — and the window narrowed to a failed check, or to one
-- rarely run, cost a request per page of every ending in it. What a narrowed
-- page cost followed the history, never what it answered.
--
-- So each narrowing is a SEEK of an index that leads with it, in the order the
-- listing answers, the cursor's place a keyset after its equalities:
--
-- code_checks_state_idx is a repository's attempts in one state, newest ended
-- first and those not yet ended (finished_at 0) last — the order of its
-- unnarrowed listing — and it holds EVERY state, so `state=running` and
-- `state=queued` are seeks as `state=failed` is.
--
-- code_checks_name_idx is a repository's attempts of one check in one state,
-- in the same order. A listing narrowed by a name and a state is one seek of
-- it; by a name alone, one seek a state, their places merged
-- (gitlet.narrowedPlaces): seven seeks of at most a page's places each, which
-- spares a third index ordered by the name alone.
--
-- code_checks_state_recent_idx and code_checks_name_recent_idx are the
-- window's two, over every repository's attempts by when they ended, PARTIAL
-- on the ended rows as code_checks_recent_idx is: the window lists the
-- attempts that ended, and refuses a state that is not an ending.
--
-- WHAT THEY COST. The two window indexes are written once an attempt, when it
-- ends, and again only when a later ending replaces that one — as
-- code_checks_recent_idx is. The two over a repository's attempts hold every
-- state, so they are written by each of an attempt's three records (queued,
-- claimed, ended), as code_checks_finished_idx is: the queued and running rows
-- they hold are what a repository's listing narrowed to those states seeks,
-- and a partial pair beside a third index for the live rows would cost the
-- same entries. Each holds the key's columns and the ending, some 150 bytes
-- an attempt against a row that carries up to 4 KiB of summary.
--
-- Class: code_checks stays REPRODUCIBLE: an index adds no row.

CREATE INDEX code_checks_state_idx ON code_checks (repo_id, state, finished_at, commit_oid, target_ref, name, attempt);    -- a repository's attempts in one state, newest ended first: its checks listed narrowed by a state

CREATE INDEX code_checks_name_idx ON code_checks (repo_id, name, state, finished_at, commit_oid, target_ref, attempt);    -- a repository's attempts of one check in one state, newest ended first: its checks listed narrowed by a check's name

CREATE INDEX code_checks_state_recent_idx ON code_checks (state, finished_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE finished_at > 0;    -- every repository's attempts in one ending by when they ended: the window narrowed by a state

CREATE INDEX code_checks_name_recent_idx ON code_checks (name, state, finished_at, repo_id, commit_oid, target_ref, attempt)
    WHERE finished_at > 0;    -- every repository's attempts of one check in one ending by when they ended: the window narrowed by a check's name
