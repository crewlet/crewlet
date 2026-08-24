-- webhook_deliveries — "have I already handled this inbound delivery?"
--
-- Answered from a per-process ring before this table existed, which is
-- right for one process and wrong for two: the same delivery retried to a
-- different node is a fresh delivery to THAT node, so the agent wakes
-- twice and answers twice — the duplicate reply the people the company
-- talks to actually see. GitHub and GitLab had no ring at all, so every
-- provider retry and every replay from the provider UI woke the seat
-- again.
--
-- The claim is ONE statement (INSERT … ON CONFLICT DO UPDATE … WHERE), so
-- there is no read-then-write window for a peer to slip through.
--
-- Each source keeps deriving its OWN key. What counts as "the same
-- delivery" is genuinely source-specific — a provider delivery id where
-- one exists, event coordinates where it does not — and a key column that
-- tried to be universal would only move that decision somewhere with less
-- context.
--
-- Polarity: FAIL OPEN. A store that cannot be reached must not stop
-- inbound work; a duplicate is recoverable noise, a dropped delivery is a
-- message nobody ever answers.
CREATE TABLE webhook_deliveries (
    -- The integration that sent it: 'github', 'gitlab', 'plane', …
    source       TEXT    NOT NULL,
    -- The provider's own identity for this delivery.
    delivery_key TEXT    NOT NULL,
    seen_at      INTEGER NOT NULL,
    PRIMARY KEY (source, delivery_key)
);

-- The retention sweep is a range delete over seen_at. Claiming already
-- enforces the TTL, so a deployment whose sweep never runs is wasteful
-- rather than wrong.
CREATE INDEX webhook_deliveries_seen_at_idx
    ON webhook_deliveries (seen_at);
