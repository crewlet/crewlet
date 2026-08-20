-- turn_completions — "has this trigger already been worked?"
--
-- A seat's inbox delivers at-least-once. If the owning node dies after a
-- turn finished but before the delivery was acked, the trigger is
-- redelivered to the seat's next owner, which re-runs the whole turn and
-- duplicates every outbound effect the first attempt already emitted:
-- the Slack reply, the Jira comment, the git push.
--
-- What this is NOT, deliberately: a claim. The design this replaced was
-- a state machine with an `in_progress` state, and five of five
-- reviewers returned `redesign-needed` on it for one root reason —
-- `in_progress` provides no exclusion the seat lease does not already
-- provide. A seat has exactly one consuming node, and that consumer is
-- serial. The only defined disposition for a stale `in_progress` row was
-- "supersede and re-run", which is exactly what you do with no row at
-- all; every other defect in that design (an undefined third state, an
-- unfenced delete, a claim-then-defer path, an unimplementable resume
-- exemption) existed only to service a state that carried no guarantee.
--
-- So: no `state`, no `expires_at`, no supersede rule. One row per
-- trigger that COMPLETED, written at the end of the turn, read before
-- the next one starts.
--
-- `trigger_id` is a CONSTITUENT inbox event id, never a derived one.
-- Multi-event partitions are coalesced into a single digest before a
-- turn runs, and the digest is minted fresh on every coalesce — so a key
-- taken from it would differ on every redelivery and match nothing. The
-- constituents' ids are the only identity that survives the retry
-- boundary. Recording them individually also means a re-delivered
-- partition that overlaps a previous one only partially (A+B first, then
-- A+B+C) short-circuits A and B and runs C, which a single-key design
-- could not express at all.
--
-- `node` is the process INCARNATION, not the node id — the same identity
-- the lease table holds, for the same reason (see 019_leases.sql).
-- `owner_epoch` is the fencing token the turn ran under. Neither is read
-- by the engine: they are here so an operator asking "which node
-- answered this, and under which claim?" has an answer.

CREATE TABLE IF NOT EXISTS turn_completions (
    seat         TEXT        NOT NULL,
    trigger_id   TEXT        NOT NULL,
    turn_id      TEXT        NOT NULL DEFAULT '',
    node         TEXT        NOT NULL DEFAULT '',
    owner_epoch  BIGINT      NOT NULL DEFAULT 0,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (seat, trigger_id)
);

-- Retention is a range delete over completed_at, run by the
-- MaintenanceWorker. Without this index it degrades to a full scan on a
-- table that gains a row for every trigger the company ever handles.
CREATE INDEX IF NOT EXISTS turn_completions_completed_at_idx
    ON turn_completions (completed_at);
