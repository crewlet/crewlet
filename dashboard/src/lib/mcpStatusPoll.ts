/**
 * How often `mcp_servers_status` is asked again, in ms, by every screen that
 * reads it: the MCP page and the Inbox's MCP alarms alike.
 *
 * A MODULE OF ITS OWN because the Inbox's half is in the entry every page
 * loads: imported from `lib/mcpServers.ts`, it pulled that module, which only
 * the MCP page reads, into the initial load.
 *
 * FIFTEEN SECONDS, the Nodes screen's cadence and for its reason: the answer
 * is the lease table, and a poll slower than the presence TTL would show a
 * server running on a node whose lease had already expired. It also bounds
 * how long a server just added reads "not started" after a node applied it,
 * and how long an alarm the engine raised (on its ten-second evaluation)
 * waits to reach the Inbox.
 */
export const MCP_STATUS_POLL_MS = 15_000;
