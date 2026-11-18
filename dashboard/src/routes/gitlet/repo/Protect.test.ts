/**
 * What a protect call sends: only what the person moved from the rule the
 * page read, since the engine keeps every field a call leaves out.
 *
 * The invariants worth breaking a build over: an untouched rule sends
 * nothing; a new rule sends only what differs from the engine's own new rule;
 * a gate turned off is `remove_gate` and nothing of it; a gate newly made
 * names who merges and its approvals; and the required checks move item by
 * item, so a check somebody else added meanwhile is not dropped.
 */

import { expect, test } from "vitest";
import { landingHeld, protectArgs, type HeldRule, type ProtectForm } from "./Protect.tsx";

const FORM: ProtectForm = {
  directPush: "persons",
  allowForce: false,
  allowDelete: false,
  gated: false,
  mergeBy: "writers",
  approvals: "none",
  approvalsUnless: [],
  humanPaths: [],
  checks: "",
  depth: "4",
  batch: "2",
  ownersApprove: false,
  deleteSource: "seats",
  agentsEditChecks: false,
};

const HELD: HeldRule = {
  rule: { pattern: "refs/heads/main", direct_push: "persons" },
  gate: {
    merge_by: "writers",
    approvals: "one",
    approvals_unless: ["whitespace_only"],
    human_paths: [{ glob: "migrations/**" }, { glob: "*.sql", unless: ["comment_only"] }],
    checks: ["unit", "lint"],
    landing: { depth: 2, batch: 1 },
    delete_source: "seats",
  },
} as unknown as HeldRule;

const asHeld: ProtectForm = {
  ...FORM,
  gated: true,
  approvals: "one",
  approvalsUnless: ["whitespace_only"],
  humanPaths: [
    { glob: "migrations/**", unless: [] },
    { glob: "*.sql", unless: ["comment_only"] },
  ],
  checks: "unit, lint",
  depth: "2",
  batch: "1",
};

test("a rule as it was read sends nothing", () => {
  expect(protectArgs(HELD, asHeld)).toEqual({});
});

test("a new rule sends only what differs from the engine's own new rule", () => {
  expect(protectArgs(undefined, FORM)).toEqual({});
  expect(protectArgs(undefined, { ...FORM, directPush: "none", allowDelete: true })).toEqual({
    direct_push: "none",
    allow_delete: true,
  });
});

test("a gate newly made names who merges and its approvals, even at the defaults", () => {
  expect(protectArgs(undefined, { ...FORM, gated: true })).toEqual({
    merge_by: "writers",
    approvals: "none",
  });
});

test("a gate turned off is remove_gate and nothing of it", () => {
  expect(protectArgs(HELD, { ...asHeld, gated: false, checks: "", humanPaths: [] })).toEqual({
    remove_gate: true,
  });
  // NO GATE TO REMOVE: nothing is sent for it.
  expect(protectArgs({ rule: HELD.rule, gate: undefined }, FORM)).toEqual({});
});

// THE CHECKS MOVE ITEM BY ITEM: sent whole, a check another person added
// between the read and the save would be dropped. Mutation: send `checks` as
// the list typed and this fails.
test("the required checks change by what was added and what was removed", () => {
  expect(protectArgs(HELD, { ...asHeld, checks: "unit e2e, e2e" })).toEqual({
    checks: { add: ["e2e"], remove: ["lint"] },
  });
  expect(protectArgs(HELD, { ...asHeld, approvals: "one", ownersApprove: true })).toEqual({
    owners_approve: true,
  });
});

// THE EXEMPTING CLASSES MOVE ITEM BY ITEM, in the engine's order. Mutation:
// send `approvals_unless` as the list ticked, and a class another person
// ticked meanwhile is dropped; this fails.
test("the classes that exempt a change from approval change by what moved", () => {
  expect(protectArgs(HELD, { ...asHeld, approvalsUnless: ["comment_only"] })).toEqual({
    approvals_unless: { add: ["comment_only"], remove: ["whitespace_only"] },
  });
});

// AN EXEMPTION MEANS SOMETHING ONLY BESIDE AN APPROVAL ASKED: under No approval
// none is sent and any the gate kept is removed, though the form keeps the
// ticks for a return to One approval. Mutation: send the ticks whatever
// Approvals holds, and this fails.
test("the exempting classes are sent only where the gate asks an approval", () => {
  expect(
    protectArgs(undefined, { ...FORM, gated: true, approvalsUnless: ["comment_only"] }),
  ).toEqual({ merge_by: "writers", approvals: "none" });
  expect(protectArgs(HELD, { ...asHeld, approvals: "none" })).toEqual({
    approvals: "none",
    approvals_unless: { remove: ["whitespace_only"] },
  });
  expect(
    protectArgs(undefined, {
      ...FORM,
      gated: true,
      approvals: "one",
      approvalsUnless: ["comment_only"],
    }),
  ).toEqual({ merge_by: "writers", approvals: "one", approvals_unless: { add: ["comment_only"] } });
});

// THE HUMAN PATHS MOVE BY GLOB: a new glob put, a dropped one removed, a kept
// one put again only where its classes moved. Mutation: send the paths whole,
// or put an unchanged glob, and this fails.
test("the paths a person approves change by glob", () => {
  expect(
    protectArgs(HELD, {
      ...asHeld,
      humanPaths: [
        { glob: "*.sql", unless: [] },
        { glob: "infra/**", unless: ["whitespace_only"] },
        { glob: "infra/**", unless: [] },
      ],
    }),
  ).toEqual({
    human_paths: {
      put: [{ glob: "*.sql" }, { glob: "infra/**", unless: ["whitespace_only"] }],
      remove: ["migrations/**"],
    },
  });
});

// THE LANDING A FIELD AT A TIME, against the gate's, or the engine's new
// gate's for a gate newly made. Mutation: send both fields on any move, or
// send the defaults for a new gate, and this fails.
test("the queue's depth and batch are sent a field at a time", () => {
  expect(protectArgs(HELD, { ...asHeld, depth: "3" })).toEqual({ landing: { depth: 3 } });
  expect(protectArgs(undefined, { ...FORM, gated: true })).not.toHaveProperty("landing");
  expect(protectArgs(undefined, { ...FORM, gated: true, batch: "5" })).toMatchObject({
    landing: { batch: 5 },
  });
});

// A SEAT APPROVING THE CHECKS' OWN CHANGE is sent where it moved, and only
// then. Mutation: drop the field, or send it unmoved, and this fails.
test("a seat approving a change to the checks is sent where it moved", () => {
  expect(protectArgs(HELD, { ...asHeld, agentsEditChecks: true })).toEqual({
    agents_edit_checks: true,
  });
});

// THE QUEUE'S BOUNDS ARE THE ENGINE'S, judged only for a gated rule.
// Mutation: admit 0, 9 or a fraction, or judge an ungated form, and this fails.
test("a gate's queue is held to whole numbers inside the engine's bounds", () => {
  expect(landingHeld({ gated: true, depth: "8", batch: "1" })).toBeUndefined();
  expect(landingHeld({ gated: true, depth: "9", batch: "1" })).toEqual({
    field: "depth",
    reason: "Changes tested at once is a whole number from 1 to 8.",
  });
  expect(landingHeld({ gated: true, depth: "2", batch: "0" })?.field).toBe("batch");
  expect(landingHeld({ gated: true, depth: "2", batch: "0" })?.reason).toMatch(/1 to 8/);
  expect(landingHeld({ gated: true, depth: "1.5", batch: "1" })?.reason).toMatch(/whole number/);
  expect(landingHeld({ gated: false, depth: "", batch: "" })).toBeUndefined();
});
