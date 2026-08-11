-- secret_values — encrypted secret store, keyed by environment-variable name.
--
-- The companion to `company_config`, not a duplicate of it.  A config
-- revision is an immutable full-document snapshot; this table is mutable
-- and keyed by the `${VAR}` name a config value references.  That split
-- is deliberate:
--
--   * Rotation is an UPDATE of one row.  Writing the literal into the
--     config instead would archive the OLD secret forever, because every
--     revision is an immutable copy and revisions are never scrubbed.
--   * One name, many pointers.  `role.integrations.slack.bot_token` and
--     `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN` reference the SAME var
--     today — one credential with two readers.  Keying by var name keeps
--     that one row; inlining literals would duplicate the secret across
--     pointers that must then update atomically or the identity
--     split-brains.
--
-- Every `value` is an `enc:v1:<key_id>:<base64>` envelope sealed with the
-- Tier A keyring, with the row's `name` bound in as AEAD associated data
-- — so a ciphertext moved to a different row fails to decrypt rather than
-- silently impersonating another secret.  `key_id` is denormalised out of
-- the envelope so a rotation sweep can find stale rows without decrypting
-- them.
--
-- Unlike `company_config`, this table has NO plaintext mode.  A keyring is
-- required to write or read a row.  There is no legacy plaintext corpus to
-- stay compatible with here, and a dedicated secret store that can hold
-- unencrypted secrets is a footgun with no upside.
--
-- Single-tenant, like every other table: one engine serves one company, so
-- there is no tenant column and `name` alone is the primary key.  Allowed
-- `name` values are enforced at the application layer (the shared
-- `${VAR}` grammar in `crewlet.env_refs`), matching this schema's
-- convention of no DB-level CHECK constraints on value-shaped columns.

CREATE TABLE secret_values (
    name       TEXT         PRIMARY KEY,
    value      TEXT         NOT NULL,
    key_id     TEXT         NOT NULL,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by TEXT         NOT NULL,
    source     TEXT         NOT NULL
);

CREATE INDEX secret_values_by_updated_at
    ON secret_values (updated_at DESC);
