/**
 * The built-in catalogue: the engine's groups as a rail, each tool's
 * capability as the engine read it, and the filters in the address.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { ADMIN_STATUS } from "../fixtures.ts";
import { argumentsOf } from "./arguments.ts";
import { BuiltinCatalogue } from "./Catalogue.tsx";

function mountAt(hash: string, onOpen = vi.fn()) {
  location.hash = hash;
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <BuiltinCatalogue groups={ADMIN_STATUS.builtin} onOpen={onOpen} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
  return onOpen;
}

afterEach(() => {
  cleanup();
  location.hash = "";
});

// THE ENGINE FILES EVERY TOOL: its groups are the rail, each with how many it
// holds, and a group chosen narrows the list to its own rows.
test("the rail is the engine's groups, and a group chosen is the address", () => {
  mountAt("#/settings/mcp-tools?view=builtin");
  const rail = screen.getByRole("navigation", { name: "Built-in tool groups" });
  expect(rail.textContent).toContain("All tools");
  expect(rail.textContent).toContain("Work items");
  expect(rail.textContent).toContain("Memory and skills");
  expect(screen.getByText("list_work_items")).toBeDefined();
  expect(screen.getByText("forget_note")).toBeDefined();
  fireEvent.click(screen.getByRole("link", { name: /Memory and skills/ }));
  expect(location.hash).toContain("group=memory");
  expect(screen.queryByText("list_work_items")).toBeNull();
  expect(screen.getByText("forget_note")).toBeDefined();
});

// WHAT A TOOL CAN DO IS THE ENGINE'S READING (`risk`), never the hints read
// again: a tool that declares nothing reads destructive, and the chips filter
// on the same reading.
test("each tool's capability is the engine's risk, and the chips filter on it", () => {
  mountAt("#/settings/mcp-tools?view=builtin&cap=read_only");
  expect(screen.getByText("list_work_items")).toBeDefined();
  expect(screen.queryByText("create_work_item")).toBeNull();
  expect(screen.queryByText("forget_note")).toBeNull();
  cleanup();
  mountAt("#/settings/mcp-tools?view=builtin&cap=destructive");
  expect(screen.getByText("forget_note")).toBeDefined();
  expect(screen.queryByText("list_work_items")).toBeNull();
});

// A TOOL'S ARGUMENTS are read off its schema as the schema states them:
// absent is not "none".
test("a tool's arguments are its schema's properties, and no schema is not none", () => {
  expect(argumentsOf(undefined)).toBeNull();
  expect(argumentsOf({ type: "object" })).toEqual([]);
  expect(
    argumentsOf({
      type: "object",
      properties: {
        project: { type: "string", description: "The project's key" },
        limit: { type: ["integer", "null"] },
        raw: true,
      },
      required: ["project"],
    }),
  ).toEqual([
    { name: "project", type: "string", required: true, description: "The project's key" },
    { name: "limit", type: "integer | null", required: false, description: "" },
    { name: "raw", type: "", required: false, description: "" },
  ]);
});
