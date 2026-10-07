/**
 * The words `lib/mcpServers.ts` puts on the engine's answers: pure, so every
 * rule is pinned without a socket.
 */

import { expect, test } from "vitest";
import { NAME_EXAMPLES, nameExample, reachOf, serversUpLine } from "./mcpServers.ts";
import { indexOrg } from "./seats.ts";

const seatsOf = (roles: unknown[]) => indexOrg({ name: "Acme", roles } as never).seats;

// AN EXAMPLE IS SOMETHING A READER COPIES, so it is never a name the company
// already has — copying one walked straight into "already exists".
test("the name example is never a taken name", () => {
  expect(nameExample(new Set())).toBe(NAME_EXAMPLES[0]);
  expect(nameExample(new Set([NAME_EXAMPLES[0]]))).toBe(NAME_EXAMPLES[1]);
  const all = new Set<string>([...NAME_EXAMPLES, "notes-2"]);
  expect(all.has(nameExample(all))).toBe(false);
});

// A REACH IS THE ENGINE'S GRANT, counted off each agent seat's `tool_sources`.
test("a reach counts the agent seats the engine grants the server", () => {
  const granted = seatsOf([
    { name: "Dev", handle: "dev", tool_sources: ["mcp:github", "mcp:docs"] },
    { name: "PM", handle: "pm", tool_sources: ["mcp:docs"] },
  ]);
  expect(reachOf({ name: "github" }, granted)).toBe("1 seat");
  expect(reachOf({ name: "docs" }, granted)).toBe("Every agent seat");
  expect(reachOf({ name: "linear" }, granted)).toBe("No seat");
});

// UP MEANS AN INSTANCE STARTED, which `running` and `partial` both are.
test("the servers line counts a server with any instance started", () => {
  expect(serversUpLine([])).toBe("0 of 0 servers running");
  expect(serversUpLine([{ started: 0 }])).toBe("0 of 1 server running");
  expect(serversUpLine([{ started: 2 }, { started: 1 }, { started: 0 }, { started: 0 }])).toBe(
    "2 of 4 servers running",
  );
});
