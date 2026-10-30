/**
 * The Agents tab's readings, held one by one: a row's status word, which
 * actions a seat can take, the sentence an off seat reads as, and the
 * vendor's words for what a seat holds.
 */

import { describe, expect, test } from "vitest";
import {
  CONFLUENCE_LANE,
  DATADOG_LANE,
  JIRA_LANE,
  GITHUB_LANE,
  MATTERMOST_LANE,
  datadogAnswer,
  offLane,
  onLane,
} from "./fixtures.ts";
import {
  accessHold,
  accessWords,
  availableActions,
  emptyScopeHint,
  joinLevels,
  levelName,
  productOnly,
  refusedLevel,
  scopeApplies,
  offSentence,
  sameDesired,
  scopeWords,
  statusOf,
  toolWords,
} from "./logic.ts";

const sre = () => datadogAnswer().seats.find((s) => s.handle === "sre")!.lanes.datadog!;

describe("a row's status", () => {
  test("a draft reads Not saved before anything the engine said", () => {
    expect(statusOf(DATADOG_LANE, sre(), true, false).label).toBe("Not saved");
  });

  test("an on seat with nothing to do is in sync, an off one is off", () => {
    expect(statusOf(DATADOG_LANE, sre(), false, false)).toMatchObject({
      label: "In sync",
      tone: "success",
    });
    expect(statusOf(DATADOG_LANE, offLane(), false, false).label).toBe("Off");
  });

  test("a refusal, a person's step and a change made by hand each say so", () => {
    const failed = { ...sre(), sync: { at: "", outcome: "failed" as const, steps: [] } };
    expect(statusOf(DATADOG_LANE, failed, false, false).label).toBe("Refused");
    const waiting = {
      ...sre(),
      plan: [
        { op: "create" as const, verb: "approve" as const, text: "Accept", by: "you" as const },
      ],
    };
    expect(statusOf(DATADOG_LANE, waiting, false, false).label).toBe("Needs you");
    const drift = {
      ...sre(),
      observed: { ...sre().observed, changed_by_hand_at: "2026-10-29T10:00:00Z" },
      plan: [
        { op: "level" as const, verb: "restore" as const, text: "Restore", by: "crewlet" as const },
      ],
    };
    expect(statusOf(DATADOG_LANE, drift, false, false).label).toBe("Changed at Datadog");
  });

  test("a pending plan reads Syncing while a sync runs", () => {
    const pending = {
      ...sre(),
      plan: [
        { op: "level" as const, verb: "rerole" as const, text: "Change", by: "crewlet" as const },
      ],
    };
    expect(statusOf(DATADOG_LANE, pending, false, true)).toMatchObject({
      label: "Syncing",
      busy: true,
    });
  });
});

describe("the actions a seat can take", () => {
  test("an on seat with a key can rotate it, and cannot be removed", () => {
    expect(availableActions(DATADOG_LANE, sre(), false, false)).toEqual(["rotate"]);
  });

  test("an off seat whose account is kept can be removed", () => {
    const kept = { ...offLane(), observed: { exists: true, enabled: false } };
    expect(availableActions(DATADOG_LANE, kept, false, false)).toEqual(["remove"]);
  });

  test("nothing is offered to a reader who may not change it, or while a sync runs", () => {
    expect(availableActions(DATADOG_LANE, sre(), true, false)).toEqual([]);
    expect(availableActions(DATADOG_LANE, sre(), false, true)).toEqual([]);
  });

  test("a vendor whose tokens are minted per use offers no rotation", () => {
    const github = onLane({ variable: "GITHUB_APP_KEY_QA" });
    expect(availableActions(GITHUB_LANE, github, false, false)).toEqual([]);
  });
});

test("an off seat reads as one sentence about what turning it on does", () => {
  expect(offSentence(DATADOG_LANE, offLane())).toBe(
    "Off. Turning it on creates a Datadog service account with the chosen role and its own key.",
  );
  const kept = { ...offLane(), observed: { exists: true, enabled: false } };
  expect(offSentence(DATADOG_LANE, kept)).toMatch(/^Off\. Its account is disabled and kept/);
});

test("the tool reads as one value", () => {
  expect(toolWords(DATADOG_LANE, sre())).toBe("Live, Datadog MCP server, one per agent");
  expect(toolWords(DATADOG_LANE, { ...sre(), tool: "no_server" })).toBe(
    "Not set up: no datadog tool server is declared",
  );
});

test("what the vendor holds is said in the vendor's words", () => {
  expect(accessWords(DATADOG_LANE, sre().observed)).toBe("Datadog Admin Role");
  expect(
    accessWords(GITHUB_LANE, {
      exists: true,
      enabled: true,
      permissions: { contents: "write", issues: "read" },
    }),
  ).toBe("Contents: Read and write, Issues: Read-only");
});

test("two desired states are the same whatever order their scope is in", () => {
  expect(
    sameDesired(
      GITHUB_LANE,
      { enabled: true, scope: ["a", "b"] },
      { enabled: true, scope: ["b", "a"] },
    ),
  ).toBe(true);
  expect(
    sameDesired(GITHUB_LANE, { enabled: true, permissions: { issues: "read" } }, { enabled: true }),
  ).toBe(false);
});

test("a credential provided externally is never rotated by Crewlet", () => {
  const external = sre();
  external.observed.credential = {
    variable: "V",
    minted_at: new Date().toISOString(),
    source: "external",
  };
  expect(availableActions(DATADOG_LANE, external, false, false)).toEqual([]);
});

test("an agent that left the company reads as such, whatever its sync", () => {
  expect(statusOf(DATADOG_LANE, sre(), false, false, true)).toMatchObject({
    label: "Agent left",
    tone: "warning",
  });
});

test("an empty Mattermost channel list is literal: no channels, and a hint while on", () => {
  expect(scopeWords(MATTERMOST_LANE, [])).toBe("No channels");
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: true, scope: [] })).toMatch(
    /cannot read or post anywhere/,
  );
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: false })).toBeNull();
  expect(emptyScopeHint(MATTERMOST_LANE, { enabled: true, scope: ["incidents"] })).toBeNull();
  expect(emptyScopeHint(GITHUB_LANE, { enabled: true })).toBeNull();
});

test("an operator-written document holds only a reader who is not an admin; any other managed one holds all", () => {
  const line = "Access settings are managed by the operator through each agent's Seat object.";
  expect(accessHold("Managed by the operator.", true, line)).toBeUndefined();
  expect(accessHold("Managed by the operator.", false, line)).toBe(
    "Admins change an agent's access.",
  );
  expect(accessHold("Managed by GitOps.", true, undefined)).toBe("Managed by GitOps.");
  expect(accessHold(null, true, undefined)).toBeUndefined();
});

describe("a lane of sets (Confluence by operation)", () => {
  test("stores the chosen ids sorted, once each, and the empty level as nothing", () => {
    expect(joinLevels(["read:space", "create:page", "read:space"])).toBe("create:page read:space");
    expect(joinLevels([""])).toBe("");
    expect(joinLevels([])).toBe("");
  });

  test("names the set in the vendor's order, and the empty set as product access only", () => {
    expect(levelName(CONFLUENCE_LANE, "create:page read:space")).toBe("View, Add pages");
    expect(levelName(CONFLUENCE_LANE, "")).toBe("Product access only");
  });

  test("the same set in another order is no change", () => {
    expect(
      sameDesired(
        CONFLUENCE_LANE,
        { enabled: true, level: "read:space create:page" },
        { enabled: true, level: "create:page read:space" },
      ),
    ).toBe(true);
  });

  test("product access only is valid, and refused only together with a space list", () => {
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "" })).toBeNull();
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "", scope: ["ENG"] })).toMatch(
      /takes no spaces/,
    );
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: false })).toBeNull();
    expect(refusedLevel(CONFLUENCE_LANE, { enabled: true, level: "read:space" })).toBeNull();
  });
});

test("Jira's empty level is product access only, which has no scope", () => {
  expect(productOnly(JIRA_LANE, "")).toBe("Product access only");
  expect(productOnly(JIRA_LANE, undefined)).toBe("Product access only");
  expect(productOnly(JIRA_LANE, "10003")).toBeNull();
  expect(productOnly(DATADOG_LANE, "")).toBeNull();
  expect(productOnly(CONFLUENCE_LANE, "")).toBe("Product access only");
  expect(levelName(JIRA_LANE, "")).toBe("Product access only");
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "" })).toBe(false);
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "10003" })).toBe(true);
  // A LIST HELD WITH IT is offered, so it can be cleared.
  expect(scopeApplies(JIRA_LANE, { enabled: true, level: "", scope: ["ENG"] })).toBe(true);
});

test("a lane with roles and no empty level refuses an agent that is on with none", () => {
  expect(refusedLevel(DATADOG_LANE, { enabled: true })).toBe(
    "Choose a Datadog role for every agent that is on.",
  );
  expect(refusedLevel(DATADOG_LANE, { enabled: true, level: "r-ro" })).toBeNull();
  expect(refusedLevel(DATADOG_LANE, { enabled: false })).toBeNull();
  // GitHub has permissions rather than levels, and Jira an empty level of its own.
  expect(refusedLevel(GITHUB_LANE, { enabled: true })).toBeNull();
  expect(refusedLevel(JIRA_LANE, { enabled: true, level: "" })).toBeNull();
});

test("an agent that left the company reads as such when its details open, not as off", () => {
  const kept = { ...offLane(), observed: { exists: true, enabled: false } };
  expect(offSentence(DATADOG_LANE, kept, true)).toBe(
    "This agent left the company. Its account is kept, disabled, until you remove it.",
  );
  expect(offSentence(DATADOG_LANE, offLane(), true)).toBe(
    "This agent left the company and has no account here.",
  );
});
