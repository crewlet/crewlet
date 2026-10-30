/**
 * The Agents tab's readings, held one by one: a row's status word, which
 * actions a seat can take, the sentence an off seat reads as, and which saves
 * ask for a fresh step-up (D10).
 */

import { describe, expect, test } from "vitest";
import { DATADOG_LANE, GITHUB_LANE, datadogAnswer, offLane, onLane } from "./fixtures.ts";
import {
  accessWords,
  availableActions,
  offSentence,
  raisesToAdmin,
  sameDesired,
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
    expect(statusOf(DATADOG_LANE, offLane("V"), false, false).label).toBe("Off");
  });

  test("a refusal, a person's step and a change made by hand each say so", () => {
    const failed = { ...sre(), sync: { at: "", outcome: "failed" as const, steps: [] } };
    expect(statusOf(DATADOG_LANE, failed, false, false).label).toBe("Refused");
    const waiting = {
      ...sre(),
      plan: [{ verb: "approve" as const, text: "Accept", by: "you" as const }],
    };
    expect(statusOf(DATADOG_LANE, waiting, false, false).label).toBe("Needs you");
    const drift = {
      ...sre(),
      observed: { ...sre().observed, changed_by_hand_at: "2026-10-29T10:00:00Z" },
      plan: [{ verb: "restore" as const, text: "Restore", by: "crewlet" as const }],
    };
    expect(statusOf(DATADOG_LANE, drift, false, false).label).toBe("Changed at Datadog");
  });

  test("a pending plan reads Syncing while a sync runs", () => {
    const pending = {
      ...sre(),
      plan: [{ verb: "rerole" as const, text: "Change", by: "crewlet" as const }],
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
    const kept = { ...offLane("V"), observed: { exists: true, enabled: false } };
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
  expect(offSentence(DATADOG_LANE, offLane("V"))).toBe(
    "Off. Turning it on creates a Datadog service account with the chosen role and its own key.",
  );
  const kept = { ...offLane("V"), observed: { exists: true, enabled: false } };
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

describe("a raise to administration asks for a step-up", () => {
  test("to an admin level, and only when it is a change", () => {
    const from = { enabled: true, level: "r-std" };
    expect(raisesToAdmin(DATADOG_LANE, from, { enabled: true, level: "r-admin" })).toBe(true);
    expect(
      raisesToAdmin(
        DATADOG_LANE,
        { enabled: true, level: "r-admin" },
        { enabled: true, level: "r-admin" },
      ),
    ).toBe(false);
    expect(
      raisesToAdmin(
        DATADOG_LANE,
        { enabled: false, level: "r-admin" },
        { enabled: true, level: "r-admin" },
      ),
    ).toBe(true);
    expect(raisesToAdmin(DATADOG_LANE, from, { enabled: false, level: "r-admin" })).toBe(false);
  });

  test("to Read and write on GitHub Contents or Workflows", () => {
    const from = { enabled: true, permissions: { contents: "read" as const } };
    expect(
      raisesToAdmin(GITHUB_LANE, from, { enabled: true, permissions: { contents: "write" } }),
    ).toBe(true);
    expect(
      raisesToAdmin(GITHUB_LANE, from, {
        enabled: true,
        permissions: { contents: "read", issues: "write" },
      }),
    ).toBe(false);
  });
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
