/**
 * `mcp_servers_status`: what each MCP server did on each live node, read off
 * every node's presence heartbeat, beside what the configuration declares for
 * it — the launch and never a credential (`env` and `headers` are not here).
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheToolsScreenReadsWhatTheServerStatusSends` in both directions, the
 * state union included.
 */

/**
 * A server's condition across the fleet, decided by the engine. `running`:
 * every instance launched started. `partial`: some started and some did not —
 * a node or one seat's credentials rather than the server. `failing`: none
 * that was launched started. `not_started`: every reporting node started
 * nothing for it. `unreported`: no live node publishes the report.
 */
export type McpServerState = "running" | "partial" | "failing" | "not_started" | "unreported";

/** One live node, and whether it reports its MCP starts at all. */
export interface McpStatusNode {
  id: string;
  /** False is an older build: its cells are unknown, never zero. */
  reported: boolean;
}

/** What one node did with one server. */
export interface McpServerNode {
  node: string;
  reported: boolean;
  started: number;
  failed: number;
  /** Tools one started instance serves. */
  tools: number;
  /** One failed instance's reason, cut to 240 bytes, or "". */
  error: string;
  /** The seat that instance was launched for, or "" for a shared server. */
  error_seat: string;
}

/** One server. */
export interface McpServerStatus {
  name: string;
  /** False for a server a node reports and this configuration does not carry. */
  configured: boolean;
  /** One instance for the company, rather than one per seat granted it. */
  shared: boolean;
  /** `stdio` or `http`; "" on an unconfigured server. */
  transport: string;
  command: string;
  args: string[];
  url: string;
  state: McpServerState;
  /** Summed over the nodes that report. */
  started: number;
  failed: number;
  /** The most tools one started instance serves. */
  tools: number;
  /** One cell per live node, in `nodes` order. */
  nodes: McpServerNode[];
}

/** The whole answer. */
export interface McpServersStatusAnswer {
  /** In name order. */
  servers: McpServerStatus[];
  /** The live nodes in id order: the columns every server is drawn against. */
  nodes: McpStatusNode[];
}
