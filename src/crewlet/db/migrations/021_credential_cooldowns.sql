-- credential_cooldowns — fleet-wide LLM credential cooldowns.
--
-- When a provider returns 429, the pool cools that key so the next call
-- rotates to another. Held in memory that only works for the process
-- that saw the 429: every peer keeps hammering the rate-limited key for
-- the full hour, so the org earns N times the 429s and the rotation
-- feature is defeated in exact proportion to replica count.
--
-- Worse than wasteful. When every key in a pool is cooled the provider
-- raises AllCredentialsExhausted, which FallbackLLMProvider catches to
-- fall through to the next provider in the chain — so one replica can be
-- answering a seat on Anthropic while another answers the same seat on
-- OpenAI, with no signal that it happened.
--
-- `credential_hint` is the stable sha256 prefix the pool already uses to
-- name a key in logs and events — never the key itself, so this table
-- holds no secret.
--
-- `cooled_until` is WALL CLOCK, unlike the in-process bookkeeping it
-- mirrors. Monotonic readings are per-boot and meaningless in another
-- process; a timestamp that has to travel cannot be one.

CREATE TABLE IF NOT EXISTS credential_cooldowns (
    provider_key    TEXT        NOT NULL,
    credential_hint TEXT        NOT NULL,
    cooled_until    TIMESTAMPTZ NOT NULL,
    reason          TEXT        NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_key, credential_hint)
);

CREATE INDEX IF NOT EXISTS credential_cooldowns_until_idx
    ON credential_cooldowns (cooled_until);
