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
  mayChangeConfig,
  messageSeatAccess,
  orgWriteAccess,
  OWN_SEAT_MESSAGE,
  SERVICE_OFF_REASONS,
  writeAccess,
} from "./useWriteAccess.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { leadScope, NO_SCOPE } from "./leadScope.ts";
import type { ViewerState } from "./viewer.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";
import type { ServicesRun } from "./services.ts";

/** A company running every service built into Crewlet. */
const EVERY: ServicesRun = { tracker: true, knowledge: true };

const BOUND: ViewerState = {
  login: "jane.founder",
  grants: ["worklet", "doclet"],
  admin: false,
  worklet: true,
  doclet: true,
  chatlet: true,
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
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
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
  const access = writeAccess("set_pins", viewer, link, EVERY);
  expect(access).toEqual({ can: false, block, reason: WRITE_REASONS[block] });
});

// THE APP FIRST, WHERE THE READER LACKS IT: a change the engine does not offer
// a reader who holds no Worklet is said as the access to ask an admin for,
// while one it does offer them without it (answering an ask they were asked)
// is never held for it, and a change that is no app's still says it is not
// served. Mutation: drop the app's arm and the first reads `not_served`.
test("a change in an app the reader lacks names that app", () => {
  const noWorklet: ViewerState = { ...BOUND, grants: ["doclet"], worklet: false, acts: [] };
  expect(writeAccess("update_work_item", noWorklet, "open", EVERY)).toEqual({
    can: false,
    block: "no_access",
    reason: "This change needs Worklet access. An admin can give it to you.",
  });
  expect(
    writeAccess(
      "comment_on_work_item",
      { ...noWorklet, acts: ["comment_on_work_item"] },
      "open",
      EVERY,
    ),
  ).toMatchObject({ can: true });
  expect(writeAccess("pause_seat", noWorklet, "open", EVERY)).toMatchObject({
    block: "not_served",
  });
  expect(
    writeAccess(
      "write_page",
      { ...BOUND, grants: ["worklet"], doclet: false, acts: [] },
      "open",
      EVERY,
    ),
  ).toMatchObject({ block: "no_access", reason: expect.stringContaining("Doclet access") });
});

// A CHANGE THAT NEEDS AN APP AND LANDS IN NO LOG. `answer_knowledge` spends a
// model call and writes nothing a question reads, and it is either of the two
// apps it answers from: a reader holding neither is told either would do,
// where reading the app off the log the write lands in told them the engine
// does not make the change. The CONTROL is a reader holding one app, whom the
// app does not hold — what holds them is the engine's own list. Mutation: read
// the app off `domain` again and the first reads `not_served`.
test("a change several apps open names each of them to a reader holding none", () => {
  const none: ViewerState = {
    ...BOUND,
    grants: [],
    worklet: false,
    doclet: false,
    chatlet: false,
    acts: [],
  };
  expect(writeAccess("answer_knowledge", none, "open", EVERY)).toEqual({
    can: false,
    block: "no_access",
    reason: "This change needs Worklet or Doclet access. An admin can give it to you.",
  });
  expect(
    writeAccess("answer_knowledge", { ...none, grants: ["doclet"], doclet: true }, "open", EVERY),
  ).toMatchObject({ block: "not_served" });
});

// A QUEUED WRITE IS ONE THE PERSON WALKED AWAY FROM believing it happened:
// offline outranks every other reason, including a reader who could act.
test("offline outranks everything, a bound person included", () => {
  expect(writeAccess("set_pins", BOUND, "offline", EVERY)).toMatchObject({
    can: false,
    block: "offline",
  });
});

// A BROWSER THE ENGINE REFUSES IS NOT OFFLINE. A refused socket stops
// dialling, so it is down — and "reconnect to make changes" sent a person
// online and refused looking for a network fault. The refusal outranks
// offline, and the viewer that never answers such a session. Mutation: rank
// offline first and the first two go red.
test("a browser the engine refuses is told so, never that it is offline", () => {
  const refused = linkOf({
    connected: false,
    accessRefused: "your seat is no longer in the chart",
  });
  expect(refused).toBe("refused");
  expect(writeAccess("set_pins", { ...BOUND, loading: true }, refused, EVERY)).toEqual({
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
  expect(writeAccess("set_pins", BOUND, "open", EVERY)).toEqual({
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
  expect(writeAccess("set_pins", UNBOUND, "open", EVERY)).toEqual({
    can: true,
    as: "jane.founder",
    acts: ["set_pins"],
  });
});

// THE ENGINE'S LIST, NEVER A GUESS: `viewer.acts` is empty for anybody who
// may not act, so a viewer answer that somehow carried a handle and no acts
// still acts for nobody.
test("a handle with an empty acts list acts for nobody", () => {
  expect(writeAccess("set_pins", { ...BOUND, acts: [] }, "open", EVERY).can).toBe(false);
});

// MESSAGING YOUR OWN SEAT IS HELD, by the control's own rule and in one
// sentence wherever it is drawn — but LAST, like every hold: a reader who is
// offline is told that first, and a colleague's seat is free. An unbound
// reader has no seat of their own, so no seat is theirs. Mutation: compare
// against anything but the reader's bound seat and a row goes red.
test("messaging your own seat is held after everything a person can clear", () => {
  const can = writeAccess(
    "create_work_item",
    { ...BOUND, acts: ["create_work_item"] },
    "open",
    EVERY,
  );
  expect(messageSeatAccess(can, BOUND, "jane")).toEqual({
    can: false,
    block: "held",
    reason: OWN_SEAT_MESSAGE,
  });
  expect(messageSeatAccess(can, BOUND, "swe")).toBe(can);
  expect(messageSeatAccess(can, UNBOUND, "")).toBe(can);
  const offline = writeAccess("create_work_item", BOUND, "offline", EVERY);
  expect(messageSeatAccess(offline, BOUND, "jane")).toBe(offline);
});

// A CHANGE INTO A SERVICE THE COMPANY DOES NOT RUN IS HELD, with the sentence
// that says so, ahead of any access the reader could be given: no grant gives
// a task anywhere to land where the company runs no Worklet. A change
// that is no service's is not held for it. Mutation: drop the service arm and
// the first two read `can: true`.
test("a change into a service the company does not run is held, and says why", () => {
  const acting = { ...BOUND, acts: ["create_work_item", "write_page", "pause_seat"] };
  const jira: ServicesRun = { tracker: false, knowledge: true };
  expect(writeAccess("create_work_item", acting, "open", jira)).toEqual({
    can: false,
    block: "held",
    reason: SERVICE_OFF_REASONS.tracker,
  });
  const confluence: ServicesRun = { tracker: true, knowledge: false };
  expect(writeAccess("write_page", acting, "open", confluence)).toEqual({
    can: false,
    block: "held",
    reason: SERVICE_OFF_REASONS.knowledge,
  });
  expect(writeAccess("pause_seat", acting, "open", jira).can).toBe(true);
  // AFTER WHAT THE PERSON CAN CLEAR: offline is still said first.
  expect(writeAccess("create_work_item", acting, "offline", jira)).toMatchObject({
    block: "offline",
  });
});

// A SCREEN SHOWING SOMEBODY ELSE'S RECORD HOLDS EVERY CHANGE ON IT, with its
// own sentence — but LAST: a reader who is offline or signed out is told the
// thing they can clear first, and a hold is a fact about the screen.
test("a hold is the screen's sentence, and it ranks after everything a person can clear", () => {
  const hold = "This is Rui Santos’s day.";
  expect(writeAccess("set_pins", BOUND, "open", EVERY, hold)).toEqual({
    can: false,
    block: "held",
    reason: hold,
  });
  expect(writeAccess("set_pins", BOUND, "offline", EVERY, hold)).toMatchObject({
    block: "offline",
  });
  expect(
    writeAccess(
      "set_pins",
      { ...BOUND, login: "", handle: "", owner: "", anonymous: true, acts: [] },
      "open",
      EVERY,
      hold,
    ),
  ).toMatchObject({ block: "anonymous" });
  // AND NO HOLD IS NO HOLD: the empty value of the context is `null`.
  expect(writeAccess("set_pins", BOUND, "open", EVERY, null).can).toBe(true);
});

describe("changing the company's configuration", () => {
  const viewer = (over: Partial<ViewerState>): ViewerState => ({
    ...UNBOUND,
    login: "ops.lead",
    owner: "ops.lead",
    grants: ["admin"],
    ...over,
  });

  // ADMIN, NOT A BINDING: `/config` is decided by Admin, so a principal bound
  // to no seat who holds it may change a ceiling, and a bound person holding
  // every app but Admin may not.
  test("is Admin, not the seat binding", () => {
    expect(configWriteAccess(viewer({ unbound: true }), "open")).toEqual({ can: true });
    expect(
      configWriteAccess(
        viewer({ grants: ["worklet", "doclet"], handle: "jane", owner: "jane", unbound: false }),
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

  // A MANAGED DOCUMENT IS WRITTEN SOMEWHERE ELSE (ADR-0030): every credential
  // but its writers is held, with the sentence naming who manages it — after
  // the reasons a person can clear here, and before a hold no screen releases.
  test("a managed document holds every credential but its writers", () => {
    const managed = viewer({ configWriter: false, configManagedBy: ["gitops"] });
    expect(configWriteAccess(managed, "open")).toEqual({
      can: false,
      block: "managed",
      reason: managedSentence(["gitops"]),
    });
    expect(managedSentence(["gitops"])).toBe("Managed by gitops. Change it there.");
    expect(configWriteAccess(managed, "open", "these are Rui's")).toMatchObject({
      block: "managed",
    });
    // The grant ranks first: a reader who could not write anyway is told
    // what they lack, which is the thing they can clear.
    expect(configWriteAccess({ ...managed, grants: ["worklet"] }, "open")).toMatchObject({
      block: "no_grant",
    });
    expect(
      configWriteAccess(viewer({ configWriter: true, configManagedBy: ["gitops"] }), "open"),
    ).toEqual({ can: true });
    expect(configWriteAccess(viewer({ configWriter: true, configManagedBy: [] }), "open")).toEqual({
      can: true,
    });
  });

  // THE STANDING ANSWER agrees with the press-time one about WHO may write,
  // and ignores what only this moment decides — the socket, a screen's hold.
  test("whether the reader may change the configuration at all is the grant and the writers", () => {
    expect(mayChangeConfig(viewer({}))).toBe(true);
    expect(mayChangeConfig(viewer({ grants: ["worklet", "doclet"] }))).toBe(false);
    expect(mayChangeConfig(viewer({ configWriter: false, configManagedBy: ["gitops"] }))).toBe(
      false,
    );
    expect(mayChangeConfig(viewer({ configWriter: true, configManagedBy: ["gitops"] }))).toBe(true);
  });
});

// ADMIN, OR A LEAD INSIDE THEIR SUBTREE: the engine admits a write by a
// person without Admin when everything it changes is inside a unit they lead,
// so a control about one of their seats or units is open to them, and one
// outside says which units they do lead rather than that access is missing.
describe("changing one part of the org chart", () => {
  const lead: ViewerState = {
    ...BOUND,
    login: "cto.person",
    handle: "cto",
    owner: "cto",
    grants: ["worklet"],
  };
  const scope = leadScope(CHART_ORG, "cto");

  test("a lead may change a seat or unit they lead, and nothing else", () => {
    expect(orgWriteAccess(lead, "open", scope, { seat: "swe" })).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, { unit: "core" })).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, "anywhere")).toEqual({ can: true });
    expect(orgWriteAccess(lead, "open", scope, { seat: "pm" })).toEqual({
      can: false,
      block: "outside_scope",
      reason: "You lead Core, and this is outside it: changing it takes Admin access.",
    });
    expect(
      orgWriteAccess(lead, "open", leadScope(CHART_ORG, "pm"), { unit: "core" }),
    ).toMatchObject({
      reason: expect.stringContaining("You lead Management and Developer Relations"),
    });
  });

  test("a person who leads nothing is told about Admin, and every earlier reason still ranks first", () => {
    expect(orgWriteAccess(lead, "open", NO_SCOPE, { seat: "swe" })).toMatchObject({
      block: "no_grant",
    });
    expect(orgWriteAccess(lead, "offline", scope, { seat: "swe" })).toMatchObject({
      block: "offline",
    });
    expect(orgWriteAccess(lead, "open", scope, { seat: "swe" }, "these are Rui's")).toMatchObject({
      block: "held",
    });
    // Admin reaches everything, a lead's scope or not.
    const admin = { ...lead, grants: ["admin"] };
    expect(orgWriteAccess(admin, "open", scope, { seat: "pm" })).toEqual({ can: true });
  });

  // A LEAD IS NO WRITER OF A MANAGED DOCUMENT: the engine refuses a subtree
  // write from every credential the writers list does not name (ADR-0030), so
  // a control inside what they lead is held with the managed sentence — while
  // one outside still says it is outside, which is the nearer reason.
  //
  // Mutation: drop the managed check on the lead path, and the first goes green
  // as `{ can: true }` for a write the engine would refuse.
  test("a lead's subtree write is held on a managed document", () => {
    const managed = { ...lead, configWriter: false, configManagedBy: ["gitops"] };
    expect(orgWriteAccess(managed, "open", scope, { seat: "swe" })).toEqual({
      can: false,
      block: "managed",
      reason: managedSentence(["gitops"]),
    });
    expect(orgWriteAccess(managed, "open", scope, { seat: "pm" })).toMatchObject({
      block: "outside_scope",
    });
    expect(
      orgWriteAccess(managed, "open", scope, { seat: "swe" }, "these are Rui's"),
    ).toMatchObject({ block: "managed" });
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
    // Nor does the managed sentence: it names who manages the document.
    expect(managedSentence(["gitops"])).not.toMatch(/token/i);
  });
});
