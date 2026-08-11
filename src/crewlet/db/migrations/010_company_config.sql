-- company_config — versioned Tier B (founder-owned) company configuration.
--
-- Single-tenant: each engine serves exactly one company.  At most one
-- row is marked active at any given time; zero rows means the engine
-- is in the unconfigured state and waiting for the first `PUT /config`
-- or `crewlet config import` to bootstrap.
--
-- Each row is an immutable snapshot of the full Tier B `CompanyConfig`
-- as a JSONB document; revisions form a chain via `parent_revision_id`.
-- Audit attribution (`created_by`, `source`, `summary`) and timestamps
-- let operators trace who changed what, when, and why.
--
-- Activation is atomic via the partial unique index on `is_active`
-- (Postgres allows exactly one TRUE value in a unique partial index).
-- The application enforces "at most one active" by wrapping
-- deactivate-then-activate in a single transaction.

CREATE TABLE company_config (
    revision_id        UUID         PRIMARY KEY,
    parent_revision_id UUID         REFERENCES company_config(revision_id),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_by         TEXT         NOT NULL,
    source             TEXT         NOT NULL,
    summary            TEXT         NOT NULL,
    payload            JSONB        NOT NULL,
    is_active          BOOLEAN      NOT NULL DEFAULT FALSE,
    activated_at       TIMESTAMPTZ
);

CREATE UNIQUE INDEX company_config_one_active
    ON company_config (is_active) WHERE is_active;

CREATE INDEX company_config_by_created_at
    ON company_config (created_at DESC);
