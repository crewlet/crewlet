-- The founder-owned configuration and the secrets it points at.

-- company_config — versioned Tier B (founder-owned) company config.
--
-- Single-tenant, like everything here: one engine serves one company. At
-- most one row is active; zero rows is the unconfigured state, waiting on
-- the first config import. Each row is an immutable snapshot of the whole
-- Tier B document, and revisions chain through parent_revision_id, so the
-- audit fields (created_by / source / summary) plus the chain answer who
-- changed what, when and why.
CREATE TABLE company_config (
    revision_id        TEXT    NOT NULL PRIMARY KEY,
    parent_revision_id TEXT    REFERENCES company_config(revision_id),
    created_at         INTEGER NOT NULL,
    created_by         TEXT    NOT NULL,
    source             TEXT    NOT NULL,
    summary            TEXT    NOT NULL,
    -- The whole config document as JSON. When a keyring is configured
    -- this is the encrypted envelope {"__encrypted__": "enc:v1:…"} rather
    -- than the plaintext structure — the payload is opaque to SQL either
    -- way, which is why it is one column and not a schema.
    payload            TEXT    NOT NULL,
    is_active          INTEGER NOT NULL DEFAULT 0,
    activated_at       INTEGER
);

-- At most one active revision, enforced by the database rather than by
-- the application remembering to. A partial UNIQUE index is legal on both
-- drivers (measured); what is not legal is aiming a bare ON CONFLICT at
-- one, and nothing does — activation is a deactivate-then-activate pair
-- inside a single transaction.
CREATE UNIQUE INDEX company_config_one_active_idx
    ON company_config (is_active) WHERE is_active <> 0;

CREATE INDEX company_config_created_at_idx
    ON company_config (created_at DESC);


-- secret_values — the encrypted secret store, keyed by the ${VAR} name a
-- config value references.
--
-- The companion to company_config, not a duplicate of it, and the split
-- is deliberate:
--
--   * Rotation is an UPDATE of one row. Writing the literal into the
--     config instead would archive the OLD secret forever, because every
--     revision is an immutable copy and revisions are never scrubbed.
--   * One name, many pointers. A Slack bot token referenced from both an
--     integration block and a per-role MCP env is ONE credential with two
--     readers; keying by var name keeps it one row, where inlining
--     literals would duplicate it across pointers that must then update
--     atomically or split-brain.
CREATE TABLE secret_values (
    name       TEXT    NOT NULL PRIMARY KEY,
    -- An enc:v1:<key_id>:<base64> envelope sealed with the Tier A
    -- keyring, with this row's `name` bound in as AEAD associated data —
    -- so a ciphertext moved to another row fails to decrypt instead of
    -- silently impersonating a different secret.
    value      TEXT    NOT NULL,
    -- Denormalised out of the envelope so a rotation sweep can find stale
    -- rows without decrypting any of them.
    key_id     TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT    NOT NULL,
    source     TEXT    NOT NULL
);

-- Unlike company_config this table has NO plaintext mode: a keyring is
-- required to read or write a row. There is no legacy corpus to stay
-- compatible with, and a dedicated secret store that can hold unencrypted
-- secrets is a footgun with no upside. Reads FAIL CLOSED — an unreadable
-- secret must raise, because "" becomes an empty
-- Bearer token hours later and somewhere else entirely.

CREATE INDEX secret_values_updated_at_idx
    ON secret_values (updated_at DESC);
