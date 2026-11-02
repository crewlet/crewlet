/**
 * The flows a server's action opens: what each sends, and what each holds
 * back until the engine has said what it would meet.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { removeServer, restartServer, setEnabled } from "../api.ts";
import { mcpData } from "../fixtures.ts";
import { useFlow } from "../parts.tsx";
import { Flows } from "./Flows.tsx";

vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
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

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

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
