/**
 * A merge gate said in a line states every field a merge is held to.
 *
 * The invariant worth breaking a build over: two gates that hold a merge
 * differently never read alike. A field the line leaves out is one a reader
 * of the repository's Settings, or of Settings › Gitlet, cannot see, and a
 * rule exempting whitespace changes from approval read the same as one that
 * does not.
 */

import { expect, test } from "vitest";
import type { CodeMergeGate } from "~/protocol/index.ts";
import { gateLine } from "./protection.ts";

const GATE: CodeMergeGate = {
  merge_by: "writers",
  approvals: "one",
  checks: ["unit"],
  landing: { depth: 4, batch: 2 },
  delete_source: "seats",
};

// EACH FIELD THAT HOLDS A MERGE MOVES THE LINE. Mutation: drop the exempting
// classes, a human path's classes, the queue's depth or batch, or whether a
// seat approves a change to the checks from the line, and this fails.
test.each<[string, Partial<CodeMergeGate>, string]>([
  [
    "the classes that exempt a change",
    { approvals_unless: ["whitespace_only"] },
    "none for a change of only whitespace",
  ],
  [
    "a human path and the classes that exempt it",
    { human_paths: [{ glob: "migrations/**", unless: ["comment_only"] }] },
    "a person approves migrations/** (unless a change of only comments)",
  ],
  ["the queue's depth", { landing: { depth: 6, batch: 2 } }, "6 candidates at once"],
  ["the queue's batch", { landing: { depth: 4, batch: 3 } }, "3 changes each"],
  [
    "a seat approving the checks' own change",
    { agents_edit_checks: true },
    "a seat may approve a change to its checks",
  ],
])("a gate's line states %s", (_what, field, says) => {
  const line = gateLine({ ...GATE, ...field });
  expect(line).toContain(says);
  expect(line).not.toBe(gateLine(GATE));
});

// AN EXEMPTION FROM NO APPROVAL HOLDS NO MERGE, so its gate reads as its twin
// without one. Mutation: say the classes whatever the approvals, and this fails.
test("a gate asking no approval says no exemption from one", () => {
  const none: CodeMergeGate = { ...GATE, approvals: "none" };
  expect(gateLine({ ...none, approvals_unless: ["whitespace_only"] })).toBe(gateLine(none));
});

// MANY HUMAN PATHS ARE COUNTED, a few are named. Mutation: name every one of
// thirty-two globs in one line, and this fails.
test("a gate with many human paths counts them", () => {
  const paths = ["a/**", "b/**", "c/**", "d/**"].map((glob) => ({ glob }));
  expect(gateLine({ ...GATE, human_paths: paths })).toContain("a person approves 4 paths");
});
