-- A signed write's id outlives its node's restart.
--
-- One row per request a node took on a signed channel whose replay writes
-- (internal/signedask): the channel's name, the request's id, and until when,
-- by this node's clock, the channel would still admit it. A guard reads its
-- channel's rows back at boot, so a request captured before a restart and
-- replayed after it is refused, not run again as the actor it names.
--
-- WHO HAS TO AGREE ON IT: this node alone. Each node takes a scattered
-- request once, so what one node took says nothing about another.
CREATE TABLE signed_request_taken (
    channel TEXT    NOT NULL,
    id      TEXT    NOT NULL,
    until   INTEGER NOT NULL,
    PRIMARY KEY (channel, id)
);
CREATE INDEX signed_request_taken_until ON signed_request_taken (channel, until);
