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
 * A server's condition across the fleet, decided by the engine. `disabled`:
 * the configuration turns it off (`enabled: false`), so nothing is started for
 * it and no seat is granted it, decided before anything a node reports.
 * `running`: every instance launched is serving: it started, listed its tools and was up
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
  "disabled" | "running" | "partial" | "failing" | "waiting" | "not_started" | "unreported";

/** One live node, and whether its latest heartbeat carried its MCP servers. */
export interface McpStatusNode {
  id: string;
  /** False: the node's last heartbeat carried no status (its status hook
   *  did not answer within its share of the beat), so its cells are unknown,
   *  never zero. */
  reported: boolean;
}

/**
 * Why talking to a server failed, in the classes a person tells apart by what
 * fixes them, decided by the engine where the facts are (the HTTP status a
 * server answered, what became of a local server's process, the dial).
 */
type McpFailureClass =
  | "blocked_address"
  | "dns"
  | "unreachable"
  | "tls"
  | "needs_sign_in"
  | "credential_rejected"
  | "missing_permission"
  | "no_endpoint"
  | "not_mcp"
  | "old_protocol"
  | "rate_limited"
  | "empty_tools"
  | "command_not_found"
  | "exited";

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
  /** Why the failed instance `error` names failed, or "" where no class fits. */
  error_class: McpFailureClass | "";
  /** When the longest-failed instance on the node stopped serving, or null. */
  down_since: string | null;
  /** How many times the node brought the server's instances back, summed. */
  restarts: number;
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
  /** The configuration runs it (`enabled`, default true); false is `disabled`.
   *  True on an unconfigured server, which a node is running. */
  enabled: boolean;
  /** What the configuration says it is for, the line each agent's prompt lists
   *  beside its name; "" when it says nothing. */
  description: string;
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
  /**
   * Tools an instance on a reporting node lists that are not offered for
   * their name, each once across the fleet, sorted by tool: every one any
   * node named, which is at most 8 a node.
   */
  refused_tools: McpRefusedTool[];
  /** How many tools are refused, as the node counting the most counts them;
   *  never fewer than `refused_tools` lists. */
  refused_count: number;
  /** Tools another server's tool of the same name is offered in place of,
   *  listed as `refused_tools` is. */
  shadowed_tools: McpShadowedTool[];
  /** How many tools are shadowed, counted as `refused_count` is. */
  shadowed_count: number;
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

/**
 * Why a tool is not offered under its name. `characters`: the name holds one
 * a model's tool name cannot (only letters, digits, `_` and `-`). `too_long`:
 * with the server's `tool_prefix`, it is longer than a harness's prefix
 * leaves room for. `builtin`: a built-in tool has the name, and built-ins are
 * filed first.
 */
type McpToolRefusal = "characters" | "too_long" | "builtin";

/** One tool not offered for its name. */
interface McpRefusedTool {
  /** The name it would be filed under: the server's `tool_prefix` and its own. */
  tool: string;
  reason: McpToolRefusal;
  /** The engine's sentence saying why and what would offer it, shown as written. */
  message: string;
}

/** One tool another server's tool of the same name is offered in place of. */
interface McpShadowedTool {
  tool: string;
  /** The configured name of the server whose tool is offered instead. */
  by: string;
  /** The engine's sentence, shown as written. */
  message: string;
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
