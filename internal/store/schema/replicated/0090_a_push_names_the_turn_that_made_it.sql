-- A push names the turn that made it.
--
-- A push row says who pushed and what became of every ref. It now says how the
-- push's pack was built (via: `git`, a client's git sent it and receive-pack
-- took it; `engine`, the engine built the commit and its pack itself from a
-- tool's call) and, for a SEAT's push, the run behind it: the turn that made
-- the write, the work item that turn is charged to — its tracker and the key
-- that tracker shows — and the model it ran on (code.RunProvenance). Each is
-- stamped by the server from the call that made the push and never read from
-- a commit, whose headers and trailers whoever made the commit writes. A seat's
-- row names the seat as both actor and actor_seat, beside actor_kind `agent`;
-- a person's names their credential as actor and the seat they hold.
--
-- EVERY DEFAULT IS '' because SQLite requires one for a NOT NULL column added
-- to a table, and the push apply writes every column of every row. via's is
-- deliberately no way at all: a row whose via is not one this build knows is
-- refused by every reader of it (code's scanPush), so an apply that forgot the
-- column fails a read rather than answering every push the engine built as one
-- a client's git sent, which a default of 'git' would have done.
--
-- WHAT A ROW WEIGHS GROWS WITH THE RUN: code_pushes.row_bytes, which the
-- history's horizon and a purge part are bounded by, counts the four run
-- fields beside the commands and the introduced ids — up to four times
-- code.MaxProvenanceBytes, 256 bytes each.
--
-- code_seat_days is a seat's pushes counted on its company day, which the
-- decide reads to hold the seat to code.limits.pushes_per_seat_day: one row a
-- seat and a day — the day's label on the company calendar, period.Day's, which
-- sorts as text — read by its primary key, one read a seat a batch. A COUNTER
-- RATHER THAN AN INDEX over the push rows: the one-day question answered from
-- code_pushes would cost every seat's push an index entry kept 181 days on every
-- data node (about 1.8 GB at 250 000 pushes a day) and the decide up to a
-- thousand of those entries a seat a batch inside the committer's snapshot,
-- where the counter is at most two rows a seat that ever pushed — the
-- repository row's own idiom, which counts its refs, packs and pushes so no
-- read walks them. Its writer is the push apply alone, counting each agent
-- entry on the day the decide stamped on it, and forgetting a seat's oldest
-- day beyond its two newest as it counts a newer one — the day before the
-- newest kept for a committer whose clock has not reached midnight yet —
-- alike on every node with no clock of its own.
--
-- Class: code_pushes stays reproducible: every column is written only from the
-- records the applier applies. code_seat_days is reproducible
-- (code.ReproducibleTables): written only by the push apply, from the push
-- records, so a replay from zero rebuilds it.
ALTER TABLE code_pushes ADD COLUMN via          TEXT NOT NULL DEFAULT '';
ALTER TABLE code_pushes ADD COLUMN turn_id      TEXT NOT NULL DEFAULT '';
ALTER TABLE code_pushes ADD COLUMN work_backend TEXT NOT NULL DEFAULT '';
ALTER TABLE code_pushes ADD COLUMN work_key     TEXT NOT NULL DEFAULT '';
ALTER TABLE code_pushes ADD COLUMN model        TEXT NOT NULL DEFAULT '';

CREATE TABLE code_seat_days (
    seat   TEXT    NOT NULL,   -- the agent entry's code.Principal.Seat
    day    TEXT    NOT NULL,   -- period.Day's label on the company calendar ('2026-09-23')
    pushes INTEGER NOT NULL,
    PRIMARY KEY (seat, day)
);
