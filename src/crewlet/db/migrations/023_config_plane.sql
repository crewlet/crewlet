-- config_activations / config_apply_status — the control plane.
--
-- Config activation used to be delivered over COMPETING-consumer Pulsar
-- subscriptions (groups `engine-config` and `api-config`) into per-process
-- memory, with no reconcile loop anywhere. With N processes exactly ONE
-- applied a revision and the rest ran the previous company forever:
-- deleted roles kept answering Slack, rotated credentials kept being used,
-- and the dashboard reported success because the one node that applied it
-- published `revision_applied(ok)`.
--
-- Two tables replace that.
--
-- `config_activations` is the authoritative pointer, and it is an APPEND
-- log rather than a column on company_config, because the counter has to
-- move on every activation INCLUDING re-activation of an unchanged
-- revision. That is the documented gesture for picking up a rotated
-- credential (see docs/concepts/secret-store.md), so a pointer keyed on
-- revision id could never express it. `BIGSERIAL` is the monotonic epoch;
-- the current target is simply MAX(epoch).
--
-- `config_apply_status` is what each node has actually managed to do, and
-- it is what makes partial apply visible. Three outcomes, not two:
--
--   ok        — applied cleanly.
--   error     — failed and rolled back; the node still serves the prior
--               epoch, which is a legitimate degraded-but-correct state.
--   degraded  — failed AFTER a restart-required subsystem was mutated.
--               Rollback is synchronous and cannot respawn MCP children
--               or restart stopped transports, so this node's declared
--               epoch is a LIE: it reports the prior config while its
--               tool surface is amputated and its inbound path may be
--               dead. Never treated as converged.
--
-- Reading these two together is what lets a node distinguish "I am behind
-- because propagation takes a moment" from "I am behind because I cannot
-- apply this" — which need opposite responses.

CREATE TABLE IF NOT EXISTS config_activations (
    epoch        BIGSERIAL PRIMARY KEY,
    revision_id  UUID        NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    summary      TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS config_apply_status (
    node_id     TEXT        PRIMARY KEY,
    epoch       BIGINT      NOT NULL,
    revision_id UUID,
    status      TEXT        NOT NULL,
    error       TEXT        NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT config_apply_status_status_known
        CHECK (status IN ('ok', 'error', 'degraded'))
);

-- "Which peers applied this epoch, and how did it go" is the query the
-- shed decision runs on every reconcile tick.
CREATE INDEX IF NOT EXISTS config_apply_status_epoch_idx
    ON config_apply_status (epoch, status);
