// @vitest-environment node
/** A landing's gate in words: the rows' facts said as they stand, never a guess. */
import { expect, test } from "vitest";
import type { CodeChangeLanding, CodeCheckRollup } from "~/protocol/index.ts";
import { checksWords, gateWords, type GatePart } from "./Provenance.tsx";

const said = (parts: readonly GatePart[]) =>
  parts
    .map((p) => (typeof p === "string" ? p : "commit" in p ? p.commit.slice(0, 7) : p.who))
    .join("");

const rollup = (extra: Partial<CodeCheckRollup>): CodeCheckRollup => ({
  state: "passed",
  counts: {},
  required: 0,
  required_passed: 0,
  ...extra,
});

// Mutation: drop a branch of the rollup's states, and its words fall to the default.
test.each([
  [rollup({ required: 2, required_passed: 2 }), "2 of 2 required checks passed"],
  [rollup({ counts: { passed: 3 } }), "3 checks passed"],
  [rollup({ counts: { passed: 1 }, more: true }), "1 check passed, of the checks read"],
  [
    rollup({
      state: "failing",
      failing: [
        { name: "unit", state: "failed", required: true },
        { name: "e2e", state: "failed" },
      ],
      failing_more: 2,
    }),
    "unit, e2e and 2 more failed",
  ],
  [rollup({ state: "failing", counts: { failed: 2 } }), "2 checks failed"],
  [rollup({ state: "running" }), "its checks were still running"],
  [rollup({ state: "cancelled" }), "its checks were cancelled"],
] as const)("checks read %#", (checks, words) => {
  expect(checksWords(checks, false)).toBe(words);
});

const landing = (extra: Partial<CodeChangeLanding>): CodeChangeLanding => ({
  repo: "web",
  ref: "web!41",
  title: "t",
  state: "merged",
  merged_via: "direct",
  merged_by: "cto",
  merged_by_kind: "agent",
  ...extra,
});

// Mutation: list approvals without "and more", or name a person by the chart.
test("approvals are listed, a bounded list ends in and more, and no merger is a person", () => {
  const three = [
    { handle: "a", kind: "agent", revision: 1 },
    { handle: "b", kind: "agent", revision: 1 },
    { handle: "jane", kind: "person", revision: 1 },
  ];
  expect(said(gateWords(landing({ gate: { approvals: three } })))).toBe(
    "cto merged it through its gate: approved by a, b and jane; no check ran on what it merged.",
  );
  expect(said(gateWords(landing({ gate: { approvals: three, approvals_more: true } })))).toBe(
    "cto merged it through its gate: approved by a, b, jane and more; no check ran on what it merged.",
  );
  const parts = gateWords(landing({ merged_by: undefined, gate: {} }));
  expect(parts[0]).toBe("A person");
  const person = gateWords(landing({ merged_by: "jane", merged_by_kind: "person" }));
  expect(person[0]).toEqual({ who: "jane", person: true });
});

// A QUEUE LANDING WITHOUT THE TRAIN'S CHECKS says the checks it has, naming no commit.
test("a queue landing with no checks kept names no commit", () => {
  const parts = gateWords(landing({ merged_via: "queue", gate: { gone: true } }));
  expect(parts.some((p) => typeof p !== "string" && "commit" in p)).toBe(false);
  expect(said(parts)).toBe(
    "cto added it to the merge queue, and it landed through its gate: with no approval; its checks are no longer kept.",
  );
});
