-- leases — the cross-process ownership primitive.
--
-- One row per owned resource.  A node claims a resource by winning the
-- conditional upsert in `crewlet.db.leases.LeaseStore.try_acquire`, keeps
-- it by renewing before `expires_at`, and loses it by crashing (TTL
-- expiry) or releasing.  This is what lets more than one Crewlet process
-- run against one company: a seat's turns, its MCP children and its inbox
-- consumer all belong to whoever holds `seat:{handle}`, and the
-- per-company singleton duties (scheduler tick, skill clustering,
-- curator, sandbox waiter) each belong to whoever holds `worker:{duty}`.
--
-- Why a lease and not `pg_advisory_lock`: an advisory lock has no TTL and
-- no fencing token.  A holder that is hung-but-connected blocks the fleet
-- forever, while one that dies drops the lock instantly with no way for
-- the successor to know the predecessor may still be mid-write.  A lease
-- bounds the first case and *detects* the second.
--
-- `epoch` is the fencing token, and it is the reason this table is not
-- just a mutex.  It increments on every ownership change, and writes made
-- on behalf of a resource carry `WHERE owner_epoch = $current` — so a
-- zombie that wakes after losing its lease bounces off the predicate
-- instead of resurrecting a run or double-spending a budget.  Crucially,
-- the epoch also increments when the SAME owner re-acquires after its
-- lease lapsed: during the gap its own in-flight work is no longer
-- covered, so it must be fenced against itself.  Only an unbroken
-- same-owner hold keeps the epoch stable.
--
-- The counter must be monotonic for the lifetime of the RESOURCE, not of
-- a row, which is why releasing a lease expires it in place (`expires_at
-- = now(), owner = ''`) instead of deleting it.  A deleted row would send
-- the next acquirer back to epoch 1 — the exact token a zombie from the
-- released tenure is still fencing its writes with.
--
-- `owner` identifies a process INCARNATION, not a machine: a live lease
-- is renewable by its own owner string, so two processes sharing one
-- would both hold the resource at the same epoch (and the default node id
-- is the shared constant `node-0`).  Callers pass
-- `{node_id}:{random}` — see `crewlet.config.resolve_node_incarnation`.
--
-- `preferred` is a placement hint, not a rule: it carries the STABLE node
-- id of the acquirer, survives a graceful release, and lets a rolling
-- deploy tend to land seats back where their warm state (MCP children,
-- caches) already is.  Claim logic may ignore it; correctness never
-- depends on it.
--
-- `protocol` gates mixed-version fleets.  During a rolling upgrade a vN
-- and a vN+1 node share this table; a node that would change the meaning
-- of a claim refuses to take one while an older-protocol holder is live.
-- Schema evolution here is additive-only for the same reason.

CREATE TABLE IF NOT EXISTS leases (
    resource    TEXT PRIMARY KEY,
    owner       TEXT        NOT NULL,
    epoch       BIGINT      NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    preferred   TEXT        NOT NULL DEFAULT '',
    protocol    INT         NOT NULL DEFAULT 1,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- "What do I still own?" on every heartbeat tick, and "what is claimable?"
-- on every acquire sweep — both are owner/expiry scans, not PK lookups.
CREATE INDEX IF NOT EXISTS leases_owner_idx ON leases (owner);
CREATE INDEX IF NOT EXISTS leases_expires_at_idx ON leases (expires_at);
