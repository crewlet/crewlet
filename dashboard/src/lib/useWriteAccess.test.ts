// @vitest-environment node

/**
 * Which reason a control is disabled with, in the order a person clears them.
 */

import { describe, expect, test } from "vitest";
import {
  CONFIG_WRITE_REASONS,
  WRITE_REASONS,
  configWriteAccess,
  writeAccess,
} from "./useWriteAccess.ts";
import type { ViewerState } from "./viewer.ts";

const BOUND: ViewerState = {
  login: "jane.founder",
  grants: ["state:read", "work:write"],
  operatesFleet: false,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["set_pins"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

/** A principal the directory binds to no seat, which the engine still serves. */
const UNBOUND: ViewerState = {
  ...BOUND,
  login: "ops.lead",
  handle: "",
  owner: "ops.lead",
  name: "",
  kind: "",
  unbound: true,
};

const NOBODY: ViewerState = {
  ...BOUND,
  login: "",
  grants: [],
  handle: "",
  owner: "",
  name: "",
  kind: "",
  acts: [],
  anonymous: true,
};

test.each([
  ["offline, whoever is reading", BOUND, false, "offline"],
  ["nobody has said who this is", { ...BOUND, loading: true }, true, "loading"],
  ["nobody signed in", NOBODY, true, "anonymous"],
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

// UNBOUND IS NOT A REASON: the engine makes the change and records it under
// the caller's own login (ADR-0024), so a gate that held them would lock a
// person out of a change the engine makes for them.
test("a caller bound to no seat acts under their own login", () => {
  expect(writeAccess("set_pins", UNBOUND, true)).toEqual({
    can: true,
    as: "ops.lead",
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
// own sentence — but LAST: a reader who is offline, anonymous or not served
// is told the thing they can clear first, and a hold is a fact about the
// screen.
test("a hold is the screen's sentence, and it ranks after everything a person can clear", () => {
  const hold = "This is Rui Santos’s day.";
  expect(writeAccess("set_pins", BOUND, true, hold)).toEqual({
    can: false,
    block: "held",
    reason: hold,
  });
  expect(writeAccess("set_pins", BOUND, false, hold)).toMatchObject({ block: "offline" });
  expect(writeAccess("set_pins", NOBODY, true, hold)).toMatchObject({ block: "anonymous" });
  expect(writeAccess("set_pins", { ...BOUND, acts: [] }, true, hold)).toMatchObject({
    block: "not_served",
  });
  // AND NO HOLD IS NO HOLD: the empty value of the context is `null`.
  expect(writeAccess("set_pins", BOUND, true, null).can).toBe(true);
});

// NO SENTENCE NAMES A CREDENTIAL TO PASTE: the dashboard holds no token, and a
// remedy pointing at one sends a person looking for a setting that is gone.
test("no reason tells a person to set a token", () => {
  for (const reason of [...Object.values(WRITE_REASONS), ...Object.values(CONFIG_WRITE_REASONS)]) {
    expect(reason).not.toMatch(/token/i);
  }
});

describe("changing the company's configuration", () => {
  const viewer = (over: Partial<ViewerState>): ViewerState => ({
    ...UNBOUND,
    grants: ["config:read", "config:write"],
    ...over,
  });

  // THE GRANT, NOT A BINDING: `/config` and a `/chart` runtime write are
  // decided by `config:write`, so a caller bound to no seat who holds it may
  // change a ceiling — and a bound person without it may not.
  test("is the config:write grant, not the seat binding", () => {
    expect(configWriteAccess(viewer({ unbound: true }), true)).toEqual({ can: true });
    expect(
      configWriteAccess(
        viewer({ grants: ["state:read"], handle: "jane", owner: "jane", unbound: false }),
        true,
      ),
    ).toEqual({
      can: false,
      block: "no_grant",
      reason: CONFIG_WRITE_REASONS.no_grant,
    });
    // `config:read` alone reads the company document and changes nothing.
    expect(configWriteAccess(viewer({ grants: ["config:read"] }), true)).toMatchObject({
      block: "no_grant",
    });
  });

  test("in the order a person clears them", () => {
    expect(configWriteAccess(viewer({}), false)).toMatchObject({ block: "offline" });
    expect(configWriteAccess(viewer({ loading: true }), true)).toMatchObject({ block: "loading" });
    expect(configWriteAccess(viewer({ anonymous: true, grants: [] }), true)).toMatchObject({
      block: "anonymous",
    });
    expect(configWriteAccess(viewer({}), true, "these are Rui's")).toEqual({
      can: false,
      block: "held",
      reason: "these are Rui's",
    });
  });
});
