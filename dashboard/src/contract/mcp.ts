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

import type { ToolRow } from "./tools.ts";

/**
 * A server's condition across the fleet, decided by the engine. `running`:
 * every instance launched is serving — it started, listed its tools and was up
 * at its node's last beat. `partial`: some are serving and some are not — a
 * node, one seat's credentials or one child being restarted rather than the
 * server. `failing`: none that was launched is serving. `waiting`: nothing
 * was launched and nothing failed, and a seat's instance is held back for a
 * credential a provisioning pass mints for it and has not sealed yet — not a
 * failure; it starts once the credential is sealed. `not_started`: every
 * reporting node started nothing for it. `unreported`: no live node's latest
 * heartbeat carried the report.
 */
export type McpServerState =
  "running" | "partial" | "failing" | "waiting" | "not_started" | "unreported";

/** One live node, and whether its latest heartbeat carried its MCP servers. */
export interface McpStatusNode {
  id: string;
  /** False: the node's last heartbeat carried no status (its status hook
   *  did not answer within its share of the beat), so its cells are unknown,
   *  never zero. */
  reported: boolean;
}

/** What one node did with one server. */
export interface McpServerNode {
  node: string;
  reported: boolean;
  started: number;
  failed: number;
  /** Instances held back, unstarted, for a credential still being minted. */
  waiting: number;
  /** Tools one serving instance serves. */
  tools: number;
  /** One failed instance's reason, cut to 512 bytes, or "". */
  error: string;
  /** The seat that instance was launched for, or "" for a shared server. */
  error_seat: string;
  /** What one waiting instance waits for — the variables and the integration
   *  minting them — cut to 512 bytes, or "". */
  wait: string;
  /** The seat that waiting instance belongs to, or "". */
  wait_seat: string;
}

/** One server. */
export interface McpServerStatus {
  name: string;
  /** The curated catalogue entry's title where the engine recognises the
   *  server by its address or its command, else the server's name. */
  title: string;
  /** That entry's key, or "" for a server the catalogue does not know. */
  catalogue: string;
  /** The vetted vendor mark to draw, or "" for the generic server glyph. */
  vendor: string;
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
  /** Neither started nor failed: held back for a credential still being minted. */
  waiting: number;
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

/**
 * One shelf of the built-in catalogue: the engine's own tools as a seat is
 * offered them, filed and worded by the engine.
 */
interface BuiltinGroup {
  key: Exclude<ToolRow["group"], "">;
  label: string;
  /** One line about what the shelf's tools are for. */
  description: string;
  /** In the catalogue's order, each naming who may call it (`gate`). */
  tools: ToolRow[];
}

/** The whole answer. */
export interface McpServersStatusAnswer {
  /** In name order. */
  servers: McpServerStatus[];
  /** The live nodes in id order: the columns every server is drawn against. */
  nodes: McpStatusNode[];
  /**
   * The engine's built-ins, shelf by shelf in the catalogue's order. A shelf
   * the company offers no tool of (Worklet's on a company running Jira) is
   * left out.
   */
  builtin: BuiltinGroup[];
}
