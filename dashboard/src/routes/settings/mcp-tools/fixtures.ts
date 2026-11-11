/**
 * Settings › MCP & tools' readings as the engine sends them, for every suite
 * on the page: `mcp_servers_status` in both readings and a server whole, over
 * one company.
 *
 * ONE COMPANY: two agent seats whose display names differ from their handles
 * (a surface that printed a handle where a name belongs fails on sight), and
 * four servers, one in each condition a reader tells apart: a per-agent
 * GitHub server bound to its integration and running for both agents, a
 * shared remote Linear server down on one node, a local docs server turned
 * off, and a search server with a tool no agent is offered. Every instant is
 * `MCP_NOW` or minutes before it.
 */

import type {
  McpApproval,
  McpChangeView,
  McpMemberServerAnswer,
  McpMemberServerStatus,
  McpMemberServersStatusAnswer,
  McpOAuthView,
  McpServerAnswer,
  McpServerStatus,
  McpServersStatusAnswer,
  McpToolView,
} from "~/contract/mcp.ts";
import type { ToolAnnotations, ToolRow } from "~/contract/tools.ts";
import { indexOrg } from "~/lib/seats.ts";
import { withDerived } from "~/test/org.ts";
import { mcpViewOf, type McpData } from "./data.ts";

/** The instant every reading here is taken at. */
export const MCP_NOW = Date.parse("2026-11-01T12:00:00Z");

/** An instant `minutes` from `MCP_NOW` (negative for the past), as the wire writes it. */
export const at = (minutes: number) => new Date(MCP_NOW + minutes * 60_000).toISOString();

/** The company: two agents, named apart from their handles, each holding the servers it reaches. */
export const MCP_ORG = withDerived({
  name: "Acme",
  roles: [
    {
      name: "Backend Engineer",
      handle: "backend-engineer",
      tool_sources: ["builtin", "mcp:github", "mcp:linear", "mcp:search"],
    },
    {
      name: "Product Manager",
      handle: "pm",
      tool_sources: ["builtin", "mcp:github", "mcp:linear"],
    },
  ],
});

const node = (id: string, cell: Partial<McpServerStatus["nodes"][number]> = {}) => ({
  node: id,
  reported: true,
  started: 0,
  failed: 0,
  waiting: 0,
  tools: 0,
  error: "",
  error_seat: "",
  wait: "",
  wait_seat: "",
  error_class: "" as const,
  down_since: null,
  restarts: 0,
  ...cell,
});

const server = (
  row: Partial<McpServerStatus> & Pick<McpServerStatus, "name">,
): McpServerStatus => ({
  title: row.name,
  catalogue: "",
  vendor: "",
  configured: true,
  shared: true,
  enabled: true,
  description: "",
  transport: "http",
  command: "",
  args: [],
  url: "",
  oauth: null,
  state: "running",
  started: 0,
  failed: 0,
  waiting: 0,
  tools: 0,
  unlisted_annotations: [],
  refused_tools: [],
  refused_count: 0,
  shadowed_tools: [],
  shadowed_count: 0,
  nodes: [],
  ...row,
});

/** The four servers as an administrator reads them, in name order. */
export const SERVERS: McpServerStatus[] = [
  server({
    name: "docs",
    title: "docs",
    transport: "stdio",
    command: "docs-mcp",
    args: ["--read-only"],
    enabled: false,
    state: "disabled",
    description: "Searches the engineering handbook",
    nodes: [node("node-a"), node("node-b")],
  }),
  server({
    name: "github",
    title: "GitHub",
    catalogue: "github",
    vendor: "github",
    shared: false,
    transport: "stdio",
    command: "github-mcp-server",
    args: ["stdio"],
    state: "running",
    started: 2,
    tools: 42,
    nodes: [node("node-a", { started: 1, tools: 42 }), node("node-b", { started: 1, tools: 42 })],
  }),
  server({
    name: "linear",
    title: "Linear",
    catalogue: "linear",
    vendor: "linear",
    url: "https://mcp.linear.app/mcp",
    state: "partial",
    started: 1,
    failed: 1,
    tools: 23,
    nodes: [
      node("node-a", { started: 1, tools: 23 }),
      node("node-b", {
        failed: 1,
        error: "401 from the server: invalid token",
        error_class: "credential_rejected",
        down_since: at(-12),
        restarts: 3,
      }),
    ],
  }),
  server({
    name: "search",
    title: "search",
    url: "https://search.example.com/mcp",
    state: "running",
    started: 1,
    tools: 5,
    refused_tools: [
      {
        tool: "search_query.v2",
        reason: "characters",
        message:
          "search_query.v2 holds a character a tool name cannot: set a tool_prefix or ask the server's owner",
      },
    ],
    refused_count: 1,
    nodes: [node("node-a", { started: 1, tools: 5 }), node("node-b")],
  }),
];

const unadvertised: ToolAnnotations = {
  read_only: "unknown",
  destructive: "unknown",
  idempotent: "unknown",
  open_world: "unknown",
};

const builtinTool = (name: string, row: Partial<ToolRow>): ToolRow => ({
  name,
  description: `What ${name} does`,
  source: "builtin",
  annotations: unadvertised,
  risk: "destructive",
  risk_declared: false,
  external: true,
  delivers: "",
  group: "work",
  gate: "worklet",
  ...row,
});

/** The administrator's `mcp_servers_status`. */
export const ADMIN_STATUS: McpServersStatusAnswer = {
  reading: "admin",
  callback_url: "https://crewlet.example.com/webhooks/mcp-oauth",
  client_metadata_url: "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json",
  servers: SERVERS,
  nodes: [
    { id: "node-a", reported: true },
    { id: "node-b", reported: true },
  ],
  builtin: [
    {
      key: "work",
      label: "Work items",
      description: "Worklet, the company's tracker: read, file, discuss and move work.",
      tools: [
        builtinTool("list_work_items", {
          risk: "read_only",
          risk_declared: true,
          input_schema: {
            type: "object",
            properties: { project: { type: "string", description: "The project's key" } },
            required: ["project"],
          },
        }),
        builtinTool("create_work_item", { risk: "additive", risk_declared: true }),
      ],
    },
    {
      key: "memory",
      label: "Memory and skills",
      description: "The agent's own notes, turns and skills.",
      tools: [builtinTool("forget_note", { group: "memory", gate: "self" })],
    },
  ],
  alarms: [
    {
      kind: "mcp_server_partial",
      detail: "1 of 2 instances of linear is not serving",
      remedy: "",
      entries: [
        {
          key: "linear",
          detail: "1 of 2 instances of linear is not serving (on node-b, credential_rejected): 401",
          since: at(-12),
          seat: "",
          fix: { kind: "restart", server: "linear", seat: "" },
        },
      ],
    },
    {
      kind: "mcp_tools_refused",
      detail: "search: 1 tool is not offered",
      remedy: "",
      entries: [
        {
          key: "search",
          detail: "search: 1 tool is not offered",
          since: null,
          seat: "",
          fix: { kind: "edit", server: "search", seat: "" },
        },
      ],
    },
  ],
  approvals: [],
  agents: [
    {
      seat: "backend-engineer",
      name: "Backend Engineer",
      unit: null,
      unit_key: null,
      servers: [
        { server: "docs", reason: "every_agent", tools: 0, state: "disabled" },
        { server: "github", reason: "integration", tools: 42, state: "running" },
        { server: "linear", reason: "every_agent", tools: 23, state: "partial" },
        { server: "search", reason: "seat", tools: 5, state: "running" },
      ],
      mcp_tools: 70,
      builtin_tools: 3,
      over_budget: false,
    },
    {
      seat: "pm",
      name: "Product Manager",
      unit: null,
      unit_key: null,
      servers: [
        { server: "docs", reason: "every_agent", tools: 0, state: "disabled" },
        { server: "github", reason: "integration", tools: 42, state: "running" },
        { server: "linear", reason: "every_agent", tools: 23, state: "partial" },
      ],
      mcp_tools: 65,
      builtin_tools: 3,
      over_budget: false,
    },
  ],
  tool_budget: 100,
};

/** A member's row of a server: the administrator's, its launch line and sentences left out. */
function memberRow(row: McpServerStatus): McpMemberServerStatus {
  const { command: _c, args: _a, url: _u, unlisted_annotations: _n, nodes, ...rest } = row;
  return {
    ...rest,
    nodes: nodes.map(({ error: _e, error_seat: _s, wait: _w, wait_seat: _ws, ...cell }) => cell),
  };
}

/** A member's `mcp_servers_status`: no launch line, no sentence, no alarm. */
export const MEMBER_STATUS: McpMemberServersStatusAnswer = {
  reading: "member",
  servers: SERVERS.map(memberRow),
  nodes: ADMIN_STATUS.nodes,
  builtin: ADMIN_STATUS.builtin,
};

/** Linear whole, as an administrator reads it: a shared remote server down on one node. */
export const LINEAR_DETAIL: McpServerAnswer = {
  reading: "admin",
  server: SERVERS[2]!,
  nodes: ADMIN_STATUS.nodes,
  listed: [{ node: "node-a", at: at(-30), instances: 1, tools: 2 }],
  tools: [
    {
      name: "linear_list_issues",
      raw: "list_issues",
      title: "List issues",
      description: "List the issues in a team",
      annotations: { ...unadvertised, read_only: "yes", open_world: "no" },
      risk: "read_only",
      risk_declared: true,
      external: false,
      digest: "a".repeat(64),
      definitions: 1,
      instances: 1,
      nodes: ["node-a"],
      input_schema: { type: "object" },
      output_schema: null,
      level: "allow",
      custom: false,
      held: false,
      removed: false,
    },
    {
      name: "linear_delete_issue",
      raw: "delete_issue",
      title: "",
      description: "Delete an issue",
      annotations: unadvertised,
      risk: "destructive",
      risk_declared: false,
      external: true,
      digest: "b".repeat(64),
      definitions: 1,
      instances: 1,
      nodes: ["node-a"],
      input_schema: { type: "object" },
      output_schema: null,
      level: "approve",
      custom: true,
      held: false,
      removed: false,
    },
  ],
  tool_count: 2,
  baseline: null,
  unlisted_levels: [],
  server_info: { name: "linear-mcp", version: "2.1.0", protocol: "2026-07-28" },
  instructions: "Search before you create an issue.",
  dependants: { mcp_env: [], sandbox: [], mcp_limits: [], secrets: ["LINEAR_TOKEN"], oauth: false },
  instances: [
    {
      seat: "",
      node: "node-a",
      state: "running",
      since: at(-30),
      restarts: 0,
      tools: 2,
      failure: null,
      last_failure: null,
      last_answer: at(-2),
      wait: "",
    },
    {
      seat: "",
      node: "node-b",
      state: "failed",
      since: at(-12),
      restarts: 3,
      tools: 0,
      failure: {
        class: "credential_rejected",
        said: "401 from the server: invalid token",
        at: at(-12),
        node: "node-b",
        seat: "",
      },
      last_failure: null,
      last_answer: null,
      wait: "",
    },
  ],
  instance_nodes: [
    { node: "node-a", answered: true, reason: "", count: 1 },
    { node: "node-b", answered: true, reason: "", count: 1 },
  ],
  last_success: at(-2),
  last_failure: {
    class: "credential_rejected",
    said: "401 from the server: invalid token",
    at: at(-12),
    node: "node-b",
    seat: "",
  },
  resolved: {
    auth: "headers",
    runs_on: "",
    timeouts: {
      startup_seconds: 120,
      request_seconds: 300,
      startup_default: true,
      request_default: true,
    },
    binding: null,
    box_seats: [],
    warnings: [],
  },
  usage: {
    since: "2026-10-26",
    until: "2026-11-01",
    days: 7,
    elided: 0,
    tools: [
      {
        tool: "linear_list_issues",
        calls: 18,
        failed: 2,
        unavailable: 0,
        refused: 0,
        total_ms: 7300,
        last_at: at(-2),
        seats: ["backend-engineer"],
      },
    ],
  },
  calls: {
    calls: [
      {
        at: at(-2),
        seat: "backend-engineer",
        turn_id: "6c1f0e2a-5b7d-4e8f-9a1b-2c3d4e5f6a7b",
        tool: "linear_list_issues",
        outcome: "answered",
        refusal: "",
        duration_ms: 412,
      },
    ],
    coverage: { nodes: [{ id: "node-a", answered: true, error: "" }], complete: true },
  },
  last_check: null,
  values: [],
  reaches: [
    { seat: "backend-engineer", reason: "every_agent", via: null, limited: null },
    { seat: "pm", reason: "every_agent", via: null, limited: null },
  ],
  changes: [],
};

/** A call Backend Engineer's run holds for approval, to Linear's tool that deletes. */
export const LINEAR_APPROVAL: McpApproval = {
  id: "0190f3a8-0000-7000-8000-00000000000a",
  at: at(-4),
  server: "linear",
  tool: "linear_delete_issue",
  raw: "delete_issue",
  digest: "b".repeat(64),
  seat: "backend-engineer",
  turn_id: "turn-1",
  launch_id: "launch-1",
  item: "ENG-12",
  acting: null,
  args: { issue: "ENG-9" },
  status: "waiting",
  answer: "",
  answered_by: "",
  answered_at: null,
};

/**
 * Two changes Linear made since its levels were decided: its delete tool's
 * description and hints, now claiming less risk and held at Needs approval,
 * and a tool it no longer lists.
 */
export const LINEAR_CHANGES: McpChangeView[] = [
  {
    tool: "delete_issue",
    kinds: ["description", "annotations"],
    unshown: [],
    before: {
      digest: "b".repeat(64),
      whole: true,
      title: "",
      hints_title: "",
      description: "Delete an issue",
      annotations: { ...unadvertised, destructive: "yes" },
      risk: "destructive",
      risk_declared: true,
      external: true,
      input_schema: {
        type: "object",
        properties: { issue: { type: "string" } },
        required: ["issue"],
      },
      output_schema: null,
      accepted_by: null,
      accepted_at: at(-600),
      first_listing: true,
    },
    after: {
      digest: "c".repeat(64),
      whole: true,
      title: "",
      hints_title: "",
      description: "Archive an issue, which can be restored",
      annotations: { ...unadvertised, destructive: "no" },
      risk: "additive",
      risk_declared: true,
      external: true,
      input_schema: {
        type: "object",
        properties: { issue: { type: "string" } },
        required: ["issue"],
      },
      output_schema: null,
      instances: 1,
    },
    loosened: true,
    held: true,
    seen_at: at(-20),
  },
  {
    tool: "export_board",
    kinds: ["removed"],
    unshown: [],
    before: {
      digest: "d".repeat(64),
      whole: true,
      title: "",
      hints_title: "",
      description: "Export a board",
      annotations: { ...unadvertised, read_only: "yes" },
      risk: "read_only",
      risk_declared: true,
      external: false,
      input_schema: { type: "object" },
      output_schema: null,
      accepted_by: "jane.doe",
      accepted_at: at(-300),
      first_listing: false,
    },
    after: null,
    loosened: false,
    held: false,
    seen_at: null,
  },
];

/** A read-only tool's definition, by the digest's seed letter and its words. */
function readOnlyDefinition(seed: string, description: string) {
  const annotations: ToolAnnotations = { ...unadvertised, read_only: "yes", open_world: "no" };
  return {
    digest: seed.repeat(64),
    whole: true,
    title: "",
    hints_title: "",
    description,
    annotations,
    risk: "read_only" as const,
    risk_declared: true,
    external: false,
    input_schema: { type: "object", properties: { team: { type: "string" } } },
    output_schema: null,
  };
}

/** A wording change to one of Linear's read-only tools, its description reworded. */
export function wordingChange(
  tool: string,
  was: string,
  now: string,
  seeds: [string, string],
): McpChangeView {
  return {
    tool,
    kinds: ["description"],
    unshown: [],
    before: {
      ...readOnlyDefinition(seeds[0], was),
      accepted_by: null,
      accepted_at: at(-600),
      first_listing: true,
    },
    after: { ...readOnlyDefinition(seeds[1], now), instances: 1 },
    loosened: false,
    held: true,
    seen_at: at(-20),
  };
}

/** A description that runs past the first KiB a listing keeps of it, and ends as given. */
export function longDescription(ending: string): string {
  return `${"List the issues in a team, newest first, with their state and assignee. ".repeat(16)}${ending}`;
}

/**
 * Linear's read-only tools as it lists them for the wording changes: the
 * administrator's detail rows, each running read-only.
 */
export const LINEAR_READ_TOOLS: McpToolView[] = ["list_issues", "list_teams", "get_issue"].map(
  (raw, i) => ({
    ...LINEAR_DETAIL.tools[0]!,
    name: `linear_${raw}`,
    raw,
    digest: String.fromCharCode(0x6b + i).repeat(64),
  }),
);

/** Linear whole, as a member reads it. */
export const LINEAR_MEMBER_DETAIL: McpMemberServerAnswer = {
  reading: "member",
  server: memberRow(SERVERS[2]!),
  nodes: LINEAR_DETAIL.nodes,
  listed: LINEAR_DETAIL.listed,
  tools: LINEAR_DETAIL.tools,
  tool_count: LINEAR_DETAIL.tool_count,
  server_info: LINEAR_DETAIL.server_info,
  instances: LINEAR_DETAIL.instances.map(({ wait: _w, failure, last_failure, ...i }) => ({
    ...i,
    failure: failure && {
      class: failure.class,
      at: failure.at,
      node: failure.node,
      seat: failure.seat,
    },
    last_failure: last_failure && {
      class: last_failure.class,
      at: last_failure.at,
      node: last_failure.node,
      seat: last_failure.seat,
    },
  })),
  instance_nodes: LINEAR_DETAIL.instance_nodes.map(({ reason: _r, ...n }) => n),
  last_success: LINEAR_DETAIL.last_success,
  last_failure: { class: "credential_rejected", at: at(-12), node: "node-b", seat: "" },
  resolved: {
    auth: "headers",
    runs_on: "",
    timeouts: LINEAR_DETAIL.resolved!.timeouts,
    binding: null,
  },
  usage: {
    ...LINEAR_DETAIL.usage,
    tools: LINEAR_DETAIL.usage.tools.map(({ seats: _s, ...u }) => u),
  },
};

/**
 * The page's whole reading of the company, as the hook joins it, in one
 * reader's reading; `admin` changes the administrator's answer (calls held
 * for approval, each agent's tools).
 */
export function mcpData(
  reading: "admin" | "member",
  reload: () => void = () => {},
  admin: Partial<McpServersStatusAnswer> = {},
): McpData {
  // A MEMBER'S ORG CARRIES NO `tool_sources`: the engine pushes each seat's
  // grants to an administrator alone.
  const org =
    reading === "admin"
      ? MCP_ORG
      : {
          ...MCP_ORG,
          roles: MCP_ORG.roles.map(({ tool_sources: _t, ...role }) => role),
        };
  const seats = indexOrg(org as never).seats.filter((s) => s.kind === "agent");
  return {
    ...mcpViewOf(reading === "admin" ? { ...ADMIN_STATUS, ...admin } : MEMBER_STATUS, seats),
    loading: false,
    failure: null,
    reload,
  };
}

/** Linear's sign-in, made by jane.doe through a client registered automatically. */
export const LINEAR_OAUTH: McpOAuthView = {
  grant: "authorization_code",
  status: "connected",
  client: "dcr",
  client_id: "registered-client",
  issuer: "https://auth.linear.app",
  resource: "https://mcp.linear.app/mcp",
  scopes: ["read", "write"],
  connected_by: "jane.doe",
  connected_at: "2026-11-01T09:00:00Z",
  expires_at: "2026-11-01T13:00:00Z",
  failure: null,
};

/** A service identity's view, as the engine answers it: always connected, nobody behind it. */
export const SERVICE_OAUTH: McpOAuthView = {
  grant: "client_credentials",
  status: "connected",
  client: "",
  client_id: "crewlet-agents",
  issuer: "",
  resource: "",
  scopes: ["read"],
  connected_by: null,
  connected_at: null,
  expires_at: null,
  failure: null,
};

/** The page's data with one server signed in to with OAuth, as `oauth` says. */
export function withOAuth(data: McpData, name: string, oauth: McpOAuthView): McpData {
  return {
    ...data,
    adminRow: (n) => {
      const row = data.adminRow(n);
      return row && n === name ? { ...row, oauth } : row;
    },
  };
}
