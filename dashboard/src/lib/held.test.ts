/**
 * Which node holds a seat, as the seat's page and its peek both say it to a
 * reader without the operator-only fleet read.
 *
 * IT WAS THE AGENT INSTANCE ID, which exists only while a turn runs — so an
 * idle seat THIS node held read "not running on this node" on its own page,
 * beside a roster calling it idle here. What a reader means by "where does it
 * run" is which node holds the lease, and this node's health push says which
 * seats it holds.
 */

import { expect, test } from "vitest";

import { heldBy } from "./seats.ts";

const health = { status: "ok", node: "node-a", seats: ["agent-cto"] };

test("a seat this node holds says so, idle or not", () => {
  expect(
    heldBy("agent-cto", { id: "a", agent_id: "a", role: "CTO", activity: "idle" }, health),
  ).toBe("this node · node-a");
});

test("a seat held elsewhere is a peer's, and one nobody holds is said", () => {
  expect(heldBy("agent-pm", { id: "b", agent_id: "b", role: "PM", activity: "idle" }, health)).toBe(
    "another node",
  );
  expect(
    heldBy(
      "agent-pm",
      { id: "b", agent_id: "b", role: "PM", activity: "stopped", stopped_reason: "unplaced" },
      health,
    ),
  ).toBe("no node — not placed");
});

// UNKNOWN IS NOT "ELSEWHERE": with no push, or an older node that sends no
// list, this node has said nothing about what it holds.
test("a node that has not said what it holds claims nothing", () => {
  expect(heldBy("agent-cto", undefined, null)).toBe("not reported by this node");
  expect(heldBy("agent-cto", undefined, { status: "ok" })).toBe("not reported by this node");
});
