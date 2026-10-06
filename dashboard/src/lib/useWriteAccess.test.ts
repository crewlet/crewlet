// @vitest-environment node

/**
 * Which reason a control is disabled with, in the order a person clears them.
 */

import { describe, expect, test } from "vitest";
import {
  CONFIG_WRITE_REASONS,
  WRITE_REASONS,
  configGuardedReason,
  configWriteAccess,
  linkOf,
  orgWriteAccess,
  writeAccess,
} from "./useWriteAccess.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { leadScope, NO_SCOPE } from "./leadScope.ts";
import type { ViewerState } from "./viewer.ts";

const BOUND: ViewerState = {
  login: "jane.founder",
  grants: ["work:write", "knowledge:write"],
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

/** Signed in, and the directory binds them to no seat. */
const UNBOUND: ViewerState = {
  ...BOUND,
  handle: "",
  owner: "jane.founder",
  name: "",
  kind: "",
  unbound: true,
};

test.each([
  ["refused by an engine that knows them, whoever is reading", BOUND, "refused", "refused"],
  ["offline, whoever is reading", BOUND, "offline", "offline"],
  ["nobody has said who this is", { ...BOUND, loading: true }, "open", "loading"],
  [
    "nobody is signed in",
    { ...BOUND, login: "", handle: "", owner: "", anonymous: true, acts: [] },
    "open",
    "anonymous",
  ],
  [
    "a change the engine does not make for this person",
    { ...BOUND, acts: [] },
    "open",
    "not_served",
  ],
] as const)("%s", (_name, viewer, link, block) => {
  const access = writeAccess("set_pins", viewer, link);
  expect(access).toEqual({ can: false, block, reason: WRITE_REASONS[block] });
});

// A QUEUED WRITE IS ONE THE PERSON WALKED AWAY FROM believing it happened:
// offline outranks every other reason, including a reader who could act.
test("offline outranks everything, a bound person included", () => {
  expect(writeAccess("set_pins", BOUND, "offline")).toMatchObject({ can: false, block: "offline" });
});

// A BROWSER THE ENGINE REFUSES IS NOT OFFLINE. A session without state:read is
// never dialled, so its socket is down — and "reconnect to make changes" sent
// a person invited with no grants, online and refused, looking for a network
// fault. The refusal outranks offline, and the viewer that never answers such
// a session. Mutation: rank offline first and the first two go red.
test("a browser the engine refuses is told so, never that it is offline", () => {
  const refused = linkOf({ connected: false, accessRefused: "grant withdrawn: state:read" });
  expect(refused).toBe("refused");
  expect(writeAccess("set_pins", { ...BOUND, loading: true }, refused)).toEqual({
    can: false,
    block: "refused",
    reason: WRITE_REASONS.refused,
  });
  expect(configWriteAccess(BOUND, refused)).toMatchObject({ block: "refused" });
  // THE CONTROL: the same socket down with no refusal is the outage it is.
  expect(linkOf({ connected: false, accessRefused: null })).toBe("offline");
  expect(linkOf({ connected: true, accessRefused: null })).toBe("open");
});

test("a bound person the engine serves acts as their own seat", () => {
  expect(writeAccess("set_pins", BOUND, "open")).toEqual({
    can: true,
    as: "jane",
    acts: ["set_pins"],
  });
});

// AN UNBOUND PRINCIPAL ACTS TOO, under its own login (ADR-0024): what a
// binding adds is the seat a person acts as, never the right to act. Blocked
// here, a person the directory had not bound yet could change nothing the
// engine would have taken from them.
test("a person bound to no seat acts under their own login", () => {
  expect(writeAccess("set_pins", UNBOUND, "open")).toEqual({
    can: true,
    as: "jane.founder",
    acts: ["set_pins"],
  });
});

// THE ENGINE'S LIST, NEVER A GUESS: `viewer.acts` is empty for anybody who
// may not act, so a viewer answer that somehow carried a handle and no acts
// still acts for nobody.
test("a handle with an empty acts list acts for nobody", () => {
  expect(writeAccess("set_pins", { ...BOUND, acts: [] }, "open").can).toBe(false);
});

// A SCREEN SHOWING SOMEBODY ELSE'S RECORD HOLDS EVERY CHANGE ON IT, with its
// own sentence — but LAST: a reader who is offline or signed out is told the
// thing they can clear first, and a hold is a fact about the screen.
test("a hold is the screen's sentence, and it ranks after everything a person can clear", () => {
  const hold = "This is Rui Santos’s day.";
  expect(writeAccess("set_pins", BOUND, "open", hold)).toEqual({
    can: false,
    block: "held",
    reason: hold,
  });
  expect(writeAccess("set_pins", BOUND, "offline", hold)).toMatchObject({ block: "offline" });
  expect(
    writeAccess(
      "set_pins",
      { ...BOUND, login: "", handle: "", owner: "", anonymous: true, acts: [] },
      "open",
      hold,
    ),
  ).toMatchObject({ block: "anonymous" });
  // AND NO HOLD IS NO HOLD: the empty value of the context is `null`.
  expect(writeAccess("set_pins", BOUND, "open", null).can).toBe(true);
});

describe("changing the company's configuration", () => {
  const viewer = (over: Partial<ViewerState>): ViewerState => ({
    ...UNBOUND,
    login: "ops.lead",
    owner: "ops.lead",
    grants: ["config:read", "config:write"],
    ...over,
  });

  // THE GRANT, NOT A BINDING: `/config` is decided by `config:write`, so a
  // person bound to no seat who holds it may change a ceiling, and a bound
  // person without it may not.
  test("is the config:write grant, not the seat binding", () => {
    expect(configWriteAccess(viewer({ unbound: true }), "open")).toEqual({ can: true });
    expect(
      configWriteAccess(
        viewer({ grants: ["config:read"], handle: "jane", owner: "jane", unbound: false }),
        "open",
      ),
    ).toEqual({
      can: false,
      block: "no_grant",
      reason: CONFIG_WRITE_REASONS.no_grant,
    });
  });

  test("in the order a person clears them", () => {
    expect(configWriteAccess(viewer({}), "offline")).toMatchObject({ block: "offline" });
    expect(configWriteAccess(viewer({ loading: true }), "open")).toMatchObject({
      block: "loading",
    });
    expect(
      configWriteAccess(viewer({ anonymous: true, login: "", owner: "", grants: [] }), "open"),
    ).toMatchObject({ block: "anonymous" });
    expect(configWriteAccess(viewer({}), "open", "these are Rui's")).toEqual({
      can: false,
      block: "held",
      reason: "these are Rui's",
    });
  });
});

// THE COMPANY'S GRANT, OR A LEAD INSIDE THEIR SUBTREE: the engine admits a
// write by a person without config:write when everything it changes is inside
// a unit they lead, so a control about one of their seats or units is open to
// them, and one outside says which units they do lead rather than that a
// grant is missing.
describe("changing one part of the org chart", () => {
  const lead: ViewerState = {
    ...BOUND,
    login: "cto.person",
    handle: "cto",
    owner: "cto",
    grants: ["work:write"],
  };
  const scope = leadScope(CHART_ORG, "cto");

  test("a lead may change a seat or unit they lead, and nothing else", () => {
    expect(orgWriteAccess(lead, "open", scope, { seat: "swe" })).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, { unit: "core" })).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, "anywhere")).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, { seat: "pm" })).toEqual({
      can: false,
      block: "outside_scope",
      reason: "You lead Core, and this is outside it: changing it takes the config:write grant.",
    });
    expect(
      orgWriteAccess(lead, "open", leadScope(CHART_ORG, "pm"), { unit: "core" }),
    ).toMatchObject({
      reason: expect.stringContaining("You lead Management and Developer Relations"),
    });
  });

  test("a person who leads nothing is told about the grant, and every earlier reason still ranks first", () => {
    expect(orgWriteAccess(lead, "open", NO_SCOPE, { seat: "swe" })).toMatchObject({
      block: "no_grant",
    });
    expect(orgWriteAccess(lead, "offline", scope, { seat: "swe" })).toMatchObject({
      block: "offline",
    });
    expect(orgWriteAccess(lead, "open", scope, { seat: "swe" }, "these are Rui's")).toMatchObject({
      block: "held",
    });
    // The grant reaches everything, a lead's scope or not.
    const admin = { ...lead, grants: ["config:read", "config:write"] };
    expect(orgWriteAccess(admin, "open", scope, { seat: "pm" })).toEqual({ can: true });
  });
});

// A REFUSED /config WRITE IS SAID BY ITS CODE. The browser holds no token, so
// "the engine did not take this token" sent a person looking for one; and a
// 403 is not always the grant — a step-up they declined, or a write from
// another site, sent to an administrator for a grant would never be cured.
describe("a configuration write refused on authority", () => {
  test("is the grant the gate names when the engine refused on the grant", () => {
    expect(configGuardedReason("unauthorized")).toBe(CONFIG_WRITE_REASONS.no_grant);
    // AN ANSWER WITH NO CODE is a refusal on authority all the same.
    expect(configGuardedReason("")).toBe(CONFIG_WRITE_REASONS.no_grant);
  });

  test("is the guard's own refusal when the engine named another", () => {
    expect(configGuardedReason("step_up_required")).toBe(ACT_ERRORS.step_up_required);
    expect(configGuardedReason("csrf_origin")).toBe(ACT_ERRORS.csrf_origin);
    expect(configGuardedReason("invalid_token")).toBe(ACT_ERRORS.invalid_token);
  });

  test("never speaks of a token", () => {
    for (const code of ["unauthorized", "", "step_up_required", "csrf_origin", "invalid_token"]) {
      expect(configGuardedReason(code)).not.toMatch(/token/i);
    }
  });
});
