-- config_activations / config_apply_status — the control plane.
--
-- Config activation used to reach processes over COMPETING-consumer
-- subscriptions, so with N processes exactly ONE applied a revision and
-- the rest ran the previous company forever: deleted roles kept
-- answering chat, rotated credentials kept being used, and the dashboard
-- reported success because the one node that applied it said so.
--
-- Two tables replace that, and neither is a delivery mechanism. The
-- pointer is authoritative and every node polls it; every node records
-- what it managed to do.


-- config_activations — the authoritative pointer.
--
-- An APPEND LOG rather than a column on company_config, because the
-- counter has to move on every activation INCLUDING re-activation of an
-- unchanged revision. That is the documented gesture for picking up a
-- rotated credential, so a pointer keyed on revision id could never
-- express it — it would rebuild nothing on exactly the operation an
-- operator performs to make the fleet re-resolve its secrets.
--
-- AUTOINCREMENT rather than a bare INTEGER PRIMARY KEY: the epoch is a
-- monotonic fence every node compares against its own applied epoch, and
-- a rowid that can be REUSED after a delete would let a new activation
-- appear older than one a node has already applied. Nothing deletes from
-- this table today, which is precisely the kind of assumption that stops
-- being true quietly.
CREATE TABLE config_activations (
    epoch        INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    revision_id  TEXT    NOT NULL,
    activated_at INTEGER NOT NULL,
    summary      TEXT    NOT NULL DEFAULT ''
);


-- config_apply_status — what each node actually managed, and what makes a
-- PARTIAL apply visible. Three outcomes, not two:
--
--   ok        — applied cleanly.
--   error     — failed and rolled back; the node still serves the prior
--               epoch, which is a legitimate degraded-but-correct state
--               and safe to route work to.
--   degraded  — failed AFTER a restart-required subsystem was mutated.
--               Rollback restores and restarts transports but cannot
--               un-respawn the per-role MCP children the failed revision
--               already started, so this node reports the prior config
--               while its tool surface may be amputated. NEVER counts as
--               converged, and never counts as a healthy peer.
--
-- Reading the two tables together is what lets a node tell "I am behind
-- because propagation takes a moment" from "I am behind because I cannot
-- apply this" — which need opposite responses.
--
-- Keyed by node, not by event, so a row is a node's LAST WORD rather than
-- a history. That is why reads bound it on freshness: a scaled-in pod
-- leaves its `ok` behind forever, and counting that ghost makes a
-- diverged survivor shed its seats to a node that no longer exists.
CREATE TABLE config_apply_status (
    node_id     TEXT    NOT NULL PRIMARY KEY,
    epoch       INTEGER NOT NULL,
    revision_id TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL,
    error       TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    CHECK (status IN ('ok', 'error', 'degraded'))
);

-- "Which peers applied this epoch, and how did it go" is the query the
-- shed decision runs on every reconcile tick.
CREATE INDEX config_apply_status_epoch_idx
    ON config_apply_status (epoch, status);

-- The sweep drops nodes that stopped reporting.
CREATE INDEX config_apply_status_updated_at_idx
    ON config_apply_status (updated_at);
