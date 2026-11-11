-- The access log records every verdict on a delegated request (ADR-0125): what
-- a delegate seat answered, what the engine decided on its own (a deadline,
-- delegation turned off) and what a person approved through a decision the
-- engine composed, beside the SSH certificates and commands (0151).
-- internal/accessdomain is the domain.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ACCESS_LOG stream by its applier, as every access table is: the audit
-- and a person's daily digest are answered by whichever data node is reached.
--
-- access_verdicts: one row per request and giver, created once and never
-- updated but for a rekey's reseal. `agent_handle` is the seat whose request it
-- is; `verdict_id` is derived from the request and the giver
-- (accessdomain.VerdictID), so a giver answering twice lands on one row and a
-- panel's two delegates on two; `by_handle` is empty for the engine.
-- `subject_sealed` (what was decided, in clear for a person) and
-- `words_sealed` (the giver's reason and findings) are sealed under the fleet
-- keyring by the writer, each bound to the row, and opened only for an
-- administrator's audit read; `content_key` is the key both are sealed under,
-- which the secret store's census counts. `overruled` is a JSON array of the
-- engine's closed words. The host, user and stdin columns are an ssh_operate's.
CREATE TABLE access_verdicts (
    agent_handle   TEXT    NOT NULL,
    verdict_id     TEXT    NOT NULL,
    request_id     TEXT    NOT NULL,
    request_kind   TEXT    NOT NULL,
    rule_number    INTEGER NOT NULL DEFAULT 0,
    turn_id        TEXT    NOT NULL DEFAULT '',
    by_handle      TEXT    NOT NULL DEFAULT '',
    by_kind        TEXT    NOT NULL,
    through        TEXT    NOT NULL,
    asked          TEXT    NOT NULL DEFAULT '',
    confidence     TEXT    NOT NULL DEFAULT '',
    verdict        TEXT    NOT NULL,
    final          INTEGER NOT NULL DEFAULT 0,
    overruled      TEXT    NOT NULL DEFAULT '[]',
    sampled        INTEGER NOT NULL DEFAULT 0,
    digest         TEXT    NOT NULL,
    host           TEXT    NOT NULL DEFAULT '',
    login_user     TEXT    NOT NULL DEFAULT '',
    stdin_bytes    INTEGER NOT NULL DEFAULT 0,
    stdin_sha256   TEXT    NOT NULL DEFAULT '',
    decision_id    TEXT    NOT NULL DEFAULT '',
    item           TEXT    NOT NULL DEFAULT '',
    subject_sealed TEXT    NOT NULL,
    words_sealed   TEXT    NOT NULL,
    content_key    TEXT    NOT NULL,
    at             INTEGER NOT NULL,
    position       INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, verdict_id)
);
CREATE INDEX access_verdicts_position_idx ON access_verdicts (position);                    -- the audit, newest first
CREATE INDEX access_verdicts_seat_idx ON access_verdicts (agent_handle, position);           -- one requesting seat's verdicts
CREATE INDEX access_verdicts_by_idx ON access_verdicts (by_handle, position);               -- one giver's verdicts, a delegate's digest
CREATE INDEX access_verdicts_request_idx ON access_verdicts (request_id, position);          -- one request's verdicts, a panel's two
CREATE INDEX access_verdicts_sampled_idx ON access_verdicts (position) WHERE sampled = 1;   -- the spot-checks
CREATE INDEX access_verdicts_expiry_idx ON access_verdicts (at, agent_handle);              -- the retention
CREATE INDEX access_verdicts_key_idx ON access_verdicts (content_key);                      -- the sealed-value census
