/**
 * One server's page, tab by tab, in both readings: each tab draws what the
 * engine answered and what only an administrator reads stays theirs.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type { RestResult } from "~/lib/useRest.ts";
import type { McpApprovalAnswer } from "~/contract/mcp.ts";
import { answerHeldCall } from "~/lib/heldCall.ts";
import { putEntity } from "~/protocol/configWrite.ts";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { sheetRules } from "~/test/sheets.ts";
import { setLevels, type ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import {
  LINEAR_APPROVAL,
  LINEAR_CHANGES,
  LINEAR_DETAIL,
  LINEAR_MEMBER_DETAIL,
  LINEAR_OAUTH,
  MCP_NOW,
  MCP_ORG,
  SERVICE_OAUTH,
  mcpData,
  withOAuth,
} from "../fixtures.ts";
import { ServerPage } from "./ServerPage.tsx";

let detail: ServerDetail;
vi.mock("../data.ts", async (actual) => ({
  ...(await actual<typeof import("../data.ts")>()),
  useServerDetail: (): RestResult<ServerDetail> => ({
    data: detail,
    error: null,
    code: null,
    refusal: null,
    loading: false,
    reload: async () => {},
  }),
}));
let entry: Record<string, unknown> | null = null;
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getEntity: vi.fn(async () =>
    entry ? { kind: "entity", entity: entry, etag: '"r1"' } : { kind: "missing" },
  ),
  putEntity: vi.fn(),
}));
vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  setLevels: vi.fn(async () => ({ kind: "ok", status: 200, value: {} })),
}));
// WHO MAY ANSWER is the rule's own suite (`useWriteAccess.test.ts`): here an admin
// reads the page, and no viewer is mounted.
vi.mock("~/lib/useWriteAccess.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/useWriteAccess.ts")>()),
  useAnswerHold: () => undefined,
}));
vi.mock("~/lib/heldCall.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/heldCall.ts")>()),
  answerHeldCall: vi.fn(),
}));

function mountAt(tab: string, data: McpData, onFlow: (...a: unknown[]) => void = () => {}) {
  location.hash = `#/settings/mcp-tools/servers/linear${tab ? `?tab=${tab}` : ""}`;
  // THE CHART the grant picker lists, as the store holds it.
  const store = new Store();
  store.applyOrg(MCP_ORG as never);
  render(
    <TestClient store={store}>
      <ToastProvider>
        <LayerHost>
          <Router>
            <ServerPage server={data.server("linear")!} data={data} onFlow={onFlow} />
          </Router>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

/** Choose `option` in the kit select named `name`. */
function choose(name: string, option: string | RegExp) {
  fireEvent.click(screen.getByRole("combobox", { name }));
  fireEvent.mouseDown(screen.getByRole("option", { name: option }));
}

beforeEach(() => {
  vi.useFakeTimers({ now: MCP_NOW, toFake: ["Date"] });
  entry = null;
  vi.mocked(setLevels).mockClear();
  vi.mocked(putEntity).mockReset();
  vi.mocked(answerHeldCall).mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

// THE OVERVIEW: each instance with its node and what is wrong in its class's
// words, when the server last answered, what it reported itself as, and, for
// an administrator, the instructions it reports, said not to be the agents'.
test("the overview draws each instance and, for an administrator, the server's instructions", () => {
  detail = LINEAR_DETAIL;
  mountAt("", mcpData("admin"));
  // THE INSTANCES ARE A TABLE, failed rows first and marked as failed.
  const card = screen.getByText("Instance").closest(".crewlet-card") as HTMLElement;
  const instances = within(card).getByRole("table");
  expect(
    within(instances)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Agent", "Node", "State", "Detail", "Since"]);
  const rows = within(instances).getAllByRole("row").slice(1);
  expect(within(rows[0]!).getByText("node-b")).toBeDefined();
  expect(rows[0]!.className).toMatch(/--failed/);
  expect(rows[1]!.className).not.toMatch(/--failed/);
  expect(within(instances).getAllByText("The company")).toHaveLength(2);
  expect(within(instances).getByText("Credential rejected")).toBeDefined();
  expect(screen.getByText("linear-mcp 2.1.0")).toBeDefined();
  expect(screen.getByText("Search before you create an issue.")).toBeDefined();
  expect(screen.getByText(/The agents are not handed them/)).toBeDefined();
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("", mcpData("member"));
  expect(screen.queryByText("Search before you create an issue.")).toBeNull();
  expect(screen.getByText("linear-mcp 2.1.0")).toBeDefined();
});

// THE CONNECTION: an administrator reads the address the engine answers (its
// credentials withheld) and every header by name from the engine's `values`,
// whether it is stored and a way to replace it, never a value; a key an
// integration fills has no Replace; a member reads how it connects and
// signs in, and no name. Mutation: read the headers off the entry again, or
// offer Replace on a bound key, and this is red.
test("the connection names the address and the headers to an administrator alone, never a value", async () => {
  const value = {
    as: "header" as const,
    stored_at: null,
    stored_by: null,
    optional: false,
  };
  detail = {
    ...LINEAR_DETAIL,
    values: [
      {
        ...value,
        key: "Authorization",
        variable: "LINEAR_TOKEN",
        set: true,
        bound_by: null,
        stored_at: "2026-11-01T11:00:00Z",
        stored_by: "jane.doe",
      },
      { ...value, key: "X-Team", variable: "", set: true, bound_by: null },
      { ...value, key: "X-Agent", variable: "", set: true, bound_by: "github" },
      { ...value, key: "X-Region", variable: "MCP_LINEAR_X_REGION", set: false, bound_by: null },
    ],
  };
  entry = { name: "linear", url: "https://mcp.linear.app/mcp", tool_prefix: "lin_" };
  mountAt("connection", mcpData("admin"));
  expect(screen.getByText("https://mcp.linear.app/mcp")).toBeDefined();
  expect(await screen.findByText("lin_")).toBeDefined();
  expect(screen.getByText("Authorization")).toBeDefined();
  expect(screen.getByText("Reads ${LINEAR_TOKEN}")).toBeDefined();
  expect(screen.getByText("Set in the configuration")).toBeDefined();
  expect(screen.getByText("Each agent's own, from github")).toBeDefined();
  expect(screen.getByRole("button", { name: "Replace Authorization" })).toBeDefined();
  expect(screen.getByText("by jane.doe")).toBeDefined();
  expect(screen.getByRole("button", { name: "Set X-Region" })).toBeDefined();
  expect(screen.queryByRole("button", { name: /X-Agent/ })).toBeNull();
  expect(screen.getByText("120 s (the default)")).toBeDefined();
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("connection", mcpData("member"));
  expect(screen.queryByText("https://mcp.linear.app/mcp")).toBeNull();
  expect(screen.queryByText("Authorization")).toBeNull();
  expect(screen.getByText("Values the configuration sets")).toBeDefined();
});

// A SIGN-IN IS ITS OWN CARD: where it is, who made it and when, when access
// ends, the authorization server, the client and the scopes, never a token. A
// person's sign-in is made again and given up here; a service identity names
// its client and its secret, write-only, and offers neither; the secret is the
// sign-in's, never a request header. Mutation: offer Revoke on a sign-in nobody
// holds, Sign in on a service identity, or list the secret among the headers,
// and this is red.
test("the connection's sign-in card says where an OAuth sign-in is and offers what it can do", () => {
  detail = LINEAR_DETAIL;
  const onFlow = vi.fn();
  mountAt("connection", withOAuth(mcpData("admin"), "linear", LINEAR_OAUTH), onFlow);
  expect(screen.getByText("Connected")).toBeDefined();
  expect(screen.getByText(/by jane\.doe on/)).toBeDefined();
  expect(screen.getByText("https://auth.linear.app")).toBeDefined();
  expect(screen.getByText("Register automatically")).toBeDefined();
  expect(screen.getByText("read write")).toBeDefined();
  expect(screen.getByText("https://crewlet.example.com/webhooks/mcp-oauth")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
  expect(onFlow).toHaveBeenLastCalledWith("revoke", "linear");
  fireEvent.click(screen.getByRole("button", { name: "Sign in again" }));
  expect(onFlow).toHaveBeenLastCalledWith("signin", "linear");
  cleanup();

  mountAt(
    "connection",
    withOAuth(mcpData("admin"), "linear", { ...LINEAR_OAUTH, status: "needs_sign_in" }),
  );
  expect(screen.getByText("Needs sign-in")).toBeDefined();
  expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
  expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  cleanup();

  detail = {
    ...LINEAR_DETAIL,
    values: [
      {
        key: "oauth.client_secret",
        as: "oauth",
        variable: "MCP_LINEAR_OAUTH_CLIENT_SECRET",
        set: true,
        stored_at: "2026-11-01T11:00:00Z",
        stored_by: "jane.doe",
        optional: false,
        bound_by: null,
      },
    ],
  };
  mountAt("connection", withOAuth(mcpData("admin"), "linear", SERVICE_OAUTH), onFlow);
  expect(
    screen.getByText("OAuth client credentials, the company's service identity"),
  ).toBeDefined();
  expect(screen.getByText("crewlet-agents")).toBeDefined();
  expect(screen.queryByRole("button", { name: /^Sign in/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  expect(screen.queryByText("oauth.client_secret")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Replace the client secret" }));
  expect(onFlow).toHaveBeenLastCalledWith("value", "linear", { key: "oauth.client_secret" });
  // AN OAUTH SERVER WITH NO HEADER has no headers card: the wizard draws no
  // header rows for it, so an empty card's Edit would lead nowhere.
  expect(screen.queryByText("Request headers")).toBeNull();
});

// THE TOOLS: each with the risk the engine read, a tool that declares nothing
// marked as such, how often it was called this week, and the tool no agent is
// offered under its name with the engine's reason.
test("the tools draw the engine's risk, the week's calls and every name not offered", () => {
  detail = LINEAR_DETAIL;
  const data = mcpData("admin");
  mountAt("tools", data);
  const rowOf = (text: string) => {
    const row = screen.getByText(text).closest("tr, [role='row']");
    if (!(row instanceof HTMLElement)) throw new Error(`no row holds ${text}`);
    return row;
  };
  const list = rowOf("linear_list_issues");
  expect(within(list).getByText("Read only")).toBeDefined();
  expect(within(list).getByText("18")).toBeDefined();
  // A TOOL THAT DECLARES NOTHING says so in words on its chip, never a bare
  // asterisk whose meaning only a hover tip carries.
  expect(within(rowOf("linear_delete_issue")).getByText("Assumed destructive")).toBeDefined();
  expect(screen.queryByText(/\*$/)).toBeNull();
});

// THE ACTIVITY: the counts are every reader's, the calls an administrator's.
test("the activity's counts are every reader's and its calls an administrator's", () => {
  detail = LINEAR_DETAIL;
  mountAt("activity", mcpData("admin"));
  expect(screen.getByText("Calls by tool, last 7 days")).toBeDefined();
  expect(screen.getByText("Recent calls")).toBeDefined();
  expect(screen.getByText("Backend Engineer")).toBeDefined();
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("activity", mcpData("member"));
  expect(screen.getByText("Calls by tool, last 7 days")).toBeDefined();
  expect(screen.getByText("18")).toBeDefined();
  expect(screen.queryByText("Recent calls")).toBeNull();
  expect(screen.queryByText("Backend Engineer")).toBeNull();
});

// THE LEVELS ARE THE ENGINE'S, WRITTEN BY THE SERVER'S OWN NAME: a tool's
// select sends its level keyed by `raw`, a custom one goes back to the
// default with null, the default card sends `default` (null for "by its
// hints"), and Set N shown sends every tool shown. A member reads the level
// in words. Mutation: key a level by its prefixed name, and this is red.
test("the tools' levels are set by the server's own name for each tool, and read by a member", async () => {
  detail = LINEAR_DETAIL;
  mountAt("tools", mcpData("admin"));
  // The custom level's tag, beside the filter chip of the same name.
  expect(screen.getAllByText("Custom")).toHaveLength(2);
  choose("Level for linear_list_issues", "Blocked");
  await afterMicrotasks();
  expect(setLevels).toHaveBeenLastCalledWith(
    "linear",
    { levels: { list_issues: "blocked" } },
    "linear_list_issues set to Blocked",
  );
  choose("Level for linear_delete_issue", /Server default/);
  await afterMicrotasks();
  expect(setLevels).toHaveBeenLastCalledWith(
    "linear",
    { levels: { delete_issue: null } },
    "linear_delete_issue follows the server's default",
  );
  expect(screen.getByRole("combobox", { name: "Default level" }).textContent).toContain(
    "By each tool's hints",
  );
  choose("Default level", "Needs approval");
  await afterMicrotasks();
  expect(setLevels).toHaveBeenLastCalledWith(
    "linear",
    { default: "approve" },
    "Linear's default set to Needs approval",
  );
  fireEvent.click(screen.getByRole("button", { name: "Set 2 tools" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "Always allow" }));
  await afterMicrotasks();
  expect(setLevels).toHaveBeenLastCalledWith(
    "linear",
    { levels: { list_issues: "allow", delete_issue: "allow" } },
    "2 tools set to Always allow",
  );
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("tools", mcpData("member"));
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByText("Default for this server")).toBeNull();
  expect(screen.getByText("Needs approval")).toBeDefined();
  expect(screen.getByText("Always allow")).toBeDefined();
});

// A REFUSED LEVEL SAYS THE ENGINE'S PROBLEM (a box that lists the server,
// say) and changes nothing.
test("a level the engine refuses is said in its words", async () => {
  detail = LINEAR_DETAIL;
  vi.mocked(setLevels).mockResolvedValueOnce({
    kind: "refused",
    status: 422,
    code: "validation_error",
    sentence: "The configuration is not valid.",
    body: {
      problems: [
        {
          path: "roles.0.sandbox.mcp.servers.0",
          message: "Take linear out of the box first: a box runs every tool at Always allow.",
        },
      ],
    },
  });
  mountAt("tools", mcpData("admin"));
  choose("Level for linear_list_issues", "Needs approval");
  await afterMicrotasks();
  expect(screen.getAllByText(/Nothing was changed/).length).toBeGreaterThan(0);
  expect(screen.getAllByText(/Take linear out of the box first/).length).toBeGreaterThan(0);
});

// WHO GETS IT: the table is the engine's `reaches` with each reason, the
// picker writes the entry's `access` through the entity route, and a member
// has no Access tab. Mutation: write the grant beside the entry's own
// fields, or drop `access` for a chosen agent, and this is red.
test("the access tab lists who the server reaches and writes who gets it on its entry", async () => {
  detail = {
    ...LINEAR_DETAIL,
    reaches: [
      {
        seat: "backend-engineer",
        reason: "seat",
        via: null,
        limited: {
          off: false,
          off_by: null,
          tools: [{ tool: "delete_issue", level: "blocked", by: "eng" }],
        },
      },
      {
        seat: "pm",
        reason: "unit",
        via: "product",
        limited: { off: true, off_by: null, tools: [] },
      },
    ],
  };
  entry = {
    name: "linear",
    url: "https://mcp.linear.app/mcp",
    access: { seats: ["backend-engineer"] },
  };
  vi.mocked(putEntity).mockResolvedValue({
    kind: "saved",
    revisionId: "r2",
    epoch: 3,
    warnings: [],
    derived: null,
  });
  mountAt("access", mcpData("admin"));
  const tabs = screen.getByRole("tablist", { name: "Linear sections" });
  expect(within(tabs).getByRole("tab", { name: /Access/ })).toBeDefined();
  expect(screen.getByText("Granted to it")).toBeDefined();
  expect(screen.getByText("Through product")).toBeDefined();
  // A TEAM LEAD'S LIMIT, as the engine resolved it for each agent.
  expect(screen.getByText("1 tool held back by eng's lead")).toBeDefined();
  expect(screen.getByText("Turned off for it by its lead")).toBeDefined();
  expect(screen.getByText("What each agent can call")).toBeDefined();
  expect(await screen.findByRole("radio", { name: /Chosen teams and agents/ })).toBeDefined();
  expect(
    (screen.getByRole("radio", { name: /Chosen teams and agents/ }) as HTMLInputElement).checked,
  ).toBe(true);
  expect(screen.queryByRole("button", { name: "Save access" })).toBeNull();
  fireEvent.click(screen.getByRole("checkbox", { name: "Product Manager" }));
  fireEvent.click(screen.getByRole("button", { name: "Save access" }));
  await afterMicrotasks();
  expect(putEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "linear",
    {
      name: "linear",
      url: "https://mcp.linear.app/mcp",
      access: { seats: ["backend-engineer", "pm"] },
    },
    '"r1"',
    "Changed who reaches linear",
    expect.anything(),
  );
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("access", mcpData("member"));
  const memberTabs = screen.getByRole("tablist", { name: "Linear sections" });
  expect(within(memberTabs).queryByRole("tab", { name: /Access/ })).toBeNull();
  expect(screen.queryByText("What each agent can call")).toBeNull();
});

test("a grant the engine refuses says why and saves nothing", async () => {
  detail = LINEAR_DETAIL;
  entry = { name: "linear", url: "https://mcp.linear.app/mcp" };
  vi.mocked(putEntity).mockResolvedValue({
    kind: "problems",
    problems: [
      {
        path: "roles.0.mcp_env.linear",
        segments: ["roles", 0, "mcp_env", "linear"],
        kind: "invalid",
        message: "Backend Engineer's values for linear name a server it would no longer reach.",
      },
    ],
  } as never);
  mountAt("access", mcpData("admin"));
  fireEvent.click(await screen.findByRole("radio", { name: /Chosen teams and agents/ }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Product Manager" }));
  fireEvent.click(screen.getByRole("button", { name: "Save access" }));
  await afterMicrotasks();
  expect(screen.getByText("Nothing was saved")).toBeDefined();
  expect(screen.getByText(/name a server it would no longer reach/)).toBeDefined();
});

// A HELD CALL IS ANSWERED ON ITS SERVER'S OVERVIEW, as the Inbox answers it,
// and the tab says one waits. Mutation: answer through the run's own answer,
// or drop the marker, and this is red.
test("a call held for approval is answered on the server's overview, and its tab says it waits", async () => {
  detail = LINEAR_DETAIL;
  const answered: McpApprovalAnswer = {
    id: LINEAR_APPROVAL.id,
    server: "linear",
    tool: "linear_delete_issue",
    seat: "backend-engineer",
    turn_id: "turn-1",
    launch_id: "launch-1",
    answer: "once",
    outcome: "pending",
    answer_id: "a-1",
    revision: null,
  };
  vi.mocked(answerHeldCall).mockResolvedValue(answered);
  const reload = vi.fn();
  mountAt("", mcpData("admin", reload, { approvals: [LINEAR_APPROVAL] }));
  expect(screen.getByRole("tab", { name: /1 waiting for approval/ })).toBeDefined();
  expect(screen.getByText("Waiting for your approval")).toBeDefined();
  expect(screen.getByText("linear_delete_issue")).toBeDefined();
  expect(screen.getByText("ENG-12")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
  await afterMicrotasks();
  expect(answerHeldCall).toHaveBeenCalledWith(LINEAR_APPROVAL.id, "once");
  expect(screen.getByText("Approval answered")).toBeDefined();
  expect(reload).toHaveBeenCalled();
  cleanup();

  mountAt(
    "activity",
    mcpData("admin", () => {}, { approvals: [LINEAR_APPROVAL] }),
  );
  expect(screen.getByText("Calls held for approval")).toBeDefined();
  expect(screen.getByText("Waiting")).toBeDefined();
});

// A CHANGED TOOL IS SAID ON THE TOOLS TAB: marked on its row and its tab, the
// changes counted with a Review that opens the dialog, and a filter of them.
test("a changed tool is marked on the tools tab, with a review of the changes", () => {
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_DETAIL.tools.map((t) =>
      t.raw === "delete_issue" ? { ...t, held: true, level: "approve" as const } : t,
    ),
    changes: LINEAR_CHANGES,
  };
  const onFlow = vi.fn();
  mountAt("tools", mcpData("admin"), onFlow);
  expect(screen.getByRole("tab", { name: /2 changed/ })).toBeDefined();
  expect(screen.getByText("2 tools changed since their levels were decided")).toBeDefined();
  expect(screen.getAllByText("Changed").length).toBeGreaterThan(1);
  fireEvent.click(screen.getByRole("button", { name: "Review" }));
  expect(onFlow).toHaveBeenCalledWith("drift");
});

// A TOOL A TEAM'S LEAD HOLDS BACK is marked on the Tools tab, naming who it is
// held back for and how, from the engine's resolved limits. Mutation: read the
// limits off the entry rather than `reaches[].limited`, and this is red.
test("a tool a team's lead holds back is marked, with who it is held back for", () => {
  detail = {
    ...LINEAR_DETAIL,
    reaches: [
      {
        seat: "backend-engineer",
        reason: "every_agent",
        via: null,
        limited: {
          off: false,
          off_by: null,
          tools: [{ tool: "list_issues", level: "approve", by: null }],
        },
      },
      { seat: "pm", reason: "every_agent", via: null, limited: null },
    ],
  };
  mountAt("tools", mcpData("admin"));
  const row = screen.getByText("linear_list_issues").closest("tr, [role='row']") as HTMLElement;
  expect(within(row).getByText("Held back")).toBeDefined();
  expect(screen.queryAllByText("Held back")).toHaveLength(1);
});

// NOT OFFERED IS A TABLE OF ITS OWN: the tool and why it is not offered each
// under a head that names it, in tool order, which the Tool head says.
test("the tools not offered name each tool and its reason in columns of their own, by tool", () => {
  detail = LINEAR_DETAIL;
  const data = mcpData("admin");
  const linear = data.server("linear")!;
  const refused = [
    {
      tool: "z_tool",
      reason: "characters" as const,
      message: "z_tool holds a character a tool name cannot",
    },
    {
      tool: "a_tool",
      reason: "characters" as const,
      message: "a_tool holds a character a tool name cannot",
    },
  ];
  mountAt("tools", {
    ...data,
    server: (name: string) =>
      name === "linear"
        ? { ...linear, refused_tools: refused, refused_count: 2 }
        : data.server(name),
  });
  const card = screen.getByText("Not offered").closest(".crewlet-card") as HTMLElement;
  const grid = within(card).getByRole("table");
  const heads = within(grid).getAllByRole("columnheader");
  expect(heads.map((h) => h.textContent)).toEqual(["Tool", "Why it is not offered"]);
  expect(heads[0]!.getAttribute("aria-sort")).toBe("ascending");
  const rows = within(grid).getAllByRole("row").slice(1);
  expect(rows.map((r) => within(r).getAllByRole("cell")[0]!.textContent)).toEqual([
    "a_tool",
    "z_tool",
  ]);
  expect(within(rows[0]!).getAllByRole("cell")[1]!.textContent).toBe(
    "a_tool holds a character a tool name cannot",
  );
});

// TIMES SHARE THE RIGHT EDGE, as every figure does, and a held call's list
// says its order on the Asked head: newest first.
test("the activity's times sit on the right, and the held calls are newest first by Asked", () => {
  detail = LINEAR_DETAIL;
  const older = { ...LINEAR_APPROVAL, id: "older", at: "2026-01-01T00:00:00Z", tool: "linear_old" };
  mountAt(
    "activity",
    mcpData("admin", () => {}, { approvals: [older, LINEAR_APPROVAL] }),
  );
  const gridOf = (title: string) =>
    within(screen.getByText(title).closest(".crewlet-card") as HTMLElement).getByRole("table");
  const when = within(gridOf("Recent calls")).getByRole("columnheader", { name: /When/ });
  expect(when.className).toMatch(/--right/);
  const held = gridOf("Calls held for approval");
  const asked = within(held).getByRole("columnheader", { name: /Asked/ });
  expect(asked.className).toMatch(/--right/);
  expect(asked.getAttribute("aria-sort")).toBe("descending");
  const tools = within(held)
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[2]!.textContent);
  expect(tools[0]).toContain("delete_issue");
});

// A TOOL'S RISK CHIPS WRAP IN THEIR CELL: the Risk column is content-sized and
// capped, so a pair wider than the cap ("Assumed destructive", "Open world")
// stacks rather than being clipped at the cell's edge.
test("the risk chips wrap inside their cell rather than run past it", () => {
  const rule =
    sheetRules().find((r) => r.file === "mcp.css" && r.selector === ".mcp-risk")?.body ?? "";
  expect(rule).toMatch(/flex-wrap: wrap;/);
  expect(rule).toMatch(/max-inline-size: 100%;/);
});

// A REASON IS PROSE that quotes a tool's whole name, which has no place to
// break: its column wraps anywhere, so a long name never runs past the cell.
test("the reason a tool is not offered wraps, a long quoted name included", () => {
  detail = LINEAR_DETAIL;
  const data = mcpData("admin");
  const linear = data.server("linear")!;
  const refused = [
    {
      tool: "a_very_long_tool_name_with_no_break",
      reason: "too_long" as const,
      message: '"a_very_long_tool_name_with_no_break" is too long',
    },
  ];
  mountAt("tools", {
    ...data,
    server: (name: string) =>
      name === "linear"
        ? { ...linear, refused_tools: refused, refused_count: 1 }
        : data.server(name),
  });
  const card = screen.getByText("Not offered").closest(".crewlet-card") as HTMLElement;
  const reason = within(card).getAllByRole("cell")[1]!;
  expect(reason.className).toMatch(/crewlet-data-grid__cell--wrap/);
});
