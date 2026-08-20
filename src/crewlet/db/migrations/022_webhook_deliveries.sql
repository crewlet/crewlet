-- webhook_deliveries — cross-process inbound delivery dedupe.
--
-- Every inbound surface needs to answer "have I already handled this
-- delivery", and every one of them answered it from a per-process dict
-- with a TTL. That is correct for exactly one process. Behind a load
-- balancer the same webhook retried to a different node is a fresh
-- delivery to that node, so the agent gets woken twice and can reply
-- twice — the duplicate-side-effect failure that is most visible to the
-- people the company talks to.
--
-- The claim is `INSERT … ON CONFLICT DO NOTHING RETURNING`: the first
-- caller inserts and gets a row back, everyone else gets nothing. No
-- read-then-write window.
--
-- `source` namespaces the key so two integrations cannot collide, and
-- `delivery_key` is whatever that source can offer as a stable identity:
-- a provider delivery id where one exists (GitHub's X-GitHub-Delivery,
-- GitLab's X-Gitlab-Event-UUID), or the event coordinates where it does
-- not (Slack's channel+ts, Plane's event+action+id+updated_at+activity —
-- CE sends one webhook per changed field with an identical data
-- snapshot, so the activity discriminator is what keeps a bulk edit's
-- directed route from being dropped as a duplicate of a state change).
--
-- Rows are swept on a TTL rather than kept: this is a short-horizon
-- "did I just see this", not an audit log. The event store is the audit
-- log.

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    source       TEXT        NOT NULL,
    delivery_key TEXT        NOT NULL,
    seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, delivery_key)
);

-- The sweep is a range delete over seen_at; without this it degrades to
-- a full scan on a table that sees every inbound delivery.
CREATE INDEX IF NOT EXISTS webhook_deliveries_seen_at_idx
    ON webhook_deliveries (seen_at);
