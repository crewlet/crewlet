-- An MCP instance's outage outlives its node's restart.
--
-- One row per tool server instance this node holds that is not serving: its
-- server, its seat ('' for a shared server's) and when it stopped serving.
--
-- WHO HAS TO AGREE ON IT: this node alone, which runs and dates its own instances.
CREATE TABLE mcp_instance_down (
    server TEXT    NOT NULL,
    seat   TEXT    NOT NULL DEFAULT '',
    since  INTEGER NOT NULL,
    PRIMARY KEY (server, seat)
);
