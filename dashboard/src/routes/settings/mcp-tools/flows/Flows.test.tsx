/**
 * The flows a server's action opens: what each sends, and what each holds
 * back until the engine has said what it would meet.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import type { McpCheckAnswer, McpServerAnswer } from "~/contract/mcp.ts";
import { removeServer, restartServer, setEnabled, storeValue, testServer } from "../api.ts";
import { LINEAR_DETAIL, mcpData } from "../fixtures.ts";
import { FAILURE } from "../words.ts";
import { useFlow } from "../parts.tsx";
import { Flows } from "./Flows.tsx";

/** A test that stopped at Linear's handshake: the credential was refused. */
const REFUSED: McpCheckAnswer = {
  server: "linear",
  seat: "",
  draft: false,
  at: "2026-11-01T12:00:00Z",
  by: "jane.doe",
  node: "node-a",
  ok: false,
  nodes: [
    {
      node: "node-a",
      answered: true,
      reason: "",
      ok: false,
      steps: [
        { stage: "address", state: "passed", took_ms: 1, detail: "" },
        { stage: "resolve", state: "passed", took_ms: 4, detail: "" },
        { stage: "connect", state: "passed", took_ms: 20, detail: "" },
        { stage: "tls", state: "passed", took_ms: 30, detail: "" },
        { stage: "discover", state: "failed", took_ms: 90, detail: "the server answered 401" },
        { stage: "list", state: "not_reached", took_ms: 0, detail: "" },
      ],
      failure: { class: "credential_rejected", said: "the server answered 401" },
      server_info: null,
      tools: 0,
      refused: 0,
      took_ms: 145,
    },
  ],
  server_info: null,
  tools: [],
  warnings: [],
};

/** Linear's answer with one header it is sent, its value stored. */
const WITH_VALUE: McpServerAnswer = {
  ...LINEAR_DETAIL,
  values: [
    {
      key: "Authorization",
      as: "header",
      variable: "MCP_LINEAR_AUTHORIZATION",
      set: true,
      stored_at: "2026-11-01T11:00:00Z",
      stored_by: "jane.doe",
      optional: false,
      bound_by: null,
    },
  ],
};

vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  readServer: vi.fn(async () => WITH_VALUE),
  testServer: vi.fn(async () => ({ kind: "ok", status: 200, value: REFUSED })),
  storeValue: vi.fn(),
  restartServer: vi.fn(async () => ({
    kind: "ok",
    status: 202,
    value: { server: "linear", seat: "pm", at: "2026-11-01T12:00:00Z", node: "node-a" },
  })),
  setEnabled: vi.fn(async () => ({
    kind: "saved",
    revisionId: "r2",
    epoch: 3,
    warnings: [],
    derived: null,
  })),
  removeServer: vi.fn(async (_name: string, _removal: unknown, options: { dryRun: boolean }) =>
    options.dryRun
      ? {
          outcome: { kind: "valid", baseRevisionId: "r1", warnings: [], derived: null },
          check: {
            valid: true,
            base_revision_id: "r1",
            warnings: [],
            derived: null,
            dependants: {
              mcp_env: [
                {
                  kind: "seat",
                  key: "pm",
                  name: "Product Manager",
                  segments: ["roles", 1, "mcp_env", "linear"],
                },
              ],
              sandbox: [],
              mcp_limits: [
                {
                  kind: "unit",
                  key: "eng",
                  name: "Engineering",
                  segments: ["units", 0, "mcp_limits", "linear"],
                },
              ],
              secrets: ["LINEAR_TOKEN"],
              oauth: true,
            },
          },
          written: null,
        }
      : new Promise(() => {}),
  ),
}));
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getEntity: vi.fn(async () => ({
    kind: "entity",
    entity: { name: "linear" },
    etag: '"entry-r1"',
  })),
}));

function Host() {
  const flow = useFlow();
  return <Flows data={mcpData("admin")} name="linear" flow={flow} onRemoved={() => {}} />;
}

function mountAt(hash: string) {
  location.hash = hash;
  render(
    <TestClient>
      <ToastProvider>
        <LayerHost>
          <Router>
            <Host />
          </Router>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

/** Every stub's answer has landed, and what it drew (`test/settle.ts`). */
const settle = afterMicrotasks;

beforeEach(() => vi.clearAllMocks());

afterEach(() => {
  cleanup();
  location.hash = "";
});

// A REMOVAL IS CHECKED BEFORE IT CAN BE MADE: the dry run lists what still
// names the server and says its sign-in goes with it, Remove waits until the
// reader clears those in the same change, and the removal states the revision
// of the entry it read and the values ticked to go with it.
test("a removal waits on its dry run, asks to clear what names the server, and states its revision", async () => {
  mountAt("#/settings/mcp-tools?flow=remove&server=linear");
  await settle();
  expect(removeServer).toHaveBeenCalledWith(
    "linear",
    { clear: true, deleteSecrets: [] },
    expect.objectContaining({ etag: '"entry-r1"', dryRun: true }),
    expect.anything(),
  );
  expect(screen.getByText("Still named in 2 places")).toBeDefined();
  expect(screen.getByText(/Product Manager: its own/)).toBeDefined();
  expect(screen.getByText(/Team Engineering: its/)).toBeDefined();
  expect(screen.getByText(/its sign-in is revoked at the vendor/)).toBeDefined();
  const remove = screen.getByRole("button", { name: "Remove server" });
  expect(remove.hasAttribute("disabled") || remove.getAttribute("aria-disabled") === "true").toBe(
    true,
  );
  fireEvent.click(screen.getByRole("checkbox", { name: "Clear these in the same change" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove server" }));
  await settle();
  expect(removeServer).toHaveBeenLastCalledWith(
    "linear",
    { clear: true, deleteSecrets: ["LINEAR_TOKEN"] },
    expect.objectContaining({ etag: '"entry-r1"', dryRun: false }),
  );
});

// DISABLE IS ONE ENTITY WRITE that turns the server off, and nothing else.
test("disabling a server writes it off", async () => {
  mountAt("#/settings/mcp-tools?flow=disable&server=linear");
  fireEvent.click(screen.getByRole("button", { name: "Disable" }));
  await settle();
  expect(setEnabled).toHaveBeenCalledWith(
    "linear",
    false,
    "Disabled the MCP server linear",
    expect.anything(),
  );
});

// A RESTART NAMING AN AGENT restarts that agent's instance alone, as the
// alarm's fix asked.
test("a restart for one agent asks the engine for that agent's instance", async () => {
  mountAt("#/settings/mcp-tools?flow=restart&server=linear&seat=pm");
  expect(screen.getByText("Restart Linear for Product Manager?")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Restart" }));
  await settle();
  expect(restartServer).toHaveBeenCalledWith("linear", "pm");
});

// A TEST RUNS ONCE, ON OPEN, and says where it stopped in the words of the
// class the engine decided, with the stage list beside it. Mutation: key the
// test on the caller's callback, or word the failure on the page, and this
// is red.
test("a test runs once on open and names the class the engine decided", async () => {
  mountAt("#/settings/mcp-tools?flow=test&server=linear");
  await settle();
  await settle();
  expect(testServer).toHaveBeenCalledTimes(1);
  expect(testServer).toHaveBeenCalledWith("linear", "");
  expect(screen.getByText(FAILURE.credential_rejected.title)).toBeDefined();
  expect(screen.getByRole("list", { name: "Stages on node-a" })).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Test again" }));
  await settle();
  expect(testServer).toHaveBeenCalledTimes(2);
});

/** A value stored, as the values route answers it. */
const STORED = {
  kind: "ok" as const,
  status: 200,
  value: {
    server: "linear",
    key: "Authorization",
    seat: "",
    variable: "MCP_LINEAR_AUTHORIZATION",
    tested: true,
    check: null,
    pointed: false,
    reload: { revision_id: "r3", epoch: 4 },
    values: WITH_VALUE.values,
  },
};

// A VALUE IS WRITE-ONLY AND TESTED FIRST: a refused one says why on its
// field, with what the test found, and stores nothing; a value the server
// takes closes the dialog. Mutation: send the value untested, or drop the
// refusal, and this is red.
test("a replaced value is tested before it is stored, and a refused one says why on its field", async () => {
  vi.mocked(storeValue)
    .mockResolvedValueOnce({
      kind: "refused",
      status: 422,
      code: "value_test_failed",
      sentence: "MCP server linear did not work with this value on node-a. Nothing was stored.",
      body: { check: REFUSED, stage: "discover", class: "credential_rejected", node: "node-a" },
    })
    .mockResolvedValueOnce(STORED);
  mountAt("#/settings/mcp-tools?flow=value&server=linear&key=Authorization");
  await settle();
  expect(screen.getByRole("heading", { name: "Replace Authorization" })).toBeDefined();
  const field = screen.getByLabelText("New value") as HTMLInputElement;
  expect(field.type).toBe("password");
  fireEvent.change(field, { target: { value: "Bearer lin_not_real" } });
  fireEvent.click(screen.getByRole("button", { name: "Test and save" }));
  await settle();
  expect(storeValue).toHaveBeenCalledWith("linear", "Authorization", "Bearer lin_not_real", {
    test: true,
  });
  expect(screen.getByText(/did not work with this value/)).toBeDefined();
  expect(screen.getByText("What the test found")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Test and save" }));
  await settle();
  expect(storeValue).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole("heading", { name: "Replace Authorization" })).toBeNull();
});

// A VALUE NO NODE COULD TEST is stored untested only when the person says so.
test("a value no node answered for is stored untested only on request", async () => {
  vi.mocked(storeValue)
    .mockResolvedValueOnce({
      kind: "refused",
      status: 503,
      code: "unavailable",
      sentence: "No node running MCP server linear answered the test.",
      body: {},
    })
    .mockResolvedValueOnce(STORED);
  mountAt("#/settings/mcp-tools?flow=value&server=linear&key=Authorization");
  await settle();
  fireEvent.change(screen.getByLabelText("New value"), {
    target: { value: "Bearer lin_not_real" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Test and save" }));
  await settle();
  expect(screen.getByText("No node answered the test")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Save untested" }));
  await settle();
  expect(storeValue).toHaveBeenLastCalledWith("linear", "Authorization", "Bearer lin_not_real", {
    test: false,
  });
});
