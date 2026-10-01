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

// UNKNOWN IS NOT NOBODY: a per-seat server on a roster with no grant on it is
// unknown; a shared one reaches every agent seat by the engine's own rule.
test("a reach the roster cannot say is unknown, never nobody", () => {
  const old = seatsOf([{ name: "PM", handle: "pm" }]);
  expect(reachOf({ name: "github", shared: false }, old)).toEqual({
    text: "Unknown: this node's roster does not say",
    known: false,
  });
  expect(reachOf({ name: "docs", shared: true }, old)).toEqual({
    text: "Every agent seat",
    known: true,
  });
  const granted = seatsOf([
    { name: "Dev", handle: "dev", tool_sources: ["mcp:github"] },
    { name: "PM", handle: "pm", tool_sources: [] },
  ]);
  expect(reachOf({ name: "github", shared: false }, granted)).toEqual({
    text: "1 seat",
    known: true,
  });
  expect(reachOf({ name: "linear", shared: false }, granted).text).toBe("No seat");
});

// UP MEANS AN INSTANCE STARTED, which `running` and `partial` both are.
test("the servers line counts a server with any instance started", () => {
  expect(serversUpLine([])).toBe("0 of 0 servers running");
  expect(serversUpLine([{ started: 0 }])).toBe("0 of 1 server running");
  expect(serversUpLine([{ started: 2 }, { started: 1 }, { started: 0 }, { started: 0 }])).toBe(
    "2 of 4 servers running",
  );
});
