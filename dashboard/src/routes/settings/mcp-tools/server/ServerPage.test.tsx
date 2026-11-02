/**
 * One server's page, tab by tab, in both readings: each tab draws what the
 * engine answered and what only an administrator reads stays theirs.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type { RestResult } from "~/lib/useRest.ts";
import { TestClient } from "~/test/client.tsx";
import type { ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import { LINEAR_DETAIL, LINEAR_MEMBER_DETAIL, MCP_NOW, mcpData } from "../fixtures.ts";
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
}));

function mountAt(tab: string, data: McpData) {
  location.hash = `#/settings/mcp-tools/servers/linear${tab ? `?tab=${tab}` : ""}`;
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <ServerPage server={data.server("linear")!} data={data} onFlow={() => {}} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
}

beforeEach(() => {
  vi.useFakeTimers({ now: MCP_NOW, toFake: ["Date"] });
  entry = null;
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
  const instances = document.querySelector(".mcp-instances") as HTMLElement;
  expect(within(instances).getAllByText("The company")).toHaveLength(2);
  expect(within(instances).getByText("node-b")).toBeDefined();
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

// THE CONNECTION, READ ONLY: an administrator reads the address the engine
// answers (its credentials withheld) and every header by name, never a
// value; a member reads how it connects and signs in, and no name.
test("the connection names the address and the headers to an administrator alone, never a value", async () => {
  detail = LINEAR_DETAIL;
  entry = {
    name: "linear",
    url: "https://mcp.linear.app/mcp",
    headers: { Authorization: "${LINEAR_TOKEN}", "X-Team": "__redacted__" },
  };
  mountAt("connection", mcpData("admin"));
  expect(screen.getByText("https://mcp.linear.app/mcp")).toBeDefined();
  expect(await screen.findByText("Authorization")).toBeDefined();
  expect(screen.getByText("Reads ${LINEAR_TOKEN}")).toBeDefined();
  expect(screen.getByText("X-Team")).toBeDefined();
  expect(screen.getByText("Set in the configuration")).toBeDefined();
  expect(document.body.textContent).not.toContain("__redacted__");
  expect(screen.getByText("120 s (the default)")).toBeDefined();
  cleanup();

  detail = LINEAR_MEMBER_DETAIL;
  mountAt("connection", mcpData("member"));
  expect(screen.queryByText("https://mcp.linear.app/mcp")).toBeNull();
  expect(screen.queryByText("Authorization")).toBeNull();
  expect(screen.getByText("Values the configuration sets")).toBeDefined();
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
  // A TOOL THAT DECLARES NOTHING reads destructive, marked as undeclared.
  expect(within(rowOf("linear_delete_issue")).getByText("Destructive*")).toBeDefined();
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
