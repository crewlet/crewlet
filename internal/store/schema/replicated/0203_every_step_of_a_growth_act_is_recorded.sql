-- The access log records every step of a growth act (ADR-0171): a read or
-- operate call made at once, and a held public, spend or admin call's ask, its
-- answer and its outcome, beside the SSH records (0151) and the verdicts
-- (0161). internal/accessdomain is the domain.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ACCESS_LOG stream by its applier, as every access table is: the
-- Growth page's recent changes and a vendor's rate rule are answered by
-- whichever data node is reached.
--
-- access_growth: one row per act and phase, created once and never updated.
-- `agent_handle` is the seat that called; `growth_id` is derived from the act
-- (`held_call`, or `call_id` for a call made at once) and the step's phase
-- (accessdomain.GrowthID), so a held call holds one answer and one outcome.
-- `operation_digest` and `before_digest` are SHA-256 hex of the exact vendor
-- operation and the target's before-state. `changes` is a JSON array of the
-- changed fields, a published text's values kept as their SHA-256 and length
-- alone. The money columns are micros of `currency`, '' for a step that spends
-- nothing. `via` is the vendor role of the credential a product holding
-- several made the call through, never the credential. `failure` is a closed
-- word. No column holds a token, a credential, a person's personal data or
-- published text: the writer refuses them.
CREATE TABLE access_growth (
    agent_handle          TEXT    NOT NULL,
    growth_id             TEXT    NOT NULL,
    turn_id               TEXT    NOT NULL DEFAULT '',
    call_id               TEXT    NOT NULL DEFAULT '',
    held_call             TEXT    NOT NULL DEFAULT '',
    product               TEXT    NOT NULL,
    connection            TEXT    NOT NULL,
    account               TEXT    NOT NULL,
    target                TEXT    NOT NULL DEFAULT '',
    tool                  TEXT    NOT NULL,
    class                 TEXT    NOT NULL,
    step                  TEXT    NOT NULL,
    answered_by           TEXT    NOT NULL DEFAULT '',
    answerer              TEXT    NOT NULL DEFAULT '',
    via                   TEXT    NOT NULL DEFAULT '',
    operation_digest      TEXT    NOT NULL,
    before_digest         TEXT    NOT NULL DEFAULT '',
    changes               TEXT    NOT NULL DEFAULT '[]',
    currency              TEXT    NOT NULL DEFAULT '',
    delta_micros          INTEGER NOT NULL DEFAULT 0,
    daily_micros          INTEGER NOT NULL DEFAULT 0,
    daily_after_micros    INTEGER NOT NULL DEFAULT 0,
    per_change_cap_micros INTEGER NOT NULL DEFAULT 0,
    daily_cap_micros      INTEGER NOT NULL DEFAULT 0,
    failure               TEXT    NOT NULL DEFAULT '',
    at                    INTEGER NOT NULL,
    position              INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, growth_id)
);
CREATE INDEX access_growth_changes_idx ON access_growth (position) WHERE class != 'read';             -- the recent changes, newest first
CREATE INDEX access_growth_product_idx ON access_growth (product, position) WHERE class != 'read';    -- one product's recent changes
CREATE INDEX access_growth_target_idx ON access_growth (product, account, target, at);               -- a vendor's rate rule on one target
CREATE INDEX access_growth_expiry_idx ON access_growth (at, agent_handle);                           -- the retention
