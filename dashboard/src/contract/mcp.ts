/**
 * Settings › MCP & tools, as the engine answers it: `mcp_servers_status`
 * (what each MCP server is doing on each live node, read off every node's
 * presence heartbeat, beside what the configuration declares for it), one
 * server whole (`GET /mcp-servers/{name}`), its removal
 * (`DELETE /config/mcp-servers/{name}`) and its restart
 * (`POST /mcp-servers/{name}/retry`).
 *
 * TWO READINGS OF EVERY READ. Whoever `config.read` admits reads the
 * administrator's (`reading: "admin"`); everybody else reads a member's
 * (`reading: "member"`), which names no launch line, header, variable,
 * failure sentence or call. Each member shape is declared on its own, and an
 * administrator's shape extends it with what only an administrator reads, so
 * a screen typed by the member's shape draws both.
 *
 * EXACTLY WHAT THE ENGINE SENDS, held in both directions by
 * `TestTheToolsScreenReadsWhatTheServerStatusSends` (the list, the state
 * union included) and `TestTheServerPageReadsWhatTheDetailSends` (a server
 * whole) in `internal/api/queries`, `TestTheRemoveDialogReadsWhatTheRemovalSends`
 * in `internal/api/configapi` and `TestTheRestartReadsWhatTheRouteSends` in
 * `internal/api/mcpapi`.
 */

import type { Coverage } from "./coverage.ts";
import type { ToolAnnotations, ToolRow } from "./tools.ts";

/**
 * A server's condition across the fleet, decided by the engine. `disabled`:
 * the configuration turns it off (`enabled: false`), so nothing is started for
 * it and no seat is granted it, decided before anything a node reports.
 * `needs_sign_in`: it is signed in to with OAuth and nobody holds a grant for
 * it, or its vendor refused to renew the grant or let it run out, so nothing
 * is started until somebody signs in, decided next.
 * `running`: every instance launched is serving: it started, listed its tools
 * and was up at its node's last beat. `partial`: some are serving and some are
 * not (a node, one seat's credentials or one child being restarted rather
 * than the server). `failing`: none that was launched is serving. `waiting`:
 * nothing was launched and nothing failed, and a seat's instance is held back
 * for a credential a provisioning pass mints for it and has not sealed yet,
 * which is not a failure: it starts once the credential is sealed.
 * `not_started`: every reporting node started nothing for it. `unreported`:
 * no live node's latest heartbeat carried the report.
 */
export type McpServerState =
  | "disabled"
  | "needs_sign_in"
  | "running"
  | "partial"
  | "failing"
  | "waiting"
  | "not_started"
  | "unreported";

/** One live node, and whether its latest heartbeat carried its MCP servers. */
interface McpStatusNode {
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
export type McpFailureClass =
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

/** What one node did with one server, as a member reads it. */
export interface McpMemberServerNode {
  node: string;
  reported: boolean;
  started: number;
  failed: number;
  /** Instances held back, unstarted, for a credential still being minted. */
  waiting: number;
  /** Tools one serving instance serves. */
  tools: number;
  /** Why the failed instance `error` names failed, or "" where no class fits. */
  error_class: McpFailureClass | "";
  /** When the longest-failed instance on the node stopped serving, or null. */
  down_since: string | null;
  /** How many times the node brought the server's instances back, summed. */
  restarts: number;
}

/** What one node did with one server, as an administrator reads it. */
export interface McpServerNode extends McpMemberServerNode {
  /** One failed instance's reason, cut to 512 bytes, or "". */
  error: string;
  /** The seat that instance was launched for, or "" for a shared server. */
  error_seat: string;
  /** What one waiting instance waits for (the variables and the integration
   *  minting them), cut to 512 bytes, or "". */
  wait: string;
  /** The seat that waiting instance belongs to, or "". */
  wait_seat: string;
}

/** One server, as a member reads it: what it is and how it is doing. */
export interface McpMemberServerStatus {
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
  state: McpServerState;
  /** Summed over the nodes that report. */
  started: number;
  failed: number;
  /** Neither started nor failed: held back for a credential still being minted. */
  waiting: number;
  /** The most tools one serving instance serves. */
  tools: number;
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
  nodes: McpMemberServerNode[];
}

/**
 * Where a server's OAuth sign-in is. `connected`: the engine renews it as the
 * server is used. `needs_sign_in`: nobody has signed in. `expired`: past the
 * end its vendor stated. `refresh_failed`: its vendor refused to renew it.
 */
type McpOAuthStatus = "connected" | "needs_sign_in" | "expired" | "refresh_failed";

/** A renewal a vendor refused for good, in its own words. */
interface McpOAuthFailure {
  said: string;
  at: string;
}

/**
 * A server's OAuth sign-in as an administrator reads it: never a token, never
 * the client's secret, and never on a member's reading.
 */
interface McpOAuthView {
  /**
   * `authorization_code`: a person signs the company in. `client_credentials`:
   * the engine signs in as the company's service identity, nobody signs in,
   * and `status` is always `connected` (a credential the vendor refuses is the
   * server's failure).
   */
  grant: string;
  status: McpOAuthStatus;
  /** How the engine registers for a person's sign-in: `cimd`, `dcr` or `own`; "" for a service identity. */
  client: string;
  /** The client id the grant was made as, else the one the configuration names; "". */
  client_id: string;
  /** The authorization server the grant is from; "" with none. */
  issuer: string;
  /** The server address the grant was issued for, the one its tokens go to; "" with none. */
  resource: string;
  /** What the grant was granted, else what the configuration asks for. */
  scopes: string[];
  connected_by: string | null;
  connected_at: string | null;
  /** When the grant runs out where its vendor said, else null. */
  expires_at: string | null;
  failure: McpOAuthFailure | null;
}

/** One server, as an administrator reads it: its launch and every node's sentence. */
export interface McpServerStatus extends McpMemberServerStatus {
  command: string;
  args: string[];
  /** The address, any credential it carries (a query parameter, a path
   *  segment, the fragment) withheld by the engine. */
  url: string;
  /** Its OAuth sign-in, or null for a server not signed in to with OAuth. */
  oauth: McpOAuthView | null;
  /**
   * The entry's `tool_annotations` keys that no instance started on a
   * reporting node lists a tool for, sorted: each overrides nothing anywhere.
   * A key only some instances lack is not here, since a per-seat child serves
   * what its seat's credentials reach. Empty where no instance started.
   */
  unlisted_annotations: string[];
  nodes: McpServerNode[];
}

/**
 * One shelf of the built-in catalogue: the engine's own tools as a seat is
 * offered them, filed and worded by the engine.
 */
export interface BuiltinGroup {
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

/**
 * The MCP alarms (statelog's per-server kinds). `mcp_server_failing`: no
 * instance serves, and one has been down past the restart schedule's ceiling
 * plus a heartbeat. `mcp_server_partial`: some serve, and one that does not has
 * been down that long. `mcp_server_waiting_long`: an agent's instance has been
 * held back past a provisioning pass for a credential that pass mints.
 * `mcp_tools_refused`: a tool cannot be offered under its name.
 * `mcp_oauth_needs_sign_in`: nobody is signed in to a server signed in to
 * with OAuth, or its vendor refused to renew the sign-in (in its words).
 * `mcp_oauth_expiring`: a sign-in runs out within seven days.
 * `mcp_approval_waiting`: an agent's run has waited past the grace for a
 * person to approve a call to one of the server's tools.
 * `mcp_tools_changed`: a tool the server changed, or added after its first
 * listing, asks a person before each call until somebody accepts the change.
 */
export type McpAlarmKind =
  | "mcp_server_failing"
  | "mcp_server_partial"
  | "mcp_server_waiting_long"
  | "mcp_tools_refused"
  | "mcp_oauth_needs_sign_in"
  | "mcp_oauth_expiring"
  | "mcp_approval_waiting"
  | "mcp_tools_changed";

/**
 * The one gesture that fixes a server's alarm. `restart`: restart its
 * instances now (`POST /mcp-servers/{name}/retry`), only the named agent's
 * where `seat` names one. `integration`: the integration that mints the named
 * agent's credential, which the server's binding names. `edit`: the server's
 * configuration, its `tool_prefix`. `sign_in`: sign the company in to the
 * server again (`POST /mcp-servers/{name}/oauth`). `approve`: the calls the
 * named agent's run holds for approval on the server, each in `approvals`.
 * `review`: the server's changed tools, each in its detail's `changes`.
 */
export type McpFixKind = "restart" | "integration" | "edit" | "sign_in" | "approve" | "review";

/** The gesture that fixes one server's alarm, and what it acts on. */
interface McpFix {
  kind: McpFixKind;
  server: string;
  /** The one agent's instance it acts on, by handle; "" acts on every instance. */
  seat: string;
}

/** One server an MCP alarm stands on. */
export interface McpAlarmEntry {
  /** The server's name. */
  key: string;
  /** The engine's measurement for this server, shown as written. */
  detail: string;
  /** When the condition began for this server, or null where it carries no
   *  instant (a refused name). */
  since: string | null;
  /** The agent whose instance the measurement names, by handle, or "". */
  seat: string;
  fix: McpFix;
}

/**
 * One MCP alarm of the answering node's latest evaluation: the same one the
 * health count and the log lines were told.
 */
export interface McpAlarm {
  kind: McpAlarmKind;
  /** The longest-standing server's measurement. */
  detail: string;
  /** What to do about the kind, in the engine's words. */
  remedy: string;
  /** The servers it stands on, in name order. */
  entries: McpAlarmEntry[];
}

/** `mcp_servers_status` as an administrator reads it. */
export interface McpServersStatusAnswer {
  /** Which of the answer's two readings this is. */
  reading: "admin";
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
  /**
   * The MCP alarms standing on the answering node's latest evaluation, each
   * server with the gesture that fixes it. Empty where none stands, and on a
   * node that runs no state log, which evaluates no alarm.
   */
  alarms: McpAlarm[];
  /** Where a vendor sends a person back after a sign-in, for a vendor that
   *  allowlists it; "" where the engine has no public address. */
  callback_url: string;
  /** The engine's client metadata document, the client id a `cimd` sign-in
   *  is made as; "" where the engine has no public address. */
  client_metadata_url: string;
  /**
   * The calls agents' runs hold for a person's approval, the longest waiting
   * first: each while its run waits, and once answered until the run carries
   * on. The administrator's reading alone.
   */
  approvals: McpApproval[];
  /**
   * Every agent seat in the chart's order, with the servers its grants reach
   * and how many MCP tools they hand it. Null where the answering node cannot
   * read the fleet's listings yet, which is not an agent holding no tools.
   */
  agents: McpAgentTools[] | null;
  /** How many MCP tools an agent may hold before the engine flags it. */
  tool_budget: number;
}

/** Where a held call's answer stands. */
type McpApprovalStatus = "waiting" | "answered";

/** One call to a tool at Needs approval, held for a person's answer (`POST /mcp-approvals/{id}`). */
interface McpApproval {
  /** What the approval is answered by. */
  id: string;
  /** When the call was held. */
  at: string;
  server: string;
  /** The tool as the run called it, its prefix applied. */
  tool: string;
  /** The server's own name for it, which Always allow sets the level of. */
  raw: string;
  digest: string;
  /** The agent whose run made the call, by handle. */
  seat: string;
  turn_id: string;
  launch_id: string;
  /** The key of the work item the run was on, or null. */
  item: string | null;
  /** What the call would send; null where its seal does not open on this node. */
  args: Record<string, unknown> | null;
  status: McpApprovalStatus;
  /** `once`, `always` or `deny` once answered; "" while waiting or where the answer's seal does not open. */
  answer: string;
  answered_by: string;
  answered_at: string | null;
}

/** One agent seat's MCP tools, counted against `tool_budget`. */
interface McpAgentTools {
  /** The seat's handle. */
  seat: string;
  name: string;
  /** The unit the seat sits in, by name and by the key a grant names it by;
   *  both null for a seat at the root of the chart. */
  unit: string | null;
  unit_key: string | null;
  /** Every server whose grant reaches the seat, on or off, in name order. */
  servers: McpAgentServer[];
  /** Every tool its servers offer it, a name two servers offer counted once. */
  mcp_tools: number;
  /** The engine's own tools every agent holds, which the budget does not count. */
  builtin_tools: number;
  /** `mcp_tools` is past `tool_budget`. */
  over_budget: boolean;
}

/** One server's part in an agent's tools. */
interface McpAgentServer {
  server: string;
  reason: McpReachReason;
  /** The tools it offers the agent: every one it lists at a level other than
   *  blocked, a whole listing for a per-agent server, 0 for one turned off. */
  tools: number;
  /** The server's own state, as its row says. */
  state: McpServerState;
}

/**
 * `mcp_servers_status` as a member reads it: every server's condition and the
 * built-ins, and no alarm, since an alarm's measurement quotes what a server
 * said.
 */
export interface McpMemberServersStatusAnswer {
  reading: "member";
  servers: McpMemberServerStatus[];
  nodes: McpStatusNode[];
  builtin: BuiltinGroup[];
}

/** How much a call to a tool can change, the engine's one reading of its hints. */
export type McpRisk = "read_only" | "additive" | "destructive";

/** One instance's condition on its node. */
export type McpInstanceState = "running" | "failed" | "waiting";

/**
 * How the engine proves who it is to a server. `none`: it sends no
 * credential. `headers`: values the configuration sets, as HTTP headers to a
 * remote server and as environment variables to a local one. `integration`:
 * each agent's own credential from one of the company's integrations.
 * `oauth`: an OAuth access token, issued under a person's sign-in or to the
 * company's service identity (`oauth.grant` on the administrator's reading).
 */
export type McpAuth = "none" | "headers" | "integration" | "oauth";

/**
 * Why the engine expects a placement to fail. `private_address_in_cloud_box`:
 * a remote server on a private network given to a box in the `e2b` cell,
 * which runs outside that network. `command_in_box`: a local server given to
 * a box that runs its own image, which must provide the command.
 */
type McpPlacementWarningKind = "private_address_in_cloud_box" | "command_in_box";

/**
 * How a call to a server's tool ended. `answered`: it ran and the server
 * answered. `failed`: the server answered with an error, or the call broke
 * on the way. `unavailable`: the server was not there to ask. `refused`: the
 * engine refused it before any tool ran.
 */
export type McpCallOutcome = "answered" | "failed" | "unavailable" | "refused";

/** One live node's listing of the server. */
interface McpServerListing {
  node: string;
  /** When the node last changed its listing. */
  at: string;
  /** How many instances on the node the listing is the union of. */
  instances: number;
  tools: number;
}

/** One tool the server lists, the same in both readings. */
export interface McpToolView {
  /** The name it is offered under, the server's `tool_prefix` applied. */
  name: string;
  /** The server's own name for it. */
  raw: string;
  title: string;
  /** The server's words, cut by the engine. */
  description: string;
  /** Its hints with the company's overrides applied. */
  annotations: ToolAnnotations;
  /** How much a call can change, decided by the engine from the hints. */
  risk: McpRisk;
  /** Whether a hint the tool carries decided `risk`; false reads as destructive by default. */
  risk_declared: boolean;
  /** Whether a call may reach past the company (its open-world hint is not `no`). */
  external: boolean;
  /** Names the definition shown, the one most instances list. */
  digest: string;
  /** How many different definitions the fleet lists under the name. */
  definitions: number;
  instances: number;
  nodes: string[];
  /** Null where no schema is stored. */
  input_schema: Record<string, unknown> | null;
  output_schema: Record<string, unknown> | null;
  /** The level it runs at, decided by the server's own name for it; a blocked tool is offered to no agent. */
  level: McpToolLevel;
  /** Whether the server's `tools.levels` names it, rather than leaving it at the baseline. */
  custom: boolean;
  /** Its definition changed since its level allowed it, so it asks first until the change is accepted; `level` reads approve. */
  held: boolean;
  /** The server no longer lists it; kept, named by its accepted definition, until somebody accepts that it went. */
  removed: boolean;
}

/**
 * The level a tool of an MCP server runs at: `allow` offers it, `approve`
 * offers it and holds each call for a person's answer, `blocked` never offers it.
 */
type McpToolLevel = "allow" | "approve" | "blocked";

/** What the server said of itself in its handshake. */
interface McpServerInfo {
  name: string;
  title?: string;
  version?: string;
  protocol?: string;
}

/** One failure of an instance, as a member reads it: its class, never its sentence. */
interface McpMemberFailureView {
  /** "" where no class fits. */
  class: McpFailureClass | "";
  at: string;
  node: string;
  /** The agent the instance runs for, by handle; "" for a shared server's. */
  seat: string;
}

/** One failure of an instance, with what the server or its process said. */
interface McpFailureView extends McpMemberFailureView {
  /** Redacted and bounded by the engine; the server's words, shown as written. */
  said: string;
}

/** One instance of the server, as a member reads it. */
interface McpMemberInstanceView {
  /** The agent it runs for, by handle; "" for a shared server's one instance. */
  seat: string;
  node: string;
  state: McpInstanceState;
  /** When it entered its state, or null where the node does not know. */
  since: string | null;
  restarts: number;
  /** Tools it serves while it runs. */
  tools: number;
  /** Why it is down now, or null. */
  failure: McpMemberFailureView | null;
  /** Its latest failure, kept after it came back, or null. */
  last_failure: McpMemberFailureView | null;
  /** When it last answered, or null. */
  last_answer: string | null;
}

/** One instance of the server, as an administrator reads it. */
interface McpInstanceView extends McpMemberInstanceView {
  failure: McpFailureView | null;
  last_failure: McpFailureView | null;
  /** What a waiting instance waits for, or "". */
  wait: string;
}

/** One node asked for its instances, as a member reads it. */
interface McpMemberInstanceNodeView {
  node: string;
  answered: boolean;
  count: number;
}

/** One node asked for its instances, with why it did not answer. */
interface McpInstanceNodeView extends McpMemberInstanceNodeView {
  reason: string;
}

/** The startup and per-call bounds in force, in seconds, and whether each is the default. */
interface McpTimeouts {
  startup_seconds: number;
  request_seconds: number;
  startup_default: boolean;
  request_default: boolean;
}

/** The integration a per-agent server reads each agent's credential from. */
interface McpMemberBinding {
  /** The integration (`atlassian`, `github`, ...). */
  vendor: string;
  /** `declared` by the server's `seat_credentials`, or `recognised` from its address or command. */
  by: string;
}

/** One credential a per-agent server is started with, and where it goes. */
interface McpBoundKey {
  field: string;
  /** The header or variable it reaches the server under. */
  key: string;
}

interface McpBinding extends McpMemberBinding {
  keys: McpBoundKey[];
}

/** One seat whose executor box is given the server, and the cell the box runs in. */
interface McpBoxSeat {
  seat: string;
  cell: string;
}

/** One placement the engine expects to fail. */
interface McpPlacementWarning {
  kind: McpPlacementWarningKind;
  seat: string;
  cell: string;
  message: string;
}

/** What the engine decides about a server from its configuration, as a member reads it. */
interface McpMemberResolved {
  auth: McpAuth;
  /** `engine_host` for a local server, "" for a remote one. */
  runs_on: string;
  timeouts: McpTimeouts;
  binding: McpMemberBinding | null;
}

/** What the engine decides about a server, with the boxes and warnings an administrator reads. */
interface McpResolved extends McpMemberResolved {
  binding: McpBinding | null;
  box_seats: McpBoxSeat[];
  warnings: McpPlacementWarning[];
}

/** One tool's calls over the answer's days, as a member reads them. */
export interface McpMemberToolUsage {
  /** The name the calls used, prefix included: a tool's `name`. */
  tool: string;
  calls: number;
  failed: number;
  unavailable: number;
  refused: number;
  /** How long the calls that ran took between them. */
  total_ms: number;
  /** The newest call that ran, or null when none did. */
  last_at: string | null;
}

/** One tool's calls, with the seats that made them. */
interface McpToolUsage extends McpMemberToolUsage {
  seats: string[];
}

/** A server's calls per tool over a range of company days, as a member reads them. */
interface McpMemberServerUsage {
  /** The range's first and last company day, both inclusive. */
  since: string;
  until: string;
  days: number;
  tools: McpMemberToolUsage[];
  /** How many entries a per-seat-day cap dropped across the range; 0 says every count is whole. */
  elided: number;
}

interface McpServerUsage extends McpMemberServerUsage {
  tools: McpToolUsage[];
}

/** One call to one of the server's tools, never its arguments or its result. */
export interface McpCallRow {
  at: string;
  /** The seat that made it, by handle, and the run it was made in. */
  seat: string;
  turn_id: string;
  tool: string;
  outcome: McpCallOutcome;
  /** The engine's class of a failure it classified, or "". */
  refusal: string;
  /** Null for a call nothing timed. */
  duration_ms: number | null;
}

/** A server's newest calls, and which data nodes they were read from. */
interface McpServerCalls {
  calls: McpCallRow[];
  coverage: Coverage;
}

/** Who holds an `mcp_env` block naming the server. */
type McpHolderKind = "seat" | "unit";

/** One `mcp_env` block naming the server: the agent's or the team's own values for it. */
interface McpEnvDependant {
  kind: McpHolderKind;
  /** The seat's handle or the unit's key. */
  key: string;
  /** The seat's or the unit's name. */
  name: string;
  /** Where in the company document. */
  segments: (string | number)[];
}

/** One executor box given the server (`sandbox.mcp.servers`). */
interface McpSandboxDependant {
  seat: string;
  name: string;
  segments: (string | number)[];
}

/**
 * What removing the server takes with it: the fields that name it alone,
 * cleared in the same revision, and the stored values nothing reads once it
 * is gone, which may be deleted with it.
 */
export interface McpDependants {
  mcp_env: McpEnvDependant[];
  sandbox: McpSandboxDependant[];
  /** Names only, never a value. */
  secrets: string[];
  /** Whether somebody holds an OAuth grant for it, which the removal revokes. */
  oauth: boolean;
}

/** `GET /mcp-servers/{name}` as an administrator reads it: one server, whole. */
export interface McpServerAnswer {
  reading: "admin";
  server: McpServerStatus;
  nodes: McpStatusNode[];
  listed: McpServerListing[];
  /** Every tool a live node's instances list, by name. */
  tools: McpToolView[];
  /** How many tools the server lists; never fewer than `tools` holds. */
  tool_count: number;
  /**
   * The level of every tool `tools.levels` does not name, or null where the
   * entry sets no default, which leaves each such tool at the level its hints
   * start it at (allow for a read-only tool, approve for every other).
   */
  baseline: McpToolLevel | null;
  /** Tools `tools.levels` names that no live listing names; empty where the listings cannot say. */
  unlisted_levels: string[];
  server_info: McpServerInfo | null;
  /** What the server offers a client for using it, its own words; "" where none. */
  instructions: string;
  dependants: McpDependants;
  instances: McpInstanceView[];
  instance_nodes: McpInstanceNodeView[];
  /** When an instance last answered, or null. */
  last_success: string | null;
  /** The latest failure of any instance, kept after it came back, or null. */
  last_failure: McpFailureView | null;
  /** Null for a server the configuration does not declare. */
  resolved: McpResolved | null;
  usage: McpServerUsage;
  calls: McpServerCalls;
  /** The server's latest test, whoever asked through whichever node; null where never tested. */
  last_check: McpCheckView | null;
  /** Every header or variable the server is sent, and its OAuth client's secret, by name and never by value. */
  values: McpValueState[];
  /** Every agent seat the server's grant reaches, and why, whether or not it is on. */
  reaches: McpReach[];
  /** Every change the server made to its tools since their levels were decided, by tool. */
  changes: McpChangeView[];
}

/** One definition of a tool, in the server's words, read off the hints it declared. */
interface McpDefinitionView {
  digest: string;
  title: string;
  description: string;
  annotations: ToolAnnotations;
  risk: McpRisk;
  risk_declared: boolean;
  external: boolean;
  input_schema: Record<string, unknown> | null;
  output_schema: Record<string, unknown> | null;
}

/** The definition a tool's level was decided on. */
interface McpAcceptedView extends McpDefinitionView {
  /** Null for the server's first listing, accepted as it was listed. */
  accepted_by: string | null;
  accepted_at: string;
  first_listing: boolean;
}

/** The definition a server lists for a tool now. */
interface McpListedView extends McpDefinitionView {
  instances: number;
}

/**
 * One change to a server's tools since their levels were decided: `kinds` is
 * `added` or `removed` alone, or the parts that moved (`title`,
 * `description`, `schema`, `annotations`). Decided with
 * `POST /mcp-servers/{name}/changes` by `after.digest` (null for a removal).
 */
interface McpChangeView {
  tool: string;
  kinds: string[];
  before: McpAcceptedView | null;
  after: McpListedView | null;
  /** Its declared hints claim less risk than the accepted ones. */
  loosened: boolean;
  /** It holds the tool at Needs approval now. */
  held: boolean;
  /** When the fleet first stored the definition listed now, or null. */
  seen_at: string | null;
}

/**
 * Why a server reaches an agent: no `access` (every agent), a unit or the seat
 * it names, or the integration lane a vendor-bound server's agents are on at.
 */
type McpReachReason = "every_agent" | "unit" | "seat" | "integration";

/** One agent seat a server reaches. */
interface McpReach {
  /** The seat's handle. */
  seat: string;
  reason: McpReachReason;
  /** The granting unit's key for `unit`, the lane for `integration`, null otherwise. */
  via: string | null;
}

/**
 * Where a value goes: a remote server's header, a local one's variable, or
 * (`oauth`, key `oauth.client_secret`) the company's own OAuth client's
 * secret, sent to the server's authorization server alone.
 */
type McpValueAs = "header" | "env" | "oauth";

/**
 * One header or variable a server is sent, by name: the variable it reads and
 * whether the store holds it. Never the value (values stored from the page are
 * write-only).
 */
interface McpValueState {
  key: string;
  as: McpValueAs;
  /** The one variable it reads, "" for a literal, several, or a per-agent binding. */
  variable: string;
  set: boolean;
  stored_at: string | null;
  stored_by: string | null;
  /** The server starts without it when it resolves to nothing. */
  optional: boolean;
  /** The integration that hands each agent its own value under the key, or null. */
  bound_by: string | null;
}

/** A stage a test of a server walks, in order (`mcp.TestStages`). */
type McpTestStage = "address" | "resolve" | "connect" | "tls" | "launch" | "discover" | "list";

/** How one stage of a test went. */
type McpStepState = "passed" | "failed" | "skipped" | "not_reached";

/** A server's latest test, as its detail answers it: never its tools. */
interface McpCheckView {
  at: string;
  by: string;
  /** The handle of the seat a per-agent server was tested for, "" for a shared server. */
  seat: string;
  /** Some node answered, and every node that answered found the server serving. */
  ok: boolean;
  nodes: McpCheckNodeView[];
}

/** What one node's test found. */
interface McpCheckNodeView {
  /** "" for a seat's test no node holding the seat answered. */
  node: string;
  answered: boolean;
  /** Why a node that did not answer did not. */
  reason: string;
  ok: boolean;
  steps: McpCheckStepView[];
  /** Why the test stopped, or `empty_tools` for a server listing nothing it can offer. */
  failure: McpCheckFailure | null;
  server_info: McpServerInfo | null;
  tools: number;
  refused: number;
  took_ms: number;
}

/** One stage of a node's test. */
interface McpCheckStepView {
  stage: McpTestStage;
  state: McpStepState;
  took_ms: number;
  /** What the stage found, or the failure's sentence for the stage that failed. */
  detail: string;
}

/** Why a node's test stopped: its class ("" where none fits) and its sentence. */
interface McpCheckFailure {
  class: McpFailureClass | "";
  said: string;
}

/** `GET /mcp-servers/{name}` as a member reads it. */
export interface McpMemberServerAnswer {
  reading: "member";
  server: McpMemberServerStatus;
  nodes: McpStatusNode[];
  listed: McpServerListing[];
  tools: McpToolView[];
  tool_count: number;
  server_info: McpServerInfo | null;
  instances: McpMemberInstanceView[];
  instance_nodes: McpMemberInstanceNodeView[];
  last_success: string | null;
  last_failure: McpMemberFailureView | null;
  resolved: McpMemberResolved | null;
  usage: McpMemberServerUsage;
}

/**
 * What became of one value a removal was asked to delete. `deleted`: the
 * store held it and no longer does. `absent`: it held nothing under the name.
 * `kept`: a revision activated after the removal reads it. `failed`: the
 * store or the fleet's revision did not answer, so it may still be there.
 */
type McpSecretOutcome = "deleted" | "absent" | "kept" | "failed";

/** One value a removal was asked to delete, and what became of it. */
export interface McpSecretDeletion {
  name: string;
  outcome: McpSecretOutcome;
  /** For a value kept, the paths of the fleet's revision that read it. */
  read_by?: string[];
}

/** The fields a written removal cleared with the server. */
interface McpCleared {
  mcp_env: McpEnvDependant[];
  sandbox: McpSandboxDependant[];
}

/** A dry run of a removal that would land: what it checked and what it takes. */
export interface McpRemovalCheck {
  valid: boolean;
  base_revision_id: string;
  warnings: unknown[];
  derived: unknown;
  dependants: McpDependants;
}

/** A removal written: the revision, what it cleared, and each value's fate. */
export interface McpRemovalWritten {
  revision_id: string;
  epoch: number;
  warnings: unknown[];
  derived: unknown;
  removed: string;
  cleared: McpCleared;
  secrets: McpSecretDeletion[];
  /** What became of its OAuth grant, null where nobody held one. */
  oauth: McpGrantRevocation | null;
}

/**
 * What a removal did with the server's OAuth grant: what its vendor was asked
 * (`revoked`, `not_offered` where it publishes no revocation endpoint,
 * `failed`) and, where it refused, its words. Forgotten here whatever it said.
 */
interface McpGrantRevocation {
  revocation: "revoked" | "not_offered" | "failed";
  said: string;
}

/**
 * A restart as recorded (`202`): every node restarts each instance whose
 * state began before `at`, or the node holding `seat` that agent's instance.
 */
export interface McpRestartAnswer {
  server: string;
  /** "" for every instance. */
  seat: string;
  at: string;
  /** The node that recorded the request. */
  node: string;
}
