// @vitest-environment node

/**
 * Which reason a control is disabled with, in the order a person clears them.
 */

import { expect, test } from "vitest";
import { WRITE_REASONS, writeAccess } from "./useWriteAccess.ts";
import type { ViewerState } from "./viewer.ts";

const BOUND: ViewerState = {
  operatorID: "founder",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  acts: ["set_pins"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

test.each([
  ["offline, whoever is reading", BOUND, false, "offline"],
  ["nobody has said who this is", { ...BOUND, loading: true }, true, "loading"],
  [
    "no token",
    { ...BOUND, handle: "", operatorID: "", anonymous: true, acts: [] },
    true,
    "anonymous",
  ],
  ["a token no seat binds", { ...BOUND, handle: "", unbound: true, acts: [] }, true, "unbound"],
  ["a change the engine does not make for this person", { ...BOUND, acts: [] }, true, "not_served"],
] as const)("%s", (_name, viewer, connected, block) => {
  const access = writeAccess("set_pins", viewer, connected);
  expect(access).toEqual({ can: false, block, reason: WRITE_REASONS[block] });
});

// A QUEUED WRITE IS ONE THE PERSON WALKED AWAY FROM believing it happened:
// offline outranks every other reason, including a reader who could act.
test("offline outranks everything, a bound person included", () => {
  expect(writeAccess("set_pins", BOUND, false)).toMatchObject({ can: false, block: "offline" });
});

test("a bound person the engine serves acts as their own seat", () => {
  expect(writeAccess("set_pins", BOUND, true)).toEqual({
    can: true,
    as: "jane",
    acts: ["set_pins"],
  });
});

// THE ENGINE'S LIST, NEVER A GUESS: `viewer.acts` is empty for anybody who
// may not act, so a viewer answer that somehow carried a handle and no acts
// still acts for nobody.
test("a handle with an empty acts list acts for nobody", () => {
  expect(writeAccess("set_pins", { ...BOUND, acts: [] }, true).can).toBe(false);
});
