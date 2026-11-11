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
 * whole) and `TestTheInboxReadsWhatAHeldCallSends` (a parked run's held call)
 * in `internal/api/queries`, `TestTheRemoveDialogReadsWhatTheRemovalSends`
 * in `internal/api/configapi`, and in `internal/api/mcpapi`
 * `TestTheRestartReadsWhatTheRouteSends`, `TestTheWizardReadsWhatTheCatalogueSends`,
 * `TestTheTestReadsWhatTheCheckSends`, `TestTheReplaceValueReadsWhatTheRouteSends`,
 * `TestTheApprovalReadsWhatTheRouteSends` and `TestTheReviewReadsWhatTheRouteSends`.
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
export type McpOAuthStatus = "connected" | "needs_sign_in" | "expired" | "refresh_failed";

/** A renewal a vendor refused for good, in its own words. */
interface McpOAuthFailure {
  said: string;
  at: string;
}

/**
 * A server's OAuth sign-in as an administrator reads it: never a token, never
 * the client's secret, and never on a member's reading.
 */
export interface McpOAuthView {
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
export interface McpApproval {
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
  /** The decision the call carries out: the newest question the seat asked on
   *  that item which a person answered; null for none. */
  acting: McpApprovalActing | null;
  /** What the call would send; null where its seal does not open on this node. */
  args: Record<string, unknown> | null;
  status: McpApprovalStatus;
  /** `once`, `always` or `deny` once answered; "" while waiting or where the answer's seal does not open. */
  answer: string;
  answered_by: string;
  answered_at: string | null;
}

/** The decision a held call carries out, as its approver reads it. */
interface McpApprovalActing {
  /** The work item, by id and by key. */
  item: string;
  key?: string;
  /** The question's comment id, and what it asked. */
  ask: string;
  question: string;
  /** The chosen option's label; absent for an answer in words. */
  choice?: string;
  answered_by: string;
  answered_at: string;
}

/** One agent seat's MCP tools, counted against `tool_budget`. */
export interface McpAgentTools {
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
export interface McpAgentServer {
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
export type McpToolLevel = "allow" | "approve" | "blocked";

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

/** Who holds an `mcp_env` or `mcp_limits` block naming the server. */
type McpHolderKind = "seat" | "unit";

/**
 * One agent's or team's block naming the server: its own values for it
 * (`mcp_env`), or what its lead holds it back to (`mcp_limits`).
 */
interface McpBlockDependant {
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
  mcp_env: McpBlockDependant[];
  sandbox: McpSandboxDependant[];
  mcp_limits: McpBlockDependant[];
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
  /**
   * Every agent seat the server's grant reaches, and why, whether or not it is
   * on, with what its team leads' limits hold the server to for it.
   */
  reaches: McpReach[];
  /** Every change the server made to its tools since their levels were decided, by tool. */
  changes: McpChangeView[];
}

/** One definition of a tool, in the server's words, read off the hints it declared. */
interface McpDefinitionView {
  digest: string;
  /**
   * Read from the definition's record, so its words and schemas are whole.
   * False where the fleet keeps none: the title and description are a
   * listing's cut words.
   */
  whole: boolean;
  /** The tool's own title, and the one its hints carry ("" where `whole` is false). */
  title: string;
  hints_title: string;
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
export interface McpChangeView {
  tool: string;
  kinds: string[];
  /**
   * The parts in `kinds` this answer cannot show the change of: say each
   * changed and is not shown, never that nothing changed.
   */
  unshown: string[];
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
export type McpReachReason = "every_agent" | "unit" | "seat" | "integration";

/** One agent seat a server reaches. */
export interface McpReach {
  /** The seat's handle. */
  seat: string;
  reason: McpReachReason;
  /** The granting unit's key for `unit`, the lane for `integration`, null otherwise. */
  via: string | null;
  /** What the seat's own and its units' `mcp_limits` hold the server to; null where none does. */
  limited: McpReachLimit | null;
}

/** The level a limit holds a tool to: never `allow`, which only the server's entry sets. */
type McpToolLimitLevel = "approve" | "blocked";

/**
 * What the limits on the way down to one agent hold a server to: the strictest
 * of its own block and every enclosing team's, each part naming the block
 * nearest the agent that holds it there.
 */
interface McpReachLimit {
  /** A limit turns the server off for the agent; its grant stands. */
  off: boolean;
  /** The key of the team whose limit turns it off; null where the agent's own does, or none. */
  off_by: string | null;
  /** Each tool held back, by the server's own name for it. */
  tools: McpToolLimit[];
}

/** One tool a limit holds back for an agent; it runs at the stricter of this and its own level. */
interface McpToolLimit {
  tool: string;
  level: McpToolLimitLevel;
  /** The key of the team whose limit holds it; null for the agent's own. */
  by: string | null;
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
export interface McpValueState {
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
export type McpTestStage =
  "address" | "resolve" | "connect" | "tls" | "launch" | "discover" | "list";

/** How one stage of a test went. */
export type McpStepState = "passed" | "failed" | "skipped" | "not_reached";

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
export interface McpCheckNodeView {
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
  mcp_env: McpBlockDependant[];
  sandbox: McpSandboxDependant[];
  mcp_limits: McpBlockDependant[];
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
 * Where a sign-in is: `pending` until the person consents at the vendor,
 * `completing` while the engine exchanges what the vendor sent back, then
 * `approved` or `failed` (with `detail`).
 */
type McpSignInStatus = "pending" | "completing" | "approved" | "failed";

/**
 * A sign-in to a server's OAuth: what `POST /mcp-servers/{name}/oauth` answers
 * and `GET /mcp-sign-ins/{id}` reads. Never a token.
 */
export interface McpSignIn {
  id: string;
  server: string;
  status: McpSignInStatus;
  /** Why it failed, in the engine's or the vendor's words; "" otherwise. */
  detail: string;
  started_at: string;
  /** When the person's consent stops being taken. */
  expires_at: string;
  /** Where the person's browser consents: on the start's answer alone, "" on every read. */
  authorize_url: string;
}

/** What `DELETE /mcp-servers/{name}/oauth` did with the server's grant. */
export interface McpSignOut extends McpGrantRevocation {
  server: string;
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

/**
 * How a server is signed in to, as the add flow offers it. `integration`: the
 * company's integration for the vendor hands each agent its own credential.
 * `oauth`: the company signs in once at the vendor. `headers`: a static
 * credential in a request header. `none`: no sign-in.
 */
export type McpAuthMethod = "integration" | "oauth" | "headers" | "none";

/**
 * How the engine registers as an OAuth client at a vendor. `cimd`: it
 * publishes its own client metadata, which the vendor reads. `dcr`: it
 * registers a client on first use. `own`: an administrator registered one.
 */
export type McpOAuthClient = "cimd" | "dcr" | "own";

/** One header or variable a catalogue entry reads, and how to fill it. */
interface McpCatalogueInput {
  name: string;
  description: string;
  /** Whether the server works without it. */
  required: boolean;
  /** Whether the value is a credential, entered write-only. */
  secret: boolean;
  /** A fixed value the entry sets (never a secret), or "" for one the operator gives. */
  value: string;
  /** How a value is spelled ("Bearer <API key>"), or "". */
  format: string;
  /** Whether the entry's integration hands each agent its own value under it. */
  from_integration: boolean;
}

/** A tool's hints as an entry corrects them; an absent hint is left as the server says it. */
interface McpCatalogueHints {
  read_only?: boolean;
  destructive?: boolean;
  idempotent?: boolean;
  open_world?: boolean;
}

/**
 * One server the catalogue offers: a curated entry, or a community listing
 * from the company's registry, which nobody here reviewed (`community`).
 */
export interface McpCatalogueEntry {
  /** The entry's key: the curated table's, or the registry's listing name. */
  id: string;
  /** The server name the add flow suggests. */
  name: string;
  title: string;
  /** The vetted vendor mark to draw, or "". Never anything a listing supplied. */
  vendor: string;
  description: string;
  transport: "http" | "stdio";
  /** A remote server's address, "" for a local one. */
  url: string;
  /** A local server's launch, passed as written, never through a shell. */
  command: string;
  args: string[];
  /** What a remote server reads. */
  headers: McpCatalogueInput[];
  /** What a local server reads. */
  env: McpCatalogueInput[];
  tool_prefix: string;
  /** Corrections to the hints of tools the server leaves undeclared, by the tool's own name. */
  tool_annotations: Record<string, McpCatalogueHints>;
  /** The sign-ins the vendor offers, the add flow's first first; empty where a listing declares none. */
  auth: McpAuthMethod[];
  oauth_client: McpOAuthClient | "";
  /** The company integration that hands each agent its own credential, or "". */
  integration: string;
  /** What an administrator must do at the vendor before the server works, or "". */
  prerequisite: string;
  /** The vendor's page for the server, always https, or "". */
  docs: string;
  /** The day a curated entry's facts were last read (YYYY-MM-DD), "" for a listing. */
  read_at: string;
  package: string;
  release: string;
  community: boolean;
  /** The namespace the registry verified the listing's publisher for, "" for a curated entry. */
  publisher: string;
  version: string;
  /** When the registry last saw the listing change, or null. */
  updated_at: string | null;
}

/** `GET /mcp-catalogue`, the administrator's reading: what a server is added from. */
export interface McpCatalogueAnswer {
  reading: "admin";
  /** Every curated server, in the catalogue's order, whatever `q` says. */
  curated: McpCatalogueEntry[];
  /** What the registry answered for `q`; empty without one. */
  community: McpCatalogueEntry[];
  /** The registry the company names (`mcp_registry.url`), "" for none. */
  registry: string;
  /** Why a search found nothing to offer, in the engine's words; "" otherwise. */
  reason: string;
  /** How many listings the registry answered that this build could not offer. */
  left_out: number;
}

/** One tool a test listed, as the engine would offer it. */
export interface McpCheckedTool {
  /** The name it would be offered under, prefix included. */
  name: string;
  /** The server's own name for it. */
  raw: string;
  title: string;
  description: string;
  annotations: ToolAnnotations;
  risk: McpRisk;
  risk_declared: boolean;
  external: boolean;
  /** The level it starts at when its server is added, decided by the engine from its hints. */
  starting_level: McpToolLevel;
  /** Why it would not be offered under its name, or "". */
  refused: McpToolRefusal | "";
}

/**
 * A test of a server, saved (`POST /mcp-servers/{name}/check`) or a draft
 * (`POST /mcp-checks`): what each node found, and from the first that found
 * it serving, who it said it is and the tools it would offer.
 */
export interface McpCheckAnswer {
  server: string;
  /** The seat a per-agent server was tested for, "" for a shared one. */
  seat: string;
  draft: boolean;
  at: string;
  by: string;
  /** The node the test was asked through. */
  node: string;
  /** Some node answered, and every node that answered found the server serving. */
  ok: boolean;
  nodes: McpCheckNodeView[];
  server_info: McpServerInfo | null;
  tools: McpCheckedTool[];
  /** What a reader of the tools should act on, in the engine's words. */
  warnings: string[];
}

/** What a server's OAuth metadata says, each address with its credentials withheld. */
interface McpProbeOAuth {
  resource_metadata: string;
  resource: string;
  authorization_servers: string[];
  issuer: string;
  scopes: string[];
  client: McpOAuthClient | "";
  pkce: boolean;
  /** Why the discovery stopped short, or null. */
  failure: McpCheckFailure | null;
}

/** `POST /mcp-probe`: an address checked before a server is added at it, with no credential sent. */
export interface McpProbeAnswer {
  /** The address checked, its credentials withheld. */
  address: string;
  /** Whether the server answered: its handshake, or a refusal asking for a credential. */
  reachable: boolean;
  check: McpCheckNodeView;
  /** The sign-in it asks for, "" where the check did not reach an answer. */
  auth: McpAuthMethod | "";
  oauth: McpProbeOAuth | null;
  /** The tools it lists to no credential, null where it asks for a sign-in first. */
  tools: number | null;
  /** The curated entry the address is, "" where the catalogue does not know it. */
  catalogue: string;
  title: string;
  vendor: string;
}

/** The revision a stored value was activated in, or why it was not. */
interface McpValueReload {
  revision_id?: string;
  epoch?: number;
  error?: string;
  detail?: string;
  /** Sealed and not active yet: a later re-activation makes it so. */
  deferred?: boolean;
}

/** `PUT /mcp-servers/{name}/values/{key}`: a value stored, write-only, and what it was tested as. */
export interface McpValueAnswer {
  server: string;
  key: string;
  /** "" for the server's own value. */
  seat: string;
  /** The variable it is sealed under. */
  variable: string;
  tested: boolean;
  /** The test it passed, null for a value stored untested. */
  check: McpCheckAnswer | null;
  /** Whether the configuration was written to read the variable. */
  pointed: boolean;
  /** The revision that resolves it: the pointer's write or a re-activation. */
  reload: McpValueReload | null;
  /** The server's values now, by name. */
  values: McpValueState[];
}

/**
 * A value the server did not work with (`422 value_test_failed`): where the
 * test stopped, and the test whole. Nothing was stored.
 */
export interface McpValueRefusal {
  detail: string;
  stage: McpTestStage | "";
  class: McpFailureClass | "";
  node: string;
  check: McpCheckAnswer;
}

/**
 * The call a parked executor's run holds for a person's approval, on its row
 * of the executors board (`held_call`): who reads the run's question reads
 * it, never what it would send (`held_arguments`, the seat's trail). It is
 * answered through `POST /mcp-approvals/{id}`, never the run's own answer.
 */
export interface McpHeldCall {
  /** What the approval is answered by. */
  id: string;
  server: string;
  /** The tool as the run called it, its prefix applied. */
  tool: string;
  /** The server's own name for it. */
  raw: string;
  /** The definition the call was held on. */
  digest: string;
  /** When the call was held; "" where the row does not say. */
  held_at: string;
  /** A call its tool asks about every time (an SSH operate command): answered Allow once or Deny alone. */
  fixed?: boolean;
  /** Who answers it where not the seat's lead: `ssh` for an SSH operate command's approvers. */
  approvers?: string;
}

/** `GET /mcp-approvals/{id}`: the held call whole, for whoever may answer it. */
export interface McpHeldWhole {
  id: string;
  server: string;
  tool: string;
  seat: string;
  turn_id: string;
  launch_id: string;
  question: string;
  context: string;
  /** Every argument the call would send, opened from its seal. */
  arguments: Record<string, unknown>;
  /** What Allow once of a call read whole names, so it is given only from a surface that showed all of it. */
  arguments_sha256: string;
}

/**
 * How an answer to a held call travels. `pending`: on its way to the node
 * holding the agent's seat, which makes or refuses the call and carries the
 * run on. `unknown`: it may or may not have landed, and answering again is
 * safe, since the seat records one answer.
 */
type McpApprovalOutcome = "pending" | "unknown";

/** What `POST /mcp-approvals/{id}` answers: the call it answered, and how the answer travels. */
export interface McpApprovalAnswer {
  id: string;
  server: string;
  tool: string;
  seat: string;
  turn_id: string;
  launch_id: string;
  /** `once`, `always` or `deny`, as sent. */
  answer: string;
  outcome: McpApprovalOutcome;
  /** The delivery's id, "" where none was minted. */
  answer_id: string;
  /** The configuration's answer to the level `always` wrote; null for `once` and `deny`. */
  revision: Record<string, unknown> | null;
}

/**
 * What `POST /mcp-servers/{name}/changes` answers: the tools whose decision
 * was written, by the server's own name, and the ones somebody else decided
 * between the read and the write, which are read again.
 */
export interface McpChangesAnswer {
  server: string;
  accepted: string[];
  blocked: string[];
  /** Decided by somebody else meanwhile: nothing was written for them. */
  moved: string[];
  /** The configuration's answer to the levels a block wrote; null where nothing was blocked. */
  revision: Record<string, unknown> | null;
}
