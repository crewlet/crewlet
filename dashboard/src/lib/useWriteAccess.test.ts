// @vitest-environment node

/**
 * Which reason a control is disabled with, in the order a person clears them.
 */

import { describe, expect, test } from "vitest";
import {
  WRITE_REASONS,
  configWriteAccess,
  mayChangeConfig,
  notAdminSentence,
  writeAccess,
} from "./useWriteAccess.ts";
import type { ViewerState } from "./viewer.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";

const BOUND: ViewerState = {
  tokenID: "founder",
  role: "admin",
  reach: "admin",
  admin: true,
  linked: true,
  handle: "jane",
  name: "Jane Founder",
  line: [],
  acts: ["set_pins"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: true,
  configManagedBy: [],
  admins: [{ handle: "jane", name: "Jane Founder" }],
};

test.each([
  ["offline, whoever is reading", BOUND, false, "offline"],
  ["nobody has said who this is", { ...BOUND, loading: true }, true, "loading"],
  [
    "no token",
    {
      ...BOUND,
      handle: "",
      tokenID: "",
      role: "",
      reach: "public",
      admin: false,
      linked: false,
      anonymous: true,
      acts: [],
      admins: [],
    },
    true,
    "anonymous",
  ],
  [
    "a token no seat binds",
    { ...BOUND, handle: "", linked: false, unbound: true, acts: [] },
    true,
    "unbound",
  ],
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
    writeAccess(
      "set_pins",
      { ...BOUND, handle: "", linked: false, unbound: true, acts: [] },
      true,
      hold,
    ),
  ).toMatchObject({ block: "unbound" });
  // AND NO HOLD IS NO HOLD: the empty value of the context is `null`.
  expect(writeAccess("set_pins", BOUND, true, null).can).toBe(true);
});

describe("changing the company's configuration", () => {
  const viewer = (over: Partial<ViewerState>): ViewerState => ({
    tokenID: "U0",
    role: "admin",
    reach: "admin",
    admin: true,
    linked: false,
    handle: "",
    name: "",
    line: [],
    acts: [],
    kind: "",
    project: "",
    unbound: true,
    anonymous: false,
    loading: false,
    asking: false,
    configWriter: true,
    configManagedBy: [],
    admins: [],
    ...over,
  });
  /** A teammate: a member's key, linked to their seat. */
  const member = (over: Partial<ViewerState> = {}): ViewerState =>
    viewer({
      role: "member",
      reach: "member",
      admin: false,
      linked: true,
      handle: "ada",
      unbound: false,
      configWriter: false,
      ...over,
    });

  // THE ROLE, NOT A LINK: `/config` is an admin's surface (ADR-0031) and is
  // not gated by a seat, so an unlinked admin key may change a ceiling it
  // could never answer an ask with — and a linked teammate holding a
  // member's key may not.
  test("is the engine's admin answer, not the seat link", () => {
    expect(configWriteAccess(viewer({ unbound: true }), true)).toEqual({ can: true });
    expect(configWriteAccess(member(), true)).toMatchObject({ can: false, block: "not_admin" });
  });

  // A MEMBER IS HELD BY NAME. The member's key works; what stops the change
  // is the role, and the remedy is a person — the first admin the engine
  // named, in handle order — never a token dialog nor `api.auth.tokens`.
  test("a member is told which admin to ask", () => {
    const admins = [
      { handle: "jane", name: "Jane Founder" },
      { handle: "rui", name: "Rui Santos" },
    ];
    expect(configWriteAccess(member({ admins }), true)).toEqual({
      can: false,
      block: "not_admin",
      reason: "Only an admin can change this — ask Jane Founder.",
    });
    expect(configWriteAccess(member({ admins }), true)).toMatchObject({
      reason: notAdminSentence(admins),
    });
  });

  // NOBODY TO NAME is still a sentence with a remedy: no person holds an
  // admin key (a deployment's only admin key is a pipeline's), or the engine
  // named none. A seat with no display name is asked for by its handle.
  test("with no admin to name, a member is told to ask an admin", () => {
    expect(configWriteAccess(member({ admins: [] }), true)).toMatchObject({
      block: "not_admin",
      reason: "Only an admin can change this — ask an admin.",
    });
    expect(notAdminSentence([{ handle: "jane", name: "" }])).toBe(
      "Only an admin can change this — ask jane.",
    );
  });

  test("in the order a person clears them", () => {
    expect(configWriteAccess(viewer({}), false)).toMatchObject({ block: "offline" });
    expect(configWriteAccess(viewer({ loading: true }), true)).toMatchObject({ block: "loading" });
    expect(
      configWriteAccess(
        viewer({ anonymous: true, tokenID: "", role: "", reach: "public", admin: false }),
        true,
      ),
    ).toMatchObject({ block: "anonymous" });
    expect(configWriteAccess(member(), true)).toMatchObject({ block: "not_admin" });
    expect(configWriteAccess(viewer({}), true, "these are Rui's")).toEqual({
      can: false,
      block: "held",
      reason: "these are Rui's",
    });
  });

  // A MANAGED DOCUMENT IS WRITTEN SOMEWHERE ELSE (ADR-0030): every admin but
  // its writers is held, with the sentence naming who manages it — after the
  // reasons a person can clear here, and before a hold no screen releases.
  test("a managed document holds every admin but its writers", () => {
    const managed = viewer({ configWriter: false, configManagedBy: ["gitops"] });
    expect(configWriteAccess(managed, true)).toEqual({
      can: false,
      block: "managed",
      reason: managedSentence(["gitops"]),
    });
    expect(managedSentence(["gitops"])).toContain("managed by gitops");
    expect(configWriteAccess(managed, true, "these are Rui's")).toMatchObject({
      block: "managed",
    });
    expect(configWriteAccess({ ...managed, admin: false, reach: "member" }, true)).toMatchObject({
      block: "not_admin",
    });
    expect(
      configWriteAccess(viewer({ configWriter: true, configManagedBy: ["gitops"] }), true),
    ).toEqual({ can: true });
    expect(configWriteAccess(viewer({ configWriter: true, configManagedBy: [] }), true)).toEqual({
      can: true,
    });
  });

  // THE STANDING ANSWER agrees with the press-time one about WHO may write,
  // and ignores what only this moment decides — the socket, a screen's hold.
  test("whether the reader may change the configuration at all is the admin and the writers", () => {
    expect(mayChangeConfig(viewer({}))).toBe(true);
    expect(mayChangeConfig(member())).toBe(false);
    expect(mayChangeConfig(viewer({ configWriter: false, configManagedBy: ["gitops"] }))).toBe(
      false,
    );
    expect(mayChangeConfig(viewer({ configWriter: true, configManagedBy: ["gitops"] }))).toBe(true);
  });
});
