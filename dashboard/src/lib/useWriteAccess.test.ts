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

// A SCREEN SHOWING SOMEBODY ELSE'S RECORD HOLDS EVERY CHANGE ON IT, with its
// own sentence — but LAST: a reader who is offline, anonymous or unbound is
// told the thing they can clear first, and a hold is a fact about the screen.
test("a hold is the screen's sentence, and it ranks after everything a person can clear", () => {
  const hold = "This is Rui Santos’s day.";
  expect(writeAccess("set_pins", BOUND, true, hold)).toEqual({
    can: false,
    block: "held",
    reason: hold,
  });
  expect(writeAccess("set_pins", BOUND, false, hold)).toMatchObject({ block: "offline" });
  expect(
    writeAccess("set_pins", { ...BOUND, handle: "", unbound: true, acts: [] }, true, hold),
  ).toMatchObject({ block: "unbound" });
  // AND NO HOLD IS NO HOLD: the empty value of the context is `null`.
  expect(writeAccess("set_pins", BOUND, true, null).can).toBe(true);
});
