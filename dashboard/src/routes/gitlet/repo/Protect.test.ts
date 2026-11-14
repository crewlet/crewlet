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
import { protectArgs, type HeldRule } from "./Protect.tsx";

const FORM = {
  directPush: "persons",
  allowForce: false,
  allowDelete: false,
  gated: false,
  mergeBy: "writers",
  approvals: "none",
  checks: "",
  ownersApprove: false,
  deleteSource: "seats",
};

const HELD: HeldRule = {
  rule: { pattern: "refs/heads/main", direct_push: "persons" },
  gate: {
    merge_by: "writers",
    approvals: "one",
    checks: ["unit", "lint"],
    delete_source: "seats",
  },
} as unknown as HeldRule;

const asHeld = { ...FORM, gated: true, approvals: "one", checks: "unit, lint" };

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
  expect(protectArgs(HELD, { ...asHeld, gated: false, checks: "" })).toEqual({
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
  expect(protectArgs(HELD, { ...asHeld, approvals: "none", ownersApprove: true })).toEqual({
    approvals: "none",
    owners_approve: true,
  });
});
