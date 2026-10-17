/**
 * `mcp_servers_status`: what each MCP server is doing on each live node, read
 * off every node's presence heartbeat, beside what the configuration declares
 * for it — the launch and never a credential (`env` and `headers` are not
 * here).
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheToolsScreenReadsWhatTheServerStatusSends` in both directions, the
 * state union included.
 */

/**
 * A server's condition across the fleet, decided by the engine. `running`:
 * every instance launched is serving — it started, listed its tools and was up
 * at its node's last beat. `partial`: some are serving and some are not — a
 * node, one seat's credentials or one child being restarted rather than the
 * server. `failing`: none that was launched is serving. `not_started`: every
 * reporting node started nothing for it. `unreported`: no live node's latest
 * heartbeat carried the report.
 */
export type McpServerState = "running" | "partial" | "failing" | "not_started" | "unreported";

/** One live node, and whether its latest heartbeat carried its MCP servers. */
export interface McpStatusNode {
  id: string;
  /** False: the node's last heartbeat carried no status (its status hook
   *  overran the beat), so its cells are unknown, never zero. */
  reported: boolean;
}

/** What one node did with one server. */
export interface McpServerNode {
  node: string;
  reported: boolean;
  started: number;
  failed: number;
  /** Tools one serving instance serves. */
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
  /** The most tools one serving instance serves. */
  tools: number;
  /**
   * The entry's `tool_annotations` keys that no instance started on a
   * reporting node lists a tool for, sorted: each overrides nothing anywhere.
   * A key only some instances lack is not here — a per-seat child serves what
   * its seat's credentials reach. Empty where no instance started.
   */
  unlisted_annotations: string[];
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
