/**
 * One server's page, tab by tab, in both readings: each tab draws what the
 * engine answered and what only an administrator reads stays theirs.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type { RestResult } from "~/lib/useRest.ts";
import { TestClient } from "~/test/client.tsx";
import type { ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import {
  LINEAR_DETAIL,
  LINEAR_MEMBER_DETAIL,
  LINEAR_OAUTH,
  MCP_NOW,
  SERVICE_OAUTH,
  mcpData,
  withOAuth,
} from "../fixtures.ts";
import type { OnFlow } from "../parts.tsx";
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

function mountAt(tab: string, data: McpData, onFlow: OnFlow = () => {}) {
  location.hash = `#/settings/mcp-tools/servers/linear${tab ? `?tab=${tab}` : ""}`;
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <ServerPage server={data.server("linear")!} data={data} onFlow={onFlow} />
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
