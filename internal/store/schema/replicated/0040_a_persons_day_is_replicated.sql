-- usage_person_tokens — one row per (day, node, person, phase, worker, model,
-- provider key): what the seats' auxiliary model spent for a PERSON that day on
-- that node, written only by the usage domain's applier from a `person` record
-- (internal/usage, record version 2).
--
-- # Why a person has a table of their own
--
-- Every other row in this domain is a SEAT's, keyed on the agent id every node
-- derives alike — and a person has none: identity is derived for agent seats
-- alone, and a human seat has no budget and takes no turns. What the auxiliary
-- model spends for one — a question answered on the operator surface, a pass
-- on a unit a person leads — named no agent, so it was in no seat's day and in
-- no named spend window, while the company's counter was charged for it. Keyed
-- on the person's seat HANDLE instead, in rows that are a seat's tokens and
-- nothing else, since a person's day is its spend alone.
--
-- # The guard is the newest version among an object's rows
--
-- For `usage_schedule_runs`' reason (0023): no head row, so `version` is on
-- every row and the apply replaces the object's rows only for a record newer
-- than all of them. `role` is the person's seat's role as the record named it,
-- on every row of one record alike.
--
-- Divergent, like every row table here: a node that joined last month holds
-- fewer days, so these travel inside a snapshot and are outside the identity
-- claim. The horizon is the applier's, by the record's day, as 0023 states.
CREATE TABLE usage_person_tokens (
    day          TEXT    NOT NULL,
    node         TEXT    NOT NULL,
    person       TEXT    NOT NULL,
    role         TEXT    NOT NULL DEFAULT '',
    phase        TEXT    NOT NULL DEFAULT '',
    worker       TEXT    NOT NULL DEFAULT '',
    model        TEXT    NOT NULL DEFAULT '',
    provider_key TEXT    NOT NULL DEFAULT '',
    input        INTEGER NOT NULL DEFAULT 0,
    output       INTEGER NOT NULL DEFAULT 0,
    cache_read   INTEGER NOT NULL DEFAULT 0,
    cache_write  INTEGER NOT NULL DEFAULT 0,
    total        INTEGER NOT NULL DEFAULT 0,
    calls        INTEGER NOT NULL DEFAULT 0,
    version      INTEGER NOT NULL,
    PRIMARY KEY (day, node, person, phase, worker, model, provider_key)
);

-- Every spend window is a day range over every node and every person, as
-- usage_tokens_day_idx is for the seats.
CREATE INDEX usage_person_tokens_day_idx ON usage_person_tokens (day);
